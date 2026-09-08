package surface

import (
	"net/http"
	"net/url"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

const readPrefix = upstream.ReadPathPrefix

func (s *Server) registerRead(mux *http.ServeMux) {
	mux.HandleFunc("POST "+readPrefix+"/analyze", s.readAnalyze)
	mux.HandleFunc("POST "+readPrefix+"/syncAnalyze", s.readSyncAnalyze)
	mux.HandleFunc("GET "+readPrefix+"/analyzeResults/{operationId}", s.readPoll)
	mux.HandleFunc("HEAD "+readPrefix+"/analyzeResults/{operationId}", s.readPoll)
}

// readAnalyze accepts an asynchronous read.
func (s *Server) readAnalyze(w http.ResponseWriter, r *http.Request) {
	s.submit(w, r, submitParams{
		surface:           azerr.SurfaceRead,
		surfaceName:       jobs.SurfaceRead,
		apiVersion:        r.URL.Query().Get("model-version"),
		upstreamQuery:     upstreamQuery(r.URL.Query()),
		operationLocation: readOperationLocation,
	})
}

// readOperationLocation builds the poll URL for a Computer Vision Read operation.
//
// Unlike Document Intelligence, this one must be bare: no query string, no trailing slash, no
// fragment, ending in a value that parses as a GUID. The Read SDK has no poller at all, so
// callers follow Microsoft's canonical samples, which do operationLocation.split("/")[-1] in
// Python and operationLocation.Substring(len-36) + Guid.Parse in .NET. A query string turns the
// Python form into "<guid>?model-version=…", which the client then percent-encodes into the path
// and 404s on, and makes the .NET form throw.
func readOperationLocation(base, id string) string {
	return base + readPrefix + "/analyzeResults/" + url.PathEscape(id)
}

// readSyncAnalyze passes a synchronous read straight through.
//
// This is the one genuinely stateless call available against either container: it mints no
// operation id, so it load-balances correctly across replicas with no affinity and no shared
// result store.
func (s *Server) readSyncAnalyze(w http.ResponseWriter, r *http.Request) {
	s.passthrough(w, r, azerr.SurfaceRead, jobs.SurfaceRead, upstream.Request{
		Query: upstreamQuery(r.URL.Query()),
	})
}

func (s *Server) readPoll(w http.ResponseWriter, r *http.Request) {
	s.poll(w, r, azerr.SurfaceRead, jobs.SurfaceRead, r.PathValue("operationId"))
}
