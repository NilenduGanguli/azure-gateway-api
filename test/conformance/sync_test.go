package conformance

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"

	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

// TestSyncPassthroughResolvesADegraded202 is a regression test for the worst client-visible
// failure the review found.
//
// The container's synchronous route is not contractually synchronous — it degrades to 202 under
// memory pressure. Relaying that 202 handed the caller an empty body and no operation id, because
// the header carrying it is dropped (it names an in-cluster address, and the Read container
// corrupts it outright). The analysis was paid for and unreachable. The gateway now polls it out.
func TestSyncPassthroughResolvesADegraded202(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		opts harnessOpts
	}{
		{"document intelligence", diSync, harnessOpts{
			di: mockazure.Options{Sync: mockazure.Sync202, PollsBeforeSuccess: 2}}},
		{"computer vision read", readSync, harnessOpts{
			read: mockazure.Options{Sync: mockazure.Sync202, PollsBeforeSuccess: 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts)
			resp := h.post(tc.path, "%PDF-1.7 fake")
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode == http.StatusAccepted {
				t.Fatal("the gateway relayed the container's 202; the caller has no operation id " +
					"and no result, and the analysis has already been paid for")
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("got %d, want 200", resp.StatusCode)
			}
			body := decode(t, resp)
			if body["status"] != "succeeded" {
				t.Errorf("status is %v, want succeeded", body["status"])
			}
			if _, ok := body["analyzeResult"]; !ok {
				t.Error("resolved synchronous response carries no analyzeResult")
			}
			if resp.Header.Get("Operation-Location") != "" {
				t.Error("a synchronous response must not carry Operation-Location")
			}
		})
	}
}

// TestPollNeverReturnsABodyWithoutStatus is a regression test.
//
// A succeeded row whose result blob has gone answered with a bare 500. Java's poller never checks
// the poll status code — it deserialises, finds no status, and throws an opaque
// NullPointerException. The operation is now reported as failed at 200, which every client handles.
func TestPollNeverReturnsABodyWithoutStatus(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	h.pollUntil(loc, "succeeded")

	id := loc[strings.LastIndex(loc, "/")+1:]
	if i := strings.Index(id, "?"); i >= 0 {
		id = id[:i]
	}
	// Simulate the blob going missing while the row survives — the state a partial delete or a
	// failed unlink used to leave behind.
	if err := h.store.Blob.Remove(id, store.KindResult); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	poll := h.get(loc)
	defer func() { _ = poll.Body.Close() }()
	if poll.StatusCode != http.StatusOK {
		t.Fatalf("poll returned %d; a poll must answer 200 with a status, not an HTTP error",
			poll.StatusCode)
	}
	body := decode(t, poll)
	if body["status"] != "failed" {
		t.Errorf("status is %v, want failed", body["status"])
	}
	if _, ok := body["error"]; !ok {
		t.Error("the failed envelope carries no error object")
	}
}

// TestResourceScoped404DoesNotDisableTheSyncRoute is a regression test.
//
// Latching the synchronous capability off is process-wide and permanent, so treating every 404 as
// "this route does not exist" meant one request naming a missing model disabled the fast path for
// every later job until the pod restarted.
func TestResourceScoped404DoesNotDisableTheSyncRoute(t *testing.T) {
	h := newHarness(t, harnessOpts{di: mockazure.Options{Sync: mockazure.SyncModelNotFound}})

	// A job against the model the container rejects.
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	h.pollUntil(loc, "failed")

	before := countCalls(h.di.Requests(), ":syncAnalyze")
	if before == 0 {
		t.Fatal("the synchronous route was never tried")
	}

	// A later job must still try the synchronous route.
	resp2 := h.post(diAnalyze, "%PDF-1.7 fake")
	loc2 := resp2.Header.Get("Operation-Location")
	_ = resp2.Body.Close()
	h.pollUntil(loc2, "failed")

	if after := countCalls(h.di.Requests(), ":syncAnalyze"); after == before {
		t.Error("a model-not-found 404 latched the synchronous capability off; only a 404 that " +
			"means the route is unrouted should do that")
	}
}

// TestSyncPassthroughNormalisesUpstreamErrors covers the container's unschematized error class —
// HTML from an nginx sidecar, a bare {"status":"Failed"}, a bodyless Kestrel 404 — none of which a
// client's SDK can parse, and a Retry-After that must not survive onto a non-retriable status.
func TestSyncPassthroughNormalisesUpstreamErrors(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{SyncErrorStatus: http.StatusRequestEntityTooLarge},
	})

	resp := h.post(diSync, "%PDF-1.7 fake")
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type is %q; the upstream's HTML reached the client", ct)
	}
	body := decode(t, resp)
	if body["error"] == nil {
		t.Errorf("body is not a Document Intelligence error object: %v", body)
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		t.Errorf("Retry-After %q was relayed onto a %d; azure-core retries any >=400 carrying it "+
			"ten times", v, resp.StatusCode)
	}
}

// TestClientRequestIDIsEchoed checks the correlation header round-trips, which is the first thing
// anyone debugging a stuck operation reaches for.
func TestClientRequestIDIsEchoed(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	const cid = "11111111-2222-4333-8444-555555555555"

	resp := h.post(diAnalyze, "%PDF-1.7 fake", "x-ms-client-request-id", cid)
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("x-ms-client-request-id"); got != cid {
		t.Errorf("x-ms-client-request-id echoed as %q, want %q", got, cid)
	}
	if resp.Header.Get("apim-request-id") == "" {
		t.Error("apim-request-id is missing")
	}
}

// TestReadSurfaceHasNoDeleteRoute checks that the gateway does not invent an endpoint. The Read
// contract has no DELETE on a read result.
func TestReadSurfaceHasNoDeleteRoute(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	resp := h.post(readAnalyze, "fake-image")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, loc, nil)
	del, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer func() { _ = del.Body.Close() }()

	if del.StatusCode == http.StatusNoContent {
		t.Error("the Read surface served a DELETE, which its contract does not define")
	}
}

// TestLegacyFormRecognizerPrefixIsServed covers a family the design originally excluded.
//
// A probe found the layout-4.0 container declaring /formrecognizer/documentModels/{modelId}:analyze,
// :syncAnalyze and the matching analyzeResults route alongside the /documentintelligence ones. A
// client on azure-ai-formrecognizer therefore reaches the container today, and a gateway serving
// only the newer prefix would have 404'd it.
//
// The prefix the caller arrived on must come back in Operation-Location: every SDK derives the
// operation id from that header with a regex hard-coding its own family.
func TestLegacyFormRecognizerPrefixIsServed(t *testing.T) {
	h := newHarness(t, harnessOpts{di: mockazure.Options{Sync: mockazure.Sync200}})

	resp := h.post("/formrecognizer/documentModels/prebuilt-layout:analyze?api-version="+apiVersion,
		"%PDF-1.7 fake")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("legacy submit returned %d, want 202", resp.StatusCode)
	}
	loc := resp.Header.Get("Operation-Location")
	if !strings.Contains(loc, "/formrecognizer/") {
		t.Fatalf("Operation-Location is %q; it must echo the caller's own family, or the SDK "+
			"regex that derives the operation id will not match", loc)
	}
	if strings.Contains(loc, "/documentintelligence/") {
		t.Errorf("Operation-Location switched families: %q", loc)
	}

	body := h.pollUntil(loc, "succeeded")
	if body["status"] != "succeeded" {
		t.Fatalf("status is %v, want succeeded", body["status"])
	}
	if _, ok := body["analyzeResult"]; !ok {
		t.Error("terminal body has no analyzeResult")
	}
}

// TestHungSyncRouteFallsBackQuickly is a regression test for the worst operational trap the probe
// found: a container that declares :syncAnalyze in its own swagger and then never answers it.
//
// Without a bound of its own, the attempt inherits DI_UPSTREAM_TIMEOUT — fifteen minutes by
// default — and every job pays it before falling back to the route that works. The probe timeout
// caps the attempt, and the capability latches off so later jobs skip it entirely.
func TestHungSyncRouteFallsBackQuickly(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{Sync: mockazure.SyncHang},
		tweak: func(c *config.Config) {
			c.DISyncProbeTimeout = 2 * time.Second
			c.DI.Timeout = 60 * time.Second // what the attempt would otherwise inherit
		},
	})

	started := time.Now()
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	h.pollUntil(loc, "succeeded")
	elapsed := time.Since(started)

	if elapsed > 30*time.Second {
		t.Errorf("the job took %v; a hung synchronous route must be abandoned at the probe "+
			"timeout, not at the upstream timeout", elapsed)
	}
	if !h.di.Called(":analyze") {
		t.Error("the gateway never fell back to :analyze")
	}
	assertJobMode(t, h, "async-fallback")

	// A second job must not pay the probe again.
	before := countCalls(h.di.Requests(), ":syncAnalyze")
	resp2 := h.post(diAnalyze, "%PDF-1.7 fake")
	loc2 := resp2.Header.Get("Operation-Location")
	_ = resp2.Body.Close()
	h.pollUntil(loc2, "succeeded")

	if after := countCalls(h.di.Requests(), ":syncAnalyze"); after != before {
		t.Errorf("the hung route was probed again on a later job (%d then %d); the capability "+
			"should have latched off", before, after)
	}
}
