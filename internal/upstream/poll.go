package upstream

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
)

// pollConfig parameterises the affinity poll loop.
type pollConfig struct {
	// URL is the poll target, always rebuilt against the configured upstream base rather than
	// taken from the container's own Operation-Location, which may name an unroutable internal
	// host.
	URL string
	// BlindBudget caps consecutive 404s tolerated before the job is failed. A 404 means the poll
	// reached a replica that never saw this operation, because the containers' default result
	// store is instance-local. Retrying can still land on the owning replica.
	BlindBudget int
	Deadline    time.Time
	Prefix      string
}

// pollUntilTerminal polls an upstream operation until it succeeds, fails, or the deadline passes.
//
// The client passed in carries the per-job connection pool and cookie jar that keep the polls on
// the replica that owns the operation.
func (b *base) pollUntilTerminal(ctx context.Context, ac *affinityClient, cfg pollConfig) (*Result, error) {
	const (
		minBackoff = 500 * time.Millisecond
		maxBackoff = 5 * time.Second
	)
	backoff := minBackoff
	misses := 0
	transient := 0
	var lastTransientErr error
	started := time.Now()

	for {
		if time.Now().After(cfg.Deadline) {
			if transient > 0 {
				return nil, azerr.Internal(b.surface, fmt.Sprintf(
					"The %s container was unreachable for the remainder of the configured timeout "+
						"(%d consecutive failures, last: %v).", b.name, transient, lastTransientErr))
			}
			return nil, azerr.Internal(b.surface, fmt.Sprintf(
				"The %s container did not complete the analysis within the configured timeout.", b.name))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff, maxBackoff)

		// Bound each request by whatever is left of the job's deadline. Without this the deadline
		// is only a loop guard: a single hung poll runs to the client's own timeout and holds an
		// admission slot for a full upstream timeout past the point the job should have failed.
		reqCtx, cancelReq := context.WithDeadline(ctx, cfg.Deadline)
		req, err := b.newRequest(reqCtx, http.MethodGet, cfg.URL, nil)
		if err != nil {
			cancelReq()
			return nil, err
		}
		resp, err := ac.http.Do(req)
		if err != nil {
			cancelReq()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			// Keep retrying until the job's own deadline. Capping transport errors at a fixed
			// count abandoned a live operation after well under a minute of container churn —
			// which is exactly the pod rescheduling this gateway exists to ride out — no matter
			// how much of the configured timeout was left.
			transient++
			lastTransientErr = transportCause(err)
			// Not a 404, so the run of them is broken. The budget counts CONSECUTIVE misses —
			// evidence that this replica genuinely never saw the operation — and letting a
			// transport blip carry the count forward turned unrelated churn into a wrong verdict.
			misses = 0
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			transient = 0
			lastTransientErr = nil
			path, size, derr := b.downloadBody(resp, cfg.Prefix)
			drain(resp)
			cancelReq()
			if derr != nil {
				return nil, derr
			}
			view, verr := inspectOperation(path, size, b.surface)
			if verr != nil {
				_ = os.Remove(path)
				return nil, verr
			}
			switch {
			case isSucceeded(view.Status) && view.HasResult:
				return &Result{
					Path:           path,
					Start:          view.Start,
					End:            view.End,
					UpstreamMS:     time.Since(started).Milliseconds(),
					UpstreamStatus: http.StatusOK,
				}, nil
			case isFailed(view.Status):
				_ = os.Remove(path)
				return nil, view.Err
			case isPending(view.Status):
				_ = os.Remove(path)
				// Honour the container's own pacing hint when it offers one.
				if ra := retryAfterSeconds(resp.Header.Get("Retry-After")); ra > 0 {
					backoff = time.Duration(ra) * time.Second
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				misses = 0
				continue
			default:
				_ = os.Remove(path)
				return nil, azerr.Internal(b.surface, fmt.Sprintf(
					"The %s container reported an unrecognised operation status %q.", b.name, view.Status))
			}

		case resp.StatusCode == http.StatusNotFound:
			drain(resp)
			cancelReq()
			// The poll reached a replica that does not know this operation. Router affinity has
			// been lost — the pod was rescheduled, or the cookie was not honoured — so keep
			// polling in the hope of landing on the owner, bounded by the budget.
			misses++
			if misses > cfg.BlindBudget {
				return nil, azerr.Internal(b.surface, fmt.Sprintf(
					"The %s container lost the operation: %d consecutive polls reached a replica "+
						"that had no record of it. The container's result store is instance-local, "+
						"so the owning replica is gone.", b.name, misses))
			}
			continue

		case resp.StatusCode >= 500:
			drain(resp)
			cancelReq()
			transient++
			lastTransientErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			misses = 0
			continue

		default:
			// Read the body before releasing the request context. Cancelling first aborts the
			// still-in-flight body, so a chunked or slow error response arrives truncated,
			// ParseUpstream fails on it, and the container's own diagnosis is replaced by a
			// generic message — losing exactly the detail this path exists to preserve.
			apiErr := b.readErrorBody(resp)
			cancelReq()
			return nil, apiErr
		}
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

// operationIDFrom extracts the trailing operation identifier from a container's
// Operation-Location header.
//
// Only the identifier is kept. The rest of the header is discarded because the container mints it
// from the inbound Host and Referer, so it can name an internal address the gateway cannot reach,
// and because the Read container is known to corrupt it when the request authority contains the
// substring "vision".
func operationIDFrom(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	s := header
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	i := strings.LastIndex(s, "/")
	if i < 0 || i == len(s)-1 {
		return "", false
	}
	id := s[i+1:]
	if id == "" {
		return "", false
	}
	return id, true
}
