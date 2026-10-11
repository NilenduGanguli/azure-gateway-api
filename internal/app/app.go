// Package app assembles the gateway's HTTP handler.
//
// It exists so the binary and the conformance suite cannot drift apart. They used to build the
// mux separately, and they disagreed: the suite omitted the "/" catch-all, so a wrong verb on a
// registered route answered 405 under test and a bodyless 404 in production — a divergence the
// suite was structurally incapable of catching. The suite also passed the DI poll budget and the
// request-size limit to the Read client where the binary passes the Read budget and the result
// limit. Anything that shapes how a client sees the gateway belongs here, not in main.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/admin"
	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/engine"
	"github.com/NilenduGanguli/azure-gateway-api/internal/httpx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/sched"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
	"github.com/NilenduGanguli/azure-gateway-api/internal/surface"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// splitConcurrency and splitBudget bound PDF splitting across both surfaces: pdfcpu holds a whole
// document in memory, so the number of documents cut at once is what bounds the memory splitting
// can take, and the budget stops one pathological file holding a slot for the whole job deadline.
const (
	splitConcurrency = 2
	splitBudget      = 60 * time.Second
)

// Engines are the configured surfaces' engines and upstream clients. A surface that is not
// configured has nil for both.
type Engines struct {
	DI, Read             *engine.Engine
	DIClient, ReadClient *upstream.Client
}

// NewEngines builds an engine and an upstream client for each configured surface.
//
// The binary and the conformance suite both build their engines here, for the reason NewHandler
// exists: two hand-built copies of this wiring drifted before, and the suite then tested a gateway
// production did not run. Each surface gets its own scheduler, sized MaxInflight, because that is
// the container's concurrency limit and every call to it — a job's parts, its hedges, a client's
// :syncAnalyze — goes through that one scheduler. The splitter is shared, because it bounds the
// process's memory, not a container.
func NewEngines(cfg *config.Config, st *store.Store, log *slog.Logger) Engines {
	splitter := pdfsplit.NewSplitter(splitConcurrency, splitBudget)
	workRoot := jobs.WorkRoot(cfg.DataDir)
	build := func(s azerr.Surface, up config.Upstream, name string) (*engine.Engine, *upstream.Client) {
		// The blob root receives the containers' answers, so the blob sweeper reclaims a file a
		// process left behind when it died mid-download.
		client := upstream.NewClient(s, up, st.Blob.Root(), cfg.MaxResultBytes)
		e := engine.New(engine.Options{
			Client:       client,
			Sched:        sched.New(up.MaxInflight),
			Splitter:     splitter,
			SplitPages:   cfg.SplitPages,
			WorkRoot:     workRoot,
			WholeTimeout: up.Timeout,
			Log:          log.With("surface", name),
		})
		return e, client
	}
	var out Engines
	if cfg.DI.Enabled() {
		out.DI, out.DIClient = build(azerr.SurfaceDI, cfg.DI, jobs.SurfaceDI)
	}
	if cfg.Read.Enabled() {
		out.Read, out.ReadClient = build(azerr.SurfaceRead, cfg.Read, jobs.SurfaceRead)
	}
	return out
}

// Deps is everything the handler needs that outlives a request.
type Deps struct {
	Config  *config.Config
	Store   *store.Store
	Jobs    *jobs.Manager
	Logger  *slog.Logger
	Build   admin.BuildInfo
	Started time.Time
}

// probeMethods are the verbs a client might use against a routed path. A path that answers none of
// them is genuinely unrouted; one that answers another verb is a method mismatch.
var probeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost,
	http.MethodPut, http.MethodPatch, http.MethodDelete,
}

// NewHandler builds the full request chain: both Azure surfaces, the /_gw admin surface, the
// catch-all, and the middleware.
func NewHandler(d Deps) http.Handler {
	// routes carries only the real routes. It is kept separate from the served mux so the
	// catch-all can ask it whether a path is routed under some other verb — Go's ServeMux answers
	// a method mismatch with 405 and an Allow header on its own, but only while no other pattern
	// matches, and "/" matches everything.
	routes := http.NewServeMux()

	register := func(mux *http.ServeMux) {
		surface.New(surface.Deps{
			Config:  d.Config,
			Store:   d.Store,
			Jobs:    d.Jobs,
			BaseURL: httpx.BaseURLResolver{Configured: d.Config.PublicBaseURL},
		}).Register(mux)
		admin.New(d.Config, d.Store, d.Jobs, d.Build, d.Started).Register(mux)
	}

	register(routes)

	mux := http.NewServeMux()
	register(mux)
	mux.HandleFunc("/", unrouted(routes))

	return httpx.Chain(mux,
		httpx.RequestID(d.Logger),
		// Outside Recover, so the 500 a panicking handler is turned into still names Kestrel.
		httpx.ServerHeader(),
		httpx.AccessLog(),
		httpx.Recover(onPanic),
		httpx.NoCache(),
	)
}

// unrouted answers a path neither surface serves.
//
// Both containers answer a genuinely unrouted path with a bodyless 404, and matching them is safe:
// no SDK parses an unrouted response as operation state. Routed endpoints always carry a body.
//
// A method mismatch is not an unrouted path, and the containers answer it 405. Without this the
// catch-all swallowed every mismatch into the same bodyless 404, with no Allow header.
func unrouted(routes *http.ServeMux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := surfaceFor(r)
		if allow := allowedMethods(routes, r); allow != "" {
			w.Header().Set("Allow", allow)
			azerr.MethodNotAllowed(s).WriteTo(w, s)
			return
		}
		azerr.Unrouted(s).WriteTo(w, s)
	}
}

// allowedMethods returns the Allow header for a path that is routed under verbs other than the
// one used, or "" when the path is not routed at all.
func allowedMethods(routes *http.ServeMux, r *http.Request) string {
	var allowed []string
	for _, m := range probeMethods {
		if m == r.Method {
			continue
		}
		probe := r.Clone(context.Background())
		probe.Method = m
		if _, pattern := routes.Handler(probe); pattern != "" {
			allowed = append(allowed, m)
		}
	}
	// HEAD appears whenever GET is registered: Go's ServeMux serves HEAD from a GET pattern, so
	// probing reports what the mux will actually answer rather than what was written down.
	return strings.Join(allowed, ", ")
}

func onPanic(w http.ResponseWriter, r *http.Request, _ any) {
	s := surfaceFor(r)
	azerr.Internal(s, "").WriteTo(w, s)
}

// surfaceFor guesses which error vocabulary a request expects from its path.
func surfaceFor(r *http.Request) azerr.Surface {
	if strings.HasPrefix(r.URL.Path, upstream.ReadPathPrefix) {
		return azerr.SurfaceRead
	}
	return azerr.SurfaceDI
}
