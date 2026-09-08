package upstream

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

// SyncOutcome is the result of a client-facing synchronous analyze.
//
// Exactly one of Live and Resolved is set. Live is the container's own response, streamed straight
// through. Resolved means the container degraded to a 202 and the gateway polled the operation out
// rather than handing the client a 202 it could never follow.
type SyncOutcome struct {
	// Live is the container's response, still open. The caller must close its Body.
	Live *http.Response
	// Resolved holds a completed analysis whose analyzeResult the caller should serve. The caller
	// must call its Cleanup.
	Resolved *Result

	release func()
}

// Close releases the per-call connection pool.
func (o *SyncOutcome) Close() {
	if o == nil {
		return
	}
	if o.Live != nil && o.Live.Body != nil {
		_ = o.Live.Body.Close()
	}
	if o.Resolved != nil {
		o.Resolved.Cleanup()
	}
	if o.release != nil {
		o.release()
	}
}

// SyncAnalyzer is implemented by both surfaces' clients.
type SyncAnalyzer interface {
	SyncAnalyze(ctx context.Context, req Request, contentType string, body io.Reader,
		contentLength int64) (*SyncOutcome, error)
}

// syncAnalyze performs a client-facing synchronous call and resolves a degraded 202.
//
// Relaying that 202 is what the naive implementation does, and it strands the caller: the header
// carrying the operation id is not usable by a client — it names an address inside the cluster,
// and on the Read container it is corrupted outright when the authority contains "vision" — so
// dropping it leaves a bodyless 202 with no way to reach a result the container has already been
// paid to produce. Polling it out here costs the caller the wait it already signed up for by
// choosing the synchronous route.
func (b *base) syncAnalyze(ctx context.Context, path string, q url.Values, contentType string,
	body io.Reader, contentLength int64, pollURL func(opID string) string,
	blindBudget int) (*SyncOutcome, error) {

	ac, err := newAffinityClient(b.timeout)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(b.timeout)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.joinURL(path, q), body)
	if err != nil {
		ac.close()
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// A negative length is a chunked body with no Content-Length, which every SDK sends when
	// streaming a file. Leaving it at -1 forwards it chunked rather than buffering to measure it.
	req.ContentLength = contentLength
	if b.apiKey != "" {
		req.Header.Set(HeaderAPIKey, b.apiKey)
	}

	resp, err := ac.http.Do(req)
	if err != nil {
		ac.close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, azerr.Internal(b.surface, "The "+b.name+" container is unreachable.")
	}

	if resp.StatusCode != http.StatusAccepted {
		// Including errors: the caller inspects and normalises them.
		return &SyncOutcome{Live: resp, release: ac.close}, nil
	}

	loc := resp.Header.Get("Operation-Location")
	drain(resp)
	opID, ok := operationIDFrom(loc)
	if !ok {
		ac.close()
		return nil, azerr.Internal(b.surface,
			"The "+b.name+" container degraded to an asynchronous operation but returned no "+
				"usable Operation-Location, so the analysis cannot be retrieved.")
	}

	res, err := b.pollUntilTerminal(ctx, ac, pollConfig{
		URL:         pollURL(opID),
		BlindBudget: blindBudget,
		Deadline:    deadline,
		Prefix:      "sync-poll",
	})
	if err != nil {
		ac.close()
		return nil, err
	}
	res.Mode = store.ModeDegraded
	res.UpstreamOpID = opID
	return &SyncOutcome{Resolved: res, release: ac.close}, nil
}

// SyncAnalyze runs the Document Intelligence synchronous route for a client that called ours.
func (c *DIClient) SyncAnalyze(ctx context.Context, req Request, contentType string,
	body io.Reader, contentLength int64) (*SyncOutcome, error) {

	if req.ModelID == "" {
		return nil, azerr.BadRequest(azerr.SurfaceDI, "A model id is required.")
	}
	path := DIPathPrefix + "/documentModels/" + escapeSegment(req.ModelID) + ":syncAnalyze"
	return c.syncAnalyze(ctx, path, cloneQuery(req.Query), contentType, body, contentLength,
		func(opID string) string { return c.pollURL(req.ModelID, opID, req.Query) },
		c.blindBudget)
}

// SyncAnalyze runs the Computer Vision Read synchronous route for a client that called ours.
func (c *ReadClient) SyncAnalyze(ctx context.Context, req Request, contentType string,
	body io.Reader, contentLength int64) (*SyncOutcome, error) {

	return c.syncAnalyze(ctx, ReadPathPrefix+"/syncAnalyze", cloneQuery(req.Query), contentType,
		body, contentLength, c.pollURL, c.blindBudget)
}

// OpenResolved returns a reader over the analyzeResult of a resolved outcome.
func (o *SyncOutcome) OpenResolved() (io.ReadCloser, int64, error) {
	f, err := os.Open(o.Resolved.Path)
	if err != nil {
		return nil, 0, err
	}
	size := o.Resolved.End - o.Resolved.Start
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, o.Resolved.Start, size), f}, size, nil
}
