package conformance

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
)

// The routes of each surface, as a pod of the other kind answers them: not at all. A pod routes
// none of the other kind's paths, so each is Kestrel's bodyless 404, whatever the verb.
var (
	readRoutes = []struct{ method, path string }{
		{http.MethodPost, "/vision/v3.2/read/analyze"},
		{http.MethodPost, "/vision/v3.2/read/syncAnalyze"},
		{http.MethodGet, "/vision/v3.2/read/analyzeResults/" + ids.New()},
		{http.MethodHead, "/vision/v3.2/read/analyzeResults/" + ids.New()},
		{http.MethodPut, "/vision/v3.2/read/analyze"},
	}
	diRoutes = []struct{ method, path string }{
		{http.MethodPost, diAnalyze},
		{http.MethodPost, diSync},
		{http.MethodPost, frFam + "/documentModels/prebuilt-layout:analyze?api-version=" + frAPIVersion},
		{http.MethodGet, diFam + "/documentModels/prebuilt-layout/analyzeResults/" + ids.New() +
			"?api-version=" + apiVersion},
		{http.MethodDelete, diFam + "/documentModels/prebuilt-layout/analyzeResults/" + ids.New() +
			"?api-version=" + apiVersion},
		{http.MethodGet, diFam + "/documentModels/prebuilt-layout/analyzeResults/" + ids.New() + "/pdf"},
		{http.MethodGet, diFam + "/info?api-version=" + apiVersion},
		{http.MethodGet, frFam + "/documentModels?api-version=" + apiVersion},
		{http.MethodGet, diFam + "/operations?api-version=" + apiVersion},
		{http.MethodPut, diAnalyze},
	}
)

// TestSingleSurfaceGateway covers a gateway fronting one kind of container, which is how each is
// deployed when only one is licensed. It must serve its own surface as before, answer every route
// of the other kind exactly as a pod of its own kind would — a bodyless 404, never a 202 promising
// work no container can do — and report only the one upstream it has.
func TestSingleSurfaceGateway(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     harnessOpts
		served   string // the analyze path that must work
		absent   []struct{ method, path string }
		enabled  string // the /_gw/config switch that must be true
		disabled string // and the one that must be false
		inflight string // the disabled surface's inflight, which must be 0
		home     string // what the relayed home page names
	}{
		{
			name: "DI only", opts: harnessOpts{diOnly: true}, served: diAnalyze, absent: readRoutes,
			enabled: "diEnabled", disabled: "readEnabled", inflight: "readMaxInflight",
			home: "Document Intelligence",
		},
		{
			name: "Read only", opts: harnessOpts{readOnly: true}, served: readAnalyze, absent: diRoutes,
			enabled: "readEnabled", disabled: "diEnabled", inflight: "diMaxInflight",
			home: "Computer Vision Read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.opts)

			h.awaitSucceeded(h.submit(tc.served, []byte("%PDF-1.7 one surface")))

			for _, r := range tc.absent {
				resp, body := h.do(r.method, r.path, []byte("%PDF-1.7 other surface"))
				if resp.StatusCode != http.StatusNotFound {
					t.Errorf("%s %s: status %d, want the bodyless 404 of an unrouted path",
						r.method, r.path, resp.StatusCode)
					continue
				}
				bodyless(t, resp, body)
				if allow := resp.Header.Get("Allow"); allow != "" {
					t.Errorf("%s %s carries Allow %q for a path this gateway does not route",
						r.method, r.path, allow)
				}
			}
			assertNoJobsOtherThan(t, h, 1)

			// The container's root routes go to the one container there is.
			resp, body := h.do(http.MethodGet, "/", nil)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET /: status %d", resp.StatusCode)
			}
			textContains("text/html", tc.home)(t, resp, body)

			_, body = h.do(http.MethodGet, "/_gw/health", nil)
			var health struct {
				Status    string
				Upstreams []struct {
					Name string
					OK   bool
				}
			}
			if err := json.Unmarshal(body, &health); err != nil {
				t.Fatalf("/_gw/health: %v: %s", err, body)
			}
			if len(health.Upstreams) != 1 || !health.Upstreams[0].OK || health.Status != "ok" {
				t.Errorf("/_gw/health = %s, want status ok and exactly one healthy upstream", body)
			}

			_, body = h.do(http.MethodGet, "/_gw/config", nil)
			var cfg map[string]any
			if err := json.Unmarshal(body, &cfg); err != nil {
				t.Fatalf("/_gw/config: %v: %s", err, body)
			}
			if cfg[tc.enabled] != true || cfg[tc.disabled] != false {
				t.Errorf("/_gw/config %s=%v %s=%v, want true and false",
					tc.enabled, cfg[tc.enabled], tc.disabled, cfg[tc.disabled])
			}
			// Wega's observer parses the inflight numbers; a disabled surface has no capacity.
			if cfg[tc.inflight] != float64(0) {
				t.Errorf("/_gw/config %s = %v, want 0 for the disabled surface", tc.inflight, cfg[tc.inflight])
			}

			// /status reports only the configured upstream, so it is healthy.
			_, body = h.do(http.MethodGet, "/status", nil)
			jsonEquals("apiStatus", "Valid")(t, resp, body)
		})
	}
}

// assertNoJobsOtherThan fails the test unless exactly n job rows exist.
func assertNoJobsOtherThan(t *testing.T, h *harness, n int) {
	t.Helper()
	list, err := h.store.Recent(t.Context(), 1000)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(list) != n {
		t.Errorf("%d job rows exist, want %d: the other surface's routes must store nothing", len(list), n)
	}
}
