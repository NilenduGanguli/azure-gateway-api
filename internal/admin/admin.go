// Package admin serves the gateway's own operational endpoints.
//
// They live under /_gw so they can never collide with an Azure route, present or future. The
// container-style /status and /ready are also served, because both upstream containers claim
// those paths at their own root and the gateway fronts both at once — proxying either would have
// to pick a winner, so it reports on both instead.
package admin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/engine"
	"github.com/NilenduGanguli/azure-gateway-api/internal/httpx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// BuildInfo describes the running binary.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Built   string `json:"built"`
	Go      string `json:"go"`
}

// Server serves the admin surface.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	jobs    *jobs.Manager
	build   BuildInfo
	started time.Time
}

// New builds an admin server.
func New(cfg *config.Config, st *store.Store, jm *jobs.Manager, build BuildInfo, started time.Time) *Server {
	return &Server{cfg: cfg, store: st, jobs: jm, build: build, started: started}
}

// Register wires the admin routes onto a mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /_gw/live", s.live)
	mux.HandleFunc("GET /_gw/ready", s.ready)
	mux.HandleFunc("GET /_gw/health", s.health)
	mux.HandleFunc("GET /_gw/version", s.version)
	mux.HandleFunc("GET /_gw/config", s.config)
	mux.HandleFunc("GET /_gw/jobs", s.listJobs)
	mux.HandleFunc("GET /_gw/jobs/{id}", s.getJob)
	mux.HandleFunc("GET /_gw/metrics", s.metrics)

	// Container-shaped probes, so existing tooling pointed at a container keeps working.
	mux.HandleFunc("GET /ready", s.containerReady)
	mux.HandleFunc("GET /status", s.containerStatus)
}

// live reports that the process is running. It never touches a dependency, so a stalled upstream
// cannot cause a restart loop.
func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready reports whether the gateway can accept work, which depends only on its own store.
//
// Upstream reachability is deliberately excluded: a container that is briefly down should make
// individual jobs fail, not take the gateway out of service and strand every stored result.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"detail": "the job store is not reachable",
		})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type healthReport struct {
	Status    string                    `json:"status"`
	Uptime    string                    `json:"uptime"`
	Build     BuildInfo                 `json:"build"`
	Store     storeHealth               `json:"store"`
	Capacity  map[string]map[string]int `json:"capacity"`
	Upstreams []upstream.Health         `json:"upstreams"`
	Jobs      map[string]*store.Counts  `json:"jobs"`
	Warnings  []string                  `json:"warnings,omitempty"`
}

type storeHealth struct {
	DataDir   string  `json:"dataDir"`
	DiskUsed  float64 `json:"diskUsed"`
	DiskKnown bool    `json:"diskUsedKnown"`
	Reachable bool    `json:"reachable"`
}

// health is the full diagnostic view.
//
// The upstream section lists the configured containers only, each checked through its GET
// /status. There is no capability to report: every call is synchronous, so a container either
// serves its sync route or each job says so in its own error.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	used, known := s.store.DiskUsage()
	rep := healthReport{
		Status:   "ok",
		Uptime:   time.Since(s.started).Round(time.Second).String(),
		Build:    s.build,
		Capacity: s.jobs.Capacity(),
		Warnings: s.cfg.Warnings(),
		Store: storeHealth{
			DataDir:   s.store.DataDir(),
			DiskUsed:  used,
			DiskKnown: known,
			Reachable: s.store.Ping(ctx) == nil,
		},
	}
	for _, name := range []string{jobs.SurfaceDI, jobs.SurfaceRead} {
		if c := s.jobs.Client(name); c != nil {
			rep.Upstreams = append(rep.Upstreams, c.Health(ctx))
		}
	}
	if counts, err := s.store.Stats(ctx); err == nil {
		rep.Jobs = counts
	}

	code := http.StatusOK
	if !rep.Store.Reachable {
		rep.Status = "degraded"
		code = http.StatusServiceUnavailable
	} else {
		for _, u := range rep.Upstreams {
			if !u.OK {
				rep.Status = "degraded"
			}
		}
	}
	httpx.WriteJSON(w, code, rep)
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, s.build)
}

// config renders the effective configuration with every secret redacted.
func (s *Server) config(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, s.cfg.Redacted())
}

type jobView struct {
	ID          string `json:"id"`
	Surface     string `json:"surface"`
	ModelID     string `json:"modelId,omitempty"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	ExpiresAt   string `json:"expiresAt"`
	InputBytes  int64  `json:"inputBytes"`
	ResultBytes int64  `json:"resultBytes"`
	UpstreamMS  int64  `json:"upstreamMs"`
	// Attempts counts runs of the job: more than one means a crash or restart re-ran it.
	Attempts int `json:"attempts"`
	// UpstreamAttempts counts the last run's calls to the container, retries and hedges
	// included; UpstreamParts is how many parts it was analysed in, 1 when sent whole.
	UpstreamAttempts int    `json:"upstreamAttempts"`
	UpstreamParts    int    `json:"upstreamParts"`
	Error            string `json:"error,omitempty"`
}

func toView(j *store.Job) jobView {
	return jobView{
		ID: j.ID, Surface: j.Surface, ModelID: j.ModelID, Status: string(j.Status),
		CreatedAt: j.CreatedAt.Format(time.RFC3339), UpdatedAt: j.UpdatedAt.Format(time.RFC3339),
		ExpiresAt:  j.ExpiresAt.Format(time.RFC3339),
		InputBytes: j.InputBytes, ResultBytes: j.ResultBytes, UpstreamMS: j.UpstreamMS,
		Attempts: j.Attempts, UpstreamAttempts: j.UpstreamAttempts, UpstreamParts: j.UpstreamParts,
		Error: j.ErrorJSON,
	}
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	list, err := s.store.Recent(r.Context(), limit)
	if err != nil {
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]jobView, 0, len(list))
	for _, j := range list {
		out = append(out, toView(j))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"jobs": out, "count": len(out)})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toView(j))
}

// metrics renders Prometheus text format.
//
// It is written by hand rather than pulled from a client library: the gateway exports a handful
// of gauges, and a dependency-free binary is worth more here than a metrics framework.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder

	writeMetric := func(name, help, typ string, samples map[string]float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		keys := make([]string, 0, len(samples))
		for k := range samples {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "" {
				fmt.Fprintf(&b, "%s %g\n", name, samples[k])
			} else {
				fmt.Fprintf(&b, "%s{%s} %g\n", name, k, samples[k])
			}
		}
	}

	writeMetric("gateway_uptime_seconds", "Seconds since the gateway started.", "gauge",
		map[string]float64{"": time.Since(s.started).Seconds()})

	inflight := map[string]float64{}
	limit := map[string]float64{}
	queued := map[string]float64{}
	for surfaceName, c := range s.jobs.Capacity() {
		label := fmt.Sprintf("surface=%q", surfaceName)
		inflight[label] = float64(c["inFlight"])
		limit[label] = float64(c["limit"])
		queued[label] = float64(c["queued"])
	}
	writeMetric("gateway_jobs_in_flight", "Accepted jobs that have not reached a terminal state.", "gauge", inflight)
	writeMetric("gateway_jobs_capacity", "Maximum simultaneously accepted jobs.", "gauge", limit)
	writeMetric("gateway_jobs_queued", "Admitted jobs waiting for a worker.", "gauge", queued)

	if counts, err := s.store.Stats(r.Context()); err == nil {
		byStatus := map[string]float64{}
		for surfaceName, c := range counts {
			byStatus[fmt.Sprintf("surface=%q,status=%q", surfaceName, "notStarted")] = float64(c.NotStarted)
			byStatus[fmt.Sprintf("surface=%q,status=%q", surfaceName, "running")] = float64(c.Running)
			byStatus[fmt.Sprintf("surface=%q,status=%q", surfaceName, "succeeded")] = float64(c.Succeeded)
			byStatus[fmt.Sprintf("surface=%q,status=%q", surfaceName, "failed")] = float64(c.Failed)
		}
		writeMetric("gateway_jobs_total", "Stored jobs by surface and status.", "gauge", byStatus)
	}

	if used, ok := s.store.DiskUsage(); ok {
		writeMetric("gateway_volume_used_ratio", "Fraction of the data volume in use.", "gauge",
			map[string]float64{"": used})
	}

	s.writeEngineMetrics(&b, writeMetric)

	body := b.String()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// Label values that are always exported, at zero until they happen, so a dashboard or an alert on
// one of them has a series to read from the first scrape on.
var (
	partOutcomes    = []string{"succeeded", "retried", "halved", "hedged", "hedge_won", "rechecked"}
	fallbackReasons = []string{"unparseable", "encrypted", "budget", "panic", "pages_param", "merge_check"}
)

// writeEngineMetrics appends each configured surface's engine counters and upstream slots.
func (s *Server) writeEngineMetrics(b *strings.Builder,
	writeMetric func(name, help, typ string, samples map[string]float64)) {

	snaps := map[string]engine.MetricsSnapshot{}
	slots := map[string]float64{}
	for _, name := range []string{jobs.SurfaceDI, jobs.SurfaceRead} {
		e := s.jobs.Engine(name)
		if e == nil {
			continue
		}
		snaps[name] = e.Metrics().Snapshot()
		st := e.Sched().Stats()
		slots[fmt.Sprintf("surface=%q,state=%q", name, "inUse")] = float64(st.InUse)
		slots[fmt.Sprintf("surface=%q,state=%q", name, "waiting")] = float64(st.Waiting)
		slots[fmt.Sprintf("surface=%q,state=%q", name, "limit")] = float64(st.Limit)
	}

	histogram := func(name, help string, pick func(engine.MetricsSnapshot) engine.Histogram) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
		for _, surface := range sortedKeys(snaps) {
			writeHistogram(b, name, surface, pick(snaps[surface]))
		}
	}
	histogram("gateway_part_seconds", "Wall time of one successful part analysis, in seconds.",
		func(m engine.MetricsSnapshot) engine.Histogram { return m.PartSeconds })
	histogram("gateway_job_parts", "Parts each analysed document was sent in; 1 when whole.",
		func(m engine.MetricsSnapshot) engine.Histogram { return m.JobParts })
	histogram("gateway_merge_seconds", "Time to merge a split document's part results, in seconds.",
		func(m engine.MetricsSnapshot) engine.Histogram { return m.MergeSeconds })

	labelled := func(label string, known []string, pick func(engine.MetricsSnapshot) map[string]int64) map[string]float64 {
		out := map[string]float64{}
		for surface, m := range snaps {
			for _, v := range known {
				out[fmt.Sprintf("surface=%q,%s=%q", surface, label, v)] = 0
			}
			for v, n := range pick(m) {
				out[fmt.Sprintf("surface=%q,%s=%q", surface, label, v)] = float64(n)
			}
		}
		return out
	}
	writeMetric("gateway_parts_total", "Part attempts by outcome.", "counter",
		labelled("outcome", partOutcomes, func(m engine.MetricsSnapshot) map[string]int64 { return m.Parts }))
	writeMetric("gateway_split_fallback_total",
		"Documents sent whole although splitting was on, by reason.", "counter",
		labelled("reason", fallbackReasons, func(m engine.MetricsSnapshot) map[string]int64 { return m.SplitFallbacks }))
	writeMetric("gateway_upstream_retries_total", "Upstream attempts retried, by reason.", "counter",
		labelled("reason", nil, func(m engine.MetricsSnapshot) map[string]int64 { return m.Retries }))

	exhausted := map[string]float64{}
	for surface, m := range snaps {
		exhausted[fmt.Sprintf("surface=%q", surface)] = float64(m.Exhausted)
	}
	writeMetric("gateway_upstream_exhausted_total",
		"Analyses that failed after spending every attempt.", "counter", exhausted)
	writeMetric("gateway_upstream_slots",
		"Upstream call slots by state: in use, waited for, and in all.", "gauge", slots)
}

// writeHistogram renders one surface's histogram in Prometheus text form. Counts are already
// cumulative, as le buckets are; +Inf is the total count.
func writeHistogram(b *strings.Builder, name, surface string, h engine.Histogram) {
	for i, bound := range h.Bounds {
		var n int64
		if i < len(h.Counts) {
			n = h.Counts[i]
		}
		fmt.Fprintf(b, "%s_bucket{surface=%q,le=%q} %d\n", name, surface,
			strconv.FormatFloat(bound, 'g', -1, 64), n)
	}
	fmt.Fprintf(b, "%s_bucket{surface=%q,le=\"+Inf\"} %d\n", name, surface, h.Count)
	fmt.Fprintf(b, "%s_sum{surface=%q} %g\n", name, surface, h.Sum)
	fmt.Fprintf(b, "%s_count{surface=%q} %d\n", name, surface, h.Count)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// containerReady mirrors the containers' /ready body, including the exact "ready":"ready" pair
// that Microsoft's own compose healthcheck greps for.
func (s *Server) containerReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"ready": "notReady"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"ready": "ready"})
}

// containerStatus mirrors the containers' /status body, whose apiStatus field Microsoft's own
// batch tooling parses and compares against "valid".
func (s *Server) containerStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	var unhealthy []string
	for _, name := range []string{jobs.SurfaceDI, jobs.SurfaceRead} {
		if c := s.jobs.Client(name); c != nil {
			if h := c.Health(ctx); !h.OK {
				unhealthy = append(unhealthy, h.Name)
			}
		}
	}
	if len(unhealthy) > 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{
			"apiStatus":        "Invalid",
			"apiStatusMessage": "Upstream containers unreachable: " + strings.Join(unhealthy, ", "),
		})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"apiStatus":        "Valid",
		"apiStatusMessage": "All upstream containers are reachable.",
	})
}
