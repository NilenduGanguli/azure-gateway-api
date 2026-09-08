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
	// workers is how many jobs actually run at once, and therefore how many concurrent calls the
	// container sees.
	workers int
}

// Manager runs jobs for both surfaces.
type Manager struct {
	cfg   *config.Config
	store *store.Store
	log   *slog.Logger

	runners map[string]*surfaceRunner

	wg     sync.WaitGroup
	cancel context.CancelFunc

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
		m.recover(ctx)
	}()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.sweep(ctx)
	}()
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

// Enqueue hands an admitted job to the workers.
//
// The caller must already hold a slot from Admit and must have committed the job row, because the
// 202 has been promised on the strength of that row existing.
func (m *Manager) Enqueue(surface, jobID string) error {
	r, ok := m.runners[surface]
	if !ok {
		return fmt.Errorf("jobs: unknown surface %q", surface)
	}
	select {
	case r.queue <- jobID:
		return nil
	default:
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
			"inFlight": len(r.admits),
			"limit":    cap(r.admits),
			"workers":  r.workers,
			"queued":   len(r.queue),
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
		RequireOperation: wantsArtifacts(query),
	})
	if err != nil {
		apiErr, ok := azerr.AsAPIError(err)
		if !ok {
			if errors.Is(err, context.Canceled) {
				// Shutting down. Leave the job for the recovery pass rather than failing a job
				// the client is still entitled to.
				log.Info("job interrupted by shutdown; will be recovered")
				return
			}
			apiErr = azerr.Internal(r.analyzer.Surface(), err.Error())
		}
		log.Warn("analysis failed", "code", apiErr.Code, "message", apiErr.Message)
		m.failJob(context.WithoutCancel(ctx), r, id, apiErr, 0, 0)
		return
	}
	defer res.Cleanup()

	if err := m.finish(context.WithoutCancel(ctx), r, job, res); err != nil {
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
func (m *Manager) finish(ctx context.Context, r *surfaceRunner, job *store.Job, res *upstream.Result) error {
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

	hasPDF, figures := m.fetchArtifacts(ctx, r, job, res)

	if err := m.store.Succeed(ctx, job.ID, m.store.Blob.Path(job.ID, store.KindResult), size,
		res.Mode, res.UpstreamMS, hasPDF, strings.Join(figures, ","), now); err != nil {
		return err
	}
	// The input is only needed to retry the upstream call, which can no longer happen.
	_ = m.store.Blob.Remove(job.ID, store.KindInput)
	return nil
}

// fetchArtifacts eagerly retrieves result files the client asked for.
//
// They must be fetched now: the result-file endpoints are addressed by the container's own
// operation id, whose lifetime is governed by the container's StorageTimeToLiveInMinutes and
// whose owning replica may disappear at any time. Failures are logged, not fatal — the analysis
// itself succeeded, and an artifact the client never requests should not fail the job.
func (m *Manager) fetchArtifacts(ctx context.Context, r *surfaceRunner, job *store.Job,
	res *upstream.Result) (hasPDF bool, figures []string) {

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

	if outputs["pdf"] {
		path, _, _, err := di.FetchArtifact(ctx, job.ModelID, res.UpstreamOpID, "/pdf", query)
		if err != nil {
			log.Warn("could not fetch searchable pdf", "error", err)
		} else {
			if err := m.moveInto(path, job.ID, store.KindPDF); err != nil {
				log.Warn("could not store searchable pdf", "error", err)
			} else {
				hasPDF = true
			}
		}
	}

	if outputs["figures"] {
		for _, figID := range figureIDs(res, log) {
			path, _, _, err := di.FetchArtifact(ctx, job.ModelID, res.UpstreamOpID,
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
	if err := m.store.Fail(ctx, id, string(payload), store.UpstreamMode(""),
		upstreamStatus, upstreamMS, m.now()); err != nil {
		m.log.Error("could not record job failure", "job", id, "error", err)
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
