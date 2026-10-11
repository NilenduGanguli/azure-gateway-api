package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

// submitJob does what the surface does for :analyze: admit, store the input, commit the row,
// enqueue.
func submitJob(t *testing.T, m *Manager, st *store.Store, id string, now time.Time) {
	t.Helper()
	release, err := m.Admit(SurfaceDI)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := st.Blob.WriteAll(id, store.KindInput, []byte("%PDF-1.7 fake")); err != nil {
		release()
		t.Fatalf("write input: %v", err)
	}
	if err := st.Create(context.Background(), &store.Job{
		ID: id, Surface: SurfaceDI, ModelID: "prebuilt-layout", Status: store.StatusNotStarted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		Query: "api-version=2024-11-30", ContentType: "application/pdf", InputBytes: 13,
	}); err != nil {
		release()
		t.Fatalf("create: %v", err)
	}
	if err := m.Enqueue(SurfaceDI, id); err != nil {
		release()
		t.Fatalf("enqueue: %v", err)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestJobRunsThroughTheEngine checks the whole worker path: the engine's output becomes the stored
// envelope, the row records the run's upstream counts, and the input is gone once it can no
// longer be needed.
func TestJobRunsThroughTheEngine(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	container := mockazure.NewDI(mockazure.Options{})
	t.Cleanup(container.Close)
	m, st := newTestManager(t, container.URL(), now)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop() })

	const id = "00000000-0000-4000-8000-0000000000b1"
	submitJob(t, m, st, id, now)

	var job *store.Job
	waitFor(t, "the job to succeed", func() bool {
		job, _ = st.Get(context.Background(), id)
		return job != nil && job.Status == store.StatusSucceeded
	})
	if job.UpstreamAttempts != 1 || job.UpstreamParts != 1 {
		t.Errorf("upstream counts are attempts=%d parts=%d, want 1 and 1",
			job.UpstreamAttempts, job.UpstreamParts)
	}
	if st.Blob.Exists(id, store.KindInput) {
		t.Error("the input is still stored after the job succeeded")
	}
	f, _, err := st.Blob.Open(id, store.KindResult)
	if err != nil {
		t.Fatalf("open result: %v", err)
	}
	defer func() { _ = f.Close() }()
	var env struct {
		Status        string          `json:"status"`
		Created       string          `json:"createdDateTime"`
		AnalyzeResult json.RawMessage `json:"analyzeResult"`
	}
	if err := json.NewDecoder(f).Decode(&env); err != nil {
		t.Fatalf("the stored envelope is not JSON: %v", err)
	}
	if env.Status != "succeeded" || env.Created != now.Format(TimeFormat) ||
		!strings.HasPrefix(string(env.AnalyzeResult), "{") {
		t.Errorf("stored envelope %+v is not a succeeded operation with a result", env)
	}
	if container.SyncCalls() != 1 {
		t.Errorf("the container saw %d calls, want 1", container.SyncCalls())
	}
}

// TestFailedJobRecordsItsUpstreamCounts checks that a failed job's row says what its run cost, as
// a succeeded one's does: an operator reading a failure needs to know whether the gateway tried
// once or five times.
func TestFailedJobRecordsItsUpstreamCounts(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	container := mockazure.NewDI(mockazure.Options{StatusFirst: 99, StatusFirstCode: 400})
	t.Cleanup(container.Close)
	m, st := newTestManager(t, container.URL(), now)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop() })

	const id = "00000000-0000-4000-8000-0000000000b2"
	submitJob(t, m, st, id, now)

	var job *store.Job
	waitFor(t, "the job to fail", func() bool {
		job, _ = st.Get(context.Background(), id)
		return job != nil && job.Status == store.StatusFailed
	})
	if job.UpstreamAttempts != 1 || job.UpstreamParts != 1 {
		t.Errorf("upstream counts are attempts=%d parts=%d, want 1 and 1",
			job.UpstreamAttempts, job.UpstreamParts)
	}
}

// TestRecoveryFailsAJobThatExhaustedItsAttempts covers a poison input: a job whose runs keep
// dying with the process is failed at recovery, with a well-formed error, and never sent to the
// container again.
func TestRecoveryFailsAJobThatExhaustedItsAttempts(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	container := newHangingContainer(t)
	m, st := newTestManager(t, container.URL(), now)

	const id = "00000000-0000-4000-8000-0000000000b2"
	if err := st.Create(context.Background(), &store.Job{
		ID: id, Surface: SurfaceDI, ModelID: "prebuilt-layout", Status: store.StatusNotStarted,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(time.Hour), Query: "api-version=2024-11-30",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Blob.WriteAll(id, store.KindInput, []byte("%PDF-1.7 fake")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	for range maxAttempts {
		if err := st.MarkRunning(context.Background(), id, "dead", now, now); err != nil {
			t.Fatalf("mark running: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	m.Recover(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()
	m.Stop()

	job, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != store.StatusFailed || !strings.Contains(job.ErrorJSON, "repeatedly") {
		t.Errorf("job is %s with error %s, want failed as interrupted repeatedly",
			job.Status, job.ErrorJSON)
	}
	if n := container.SyncCalls(); n != 0 {
		t.Errorf("the container saw %d calls for a job that exhausted its attempts", n)
	}
}

// TestDeletedRowDiscardsTheResult covers a client DELETE, or an eviction, while the job runs: the
// result the engine then produces references nothing and must not stay on the volume.
func TestDeletedRowDiscardsTheResult(t *testing.T) {
	now := time.Now().UTC()
	container := mockazure.NewDI(mockazure.Options{Latency: 300 * time.Millisecond})
	t.Cleanup(container.Close)
	m, st := newTestManager(t, container.URL(), now)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Stop() })

	const id = "00000000-0000-4000-8000-0000000000b3"
	submitJob(t, m, st, id, now)
	waitFor(t, "the container to receive the call", func() bool { return container.SyncCalls() > 0 })
	if err := st.Delete(context.Background(), id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "the worker to finish", func() bool { return m.Capacity()[SurfaceDI]["inFlight"] == 0 })

	if st.Blob.Exists(id, store.KindResult) {
		t.Error("the result of a deleted job was left on the volume")
	}
	if ok, err := st.Exists(context.Background(), id); err != nil || ok {
		t.Errorf("the deleted row is back: exists=%v err=%v", ok, err)
	}
}

// TestWorkFoldersAreSweptAfterACrash checks the work-folder sweep. A process that dies
// mid-analysis leaves its part files behind; the job re-runs from its stored input, so the folder
// is debris once it is old and nothing here claims it. A folder still claimed — its job queued or
// running, or its :syncAnalyze request still open — and a young one are left alone.
func TestWorkFoldersAreSweptAfterACrash(t *testing.T) {
	now := time.Now().UTC()
	m, st := newTestManager(t, newHangingContainer(t).URL(), now)
	root := WorkRoot(st.DataDir())
	old := now.Add(-2 * time.Hour)

	folders := []struct {
		name    string
		mtime   time.Time
		claimed bool
		kept    bool
	}{
		{"00000000-0000-4000-8000-0000000000c1", old, false, false},
		{"sync-00000000-0000-4000-8000-0000000000c2", old, false, false},
		{"00000000-0000-4000-8000-0000000000c3", old, true, true},
		{"sync-00000000-0000-4000-8000-0000000000c4", old, true, true},
		{"00000000-0000-4000-8000-0000000000c5", now, false, true},
	}
	for _, f := range folders {
		dir := filepath.Join(root, f.name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "part-1-5.pdf"), []byte("%PDF"), 0o640); err != nil {
			t.Fatalf("write part: %v", err)
		}
		if err := os.Chtimes(dir, f.mtime, f.mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if f.claimed {
			release := m.Track(f.name)
			t.Cleanup(release)
		}
	}

	m.sweepOnce(context.Background(), false)

	for _, f := range folders {
		_, err := os.Stat(filepath.Join(root, f.name))
		if kept := err == nil; kept != f.kept {
			t.Errorf("folder %s kept=%v, want %v", f.name, kept, f.kept)
		}
	}
}
