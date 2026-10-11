package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Passthrough forwards a request to the container and returns the live response for the caller to
// stream on.
//
// It is used for the synchronous client-facing routes and for read-only metadata endpoints, where
// the gateway adds nothing but the upstream credential. The body is streamed in both directions:
// a synchronous analyze can carry hundreds of megabytes, and buffering either side would defeat
// the point of passing it through.
//
// The caller owns the returned response and must close its body.
func (b *base) Passthrough(ctx context.Context, method, path string, q url.Values,
	contentType string, body io.Reader, contentLength int64) (*http.Response, error) {

	req, err := http.NewRequestWithContext(ctx, method, b.joinURL(path, q), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// A negative length means the client sent a chunked body with no Content-Length, which every
	// SDK does when streaming a file. Leaving ContentLength at -1 makes Go forward it chunked
	// rather than buffering it to compute a length.
	req.ContentLength = contentLength
	if b.apiKey != "" {
		req.Header.Set(HeaderAPIKey, b.apiKey)
	}
	return b.client.Do(req)
}

// PassthroughGET forwards a read-only request, such as a model or info lookup.
func (b *base) PassthroughGET(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	return b.Passthrough(ctx, http.MethodGet, path, q, "", nil, 0)
}

// BaseURL reports the configured upstream root.
func (b *base) BaseURL() string { return b.baseURL }

// Name reports the upstream's log-facing name.
func (b *base) Name() string { return b.name }

// proxyTimeout bounds one relayed GET. These routes answer in milliseconds; letting one inherit
// the analysis timeout would let a wedged container hold it open for a quarter of an hour.
const proxyTimeout = 10 * time.Second

// ProxyGET relays a small GET to the container — metadata, swagger, usage records — and returns
// its live response. The caller must close the body.
//
// pathAndQuery is sent exactly as given, so the caller builds it from routed, validated parts and
// never from the client's raw path: the request carries the gateway's own key. accept is the
// client's Accept header, so swagger's HTML and JSON both relay; empty sends none.
//
// The client lives for this one call, on a connection of its own like every attempt, and
// proxyTimeout keeps running while the caller reads the body.
func (c *Client) ProxyGET(ctx context.Context, pathAndQuery, accept string) (*http.Response, error) {
	// Appended to the base URL, anything else could change the host: "@evil.example/" after
	// "http://container:5000" makes the container's host and port into userinfo.
	if !strings.HasPrefix(pathAndQuery, "/") {
		return nil, fmt.Errorf("upstream: proxy path %q does not start with /", pathAndQuery)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.b.baseURL+pathAndQuery, nil)
	if err != nil {
		return nil, fmt.Errorf("upstream: build proxy request: %w", transportCause(err))
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if c.b.apiKey != "" {
		req.Header.Set(HeaderAPIKey, c.b.apiKey)
	}
	hc := &http.Client{Transport: newAttemptTransport(), Timeout: proxyTimeout, CheckRedirect: noRedirects}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: proxy GET: %w", err)
	}
	return resp, nil
}
