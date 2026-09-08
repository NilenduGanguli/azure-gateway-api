package conformance

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// TestDIUsesSyncAnalyzeWhenAvailable checks the happy path: the undocumented synchronous route is
// preferred, which keeps the upstream call stateless and immune to autoscaling.
func TestDIUsesSyncAnalyzeWhenAvailable(t *testing.T) {
	h := newHarness(t, harnessOpts{di: mockazure.Options{Sync: mockazure.Sync200}})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	h.pollUntil(loc, "succeeded")

	if !h.di.Called(":syncAnalyze") {
		t.Error("gateway did not use :syncAnalyze although the container serves it")
	}
	if h.di.Called("analyzeResults") {
		t.Error("gateway polled the container even though the synchronous call succeeded")
	}
}

// TestDISyncDegradesTo202AndStillSucceeds covers the case Microsoft called "not standard or
// documented behavior": the synchronous route answers 202 under memory pressure, leaving an
// operation that only the answering replica knows about.
func TestDISyncDegradesTo202AndStillSucceeds(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{Sync: mockazure.Sync202, PollsBeforeSuccess: 2},
	})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit returned %d, want 202", resp.StatusCode)
	}
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	body := h.pollUntil(loc, "succeeded")
	if body["status"] != "succeeded" {
		t.Fatalf("status is %v, want succeeded", body["status"])
	}
	if !h.di.Called("analyzeResults") {
		t.Error("gateway did not poll the container after the synchronous call degraded to 202")
	}
	assertJobMode(t, h, "degraded-202")
}

// TestDIFallsBackWhenSyncRouteMissing covers image builds that do not serve :syncAnalyze at all.
func TestDIFallsBackWhenSyncRouteMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode mockazure.SyncBehavior
	}{
		{"404", mockazure.Sync404},
		{"500 UnhandledEndpointException", mockazure.Sync500Unhandled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{di: mockazure.Options{Sync: tc.mode}})

			resp := h.post(diAnalyze, "%PDF-1.7 fake")
			loc := resp.Header.Get("Operation-Location")
			_ = resp.Body.Close()
			h.pollUntil(loc, "succeeded")

			if !h.di.Called(":analyze") {
				t.Error("gateway never fell back to :analyze")
			}
			assertJobMode(t, h, "async-fallback")

			// The capability latch means a second job must not re-probe the missing route.
			before := countCalls(h.di.Requests(), ":syncAnalyze")
			resp2 := h.post(diAnalyze, "%PDF-1.7 fake")
			loc2 := resp2.Header.Get("Operation-Location")
			_ = resp2.Body.Close()
			h.pollUntil(loc2, "succeeded")

			if after := countCalls(h.di.Requests(), ":syncAnalyze"); after != before {
				t.Errorf("gateway re-probed :syncAnalyze on a later job (%d then %d calls); the "+
					"capability should latch off", before, after)
			}
		})
	}
}

// TestPollSurvivesWrongReplica404 is the core resilience case. The containers' default result
// store is instance-local, so a poll routed to another replica 404s. The gateway must absorb that
// and keep trying rather than failing the client's operation.
func TestPollSurvivesWrongReplica404(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{
			Sync:               mockazure.Sync202,
			PollsBeforeSuccess: 3,
			WrongReplicaEvery:  2, // every other poll lands on a replica with no record
		},
	})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	body := h.pollUntil(loc, "succeeded")
	if body["status"] != "succeeded" {
		t.Fatalf("status is %v; the gateway failed an operation it should have recovered", body["status"])
	}
}

// TestBlindPollBudgetIsBounded checks the other side of that tolerance: an operation whose owning
// replica is genuinely gone must fail rather than poll forever.
func TestBlindPollBudgetIsBounded(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{
			Sync:               mockazure.Sync202,
			PollsBeforeSuccess: 1000,
			WrongReplicaEvery:  1, // every poll misses: the owner is gone
		},
		tweak: func(c *config.Config) { c.DIBlindPollBudget = 3 },
	})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	body := h.pollUntil(loc, "failed")
	if body["status"] != "failed" {
		t.Fatalf("status is %v, want failed once the blind-poll budget is spent", body["status"])
	}
}

// TestReadAlwaysUsesSyncAnalyze checks the configured Read strategy.
func TestReadAlwaysUsesSyncAnalyze(t *testing.T) {
	h := newHarness(t, harnessOpts{read: mockazure.Options{Sync: mockazure.Sync200}})

	resp := h.post(readAnalyze, "fake-image")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	h.pollUntil(loc, "succeeded")

	if !h.read.Called("/syncAnalyze") {
		t.Error("Read job did not use syncAnalyze")
	}
	if h.read.Called("/read/analyze") {
		t.Error("Read job used the asynchronous route although syncAnalyze was available")
	}
}

// TestReadBareFailedStatusIsTranslated covers the only error form Microsoft documents for CV
// syncAnalyze: {"status":"Failed"} with a capital F, no code and no message. It is unactionable,
// so the gateway must translate it rather than pass it on.
func TestReadBareFailedStatusIsTranslated(t *testing.T) {
	h := newHarness(t, harnessOpts{read: mockazure.Options{FailSyncBare: true}})

	resp := h.post(readAnalyze, "fake-image")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	body := h.pollUntil(loc, "failed")
	if body["status"] != "failed" {
		t.Fatalf("status is %v, want lowercase failed", body["status"])
	}
	if body["status"] == "Failed" {
		t.Error("capital-F Failed reached the client; only the four canonical strings are safe")
	}
}

// TestBareAnalyzeResultShapeAccepted covers the unverified question of whether the DI synchronous
// route returns an operation envelope or a bare AnalyzeResult. Both must work.
func TestBareAnalyzeResultShapeAccepted(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{Sync: mockazure.Sync200, SyncBodyBare: true},
	})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	body := h.pollUntil(loc, "succeeded")
	result, ok := body["analyzeResult"].(map[string]any)
	if !ok {
		t.Fatalf("analyzeResult missing or not an object: %v", body["analyzeResult"])
	}
	if result["modelId"] != "prebuilt-layout" {
		t.Errorf("analyzeResult was not unwrapped correctly: %v", result)
	}
}

// TestLargeResultIsStreamedIntact checks that a result far larger than any sensible buffer
// survives the byte-range composition path unchanged.
func TestLargeResultIsStreamedIntact(t *testing.T) {
	const pad = 4 << 20
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{Sync: mockazure.Sync200, PadResultBytes: pad},
	})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	body := h.pollUntil(loc, "succeeded")
	result, ok := body["analyzeResult"].(map[string]any)
	if !ok {
		t.Fatal("analyzeResult missing")
	}
	padding, _ := result["padding"].(string)
	if len(padding) != pad {
		t.Errorf("padding survived as %d bytes, want %d; the range copy lost data", len(padding), pad)
	}
	if strings.Trim(padding, "x") != "" {
		t.Error("padding content was corrupted")
	}
}

// TestArtifactsAreCapturedEagerly checks that output=pdf forces the asynchronous upstream path,
// because result files are addressed by the container's own operation id and a synchronous call
// never mints one.
func TestArtifactsAreCapturedEagerly(t *testing.T) {
	h := newHarness(t, harnessOpts{di: mockazure.Options{Sync: mockazure.Sync200}})

	resp := h.post(diAnalyze+"&output=pdf", "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	h.pollUntil(loc, "succeeded")

	if h.di.Called(":syncAnalyze") {
		t.Error("output=pdf used the synchronous route, which mints no operation id to fetch the " +
			"PDF from")
	}
	if !h.di.Called("/pdf") {
		t.Error("gateway never fetched the searchable PDF")
	}

	// The stored artifact must then be servable from the gateway.
	id := loc[strings.LastIndex(loc, "/")+1:]
	if i := strings.Index(id, "?"); i >= 0 {
		id = id[:i]
	}
	pdf := h.get("/documentintelligence/documentModels/prebuilt-layout/analyzeResults/" + id +
		"/pdf?api-version=" + apiVersion)
	defer func() { _ = pdf.Body.Close() }()
	if pdf.StatusCode != http.StatusOK {
		t.Fatalf("PDF fetch returned %d, want 200", pdf.StatusCode)
	}
	data, _ := io.ReadAll(pdf.Body)
	if !strings.HasPrefix(string(data), "%PDF") {
		t.Errorf("stored PDF is not a PDF: %q", truncate(string(data)))
	}
}

// TestExpiredOperationReturns404 checks Azure's TTL semantics: once a result expires it is gone,
// and the poll answers exactly as it would for an id that never existed.
func TestExpiredOperationReturns404(t *testing.T) {
	h := newHarness(t, harnessOpts{
		// Long enough for the job to finish, short enough to expire inside the test.
		tweak: func(c *config.Config) { c.ResultTTL = 2 * time.Second },
	})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	h.pollUntil(loc, "succeeded")

	time.Sleep(2100 * time.Millisecond)

	expired := h.get(loc)
	defer func() { _ = expired.Body.Close() }()
	if expired.StatusCode != http.StatusNotFound {
		t.Fatalf("expired operation returned %d, want 404", expired.StatusCode)
	}
	if v := expired.Header.Get("Retry-After"); v != "" {
		t.Errorf("expired 404 carries Retry-After %q; azure-core would retry it 10 times", v)
	}
}

// TestDeleteResultPurgesEarly covers Azure's DELETE on a result.
func TestDeleteResultPurgesEarly(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	h.pollUntil(loc, "succeeded")

	req, _ := http.NewRequest(http.MethodDelete, loc, nil)
	del, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete returned %d, want 204", del.StatusCode)
	}

	after := h.get(loc)
	defer func() { _ = after.Body.Close() }()
	if after.StatusCode != http.StatusNotFound {
		t.Errorf("deleted operation returned %d, want 404", after.StatusCode)
	}
}

// TestSyncPassthroughStreamsBothWays checks that a synchronous client call goes straight to the
// container and back, with nothing persisted.
func TestSyncPassthroughStreamsBothWays(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di:   mockazure.Options{Sync: mockazure.Sync200},
		read: mockazure.Options{Sync: mockazure.Sync200},
	})

	for _, tc := range []struct{ name, path string }{
		{"document intelligence", diSync},
		{"computer vision read", readSync},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.post(tc.path, "%PDF-1.7 fake")
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("sync passthrough returned %d, want 200", resp.StatusCode)
			}
			body := decode(t, resp)
			if body["analyzeResult"] == nil && body["modelId"] == nil {
				t.Errorf("passthrough body carries no result: %v", body)
			}
			if resp.Header.Get("Operation-Location") != "" {
				t.Error("synchronous response carries Operation-Location; there is no operation to poll")
			}
		})
	}
}

// TestUpstreamAPIKeyIsInjected checks that the gateway authenticates upstream with its own key
// while requiring nothing from the client.
func TestUpstreamAPIKeyIsInjected(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{Sync: mockazure.Sync200, RequireAPIKey: "di-key"},
	})

	// No Ocp-Apim-Subscription-Key from the client at all.
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit returned %d, want 202 with no client credential", resp.StatusCode)
	}
	body := h.pollUntil(loc, "succeeded")
	if body["status"] != "succeeded" {
		t.Fatalf("status is %v; the gateway did not authenticate upstream", body["status"])
	}
}

// TestCrashRecoveryResumesAcceptedJobs is the durability guarantee: a job that was admitted and
// committed but never ran must be picked up by a later process, because its 202 is already out.
func TestCrashRecoveryResumesAcceptedJobs(t *testing.T) {
	dir := t.TempDir()
	diC := mockazure.NewDI(mockazure.Options{Sync: mockazure.Sync200})
	defer diC.Close()
	readC := mockazure.NewRead(mockazure.Options{})
	defer readC.Close()

	cfg := &config.Config{
		DataDir: dir, AllowNetworkFS: true, DISyncMode: config.SyncAuto, DIBlindPollBudget: 5,
		ResultTTL: time.Hour, GCInterval: time.Hour, DiskHighWatermark: 0.99,
		MaxRequestBytes: 1 << 20, PollRetryAfter: 1, BusyRetryAfter: 5,
		DI:   config.Upstream{BaseURL: diC.URL(), MaxInflight: 2, Timeout: 20 * time.Second},
		Read: config.Upstream{BaseURL: readC.URL(), MaxInflight: 2, Timeout: 20 * time.Second},
	}

	// First process: commit a job exactly as the submit path would, then die before running it.
	st, err := store.Open(store.Options{DataDir: dir, AllowNetworkFS: true})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	id := ids.New()
	if _, err := st.Blob.WriteFrom(id, store.KindInput,
		strings.NewReader("%PDF-1.7 fake"), 0); err != nil {
		t.Fatalf("store input: %v", err)
	}
	now := time.Now().UTC()
	if err := st.Create(context.Background(), &store.Job{
		ID: id, Surface: jobs.SurfaceDI, ModelID: "prebuilt-layout", APIVersion: apiVersion,
		Status: store.StatusNotStarted, CreatedAt: now, UpdatedAt: now,
		ExpiresAt: now.Add(time.Hour), Query: "api-version=" + apiVersion,
		ContentType: "application/pdf", InputPath: st.Blob.Path(id, store.KindInput),
		InputBytes: 13,
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	_ = st.Close()

	// Second process: a fresh store and manager over the same volume.
	st2, err := store.Open(store.Options{DataDir: dir, AllowNetworkFS: true})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := jobs.New(jobs.Options{
		Config: cfg, Store: st2, Logger: log,
		DI:   upstream.NewDI(cfg.DI, cfg.DISyncMode, cfg.DIBlindPollBudget, st2.Blob.Root(), cfg.MaxRequestBytes),
		Read: upstream.NewRead(cfg.Read, cfg.DIBlindPollBudget, st2.Blob.Root(), cfg.MaxRequestBytes),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Stop()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err := st2.Get(context.Background(), id)
		if err == nil && job.Status == store.StatusSucceeded {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	job, _ := st2.Get(context.Background(), id)
	t.Fatalf("orphaned job was never recovered; status is %q", job.Status)
}

func assertJobMode(t *testing.T, h *harness, want string) {
	t.Helper()
	list, err := h.store.Recent(context.Background(), 5)
	if err != nil || len(list) == 0 {
		t.Fatalf("no jobs recorded: %v", err)
	}
	if got := string(list[0].Mode); got != want {
		t.Errorf("job ran in mode %q, want %q", got, want)
	}
}

func countCalls(requests []string, substr string) int {
	var n int
	for _, p := range requests {
		if strings.Contains(p, substr) {
			n++
		}
	}
	return n
}
