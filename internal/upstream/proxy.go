package upstream

import (
	"context"
	"io"
	"net/http"
	"net/url"
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
