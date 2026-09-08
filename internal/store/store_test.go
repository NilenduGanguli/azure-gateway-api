package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Options{DataDir: t.TempDir(), AllowNetworkFS: true, ReadPoolSize: 4})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestBlobWriteIsAtomic checks that an aborted write leaves nothing visible. A reader must never
// observe a partial result, because the stored envelope is served byte-for-byte.
func TestBlobWriteIsAtomic(t *testing.T) {
	s := newTestStore(t)

	w, err := s.Blob.Create("abcd1234-0000-4000-8000-000000000000", KindResult, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Abort()

	if _, _, err := s.Blob.Open("abcd1234-0000-4000-8000-000000000000", KindResult); err == nil {
		t.Error("an aborted write left a visible blob")
	}
}

func TestBlobRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id := "abcd1234-0000-4000-8000-000000000000"
	want := strings.Repeat("payload", 5000)

	if err := s.Blob.WriteAll(id, KindResult, []byte(want)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, size, err := s.Blob.Open(id, KindResult)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	if size != int64(len(want)) {
		t.Errorf("size %d, want %d", size, len(want))
	}
	got := make([]byte, size)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != want {
		t.Error("content changed on the round trip")
	}
}

func TestBlobEnforcesLimit(t *testing.T) {
	s := newTestStore(t)
	id := "abcd1234-0000-4000-8000-000000000000"

	_, err := s.Blob.WriteFrom(id, KindInput, strings.NewReader(strings.Repeat("x", 100)), 10)
	if err == nil {
		t.Fatal("expected the size limit to be enforced")
	}
	if _, _, err := s.Blob.Open(id, KindInput); err == nil {
		t.Error("an over-limit write left a visible blob")
	}
}

// TestBlobSanitizesIdentifiers matters because figure ids come from the upstream response, not
// from the gateway, so they must never be able to escape the blob root.
func TestBlobSanitizesIdentifiers(t *testing.T) {
	s := newTestStore(t)
	kind := KindFigure("../../../etc/passwd")

	if strings.Contains(string(kind), "/") || strings.Contains(string(kind), "..") {
		t.Fatalf("figure kind %q still contains path syntax", kind)
	}
	p := s.Blob.Path("../../escape", kind)
	if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(s.Blob.Root())) {
		t.Errorf("path %q escapes the blob root %q", p, s.Blob.Root())
	}
}

func TestSweepTempRemovesOnlyOldFiles(t *testing.T) {
	s := newTestStore(t)
	dir := s.Blob.Root()

	fresh := filepath.Join(dir, "job.res.tmp-fresh")
	old := filepath.Join(dir, "job.res.tmp-old")
	for _, p := range []string{fresh, old} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	n, err := s.Blob.SweepTemp(int64(time.Hour/time.Second), time.Now().Unix())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d files, want 1", n)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("sweep removed a temp file that could still belong to a live request")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("sweep left stale crash debris behind")
	}
}

func TestJobLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	job := &Job{
		ID: "abcd1234-0000-4000-8000-000000000000", Surface: "di", ModelID: "prebuilt-layout",
		APIVersion: "2024-11-30", Status: StatusNotStarted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		Query: "api-version=2024-11-30", ContentType: "application/pdf", InputBytes: 42,
	}
	if err := s.Create(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusNotStarted || got.ModelID != "prebuilt-layout" || got.InputBytes != 42 {
		t.Errorf("round trip lost data: %+v", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("createdAt is %v, want %v", got.CreatedAt, now)
	}

	if err := s.MarkRunning(ctx, job.ID, "worker-0", now.Add(time.Minute), now); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	got, _ = s.Get(ctx, job.ID)
	if got.Status != StatusRunning {
		t.Errorf("status is %q, want running", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts is %d, want 1", got.Attempts)
	}

	ok, err := s.Succeed(ctx, job.ID, "/data/blobs/x.res", 1234, ModeSync, 55, true, "1.1", now)
	if err != nil {
		t.Fatalf("succeed: %v", err)
	}
	if !ok {
		t.Fatal("Succeed reported no matching row for a job that exists")
	}
	got, _ = s.Get(ctx, job.ID)
	if got.Status != StatusSucceeded || got.ResultBytes != 1234 || got.Mode != ModeSync {
		t.Errorf("succeed did not persist: %+v", got)
	}
	if !got.HasPDF || got.Figures != "1.1" {
		t.Errorf("artifact metadata lost: hasPdf=%v figures=%q", got.HasPDF, got.Figures)
	}
	if got.InputPath != "" {
		t.Error("input path should be cleared once the job succeeds")
	}
}

// TestOrphanedFindsBothStates covers what the recovery pass must reclaim: work committed but
// never started, and work whose worker died holding a lease.
func TestOrphanedFindsBothStates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(id string, status Status) {
		if err := s.Create(ctx, &Job{
			ID: id, Surface: "di", Status: status,
			CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("00000000-0000-4000-8000-000000000001", StatusNotStarted)
	mk("00000000-0000-4000-8000-000000000002", StatusRunning)
	mk("00000000-0000-4000-8000-000000000003", StatusSucceeded)

	// Give the running job an expired lease, as a dead worker would leave it.
	if err := s.MarkRunning(ctx, "00000000-0000-4000-8000-000000000002", "dead",
		now.Add(-time.Minute), now); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	got, err := s.Orphaned(ctx, now, now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("orphaned: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d orphans, want 2 (notStarted plus lease-expired running)", len(got))
	}
	for _, j := range got {
		if j.Status == StatusSucceeded {
			t.Error("a terminal job was reported as orphaned")
		}
	}
}

func TestExpiredAndDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := "00000000-0000-4000-8000-00000000000e"

	if err := s.Create(ctx, &Job{
		ID: id, Surface: "di", Status: StatusSucceeded,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Blob.WriteAll(id, KindResult, []byte("{}")); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	expired, err := s.Expired(ctx, now, 10)
	if err != nil || len(expired) != 1 {
		t.Fatalf("expired returned %d jobs, err %v", len(expired), err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, id); err == nil {
		t.Error("deleted job is still readable")
	}
	if _, _, err := s.Blob.Open(id, KindResult); err == nil {
		t.Error("delete left the result blob behind")
	}
}

func TestStatsCountsBySurfaceAndStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for i, spec := range []struct {
		surface string
		status  Status
	}{
		{"di", StatusSucceeded}, {"di", StatusSucceeded}, {"di", StatusFailed},
		{"read", StatusRunning},
	} {
		id := "00000000-0000-4000-8000-00000000000" + string(rune('a'+i))
		if err := s.Create(ctx, &Job{
			ID: id, Surface: spec.surface, Status: spec.status,
			CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	counts, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if counts["di"].Succeeded != 2 || counts["di"].Failed != 1 || counts["di"].Total != 3 {
		t.Errorf("di counts wrong: %+v", counts["di"])
	}
	if counts["read"].Running != 1 {
		t.Errorf("read counts wrong: %+v", counts["read"])
	}
}

// TestSucceedReportsAMissingRow covers the case where a job is deleted while it runs — by a client
// DELETE or by disk-pressure eviction. The worker must learn that its result references nothing,
// or the artifacts it just wrote stay on the volume forever.
func TestSucceedReportsAMissingRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ok, err := s.Succeed(ctx, "00000000-0000-4000-8000-00000000dead", "/x", 1, ModeSync, 1, false, "", now)
	if err != nil {
		t.Fatalf("succeed: %v", err)
	}
	if ok {
		t.Error("Succeed reported a row was updated for an id that does not exist")
	}

	ok, err = s.Fail(ctx, "00000000-0000-4000-8000-00000000dead", "{}", ModeSync, 500, 1, now)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if ok {
		t.Error("Fail reported a row was updated for an id that does not exist")
	}
}

// TestDeleteRemovesTheRowBeforeTheBlobs is a regression test.
//
// Removing blobs first meant a partial failure left the row behind pointing at a result that no
// longer existed, and a poll on it produced a 500 with no top-level status — the shape that
// crashes Java clients. Row-first inverts the failure into an orphaned blob, which the sweeper
// reclaims and which no client can observe.
func TestDeleteRemovesTheRowBeforeTheBlobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := "00000000-0000-4000-8000-00000000d001"

	if err := s.Create(ctx, &Job{
		ID: id, Surface: "di", Status: StatusSucceeded,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Blob.WriteAll(id, KindResult, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if s.Exists(ctx, id) {
		t.Error("the row survived Delete")
	}
	if _, _, err := s.Blob.Open(id, KindResult); err == nil {
		t.Error("the blob survived Delete")
	}
}

// TestExpiredLeavesRunningJobsAlone checks that the TTL sweeper cannot delete a job out from under
// its worker, which would leave the worker writing a result no row references.
func TestExpiredLeavesRunningJobsAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Expired, but a worker is actively heartbeating it.
	live := "00000000-0000-4000-8000-00000000r001"
	if err := s.Create(ctx, &Job{
		ID: live, Surface: "di", Status: StatusNotStarted,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now, ExpiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.MarkRunning(ctx, live, "worker-0", now.Add(time.Minute), now); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Expired and terminal: safe to remove.
	done := "00000000-0000-4000-8000-00000000r002"
	if err := s.Create(ctx, &Job{
		ID: done, Surface: "di", Status: StatusSucceeded,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now, ExpiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.Expired(ctx, now, 10)
	if err != nil {
		t.Fatalf("expired: %v", err)
	}
	if len(got) != 1 || got[0].ID != done {
		ids := make([]string, len(got))
		for i, j := range got {
			ids[i] = j.ID
		}
		t.Fatalf("Expired returned %v; a running job with a live lease must be left alone", ids)
	}
}

// TestEvictionPrefersExpiredResults checks the ordering that keeps disk-pressure eviction from
// taking a result a client could still legitimately fetch while an already-404 one sits next to it.
func TestEvictionPrefersExpiredResults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// The unexpired one is older, so a plain updated_at ordering would offer it first.
	fresh := "00000000-0000-4000-8000-00000000v001"
	stale := "00000000-0000-4000-8000-00000000v002"
	if err := s.Create(ctx, &Job{
		ID: fresh, Surface: "di", Status: StatusSucceeded,
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour),
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(ctx, &Job{
		ID: stale, Surface: "di", Status: StatusSucceeded,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.EvictionCandidates(ctx, now, 10)
	if err != nil {
		t.Fatalf("eviction candidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if got[0].ID != stale {
		t.Errorf("first candidate is %s; the already-expired result must be offered first",
			got[0].ID)
	}
}

// TestOrphanedIgnoresFreshNotStartedRows checks the guard that keeps the periodic reclaim pass
// from racing a live submit: a row committed moments ago is still on its way to a worker, and
// reclaiming it would run the same analysis twice against the container.
func TestOrphanedIgnoresFreshNotStartedRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	fresh := "00000000-0000-4000-8000-00000000n001"
	old := "00000000-0000-4000-8000-00000000n002"
	if err := s.Create(ctx, &Job{
		ID: fresh, Surface: "di", Status: StatusNotStarted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(ctx, &Job{
		ID: old, Surface: "di", Status: StatusNotStarted,
		CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-10 * time.Minute),
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.Orphaned(ctx, now, now.Add(-4*time.Minute), 10)
	if err != nil {
		t.Fatalf("orphaned: %v", err)
	}
	if len(got) != 1 || got[0].ID != old {
		ids := make([]string, len(got))
		for i, j := range got {
			ids[i] = j.ID
		}
		t.Fatalf("Orphaned returned %v; a just-committed row must not be reclaimed", ids)
	}
}
