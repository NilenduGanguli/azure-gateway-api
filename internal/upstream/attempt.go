package upstream

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
)

// Call is one synchronous analyze attempt's input.
type Call struct {
	// ModelID is the Document Intelligence model, e.g. prebuilt-layout. Unused by Read.
	ModelID string
	// Prefix is the Document Intelligence path family, /documentintelligence or /formrecognizer.
	// A client is answered on the family it called, so the container is called on it too. Empty
	// means /documentintelligence. Unused by Read.
	Prefix string
	// Query is forwarded as it is. The caller has already applied the parameter policy.
	Query url.Values
	// Body is the stored document. It is opened afresh for every attempt, so a retry re-reads it
	// from the PVC rather than from memory.
	Body Document
	// Timeout is the attempt's own deadline, covering the answer and its download. Zero means
	// the surface's configured upstream timeout.
	Timeout time.Duration
}

// Outcome is the class of one attempt: a row of the outcome table in the sync-only spec, §1.
type Outcome int

const (
	// Success is a 200 carrying an analyzeResult (row 2).
	Success Outcome = iota
	// Cancelled means the caller's context ended (row 1). It is checked before every other row,
	// so a shutdown is never mistaken for a container fault: the job stays running and recovery
	// re-runs it. Attempt.Err is nil; the caller's ctx.Err() is the error.
	Cancelled
	// AnalysisFailed is a 200 whose status is failed: the container analysed the document and
	// could not (row 3).
	AnalysisFailed
	// RouteMissing means the container does not serve the synchronous route at all, a deployment
	// fault no retry fixes (row 4).
	RouteMissing
	// RetryOnce is the attempt's own deadline, a 500 or a 504 (row 5). A slow document times out
	// again on every pod, so a job retries this class only once.
	RetryOnce
	// Retry is a transient failure: a connection that failed or broke, a 408, 429, 502 or 503,
	// or a 202 from a degraded container (rows 6–8).
	Retry
	// Terminal is any other failure: a 4xx about this request, or an answer that is not a result
	// (rows 9–10).
	Terminal
)

func (o Outcome) String() string {
	switch o {
	case Success:
		return "success"
	case Cancelled:
		return "cancelled"
	case AnalysisFailed:
		return "analysis_failed"
	case RouteMissing:
		return "route_missing"
	case RetryOnce:
		return "retry_once"
	case Retry:
		return "retry"
	case Terminal:
		return "terminal"
	}
	return "unknown"
}

// Attempt is everything the caller needs to decide what to do next.
type Attempt struct {
	Outcome Outcome
	// Result is set on Success, and the caller owns it: it must call Result.Cleanup.
	Result *Result
	// Err is set on every failure but Cancelled, shaped for the surface, so it can be relayed to
	// a client or stored on a failed job as it is.
	Err *azerr.APIError
	// Reason labels the attempt for the retry metric and the attempt log: one of the Reason
	// constants, or status_<code> for a failing status.
	Reason string
	// Status is the container's HTTP status, 0 when no complete answer arrived.
	Status int
	// RetryAfter is the container's integer Retry-After, set only on the retriable statuses and
	// 0 when absent. Backoff takes it.
	RetryAfter time.Duration
	// Took is the attempt's wall time, including the download.
	Took time.Duration
	// Cause is the underlying transport or gateway error, for the log only. It can name internal
	// addresses and PVC paths, so it is never shown to a client; Err is.
	Cause error
}

// Do performs one attempt: one POST of call.Body to the surface's synchronous route on a fresh
// connection, classified by the sync-only spec's outcome table.
//
// The attempt runs under a deadline of its own, derived from ctx. Classification checks ctx
// first and the attempt's deadline second, so a shutdown is Cancelled even when the transport
// reports it as a timeout, and a slow analysis is RetryOnce even though its connection also
// failed.
//
// Only a Success leaves a file behind, in Result. Every other outcome removes what it wrote.
func (c *Client) Do(ctx context.Context, call Call) Attempt {
	started := time.Now()
	a := c.attempt(ctx, call, started)
	// Row 1 before every other row, whatever the wire said. A 503 or even a whole result that
	// lands as the caller leaves is still a cancellation: during shutdown the job must stay
	// running for recovery, and a client that hung up wants nothing.
	if err := ctx.Err(); err != nil && a.Outcome != Cancelled {
		a.Result.Cleanup()
		a = cancelled(err, a.Status)
	}
	a.Took = time.Since(started)
	return a
}

// attempt sends the call and classifies the answer, all but row 1's final check.
func (c *Client) attempt(ctx context.Context, call Call, started time.Time) Attempt {
	path, apiErr := c.route(call)
	if apiErr != nil {
		return Attempt{Outcome: Terminal, Reason: ReasonGateway, Err: apiErr}
	}

	timeout := call.Timeout
	if timeout <= 0 {
		timeout = c.b.timeout
	}
	var actx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		actx, cancel = context.WithCancel(ctx)
	}
	// Released only once the answer has been read to the end: classify drains every body before
	// it returns, and cancelling first would cut a body still in flight.
	defer cancel()

	req, err := c.b.newRequest(actx, http.MethodPost, c.b.joinURL(path, call.Query), &call.Body)
	if err != nil {
		return c.gatewayFailure(err, 0)
	}
	tr := newAttemptTransport()
	defer tr.CloseIdleConnections()
	// No cookie jar: the OpenShift router's sticky-session cookie would pin every retry to one pod.
	hc := &http.Client{Transport: tr, CheckRedirect: noRedirects}

	resp, err := hc.Do(req)
	if err != nil {
		return c.transportFailure(ctx, actx, err)
	}
	return c.classify(ctx, actx, resp, started)
}

// route is the path of the surface's synchronous analyze route for call.
func (c *Client) route(call Call) (string, *azerr.APIError) {
	if c.b.surface == azerr.SurfaceRead {
		return ReadPathPrefix + "/syncAnalyze", nil
	}
	if call.ModelID == "" {
		return "", azerr.BadRequest(azerr.SurfaceDI, "A model id is required.")
	}
	return prefix(call.Prefix) + "/documentModels/" + escapeSegment(call.ModelID) + ":syncAnalyze", nil
}
