// Package engine analyses one document through a surface's container: whole, or split into parts
// that are analysed in parallel and merged back into the result the whole document would have had
// (splitting spec §1, §3–§5).
//
// Every call to Azure goes through the surface's fair scheduler and is one upstream.Client.Do
// attempt. A whole document gets the sync-only spec's attempt rules; a part gets the splitting
// spec's: a short timeout, a hedge on idle capacity, a few attempts, then halving, and a re-check
// against the original before a part's terminal answer may fail the job. So a document fails only
// when Azure would have failed it whole.
package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/sched"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// The constants of splitting spec §7 and sync-only spec §1. They are not settings: each is tied
// to how the containers behave, and OPERATIONS documents them.
const (
	// PartTimeout bounds one part attempt. A 3–5 page part takes about 2 s, so a hung pod costs a
	// minute rather than the whole-document timeout.
	PartTimeout = 60 * time.Second
	// PartAttempts is how many attempts a part gets at each size before it is halved.
	PartAttempts = 3
	// WholeAttempts is how many attempts a whole document gets.
	WholeAttempts = 5
	// JobDeadline bounds a whole job, every part included: the containers' own maximum task time.
	JobDeadline = 60 * time.Minute
	// HedgeMin and HedgeMax clamp the hedge delay, HedgeFactor × the median part duration.
	HedgeMin    = 5 * time.Second
	HedgeMax    = 30 * time.Second
	HedgeFactor = 4
	// HedgeSamples is how many recent successful part durations the median is taken over, and
	// HedgeMinSample how many it needs before it is trusted over HedgeMax.
	HedgeSamples   = 200
	HedgeMinSample = 20
)

// retryOnceLimit is how many attempts of a whole document may end in the RetryOnce class (its own
// deadline, a 500 or a 504): a slow document times out again on every pod.
const retryOnceLimit = 2

// Options configures an Engine. Client, Sched and WorkRoot are required.
type Options struct {
	Client     *upstream.Client
	Sched      *sched.Scheduler
	Splitter   *pdfsplit.Splitter // nil: never split
	SplitPages int                // SPLIT_PAGES; 0 turns splitting off
	WorkRoot   string             // ${DATA_DIR}/work
	// WholeTimeout is the per-attempt timeout for a document sent whole, and for a re-check:
	// DI_UPSTREAM_TIMEOUT or READ_SYNC_TIMEOUT. Zero means the client's configured timeout.
	WholeTimeout time.Duration
	Metrics      *Metrics // nil: New creates one
	Log          *slog.Logger
	Now          func() time.Time
	Jitter       func() float64 // nil: rand.Float64
}

// Engine runs documents through one surface's container. It is safe for concurrent use; one
// engine serves every job of its surface, so the hedge delay learns from all of them.
type Engine struct {
	o       Options
	metrics *Metrics
	log     *slog.Logger
	now     func() time.Time
	jitter  func() float64
	samples ring

	// Seams for tests. Production uses the constants and upstream.Backoff.
	partTimeout time.Duration
	jobDeadline time.Duration
	hedgeDelay  func(pages []int) time.Duration
	backoff     func(attempt int, retryAfter time.Duration) time.Duration
}

// New builds an engine.
func New(o Options) *Engine {
	e := &Engine{o: o, metrics: o.Metrics, log: o.Log, now: o.Now, jitter: o.Jitter,
		partTimeout: PartTimeout, jobDeadline: JobDeadline}
	if e.metrics == nil {
		e.metrics = NewMetrics()
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.jitter == nil {
		e.jitter = rand.Float64
	}
	e.hedgeDelay = func([]int) time.Duration { return e.samples.delay() }
	e.backoff = func(attempt int, retryAfter time.Duration) time.Duration {
		return upstream.Backoff(attempt, retryAfter, e.jitter)
	}
	return e
}

// Metrics returns the engine's metrics.
func (e *Engine) Metrics() *Metrics { return e.metrics }

// Sched returns the scheduler every call of the engine takes a slot from.
func (e *Engine) Sched() *sched.Scheduler { return e.o.Sched }

// Client returns the surface's upstream client.
func (e *Engine) Client() *upstream.Client { return e.o.Client }

// Input is one document to analyse.
type Input struct {
	JobID       string
	ContentType string
	Path        string // stored input on the PVC
	Size        int64
	ModelID     string     // DI
	Prefix      string     // DI family
	Query       url.Values // forwarded, policy-checked; may contain pages
}

// Output is the analyzeResult on disk.
type Output struct {
	Path       string
	Start, End int64 // the bare AnalyzeResult object inside Path
	// Bare reports that the container's synchronous route answers a bare AnalyzeResult rather
	// than an envelope, so a sync client can be answered in the same shape. For a merged result it
	// is the first part's; the merged file itself is always the bare object.
	Bare bool
	// Attempts counts calls to Azure; a hedge belongs to its attempt and is not counted again.
	Attempts int
	// Parts is the number of results the job was assembled from: 1 for a whole document.
	Parts    int
	Halvings int
	Hedges   int
	// Fallback is the reason a document that could have been split was sent whole, or "".
	Fallback string
	// UpstreamMS is the time Azure spent on the calls whose results make up the output.
	UpstreamMS int64

	cleanup func()
}

// Cleanup removes the output's files. Safe to call more than once, and on nil.
func (o *Output) Cleanup() {
	if o == nil || o.cleanup == nil {
		return
	}
	o.cleanup()
	o.cleanup = nil
}

// tally is a job's running counts, shared by its parts.
type tally struct {
	attempts, halvings, hedges, upstreamMS atomic.Int64
}

// Run analyses one document. Errors are *azerr.APIError, or the context's error on shutdown: the
// job then stays running and recovery re-runs it from its stored input.
func (e *Engine) Run(ctx context.Context, in Input) (*Output, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, e.jobDeadline)
	defer cancel()
	log := e.log.With("job", in.JobID, "surface", e.o.Client.Surface().String())

	work, err := newWorkdir(e.o.WorkRoot, in.JobID, in.Path)
	if err != nil {
		log.Error("engine: no work folder", "err", err)
		return nil, azerr.Internal(e.o.Client.Surface(), "")
	}
	var t tally
	out, err := e.run(ctx, in, work, &t, log)
	if err != nil {
		work.remove()
		// Cancellation is classified on the context first: a shutdown says nothing about the
		// document, whatever error it surfaced as.
		if perr := parent.Err(); perr != nil {
			return nil, perr
		}
		if ctx.Err() != nil {
			log.Warn("engine: job deadline passed", "deadline", e.jobDeadline)
			return nil, azerr.Internal(e.o.Client.Surface(), fmt.Sprintf(
				"The analysis did not complete within the job deadline of %s.", minutes(e.jobDeadline)))
		}
		var apiErr *azerr.APIError
		if errors.As(err, &apiErr) {
			log.Info("engine: job failed", "err", apiErr, "attempts", t.attempts.Load(),
				"halvings", t.halvings.Load(), "hedges", t.hedges.Load())
			return nil, apiErr
		}
		// The gateway's own failure: a full PVC, an unreadable input. The text names paths, so
		// it goes to the log and the client hears only that something went wrong.
		log.Error("engine: job failed in the gateway", "err", err)
		return nil, azerr.Internal(e.o.Client.Surface(), "")
	}
	out.Attempts = int(t.attempts.Load())
	out.Halvings = int(t.halvings.Load())
	out.Hedges = int(t.hedges.Load())
	e.metrics.jobPartCount(out.Parts)
	log.Info("engine: job analysed", "parts", out.Parts, "attempts", out.Attempts,
		"halvings", out.Halvings, "hedges", out.Hedges, "fallback", out.Fallback)
	return out, nil
}

// minutes renders a deadline the way a client reads it: "60 minutes".
func minutes(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}

// run decides between whole and split and does it. The work folder is removed on return unless
// the output lives in it.
func (e *Engine) run(ctx context.Context, in Input, work *workdir, t *tally, log *slog.Logger) (*Output, error) {
	d, err := e.decide(ctx, in, work)
	if err != nil {
		return nil, err
	}
	if d.fallback != "" {
		e.metrics.fallback(d.fallback)
		log.Info("engine: sent whole", "fallback", d.fallback)
	}
	if d.parts == nil {
		out, err := e.runWhole(ctx, in, t, d.fallback, log)
		if err == nil {
			work.remove()
		}
		return out, err
	}
	return e.split(ctx, in, work, d, t, log)
}

// decision is how a document is analysed: whole when parts is nil.
type decision struct {
	fallback string     // why a split was attempted and abandoned, or ""
	pages    []int      // the selected original pages, for a split
	parts    []partFile // the cut parts, for a split
}

// partFile is a part cut to a file in the work folder.
type partFile struct {
	part pdfsplit.Part
	path string
}

// decide applies splitting spec §1. Only a gateway failure is an error; anything about the
// document itself sends it whole, exactly as the client sent it.
func (e *Engine) decide(ctx context.Context, in Input, work *workdir) (decision, error) {
	if e.o.SplitPages <= 0 || e.o.Splitter == nil {
		return decision{}, nil
	}
	src, err := pdfSource(in, work)
	if err != nil || src == "" {
		return decision{}, err
	}
	doc, err := e.o.Splitter.Open(ctx, src)
	if err != nil {
		return splitFailure(ctx, err)
	}
	// Closed before any part is analysed, and so before any halving recuts one: Recut waits for a
	// slot of the same splitter.
	defer doc.Close()

	pages, ok := selectPages(in.Query, doc.PageCount)
	if !ok {
		return decision{fallback: fallbackPagesParam}, nil
	}
	if len(pages) <= e.o.SplitPages {
		return decision{}, nil
	}
	plan := pdfsplit.Plan(pages, e.o.SplitPages)
	parts := make([]partFile, 0, len(plan))
	for _, p := range plan {
		path := work.path(partName("part", p.Pages, ".pdf"))
		err := writeFile(path, func(w io.Writer) error { return doc.WritePart(ctx, p.Pages, w) })
		if err != nil {
			return splitFailure(ctx, err)
		}
		parts = append(parts, partFile{part: p, path: path})
	}
	return decision{pages: pages, parts: parts}, nil
}

// splitFailure sorts a pdfsplit error: the document's fault sends it whole with the reason
// counted; anything else is the gateway's own failure and is not hidden behind a fallback.
func splitFailure(ctx context.Context, err error) (decision, error) {
	if cerr := ctx.Err(); cerr != nil {
		return decision{}, cerr
	}
	switch {
	case errors.Is(err, pdfsplit.ErrPanic): // before ErrUnparseable, which it wraps
		return decision{fallback: fallbackPanic}, nil
	case errors.Is(err, pdfsplit.ErrEncrypted):
		return decision{fallback: fallbackEncrypted}, nil
	case errors.Is(err, pdfsplit.ErrBudget):
		return decision{fallback: fallbackBudget}, nil
	case errors.Is(err, pdfsplit.ErrUnparseable):
		return decision{fallback: fallbackUnparseable}, nil
	}
	return decision{}, fmt.Errorf("engine: split: %w", err)
}

// selectPages is the pages to analyse: the client's pages= selection, or every page. ok is false
// when pages= is given but is not Azure's grammar or names a page the document lacks.
func selectPages(q url.Values, count int) ([]int, bool) {
	vals, given := q["pages"]
	if !given {
		all := make([]int, count)
		for i := range all {
			all[i] = i + 1
		}
		return all, true
	}
	if len(vals) != 1 {
		return nil, false
	}
	ranges, ok := pdfsplit.ParsePages(vals[0])
	if !ok {
		return nil, false
	}
	return pdfsplit.Select(ranges, count)
}

// pdfSource is the path of the PDF to split, or "" when the body is not one (splitting spec §1):
// a PDF or octet-stream body with a PDF header, or a JSON base64Source that decodes to one, which
// is written to the work folder. A urlSource is fetched by the container, so it is never split.
func pdfSource(in Input, work *workdir) (string, error) {
	mt, _, _ := mime.ParseMediaType(in.ContentType)
	switch strings.ToLower(mt) {
	case "application/pdf", "application/octet-stream":
		ok, err := hasPDFHeader(in.Path)
		if err != nil || !ok {
			return "", err
		}
		return in.Path, nil
	case "application/json":
		return decodeBase64Source(in.Path, work.path("input.pdf"))
	}
	return "", nil
}

// hasPDFHeader reports whether the file's first 1024 bytes hold a PDF header.
func hasPDFHeader(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("engine: open input: %w", err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 1024)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("engine: read input: %w", err)
	}
	return pdfsplit.IsPDF(head[:n]), nil
}

// decodeBase64Source writes the PDF a JSON body's base64Source holds to dst and returns dst, or
// "" when the body has no such PDF.
//
// The value is decoded straight from its byte range in the stored file, so a large document is
// never held in memory. A body this cannot read — not JSON, a urlSource, a value with JSON escapes
// in it, base64 that does not decode, bytes that are not a PDF — is sent whole, and the container
// gives its own answer about it.
func decodeBase64Source(src, dst string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("engine: open input: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("engine: stat input: %w", err)
	}
	members, err := jsonx.Members(f, 0, info.Size())
	if err != nil {
		return "", nil
	}
	var value *jsonx.Member
	for i, m := range members {
		switch m.Name {
		case "urlSource":
			return "", nil
		case "base64Source":
			value = &members[i]
		}
	}
	if value == nil || value.End-value.Start < 2 {
		return "", nil
	}
	quote := make([]byte, 1)
	if _, err := f.ReadAt(quote, value.Start); err != nil {
		return "", fmt.Errorf("engine: read input: %w", err)
	}
	if quote[0] != '"' {
		return "", nil
	}
	encoded := io.NewSectionReader(f, value.Start+1, value.End-value.Start-2)
	var undecodable bool
	err = writeFile(dst, func(w io.Writer) error {
		sink := &errWriter{w: w}
		_, err := io.Copy(sink, base64.NewDecoder(base64.StdEncoding, encoded))
		if err != nil && sink.err == nil {
			undecodable = true
		}
		return err
	})
	switch {
	case undecodable:
		return "", nil
	case err != nil:
		return "", fmt.Errorf("engine: decode base64Source: %w", err)
	}
	ok, err := hasPDFHeader(dst)
	if err != nil || !ok {
		return "", err
	}
	return dst, nil
}

// errWriter remembers the destination's first error, so a failed write is told apart from a body
// that does not decode.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

// document is an upstream body read from a file, re-opened for every attempt.
func document(path, contentType string, size int64) upstream.Document {
	return upstream.Document{
		ContentType: contentType,
		Size:        size,
		Open:        func() (io.ReadCloser, error) { return os.Open(path) },
	}
}

// call makes one scheduled attempt: a slot, one Do, the slot back. The error is the context's,
// when it ends while waiting for a slot.
func (e *Engine) call(ctx context.Context, job string, c upstream.Call) (upstream.Attempt, error) {
	release, err := e.o.Sched.Acquire(ctx, job)
	if err != nil {
		return upstream.Attempt{}, err
	}
	defer release()
	return e.o.Client.Do(ctx, c), nil
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ctxErr is ctx's error after an attempt came back Cancelled, which happens only once ctx has
// ended.
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

// attemptErr is a failed attempt's error. Every failure but Cancelled carries one; the fallback
// keeps a typed nil out of an error interface.
func (e *Engine) attemptErr(a upstream.Attempt) error {
	if a.Err == nil {
		return azerr.Internal(e.o.Client.Surface(), "")
	}
	return a.Err
}
