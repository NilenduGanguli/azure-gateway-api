// Package surface serves the client-facing Azure-compatible HTTP surfaces.
//
// Everything here exists to be indistinguishable from the containers it fronts. Where a choice
// looks arbitrary it is usually load-bearing for one particular SDK, and the comment says which.
package surface

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/httpx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/logging"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

// Deps are the collaborators both surfaces need.
type Deps struct {
	Config  *config.Config
	Store   *store.Store
	Jobs    *jobs.Manager
	BaseURL httpx.BaseURLResolver
	Now     func() time.Time
}

// Server serves both Azure surfaces.
type Server struct {
	deps Deps
}

// New builds a Server.
func New(d Deps) *Server {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Server{deps: d}
}

// Register wires both surfaces onto a mux.
func (s *Server) Register(mux *http.ServeMux) {
	s.registerDI(mux)
	s.registerRead(mux)
}

// envelope is the operation-status body both surfaces return from a poll.
//
// The member order matches what the containers emit. Only fields that are set appear, because a
// null analyzeResult on an in-progress poll would make .NET's AnalyzeResult.FromLroResponse throw
// when it reaches for the property.
type envelope struct {
	Status              string          `json:"status"`
	CreatedDateTime     string          `json:"createdDateTime"`
	LastUpdatedDateTime string          `json:"lastUpdatedDateTime"`
	Error               *azerr.Error    `json:"error,omitempty"`
	AnalyzeResult       json.RawMessage `json:"analyzeResult,omitempty"`
}

// submitParams describes one accepted submission.
type submitParams struct {
	surface     azerr.Surface
	surfaceName string
	modelID     string
	apiVersion  string
	// upstreamQuery is what gets forwarded to the container.
	upstreamQuery url.Values
	// operationLocation builds the client-facing poll URL for a new operation id.
	operationLocation func(base, id string) string
}

// submit is the shared async-accept path for both surfaces.
//
// The ordering is the durability contract and must not be rearranged: the input document is
// fsynced and the job row is committed before the 202 is written. Acknowledging first would hand
// a client an operation id that a crash could erase.
func (s *Server) submit(w http.ResponseWriter, r *http.Request, p submitParams) {
	log := logging.From(r.Context())

	base := s.deps.BaseURL.Resolve(r)
	if base == "" {
		azerr.Internal(p.surface,
			"The gateway cannot determine its own public address; set PUBLIC_BASE_URL.").
			WriteTo(w, p.surface)
		return
	}

	// Refuse cleanly while there is still room to write an error, rather than failing partway
	// through storing a document.
	if s.deps.Jobs.DiskPressure() {
		azerr.Unavailable(p.surface, "The gateway is temporarily out of storage. Try again.",
			s.deps.Config.BusyRetryAfter).WriteTo(w, p.surface)
		return
	}

	release, err := s.deps.Jobs.Admit(p.surfaceName)
	if err != nil {
		// Every upstream slot is busy. The containers themselves never emit 429 — they do not cap
		// TPS — so this is the gateway's own backpressure, and it always carries Retry-After
		// because the JS SDK only retries a 429 when one of the retry-after headers is present.
		azerr.TooBusy(p.surface, s.deps.Config.BusyRetryAfter).WriteTo(w, p.surface)
		return
	}
	committed := false
	defer func() {
		if !committed {
			release()
		}
	}()

	id := ids.New()
	now := s.deps.Now()

	written, err := s.deps.Store.Blob.WriteFrom(id, store.KindInput, r.Body,
		s.deps.Config.MaxRequestBytes)
	if err != nil {
		_ = s.deps.Store.Blob.RemoveAll(id)
		if errors.Is(err, store.ErrLimitExceeded) {
			azerr.ContentTooLarge(p.surface).WriteTo(w, p.surface)
			return
		}
		log.Error("could not store submitted document", "job", id, "error", err)
		azerr.Unavailable(p.surface, "The submitted document could not be stored.",
			s.deps.Config.BusyRetryAfter).WriteTo(w, p.surface)
		return
	}

	job := &store.Job{
		ID:          id,
		Surface:     p.surfaceName,
		ModelID:     p.modelID,
		APIVersion:  p.apiVersion,
		Status:      store.StatusNotStarted,
		CreatedAt:   now,
		UpdatedAt:   now,
		ExpiresAt:   now.Add(s.deps.Config.ResultTTL),
		Query:       p.upstreamQuery.Encode(),
		ContentType: r.Header.Get("Content-Type"),
		InputPath:   s.deps.Store.Blob.Path(id, store.KindInput),
		InputBytes:  written,
		ClientReqID: r.Header.Get(httpx.HeaderClientRequestID),
	}
	if err := s.deps.Store.Create(r.Context(), job); err != nil {
		_ = s.deps.Store.Blob.RemoveAll(id)
		log.Error("could not create job", "job", id, "error", err)
		azerr.Unavailable(p.surface, "The request could not be accepted.",
			s.deps.Config.BusyRetryAfter).WriteTo(w, p.surface)
		return
	}

	if err := s.deps.Jobs.Enqueue(p.surfaceName, id); err != nil {
		// The row is committed, so the operation id is valid and the recovery pass will pick it
		// up. Rather than lie about capacity, the client is told to retry.
		log.Error("could not enqueue job", "job", id, "error", err)
		azerr.TooBusy(p.surface, s.deps.Config.BusyRetryAfter).WriteTo(w, p.surface)
		return
	}
	committed = true

	// Response shape, in order of what breaks if it is wrong:
	//   - exactly 202. Every SDK family treats any other 2xx as a failure.
	//   - Operation-Location, absolute, in this surface's exact format.
	//   - no Location header: Python and JS would issue an extra final GET to it after success
	//     and then parse that response as the analyze result.
	//   - integer-seconds Retry-After: without it azure-core waits its 30s default between polls.
	w.Header().Set("Operation-Location", p.operationLocation(base, id))
	httpx.SetRetryAfter(w, s.deps.Config.PollRetryAfter)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)

	log.Info("accepted operation", "job", id, "surface", p.surfaceName, "bytes", written)
}

// poll serves an operation's current state.
func (s *Server) poll(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	surfaceName, rawID string) {

	id, ok := ids.Normalize(rawID)
	if !ok {
		azerr.NotFound(surface).WriteTo(w, surface)
		return
	}

	job, err := s.deps.Store.Get(r.Context(), id)
	if err != nil || job.Surface != surfaceName {
		// An unknown id is a genuine 404, exactly as Azure answers an expired resultId. A live id
		// is never 404'd: that would kill Java clients with an opaque NullPointerException and
		// mark the operation terminally failed in .NET and JS.
		azerr.NotFound(surface).WriteTo(w, surface)
		return
	}
	if !job.ExpiresAt.IsZero() && s.deps.Now().After(job.ExpiresAt) {
		azerr.NotFound(surface).WriteTo(w, surface)
		return
	}

	switch job.Status {
	case store.StatusSucceeded:
		s.streamResult(w, r, surface, job)

	case store.StatusFailed:
		env := envelope{
			Status:              string(store.StatusFailed),
			CreatedDateTime:     job.CreatedAt.Format(jobs.TimeFormat),
			LastUpdatedDateTime: job.UpdatedAt.Format(jobs.TimeFormat),
		}
		// A failed operation is reported at HTTP 200 with status "failed", not as an HTTP error.
		// That is how both containers behave, and it is the dominant failure mode on both
		// surfaces: code switching on HTTP status alone mis-maps every analysis failure.
		//
		// The Read schema models no error member for a failed operation. One is included anyway:
		// generated SDK models ignore unknown properties, and a failure with no diagnostics at
		// all is exactly the dead end the container's own {"status":"Failed"} creates.
		if job.ErrorJSON != "" {
			var e azerr.Error
			if json.Unmarshal([]byte(job.ErrorJSON), &e) == nil && e.Code != "" {
				env.Error = &e
			}
		}
		httpx.WriteJSON(w, http.StatusOK, env)

	default:
		// notStarted and running both report as running-in-progress with a pacing hint.
		httpx.SetRetryAfter(w, s.deps.Config.PollRetryAfter)
		httpx.WriteJSON(w, http.StatusOK, envelope{
			Status:              string(job.Status),
			CreatedDateTime:     job.CreatedAt.Format(jobs.TimeFormat),
			LastUpdatedDateTime: job.UpdatedAt.Format(jobs.TimeFormat),
		})
	}
}

// streamResult sends the stored envelope verbatim.
//
// The bytes were composed when the job completed, so this is a plain file copy with an exact
// Content-Length: no re-serialisation of a result Azure permits to be 500 MB.
func (s *Server) streamResult(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	job *store.Job) {

	f, size, err := s.deps.Store.Blob.Open(job.ID, store.KindResult)
	if err != nil {
		logging.From(r.Context()).Error("result blob missing for a succeeded job",
			"job", job.ID, "error", err)
		azerr.Internal(surface, "The stored result could not be read.").WriteTo(w, surface)
		return
	}
	defer func() { _ = f.Close() }()

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		// The client hung up mid-body. Nothing can be sent now; the header is already written.
		logging.From(r.Context()).Warn("result stream interrupted", "job", job.ID, "error", err)
	}
}

// deleteResult purges an operation early, mirroring Azure's DELETE on a result.
func (s *Server) deleteResult(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	surfaceName, rawID string) {

	id, ok := ids.Normalize(rawID)
	if !ok {
		azerr.NotFound(surface).WriteTo(w, surface)
		return
	}
	job, err := s.deps.Store.Get(r.Context(), id)
	if err != nil || job.Surface != surfaceName {
		azerr.NotFound(surface).WriteTo(w, surface)
		return
	}
	if err := s.deps.Store.Delete(r.Context(), id); err != nil {
		logging.From(r.Context()).Error("could not delete result", "job", id, "error", err)
		azerr.Internal(surface, "The result could not be deleted.").WriteTo(w, surface)
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// methodGuard rejects a verb the route does not serve, in the surface's own error shape rather
// than Go's default plain-text 405.
func methodGuard(surface azerr.Surface, allowed string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != allowed && !(allowed == http.MethodGet && r.Method == http.MethodHead) {
			w.Header().Set("Allow", allowed)
			azerr.MethodNotAllowed(surface).WriteTo(w, surface)
			return
		}
		next(w, r)
	}
}
