package upstream

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

// ReadPathPrefix is the path the Computer Vision Read surface is served under.
const ReadPathPrefix = "/vision/v3.2/read"

// ReadClient talks to the Computer Vision Read v3.2 container.
//
// Read is the straightforward surface: syncAnalyze is documented, container-only, and genuinely
// stateless, so it load-balances correctly across replicas with no affinity and no shared store.
// It is the workaround Microsoft's own field guidance points at for the multi-replica problem.
//
// The one operational catch is time, not correctness: a synchronous call holds the connection for
// the whole analysis, and an OpenShift Route's default HAProxy timeout is 30 seconds. Large
// documents need haproxy.router.openshift.io/timeout raised to match READ_SYNC_TIMEOUT.
type ReadClient struct {
	*base
	blindBudget int
}

// NewRead builds a Computer Vision Read client.
func NewRead(up config.Upstream, blindBudget int, tempDir string, maxBytes int64) *ReadClient {
	return &ReadClient{
		base: newBase("computer-vision-read", up.BaseURL, up.APIKey, up.Timeout, tempDir,
			azerr.SurfaceRead, up.MaxInflight+2, maxBytes),
		blindBudget: blindBudget,
	}
}

// Surface identifies the error vocabulary this client speaks.
func (c *ReadClient) Surface() azerr.Surface { return azerr.SurfaceRead }

// Health reports container reachability. The synchronous route is documented for this container,
// so it is reported as available rather than probed.
func (c *ReadClient) Health(ctx context.Context) Health {
	h := c.checkStatus(ctx)
	h.SyncAnalyze = "available"
	return h
}

// Analyze reads one document synchronously.
//
// A 202 is not expected here — no field report of Read degrading exists — but the same engine and
// storage path back both containers, so the degraded case is handled rather than assumed away.
func (c *ReadClient) Analyze(ctx context.Context, doc Document, req Request) (*Result, error) {
	deadline := time.Now().Add(c.timeout)

	ac, err := newAffinityClient(c.timeout)
	if err != nil {
		return nil, err
	}
	defer ac.close()

	target := c.joinURL(ReadPathPrefix+"/syncAnalyze", cloneQuery(req.Query))
	started := time.Now()

	hreq, err := c.newRequest(ctx, http.MethodPost, target, &doc)
	if err != nil {
		return nil, err
	}
	resp, err := ac.http.Do(hreq)
	if err != nil {
		return nil, c.unreachable(ctx, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		path, size, derr := c.downloadBody(resp, "read-sync")
		drain(resp)
		if derr != nil {
			return nil, derr
		}
		view, verr := inspectOperation(path, size, azerr.SurfaceRead)
		if verr != nil {
			_ = os.Remove(path)
			return nil, verr
		}
		// syncAnalyze signals failure with a bare {"status":"Failed"} — capital F, no code, no
		// message. inspectOperation compares case-insensitively and synthesises a well-formed
		// error, because that body is unactionable as-is and must never reach a client.
		if isFailed(view.Status) {
			_ = os.Remove(path)
			return nil, view.Err
		}
		if !view.HasResult {
			_ = os.Remove(path)
			return nil, azerr.Internal(azerr.SurfaceRead,
				"The computer-vision-read container returned a synchronous response with no analyzeResult.")
		}
		return &Result{
			Path: path, Start: view.Start, End: view.End,
			Mode:           store.ModeSync,
			UpstreamMS:     time.Since(started).Milliseconds(),
			UpstreamStatus: http.StatusOK,
		}, nil

	case http.StatusAccepted:
		loc := resp.Header.Get("Operation-Location")
		drain(resp)
		id, ok := operationIDFrom(loc)
		if !ok {
			return nil, azerr.Internal(azerr.SurfaceRead,
				"The computer-vision-read container degraded to an asynchronous operation but "+
					"returned no usable Operation-Location.")
		}
		res, err := c.pollUntilTerminal(ctx, ac, pollConfig{
			URL:         c.pollURL(id),
			BlindBudget: c.blindBudget,
			Deadline:    deadline,
			Prefix:      "read-poll",
		})
		if err != nil {
			return nil, err
		}
		res.Mode = store.ModeDegraded
		res.UpstreamMS = time.Since(started).Milliseconds()
		res.UpstreamOpID = id
		return res, nil

	case http.StatusNotFound, http.StatusMethodNotAllowed:
		// The container serves no syncAnalyze. Documented for every 3.2 build, so this means a
		// misconfigured upstream rather than an image difference; fall back so a job still
		// completes, and let /_gw/health show the surprise.
		drain(resp)
		return c.analyzeAsync(ctx, ac, doc, req, deadline)

	default:
		return nil, c.readErrorBody(resp)
	}
}

// analyzeAsync submits to /read/analyze and polls, used only when syncAnalyze is missing.
func (c *ReadClient) analyzeAsync(ctx context.Context, ac *affinityClient, doc Document,
	req Request, deadline time.Time) (*Result, error) {

	target := c.joinURL(ReadPathPrefix+"/analyze", cloneQuery(req.Query))
	started := time.Now()

	hreq, err := c.newRequest(ctx, http.MethodPost, target, &doc)
	if err != nil {
		return nil, err
	}
	resp, err := ac.http.Do(hreq)
	if err != nil {
		return nil, c.unreachable(ctx, err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil, c.readErrorBody(resp)
	}
	loc := resp.Header.Get("Operation-Location")
	drain(resp)

	id, ok := operationIDFrom(loc)
	if !ok {
		return nil, azerr.Internal(azerr.SurfaceRead,
			"The computer-vision-read container accepted the request but returned no usable "+
				"Operation-Location.")
	}
	res, err := c.pollUntilTerminal(ctx, ac, pollConfig{
		URL:         c.pollURL(id),
		BlindBudget: c.blindBudget,
		Deadline:    deadline,
		Prefix:      "read-poll",
	})
	if err != nil {
		return nil, err
	}
	res.Mode = store.ModeAsyncFallback
	res.UpstreamMS = time.Since(started).Milliseconds()
	res.UpstreamOpID = id
	return res, nil
}

// pollURL rebuilds the poll target against the configured base.
//
// The container's own Operation-Location is deliberately discarded: it is corrupted whenever the
// inbound authority contains the substring "vision", losing both the port and the /vision path
// segment, so following it would send the poll to an address that does not exist.
func (c *ReadClient) pollURL(operationID string) string {
	return c.joinURL(ReadPathPrefix+"/analyzeResults/"+escapeSegment(operationID), url.Values{})
}
