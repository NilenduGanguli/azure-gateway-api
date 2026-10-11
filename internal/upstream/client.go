// Package upstream makes the gateway's calls to the Azure containers.
//
// Client is the only path. One attempt is one POST of the stored document to the container's
// synchronous analyze route, through the HAProxy in front of autoscaled pods, on a connection of
// its own. Do classifies the attempt into an Outcome and leaves retrying to its caller, with
// Backoff for the wait. Nothing is ever polled: an asynchronous operation lives on the pod that
// minted it, and an autoscaled pod can be gone before the poll arrives.
//
// Both containers expose a native synchronous analyze route. Read's is documented; Document
// Intelligence's is not — it is absent from every public doc and from azure-rest-api-specs, and
// was established from the container image's own routing table. Because it is undocumented it is
// also unreliable: it has been observed degrading to 202 under memory pressure, and older image
// builds answer 500 UnhandledEndpointException. Both are classified (as Retry and RouteMissing)
// rather than assumed away.
package upstream

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
)

// HeaderAPIKey is the subscription key header both containers accept.
const HeaderAPIKey = "Ocp-Apim-Subscription-Key"

// maxErrorBody bounds how much of a non-2xx upstream body is read before giving up on parsing
// it. Container errors are small; an unbounded read here would let a misbehaving upstream pin
// memory.
const maxErrorBody = 64 << 10

// Document is the input to an analyze call.
//
// Open returns a fresh reader each time so a call can be retried, and so the body is streamed
// from the PVC rather than held in memory.
type Document struct {
	ContentType string
	Size        int64
	Open        func() (io.ReadCloser, error)
}

// base holds one surface's container address, credential and limits.
type base struct {
	name     string
	baseURL  string
	apiKey   string
	timeout  time.Duration
	tempDir  string
	surface  azerr.Surface
	maxBytes int64
	// client serves health checks. It keeps no connection between them, as attempts do not.
	client *http.Client
}

// newRequest builds an upstream request with the API key attached.
//
// The client's own key is never forwarded: the gateway requires no client credential and holds
// its own upstream key, so a client cannot influence upstream authentication.
func (b *base) newRequest(ctx context.Context, method, rawURL string, doc *Document) (*http.Request, error) {
	var body io.ReadCloser
	if doc != nil && doc.Open != nil {
		rc, err := doc.Open()
		if err != nil {
			return nil, fmt.Errorf("upstream: open document: %w", err)
		}
		body = rc
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, fmt.Errorf("upstream: build request: %w", err)
	}
	if doc != nil {
		if doc.ContentType != "" {
			req.Header.Set("Content-Type", doc.ContentType)
		}
		if doc.Size > 0 {
			req.ContentLength = doc.Size
			opener := doc.Open
			req.GetBody = func() (io.ReadCloser, error) { return opener() }
		}
	}
	if b.apiKey != "" {
		req.Header.Set(HeaderAPIKey, b.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	// Never advertise br: .NET sends no Accept-Encoding at all and cannot decompress, and Node
	// accepts only gzip and deflate. Go's transport adds gzip transparently, which is safe.
	return req, nil
}

// tempFile creates a scratch file for one upstream response inside the blob root, so the
// existing sweeper reclaims it if the process dies mid-download.
func (b *base) tempFile(prefix string) (*os.File, error) {
	if err := os.MkdirAll(b.tempDir, 0o750); err != nil {
		return nil, fmt.Errorf("upstream: create temp dir: %w", err)
	}
	f, err := os.CreateTemp(b.tempDir, prefix+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("upstream: create temp file: %w", err)
	}
	return f, nil
}

// readErrorBody consumes a bounded prefix of a failed response and turns it into an APIError in
// the gateway's own surface vocabulary.
//
// Upstream error bodies come in four shapes — the DI wrapped object, a flat variant the service
// has been observed returning, the flat Read object, and the bare {"status":"Failed"} that CV
// syncAnalyze produces. Anything unrecognised, including HTML from an intervening proxy, is
// replaced with a well-formed error rather than leaked to a client.
func (b *base) readErrorBody(resp *http.Response) *azerr.APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	drain(resp)

	if e, ok := azerr.ParseUpstream(body, resp.StatusCode); ok {
		return b.translate(e, resp.StatusCode)
	}
	return b.genericError(resp.StatusCode)
}

// translate maps an upstream error onto the surface the gateway is answering on, keeping the
// upstream's code when it is already in that surface's vocabulary.
func (b *base) translate(e *azerr.APIError, status int) *azerr.APIError {
	out := &azerr.APIError{
		Status:    status,
		Code:      e.Code,
		Message:   e.Message,
		Target:    e.Target,
		Details:   e.Details,
		InnerCode: e.InnerCode,
		InnerMsg:  e.InnerMsg,
		RequestID: e.RequestID,
	}
	if out.Message == "" {
		out.Message = azerr.CanonicalMessage(out.Code)
	}
	if out.Code == "" {
		return b.genericError(status)
	}
	return out
}

// genericError synthesises a well-formed error for an unparseable upstream failure.
//
// The container's own status is preserved. Collapsing the range — every 4xx to 400, every 5xx to
// 500 — is what azerr.ForStatus exists to prevent: it turned the container's 404 for an unknown
// model into "InvalidRequest", and its 503 during a rolling restart into a flat 500, telling the
// client the request was malformed or the gateway broken when neither was true and the correct
// answer was retriable.
func (b *base) genericError(status int) *azerr.APIError {
	switch status {
	case http.StatusUnsupportedMediaType:
		return azerr.UnsupportedMediaType(b.surface)
	case http.StatusRequestEntityTooLarge:
		return azerr.ContentTooLarge(b.surface)
	}
	return azerr.ForStatus(b.surface, status,
		fmt.Sprintf("The %s container returned HTTP %d.", b.name, status))
}

// drain empties and closes a response body, bounded, so a misbehaving container cannot hold the
// caller reading it.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// joinURL builds an upstream URL from a path and query.
func (b *base) joinURL(path string, q url.Values) string {
	u := b.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// tempPrefix derives a filename-safe prefix from a surface name.
func tempPrefix(name string) string { return filepath.Base(name) }

// redactUserinfo removes credentials from a URL before it is shown on an unauthenticated endpoint.
func redactUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username() + ":***")
	return u.String()
}

// Client makes synchronous calls to one surface's container.
//
// It holds no connection between calls. Every attempt, health check and relayed GET dials afresh,
// so HAProxy balances each one on its own and a retry is free to reach a different pod. The
// gateway cannot address or count the pods behind the route; a new connection is the only lever
// it has.
type Client struct {
	b *base
}

// NewClient builds the client for one surface's container.
//
// tempDir receives the containers' answers. It is the blob root, so the blob sweeper reclaims a
// file left by a process that died mid-download. maxResultBytes caps one answer; zero or less is
// the 512 MB default.
func NewClient(surface azerr.Surface, up config.Upstream, tempDir string, maxResultBytes int64) *Client {
	name := "document-intelligence"
	if surface == azerr.SurfaceRead {
		name = "computer-vision-read"
	}
	return &Client{b: &base{
		name:     name,
		baseURL:  strings.TrimRight(up.BaseURL, "/"),
		apiKey:   up.APIKey,
		timeout:  up.Timeout,
		tempDir:  tempDir,
		surface:  surface,
		maxBytes: maxResultBytes,
		// A pooled client would keep an idle connection to HAProxy between health checks. This
		// one shares the attempts' transport, which never keeps one, and bounds nothing itself:
		// checkStatus applies its own deadline.
		client: &http.Client{Transport: newAttemptTransport(), CheckRedirect: noRedirects},
	}}
}

// Surface identifies the error vocabulary this client speaks.
func (c *Client) Surface() azerr.Surface { return c.b.surface }

// Name is the container's name in logs and errors: "document-intelligence" or
// "computer-vision-read".
func (c *Client) Name() string { return c.b.name }

// attemptDialTimeout bounds connecting to HAProxy. It is deliberately not the attempt's deadline:
// a route that cannot be reached fails in seconds and is classified as a connection failure,
// which is retried as often as needed, rather than as a slow analysis, which is retried once.
const attemptDialTimeout = 10 * time.Second

// newAttemptTransport builds the transport for one call.
//
// Keep-alive is off, so the connection closes when the answer has been read, and nothing outlives
// the call to pin the next one to the pod that just failed. HTTP/2 is not forced: a connection
// carries one request, so multiplexing gains nothing.
//
// TCP keep-alive probes stay on. A synchronous analysis holds a silent connection open for
// minutes, and a middlebox that sees no traffic may drop it.
func newAttemptTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   attemptDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
}

// noRedirects stops a client following a redirect. .NET's pipeline sets AllowAutoRedirect=false
// and treats any 3xx as terminal, and the containers have no reason to redirect: following one
// would mask a misconfiguration that clients would experience as a hard failure.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
