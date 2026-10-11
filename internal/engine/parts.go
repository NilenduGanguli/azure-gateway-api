package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"sync"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/merge"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// errMergeCheck is a merge that cannot be trusted; the document is analysed whole instead.
var errMergeCheck = errors.New("engine: merge check failed")

// partResult is one part's analyzeResult.
type partResult struct {
	part pdfsplit.Part
	res  *upstream.Result
}

// split analyses the parts and merges their results. A merge that fails its safety checks sends
// the document whole.
func (e *Engine) split(ctx context.Context, in Input, work *workdir, d decision, t *tally,
	log *slog.Logger) (*Output, error) {
	results, err := e.runParts(ctx, in, work, d.parts, t, log)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, r := range results {
			r.res.Cleanup()
		}
	}()

	out, err := e.merge(in, work, d.pages, results, log)
	switch {
	case errors.Is(err, errMergeCheck):
		e.metrics.fallback(fallbackMergeCheck)
		log.Warn("engine: merge check failed; sending whole", "err", err)
		out, err := e.runWhole(ctx, in, t, fallbackMergeCheck, log)
		if err == nil {
			work.remove()
		}
		return out, err
	case err != nil:
		return nil, err
	}
	for _, r := range results {
		t.upstreamMS.Add(r.res.UpstreamMS)
	}
	out.UpstreamMS = t.upstreamMS.Load()
	out.cleanup = work.remove
	return out, nil
}

// fanout runs a job's parts concurrently. The first part to fail cancels the rest through ctx,
// and its error is the job's.
type fanout struct {
	e    *Engine
	in   Input
	work *workdir
	t    *tally
	log  *slog.Logger
	// query is the client's query minus pages: a part is its own document, analysed whole.
	query url.Values

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	err     error
	results []partResult
}

func (e *Engine) runParts(ctx context.Context, in Input, work *workdir, parts []partFile, t *tally,
	log *slog.Logger) ([]partResult, error) {
	f := &fanout{e: e, in: in, work: work, t: t, log: log, query: maps.Clone(in.Query)}
	delete(f.query, "pages")
	f.ctx, f.cancel = context.WithCancel(ctx)
	defer f.cancel()

	for _, p := range parts {
		f.start(p.part, p.path)
	}
	f.wg.Wait()
	if f.err != nil {
		for _, r := range f.results {
			r.res.Cleanup()
		}
		return nil, f.err
	}
	return f.results, nil
}

// start runs a part in its own goroutine. A halving starts its halves from inside a running part,
// which the WaitGroup allows: its count is above zero until that part returns.
func (f *fanout) start(p pdfsplit.Part, path string) {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		if err := f.part(p, path); err != nil {
			f.fail(err)
		}
	}()
}

func (f *fanout) fail(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
	f.cancel()
}

// add keeps a part's result, moved into the work folder beside the part.
func (f *fanout) add(p pdfsplit.Part, res *upstream.Result) {
	dst := f.work.path(partName("result", p.Pages, ".json"))
	if err := os.Rename(res.Path, dst); err == nil {
		res.Path = dst
	}
	f.mu.Lock()
	f.results = append(f.results, partResult{part: p, res: res})
	f.mu.Unlock()
}

// part is the part loop of splitting spec §4: PartAttempts hedged attempts at this size, then
// halving; a terminal answer is re-checked against the original.
func (f *fanout) part(p pdfsplit.Part, path string) error {
	ctx, e := f.ctx, f.e
	log := f.log.With("pages", p.Label())
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("engine: stat part: %w", err)
	}
	call := upstream.Call{
		ModelID: f.in.ModelID, Prefix: f.in.Prefix, Query: f.query,
		Body:    document(path, "application/pdf", info.Size()),
		Timeout: e.partTimeout,
	}

	var last upstream.Attempt
	for n := 1; n <= PartAttempts; n++ {
		release, err := e.o.Sched.Acquire(ctx, f.in.JobID)
		if err != nil {
			return err
		}
		a := f.hedged(ctx, p.Pages, call, release)
		f.t.attempts.Add(1)
		switch a.Outcome {
		case upstream.Success:
			e.metrics.part(outcomeSucceeded)
			f.add(p, a.Result)
			return nil
		case upstream.Cancelled:
			return ctxErr(ctx)
		case upstream.RouteMissing:
			// A deployment fault: no part, and no re-check, can succeed.
			return e.attemptErr(a)
		case upstream.AnalysisFailed, upstream.Terminal:
			return f.recheck(p, a, log)
		}
		last = a
		log.Info("engine: part attempt failed", "attempt", n, "reason", a.Reason, "cause", a.Cause)
		if n < PartAttempts {
			e.metrics.retry(a.Reason)
			e.metrics.part(outcomeRetried)
			if err := sleep(ctx, e.backoff(n, a.RetryAfter)); err != nil {
				return err
			}
		}
	}

	if len(p.Pages) < 2 {
		// A single page that keeps failing fails the job, as the whole document would have.
		e.metrics.exhaust()
		return e.attemptErr(last)
	}
	return f.halve(p, path, last, log)
}

// halve cuts a part that exhausted its attempts in two, from its own file, and runs both halves.
func (f *fanout) halve(p pdfsplit.Part, path string, last upstream.Attempt, log *slog.Logger) error {
	ctx, e := f.ctx, f.e
	first, second := p.Halve()
	local := make([]int, len(p.Pages))
	for i := range local {
		local[i] = i + 1
	}
	halves := []struct {
		part  pdfsplit.Part
		local []int
	}{{first, local[:len(first.Pages)]}, {second, local[len(first.Pages):]}}

	paths := make([]string, len(halves))
	for i, h := range halves {
		paths[i] = f.work.path(partName("part", h.part.Pages, ".pdf"))
		err := writeFile(paths[i], func(w io.Writer) error {
			return e.o.Splitter.Recut(ctx, path, h.local, w)
		})
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, pdfsplit.ErrUnparseable), errors.Is(err, pdfsplit.ErrEncrypted),
			errors.Is(err, pdfsplit.ErrBudget):
			// The gateway's own part could not be cut again. The part's failure stands.
			log.Warn("engine: cannot halve part", "err", err)
			return e.attemptErr(last)
		default:
			return fmt.Errorf("engine: halve part: %w", err)
		}
	}
	f.t.halvings.Add(1)
	e.metrics.part(outcomeHalved)
	log.Info("engine: halving part", "into", first.Label()+" + "+second.Label())
	for i, h := range halves {
		f.start(h.part, paths[i])
	}
	return nil
}

// recheck answers a part's terminal outcome by asking Azure about the original: the original body
// with pages= set to the part's pages. A splitting artefact therefore never fails a good
// document, and a genuinely bad page fails the job with Azure's own answer about the original.
func (f *fanout) recheck(p pdfsplit.Part, failed upstream.Attempt, log *slog.Logger) error {
	e := f.e
	e.metrics.part(outcomeRechecked)
	log.Info("engine: re-checking part against the original", "reason", failed.Reason)
	q := maps.Clone(f.query)
	if q == nil {
		q = url.Values{}
	}
	q["pages"] = []string{pdfsplit.FormatPages(p.Pages)}
	res, err := e.whole(f.ctx, f.in.JobID, e.wholeCall(f.in, q), f.t, log.With("recheck", true))
	if err != nil {
		return err
	}
	f.add(p, res)
	return nil
}

// merge writes the parts' merged analyzeResult to the work folder.
func (e *Engine) merge(in Input, work *workdir, pages []int, results []partResult,
	log *slog.Logger) (*Output, error) {
	slices.SortFunc(results, func(a, b partResult) int { return cmp.Compare(a.part.Pages[0], b.part.Pages[0]) })
	var covered []int
	parts := make([]merge.Part, len(results))
	for i, r := range results {
		covered = append(covered, r.part.Pages...)
		parts[i] = merge.Part{Path: r.res.Path, Start: r.res.Start, End: r.res.End, Pages: r.part.Pages}
	}
	if !slices.Equal(covered, pages) {
		// merge cannot see a part that is missing altogether; this would be the engine's bug.
		log.Error("engine: parts do not cover the selected pages", "covered", pdfsplit.FormatPages(covered),
			"selected", pdfsplit.FormatPages(pages))
		return nil, errMergeCheck
	}

	var unit textunit.Unit
	if e.o.Client.Surface() == azerr.SurfaceDI {
		u, ok := textunit.Parse(in.Query.Get("stringIndexType"))
		if !ok {
			return nil, fmt.Errorf("%w: unknown stringIndexType", errMergeCheck)
		}
		unit = u
	}

	path := work.path("merged.json")
	started := e.now()
	err := writeFile(path, func(w io.Writer) error {
		var err error
		if e.o.Client.Surface() == azerr.SurfaceRead {
			_, err = merge.Read(w, parts)
		} else {
			_, err = merge.DI(w, parts, unit)
		}
		return err
	})
	if errors.Is(err, merge.ErrCheck) {
		return nil, fmt.Errorf("%w: %w", errMergeCheck, err)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: merge: %w", err)
	}
	e.metrics.mergeSeconds(e.now().Sub(started))
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("engine: stat merged result: %w", err)
	}
	return &Output{Path: path, End: info.Size(), Bare: results[0].res.Bare, Parts: len(results)}, nil
}
