package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

// DIPathPrefix is the literal path segment the Document Intelligence surface is served under.
//
// It is load-bearing well beyond routing: azure-ai-documentintelligence (Python),
// Azure.AI.DocumentIntelligence (.NET), the Java SDK and the JS SDK all derive the operation id
// from Operation-Location with the hard-coded expression
//
//	[^:]+://[^/]+/documentintelligence/.+/([^?/]+)
//
// so a gateway that serves this surface under any other prefix breaks every one of them, even
// when polling itself would work.
const DIPathPrefix = "/documentintelligence"

// syncCapability tracks what the gateway has learned about the undocumented :syncAnalyze route.
type syncCapability int32

const (
	syncUnknown syncCapability = iota
	syncAvailable
	syncUnavailable
)

// DIClient talks to the Document Intelligence layout container.
type DIClient struct {
	*base
	mode        config.SyncMode
	blindBudget int
	capability  atomic.Int32
}

// NewDI builds a Document Intelligence client.
func NewDI(up config.Upstream, mode config.SyncMode, blindBudget int, tempDir string, maxBytes int64) *DIClient {
	c := &DIClient{
		base: newBase("document-intelligence", up.BaseURL, up.APIKey, up.Timeout, tempDir,
			azerr.SurfaceDI, up.MaxInflight+2, maxBytes),
		mode:        mode,
		blindBudget: blindBudget,
	}
	if mode == config.SyncOff {
		c.capability.Store(int32(syncUnavailable))
	}
	return c
}

// Surface identifies the error vocabulary this client speaks.
func (c *DIClient) Surface() azerr.Surface { return azerr.SurfaceDI }

// Health reports container reachability plus what has been learned about :syncAnalyze.
func (c *DIClient) Health(ctx context.Context) Health {
	h := c.checkStatus(ctx)
	h.SyncAnalyze = c.capabilityName()
	return h
}

func (c *DIClient) capabilityName() string {
	switch syncCapability(c.capability.Load()) {
	case syncAvailable:
		return "available"
	case syncUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// Analyze runs one document through the layout container and blocks until it has a result.
//
// The synchronous route is tried first. It is undocumented, so three outcomes are all treated as
// normal rather than exceptional:
//
//	200  the result came back directly. Fully stateless; nothing is pinned to a replica.
//	202  the container degraded to async under load. The operation now lives only on the replica
//	     that answered, so polling continues on the same connection and cookie jar.
//	404  or 500 UnhandledEndpointException: this image build does not serve the route at all.
//	     The capability is latched off so later jobs skip straight to :analyze.
func (c *DIClient) Analyze(ctx context.Context, doc Document, req Request) (*Result, error) {
	if req.ModelID == "" {
		return nil, azerr.BadRequest(azerr.SurfaceDI, "A model id is required.")
	}
	deadline := time.Now().Add(c.timeout)

	// One affinity client for the whole job: if the synchronous call degrades to 202, the polls
	// that follow must reach the very replica that answered it.
	ac, err := newAffinityClient(c.timeout)
	if err != nil {
		return nil, err
	}
	defer ac.close()

	if !req.RequireOperation && syncCapability(c.capability.Load()) != syncUnavailable {
		res, err := c.trySync(ctx, ac, doc, req, deadline)
		switch {
		case err == nil:
			return res, nil
		case isSyncUnavailable(err):
			if c.mode == config.SyncForce {
				return nil, azerr.Internal(azerr.SurfaceDI,
					"DI_SYNC_ANALYZE=force but the container does not serve "+
						"/documentintelligence/documentModels/{modelId}:syncAnalyze.")
			}
			// Latch off so every later job skips the probe. The route either exists on an image
			// build or it does not; it does not appear at runtime.
			c.capability.Store(int32(syncUnavailable))
		default:
			return nil, err
		}
	}

	return c.analyzeAsync(ctx, ac, doc, req, deadline)
}

// trySync attempts the undocumented synchronous route.
func (c *DIClient) trySync(ctx context.Context, ac *affinityClient, doc Document,
	req Request, deadline time.Time) (*Result, error) {

	q := cloneQuery(req.Query)
	target := c.joinURL(DIPathPrefix+"/documentModels/"+escapeSegment(req.ModelID)+":syncAnalyze", q)

	started := time.Now()
	hreq, err := c.newRequest(ctx, http.MethodPost, target, &doc)
	if err != nil {
		return nil, err
	}
	resp, err := ac.http.Do(hreq)
	if err != nil {
		return nil, azerr.Internal(azerr.SurfaceDI,
			fmt.Sprintf("The document-intelligence container is unreachable: %v", err))
	}

	switch resp.StatusCode {
	case http.StatusOK:
		c.capability.Store(int32(syncAvailable))
		path, size, derr := c.downloadBody(resp, "di-sync")
		drain(resp)
		if derr != nil {
			return nil, derr
		}
		view, verr := inspectOperation(path, size, azerr.SurfaceDI)
		if verr != nil {
			_ = os.Remove(path)
			return nil, verr
		}
		if isFailed(view.Status) {
			_ = os.Remove(path)
			return nil, view.Err
		}
		if !view.HasResult {
			_ = os.Remove(path)
			return nil, azerr.Internal(azerr.SurfaceDI,
				"The document-intelligence container returned a synchronous response with no analyzeResult.")
		}
		return &Result{
			Path: path, Start: view.Start, End: view.End,
			Mode:           store.ModeSync,
			UpstreamMS:     time.Since(started).Milliseconds(),
			UpstreamStatus: http.StatusOK,
		}, nil

	case http.StatusAccepted:
		// The container fell back to async. This has been observed under memory pressure, and
		// Microsoft has described the 202 on this route as not standard behaviour. The operation
		// is now instance-local, so affinity matters from here on.
		c.capability.Store(int32(syncAvailable))
		loc := resp.Header.Get("Operation-Location")
		drain(resp)
		id, ok := operationIDFrom(loc)
		if !ok {
			return nil, azerr.Internal(azerr.SurfaceDI,
				"The document-intelligence container degraded to an asynchronous operation but "+
					"returned no usable Operation-Location.")
		}
		res, err := c.pollUntilTerminal(ctx, ac, pollConfig{
			URL:         c.pollURL(req.ModelID, id, req.Query),
			BlindBudget: c.blindBudget,
			Deadline:    deadline,
			Prefix:      "di-poll",
		})
		if err != nil {
			return nil, err
		}
		res.Mode = store.ModeDegraded
		res.UpstreamMS = time.Since(started).Milliseconds()
		res.UpstreamOpID = id
		return res, nil

	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		drain(resp)
		return nil, ErrSyncUnavailable

	case http.StatusInternalServerError:
		// Older image builds answer 500 UnhandledEndpointException for this route rather than
		// 404. Distinguish that from a genuine analysis failure by inspecting the body.
		apiErr := c.readErrorBody(resp)
		if looksLikeMissingEndpoint(apiErr) {
			return nil, ErrSyncUnavailable
		}
		return nil, apiErr

	default:
		return nil, c.readErrorBody(resp)
	}
}

// analyzeAsync submits to :analyze and polls with affinity. It is the fallback for image builds
// with no synchronous route.
func (c *DIClient) analyzeAsync(ctx context.Context, ac *affinityClient, doc Document,
	req Request, deadline time.Time) (*Result, error) {

	q := cloneQuery(req.Query)
	target := c.joinURL(DIPathPrefix+"/documentModels/"+escapeSegment(req.ModelID)+":analyze", q)

	started := time.Now()
	hreq, err := c.newRequest(ctx, http.MethodPost, target, &doc)
	if err != nil {
		return nil, err
	}
	resp, err := ac.http.Do(hreq)
	if err != nil {
		return nil, azerr.Internal(azerr.SurfaceDI,
			fmt.Sprintf("The document-intelligence container is unreachable: %v", err))
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil, c.readErrorBody(resp)
	}
	loc := resp.Header.Get("Operation-Location")
	drain(resp)

	id, ok := operationIDFrom(loc)
	if !ok {
		return nil, azerr.Internal(azerr.SurfaceDI,
			"The document-intelligence container accepted the request but returned no usable "+
				"Operation-Location.")
	}
	res, err := c.pollUntilTerminal(ctx, ac, pollConfig{
		URL:         c.pollURL(req.ModelID, id, req.Query),
		BlindBudget: c.blindBudget,
		Deadline:    deadline,
		Prefix:      "di-poll",
	})
	if err != nil {
		return nil, err
	}
	res.Mode = store.ModeAsyncFallback
	res.UpstreamMS = time.Since(started).Milliseconds()
	res.UpstreamOpID = id
	return res, nil
}

// pollURL rebuilds the poll target against the configured upstream base, keeping only the
// api-version from the original query.
func (c *DIClient) pollURL(modelID, resultID string, q url.Values) string {
	pq := url.Values{}
	if v := q.Get("api-version"); v != "" {
		pq.Set("api-version", v)
	}
	return c.joinURL(
		DIPathPrefix+"/documentModels/"+escapeSegment(modelID)+"/analyzeResults/"+escapeSegment(resultID),
		pq)
}

// FetchArtifact retrieves a result-file artifact such as the searchable PDF or a cropped figure.
//
// These endpoints are only reachable while the upstream operation still exists, so they are
// fetched eagerly at job completion rather than proxied on demand.
func (c *DIClient) FetchArtifact(ctx context.Context, modelID, resultID, suffix string,
	q url.Values) (path string, size int64, contentType string, err error) {

	pq := url.Values{}
	if v := q.Get("api-version"); v != "" {
		pq.Set("api-version", v)
	}
	target := c.joinURL(
		DIPathPrefix+"/documentModels/"+escapeSegment(modelID)+"/analyzeResults/"+
			escapeSegment(resultID)+suffix, pq)

	req, err := c.newRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", 0, "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, "", c.readErrorBody(resp)
	}
	ct := resp.Header.Get("Content-Type")
	p, n, derr := c.downloadBody(resp, "di-artifact")
	drain(resp)
	if derr != nil {
		return "", 0, "", derr
	}
	return p, n, ct, nil
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

func isSyncUnavailable(err error) bool {
	return err == ErrSyncUnavailable
}

// escapeSegment percent-encodes a single path segment.
//
// Encoding matters for more than correctness: an unencoded brace makes Python's pipeline raise
// inside str.format, and an unencoded brace, space, pipe or caret makes Java's new URI(path)
// throw, which silently demotes its poller instead of failing loudly.
func escapeSegment(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), ":", "%3A")
}

func cloneQuery(q url.Values) url.Values {
	out := url.Values{}
	for k, vs := range q {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}
