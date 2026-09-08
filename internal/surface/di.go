package surface

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

func (s *Server) registerDI(mux *http.ServeMux) {
	// The colon action is part of the final path segment, so {action} captures
	// "prebuilt-layout:analyze" whole and the verb is split off below.
	mux.HandleFunc("POST "+diPrefix+"/documentModels/{action}", s.diAction)

	mux.HandleFunc("GET "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diPoll)
	mux.HandleFunc("HEAD "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diPoll)
	mux.HandleFunc("DELETE "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}", s.diDelete)
	mux.HandleFunc("GET "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}/pdf", s.diPDF)
	mux.HandleFunc("GET "+diPrefix+"/documentModels/{modelId}/analyzeResults/{resultId}/figures/{figureId}", s.diFigure)

	mux.HandleFunc("GET "+diPrefix+"/info", s.diMetadata)
	mux.HandleFunc("GET "+diPrefix+"/documentModels", s.diMetadata)
	mux.HandleFunc("GET "+diPrefix+"/documentModels/{modelId}", s.diMetadata)
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
	s.submit(w, r, submitParams{
		surface:       azerr.SurfaceDI,
		surfaceName:   jobs.SurfaceDI,
		modelID:       modelID,
		apiVersion:    apiVersion,
		upstreamQuery: upstreamQuery(r.URL.Query()),
		operationLocation: func(base, id string) string {
			return diOperationLocation(base, modelID, id, apiVersion)
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
func diOperationLocation(base, modelID, resultID, apiVersion string) string {
	q := url.Values{}
	q.Set("api-version", apiVersion)
	return base + diPrefix + "/documentModels/" + url.PathEscape(modelID) +
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
	s.passthrough(w, r, azerr.SurfaceDI, jobs.SurfaceDI,
		diPrefix+"/documentModels/"+url.PathEscape(modelID)+":syncAnalyze")
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

// diMetadata proxies the read-only model and capability endpoints.
func (s *Server) diMetadata(w http.ResponseWriter, r *http.Request) {
	s.proxyGET(w, r, azerr.SurfaceDI, jobs.SurfaceDI, r.URL.Path)
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

// passthrough streams a synchronous request to the container and the response back.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	surfaceName, path string) {

	log := logging.From(r.Context())

	release, err := s.deps.Jobs.Admit(surfaceName)
	if err != nil {
		azerr.TooBusy(surface, s.deps.Config.BusyRetryAfter).WriteTo(w, surface)
		return
	}
	defer release()

	client, ok := s.deps.Jobs.Analyzer(surfaceName).(passthroughClient)
	if !ok {
		azerr.Internal(surface, "This surface does not support synchronous passthrough.").
			WriteTo(w, surface)
		return
	}

	body := http.MaxBytesReader(w, r.Body, s.deps.Config.MaxRequestBytes)
	resp, err := client.Passthrough(r.Context(), r.Method, path, upstreamQuery(r.URL.Query()),
		r.Header.Get("Content-Type"), body, r.ContentLength)
	if err != nil {
		log.Warn("synchronous passthrough failed", "path", path, "error", err)
		azerr.Internal(surface, "The upstream container is unreachable.").WriteTo(w, surface)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyResponse(w, resp)
}

// proxyGET forwards a small read-only request and returns the response verbatim.
func (s *Server) proxyGET(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	surfaceName, path string) {

	client, ok := s.deps.Jobs.Analyzer(surfaceName).(passthroughClient)
	if !ok {
		azerr.Internal(surface, "This surface does not support metadata proxying.").
			WriteTo(w, surface)
		return
	}
	resp, err := client.PassthroughGET(r.Context(), path, upstreamQuery(r.URL.Query()))
	if err != nil {
		logging.From(r.Context()).Warn("metadata proxy failed", "path", path, "error", err)
		azerr.Internal(surface, "The upstream container is unreachable.").WriteTo(w, surface)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyResponse(w, resp)
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
