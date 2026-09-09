package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
)

// TestProbeTimeoutBoundsTheAnswerNotTheDownload is a regression test.
//
// DI_SYNC_PROBE_TIMEOUT exists to stop auto mode burning the whole upstream timeout on a
// :syncAnalyze route that never answers. It was applied by deriving the request from a
// context.WithTimeout, which also bounded the streaming of the analyzeResult — so a large result
// on a route that answered promptly failed anyway once the transfer outlived the probe bound.
//
// The failure mode was the bad part: the error is a context.DeadlineExceeded, and the worker
// classifies that as a shutdown and leaves the job recoverable rather than failing it. The job
// never completed and never failed; it was retried on every recovery pass and hit the same wall.
func TestProbeTimeoutBoundsTheAnswerNotTheDownload(t *testing.T) {
	const probe = 400 * time.Millisecond

	container := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Headers are answered immediately: the route is demonstrably alive.
		w.(http.Flusher).Flush()
		// The body then takes materially longer than the probe bound, as a real 200 MB
		// analyzeResult does over a busy cluster network.
		_, _ = w.Write([]byte(`{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30",`))
		w.(http.Flusher).Flush()
		time.Sleep(3 * probe)
		_, _ = w.Write([]byte(`"content":"hello","pages":[]}}`))
	}))
	defer container.Close()

	client := NewDI(config.Upstream{
		BaseURL: container.URL, MaxInflight: 1, Timeout: 30 * time.Second,
	}, config.SyncAuto, 5, t.TempDir(), 1<<20, probe)

	res, err := client.Analyze(context.Background(), Document{
		ContentType: "application/pdf",
		Size:        4,
		Open:        func() (readCloser, error) { return nopCloser("data"), nil },
	}, Request{ModelID: "prebuilt-layout"})

	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the probe bound cut off the result download on a route that answered promptly; " +
			"the worker reads this as a shutdown, so the job never completes and never fails")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("no result")
	}
}

// TestGenericErrorPreservesTheUpstreamStatus is a regression test.
//
// An unparseable or bodyless upstream failure used to collapse to 400 for the whole 4xx range and
// 500 for the whole 5xx range. A container 404 for an unknown model was reported as
// "InvalidRequest", and a 503 during a rolling restart as a flat 500 — telling the client the
// request was malformed, or the gateway broken, when the correct answer was retriable.
//
// The synthesised marker matters as much as the status: resourceScoped404 must still be able to
// tell a manufactured 404 from one the container actually described, or the :syncAnalyze
// capability latch stops working.
func TestGenericErrorPreservesTheUpstreamStatus(t *testing.T) {
	b := &base{surface: azerr.SurfaceDI, name: "document-intelligence"}

	for _, tc := range []struct {
		status   int
		wantCode string
	}{
		{http.StatusNotFound, azerr.CodeNotFound},
		{http.StatusServiceUnavailable, azerr.CodeServiceUnavailable},
		{http.StatusConflict, azerr.CodeConflict},
		{http.StatusUnauthorized, azerr.CodeUnauthorized},
	} {
		got := b.genericError(tc.status)
		if got.Status != tc.status {
			t.Errorf("HTTP %d became %d", tc.status, got.Status)
		}
		if got.Code != tc.wantCode {
			t.Errorf("HTTP %d got code %q, want %q", tc.status, got.Code, tc.wantCode)
		}
		if !got.Synthesised() {
			t.Errorf("HTTP %d: manufactured error is not marked synthesised; the sync capability "+
				"latch will mistake it for something the container said", tc.status)
		}
	}
}
