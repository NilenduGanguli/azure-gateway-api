package surface

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/httpx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/logging"
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
		// Model-lifecycle operations. A layout container has none, but it routes them, so a
		// gateway that did not would answer a client differently from the pod.
		mux.HandleFunc("GET "+fam+"/operations", s.diListOperations)
		mux.HandleFunc("GET "+fam+"/operations/{operationId}", s.diGetOperation)
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
	// Before admission, so a refused request stores nothing.
	if err := checkDIParams(r.URL.Query()); err != nil {
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

// diSyncAnalyze serves a synchronous analysis through the engine; see syncAnalyze.
func (s *Server) diSyncAnalyze(w http.ResponseWriter, r *http.Request, modelID string) {
	if _, err := requireAPIVersion(r); err != nil {
		err.WriteTo(w, azerr.SurfaceDI)
		return
	}
	if err := checkDIParams(r.URL.Query()); err != nil {
		err.WriteTo(w, azerr.SurfaceDI)
		return
	}
	s.syncAnalyze(w, r, azerr.SurfaceDI, jobs.SurfaceDI, modelID, prefixOf(r))
}

func (s *Server) diPoll(w http.ResponseWriter, r *http.Request) {
	s.poll(w, r, azerr.SurfaceDI, jobs.SurfaceDI, r.PathValue("resultId"))
}

func (s *Server) diDelete(w http.ResponseWriter, r *http.Request) {
	s.deleteResult(w, r, azerr.SurfaceDI, jobs.SurfaceDI, r.PathValue("resultId"))
}

// diPDF answers the searchable-PDF route of an operation.
func (s *Server) diPDF(w http.ResponseWriter, r *http.Request) { s.diArtifact(w, r) }

// diFigure answers the cropped-figure route of an operation.
func (s *Server) diFigure(w http.ResponseWriter, r *http.Request) { s.diArtifact(w, r) }

// diArtifact answers a result-file route, which never has a file to serve.
//
// The container addresses these files by an operation id that only its asynchronous :analyze
// mints, and the gateway only ever makes synchronous calls, so no job has them: output=pdf and
// output=figures are refused at submit for that reason. The routes stay so a client that asks
// anyway gets the 404 a container gives for a result file it did not produce, rather than the
// bodyless 404 of a path the container does not route. An unknown or expired id is answered as
// an unknown operation, as a poll is.
func (s *Server) diArtifact(w http.ResponseWriter, r *http.Request) {
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
	azerr.New(http.StatusNotFound, azerr.CodeNotFound,
		"The requested result file was not produced for this operation.").
		WithInner(azerr.InnerOperationNotFound, "").
		WriteTo(w, azerr.SurfaceDI)
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

// diListOperations proxies GET {family}/operations.
func (s *Server) diListOperations(w http.ResponseWriter, r *http.Request) {
	s.proxyGET(w, r, azerr.SurfaceDI, jobs.SurfaceDI, prefixOf(r)+"/operations")
}

// diGetOperation proxies GET {family}/operations/{operationId}, re-escaping the routed id for the
// reason diGetModel gives.
func (s *Server) diGetOperation(w http.ResponseWriter, r *http.Request) {
	s.proxyGET(w, r, azerr.SurfaceDI, jobs.SurfaceDI,
		prefixOf(r)+"/operations/"+url.PathEscape(r.PathValue("operationId")))
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
				relayHeader(w.Header(), k, vs)
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
			parsed.RetryAfter = config.RetryAfterBusy
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
		relayHeader(dst, k, vs)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// relayHeader gives the client the container's own values for one header, replacing any the
// gateway set first.
//
// Middleware writes Server, Cache-Control and the x-ms-client-request-id echo before any handler
// runs, and a real container sends Server and the echo too. Appending to them sent each header
// twice, which no pod does, and the spec asks for these routes to be relayed verbatim.
//
// x-ms-client-request-id is the exception. It is the client's own correlation id, echoed by the
// middleware, and the gateway's call to the container does not carry it, so whatever the container
// sends under that name is not the client's id and is dropped.
func relayHeader(dst http.Header, k string, vs []string) {
	if http.CanonicalHeaderKey(k) == http.CanonicalHeaderKey(httpx.HeaderClientRequestID) {
		return
	}
	dst.Del(k)
	for _, v := range vs {
		dst.Add(k, v)
	}
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
