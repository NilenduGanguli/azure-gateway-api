// Package mockazure provides faithful in-process fakes of the two Azure containers.
//
// They reproduce the containers' failure modes as well as their happy paths, because the failure
// modes are what this gateway exists to absorb: a synchronous route that is absent on some image
// builds, one that degrades to 202 under load, and polls that 404 because they reached a replica
// which never saw the operation. Code that is never exercised against those cases is code that
// has not been tested.
package mockazure

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SyncBehavior selects how a container answers its synchronous analyze route.
type SyncBehavior int

const (
	// Sync200 returns the result inline: the documented Read behaviour and the best case for DI.
	Sync200 SyncBehavior = iota
	// Sync202 degrades to an asynchronous operation, as the DI container has been observed doing
	// under memory pressure.
	Sync202
	// Sync404 omits the route entirely, the way an image build without it does: Kestrel answers an
	// unrouted path with a bodyless 404, not with a Document Intelligence error object.
	Sync404
	// Sync500Unhandled answers with the UnhandledEndpointException older builds return instead of
	// a clean 404.
	Sync500Unhandled
	// SyncModelNotFound serves the route but rejects the model, which is request-scoped.
	SyncModelNotFound
)

// Options configures a fake container.
type Options struct {
	// Sync selects the synchronous route's behaviour.
	Sync SyncBehavior
	// SyncBodyBare returns a bare AnalyzeResult from the synchronous route instead of wrapping it
	// in an operation envelope. Which shape the real DI container returns is unverified, so both
	// are testable.
	SyncBodyBare bool
	// PollsBeforeSuccess is how many polls report "running" before the operation completes.
	PollsBeforeSuccess int
	// WrongReplicaEvery makes every Nth poll answer 404, simulating a round-robin route landing
	// on a replica with no record of the operation. Zero disables it.
	WrongReplicaEvery int
	// FailAnalysis makes the operation terminate as failed.
	FailAnalysis bool
	// FailSyncBare makes the synchronous route answer with the bare {"status":"Failed"} body that
	// CV syncAnalyze produces — capital F, no code, no message.
	FailSyncBare bool
	// PadResultBytes inflates the result so streaming paths are exercised with a large body.
	PadResultBytes int
	// Latency delays each analyze call.
	Latency time.Duration
	// StatusUnhealthy makes /status report an invalid key.
	StatusUnhealthy bool
	// RequireAPIKey rejects calls without Ocp-Apim-Subscription-Key.
	RequireAPIKey string
	// SyncErrorStatus, when non-zero, makes the synchronous route fail with this status and an
	// unparseable body — HTML, as an nginx sidecar produces — plus a Retry-After the gateway must
	// not relay onto a non-retriable status.
	SyncErrorStatus int
}

// Container is a fake Azure container.
type Container struct {
	opts   Options
	server *httptest.Server

	mu   sync.Mutex
	ops  map[string]*operation
	seq  atomic.Int64
	hits atomic.Int64

	// Requests records every path served, for assertions about which route was used.
	reqMu    sync.Mutex
	requests []string
}

type operation struct {
	polls  int
	failed bool
}

// URL returns the container's base URL.
func (c *Container) URL() string { return c.server.URL }

// Close shuts the container down.
func (c *Container) Close() { c.server.Close() }

// Hits reports how many analyze calls were served.
func (c *Container) Hits() int64 { return c.hits.Load() }

// Requests returns the paths served so far.
func (c *Container) Requests() []string {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	return append([]string(nil), c.requests...)
}

// Called reports whether any served path contains substr.
func (c *Container) Called(substr string) bool {
	for _, p := range c.Requests() {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

func (c *Container) record(path string) {
	c.reqMu.Lock()
	c.requests = append(c.requests, path)
	c.reqMu.Unlock()
}

// NewDI starts a fake Document Intelligence layout container.
func NewDI(opts Options) *Container {
	c := &Container{opts: opts, ops: map[string]*operation{}}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /documentintelligence/documentModels/{action}", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		if !c.authorized(w, r) {
			return
		}
		_, verb, _ := strings.Cut(r.PathValue("action"), ":")
		switch verb {
		case "syncAnalyze":
			c.serveSync(w, r, true)
		case "analyze":
			c.serveSubmit(w, r, "/documentintelligence/documentModels/prebuilt-layout/analyzeResults/")
		default:
			writeDIError(w, http.StatusNotFound, "NotFound", "Resource not found.")
		}
	})

	mux.HandleFunc("GET /documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}",
		func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			c.servePoll(w, r.PathValue("resultId"), true)
		})

	mux.HandleFunc("GET /documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}/pdf",
		func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.7\n% fake searchable pdf\n"))
		})

	mux.HandleFunc("GET /documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}/figures/{figureId}",
		func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nfake-figure-" + r.PathValue("figureId")))
		})

	mux.HandleFunc("GET /documentintelligence/info", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		writeJSON(w, http.StatusOK, map[string]any{
			"customDocumentModels": map[string]int{"count": 0, "limit": 20000},
		})
	})
	mux.HandleFunc("GET /documentintelligence/documentModels", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		writeJSON(w, http.StatusOK, map[string]any{
			"value": []map[string]string{{"modelId": "prebuilt-layout"}},
		})
	})

	c.addCommon(mux)
	c.server = httptest.NewServer(mux)
	return c
}

// NewRead starts a fake Computer Vision Read v3.2 container.
func NewRead(opts Options) *Container {
	c := &Container{opts: opts, ops: map[string]*operation{}}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /vision/v3.2/read/syncAnalyze", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		if !c.authorized(w, r) {
			return
		}
		c.serveSync(w, r, false)
	})

	mux.HandleFunc("POST /vision/v3.2/read/analyze", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		if !c.authorized(w, r) {
			return
		}
		c.serveSubmit(w, r, "/vision/v3.2/read/analyzeResults/")
	})

	mux.HandleFunc("GET /vision/v3.2/read/analyzeResults/{operationId}", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		c.servePoll(w, r.PathValue("operationId"), false)
	})

	c.addCommon(mux)
	c.server = httptest.NewServer(mux)
	return c
}

func (c *Container) addCommon(mux *http.ServeMux) {
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		if c.opts.StatusUnhealthy {
			writeJSON(w, http.StatusOK, map[string]string{
				"apiStatus": "Invalid", "apiStatusMessage": "Subscription validation failed.",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"apiStatus": "Valid", "apiStatusMessage": "Api key is valid.",
		})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ready": "ready"})
	})
}

func (c *Container) authorized(w http.ResponseWriter, r *http.Request) bool {
	if c.opts.RequireAPIKey == "" {
		return true
	}
	if r.Header.Get("Ocp-Apim-Subscription-Key") == c.opts.RequireAPIKey {
		return true
	}
	writeDIError(w, http.StatusUnauthorized, "Unauthorized", "Access denied due to invalid subscription key.")
	return false
}

// serveSync answers the synchronous analyze route according to the configured behaviour.
func (c *Container) serveSync(w http.ResponseWriter, r *http.Request, di bool) {
	c.hits.Add(1)
	_, _ = io.Copy(io.Discard, r.Body)
	if c.opts.Latency > 0 {
		time.Sleep(c.opts.Latency)
	}

	if c.opts.SyncErrorStatus != 0 {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(c.opts.SyncErrorStatus)
		_, _ = w.Write([]byte("<html><head><title>413 Request Entity Too Large</title></head></html>"))
		return
	}

	switch c.opts.Sync {
	case Sync404:
		// Kestrel's unrouted response: no body at all. This is what distinguishes "this build does
		// not serve the route" from "this build serves it and your model does not exist".
		w.WriteHeader(http.StatusNotFound)
		return
	case SyncModelNotFound:
		// The route exists; the model does not. Request-scoped, so it must not disable the route.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{
				"code": "NotFound", "message": "Resource not found.",
				"innererror": map[string]string{
					"code":    "ModelNotFound",
					"message": "The requested model wasn't found. It was deleted or still building.",
				},
			},
		})
		return
	case Sync500Unhandled:
		writeDIError(w, http.StatusInternalServerError, "InternalServerError",
			"UnhandledEndpointException: no candidates found for the request path.")
		return
	case Sync202:
		// Degrade to an operation only this replica knows about.
		id := c.newOperation()
		prefix := "/vision/v3.2/read/analyzeResults/"
		if di {
			prefix = "/documentintelligence/documentModels/prebuilt-layout/analyzeResults/"
		}
		w.Header().Set("Operation-Location", c.server.URL+prefix+id)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if c.opts.FailSyncBare {
		// The only error form Microsoft documents for CV syncAnalyze: capital F, no detail.
		writeJSON(w, http.StatusOK, map[string]string{"status": "Failed"})
		return
	}
	if c.opts.FailAnalysis {
		writeJSON(w, http.StatusOK, c.failedEnvelope())
		return
	}
	if c.opts.SyncBodyBare {
		writeJSON(w, http.StatusOK, c.analyzeResult())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":              "succeeded",
		"createdDateTime":     "2024-11-30T00:00:00Z",
		"lastUpdatedDateTime": "2024-11-30T00:00:01Z",
		"analyzeResult":       c.analyzeResult(),
	})
}

// serveSubmit answers the asynchronous analyze route.
func (c *Container) serveSubmit(w http.ResponseWriter, r *http.Request, resultPrefix string) {
	c.hits.Add(1)
	_, _ = io.Copy(io.Discard, r.Body)
	id := c.newOperation()
	w.Header().Set("Operation-Location", c.server.URL+resultPrefix+id)
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusAccepted)
}

// servePoll answers a poll, optionally pretending to be the wrong replica.
func (c *Container) servePoll(w http.ResponseWriter, id string, di bool) {
	c.mu.Lock()
	op, ok := c.ops[id]
	if !ok {
		c.mu.Unlock()
		writeDIError(w, http.StatusNotFound, "NotFound", "Resource not found.")
		return
	}
	op.polls++
	polls := op.polls
	c.mu.Unlock()

	if c.opts.WrongReplicaEvery > 0 && polls%c.opts.WrongReplicaEvery == 0 {
		// A replica that never saw this operation: the containers' default result store is
		// instance-local, so a round-robin route produces exactly this.
		writeDIError(w, http.StatusNotFound, "NotFound", "Resource not found.")
		return
	}
	if polls <= c.opts.PollsBeforeSuccess {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusOK, map[string]any{
			"status":              "running",
			"createdDateTime":     "2024-11-30T00:00:00Z",
			"lastUpdatedDateTime": "2024-11-30T00:00:01Z",
		})
		return
	}
	if c.opts.FailAnalysis {
		writeJSON(w, http.StatusOK, c.failedEnvelope())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":              "succeeded",
		"createdDateTime":     "2024-11-30T00:00:00Z",
		"lastUpdatedDateTime": "2024-11-30T00:00:02Z",
		"analyzeResult":       c.analyzeResult(),
	})
}

func (c *Container) newOperation() string {
	id := fmt.Sprintf("%08x-0000-4000-8000-%012x", c.seq.Add(1), c.seq.Load())
	c.mu.Lock()
	c.ops[id] = &operation{}
	c.mu.Unlock()
	return id
}

func (c *Container) failedEnvelope() map[string]any {
	return map[string]any{
		"status":              "failed",
		"createdDateTime":     "2024-11-30T00:00:00Z",
		"lastUpdatedDateTime": "2024-11-30T00:00:02Z",
		"error": map[string]any{
			"code":    "InvalidRequest",
			"message": "Invalid request.",
			"innererror": map[string]string{
				"code":    "InvalidContent",
				"message": "The file is corrupted or format is unsupported.",
			},
		},
	}
}

// analyzeResult builds a result body, padded when the test wants a large one.
func (c *Container) analyzeResult() map[string]any {
	out := map[string]any{
		"apiVersion":      "2024-11-30",
		"modelId":         "prebuilt-layout",
		"stringIndexType": "textElements",
		"content":         "mock content",
		"pages": []map[string]any{
			{"pageNumber": 1, "angle": 0, "width": 8.5, "height": 11, "unit": "inch"},
		},
		"figures": []map[string]any{
			{"id": "1.1", "boundingRegions": []map[string]any{{"pageNumber": 1}}},
		},
	}
	if c.opts.PadResultBytes > 0 {
		out["padding"] = strings.Repeat("x", c.opts.PadResultBytes)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeDIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}
