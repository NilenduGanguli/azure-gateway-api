package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
)

// Reasons label an attempt for the retry metric and the attempt log. A failing status is labelled
// status_<code> instead; see statusReason.
const (
	// ReasonConnection is a connection that could not be made or broke, including a body cut
	// short (row 6).
	ReasonConnection = "connection"
	// ReasonTimeout is the attempt's own deadline (row 5).
	ReasonTimeout = "timeout"
	// ReasonDegraded202 is a container that answered the synchronous route with 202 (row 8).
	ReasonDegraded202 = "degraded_202"
	// ReasonCancelled is the caller's context ending (row 1).
	ReasonCancelled = "cancelled"
	// ReasonAnalysisFailed is a 200 whose status is failed (row 3).
	ReasonAnalysisFailed = "analysis_failed"
	// ReasonNoResult is a 200 that is not an analysis result (row 10).
	ReasonNoResult = "no_result"
	// ReasonTooLarge is a result over the configured ceiling (row 10).
	ReasonTooLarge = "too_large"
	// ReasonGateway is the gateway's own failure around the call: the stored input could not be
	// read, a scratch file could not be written, or the call named no model (row 10).
	ReasonGateway = "gateway"
)

// statusReason labels an attempt that ended in a failing status.
func statusReason(status int) string { return "status_" + strconv.Itoa(status) }

// maxRetryAfter bounds a parsed Retry-After before it becomes a Duration, which a value of a few
// hundred years would overflow. Backoff caps the wait far below this anyway.
const maxRetryAfter = 24 * time.Hour

// classify reads a container's answer to the end and places it in the outcome table. Row 1 is
// the caller's to check last (see Do); this covers the answers themselves, in table order.
func (c *Client) classify(ctx, actx context.Context, resp *http.Response, started time.Time) Attempt {
	status := resp.StatusCode
	switch status {
	case http.StatusOK:
		return c.readResult(ctx, actx, resp, started)
	case http.StatusAccepted:
		// The container degraded to an asynchronous operation. Its operation lives on the pod that
		// answered, which an autoscaler may remove at any moment, so it is abandoned and the
		// document sent again. Its Retry-After paces polls of that operation, not a new attempt,
		// so it is not kept.
		drain(resp)
		return Attempt{
			Outcome: Retry, Reason: ReasonDegraded202, Status: status,
			Err: azerr.Unavailable(c.b.surface, fmt.Sprintf(
				"The %s container degraded to an asynchronous operation, which this gateway "+
					"does not follow.", c.b.name), 0),
		}
	}

	retryAfter := retryAfterDuration(resp.Header.Get("Retry-After"))
	a := Attempt{Status: status, Reason: statusReason(status), Err: c.b.readErrorBody(resp)}
	switch {
	case routeMissing(status, a.Err):
		a.Outcome = RouteMissing
		a.Err = azerr.Internal(c.b.surface, fmt.Sprintf(
			"The %s container does not serve the synchronous analyze route.", c.b.name))
	case status == http.StatusInternalServerError || status == http.StatusGatewayTimeout:
		a.Outcome, a.RetryAfter = RetryOnce, retryAfter
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable:
		a.Outcome, a.RetryAfter = Retry, retryAfter
	default:
		a.Outcome = Terminal
		if status < http.StatusBadRequest {
			// A redirect, which is never followed, or a 2xx no row names. readErrorBody kept the
			// status, and an error rendered at 302 or 204 is no error at all.
			a.Err = azerr.Internal(c.b.surface, fmt.Sprintf(
				"The %s container answered HTTP %d, which is not an analysis result.", c.b.name, status))
		}
	}
	return a
}

// routeMissing reports whether a failing answer means the container has no synchronous route
// (row 4), as opposed to an answer about this request.
//
// Kestrel answers an unrouted path with a bodyless 404, or 405 for a known path with the wrong
// verb; a container that serves the route but not the model answers a Document Intelligence error
// naming it. So a 404, 405 or 501 means a missing route when its body is not an Azure error — the
// gateway had to synthesise one — or when it is one whose text says no endpoint matched. Older
// image builds answer the route with 500 UnhandledEndpointException instead.
func routeMissing(status int, e *azerr.APIError) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return e.Synthesised() || looksLikeMissingEndpoint(e)
	case http.StatusInternalServerError:
		return looksLikeMissingEndpoint(e)
	}
	return false
}

// readResult downloads a 200 and decides between rows 2, 3, 6 and 10.
func (c *Client) readResult(ctx, actx context.Context, resp *http.Response, started time.Time) Attempt {
	body := &bodyReadError{r: resp.Body}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{body, resp.Body}

	path, size, err := c.b.downloadBody(resp, c.b.name)
	drain(resp)
	switch {
	case err == nil:
	case body.err != nil:
		// The answer was cut short: the pod was killed mid-body, as scale-in does, or the
		// attempt's deadline or the caller's context ended during the download.
		return c.transportFailure(ctx, actx, body.err)
	case errors.Is(err, ErrResultTooLarge):
		return Attempt{
			Outcome: Terminal, Reason: ReasonTooLarge, Status: resp.StatusCode, Cause: err,
			Err: c.ResultTooLarge(),
		}
	default:
		return c.gatewayFailure(err, resp.StatusCode)
	}

	view, err := inspectOperation(path, size, c.b.surface)
	switch {
	case err != nil:
		_ = os.Remove(path)
		return c.noResult(err)
	case isFailed(view.Status):
		// Before the analyzeResult check, which row order alone would put first: Read reports
		// why an analysis failed inside analyzeResult.errors, so a failed body can carry one. A
		// body that says failed is never a success.
		_ = os.Remove(path)
		return Attempt{
			Outcome: AnalysisFailed, Reason: ReasonAnalysisFailed, Status: resp.StatusCode,
			Err: view.Err,
		}
	case view.HasResult:
		return Attempt{Outcome: Success, Status: resp.StatusCode, Result: &Result{
			Path: path, Start: view.Start, End: view.End, Bare: view.Bare,
			UpstreamMS:     time.Since(started).Milliseconds(),
			UpstreamStatus: resp.StatusCode,
		}}
	default:
		_ = os.Remove(path)
		return c.noResult(nil)
	}
}

// transportFailure classifies a call that got no complete answer: rows 1, 5 and 6, in that order.
//
// The order is the point. Cancelling the caller's context and expiring the attempt's deadline
// both surface from the transport as the same kind of error, and a slow analysis can also look
// like a broken connection. Only the contexts say which it was.
func (c *Client) transportFailure(ctx, actx context.Context, err error) Attempt {
	switch {
	case ctx.Err() != nil:
		return cancelled(ctx.Err(), 0)
	case actx.Err() != nil:
		return Attempt{
			Outcome: RetryOnce, Reason: ReasonTimeout, Cause: transportCause(err),
			Err: azerr.Internal(c.b.surface, fmt.Sprintf(
				"The %s container did not complete the analysis within the configured timeout.",
				c.b.name)),
		}
	default:
		// The transport's own text names the upstream address; it goes to the log, not here.
		return Attempt{
			Outcome: Retry, Reason: ReasonConnection, Cause: transportCause(err),
			Err: azerr.Internal(c.b.surface, fmt.Sprintf("The %s container is unreachable.", c.b.name)),
		}
	}
}

// cancelled is row 1. It carries no APIError, so nothing about it can be relayed to a client or
// stored as a job failure.
func cancelled(cause error, status int) Attempt {
	return Attempt{Outcome: Cancelled, Reason: ReasonCancelled, Status: status, Cause: cause}
}

// gatewayFailure is the gateway failing around the call. The raw error names PVC paths, so the
// client is told nothing more than that something went wrong.
func (c *Client) gatewayFailure(err error, status int) Attempt {
	return Attempt{
		Outcome: Terminal, Reason: ReasonGateway, Status: status, Cause: err,
		Err: azerr.Internal(c.b.surface, ""),
	}
}

// noResult is a 200 that is neither a result nor a failure (row 10).
func (c *Client) noResult(cause error) Attempt {
	return Attempt{
		Outcome: Terminal, Reason: ReasonNoResult, Status: http.StatusOK, Cause: cause,
		Err: azerr.Internal(c.b.surface, fmt.Sprintf(
			"The %s container returned a synchronous response with no analyzeResult.", c.b.name)),
	}
}

// bodyReadError remembers the first error reading a response body. downloadBody reports a failed
// read and a failed scratch-file write alike, and the two are different rows: a body cut short is
// a transport failure and retried, a write that failed is the gateway's own.
type bodyReadError struct {
	r   io.Reader
	err error
}

func (b *bodyReadError) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && b.err == nil {
		b.err = err
	}
	return n, err
}

// retryAfterDuration parses an integer-seconds Retry-After into a Duration, 0 when absent or
// unparseable.
func retryAfterDuration(v string) time.Duration {
	n := retryAfterSeconds(v)
	if n > int(maxRetryAfter/time.Second) {
		return maxRetryAfter
	}
	return time.Duration(n) * time.Second
}

// looksLikeMissingEndpoint reports whether a 500 is really "this route does not exist".
func looksLikeMissingEndpoint(e *azerr.APIError) bool {
	if e == nil {
		return false
	}
	hay := strings.ToLower(e.Code + " " + e.Message + " " + e.InnerCode + " " + e.InnerMsg)
	for _, needle := range []string{
		"unhandledendpointexception",
		"no candidates found for the request path",
		"request did not match any endpoints",
	} {
		if strings.Contains(hay, needle) {
			return true
		}
	}
	return false
}

// transportCause reduces a transport failure to its underlying cause.
//
// net/http wraps every request failure in a *url.Error whose message embeds the entire request
// URL. That message is relayed to the client in the poll timeout, which handed any caller who
// waited out a container outage the gateway's internal upstream address — host, port, path, and
// the userinfo username when the upstream is configured with credentials in the URL. Only the
// cause ("connection refused", "context deadline exceeded") is useful to a client, and it is the
// part that carries no topology.
func transportCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
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
