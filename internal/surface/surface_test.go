package surface_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	apiVersion = "2024-11-30"
	diAnalyze  = "/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=" + apiVersion
	diSync     = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=" + apiVersion
	readSync   = "/vision/v3.2/read/syncAnalyze"
	readSubmit = "/vision/v3.2/read/analyze"
)

// gateway is the real wiring — app.NewEngines, jobs.Manager and app.NewHandler — over fake
// containers.
type gateway struct {
	t       *testing.T
	url     string
	handler http.Handler
	di      *mockazure.Container // nil when DI is not configured
	read    *mockazure.Container // nil when Read is not configured
	cfg     *config.Config
}

// newGateway configures DI when di is non-nil and Read when read is non-nil.
func newGateway(t *testing.T, di, read *mockazure.Options, tweak func(*config.Config)) *gateway {
	t.Helper()
	g := &gateway{t: t}
	dir := t.TempDir()
	g.cfg = &config.Config{
		DataDir: dir, ResultTTL: time.Hour, GCInterval: time.Hour, DiskHighWatermark: 0.99,
		MaxRequestBytes: 1 << 20, ShutdownGrace: 5 * time.Second,
	}
	if di != nil {
		g.di = mockazure.NewDI(*di)
		t.Cleanup(g.di.Close)
		g.cfg.DI = config.Upstream{BaseURL: g.di.URL(), MaxInflight: 2, Timeout: 20 * time.Second}
	}
	if read != nil {
		g.read = mockazure.NewRead(*read)
		t.Cleanup(g.read.Close)
		g.cfg.Read = config.Upstream{BaseURL: g.read.URL(), MaxInflight: 2, Timeout: 20 * time.Second}
	}
	if tweak != nil {
		tweak(g.cfg)
	}

	st, err := store.Open(store.Options{DataDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := app.NewEngines(g.cfg, st, log)
	m := jobs.New(jobs.Options{Config: g.cfg, Store: st, Logger: log, DI: e.DI, Read: e.Read,
		DIClient: e.DIClient, ReadClient: e.ReadClient})
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop() })

	g.handler = app.NewHandler(app.Deps{Config: g.cfg, Store: st, Jobs: m, Logger: log,
		Started: time.Now(), Build: admin.BuildInfo{Version: "test"}})
	srv := httptest.NewServer(g.handler)
	t.Cleanup(srv.Close)
	g.url = srv.URL
	return g
}

func (g *gateway) do(method, path, body string) *http.Response {
	g.t.Helper()
	req, err := http.NewRequest(method, g.url+path, strings.NewReader(body))
	if err != nil {
		g.t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/pdf")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	g.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// workFolders lists what is left in the work root.
func (g *gateway) workFolders() []string {
	entries, _ := os.ReadDir(jobs.WorkRoot(g.cfg.DataDir))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

// apiError is the DI error shape.
type apiError struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		InnerError struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"innererror"`
	} `json:"error"`
}

func decodeError(t *testing.T, data []byte) apiError {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, data)
	}
	return e
}

// TestParameterPolicy checks every row of the sync-only spec's §2 table on both DI analyze routes.
// A refused request is answered before admission: nothing is stored and the container is never
// called.
func TestParameterPolicy(t *testing.T) {
	// Room for every accepted request at once: the forwarded rows below must not meet the
	// gateway's own 429 while earlier jobs are still running.
	g := newGateway(t, &mockazure.Options{}, &mockazure.Options{}, func(c *config.Config) {
		c.QueueDepth = 64
	})

	refused := []struct{ query, message string }{
		{"output=pdf", "output=pdf and output=figures are not supported by this gateway."},
		{"output=figures", "output=pdf and output=figures are not supported by this gateway."},
		{"output=json,PDF", "output=pdf and output=figures are not supported by this gateway."},
		{"output=json&output=Figures", "output=pdf and output=figures are not supported by this gateway."},
		{"outputContentFormat=markdown", "outputContentFormat=markdown is not supported by this gateway yet."},
		{"features=keyValuePairs", "features is not supported by this gateway yet."},
		{"Features=ocrHighResolution", "features is not supported by this gateway yet."},
		{"queryFields=Total", "queryFields is not supported by this gateway yet."},
		{"queryFields=", "queryFields is not supported by this gateway yet."},
	}
	for _, route := range []string{diAnalyze, diSync} {
		for _, tc := range refused {
			t.Run(route[strings.Index(route, ":"):strings.Index(route, "?")]+" "+tc.query, func(t *testing.T) {
				resp := g.do(http.MethodPost, route+"&"+tc.query, "%PDF-1.7 fake")
				data := readAll(t, resp)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status %d, want 400: %s", resp.StatusCode, data)
				}
				e := decodeError(t, data)
				if e.Error.Code != "InvalidArgument" || e.Error.InnerError.Code != "InvalidParameter" ||
					e.Error.InnerError.Message != tc.message {
					t.Errorf("error %+v, want InvalidArgument/InvalidParameter %q", e.Error, tc.message)
				}
				if v := resp.Header.Get("Retry-After"); v != "" {
					t.Errorf("Retry-After %q on a 400", v)
				}
			})
		}
	}
	if n := g.di.SyncCalls(); n != 0 {
		t.Errorf("the container saw %d calls for refused requests", n)
	}
	if left := g.workFolders(); len(left) != 0 {
		t.Errorf("refused requests left work folders: %v", left)
	}

	forwarded := []string{"outputContentFormat=text", "outputContentFormat=TEXT", "features=",
		"output=json", "pages=1-2&locale=en-US&stringIndexType=utf16CodeUnit", "_overload=x"}
	for _, q := range forwarded {
		t.Run("forwarded "+q, func(t *testing.T) {
			if resp := g.do(http.MethodPost, diAnalyze+"&"+q, "%PDF-1.7 fake"); resp.StatusCode != http.StatusAccepted {
				t.Errorf(":analyze with %s: status %d, want 202", q, resp.StatusCode)
			}
			if resp := g.do(http.MethodPost, diSync+"&"+q, "%PDF-1.7 fake"); resp.StatusCode != http.StatusOK {
				t.Errorf(":syncAnalyze with %s: status %d, want 200", q, resp.StatusCode)
			}
		})
	}

	// Read has no such policy: its sync route is documented to take its async route's parameters.
	if resp := g.do(http.MethodPost, readSubmit+"?features=x&output=pdf", "img"); resp.StatusCode != http.StatusAccepted {
		t.Errorf("Read submit with DI-only parameters: status %d, want 202", resp.StatusCode)
	}
}

// TestSyncAnalyzeAnswersInTheContainersShape checks the success path of the engine-backed sync
// route: a 200 with an exact Content-Length, carrying the container's result in the shape its
// sync route used — a completed operation envelope, or a bare AnalyzeResult — and nothing left
// in the work folder afterwards.
func TestSyncAnalyzeAnswersInTheContainersShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		bare bool
		path string
	}{
		{"DI envelope", false, diSync},
		{"DI bare", true, diSync},
		{"Read envelope", false, readSync},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := &mockazure.Options{SyncBodyBare: tc.bare}
			g := newGateway(t, opts, opts, nil)
			started := time.Now().UTC().Truncate(time.Second)
			resp := g.do(http.MethodPost, tc.path, "%PDF-1.7 fake")
			data := readAll(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200: %s", resp.StatusCode, data)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type %q", ct)
			}
			if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(data)) {
				t.Errorf("Content-Length %s for a %d-byte body", cl, len(data))
			}
			if resp.Header.Get("Operation-Location") != "" {
				t.Error("a synchronous answer carries Operation-Location")
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatalf("body is not a JSON object: %v", err)
			}
			if tc.bare {
				if _, ok := body["status"]; ok {
					t.Error("a bare result was wrapped in an envelope")
				}
				if _, ok := body["modelId"]; !ok {
					t.Errorf("bare result has no modelId: %s", data)
				}
			} else {
				var env struct {
					Status, CreatedDateTime, LastUpdatedDateTime string
				}
				_ = json.Unmarshal(data, &env)
				if env.Status != "succeeded" || body["analyzeResult"] == nil {
					t.Errorf("not a succeeded envelope: %s", data)
				}
				created, err := time.Parse(jobs.TimeFormat, env.CreatedDateTime)
				if err != nil || created.Before(started) {
					t.Errorf("createdDateTime %q is not when the request arrived", env.CreatedDateTime)
				}
				if _, err := time.Parse(jobs.TimeFormat, env.LastUpdatedDateTime); err != nil {
					t.Errorf("lastUpdatedDateTime %q: %v", env.LastUpdatedDateTime, err)
				}
			}
			if left := g.workFolders(); len(left) != 0 {
				t.Errorf("the request left work folders: %v", left)
			}
		})
	}
}

// TestSyncAnalyzeRelaysTheContainersError checks that a container failure reaches the client as
// the surface's own error at the container's status.
func TestSyncAnalyzeRelaysTheContainersError(t *testing.T) {
	g := newGateway(t, &mockazure.Options{Sync: mockazure.SyncModelNotFound}, nil, nil)
	resp := g.do(http.MethodPost, diSync, "%PDF-1.7 fake")
	data := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want the container's 404: %s", resp.StatusCode, data)
	}
	if e := decodeError(t, data); e.Error.Code != "NotFound" || e.Error.InnerError.Code != "ModelNotFound" {
		t.Errorf("error %+v, want the container's NotFound/ModelNotFound", e.Error)
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		t.Errorf("Retry-After %q on a 404", v)
	}
}

// TestSyncAnalyzeBusy checks the gateway's own backpressure: with every admission slot taken, a
// synchronous call is refused with 429 and Retry-After: 5 rather than queued.
func TestSyncAnalyzeBusy(t *testing.T) {
	g := newGateway(t, &mockazure.Options{Latency: 2 * time.Second}, nil, func(c *config.Config) {
		c.DI.MaxInflight = 1
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest(http.MethodPost, g.url+diSync, strings.NewReader("%PDF-1.7 fake"))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for g.di.SyncCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	resp := g.do(http.MethodPost, diSync, "%PDF-1.7 fake")
	_ = readAll(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status %d, want 429", resp.StatusCode)
	}
	if v := resp.Header.Get("Retry-After"); v != "5" {
		t.Errorf("Retry-After %q, want 5", v)
	}
	wg.Wait()
}

// TestSyncAnalyzeRefusesAnOversizedBody checks MAX_REQUEST_BYTES on the sync route: the documented
// 400, and no work folder left behind.
func TestSyncAnalyzeRefusesAnOversizedBody(t *testing.T) {
	g := newGateway(t, &mockazure.Options{}, nil, func(c *config.Config) { c.MaxRequestBytes = 8 })
	resp := g.do(http.MethodPost, diSync, "%PDF-1.7 far more than eight bytes")
	data := readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, data)
	}
	if e := decodeError(t, data); e.Error.InnerError.Code != "InvalidContentLength" {
		t.Errorf("error %+v, want InvalidContentLength", e.Error)
	}
	if g.di.SyncCalls() != 0 {
		t.Error("an oversized body reached the container")
	}
	if left := g.workFolders(); len(left) != 0 {
		t.Errorf("the request left work folders: %v", left)
	}
}

// TestSyncAnalyzeWritesNothingAfterTheClientLeaves checks that a client that hangs up mid-analysis
// is sent nothing, and that its saved body is removed.
func TestSyncAnalyzeWritesNothingAfterTheClientLeaves(t *testing.T) {
	g := newGateway(t, &mockazure.Options{Sync: mockazure.SyncHang}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, diSync, strings.NewReader("%PDF-1.7 fake")).
		WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { g.handler.ServeHTTP(rec, req); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for g.di.SyncCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after the client left")
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Errorf("wrote %q (Content-Type %q) to a client that had gone",
			rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	if left := g.workFolders(); len(left) != 0 {
		t.Errorf("the request left work folders: %v", left)
	}
}

// TestResultFilesAreNeverProduced checks the artifact routes: no job has result files, so a known
// operation answers the container's missing-file 404 and an unknown one the unknown-operation 404.
func TestResultFilesAreNeverProduced(t *testing.T) {
	g := newGateway(t, &mockazure.Options{}, nil, nil)
	resp := g.do(http.MethodPost, diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	id := loc[strings.LastIndex(loc, "/")+1 : strings.Index(loc, "?")]
	base := "/documentintelligence/documentModels/prebuilt-layout/analyzeResults/"
	unknown := "00000000-0000-4000-8000-000000000000"

	for _, suffix := range []string{"/pdf", "/figures/1.1"} {
		t.Run("known"+suffix, func(t *testing.T) {
			resp := g.do(http.MethodGet, base+id+suffix+"?api-version="+apiVersion, "")
			data := readAll(t, resp)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status %d, want 404", resp.StatusCode)
			}
			e := decodeError(t, data)
			if e.Error.Code != "NotFound" || e.Error.InnerError.Code != "OperationNotFound" ||
				e.Error.Message != "The requested result file was not produced for this operation." {
				t.Errorf("error %+v", e.Error)
			}
		})
		t.Run("unknown"+suffix, func(t *testing.T) {
			resp := g.do(http.MethodGet, base+unknown+suffix+"?api-version="+apiVersion, "")
			data := readAll(t, resp)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status %d, want 404", resp.StatusCode)
			}
			// The flat shape the DI container uses for an unknown result id.
			var flat struct{ Code, Message string }
			if err := json.Unmarshal(data, &flat); err != nil || flat.Code == "" {
				t.Errorf("body %s is not the flat unknown-operation error", data)
			}
		})
	}
}

// TestPassthroughRoutesRelayTheContainer checks each GET passthrough against the container itself:
// the same status, Content-Type and body.
func TestPassthroughRoutesRelayTheContainer(t *testing.T) {
	g := newGateway(t, &mockazure.Options{}, &mockazure.Options{}, nil)
	for _, path := range []string{
		"/",
		"/swagger",
		"/swagger/index.html",
		"/swagger/v1/swagger.json",
		"/records/usage-logs",
		"/records/usage-logs/9/2026",
		"/records/usage-logs/13/2026",
		"/documentintelligence/operations?api-version=" + apiVersion,
		"/documentintelligence/operations/abc?api-version=" + apiVersion,
		"/formrecognizer/operations?api-version=" + apiVersion,
		"/documentintelligence/info?api-version=" + apiVersion,
	} {
		t.Run(path, func(t *testing.T) {
			direct, err := http.Get(g.di.URL() + path)
			if err != nil {
				t.Fatalf("direct GET: %v", err)
			}
			want := readAll(t, direct)
			_ = direct.Body.Close()

			resp := g.do(http.MethodGet, path, "")
			got := readAll(t, resp)
			if resp.StatusCode != direct.StatusCode {
				t.Errorf("status %d, the container's is %d", resp.StatusCode, direct.StatusCode)
			}
			if a, b := resp.Header.Get("Content-Type"), direct.Header.Get("Content-Type"); a != b {
				t.Errorf("Content-Type %q, the container's is %q", a, b)
			}
			// The container's errors are relayed in its own shape; anything else byte for byte.
			if direct.StatusCode < 400 && string(got) != string(want) {
				t.Errorf("body %q, the container's is %q", got, want)
			}
		})
	}
}

// TestUsageLogMonthAndYearMustBeDigits checks the guard on /records/usage-logs/{month}/{year}:
// anything but digits is a path the container does not route, answered without calling it.
func TestUsageLogMonthAndYearMustBeDigits(t *testing.T) {
	g := newGateway(t, &mockazure.Options{}, nil, nil)
	for _, path := range []string{"/records/usage-logs/ab/2026", "/records/usage-logs/9/20x6",
		"/records/usage-logs/-1/2026"} {
		resp := g.do(http.MethodGet, path, "")
		if data := readAll(t, resp); resp.StatusCode != http.StatusNotFound || len(data) != 0 {
			t.Errorf("%s: status %d with %d bytes, want the bodyless 404", path, resp.StatusCode,
				len(data))
		}
	}
}

// TestOnlyConfiguredSurfacesAreRouted checks registration with one container: the other surface's
// routes answer the bodyless 404 of an unrouted path, and the root routes go to the one present.
func TestOnlyConfiguredSurfacesAreRouted(t *testing.T) {
	t.Run("DI only", func(t *testing.T) {
		g := newGateway(t, &mockazure.Options{}, nil, nil)
		for _, p := range []string{readSubmit, readSync} {
			resp := g.do(http.MethodPost, p, "img")
			if data := readAll(t, resp); resp.StatusCode != http.StatusNotFound || len(data) != 0 {
				t.Errorf("POST %s: status %d with %d bytes, want the bodyless 404", p,
					resp.StatusCode, len(data))
			}
		}
		if resp := g.do(http.MethodPost, diAnalyze, "%PDF-1.7 fake"); resp.StatusCode != http.StatusAccepted {
			t.Errorf("DI submit: status %d, want 202", resp.StatusCode)
		}
		if body := readAll(t, g.do(http.MethodGet, "/", "")); !strings.Contains(string(body), "Document Intelligence") {
			t.Errorf("GET / did not reach the DI container: %s", body)
		}
	})
	t.Run("Read only", func(t *testing.T) {
		g := newGateway(t, nil, &mockazure.Options{}, nil)
		for _, p := range []string{diAnalyze, diSync} {
			resp := g.do(http.MethodPost, p, "%PDF-1.7 fake")
			if data := readAll(t, resp); resp.StatusCode != http.StatusNotFound || len(data) != 0 {
				t.Errorf("POST %s: status %d with %d bytes, want the bodyless 404", p,
					resp.StatusCode, len(data))
			}
		}
		if resp := g.do(http.MethodGet, "/documentintelligence/info", ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("DI metadata: status %d, want 404", resp.StatusCode)
		}
		if resp := g.do(http.MethodPost, readSubmit, "img"); resp.StatusCode != http.StatusAccepted {
			t.Errorf("Read submit: status %d, want 202", resp.StatusCode)
		}
		if body := readAll(t, g.do(http.MethodGet, "/", "")); !strings.Contains(string(body), "Computer Vision Read") {
			t.Errorf("GET / did not reach the Read container: %s", body)
		}
	})
}

// TestSyncBodiesLiveUnderTheWorkRoot pins where :syncAnalyze saves bodies, which the sweeper
// relies on.
func TestSyncBodiesLiveUnderTheWorkRoot(t *testing.T) {
	if got, want := jobs.WorkRoot("/data"), filepath.Join("/data", "work"); got != want {
		t.Errorf("WorkRoot is %s, want %s", got, want)
	}
}
