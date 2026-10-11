package admin_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/admin"
	"github.com/NilenduGanguli/azure-gateway-api/internal/app"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

type gateway struct {
	t   *testing.T
	url string
	st  *store.Store
}

// newGateway wires the real gateway over a DI container at diURL and, when readURL is not empty,
// a Read container.
func newGateway(t *testing.T, diURL, readURL string, splitPages int) *gateway {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir: dir, ResultTTL: time.Hour, GCInterval: time.Hour, DiskHighWatermark: 0.99,
		MaxRequestBytes: 1 << 20, SplitPages: splitPages,
		DI: config.Upstream{BaseURL: diURL, MaxInflight: 2, Timeout: 20 * time.Second},
	}
	if readURL != "" {
		cfg.Read = config.Upstream{BaseURL: readURL, MaxInflight: 3, Timeout: 20 * time.Second}
	}
	st, err := store.Open(store.Options{DataDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := app.NewEngines(cfg, st, log)
	m := jobs.New(jobs.Options{Config: cfg, Store: st, Logger: log, DI: e.DI, Read: e.Read,
		DIClient: e.DIClient, ReadClient: e.ReadClient})
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop() })
	srv := httptest.NewServer(app.NewHandler(app.Deps{Config: cfg, Store: st, Jobs: m,
		Logger: log, Started: time.Now(), Build: admin.BuildInfo{Version: "test"}}))
	t.Cleanup(srv.Close)
	return &gateway{t: t, url: srv.URL, st: st}
}

func (g *gateway) get(path string, out any) []byte {
	g.t.Helper()
	resp, err := http.Get(g.url + path)
	if err != nil {
		g.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		g.t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			g.t.Fatalf("GET %s: not JSON: %v", path, err)
		}
	}
	return data
}

func newDI(t *testing.T) *mockazure.Container {
	t.Helper()
	c := mockazure.NewDI(mockazure.Options{})
	t.Cleanup(c.Close)
	return c
}

// TestHealthCoversOnlyConfiguredSurfaces checks /_gw/health on a gateway fronting DI alone: one
// upstream, checked through its /status and with no syncAnalyze capability, and capacity for DI
// only, its upstream slots read from the engine's scheduler.
func TestHealthCoversOnlyConfiguredSurfaces(t *testing.T) {
	g := newGateway(t, newDI(t).URL(), "", 0)

	var rep struct {
		Status    string                    `json:"status"`
		Capacity  map[string]map[string]int `json:"capacity"`
		Upstreams []map[string]any          `json:"upstreams"`
	}
	g.get("/_gw/health", &rep)
	if rep.Status != "ok" {
		t.Errorf("status %q, want ok", rep.Status)
	}
	if len(rep.Upstreams) != 1 || rep.Upstreams[0]["name"] != "document-intelligence" ||
		rep.Upstreams[0]["ok"] != true {
		t.Fatalf("upstreams %v, want the DI container alone, healthy", rep.Upstreams)
	}
	if _, ok := rep.Upstreams[0]["syncAnalyze"]; ok {
		t.Error("health still reports a syncAnalyze capability")
	}
	if len(rep.Capacity) != 1 || rep.Capacity[jobs.SurfaceDI] == nil {
		t.Fatalf("capacity %v, want DI alone", rep.Capacity)
	}
	di := rep.Capacity[jobs.SurfaceDI]
	if di["upstreamLimit"] != 2 || di["upstreamInUse"] != 0 || di["upstreamWaiting"] != 0 {
		t.Errorf("DI capacity %v, want upstream limit 2 and nothing in use or waiting", di)
	}

	var status map[string]string
	g.get("/status", &status)
	if status["apiStatus"] != "Valid" {
		t.Errorf("/status %v, want Valid over the one configured container", status)
	}
}

// TestConfigView checks the keys Wega's observer parses from /_gw/config, and that a surface
// that is not configured reports no capacity.
func TestConfigView(t *testing.T) {
	g := newGateway(t, newDI(t).URL(), "", 0)
	var cfg map[string]any
	g.get("/_gw/config", &cfg)
	for k, want := range map[string]any{
		"diEnabled": true, "readEnabled": false, "diMaxInflight": float64(2),
		"readMaxInflight": float64(0), "queueDepth": float64(0), "splitPages": float64(0),
	} {
		if got, ok := cfg[k]; !ok || got != want {
			t.Errorf("%s is %v (present %v), want %v", k, got, ok, want)
		}
	}
}

// TestJobViewShowsUpstreamCounts checks that a job's view carries its last run's upstream attempt
// and part counts, and none of the async-path fields that no job has any more.
func TestJobViewShowsUpstreamCounts(t *testing.T) {
	g := newGateway(t, newDI(t).URL(), "", 0)
	ctx := context.Background()
	now := time.Now().UTC()
	const id = "00000000-0000-4000-8000-0000000000d1"
	if err := g.st.Create(ctx, &store.Job{ID: id, Surface: jobs.SurfaceDI, Status: store.StatusRunning,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := g.st.Succeed(ctx, id, "/x", 10, 900, 7, 5, now); err != nil || !ok {
		t.Fatalf("succeed: ok=%v err=%v", ok, err)
	}

	var view map[string]any
	g.get("/_gw/jobs/"+id, &view)
	if view["upstreamAttempts"] != float64(7) || view["upstreamParts"] != float64(5) {
		t.Errorf("view %v, want upstreamAttempts 7 and upstreamParts 5", view)
	}
	for _, gone := range []string{"upstreamMode", "hasPdf", "figures"} {
		if _, ok := view[gone]; ok {
			t.Errorf("view still carries %s", gone)
		}
	}
}

// TestJobViewAfterASplitJob runs a document large enough to split and reads its view.
func TestJobViewAfterASplitJob(t *testing.T) {
	paged := mockazure.NewPaged(mockazure.PagedOptions{})
	t.Cleanup(paged.Close)
	g := newGateway(t, paged.URL(), "", 5)
	resp, err := http.Post(g.url+"/documentintelligence/documentModels/prebuilt-layout:analyze"+
		"?api-version=2024-11-30", "application/pdf", strings.NewReader(string(pdftest.Make(12))))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Operation-Location")
	id := loc[strings.LastIndex(loc, "/")+1 : strings.Index(loc, "?")]

	deadline := time.Now().Add(20 * time.Second)
	var view map[string]any
	for time.Now().Before(deadline) {
		g.get("/_gw/jobs/"+id, &view)
		if view["status"] == "succeeded" || view["status"] == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if view["status"] != "succeeded" {
		t.Fatalf("job %v did not succeed", view)
	}
	if parts, _ := view["upstreamParts"].(float64); parts < 2 {
		t.Errorf("upstreamParts %v, want the 12 pages in more than one part", view["upstreamParts"])
	}
}

// TestMetricsExportEngineFamilies checks every engine family on /_gw/metrics: each announced with
// # HELP and # TYPE, histograms with cumulative buckets ending in +Inf equal to _count, and the
// upstream slots per configured surface.
func TestMetricsExportEngineFamilies(t *testing.T) {
	readC := mockazure.NewRead(mockazure.Options{})
	t.Cleanup(readC.Close)
	g := newGateway(t, newDI(t).URL(), readC.URL(), 0)

	// One analysis, so the job-parts histogram has an observation.
	resp, err := http.Post(g.url+"/documentintelligence/documentModels/prebuilt-layout:syncAnalyze"+
		"?api-version=2024-11-30", "application/pdf", strings.NewReader("%PDF-1.7 fake"))
	if err != nil {
		t.Fatalf("sync analyze: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync analyze: status %d", resp.StatusCode)
	}

	text := string(g.get("/_gw/metrics", nil))
	for name, typ := range map[string]string{
		"gateway_part_seconds":             "histogram",
		"gateway_job_parts":                "histogram",
		"gateway_merge_seconds":            "histogram",
		"gateway_parts_total":              "counter",
		"gateway_split_fallback_total":     "counter",
		"gateway_upstream_retries_total":   "counter",
		"gateway_upstream_exhausted_total": "counter",
		"gateway_upstream_slots":           "gauge",
	} {
		if !strings.Contains(text, "# HELP "+name+" ") || !strings.Contains(text, "# TYPE "+name+" "+typ+"\n") {
			t.Errorf("%s is not announced as a %s", name, typ)
		}
	}

	samples := parseSamples(t, text)
	for _, surface := range []string{"di", "read"} {
		for _, h := range []string{"gateway_part_seconds", "gateway_job_parts", "gateway_merge_seconds"} {
			checkHistogram(t, samples, h, surface)
		}
		for _, state := range []string{"inUse", "waiting", "limit"} {
			key := `gateway_upstream_slots{surface="` + surface + `",state="` + state + `"}`
			if _, ok := samples[key]; !ok {
				t.Errorf("%s is missing", key)
			}
		}
		key := `gateway_upstream_exhausted_total{surface="` + surface + `"}`
		if _, ok := samples[key]; !ok {
			t.Errorf("%s is missing", key)
		}
	}
	if got := samples[`gateway_upstream_slots{surface="read",state="limit"}`]; got != 3 {
		t.Errorf("Read slot limit %v, want 3", got)
	}
	if got := samples[`gateway_job_parts_count{surface="di"}`]; got != 1 {
		t.Errorf("DI job-parts count %v, want the 1 analysis", got)
	}
	if got := samples[`gateway_parts_total{surface="di",outcome="hedge_won"}`]; got != 0 {
		t.Errorf("hedge_won is %v, want an exported 0", got)
	}
}

// parseSamples reads every sample line into name{labels} → value.
func parseSamples(t *testing.T, text string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndex(line, " ")
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("sample %q has no number: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

// checkHistogram asserts one surface's histogram: buckets non-decreasing, +Inf equal to _count,
// and _sum present.
func checkHistogram(t *testing.T, samples map[string]float64, name, surface string) {
	t.Helper()
	prev := -1.0
	for _, le := range []string{"0.25", "0.5", "1", "2", "5", "10", "30", "60", "120", "300", "+Inf"} {
		key := name + `_bucket{surface="` + surface + `",le="` + le + `"}`
		v, ok := samples[key]
		if !ok {
			t.Errorf("%s is missing", key)
			continue
		}
		if v < prev {
			t.Errorf("%s is %v, below the previous bucket's %v", key, v, prev)
		}
		prev = v
	}
	count, ok := samples[name+`_count{surface="`+surface+`"}`]
	if !ok || count != prev {
		t.Errorf("%s_count for %s is %v, want the +Inf bucket's %v", name, surface, count, prev)
	}
	if _, ok := samples[name+`_sum{surface="`+surface+`"}`]; !ok {
		t.Errorf("%s_sum for %s is missing", name, surface)
	}
}
