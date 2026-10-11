// Package conformance drives a fully wired gateway against fake containers and asserts the
// behaviours unmodified Azure SDK clients depend on.
//
// Each test names the client that breaks if the assertion fails, because that is the only reason
// most of these rules exist.
package conformance

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/admin"
	"github.com/NilenduGanguli/azure-gateway-api/internal/app"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

const (
	apiVersion  = "2024-11-30"
	diAnalyze   = "/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=" + apiVersion
	diSync      = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=" + apiVersion
	readAnalyze = "/vision/v3.2/read/analyze"
	readSync    = "/vision/v3.2/read/syncAnalyze"
)

type harness struct {
	t       *testing.T
	gateway *httptest.Server
	handler http.Handler
	di      *mockazure.Container
	read    *mockazure.Container
	store   *store.Store
	manager *jobs.Manager
	cfg     *config.Config
}

type harnessOpts struct {
	di   mockazure.Options
	read mockazure.Options

	// paged builds both containers with mockazure.NewPaged, answering from docgen for the pages a
	// pdftest document (or a part of one) carries; diPaged and readPaged configure them, and their
	// Surface is set here. di and read are then unused.
	paged     bool
	diPaged   mockazure.PagedOptions
	readPaged mockazure.PagedOptions

	// diOnly and readOnly leave the other surface unconfigured, as a gateway fronting one kind of
	// container is deployed: no container, no upstream URL, and so no routes.
	diOnly   bool
	readOnly bool

	// splitPages is SPLIT_PAGES; 0 leaves splitting off, as the zero Config does.
	splitPages int

	tweak func(*config.Config)
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	if o.diOnly && o.readOnly {
		t.Fatal("harness: diOnly and readOnly together leave no surface")
	}

	var diC, readC *mockazure.Container
	if !o.readOnly {
		if o.paged {
			o.diPaged.Surface = mockazure.DI
			diC = mockazure.NewPaged(o.diPaged)
		} else {
			diC = mockazure.NewDI(o.di)
		}
		t.Cleanup(diC.Close)
	}
	if !o.diOnly {
		if o.paged {
			o.readPaged.Surface = mockazure.Read
			readC = mockazure.NewPaged(o.readPaged)
		} else {
			readC = mockazure.NewRead(o.read)
		}
		t.Cleanup(readC.Close)
	}

	dir := t.TempDir()
	cfg := &config.Config{
		Addr:              "127.0.0.1:0",
		DataDir:           dir,
		QueueDepth:        0,
		SplitPages:        o.splitPages,
		ResultTTL:         time.Hour,
		GCInterval:        time.Hour,
		DiskHighWatermark: 0.99,
		MaxRequestBytes:   16 << 20,
		MaxResultBytes:    64 << 20,
		ShutdownGrace:     5 * time.Second,
	}
	// An unconfigured surface has an empty Upstream, exactly as config.Load leaves it when its URL
	// is unset; Upstream.Enabled is what every layer asks.
	if diC != nil {
		cfg.DI = config.Upstream{
			BaseURL: diC.URL(), APIKey: "di-key", MaxInflight: 2, Timeout: 30 * time.Second,
		}
	}
	if readC != nil {
		cfg.Read = config.Upstream{
			BaseURL: readC.URL(), APIKey: "read-key", MaxInflight: 2, Timeout: 30 * time.Second,
		}
	}
	if o.tweak != nil {
		o.tweak(cfg)
	}

	st, err := store.Open(store.Options{
		DataDir: cfg.DataDir, ReadPoolSize: 4,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The engines come from the same constructor the binary uses. Hand-built clients here once
	// took different arguments from main's, so the suite tested a gateway production did not run.
	engines := app.NewEngines(cfg, st, log)
	manager := jobs.New(jobs.Options{
		Config: cfg, Store: st, Logger: log,
		DI: engines.DI, Read: engines.Read,
		DIClient: engines.DIClient, ReadClient: engines.ReadClient,
	})
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	manager.Recover(ctx)
	t.Cleanup(func() { cancel(); manager.Stop() })

	// The same assembly the binary uses, so the suite cannot pass on a mux production does not
	// have. Building it here by hand is what let the "/" catch-all diverge.
	handler := app.NewHandler(app.Deps{
		Config: cfg, Store: st, Jobs: manager, Logger: log, Started: time.Now(),
		Build: admin.BuildInfo{Version: "test"},
	})
	gw := httptest.NewServer(handler)
	t.Cleanup(gw.Close)

	return &harness{t: t, gateway: gw, handler: handler, di: diC, read: readC,
		store: st, manager: manager, cfg: cfg}
}

// startManager builds and starts a job manager over st the way the binary does, for tests that
// simulate a second process on the same volume. It is stopped when the test ends.
func startManager(t *testing.T, cfg *config.Config, st *store.Store) *jobs.Manager {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engines := app.NewEngines(cfg, st, log)
	manager := jobs.New(jobs.Options{
		Config: cfg, Store: st, Logger: log,
		DI: engines.DI, Read: engines.Read,
		DIClient: engines.DIClient, ReadClient: engines.ReadClient,
	})
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	t.Cleanup(func() { cancel(); manager.Stop() })
	manager.Recover(ctx)
	return manager
}

// post submits a document and returns the raw response.
func (h *harness) post(path string, body string, headers ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/pdf")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := noRedirect().Do(req)
	if err != nil {
		h.t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

// get fetches an absolute or gateway-relative URL.
func (h *harness) get(target string) *http.Response {
	h.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = h.gateway.URL + target
	}
	resp, err := noRedirect().Get(target)
	if err != nil {
		h.t.Fatalf("get %s: %v", target, err)
	}
	return resp
}

// noRedirect mirrors the .NET pipeline, which sets AllowAutoRedirect=false and treats any 3xx as
// terminal. Following redirects in tests would hide a gateway that emits one.
func noRedirect() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// pollUntil polls an operation URL until it reaches a terminal status or the deadline passes.
func (h *harness) pollUntil(opLocation string, want string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		resp := h.get(opLocation)
		body := decode(h.t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			h.t.Fatalf("poll returned %d, want 200; body %v", resp.StatusCode, body)
		}
		last = body
		status, _ := body["status"].(string)
		if status == want {
			return body
		}
		if status == "succeeded" || status == "failed" {
			return body
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("operation never reached %q; last body %v", want, last)
	return nil
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(data) == 0 {
		return map[string]any{}
	}
	if err := jsonUnmarshal(data, &out); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, truncate(string(data)))
	}
	return out
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// do sends one request and returns the response with its body already read and closed, so a
// table-driven test can assert on both without leaking a connection on a failed assertion.
// headers are name/value pairs; a Content-Type among them replaces the application/pdf default a
// request with a body gets.
func (h *harness) do(method, target string, body []byte, headers ...string) (*http.Response, []byte) {
	h.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = h.gateway.URL + target
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		h.t.Fatalf("build %s %s: %v", method, target, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/pdf")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := noRedirect().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read %s %s: %v", method, target, err)
	}
	return resp, data
}

// submit posts body to an asynchronous analyze path, requires the 202, and returns its
// Operation-Location.
func (h *harness) submit(path string, body []byte) string {
	h.t.Helper()
	resp, data := h.do(http.MethodPost, path, body)
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("POST %s: status %d, want 202: %s", path, resp.StatusCode, truncate(string(data)))
	}
	loc := resp.Header.Get("Operation-Location")
	if loc == "" {
		h.t.Fatalf("POST %s: 202 without Operation-Location", path)
	}
	return loc
}

// awaitSucceeded polls an operation to completion, requires it to have succeeded, and returns the
// terminal body's bytes exactly as served.
func (h *harness) awaitSucceeded(loc string) []byte {
	h.t.Helper()
	if body := h.pollUntil(loc, "succeeded"); body["status"] != "succeeded" {
		h.t.Fatalf("operation %s ended %v, want succeeded: %v", loc, body["status"], body["error"])
	}
	_, data := h.do(http.MethodGet, loc, nil)
	return data
}

// opID is the operation id an Operation-Location ends in, without its query.
func opID(loc string) string {
	id := loc[strings.LastIndex(loc, "/")+1:]
	if i := strings.IndexByte(id, '?'); i >= 0 {
		id = id[:i]
	}
	return id
}
