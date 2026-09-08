package jobs

import (
	"context"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

// maxAttempts bounds how many times a job is retried after a crash. A job that keeps killing its
// worker is more likely to be a poison input than bad luck, and retrying it forever would starve
// healthy work.
const maxAttempts = 3

// recoverBatch is how many orphaned jobs are reconciled per pass.
const recoverBatch = 256

// recover reclaims jobs that are no longer progressing.
//
// Two states need attention: notStarted jobs that were admitted and committed but whose 202 was
// followed by a crash, and running jobs whose worker is gone. Both were already acknowledged to a
// client, so they are re-driven rather than dropped.
//
// It runs twice over: once at boot ignoring leases, and then periodically from the sweeper with
// the lease filter applied, so a worker that dies while the process lives is also caught.
//
// Recovered jobs take an admission slot like any other work, but they wait for one instead of
// being rejected: a 429 is a refusal to make a promise, and these promises were already made.
func (m *Manager) recover(ctx context.Context, atBoot bool) {
	var jobsToRecover []*store.Job
	var err error
	if atBoot {
		// A freshly booted process holds no leases, so every unfinished row was abandoned.
		// Filtering on lease expiry here would skip precisely the jobs that need rescuing: a
		// crash leaves lease_until minutes in the future.
		jobsToRecover, err = m.store.Abandoned(ctx, m.now(), recoverBatch)
	} else {
		jobsToRecover, err = m.store.Orphaned(ctx, m.now(), recoverBatch)
	}
	if err != nil {
		m.log.Error("could not scan for orphaned jobs", "error", err)
		return
	}
	if len(jobsToRecover) == 0 {
		return
	}
	m.log.Info("reclaiming jobs that stopped progressing",
		"count", len(jobsToRecover), "atBoot", atBoot)

	for _, job := range jobsToRecover {
		if ctx.Err() != nil {
			return
		}
		r, ok := m.runners[job.Surface]
		if !ok {
			m.log.Warn("orphaned job names an unknown surface", "job", job.ID, "surface", job.Surface)
			continue
		}
		m.recoverOne(ctx, r, job)
	}
}

func (m *Manager) recoverOne(ctx context.Context, r *surfaceRunner, job *store.Job) {
	log := m.log.With("job", job.ID, "surface", job.Surface, "attempts", job.Attempts)

	// Without the input document the upstream call cannot be repeated. That happens when the
	// crash landed between storing the result and committing the row, or when a previous attempt
	// already consumed it.
	if !m.store.Blob.Exists(job.ID, store.KindInput) {
		log.Warn("orphaned job has no input document; failing it")
		m.failJob(ctx, r, job.ID, azerr.Internal(r.analyzer.Surface(),
			"The analysis was interrupted and could not be resumed."), 0, 0)
		return
	}
	if job.Attempts >= maxAttempts {
		log.Warn("orphaned job exhausted its attempts; failing it")
		m.failJob(ctx, r, job.ID, azerr.Internal(r.analyzer.Surface(),
			"The analysis was interrupted repeatedly and could not be completed."), 0, 0)
		return
	}

	if err := m.store.Requeue(ctx, job.ID, m.now()); err != nil {
		log.Error("could not requeue orphaned job", "error", err)
		return
	}

	// Wait for capacity rather than refusing: this job's 202 has already been sent.
	select {
	case r.admits <- struct{}{}:
	case <-ctx.Done():
		return
	}
	select {
	case r.queue <- job.ID:
		log.Info("requeued orphaned job")
	case <-ctx.Done():
		<-r.admits
	}
}

// sweep runs the TTL and disk-pressure maintenance loop.
func (m *Manager) sweep(ctx context.Context) {
	t := time.NewTicker(m.cfg.GCInterval)
	defer t.Stop()

	// Run once at startup so a pod that was down past several TTLs does not serve stale results.
	m.sweepOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweepOnce(ctx)
		}
	}
}

// sweepOnce expires jobs past their TTL, reclaims crash debris, and evicts under disk pressure.
func (m *Manager) sweepOnce(ctx context.Context) {
	now := m.now()

	expired, err := m.store.Expired(ctx, now, 512)
	if err != nil {
		m.log.Error("could not scan for expired jobs", "error", err)
	}
	for _, job := range expired {
		if err := m.store.Delete(ctx, job.ID); err != nil {
			m.log.Warn("could not delete expired job", "job", job.ID, "error", err)
			continue
		}
	}
	if len(expired) > 0 {
		m.log.Info("expired jobs removed", "count", len(expired))
	}

	// Temp files older than an hour cannot belong to a live request: the longest configured
	// upstream timeout bounds any single download well below that.
	if n, err := m.store.Blob.SweepTemp(int64(time.Hour/time.Second), now.Unix()); err == nil && n > 0 {
		m.log.Info("removed orphaned temporary files", "count", n)
	}

	// Catch workers that died while this process kept running. The boot pass cannot cover these
	// because their leases were still valid when it ran.
	m.recover(ctx, false)

	m.evictUnderPressure(ctx)
}

// evictUnderPressure drops the oldest completed jobs when the volume approaches full.
//
// Only terminal jobs are eligible. Evicting a running job would break a promise still in flight,
// and evicting early costs a client a result they could still legitimately fetch — so this runs
// only above the high-water mark, and stops as soon as the volume is back under it.
func (m *Manager) evictUnderPressure(ctx context.Context) {
	used, ok := m.store.DiskUsage()
	if !ok || used < m.cfg.DiskHighWatermark {
		return
	}
	m.log.Warn("volume above high-water mark; evicting oldest completed results",
		"used", used, "highWatermark", m.cfg.DiskHighWatermark)

	candidates, err := m.store.EvictionCandidates(ctx, 256)
	if err != nil {
		m.log.Error("could not list eviction candidates", "error", err)
		return
	}
	var evicted int
	for _, job := range candidates {
		if err := m.store.Delete(ctx, job.ID); err != nil {
			m.log.Warn("could not evict job", "job", job.ID, "error", err)
			continue
		}
		evicted++
		if used, ok := m.store.DiskUsage(); ok && used < m.cfg.DiskHighWatermark {
			break
		}
	}
	if evicted > 0 {
		m.log.Warn("evicted completed results to reclaim space", "count", evicted)
	}
}

// DiskPressure reports whether the volume is too full to accept new work, so a submit can be
// refused cleanly instead of failing partway through storing the input.
func (m *Manager) DiskPressure() bool {
	used, ok := m.store.DiskUsage()
	if !ok {
		return false
	}
	// Refuse only well past the eviction threshold: between the two, the sweeper is expected to
	// make room without the client noticing.
	return used >= m.cfg.DiskHighWatermark+((1-m.cfg.DiskHighWatermark)/2)
}
