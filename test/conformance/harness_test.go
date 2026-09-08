// Package conformance drives a fully wired gateway against fake containers and asserts the
// behaviours unmodified Azure SDK clients depend on.
//
// Each test names the client that breaks if the assertion fails, because that is the only reason
// most of these rules exist.
package conformance

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/admin"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/httpx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
	"github.com/NilenduGanguli/azure-gateway-api/internal/surface"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
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
	di    mockazure.Options
	read  mockazure.Options
	tweak func(*config.Config)
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()

	diC := mockazure.NewDI(o.di)
	readC := mockazure.NewRead(o.read)
	t.Cleanup(diC.Close)
	t.Cleanup(readC.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		Addr:                  "127.0.0.1:0",
		DataDir:               dir,
		AllowNetworkFS:        true,
		TrustForwardedHeaders: false,
		DISyncMode:            config.SyncAuto,
		DIBlindPollBudget:     10,
		QueueDepth:            0,
		ResultTTL:             time.Hour,
		GCInterval:            time.Hour,
		DiskHighWatermark:     0.99,
		MaxRequestBytes:       16 << 20,
		PollRetryAfter:        1,
		BusyRetryAfter:        5,
		ShutdownGrace:         5 * time.Second,
		DI: config.Upstream{
			BaseURL: diC.URL(), APIKey: "di-key", MaxInflight: 2, Timeout: 30 * time.Second,
		},
		Read: config.Upstream{
			BaseURL: readC.URL(), APIKey: "read-key", MaxInflight: 2, Timeout: 30 * time.Second,
		},
	}
	if o.tweak != nil {
		o.tweak(cfg)
	}

	st, err := store.Open(store.Options{
		DataDir: cfg.DataDir, AllowNetworkFS: true, ReadPoolSize: 4,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := jobs.New(jobs.Options{
		Config: cfg, Store: st, Logger: log,
		DI:   upstream.NewDI(cfg.DI, cfg.DISyncMode, cfg.DIBlindPollBudget, st.Blob.Root(), cfg.MaxRequestBytes),
		Read: upstream.NewRead(cfg.Read, cfg.DIBlindPollBudget, st.Blob.Root(), cfg.MaxRequestBytes),
	})
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	manager.Recover(ctx)
	t.Cleanup(func() { cancel(); manager.Stop() })

	mux := http.NewServeMux()
	surface.New(surface.Deps{
		Config: cfg, Store: st, Jobs: manager,
		BaseURL: httpx.BaseURLResolver{
			Configured:     cfg.PublicBaseURL,
			TrustForwarded: cfg.TrustForwardedHeaders,
			AllowedHosts:   cfg.TrustedForwardedHosts,
		},
	}).Register(mux)
	admin.New(cfg, st, manager, admin.BuildInfo{Version: "test"}, time.Now()).Register(mux)

	handler := httpx.Chain(mux,
		httpx.RequestID(log), httpx.Recover(func(http.ResponseWriter, *http.Request, any) {}),
	)
	gw := httptest.NewServer(handler)
	t.Cleanup(gw.Close)

	return &harness{t: t, gateway: gw, handler: handler, di: diC, read: readC,
		store: st, manager: manager, cfg: cfg}
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
