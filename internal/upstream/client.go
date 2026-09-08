// Package upstream makes the gateway's calls to the Azure containers.
//
// Every strategy is hidden behind one blocking Analyze call, so a worker never knows whether the
// container answered synchronously, degraded to a 202 it had to poll, or lacks the synchronous
// route entirely.
//
// Both containers expose a native synchronous analyze route. Read's is documented; Document
// Intelligence's is not — it is absent from every public doc and from azure-rest-api-specs, and
// was established from the container image's own routing table. Because it is undocumented it is
// also unreliable: it has been observed degrading to 202 under memory pressure, and older image
// builds answer 500 UnhandledEndpointException. Both cases are handled rather than assumed away.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
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

// Result is a successful analysis.
//
// The raw upstream response is left on disk at Path and Start/End delimit the analyzeResult
// member inside it, so the caller can compose its client-facing envelope with a range copy
// instead of loading a result that Azure permits to be 500 MB.
type Result struct {
	Path           string
	Start, End     int64
	Mode           store.UpstreamMode
	UpstreamMS     int64
	UpstreamStatus int
	// UpstreamOpID is the container's own operation id, set only when the asynchronous path ran.
	// It is what the result-file endpoints are addressed by.
	UpstreamOpID string
}

// Size is the length of the analyzeResult member.
func (r *Result) Size() int64 { return r.End - r.Start }

// Cleanup removes the temporary upstream response. It is safe to call more than once.
func (r *Result) Cleanup() {
	if r == nil || r.Path == "" {
		return
	}
	_ = os.Remove(r.Path)
	r.Path = ""
}

// Request carries the per-call parameters that vary between surfaces.
type Request struct {
	// ModelID is the Document Intelligence model, e.g. prebuilt-layout. Unused by Read.
	ModelID string
	// Query is the client's query string, forwarded to the container unchanged apart from
	// parameters the gateway owns.
	Query url.Values
	// RequireOperation forces the asynchronous upstream path even when a synchronous route is
	// available.
	//
	// Result-file endpoints — the searchable PDF and cropped figures — are addressed by the
	// container's own operation id, and a synchronous call never mints one. So a submit that asks
	// for output=pdf or output=figures must go through :analyze even though it costs the
	// stateless-call property, or the artifacts would be unreachable afterwards.
	RequireOperation bool
}

// Analyzer performs one blocking analysis against a container.
type Analyzer interface {
	// Analyze runs the document through the container and returns the result. The returned
	// Result owns a temp file the caller must Cleanup.
	Analyze(ctx context.Context, doc Document, req Request) (*Result, error)
	// Surface identifies which error vocabulary this analyzer speaks.
	Surface() azerr.Surface
	// Health reports whether the container is reachable and what the gateway detected about it.
	Health(ctx context.Context) Health
}

// Health is a snapshot of one upstream's state, surfaced on the admin endpoints.
type Health struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	OK   bool   `json:"ok"`
	// Detail carries the container's own status message, or the reason it is unreachable.
	Detail string `json:"detail,omitempty"`
	// SyncAnalyze reports what the gateway has learned about the synchronous route:
	// "unknown" before the first call, then "available" or "unavailable".
	SyncAnalyze string `json:"syncAnalyze"`
	LatencyMS   int64  `json:"latencyMs,omitempty"`
}

// transportConfig builds the shared HTTP transport settings.
//
// Redirects are never followed: .NET's pipeline sets AllowAutoRedirect=false and treats any 3xx
// as terminal, and the containers have no reason to redirect. Following one here would mask a
// misconfiguration that clients would experience as a hard failure.
func newTransport(maxConns int) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          maxConns,
		MaxIdleConnsPerHost:   maxConns,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// affinityClient is a client dedicated to a single job.
//
// When a container degrades to 202 the resulting operation id is meaningful only to the replica
// that minted it. Two mechanisms keep the follow-up polls on that replica: a private connection
// pool capped at one idle connection, so keep-alive holds the same backend through the router,
// and a private cookie jar, so the OpenShift HAProxy affinity cookie is replayed.
//
// Kubernetes sessionAffinity: ClientIP cannot help here — every request from the single gateway
// pod shares a source IP, so it would pin all traffic to one backend rather than pinning each
// job to its own.
type affinityClient struct {
	http *http.Client
	tr   *http.Transport
}

func newAffinityClient(timeout time.Duration) (*affinityClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("upstream: create cookie jar: %w", err)
	}
	tr := newTransport(1)
	tr.DisableKeepAlives = false
	return &affinityClient{
		tr: tr,
		http: &http.Client{
			Transport: tr,
			Jar:       jar,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (a *affinityClient) close() { a.tr.CloseIdleConnections() }

// base holds the plumbing shared by both surface clients.
type base struct {
	name     string
	baseURL  string
	apiKey   string
	timeout  time.Duration
	tempDir  string
	client   *http.Client
	surface  azerr.Surface
	maxBytes int64
}

func newBase(name, baseURL, apiKey string, timeout time.Duration, tempDir string,
	surface azerr.Surface, maxConns int, maxBytes int64) *base {
	return &base{
		name:     name,
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		timeout:  timeout,
		tempDir:  tempDir,
		surface:  surface,
		maxBytes: maxBytes,
		client: &http.Client{
			Transport: newTransport(maxConns),
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
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
func (b *base) genericError(status int) *azerr.APIError {
	switch {
	case status == http.StatusUnsupportedMediaType:
		return azerr.UnsupportedMediaType(b.surface)
	case status == http.StatusRequestEntityTooLarge:
		return azerr.ContentTooLarge(b.surface)
	case status >= 500:
		return azerr.Internal(b.surface,
			fmt.Sprintf("The %s container returned HTTP %d.", b.name, status))
	case status >= 400:
		return azerr.BadRequest(b.surface,
			fmt.Sprintf("The %s container rejected the request with HTTP %d.", b.name, status))
	default:
		return azerr.Internal(b.surface,
			fmt.Sprintf("The %s container returned an unexpected HTTP %d.", b.name, status))
	}
}

// drain empties and closes a response body so the connection can be reused. Leaking a body would
// force a new TCP connection on the next call, which on the affinity path would also lose the
// backend the job is pinned to.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// checkStatus fetches the container's /status endpoint, which validates its billing key without
// consuming a model query.
func (b *base) checkStatus(ctx context.Context) Health {
	h := Health{Name: b.name, URL: redactUserinfo(b.baseURL), SyncAnalyze: "unknown"}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	started := time.Now()
	req, err := b.newRequest(ctx, http.MethodGet, b.baseURL+"/status", nil)
	if err != nil {
		h.Detail = err.Error()
		return h
	}
	resp, err := b.client.Do(req)
	if err != nil {
		h.Detail = err.Error()
		return h
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	drain(resp)
	h.LatencyMS = time.Since(started).Milliseconds()
	h.OK = resp.StatusCode == http.StatusOK
	h.Detail = summarizeStatus(body, resp.StatusCode)
	return h
}

// summarizeStatus extracts the container's apiStatus fields when present.
func summarizeStatus(body []byte, code int) string {
	var s struct {
		APIStatus  string `json:"apiStatus"`
		APIMessage string `json:"apiStatusMessage"`
	}
	if err := jsonUnmarshal(body, &s); err == nil && s.APIStatus != "" {
		if s.APIMessage != "" {
			return s.APIStatus + ": " + s.APIMessage
		}
		return s.APIStatus
	}
	if code != http.StatusOK {
		return fmt.Sprintf("HTTP %d", code)
	}
	return ""
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

// unreachable turns a transport failure into the right kind of error.
//
// A cancelled or expired context is not an upstream fault: during graceful shutdown every
// in-flight request fails this way, and reporting it as a container error would make the worker
// mark a job terminally failed instead of leaving it for the recovery pass to resume. The 202 for
// that job has already been sent, so failing it would break the promise.
func (b *base) unreachable(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// The transport error text embeds the full upstream URL, which is internal topology. It is
	// logged by the caller; the client is told only which surface failed.
	return azerr.Internal(b.surface,
		fmt.Sprintf("The %s container is unreachable.", b.name))
}

// redactUserinfo removes credentials from a URL before it is shown on an unauthenticated endpoint.
func redactUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username() + ":***")
	return u.String()
}

// ErrSyncUnavailable reports that a container does not serve its synchronous analyze route.
var ErrSyncUnavailable = errors.New("upstream: synchronous analyze route unavailable")

// ErrDegradedToAsync reports that a synchronous call returned 202 instead of a result.
var ErrDegradedToAsync = errors.New("upstream: synchronous analyze degraded to async")
