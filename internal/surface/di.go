package surface

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/logging"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// DefaultAPIVersion is the Document Intelligence contract this gateway reproduces.
const DefaultAPIVersion = "2024-11-30"

const diPrefix = upstream.DIPathPrefix

// frPrefix is the legacy Form Recognizer family.
//
// It is served because the containers serve it: a probed layout-4.0 build declares
// /formrecognizer/documentModels/{modelId}:analyze, :syncAnalyze and the matching analyzeResults
// route alongside the /documentintelligence ones. A client on azure-ai-formrecognizer therefore
// reaches the container today and would have hit a 404 on a gateway that only served the newer
// prefix.
//
// The prefix a caller arrived on is echoed back in Operation-Location, because every SDK derives
// the operation id from that header with a regex that hard-codes its own prefix.
const frPrefix = "/formrecognizer"

// prefixOf reports which family a request arrived on.
func prefixOf(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, frPrefix+"/") {
		return frPrefix
	}
	return diPrefix
}

func (s *Server) registerDI(mux *http.ServeMux) {
	// The colon action is part of the final path segment, so {action} captures
	// "prebuilt-layout:analyze" whole and the verb is split off below.
	mux.HandleFunc("POST "+diPrefix+"/documentModels/{action}", s.diAction)

	mux.HandleFunc("GET "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diPoll)
	mux.HandleFunc("HEAD "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diPoll)
	mux.HandleFunc("DELETE "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diDelete)
	mux.HandleFunc("GET "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}/pdf", s.diPDF)
	mux.HandleFunc("GET "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}/figures/{figureId}", s.diFigure)

	// The legacy family, served by the same handlers. Operation-Location echoes whichever prefix
	// the caller used.
	mux.HandleFunc("POST "+frPrefix+"/documentModels/{action}", s.diAction)
	mux.HandleFunc("GET "+frPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diPoll)
	mux.HandleFunc("HEAD "+frPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diPoll)
	mux.HandleFunc("DELETE "+frPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diDelete)
	mux.HandleFunc("GET "+frPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}/pdf", s.diPDF)
	mux.HandleFunc("GET "+frPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}/figures/{figureId}", s.diFigure)

	// The metadata routes are registered on both families too. A legacy SDK pinned to
	// /formrecognizer calls GetResourceDetails and GetDocumentModel on its own prefix, and
	// serving those only under /documentintelligence answered it a bodyless 404 from the
	// catch-all — a client-visible difference from the containers, which serve both.
	for _, fam := range []string{diPrefix, frPrefix} {
		mux.HandleFunc("GET "+fam+"/info", s.diInfo)
		mux.HandleFunc("GET "+fam+"/documentModels", s.diListModels)
		mux.HandleFunc("GET "+fam+"/documentModels/{modelId}", s.diGetModel)
	}
}

// diAction dispatches the colon-suffixed analyze verbs.
func (s *Server) diAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	modelID, verb, found := strings.Cut(action, ":")
	if !found || modelID == "" {
		azerr.BadRequest(azerr.SurfaceDI,
			"The request path must name a model and an action, for example "+
				"prebuilt-layout:analyze.").WriteTo(w, azerr.SurfaceDI)
		return
	}
	switch verb {
	case "analyze":
		s.diAnalyze(w, r, modelID)
	case "syncAnalyze":
		s.diSyncAnalyze(w, r, modelID)
	case "analyzeBatch":
		// Batch is addressed by Azure Blob source and target containers, which do not exist in an
		// air-gapped deployment, so there is nothing for the gateway to usefully do with it.
		azerr.New(http.StatusBadRequest, azerr.CodeInvalidRequest,
			"Batch analysis requires Azure Blob Storage and is not available through this gateway.").
			WriteTo(w, azerr.SurfaceDI)
	default:
		azerr.New(http.StatusNotFound, azerr.CodeNotFound,
			"The requested action is not supported.").WriteTo(w, azerr.SurfaceDI)
	}
}

// diAnalyze accepts an asynchronous analysis.
func (s *Server) diAnalyze(w http.ResponseWriter, r *http.Request, modelID string) {
	apiVersion, err := requireAPIVersion(r)
	if err != nil {
		err.WriteTo(w, azerr.SurfaceDI)
		return
	}
	prefix := prefixOf(r)
	s.submit(w, r, submitParams{
		surface:        azerr.SurfaceDI,
		surfaceName:    jobs.SurfaceDI,
		modelID:        modelID,
		apiVersion:     apiVersion,
		upstreamQuery:  upstreamQuery(r.URL.Query()),
		upstreamPrefix: prefix,
		operationLocation: func(base, id string) string {
			return diOperationLocation(base, prefix, modelID, id, apiVersion)
		},
	})
}

// diOperationLocation builds the poll URL for a Document Intelligence operation.
//
// The shape satisfies four independent parsers at once, and every part of it is required by at
// least one of them:
//
//   - absolute, because Python joins a relative value onto its own endpoint and produces a
//     doubled /documentintelligence/documentintelligence/ path;
//   - the literal /documentintelligence/ segment, because Python, .NET, Java and JS all derive
//     the operation id with the hard-coded regex [^:]+://[^/]+/documentintelligence/.+/([^?/]+);
//   - modelId four segments from the end and resultId two from the end, because
//     Azure.AI.FormRecognizer 4.x discards the URL and counts backwards after Split('/','?');
//   - ?api-version present, because Python never re-appends it while .NET and Java replace it;
//   - percent-encoded throughout, because a literal brace raises in Python's str.format and makes
//     Java's new URI(path) throw, which silently demotes its poller instead of failing loudly.
func diOperationLocation(base, prefix, modelID, resultID, apiVersion string) string {
	q := url.Values{}
	q.Set("api-version", apiVersion)
	return base + prefix + "/documentModels/" + url.PathEscape(modelID) +
		"/analyzeResults/" + url.PathEscape(resultID) + "?" + q.Encode()
}

// diSyncAnalyze passes a synchronous analysis straight through to the container.
//
// Nothing is persisted: the client is holding the connection and will have the whole result when
// it returns, so there is no operation id to keep alive afterwards. A slot is still taken, so the
// number of concurrent upstream calls stays bounded by one number across both call styles.
func (s *Server) diSyncAnalyze(w http.ResponseWriter, r *http.Request, modelID string) {
	if _, err := requireAPIVersion(r); err != nil {
		err.WriteTo(w, azerr.SurfaceDI)
		return
	}
	s.passthrough(w, r, azerr.SurfaceDI, jobs.SurfaceDI, upstream.Request{
		ModelID: modelID, Query: upstreamQuery(r.URL.Query()), Prefix: prefixOf(r),
	})
}

func (s *Server) diPoll(w http.ResponseWriter, r *http.Request) {
	s.poll(w, r, azerr.SurfaceDI, jobs.SurfaceDI, r.PathValue("resultId"))
}

func (s *Server) diDelete(w http.ResponseWriter, r *http.Request) {
	s.deleteResult(w, r, azerr.SurfaceDI, jobs.SurfaceDI, r.PathValue("resultId"))
}

// diPDF serves the searchable PDF captured when the submission asked for output=pdf.
func (s *Server) diPDF(w http.ResponseWriter, r *http.Request) {
	s.diArtifact(w, r, store.KindPDF, "application/pdf")
}

// diFigure serves one cropped figure captured when the submission asked for output=figures.
func (s *Server) diFigure(w http.ResponseWriter, r *http.Request) {
	s.diArtifact(w, r, store.KindFigure(r.PathValue("figureId")), "image/png")
}

// diArtifact serves a stored result file.
//
// Artifacts are captured at job completion rather than proxied on demand: they are addressed by
// the container's own operation id, which lives only as long as that container's configured TTL
// and only on the replica that produced it.
func (s *Server) diArtifact(w http.ResponseWriter, r *http.Request, kind store.Kind, contentType string) {
	id, ok := ids.Normalize(r.PathValue("resultId"))
	if !ok {
		azerr.NotFound(azerr.SurfaceDI).WriteTo(w, azerr.SurfaceDI)
		return
	}
	job, err := s.deps.Store.Get(r.Context(), id)
	if err != nil || job.Surface != jobs.SurfaceDI || s.deps.Now().After(job.ExpiresAt) {
		azerr.NotFound(azerr.SurfaceDI).WriteTo(w, azerr.SurfaceDI)
		return
	}
	f, size, err := s.deps.Store.Blob.Open(id, kind)
	if err != nil {
		azerr.New(http.StatusNotFound, azerr.CodeNotFound,
			"The requested result file was not produced for this operation.").
			WithInner(azerr.InnerOperationNotFound, "").
			WriteTo(w, azerr.SurfaceDI)
		return
	}
	defer func() { _ = f.Close() }()

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

// diInfo proxies GET {family}/info.
func (s *Server) diInfo(w http.ResponseWriter, r *http.Request) {
	s.proxyGET(w, r, azerr.SurfaceDI, jobs.SurfaceDI, prefixOf(r)+"/info")
}

// diListModels proxies GET {family}/documentModels.
func (s *Server) diListModels(w http.ResponseWriter, r *http.Request) {
	s.proxyGET(w, r, azerr.SurfaceDI, jobs.SurfaceDI, prefixOf(r)+"/documentModels")
}

// diGetModel proxies GET {family}/documentModels/{modelId}.
//
// The path is rebuilt from the routed component and re-escaped rather than taken from the request.
// Forwarding the raw path would let a caller shape the URL the gateway signs with its own upstream
// key, and the gateway is the only party holding that credential.
func (s *Server) diGetModel(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("modelId")
	if !validModelID(modelID) {
		azerr.InvalidParameter(azerr.SurfaceDI, "modelId", "the value is not a valid model name").
			WriteTo(w, azerr.SurfaceDI)
		return
	}
	s.proxyGET(w, r, azerr.SurfaceDI, jobs.SurfaceDI,
		prefixOf(r)+"/documentModels/"+url.PathEscape(modelID))
}

// validModelID applies the contract's own constraint: maxLength 64, pattern
// ^[a-zA-Z0-9][a-zA-Z0-9._~-]{1,63}$.
func validModelID(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if i == 0 {
			if !alnum {
				return false
			}
			continue
		}
		if !alnum && c != '.' && c != '_' && c != '~' && c != '-' {
			return false
		}
	}
	return true
}

// requireAPIVersion enforces the query parameter every SDK sends.
func requireAPIVersion(r *http.Request) (string, *azerr.APIError) {
	v := r.URL.Query().Get("api-version")
	if v == "" {
		return "", azerr.New(http.StatusBadRequest, azerr.CodeInvalidArgument, "").
			WithInner(azerr.InnerParameterMissing, "The parameter api-version is required.")
	}
	return v, nil
}

// upstreamQuery strips parameters the gateway owns and forwards the rest untouched.
//
// Everything the container understands — pages, locale, stringIndexType, features, queryFields,
// outputContentFormat, output, split, language, readingOrder, model-version — passes through
// unread, so a parameter added by a future container build keeps working without a gateway change.
func upstreamQuery(q url.Values) url.Values {
	out := url.Values{}
	for k, vs := range q {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}

// passthrough serves a client-facing synchronous analyze.
//
// It is not a blind relay. The container's synchronous route is not contractually synchronous —
// it has been observed degrading to 202 under memory pressure — and relaying that 202 would hand
// the caller an empty body with no way to reach a result the container has already been paid to
// produce. The upstream client resolves that case by polling the operation out; this handler only
// ever emits a terminal answer.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	surfaceName string, req upstream.Request) {

	release, err := s.deps.Jobs.Admit(surfaceName)
	if err != nil {
		azerr.TooBusy(surface, s.deps.Config.BusyRetryAfter).WriteTo(w, surface)
		return
	}
	defer release()

	// Admission alone is sized MaxInflight+QueueDepth. The container's own concurrency limit is
	// MaxInflight, so a synchronous call takes a container slot too.
	releaseUpstream, err := s.deps.Jobs.AdmitUpstream(surfaceName)
	if err != nil {
		azerr.TooBusy(surface, s.deps.Config.BusyRetryAfter).WriteTo(w, surface)
		return
	}
	defer releaseUpstream()

	client, ok := s.deps.Jobs.Analyzer(surfaceName).(upstream.SyncAnalyzer)
	if !ok {
		azerr.Internal(surface, "This surface does not support synchronous analysis.").
			WriteTo(w, surface)
		return
	}

	s.setUploadDeadline(w, r)

	body := http.MaxBytesReader(w, r.Body, s.deps.Config.MaxRequestBytes)
	outcome, err := client.SyncAnalyze(r.Context(), req, r.Header.Get("Content-Type"),
		body, r.ContentLength)
	if err != nil {
		s.writeUpstreamError(w, r, surface, err)
		return
	}
	defer outcome.Close()

	if outcome.Resolved != nil {
		s.writeResolvedSync(w, r, surface, outcome)
		return
	}
	s.relaySync(w, r, surface, outcome.Live)
}

// writeUpstreamError renders a failed synchronous call in the surface's own error shape.
//
// An oversized body is reported as the documented 400 rather than the transport-level error
// http.MaxBytesReader produces, and a body that ran past the cap is never mistaken for the
// container being unreachable.
func (s *Server) writeUpstreamError(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	err error) {

	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		azerr.ContentTooLarge(surface).WriteTo(w, surface)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The caller hung up or the deadline passed; nothing useful to send.
		logging.From(r.Context()).Info("synchronous call abandoned", "error", err)
		return
	}
	if apiErr, ok := azerr.AsAPIError(err); ok {
		apiErr.WriteTo(w, surface)
		return
	}
	logging.From(r.Context()).Warn("synchronous call failed", "error", err)
	azerr.Internal(surface, "").WriteTo(w, surface)
}

// writeResolvedSync serves an analysis the gateway had to poll out of a degraded 202.
//
// The envelope is the shape both surfaces use for a completed operation, which is also what the
// Read documentation says its synchronous route returns: "the same object graph as the
// asynchronous version".
func (s *Server) writeResolvedSync(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	outcome *upstream.SyncOutcome) {

	reader, size, err := outcome.OpenResolved()
	if err != nil {
		azerr.Internal(surface, "The analysis completed but its result could not be read.").
			WriteTo(w, surface)
		return
	}
	defer func() { _ = reader.Close() }()

	now := s.deps.Now().Format(jobs.TimeFormat)
	prefix := fmt.Sprintf(
		`{"status":"succeeded","createdDateTime":"%s","lastUpdatedDateTime":"%s","analyzeResult":`,
		now, now)
	const suffix = "}"

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.FormatInt(int64(len(prefix))+size+int64(len(suffix)), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.WriteString(w, prefix); err != nil {
		return
	}
	if _, err := io.Copy(w, reader); err != nil {
		logging.From(r.Context()).Warn("synchronous result stream interrupted", "error", err)
		return
	}
	_, _ = io.WriteString(w, suffix)
}

// relaySync forwards the container's own synchronous response.
//
// Successful bodies stream through untouched. Failures do not: the containers emit four different
// error shapes on these routes, including a bare {"status":"Failed"} with no code or message and,
// behind an nginx sidecar, HTML. Those are normalised into this surface's contract so a client's
// SDK always has something it can parse.
func (s *Server) relaySync(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	resp *http.Response) {

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		copyResponse(w, resp)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	// A bodyless upstream error is relayed as-is. Both containers answer some routes that way —
	// /info and /documentModels were bodyless 404s on a probed build — and inventing a body would
	// misrepresent them.
	if len(bytes.TrimSpace(raw)) == 0 {
		for k, vs := range resp.Header {
			// Retry-After is dropped here for the same reason the parsed branch below drops it:
			// azure-core retries ANY response >= 400 carrying it, ten times, bypassing its own
			// method allowlist. Relaying the container's header verbatim turned a bodyless 404 —
			// which is what a probed build answers for /info and /documentModels — into ten
			// retries of a request that can never succeed. It would also relay an HTTP-date
			// value, which the SDKs' integer-only parser rejects.
			if !skipRelayHeader(k) && k != "Content-Length" &&
				http.CanonicalHeaderKey(k) != "Retry-After" {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return
	}

	if parsed, ok := azerr.ParseUpstream(raw, resp.StatusCode); ok && parsed.Code != "" {
		parsed.Status = resp.StatusCode
		// Retry-After is deliberately not carried over: azure-core retries any response >= 400
		// that carries it, ten times, bypassing its method allowlist. azerr re-adds it only for
		// statuses where retrying is correct.
		parsed.RetryAfter = 0
		if retriableStatus(resp.StatusCode) {
			parsed.RetryAfter = s.deps.Config.BusyRetryAfter
		}
		parsed.WriteTo(w, surface)
		return
	}
	// Unparseable but not empty — HTML from an nginx sidecar, say. Give the client something its
	// SDK can parse, at the status the container actually returned.
	logging.From(r.Context()).Warn("upstream returned an unparseable error body",
		"status", resp.StatusCode, "bytes", len(raw))
	azerr.ForStatus(surface, resp.StatusCode, "").WriteTo(w, surface)
}

func retriableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// metadataTimeout bounds a read-only proxy call.
//
// These endpoints answer in milliseconds. Letting them inherit the analysis timeout meant a wedged
// container could hold each one open for a quarter of an hour, and with no concurrency limit they
// piled up without bound.
const metadataTimeout = 10 * time.Second

// proxyGET forwards a small read-only request.
//
// path is built by the caller from routed, validated components — never from r.URL.Path — because
// the gateway signs this request with its own upstream credential.
func (s *Server) proxyGET(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	surfaceName, path string) {

	release, err := s.deps.Jobs.AdmitMetadata(surfaceName)
	if err != nil {
		azerr.TooBusy(surface, s.deps.Config.BusyRetryAfter).WriteTo(w, surface)
		return
	}
	defer release()

	client, ok := s.deps.Jobs.Analyzer(surfaceName).(passthroughClient)
	if !ok {
		azerr.Internal(surface, "This surface does not support metadata proxying.").
			WriteTo(w, surface)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), metadataTimeout)
	defer cancel()

	resp, err := client.PassthroughGET(ctx, path, upstreamQuery(r.URL.Query()))
	if err != nil {
		logging.From(r.Context()).Warn("metadata proxy failed", "path", path, "error", err)
		azerr.Internal(surface, "The upstream container is unreachable.").WriteTo(w, surface)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	s.relaySync(w, r, surface, resp)
}

// copyResponse relays an upstream response to the client.
//
// Hop-by-hop headers are dropped, and Operation-Location is dropped rather than relayed: the
// container mints it from the inbound Host and Referer, so it can name an address the client
// cannot reach, and the Read container corrupts it outright when the authority contains the
// substring "vision". Synchronous routes have no operation to poll, so there is nothing to
// replace it with.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	dst := w.Header()
	for k, vs := range resp.Header {
		if skipRelayHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func skipRelayHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"Operation-Location", "Location", "Set-Cookie":
		return true
	}
	return false
}

// passthroughClient is the subset of an upstream client the proxy paths need.
type passthroughClient interface {
	Passthrough(ctx context.Context, method, path string, q url.Values, contentType string,
		body io.Reader, contentLength int64) (*http.Response, error)
	PassthroughGET(ctx context.Context, path string, q url.Values) (*http.Response, error)
}
