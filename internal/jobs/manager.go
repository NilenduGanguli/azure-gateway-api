// Package jobs owns the asynchronous lifecycle the gateway presents to clients.
//
// The shape is deliberately simple, because the durability promise is not: once a client holds a
// 202 and an operation id, that id must resolve for its whole TTL across any crash short of
// losing the volume. So a slot is taken at admission and held to completion, the job row is
// committed before the 202 is written, and anything left in flight by a crash is reconciled at
// startup rather than silently dropped.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/engine"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// TimeFormat is the timestamp format both surfaces use in operation envelopes.
const TimeFormat = "2006-01-02T15:04:05Z"

// leaseDuration is how long a worker's claim on a job stays valid without a heartbeat. A job
// whose lease has lapsed is assumed to belong to a dead worker and is reclaimed.
const leaseDuration = 2 * time.Minute

// heartbeatInterval keeps a long analysis's lease alive while it is genuinely progressing.
const heartbeatInterval = 30 * time.Second

// ErrBusy reports that every upstream slot is occupied.
var ErrBusy = errors.New("jobs: no capacity")

// surfaceRunner is one surface's worker pool and admission gate.
type surfaceRunner struct {
	name    string
	surface azerr.Surface
	// engine runs each job's analysis. Its scheduler, not the runner, bounds the calls the
	// container sees: a split job makes several at once, and :syncAnalyze requests share the
	// same slots, so only one scheduler per surface can hold the container to MaxInflight.
	engine *engine.Engine
	client *upstream.Client
	// admits bounds work in the system: one token per accepted-but-unfinished job. Its capacity
	// is MaxInflight + QueueDepth, so with the default QueueDepth of zero a submit is rejected
	// the moment every worker is busy.
	admits chan struct{}
	queue  chan string
	// workers is how many jobs actually run at once.
	workers int
	// metadata bounds the read-only proxy lane. Those calls are cheap and must not be able to
	// starve analysis, nor pile up without limit against a wedged container.
	metadata chan struct{}
}

// Manager runs jobs for the configured surfaces.
type Manager struct {
	cfg   *config.Config
	store *store.Store
	log   *slog.Logger

	runners map[string]*surfaceRunner

	// runCtx and cancel are the manager's own lifetime, derived from Start's argument.
	//
	// INVARIANT: every goroutine tracked by wg must observe runCtx, never the caller's context.
	// Stop cancels only this derived context and then waits on wg, so a tracked goroutine that
	// blocks on the *parent* can never be released by Stop — wg.Wait() hangs and the process has
	// to be SIGKILLed. That is exactly how the boot-recovery feeder deadlocked shutdown: it held
	// the caller's ctx while Stop cancelled the child, the workers exited, and nothing was left to
	// drain the admission channel it was blocked sending to.
	wg     sync.WaitGroup
	runCtx context.Context
	cancel context.CancelFunc

	// claims is the set of job ids this process already has queued or running.
	//
	// Nothing may enqueue an id twice. Two paths can reach the same row — a submit and the reclaim
	// pass, or the boot reclaim and the periodic one — and each duplicate is a second billed
	// analysis against the container for a result only one of them will store. Ordering the callers
	// carefully is not enough; this makes it impossible.
	claimMu sync.Mutex
	claims  map[string]struct{}

	// blobCursor is where the next orphan-blob sweep resumes, so successive sweeps rotate through
	// the whole blob tree instead of re-examining the same lexicographically smallest batch.
	//
	// Guarded: sweepOnce runs on the background sweeper and is also called directly by tests, so
	// two goroutines genuinely reach it.
	cursorMu   sync.Mutex
	blobCursor string

	// now is indirected so tests can control time.
	now func() time.Time
}

// Options configures a Manager.
//
// A surface whose engine is nil is not configured: it gets no runner, so it admits nothing and
// its routes are not registered.
type Options struct {
	Config *config.Config
	Store  *store.Store
	Logger *slog.Logger
	DI     *engine.Engine
	Read   *engine.Engine
	// DIClient and ReadClient serve health checks and the GET passthroughs. Each is nil exactly
	// when its engine is.
	DIClient   *upstream.Client
	ReadClient *upstream.Client
	Now        func() time.Time
}

// New builds a Manager. Start must be called to begin processing.
func New(opts Options) *Manager {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	m := &Manager{
		cfg:     opts.Config,
		store:   opts.Store,
		log:     opts.Logger,
		now:     nowFn,
		runners: map[string]*surfaceRunner{},
		claims:  map[string]struct{}{},
	}
	if opts.DI != nil {
		m.runners[SurfaceDI] = newRunner(SurfaceDI, azerr.SurfaceDI, opts.DI, opts.DIClient,
			opts.Config.DI.MaxInflight, opts.Config.QueueDepth)
	}
	if opts.Read != nil {
		m.runners[SurfaceRead] = newRunner(SurfaceRead, azerr.SurfaceRead, opts.Read,
			opts.ReadClient, opts.Config.Read.MaxInflight, opts.Config.QueueDepth)
	}
	return m
}

// Surface names, matching the store's surface column.
const (
	SurfaceDI   = "di"
	SurfaceRead = "read"
)

func newRunner(name string, s azerr.Surface, e *engine.Engine, c *upstream.Client,
	inflight, queueDepth int) *surfaceRunner {
	if inflight < 1 {
		inflight = 1
	}
	if c == nil {
		c = e.Client()
	}
	capacity := inflight + queueDepth
	return &surfaceRunner{
		name:     name,
		surface:  s,
		engine:   e,
		client:   c,
		admits:   make(chan struct{}, capacity),
		queue:    make(chan string, capacity),
		workers:  inflight,
		metadata: make(chan struct{}, 4),
	}
}

// Start launches the workers, the recovery pass and the sweeper.
func (m *Manager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	m.runCtx = ctx

	for _, r := range m.runners {
		for i := 0; i < r.workers; i++ {
			m.wg.Add(1)
			go func(r *surfaceRunner, n int) {
				defer m.wg.Done()
				m.worker(ctx, r, n)
			}(r, i)
		}
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.sweep(ctx)
	}()
}

// Drain waits for in-flight jobs to finish on their own, up to ctx's deadline.
//
// It is the difference between a rolling restart that finishes its work and one that abandons it:
// every job still running when Stop cancels has to be redone from its stored input on the next
// boot, and until then its client sees a status that is not advancing.
func (m *Manager) Drain(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		var inFlight int
		for _, r := range m.runners {
			inFlight += len(r.admits)
		}
		if inFlight == 0 {
			return
		}
		select {
		case <-ctx.Done():
			m.log.Warn("shutdown grace expired with jobs still running; "+
				"they will be reclaimed on the next start", "inFlight", inFlight)
			return
		case <-t.C:
		}
	}
}

// Recover reclaims work abandoned by a previous process.
//
// It is called explicitly, before the listener opens, rather than from Start: a scan racing live
// traffic can pick up a row a submit has just committed and not yet enqueued, and run it twice.
//
// ctx bounds the scan only. The feeder it leaves behind runs under the manager's own context, so
// Stop can release it — see the INVARIANT on Manager.wg.
func (m *Manager) Recover(ctx context.Context) { m.recover(ctx, true) }

// tracked returns the context every wg-tracked goroutine must use: the manager's own, falling back
// to the caller's only when Start has not run (which is a programming error everywhere but tests).
func (m *Manager) tracked(ctx context.Context) context.Context {
	if m.runCtx != nil {
		return m.runCtx
	}
	return ctx
}

// Stop cancels the workers and waits for in-flight jobs to finish or abort.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

// Admit reserves capacity for one job.
//
// It never blocks. With the default QueueDepth of zero, a submit is refused as soon as every
// worker is busy, which is what the deployment asks for: the client gets an immediate,
// well-formed 429 with a Retry-After rather than an unbounded wait.
func (m *Manager) Admit(surface string) (release func(), err error) {
	r, ok := m.runners[surface]
	if !ok {
		return nil, fmt.Errorf("jobs: unknown surface %q", surface)
	}
	select {
	case r.admits <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-r.admits }) }, nil
	default:
		return nil, ErrBusy
	}
}

// AdmitMetadata reserves a slot on the read-only proxy lane.
func (m *Manager) AdmitMetadata(surface string) (release func(), err error) {
	return acquire(m.runners[surface], func(r *surfaceRunner) chan struct{} { return r.metadata })
}

func acquire(r *surfaceRunner, pick func(*surfaceRunner) chan struct{}) (func(), error) {
	if r == nil {
		return nil, ErrBusy
	}
	ch := pick(r)
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, nil
	default:
		return nil, ErrBusy
	}
}

// Track marks a :syncAnalyze request's work folder as in use until release is called, so the
// sweeper cannot remove it from under a long analysis.
func (m *Manager) Track(id string) (release func()) {
	if !m.claim(id) {
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(func() { m.unclaim(id) }) }
}

// claimed reports whether this process has id queued, running or tracked.
func (m *Manager) claimed(id string) bool {
	m.claimMu.Lock()
	defer m.claimMu.Unlock()
	_, ok := m.claims[id]
	return ok
}

// claim reserves a job id for this process, reporting false if it is already claimed.
func (m *Manager) claim(id string) bool {
	m.claimMu.Lock()
	defer m.claimMu.Unlock()
	if _, dup := m.claims[id]; dup {
		return false
	}
	m.claims[id] = struct{}{}
	return true
}

func (m *Manager) unclaim(id string) {
	m.claimMu.Lock()
	delete(m.claims, id)
	m.claimMu.Unlock()
}

// Enqueue hands an admitted job to the workers.
//
// The caller must already hold a slot from Admit and must have committed the job row, because the
// 202 has been promised on the strength of that row existing.
func (m *Manager) Enqueue(surface, jobID string) error {
	r, ok := m.runners[surface]
	if !ok {
		return fmt.Errorf("jobs: unknown surface %q", surface)
	}
	if !m.claim(jobID) {
		// Already queued or running here. Silently correct: the job is going to be processed.
		return nil
	}
	select {
	case r.queue <- jobID:
		return nil
	default:
		m.unclaim(jobID)
		// Unreachable while queue capacity matches admission capacity, but a silent drop here
		// would strand a client polling forever, so it is reported rather than ignored.
		return ErrBusy
	}
}

// Capacity reports current occupancy for the admin surface, for configured surfaces only.
//
// The upstream numbers come from the engine's scheduler, which every analysis call on the surface
// goes through. The key names are the admin surface's contract and stay stable.
func (m *Manager) Capacity() map[string]map[string]int {
	out := map[string]map[string]int{}
	for name, r := range m.runners {
		st := r.engine.Sched().Stats()
		out[name] = map[string]int{
			"inFlight":        len(r.admits),
			"limit":           cap(r.admits),
			"workers":         r.workers,
			"queued":          len(r.queue),
			"upstreamInUse":   st.InUse,
			"upstreamWaiting": st.Waiting,
			"upstreamLimit":   st.Limit,
			"metadataInUse":   len(r.metadata),
		}
	}
	return out
}

// Client is a configured surface's upstream client, for health checks and the GET passthroughs,
// or nil when the surface is not configured.
func (m *Manager) Client(surface string) *upstream.Client {
	if r, ok := m.runners[surface]; ok {
		return r.client
	}
	return nil
}

// Engine is a configured surface's engine, or nil when the surface is not configured.
func (m *Manager) Engine(surface string) *engine.Engine {
	if r, ok := m.runners[surface]; ok {
		return r.engine
	}
	return nil
}

func (m *Manager) worker(ctx context.Context, r *surfaceRunner, n int) {
	owner := fmt.Sprintf("%s-%d", r.name, n)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.queue:
			m.run(ctx, r, owner, id)
		}
	}
}

// run executes one job to a terminal state.
//
// The admission slot is released here rather than at submit time, so accepted work stays bounded
// until it is finished. The calls the job makes into the container are bounded separately, by the
// engine's scheduler.
func (m *Manager) run(ctx context.Context, r *surfaceRunner, owner, id string) {
	log := m.log.With("job", id, "surface", r.name)

	defer func() {
		m.unclaim(id)
		<-r.admits
		if rec := recover(); rec != nil {
			log.Error("worker panicked", "panic", rec)
			m.failJob(context.WithoutCancel(ctx), r, id,
				azerr.Internal(r.surface, "An unexpected error occurred."), 0, 0, 0, 0)
		}
	}()

	job, err := m.store.Get(ctx, id)
	if err != nil {
		log.Error("job vanished before it ran", "error", err)
		return
	}
	if job.Status == store.StatusSucceeded || job.Status == store.StatusFailed {
		return // already terminal, e.g. reclaimed twice
	}

	now := m.now()
	if err := m.store.MarkRunning(ctx, id, owner, now.Add(leaseDuration), now); err != nil {
		log.Error("could not mark job running", "error", err)
		return
	}

	stopHeartbeat := m.startHeartbeat(ctx, id)
	defer stopHeartbeat()

	query, _ := url.ParseQuery(job.Query)
	out, err := r.engine.Run(ctx, engine.Input{
		JobID:       id,
		ContentType: job.ContentType,
		Path:        m.store.Blob.Path(id, store.KindInput),
		Size:        job.InputBytes,
		ModelID:     job.ModelID,
		Prefix:      job.Prefix,
		Query:       query,
	})
	if err != nil {
		var attempts, parts int
		var upstreamMS int64
		if out != nil {
			attempts, parts, upstreamMS = out.Attempts, out.Parts, out.UpstreamMS
			out.Cleanup()
		}
		// Cancellation is checked first and on the context itself, not on the error. During
		// shutdown the engine may already have wrapped the cancellation in a surface-shaped
		// error, and classifying on the error alone would mark a job terminally failed that the
		// recovery pass could have resumed — after its 202 was already sent.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Info("job interrupted by shutdown; it will be recovered on the next start")
			return
		}
		apiErr, ok := azerr.AsAPIError(err)
		if !ok {
			// The raw Go error is logged, never returned. It routinely names absolute PVC paths
			// and internal packages ("open /data/blobs/ab/cd/....input: no such file"), and this
			// message is served to any unauthenticated caller polling the operation.
			log.Error("analysis failed with an unmapped error", "error", err)
			apiErr = azerr.Internal(r.surface, "")
		}
		log.Warn("analysis failed", "code", apiErr.Code, "message", apiErr.Message)
		m.failJob(context.WithoutCancel(ctx), r, id, apiErr, 0, upstreamMS, attempts, parts)
		return
	}
	defer out.Cleanup()

	size, err := m.finish(ctx, job, out)
	if err != nil {
		log.Error("could not persist result", "error", err)
		m.failJob(context.WithoutCancel(ctx), r, id,
			azerr.Internal(r.surface, "The result could not be stored."), 0, out.UpstreamMS,
			out.Attempts, out.Parts)
		return
	}
	log.Info("job succeeded", "upstreamMs", out.UpstreamMS, "attempts", out.Attempts,
		"parts", out.Parts, "fallback", out.Fallback, "resultBytes", size)
}

// finish composes and stores the exact envelope the gateway will later serve, and returns its
// size.
//
// Writing the final bytes now, rather than assembling them per poll, means a successful poll is a
// plain file stream with an exact Content-Length: no re-serialisation, and no parsing of a result
// that Azure allows to reach 500 MB.
// ctx is the live worker context, used only to notice shutdown. The store writes below run under
// a derived uncancellable context: the analysis is already done, and losing it to a signal would
// mean paying the container to redo it.
func (m *Manager) finish(ctx context.Context, job *store.Job, out *engine.Output) (int64, error) {
	storeCtx := context.WithoutCancel(ctx)

	src, err := os.Open(out.Path)
	if err != nil {
		return 0, fmt.Errorf("jobs: reopen engine output: %w", err)
	}
	defer func() { _ = src.Close() }()

	now := m.now()
	w, err := m.store.Blob.Create(job.ID, store.KindResult, 0)
	if err != nil {
		return 0, err
	}

	prefix := fmt.Sprintf(
		`{"status":"succeeded","createdDateTime":"%s","lastUpdatedDateTime":"%s","analyzeResult":`,
		job.CreatedAt.UTC().Format(TimeFormat), now.Format(TimeFormat))

	if _, err := io.WriteString(w, prefix); err != nil {
		w.Abort()
		return 0, err
	}
	if _, err := io.Copy(w, io.NewSectionReader(src, out.Start, out.End-out.Start)); err != nil {
		w.Abort()
		return 0, err
	}
	if _, err := io.WriteString(w, "}"); err != nil {
		w.Abort()
		return 0, err
	}
	size := w.Written()
	if err := w.Commit(); err != nil {
		return 0, err
	}

	ok, err := m.store.Succeed(storeCtx, job.ID, m.store.Blob.Path(job.ID, store.KindResult), size,
		out.UpstreamMS, out.Attempts, out.Parts, now)
	if err != nil {
		return 0, err
	}
	if !ok {
		// The row went away while this job ran — a client DELETE, or disk-pressure eviction. The
		// result just written references nothing, so it is removed here rather than left for the
		// orphan sweep to find later.
		m.log.Info("job completed but its row was already deleted; discarding the result",
			"job", job.ID)
		_ = m.store.Blob.RemoveAll(job.ID)
		return size, nil
	}
	// The input is only needed to retry the upstream call, which can no longer happen.
	_ = m.store.Blob.Remove(job.ID, store.KindInput)
	return size, nil
}

// failJob records a terminal failure with the Azure-shaped error the client will receive.
//
// attempts and parts are the failed run's counts, 0 when it never reached the container.
func (m *Manager) failJob(ctx context.Context, r *surfaceRunner, id string, e *azerr.APIError,
	upstreamStatus int, upstreamMS int64, attempts, parts int) {
	payload, err := json.Marshal(e.OperationError())
	if err != nil {
		payload = []byte(`{"code":"InternalServerError","message":"An unexpected error occurred."}`)
	}
	ok, err := m.store.Fail(ctx, id, string(payload), upstreamStatus, upstreamMS, attempts, parts,
		m.now())
	if err != nil {
		m.log.Error("could not record job failure", "job", id, "error", err)
	}
	if !ok {
		_ = m.store.Blob.RemoveAll(id)
		return
	}
	_ = m.store.Blob.Remove(id, store.KindInput)
}

// startHeartbeat keeps a running job's lease fresh so the recovery pass does not reclaim work
// that is still progressing. A layout analysis can legitimately run for many minutes.
func (m *Manager) startHeartbeat(ctx context.Context, id string) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = m.store.Heartbeat(context.WithoutCancel(ctx), id, m.now().Add(leaseDuration))
			}
		}
	}()
	return func() { close(done) }
}
