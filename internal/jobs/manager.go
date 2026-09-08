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
	"mime"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
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
	name     string
	analyzer upstream.Analyzer
	// admits bounds work in the system: one token per accepted-but-unfinished job. Its capacity
	// is MaxInflight + QueueDepth, so with the default QueueDepth of zero a submit is rejected
	// the moment every worker is busy.
	admits chan struct{}
	queue  chan string
	// workers is how many jobs actually run at once.
	workers int
	// upstream bounds concurrent calls into the container. It is separate from admits because
	// admits is sized MaxInflight+QueueDepth: with a queue configured, admission alone would let
	// synchronous passthroughs put more work on the container than MaxInflight allows.
	upstream chan struct{}
	// metadata bounds the read-only proxy lane. Those calls are cheap and must not be able to
	// starve analysis, nor pile up without limit against a wedged container.
	metadata chan struct{}
}

// Manager runs jobs for both surfaces.
type Manager struct {
	cfg   *config.Config
	store *store.Store
	log   *slog.Logger

	runners map[string]*surfaceRunner

	wg     sync.WaitGroup
	cancel context.CancelFunc

	// claims is the set of job ids this process already has queued or running.
	//
	// Nothing may enqueue an id twice. Two paths can reach the same row — a submit and the reclaim
	// pass, or the boot reclaim and the periodic one — and each duplicate is a second billed
	// analysis against the container for a result only one of them will store. Ordering the callers
	// carefully is not enough; this makes it impossible.
	claimMu sync.Mutex
	claims  map[string]struct{}

	// now is indirected so tests can control time.
	now func() time.Time
}

// Options configures a Manager.
type Options struct {
	Config *config.Config
	Store  *store.Store
	Logger *slog.Logger
	DI     upstream.Analyzer
	Read   upstream.Analyzer
	Now    func() time.Time
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
	m.runners[SurfaceDI] = newRunner(SurfaceDI, opts.DI, opts.Config.DI.MaxInflight, opts.Config.QueueDepth)
	m.runners[SurfaceRead] = newRunner(SurfaceRead, opts.Read, opts.Config.Read.MaxInflight, opts.Config.QueueDepth)
	return m
}

// Surface names, matching the store's surface column.
const (
	SurfaceDI   = "di"
	SurfaceRead = "read"
)

func newRunner(name string, a upstream.Analyzer, inflight, queueDepth int) *surfaceRunner {
	if inflight < 1 {
		inflight = 1
	}
	capacity := inflight + queueDepth
	return &surfaceRunner{
		name:     name,
		analyzer: a,
		admits:   make(chan struct{}, capacity),
		queue:    make(chan string, capacity),
		workers:  inflight,
		upstream: make(chan struct{}, inflight),
		metadata: make(chan struct{}, 4),
	}
}

// Start launches the workers, the recovery pass and the sweeper.
func (m *Manager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)

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
func (m *Manager) Recover(ctx context.Context) { m.recover(ctx, true) }

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

// AdmitUpstream reserves one of the container's concurrency slots.
//
// Workers hold one for the duration of an analysis and synchronous passthroughs hold one for the
// duration of their call, so the container never sees more than MaxInflight concurrent requests
// regardless of how the admission queue is configured.
func (m *Manager) AdmitUpstream(surface string) (release func(), err error) {
	return acquire(m.runners[surface], func(r *surfaceRunner) chan struct{} { return r.upstream })
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

// Capacity reports current occupancy for the admin surface.
func (m *Manager) Capacity() map[string]map[string]int {
	out := map[string]map[string]int{}
	for name, r := range m.runners {
		out[name] = map[string]int{
			"inFlight":      len(r.admits),
			"limit":         cap(r.admits),
			"workers":       r.workers,
			"queued":        len(r.queue),
			"upstreamInUse": len(r.upstream),
			"upstreamLimit": cap(r.upstream),
			"metadataInUse": len(r.metadata),
		}
	}
	return out
}

// Analyzer exposes a surface's upstream client, for health reporting.
func (m *Manager) Analyzer(surface string) upstream.Analyzer {
	if r, ok := m.runners[surface]; ok {
		return r.analyzer
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
// The admission slot is released here rather than at submit time, so the number of concurrent
// upstream calls is bounded by the same number that bounds accepted work.
func (m *Manager) run(ctx context.Context, r *surfaceRunner, owner, id string) {
	log := m.log.With("job", id, "surface", r.name)

	defer func() {
		m.unclaim(id)
		<-r.admits
		if rec := recover(); rec != nil {
			log.Error("worker panicked", "panic", rec)
			m.failJob(context.WithoutCancel(ctx), r, id,
				azerr.Internal(r.analyzer.Surface(), "An unexpected error occurred."), 0, 0)
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

	// Hold a container slot for the analysis itself, so synchronous passthroughs and workers
	// together never exceed MaxInflight against the upstream.
	releaseUpstream, err := m.AdmitUpstream(r.name)
	for err != nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		releaseUpstream, err = m.AdmitUpstream(r.name)
	}
	defer releaseUpstream()

	doc := upstream.Document{
		ContentType: job.ContentType,
		Size:        job.InputBytes,
		Open: func() (io.ReadCloser, error) {
			f, _, err := m.store.Blob.Open(id, store.KindInput)
			return f, err
		},
	}
	query, _ := url.ParseQuery(job.Query)

	res, err := r.analyzer.Analyze(ctx, doc, upstream.Request{
		ModelID:          job.ModelID,
		Query:            query,
		Prefix:           job.Prefix,
		RequireOperation: wantsArtifacts(query),
	})
	if err != nil {
		// Cancellation is checked first and on the context itself, not on the error. During
		// shutdown the upstream client may already have wrapped the cancellation in a
		// surface-shaped error, and classifying on the error alone would mark a job terminally
		// failed that the recovery pass could have resumed — after its 202 was already sent.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Info("job interrupted by shutdown; it will be recovered on the next start")
			return
		}
		apiErr, ok := azerr.AsAPIError(err)
		if !ok {
			apiErr = azerr.Internal(r.analyzer.Surface(), err.Error())
		}
		log.Warn("analysis failed", "code", apiErr.Code, "message", apiErr.Message)
		m.failJob(context.WithoutCancel(ctx), r, id, apiErr, 0, 0)
		return
	}
	defer res.Cleanup()

	if err := m.finish(ctx, r, job, res); err != nil {
		log.Error("could not persist result", "error", err)
		m.failJob(context.WithoutCancel(ctx), r, id,
			azerr.Internal(r.analyzer.Surface(), "The result could not be stored."), 0, res.UpstreamMS)
		return
	}
	log.Info("job succeeded",
		"mode", string(res.Mode), "upstreamMs", res.UpstreamMS, "resultBytes", res.Size())
}

// finish composes and stores the exact envelope the gateway will later serve.
//
// Writing the final bytes now, rather than assembling them per poll, means a successful poll is a
// plain file stream with an exact Content-Length: no re-serialisation, and no parsing of a result
// that Azure allows to reach 500 MB.
// ctx is the live worker context, used only to notice shutdown. The store writes below run under
// a derived uncancellable context: the analysis is already done, and losing it to a signal would
// mean paying the container to redo it.
func (m *Manager) finish(ctx context.Context, r *surfaceRunner, job *store.Job, res *upstream.Result) error {
	storeCtx := context.WithoutCancel(ctx)

	src, err := os.Open(res.Path)
	if err != nil {
		return fmt.Errorf("jobs: reopen upstream result: %w", err)
	}
	defer func() { _ = src.Close() }()

	now := m.now()
	w, err := m.store.Blob.Create(job.ID, store.KindResult, 0)
	if err != nil {
		return err
	}

	prefix := fmt.Sprintf(
		`{"status":"succeeded","createdDateTime":"%s","lastUpdatedDateTime":"%s","analyzeResult":`,
		job.CreatedAt.UTC().Format(TimeFormat), now.Format(TimeFormat))

	if _, err := io.WriteString(w, prefix); err != nil {
		w.Abort()
		return err
	}
	if _, err := io.Copy(w, io.NewSectionReader(src, res.Start, res.End-res.Start)); err != nil {
		w.Abort()
		return err
	}
	if _, err := io.WriteString(w, "}"); err != nil {
		w.Abort()
		return err
	}
	size := w.Written()
	if err := w.Commit(); err != nil {
		return err
	}

	hasPDF, figures := m.fetchArtifacts(ctx, storeCtx, r, job, res)

	ok, err := m.store.Succeed(storeCtx, job.ID, m.store.Blob.Path(job.ID, store.KindResult), size,
		res.Mode, res.UpstreamMS, hasPDF, strings.Join(figures, ","), now)
	if err != nil {
		return err
	}
	if !ok {
		// The row went away while this job ran — a client DELETE, or disk-pressure eviction. The
		// artifacts just written reference nothing, so they are removed here rather than left for
		// the orphan sweep to find later.
		m.log.Info("job completed but its row was already deleted; discarding the result",
			"job", job.ID)
		_ = m.store.Blob.RemoveAll(job.ID)
		return nil
	}
	// The input is only needed to retry the upstream call, which can no longer happen.
	_ = m.store.Blob.Remove(job.ID, store.KindInput)
	return nil
}

// fetchArtifacts eagerly retrieves result files the client asked for.
//
// They must be fetched now: the result-file endpoints are addressed by the container's own
// operation id, whose lifetime is governed by the container's StorageTimeToLiveInMinutes and whose
// owning replica may disappear at any time.
//
// The phase is bounded twice over, because an artifact is a bonus and the analysis it belongs to
// is already stored. It gets one aggregate budget, so a wedged container cannot turn N figures
// into N times the per-request timeout; and it stops between artifacts once the worker context is
// cancelled, so a shutdown is never held up by work no client is waiting on. A missing artifact
// yields a clean 404 from the gateway, which is a far better outcome than a SIGKILLed pod.
//
// liveCtx observes shutdown; storeCtx survives it, for the writes that must land.
func (m *Manager) fetchArtifacts(liveCtx, storeCtx context.Context, r *surfaceRunner,
	job *store.Job, res *upstream.Result) (hasPDF bool, figures []string) {

	di, ok := r.analyzer.(*upstream.DIClient)
	if !ok || res.UpstreamOpID == "" {
		return false, nil
	}
	query, _ := url.ParseQuery(job.Query)
	outputs := outputSet(query)
	if len(outputs) == 0 {
		return false, nil
	}
	log := m.log.With("job", job.ID)

	budget := m.cfg.ArtifactFetchTimeout
	if budget <= 0 {
		budget = 2 * time.Minute
	}
	actx, cancel := context.WithTimeout(storeCtx, budget)
	defer cancel()

	// abort reports whether to stop fetching, and says why once.
	var aborted bool
	abort := func(what string) bool {
		if aborted {
			return true
		}
		switch {
		case liveCtx.Err() != nil:
			log.Warn("shutting down; skipping remaining result files", "pending", what)
		case actx.Err() != nil:
			log.Warn("result-file budget exhausted; skipping the rest",
				"budget", budget.String(), "pending", what)
		default:
			return false
		}
		aborted = true
		return true
	}

	if outputs["pdf"] {
		if !abort("pdf") {
			path, _, ct, err := di.FetchArtifact(actx, job.ModelID, res.UpstreamOpID, "/pdf", query)
			switch {
			case err != nil:
				log.Warn("could not fetch searchable pdf", "error", err)
			case !isPDF(ct):
				// A probed container answers this route 200 with application/json. Storing that
				// as a PDF would serve a client a file that is not one, with the wrong
				// Content-Type, and look like a successful capture.
				log.Warn("result-file endpoint did not return a pdf; not storing it",
					"contentType", ct)
				_ = os.Remove(path)
			default:
				if err := m.moveInto(path, job.ID, store.KindPDF); err != nil {
					log.Warn("could not store searchable pdf", "error", err)
				} else {
					hasPDF = true
				}
			}
		}
	}

	if outputs["figures"] {
		for _, figID := range figureIDs(res, log) {
			if abort("figure " + figID) {
				break
			}
			path, _, _, err := di.FetchArtifact(actx, job.ModelID, res.UpstreamOpID,
				"/figures/"+url.PathEscape(figID), query)
			if err != nil {
				log.Warn("could not fetch figure", "figure", figID, "error", err)
				continue
			}
			if err := m.moveInto(path, job.ID, store.KindFigure(figID)); err != nil {
				log.Warn("could not store figure", "figure", figID, "error", err)
				continue
			}
			figures = append(figures, figID)
		}
	}
	return hasPDF, figures
}

// moveInto installs a downloaded artifact into the blob store atomically.
func (m *Manager) moveInto(tmpPath, jobID string, kind store.Kind) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}()
	_, err = m.store.Blob.WriteFrom(jobID, kind, f, 0)
	return err
}

// figureIDs pulls the figure identifiers out of an analyzeResult.
//
// Only the figures member is decoded, located by byte range, so a large result is not loaded to
// read a short list.
func figureIDs(res *upstream.Result, log *slog.Logger) []string {
	f, err := os.Open(res.Path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	section := io.NewSectionReader(f, res.Start, res.End-res.Start)
	start, end, found, err := jsonx.ValueRange(section, res.End-res.Start, "figures")
	if err != nil || !found {
		return nil
	}
	const maxFiguresBytes = 4 << 20
	if end-start > maxFiguresBytes {
		log.Warn("figures list too large to enumerate", "bytes", end-start)
		return nil
	}
	raw := make([]byte, end-start)
	if _, err := section.ReadAt(raw, start); err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	var figs []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &figs); err != nil {
		return nil
	}
	out := make([]string, 0, len(figs))
	for _, fg := range figs {
		if fg.ID != "" {
			out = append(out, fg.ID)
		}
	}
	return out
}

// outputSet parses the DI output query parameter, which may repeat or be comma-separated.
func outputSet(q url.Values) map[string]bool {
	out := map[string]bool{}
	for _, v := range q["output"] {
		for _, part := range strings.Split(v, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part != "" {
				out[part] = true
			}
		}
	}
	return out
}

// wantsArtifacts reports whether a submit needs the container's operation id to survive the call.
func wantsArtifacts(q url.Values) bool {
	o := outputSet(q)
	return o["pdf"] || o["figures"]
}

// failJob records a terminal failure with the Azure-shaped error the client will receive.
func (m *Manager) failJob(ctx context.Context, r *surfaceRunner, id string, e *azerr.APIError,
	upstreamStatus int, upstreamMS int64) {
	payload, err := json.Marshal(e.OperationError())
	if err != nil {
		payload = []byte(`{"code":"InternalServerError","message":"An unexpected error occurred."}`)
	}
	ok, err := m.store.Fail(ctx, id, string(payload), store.UpstreamMode(""),
		upstreamStatus, upstreamMS, m.now())
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

// isPDF reports whether a Content-Type actually denotes a PDF.
func isPDF(contentType string) bool {
	ct, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return ct == "application/pdf"
}
