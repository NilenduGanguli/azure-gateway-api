package surface

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/engine"
	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/logging"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
)

// syncAnalyze serves a client's synchronous analyze through the engine.
//
// It is owned rather than relayed, for the reasons :analyze is: the engine retries what HAProxy
// and an autoscaled pod throw at a call, and splits a large PDF, and a blind relay would hand the
// client every transient 503 and every part-sized timeout instead. The body is saved first,
// because every retry and every part re-reads it, and the client's own stream can be read once.
//
// Nothing outlives the request: the saved body and every file the engine wrote are removed on
// return, and no job row is created, because there is no operation id to honour afterwards.
func (s *Server) syncAnalyze(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	surfaceName, modelID, prefix string) {

	log := logging.From(r.Context())
	eng := s.deps.Jobs.Engine(surfaceName)
	if eng == nil {
		// Unreachable while routes are registered only for configured surfaces.
		azerr.Unrouted(surface).WriteTo(w, surface)
		return
	}

	release, err := s.deps.Jobs.Admit(surfaceName)
	if err != nil {
		azerr.TooBusy(surface, config.RetryAfterBusy).WriteTo(w, surface)
		return
	}
	defer release()

	received := s.deps.Now()
	id := "sync-" + ids.New()
	untrack := s.deps.Jobs.Track(id)
	defer untrack()
	dir := filepath.Join(jobs.WorkRoot(s.deps.Store.DataDir()), id)
	defer func() { _ = os.RemoveAll(dir) }()

	// The slot is already held, so a body that never finishes arriving would hold it forever.
	s.setUploadDeadline(w, r)

	input := filepath.Join(dir, "input")
	size, err := saveBody(input, r.Body, s.deps.Config.MaxRequestBytes)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrLimitExceeded):
			azerr.ContentTooLarge(surface).WriteTo(w, surface)
		case r.Context().Err() != nil:
			log.Info("client left while its document was arriving", "error", err)
		default:
			log.Warn("could not store a synchronous request's document", "error", err)
			azerr.Unavailable(surface, "The submitted document could not be stored.",
				config.RetryAfterBusy).WriteTo(w, surface)
		}
		return
	}

	out, err := eng.Run(r.Context(), engine.Input{
		JobID:       id,
		ContentType: r.Header.Get("Content-Type"),
		Path:        input,
		Size:        size,
		ModelID:     modelID,
		Prefix:      prefix,
		Query:       upstreamQuery(r.URL.Query()),
	})
	if err != nil {
		if out != nil {
			out.Cleanup()
		}
		s.writeSyncError(w, r, surface, err)
		return
	}
	defer out.Cleanup()
	s.writeSyncResult(w, r, surface, out, received)
}

// writeSyncError answers a failed synchronous analysis.
//
// A cancellation after the client has gone writes nothing: there is nobody to read it, and the
// log line is the only record worth keeping.
func (s *Server) writeSyncError(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	err error) {

	log := logging.From(r.Context())
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if r.Context().Err() != nil {
			log.Info("synchronous analysis abandoned by the client", "error", err)
			return
		}
		// The engine's own deadline, with the client still waiting. It is told the analysis took
		// too long, at a status that is safe to retry.
		log.Warn("synchronous analysis ran out of time", "error", err)
		azerr.ForStatus(surface, http.StatusGatewayTimeout,
			"The analysis did not complete in time.").WriteTo(w, surface)
		return
	}
	apiErr, ok := azerr.AsAPIError(err)
	if !ok {
		// Logged, never returned: a raw error can name PVC paths and internal addresses.
		log.Error("synchronous analysis failed with an unmapped error", "error", err)
		apiErr = azerr.Internal(surface, "")
	}
	apiErr.WriteTo(w, surface)
}

// writeSyncResult answers 200 with the analysis in the shape the container's synchronous route
// gave it: the bare AnalyzeResult when the container sent one bare, otherwise the completed
// operation envelope, stamped with when the request arrived and when it finished.
func (s *Server) writeSyncResult(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	out *engine.Output, received time.Time) {

	f, err := os.Open(out.Path)
	if err != nil {
		logging.From(r.Context()).Error("engine output vanished", "error", err)
		azerr.Internal(surface, "The analysis completed but its result could not be read.").
			WriteTo(w, surface)
		return
	}
	defer func() { _ = f.Close() }()

	var head, tail string
	if !out.Bare {
		head = fmt.Sprintf(
			`{"status":"succeeded","createdDateTime":"%s","lastUpdatedDateTime":"%s","analyzeResult":`,
			received.Format(jobs.TimeFormat), s.deps.Now().Format(jobs.TimeFormat))
		tail = "}"
	}
	size := out.End - out.Start
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.FormatInt(int64(len(head))+size+int64(len(tail)), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, head); err != nil {
		return
	}
	if _, err := io.Copy(w, io.NewSectionReader(f, out.Start, size)); err != nil {
		logging.From(r.Context()).Warn("synchronous result stream interrupted", "error", err)
		return
	}
	_, _ = io.WriteString(w, tail)
}

// saveBody streams a request body to path, refusing more than limit bytes with
// store.ErrLimitExceeded, as the blob store does for :analyze. A limit of zero or less is none.
func saveBody(path string, body io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		limit = math.MaxInt64 - 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return 0, fmt.Errorf("surface: create work folder: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return 0, fmt.Errorf("surface: create input: %w", err)
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(f, io.LimitReader(body, limit+1))
	if err != nil {
		return n, fmt.Errorf("surface: read request body: %w", err)
	}
	if n > limit {
		return n, store.ErrLimitExceeded
	}
	return n, nil
}
