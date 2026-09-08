package conformance

import (
	"context"
	"io"
	"log/slog"
	"net"
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

// TestSignalDoesNotAbortInFlightRequests is a regression test for a graceful shutdown that was
// not graceful.
//
// Request contexts were derived from the signal context, so SIGTERM cancelled every in-flight
// handler instantly: Shutdown returned in about a millisecond and a synchronous passthrough that
// was seconds from completing died with a 500. The grace period existed and nothing ever used it.
//
// The fix is two separate lifetimes — the signal only triggers the drain; requests and workers run
// under a context that outlives it. This test reproduces that wiring.
func TestSignalDoesNotAbortInFlightRequests(t *testing.T) {
	h := newHarness(t, harnessOpts{
		read: mockazure.Options{Sync: mockazure.Sync200, Latency: 1500 * time.Millisecond},
	})

	signalCtx, fireSignal := context.WithCancel(context.Background())
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:     h.handler,
		BaseContext: func(net.Listener) context.Context { return runCtx },
	}
	go func() { _ = srv.Serve(ln) }()
	base := "http://" + ln.Addr().String()

	type outcome struct {
		status  int
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		started := time.Now()
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(
			base+readSync, "application/pdf", strings.NewReader("%PDF-1.7 fake"))
		if err != nil {
			done <- outcome{err: err, elapsed: time.Since(started)}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		done <- outcome{status: resp.StatusCode, elapsed: time.Since(started)}
	}()

	// Let the request reach the container, then signal.
	time.Sleep(300 * time.Millisecond)
	fireSignal()
	<-signalCtx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shutdownStarted := time.Now()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	shutdownTook := time.Since(shutdownStarted)

	got := <-done
	if got.err != nil {
		t.Fatalf("in-flight request failed during shutdown: %v", got.err)
	}
	if got.status != http.StatusOK {
		t.Errorf("in-flight synchronous request returned %d during a graceful shutdown, want 200; "+
			"the signal aborted work the grace period should have protected", got.status)
	}
	if shutdownTook < 500*time.Millisecond {
		t.Errorf("Shutdown returned after %v, which is too fast to have waited for the in-flight "+
			"request — request contexts are still tied to the signal", shutdownTook)
	}
}

// TestBootRecoveryReclaimsJobsWithUnexpiredLeases is a regression test.
//
// The boot pass filtered running jobs on lease expiry, which is exactly backwards for a crash: the
// process dies holding a lease minutes in the future, so the scan skipped precisely the jobs it
// existed to rescue. Recovery ran only once at startup, so those jobs stayed "running" forever and
// their clients polled a status that never advanced.
//
// A freshly booted process holds no leases, so at boot every unfinished row is abandoned by
// definition — the gateway is single-pod.
func TestBootRecoveryReclaimsJobsWithUnexpiredLeases(t *testing.T) {
	dir := t.TempDir()
	diC := mockazure.NewDI(mockazure.Options{Sync: mockazure.Sync200})
	defer diC.Close()
	readC := mockazure.NewRead(mockazure.Options{})
	defer readC.Close()

	cfg := &config.Config{
		DataDir: dir, AllowNetworkFS: true, DISyncMode: config.SyncAuto, DIBlindPollBudget: 5, DISyncProbeTimeout: 30 * time.Second,
		ResultTTL: time.Hour, GCInterval: time.Hour, DiskHighWatermark: 0.99,
		MaxRequestBytes: 1 << 20, PollRetryAfter: 1, BusyRetryAfter: 5,
		DI:   config.Upstream{BaseURL: diC.URL(), MaxInflight: 2, Timeout: 20 * time.Second},
		Read: config.Upstream{BaseURL: readC.URL(), MaxInflight: 2, Timeout: 20 * time.Second},
	}

	// A process that died mid-job: status running, lease still valid for another two minutes.
	st, err := store.Open(store.Options{DataDir: dir, AllowNetworkFS: true})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	id := ids.New()
	if _, err := st.Blob.WriteFrom(id, store.KindInput, strings.NewReader("%PDF-1.7 fake"), 0); err != nil {
		t.Fatalf("store input: %v", err)
	}
	now := time.Now().UTC()
	if err := st.Create(context.Background(), &store.Job{
		ID: id, Surface: jobs.SurfaceDI, ModelID: "prebuilt-layout", APIVersion: apiVersion,
		Status: store.StatusNotStarted, CreatedAt: now, UpdatedAt: now,
		ExpiresAt: now.Add(time.Hour), Query: "api-version=" + apiVersion,
		ContentType: "application/pdf", InputBytes: 13,
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.MarkRunning(context.Background(), id, "worker-that-died",
		now.Add(2*time.Minute), now); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	_ = st.Close()

	// A fresh process over the same volume.
	st2, err := store.Open(store.Options{DataDir: dir, AllowNetworkFS: true})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := jobs.New(jobs.Options{
		Config: cfg, Store: st2, Logger: log,
		DI:   upstream.NewDI(cfg.DI, cfg.DISyncMode, cfg.DIBlindPollBudget, st2.Blob.Root(), cfg.MaxRequestBytes, cfg.DISyncProbeTimeout),
		Read: upstream.NewRead(cfg.Read, cfg.DIBlindPollBudget, st2.Blob.Root(), cfg.MaxRequestBytes),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Stop()
	manager.Recover(ctx)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err := st2.Get(context.Background(), id)
		if err == nil && job.Status == store.StatusSucceeded {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	job, _ := st2.Get(context.Background(), id)
	t.Fatalf("job left by a crashed process was never reclaimed; status is %q with lease_until %v "+
		"— boot recovery must ignore leases", job.Status, job.LeaseUntil)
}
