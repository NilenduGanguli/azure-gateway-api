package engine

import (
	"context"
	"log/slog"
	"net/url"

	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// runWhole analyses the document as the client sent it: one body, the client's query unchanged.
func (e *Engine) runWhole(ctx context.Context, in Input, t *tally, fallback string,
	log *slog.Logger) (*Output, error) {
	res, err := e.whole(ctx, in.JobID, e.wholeCall(in, in.Query), t, log)
	if err != nil {
		return nil, err
	}
	t.upstreamMS.Add(res.UpstreamMS)
	return &Output{
		Path: res.Path, Start: res.Start, End: res.End, Bare: res.Bare,
		Parts: 1, Fallback: fallback, UpstreamMS: res.UpstreamMS,
		cleanup: res.Cleanup,
	}, nil
}

// wholeCall is a call of the original body with query.
func (e *Engine) wholeCall(in Input, query url.Values) upstream.Call {
	return upstream.Call{
		ModelID: in.ModelID, Prefix: in.Prefix, Query: query,
		Body:    document(in.Path, in.ContentType, in.Size),
		Timeout: e.o.WholeTimeout,
	}
}

// whole runs call under the sync-only spec's attempt rules (§1): up to WholeAttempts attempts,
// at most retryOnceLimit of them in the RetryOnce class, with Backoff between them.
//
// It serves a document sent whole and a part's re-check alike: both send the original document,
// so both get the long timeout and the whole-document rules. The error is an *azerr.APIError, or
// the context's.
func (e *Engine) whole(ctx context.Context, job string, call upstream.Call, t *tally,
	log *slog.Logger) (*upstream.Result, error) {
	retryOnce := 0
	for n := 1; ; n++ {
		a, err := e.call(ctx, job, call)
		if err != nil {
			return nil, err
		}
		t.attempt(a)
		switch a.Outcome {
		case upstream.Success:
			return a.Result, nil
		case upstream.Cancelled:
			return nil, ctxErr(ctx)
		case upstream.RetryOnce:
			retryOnce++
		case upstream.Retry:
		default: // AnalysisFailed, RouteMissing, Terminal: no retry changes the answer
			return nil, e.attemptErr(a)
		}
		if n >= WholeAttempts || retryOnce >= retryOnceLimit {
			e.metrics.exhaust()
			log.Warn("engine: attempts exhausted", "attempts", n, "reason", a.Reason, "status", a.Status,
				"cause", a.Cause)
			// The last attempt's error goes to the client as the container sent it: the service
			// never says how often it was asked. The count is in this log line, the job's run
			// output and /_gw/jobs (upstreamAttempts).
			return nil, e.attemptErr(a)
		}
		e.metrics.retry(a.Reason)
		wait := e.backoff(n, a.RetryAfter)
		log.Warn("engine: retrying", "attempt", n, "reason", a.Reason, "status", a.Status,
			"backoffMs", wait.Milliseconds(), "cause", a.Cause)
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}
