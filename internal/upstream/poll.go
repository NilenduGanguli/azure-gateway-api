package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
)

// jsonUnmarshal is a thin alias so the shared helpers read the same everywhere.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// ErrResultTooLarge reports an upstream response beyond the configured ceiling.
var ErrResultTooLarge = errors.New("upstream: response exceeds the configured maximum")

// downloadBody streams a response body to a scratch file, bounded by maxBytes.
//
// The body is never held in memory: Azure permits a 500 MB OCR JSON response, and buffering that
// per in-flight job would exhaust a pod running several workers.
func (b *base) downloadBody(resp *http.Response, prefix string) (path string, size int64, err error) {
	f, err := b.tempFile(tempPrefix(prefix))
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	limit := b.maxBytes
	if limit <= 0 {
		limit = 512 << 20
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		_ = os.Remove(f.Name())
		return "", 0, fmt.Errorf("upstream: read response body: %w", err)
	}
	if n > limit {
		_ = os.Remove(f.Name())
		return "", 0, ErrResultTooLarge
	}
	if err := f.Sync(); err != nil {
		_ = os.Remove(f.Name())
		return "", 0, fmt.Errorf("upstream: sync response body: %w", err)
	}
	return f.Name(), n, nil
}

// operationView is what the gateway needs from an upstream operation envelope, without loading
// the envelope itself.
type operationView struct {
	Status string
	// Start and End delimit the analyzeResult member within the file, when present.
	Start, End int64
	HasResult  bool
	Err        *azerr.APIError
}

// inspectOperation reads an upstream response file and reports its operation status, the byte
// range of its analyzeResult, and any embedded error.
//
// The response may be either an operation envelope — {"status":…,"analyzeResult":{…}} — or, on
// the undocumented synchronous routes, a bare AnalyzeResult with no envelope at all. Both are
// handled: an object with no status member is treated as a bare result.
func inspectOperation(path string, size int64, surface azerr.Surface) (*operationView, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("upstream: reopen response: %w", err)
	}
	defer func() { _ = f.Close() }()

	view := &operationView{}

	statusStart, statusEnd, found, err := jsonx.ValueRange(f, size, "status")
	if err != nil {
		return nil, fmt.Errorf("upstream: scan response: %w", err)
	}
	if found {
		// Bounded like every other member read here. A status is a short enum string; an upstream
		// that sent a multi-megabyte one would otherwise be allocated in full.
		const maxStatusBytes = 1 << 10
		if statusEnd-statusStart > maxStatusBytes {
			return nil, fmt.Errorf("upstream: status member is %d bytes, which is not a status",
				statusEnd-statusStart)
		}
		raw := make([]byte, statusEnd-statusStart)
		if _, err := f.ReadAt(raw, statusStart); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("upstream: read status: %w", err)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			s = strings.Trim(string(raw), `" `)
		}
		view.Status = s
	}

	resStart, resEnd, hasResult, err := jsonx.ValueRange(f, size, "analyzeResult")
	if err != nil {
		return nil, fmt.Errorf("upstream: scan analyzeResult: %w", err)
	}
	if hasResult {
		view.Start, view.End, view.HasResult = resStart, resEnd, true
	} else if view.Status == "" {
		// No status and no analyzeResult member: the whole document is the result. This is the
		// bare-AnalyzeResult shape the undocumented synchronous routes may return.
		lead := make([]byte, min64(64, size))
		if _, err := f.ReadAt(lead, 0); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("upstream: read response lead: %w", err)
		}
		if jsonx.IsObject(lead) {
			view.Start, view.End, view.HasResult = 0, size, true
			view.Status = string(statusSucceeded)
		}
	}

	// A failed operation carries its detail in an error member on the DI surface. The Read
	// envelope has no error member in its schema at all, so a failed Read yields only the status.
	if isFailed(view.Status) {
		errStart, errEnd, hasErr, err := jsonx.ValueRange(f, size, "error")
		if err == nil && hasErr && errEnd-errStart <= maxErrorBody {
			raw := make([]byte, errEnd-errStart)
			if _, rerr := f.ReadAt(raw, errStart); rerr == nil || errors.Is(rerr, io.EOF) {
				var e azerr.Error
				if json.Unmarshal(raw, &e) == nil && e.Code != "" {
					view.Err = &azerr.APIError{
						Status:  http.StatusInternalServerError,
						Code:    e.Code,
						Message: e.Message,
						Target:  e.Target,
						Details: e.Details,
					}
					if e.InnerError != nil {
						view.Err.InnerCode = e.InnerError.Code
						view.Err.InnerMsg = e.InnerError.Message
					}
				}
			}
		}
		// Computer Vision Read puts its failure detail inside analyzeResult.errors rather than in a
		// top-level error object — its schema models no error member for a failed operation at all.
		// A probed container returns exactly that, so the detail is lifted out here instead of
		// being discarded in favour of a generic message.
		if view.Err == nil && view.HasResult {
			if e := firstResultError(f, view.Start, view.End-view.Start); e != nil {
				view.Err = e
			}
		}
		if view.Err == nil {
			view.Err = azerr.Internal(surface,
				"The upstream container reported the analysis as failed.")
		}
	}
	return view, nil
}

// Operation status strings as the containers emit them. Comparison is case-insensitive because
// the same container emits lowercase "failed" on its async routes and capital-F "Failed" from
// CV syncAnalyze.
type statusString string

const (
	statusNotStarted statusString = "notStarted"
	statusRunning    statusString = "running"
	statusSucceeded  statusString = "succeeded"
	statusFailed     statusString = "failed"
)

func isFailed(s string) bool    { return strings.EqualFold(s, string(statusFailed)) }
func isSucceeded(s string) bool { return strings.EqualFold(s, string(statusSucceeded)) }
func isPending(s string) bool {
	return strings.EqualFold(s, string(statusRunning)) ||
		strings.EqualFold(s, string(statusNotStarted))
}

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
			lastTransientErr = err
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
			continue

		default:
			cancelReq()
			return nil, b.readErrorBody(resp)
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

// retryAfterSeconds parses an integer-seconds Retry-After. HTTP-date values are ignored rather
// than approximated.
func retryAfterSeconds(v string) int {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
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

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// firstResultError lifts the first entry of analyzeResult.errors, which is where Computer Vision
// Read reports why an analysis failed.
func firstResultError(ra io.ReaderAt, start, size int64) *azerr.APIError {
	section := io.NewSectionReader(ra, start, size)
	errStart, errEnd, found, err := jsonx.ValueRange(section, size, "errors")
	if err != nil || !found {
		return nil
	}
	const maxErrorsBytes = 1 << 20
	if errEnd-errStart > maxErrorsBytes {
		return nil
	}
	raw := make([]byte, errEnd-errStart)
	if _, rerr := section.ReadAt(raw, errStart); rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil
	}
	var list []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &list) != nil || len(list) == 0 || list[0].Code == "" {
		return nil
	}
	return &azerr.APIError{
		Status:  http.StatusInternalServerError,
		Code:    list[0].Code,
		Message: list[0].Message,
	}
}
