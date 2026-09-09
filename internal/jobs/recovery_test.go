package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// countingAnalyzer records how many upstream calls a test provoked.
type countingAnalyzer struct {
	calls atomic.Int64
}

func (c *countingAnalyzer) Analyze(ctx context.Context, _ upstream.Document,
	_ upstream.Request) (*upstream.Result, error) {
	c.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *countingAnalyzer) Surface() azerr.Surface                 { return azerr.SurfaceDI }
func (c *countingAnalyzer) Health(context.Context) upstream.Health { return upstream.Health{} }

func newTestManager(t *testing.T, a upstream.Analyzer, now time.Time) (*Manager, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(store.Options{DataDir: dir, AllowNetworkFS: true, ReadPoolSize: 4})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		QueueDepth: 0, GCInterval: time.Hour, ResultTTL: 24 * time.Hour,
		DiskHighWatermark: 0.99, ArtifactFetchTimeout: time.Minute,
		DISyncProbeTimeout: 30 * time.Second,
		DI:                 config.Upstream{MaxInflight: 4, Timeout: time.Minute},
		Read:               config.Upstream{MaxInflight: 4, Timeout: time.Minute},
	}
	m := New(Options{
		Config: cfg, Store: st, DI: a, Read: a,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return now },
	})
	return m, st
}

// TestRecoveryDoesNotReRunExpiredJobs is a regression test.
//
// After an outage longer than the TTL every stored job has expired at once. Boot recovery used to
// resubmit all of them, spending container capacity — and Azure billing — producing results whose
// operation ids already 404 by definition, so no client could ever fetch them.
func TestRecoveryDoesNotReRunExpiredJobs(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	analyzer := &countingAnalyzer{}
	m, st := newTestManager(t, analyzer, now)

	// Jobs from a pod that has been down for 25 hours.
	expired := []string{
		"00000000-0000-4000-8000-0000000000e1",
		"00000000-0000-4000-8000-0000000000e2",
	}
	live := "00000000-0000-4000-8000-0000000000a1"

	for _, id := range append(append([]string{}, expired...), live) {
		exp := now.Add(-time.Hour) // already past its TTL
		if id == live {
			exp = now.Add(time.Hour)
		}
		if err := st.Create(context.Background(), &store.Job{
			ID: id, Surface: SurfaceDI, ModelID: "prebuilt-layout", Status: store.StatusNotStarted,
			CreatedAt: now.Add(-25 * time.Hour), UpdatedAt: now.Add(-25 * time.Hour),
			ExpiresAt: exp, Query: "api-version=2024-11-30",
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := st.Blob.WriteAll(id, store.KindInput, []byte("%PDF-1.7 fake")); err != nil {
			t.Fatalf("write input %s: %v", id, err)
		}
	}

	abandoned, err := st.Abandoned(context.Background(), now, 100)
	if err != nil {
		t.Fatalf("Abandoned: %v", err)
	}
	if len(abandoned) != 1 || abandoned[0].ID != live {
		ids := make([]string, len(abandoned))
		for i, j := range abandoned {
			ids[i] = j.ID
		}
		t.Fatalf("Abandoned returned %v; only the unexpired job should be reclaimable", ids)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	m.Recover(ctx)
	time.Sleep(400 * time.Millisecond)
	got := analyzer.calls.Load()
	cancel()
	m.Stop()

	if got > 1 {
		t.Errorf("boot recovery made %d upstream calls; only the one unexpired job should have "+
			"been re-run", got)
	}
}

// TestArtifactFetchIsBoundedAndYieldsToShutdown is a regression test.
//
// Result files were fetched under an uncancellable context with only a per-request timeout, so a
// wedged container turned N figures into N times that timeout. With production settings that is
// hours — far past any shutdown grace, so the pod would be SIGKILLed mid-write. The phase now has
// one aggregate budget and stops between artifacts once the worker context is cancelled.
func TestArtifactFetchIsBoundedAndYieldsToShutdown(t *testing.T) {
	var requests atomic.Int64
	wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-r.Context().Done() // never answers
	}))
	defer wedged.Close()

	now := time.Now().UTC()
	dir := t.TempDir()
	st, err := store.Open(store.Options{DataDir: dir, AllowNetworkFS: true})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	cfg := &config.Config{
		QueueDepth: 0, GCInterval: time.Hour, ResultTTL: time.Hour, DiskHighWatermark: 0.99,
		ArtifactFetchTimeout: 600 * time.Millisecond,
		DI:                   config.Upstream{BaseURL: wedged.URL, MaxInflight: 1, Timeout: 5 * time.Second},
	}
	di := upstream.NewDI(cfg.DI, config.SyncAuto, 5, st.Blob.Root(), 1<<20, cfg.DISyncProbeTimeout)
	m := New(Options{
		Config: cfg, Store: st, DI: di, Read: di,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return now },
	})

	// A completed analysis carrying several figures, with output=figures requested.
	job := &store.Job{
		ID: "00000000-0000-4000-8000-0000000000f1", Surface: SurfaceDI,
		ModelID: "prebuilt-layout", Status: store.StatusRunning,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		Query: "api-version=2024-11-30&output=figures",
	}
	resultBody := `{"apiVersion":"2024-11-30","figures":[{"id":"1.1"},{"id":"1.2"},{"id":"1.3"},{"id":"1.4"}]}`
	resPath := filepath.Join(dir, "upstream.json")
	if err := os.WriteFile(resPath, []byte(resultBody), 0o600); err != nil {
		t.Fatalf("seed result: %v", err)
	}
	res := &upstream.Result{
		Path: resPath, Start: 0, End: int64(len(resultBody)), UpstreamOpID: "op-1",
	}

	// The aggregate budget must bound the phase even when nothing is cancelled.
	started := time.Now()
	_, figures := m.fetchArtifacts(context.Background(), context.Background(),
		m.runners[SurfaceDI], job, res)
	elapsed := time.Since(started)

	if len(figures) != 0 {
		t.Errorf("a wedged container yielded %v figures", figures)
	}
	// Four figures at the 5s per-request timeout would be 20s without an aggregate budget.
	if elapsed > 4*time.Second {
		t.Errorf("artifact phase took %v; the aggregate budget of %v is not being applied",
			elapsed, cfg.ArtifactFetchTimeout)
	}

	// And a cancelled worker context must stop it almost immediately.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	before := requests.Load()
	started = time.Now()
	_, _ = m.fetchArtifacts(cancelled, context.Background(), m.runners[SurfaceDI], job, res)
	if took := time.Since(started); took > time.Second {
		t.Errorf("artifact phase took %v after the worker context was cancelled; shutdown would "+
			"be held up by work no client is waiting on", took)
	}
	if issued := requests.Load() - before; issued > 0 {
		t.Errorf("%d artifact requests were issued after cancellation", issued)
	}
}

// TestFailedJobStoresAnAzureShapedError checks that what a client eventually reads back is a
// well-formed error object, not a Go error string.
func TestFailedJobStoresAnAzureShapedError(t *testing.T) {
	now := time.Now().UTC()
	m, st := newTestManager(t, &countingAnalyzer{}, now)

	id := "00000000-0000-4000-8000-0000000000c1"
	if err := st.Create(context.Background(), &store.Job{
		ID: id, Surface: SurfaceDI, Status: store.StatusRunning,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	m.failJob(context.Background(), m.runners[SurfaceDI], id,
		azerr.BadRequest(azerr.SurfaceDI, "the document was unreadable"), 400, 12)

	job, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != store.StatusFailed {
		t.Fatalf("status is %q, want failed", job.Status)
	}
	var e azerr.Error
	if err := json.Unmarshal([]byte(job.ErrorJSON), &e); err != nil {
		t.Fatalf("stored error is not JSON: %q", job.ErrorJSON)
	}
	if e.Code == "" || e.Message == "" {
		t.Errorf("stored error lacks code or message: %+v", e)
	}
}

// TestRecoveryNeverRunsAJobTwice is a regression test for a hole the first recovery fix opened.
//
// Recover requeues an abandoned row to notStarted, and the periodic reclaim pass then saw that
// same still-notStarted row before any worker had marked it running — so the analysis went to the
// container twice. Requeue now takes a lease, which the periodic pass respects.
func TestRecoveryNeverRunsAJobTwice(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	analyzer := &countingAnalyzer{}
	m, st := newTestManager(t, analyzer, now)

	id := "00000000-0000-4000-8000-0000000000dd"
	if err := st.Create(context.Background(), &store.Job{
		ID: id, Surface: SurfaceDI, ModelID: "prebuilt-layout", Status: store.StatusNotStarted,
		CreatedAt: now.Add(-25 * time.Hour), UpdatedAt: now.Add(-25 * time.Hour),
		ExpiresAt: now.Add(time.Hour), Query: "api-version=2024-11-30",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Blob.WriteAll(id, store.KindInput, []byte("%PDF-1.7 fake")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	m.Recover(ctx)

	// Drive the periodic pass repeatedly while the job is still in flight — the exact window in
	// which the row is notStarted-or-running with no completed worker.
	for i := 0; i < 5; i++ {
		m.sweepOnce(context.Background(), true)
		time.Sleep(50 * time.Millisecond)
	}
	got := analyzer.calls.Load()
	cancel()
	m.Stop()

	if got != 1 {
		t.Errorf("the container saw %d analyses for one job; recovery must not resubmit work that "+
			"is already claimed", got)
	}
}

// TestSweepAndBootRecoveryCannotBothEnqueue reproduces the ordering that failed inside the image
// build but not on the developer's machine: the sweeper's startup pass reached an abandoned row
// before the explicit boot recovery did, and the container ran the analysis twice.
//
// Ordering the callers is not a sufficient fix — a submit and a reclaim can collide the same way —
// so the manager keeps a claim set and refuses to enqueue an id it already has.
func TestSweepAndBootRecoveryCannotBothEnqueue(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	analyzer := &countingAnalyzer{}
	m, st := newTestManager(t, analyzer, now)

	id := "00000000-0000-4000-8000-0000000000ee"
	if err := st.Create(context.Background(), &store.Job{
		ID: id, Surface: SurfaceDI, ModelID: "prebuilt-layout", Status: store.StatusNotStarted,
		CreatedAt: now.Add(-25 * time.Hour), UpdatedAt: now.Add(-25 * time.Hour),
		ExpiresAt: now.Add(time.Hour), Query: "api-version=2024-11-30",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Blob.WriteAll(id, store.KindInput, []byte("%PDF-1.7 fake")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)

	// Deliberately the losing order: a reclaiming sweep lands first, then boot recovery.
	m.sweepOnce(context.Background(), true)
	m.Recover(ctx)
	m.sweepOnce(context.Background(), true)

	time.Sleep(300 * time.Millisecond)
	got := analyzer.calls.Load()
	cancel()
	m.Stop()

	if got != 1 {
		t.Errorf("the container saw %d analyses for one job, want exactly 1", got)
	}
}

// TestRecoverDoesNotBlockOnCapacity is a regression test for a hole the recovery-race fix opened.
//
// Recover is called before the listener opens, so that a submit cannot race the scan for the same
// row. It then waited for an admission slot per job. After a real outage there are routinely more
// abandoned jobs than slots and each can take minutes, so the listener stayed shut and SIGTERM
// went unhandled for the duration — which Kubernetes reads as a pod that failed to start.
//
// Claiming is now synchronous and fast; waiting for capacity happens in the background.
func TestRecoverDoesNotBlockOnCapacity(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	// The analyzer blocks forever, so every slot stays occupied.
	analyzer := &countingAnalyzer{}
	m, st := newTestManager(t, analyzer, now)

	// Far more abandoned jobs than the four slots newTestManager configures.
	const orphans = 40
	for i := 0; i < orphans; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		if err := st.Create(context.Background(), &store.Job{
			ID: id, Surface: SurfaceDI, ModelID: "prebuilt-layout", Status: store.StatusNotStarted,
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
			ExpiresAt: now.Add(time.Hour), Query: "api-version=2024-11-30",
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := st.Blob.WriteAll(id, store.KindInput, []byte("%PDF-1.7 fake")); err != nil {
			t.Fatalf("write input: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)

	done := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		m.Recover(ctx)
		done <- time.Since(started)
	}()

	select {
	case took := <-done:
		if took > 5*time.Second {
			t.Errorf("Recover took %v with %d orphans and 4 slots; it must not wait for capacity",
				took, orphans)
		}
	case <-time.After(10 * time.Second):
		cancel()
		m.Stop()
		t.Fatal("Recover never returned: it is still blocking on admission slots, so the listener " +
			"would never open and SIGTERM would go unhandled")
	}

	cancel()
	m.Stop()
}

// TestStopReleasesTheRecoveryFeederWithoutTheParentContext is a regression test for a shutdown
// deadlock.
//
// The boot-recovery feeder is tracked by Manager.wg but was handed Recover's caller context. Stop
// cancels only the manager's own derived context and then waits on wg, so the sequence main uses —
// Drain, Stop, then cancel the run context — left the feeder blocked forever on an admission slot
// that no surviving worker would ever drain. wg.Wait() never returned and the pod had to be
// SIGKILLed after its termination grace expired.
//
// Every other test here masks it by cancelling the parent *before* Stop, which is the opposite of
// main's order. This one reproduces main's order exactly, and needs a backlog larger than
// MaxInflight + QueueDepth so the feeder is guaranteed to be mid-send when Stop arrives.
func TestStopReleasesTheRecoveryFeederWithoutTheParentContext(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	m, st := newTestManager(t, &countingAnalyzer{}, now)

	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		if err := st.Create(context.Background(), &store.Job{
			ID: id, Surface: SurfaceDI, ModelID: "prebuilt-layout",
			Status: store.StatusNotStarted, CreatedAt: now.Add(-2 * time.Hour),
			UpdatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(time.Hour),
			Query: "api-version=2024-11-30",
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.Blob.WriteAll(id, store.KindInput, []byte("%PDF-1.7 fake")); err != nil {
			t.Fatal(err)
		}
	}

	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()

	// main.go's order, and only this order, exposes the bug.
	m.Start(runCtx)
	m.Recover(runCtx)
	time.Sleep(300 * time.Millisecond)

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(8 * time.Second):
		stopRun() // unblock the feeder so the test can exit rather than hang the package
		<-stopped
		t.Fatal("Stop() blocked: a wg-tracked goroutine is waiting on a context Stop does not " +
			"cancel, so shutdown hangs until the pod is SIGKILLed")
	}
}
