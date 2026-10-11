package upstream

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

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
		// Reduced to its cause: the *url.Error names the internal upstream address, and an error
		// that reaches a caller must not carry it.
		return nil, fmt.Errorf("upstream: proxy GET: %w", transportCause(err))
	}
	return resp, nil
}
