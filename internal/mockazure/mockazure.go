// Package mockazure provides faithful in-process fakes of the two Azure containers.
//
// They reproduce the containers' failure modes as well as their happy paths, because the failure
// modes are what this gateway exists to absorb: a synchronous route that is absent on some image
// builds, one that degrades to 202 under load, and polls that 404 because they reached a replica
// which never saw the operation. Code that is never exercised against those cases is code that
// has not been tested.
//
// The gateway reaches the containers only through HAProxy, over the synchronous route, and
// retries what fails. So the fakes also produce what that path produces: HAProxy's HTML 503 when
// every pod is busy, gateway-class statuses from a pod that is overloaded or being replaced, a
// connection torn down mid-body, and a per-pod concurrency cap. See BusyFirst, StatusFirst,
// ResetMidBody and Capacity.
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
	// SyncHang accepts the request and never answers. A probed layout-4.0 build declares
	// :syncAnalyze in its own swagger and behaves exactly like this, holding the connection open
	// past five minutes on a blank image.
	SyncHang
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
	// MetadataMissing makes /info and /documentModels answer a bodyless 404, as a probed
	// layout-4.0 build does — it declares neither route in its swagger.
	MetadataMissing bool
	// SyncErrorStatus, when non-zero, makes the synchronous route fail with this status and an
	// unparseable body — HTML, as an nginx sidecar produces — plus a Retry-After the gateway must
	// not relay onto a non-retriable status.
	SyncErrorStatus int

	// The next four model what sits between the gateway and the containers: HAProxy in front of
	// autoscaled pods. They apply to synchronous calls only, because that is the only route the
	// gateway uses. Capacity, BusyFirst and StatusFirst are checked in that order, ahead of the
	// container's own behaviour (Sync and the options above), and a call one of them answers
	// never reaches the container, so Latency does not slow it. ResetMidBody then damages the
	// 200 that behaviour sends.

	// BusyFirst answers the first n synchronous calls with 503 and Retry-After: 1, as HAProxy does
	// when every pod is at its connection cap. The body is HAProxy's own page, which is HTML, not
	// an error object a container would send.
	BusyFirst int
	// StatusFirst and StatusFirstCode answer the next n synchronous calls with that status and an
	// error body in the container's own shape. 502, 504, 500, 408 and 429 are the interesting
	// ones: every one of them is transient, so a client is expected to retry. The code must be an
	// error status, and a container sends no Retry-After with one. Each of BusyFirst and
	// StatusFirst counts the calls that reach it, so BusyFirst: 2 with StatusFirst: 1 serves
	// 503, 503, 502 and then the container's normal behaviour.
	StatusFirst     int
	StatusFirstCode int
	// ResetMidBody makes every 200 the synchronous route would send arrive half-written: the
	// headers promise the full Content-Length, half the body follows, and the connection is
	// closed. This is what a client sees when the pod answering it is killed, as scale-in does.
	ResetMidBody bool
	// Capacity answers 503 — HAProxy's HTML page, as BusyFirst does — to a synchronous call that
	// arrives while this many are already in flight; zero means unlimited. A call is in flight
	// from admission until its answer is written, Latency and a hang included, so this models a
	// pod's connection cap (pod-concurrent-connections) and not a fixed number of failures.
	Capacity int
}

// Container is a fake Azure container.
type Container struct {
	opts   Options
	server *httptest.Server

	mu   sync.Mutex
	ops  map[string]*operation
	seq  atomic.Int64
	hits atomic.Int64

	// syncCalls counts synchronous analyze requests as they arrive. inflight is how many of them
	// are being worked on: admitted by Capacity and not yet answered. busyReached and
	// statusReached count the calls that reached the BusyFirst and StatusFirst checks. All are
	// atomics because every request runs in its own goroutine.
	syncCalls     atomic.Int64
	inflight      atomic.Int64
	busyReached   atomic.Int64
	statusReached atomic.Int64

	// Requests records every path served, for assertions about which route was used.
	reqMu    sync.Mutex
	requests []string
}

type operation struct {
	polls int
}

// URL returns the container's base URL.
func (c *Container) URL() string { return c.server.URL }

// Close shuts the container down.
func (c *Container) Close() { c.server.Close() }

// Hits reports how many analyze calls were served.
func (c *Container) Hits() int64 { return c.hits.Load() }

// SyncCalls reports how many synchronous analyze requests were received, whatever became of them:
// a result, a failure mode, a hang, or a rejection for a bad API key. It is the attempt count a
// retry loop is asserted against. Asynchronous submits and polls are not counted; Hits covers both
// kinds of analyze call.
func (c *Container) SyncCalls() int64 { return c.syncCalls.Load() }

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

// validate rejects options that cannot work. A bad StatusFirstCode would otherwise panic inside a
// handler, where the client sees only a dropped connection and no hint of the cause.
func (o Options) validate() {
	if o.StatusFirst > 0 && (o.StatusFirstCode < 400 || o.StatusFirstCode > 599) {
		panic(fmt.Sprintf("mockazure: StatusFirst needs StatusFirstCode to be an error status "+
			"(400-599), got %d", o.StatusFirstCode))
	}
}

// NewDI starts a fake Document Intelligence layout container.
func NewDI(opts Options) *Container {
	opts.validate()
	c := &Container{opts: opts, ops: map[string]*operation{}}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /documentintelligence/documentModels/{action}", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		_, verb, _ := strings.Cut(r.PathValue("action"), ":")
		if verb == "syncAnalyze" {
			// Counted before the key check: SyncCalls is every attempt a client made.
			c.syncCalls.Add(1)
		}
		if !c.authorized(w, r) {
			return
		}
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

	// The legacy Form Recognizer family, which a probed layout-4.0 build declares alongside the
	// newer one.
	mux.HandleFunc("POST /formrecognizer/documentModels/{action}", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		_, verb, _ := strings.Cut(r.PathValue("action"), ":")
		if verb == "syncAnalyze" {
			c.syncCalls.Add(1)
		}
		if !c.authorized(w, r) {
			return
		}
		switch verb {
		case "syncAnalyze":
			c.serveSync(w, r, true)
		case "analyze":
			c.serveSubmit(w, r, "/formrecognizer/documentModels/prebuilt-layout/analyzeResults/")
		default:
			writeDIError(w, http.StatusNotFound, "NotFound", "Resource not found.")
		}
	})
	mux.HandleFunc("GET /formrecognizer/documentModels/{modelId}/analyzeResults/{resultId}",
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

	// Served on both families, as the containers do.
	for _, fam := range []string{"/documentintelligence", "/formrecognizer"} {
		mux.HandleFunc("GET "+fam+"/info", func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			if c.opts.MetadataMissing {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"customDocumentModels": map[string]int{"count": 0, "limit": 20000},
			})
		})
		mux.HandleFunc("GET "+fam+"/documentModels", func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			if c.opts.MetadataMissing {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"value": []map[string]string{{"modelId": "prebuilt-layout"}},
			})
		})
		mux.HandleFunc("GET "+fam+"/documentModels/{modelId}",
			func(w http.ResponseWriter, r *http.Request) {
				c.record(r.URL.Path)
				if c.opts.MetadataMissing {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"modelId":     r.PathValue("modelId"),
					"description": "Prebuilt layout model",
					"apiVersion":  "2024-11-30",
				})
			})
	}

	c.addCommon(mux)
	c.server = httptest.NewServer(mux)
	return c
}

// NewRead starts a fake Computer Vision Read v3.2 container.
func NewRead(opts Options) *Container {
	opts.validate()
	c := &Container{opts: opts, ops: map[string]*operation{}}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /vision/v3.2/read/syncAnalyze", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		c.syncCalls.Add(1)
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
	// The body is read before anything is answered, so a client still uploading meets the answer
	// it is owed and not a connection closed under it.
	_, _ = io.Copy(io.Discard, r.Body)

	// What HAProxy and the autoscaler do to a call happens in front of the container, so it is
	// decided before the container's own behaviour and costs no Latency. The order is the one
	// Options documents.
	if !c.admit() {
		writeHAProxyBusy(w)
		return
	}
	// Given back on every way out: an answer, a hang that ends when the client leaves, a torn
	// connection.
	defer c.inflight.Add(-1)
	if firstN(&c.busyReached, c.opts.BusyFirst) {
		writeHAProxyBusy(w)
		return
	}
	if firstN(&c.statusReached, c.opts.StatusFirst) {
		code, message := containerError(c.opts.StatusFirstCode, di)
		writeDIError(w, c.opts.StatusFirstCode, code, message)
		return
	}

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
	case SyncHang:
		<-r.Context().Done()
		return
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
		c.writeSyncBody(w, map[string]string{"status": "Failed"})
		return
	}
	if c.opts.FailAnalysis {
		c.writeSyncBody(w, c.failedEnvelope())
		return
	}
	if c.opts.SyncBodyBare {
		c.writeSyncBody(w, c.analyzeResult(di))
		return
	}
	c.writeSyncBody(w, map[string]any{
		"status":              "succeeded",
		"createdDateTime":     "2024-11-30T00:00:00Z",
		"lastUpdatedDateTime": "2024-11-30T00:00:01Z",
		"analyzeResult":       c.analyzeResult(di),
	})
}

// admit reserves an in-flight slot for a synchronous call, or reports that the container is at
// Capacity. The check and the reservation are one compare-and-swap, so a call that is turned away
// never inflates the count another call sees and cannot cause that call to be turned away too.
func (c *Container) admit() bool {
	if c.opts.Capacity <= 0 {
		c.inflight.Add(1)
		return true
	}
	for {
		cur := c.inflight.Load()
		if cur >= int64(c.opts.Capacity) {
			return false
		}
		if c.inflight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// firstN reports whether this is one of the first n calls to reach a check, counting arrivals in
// seen. The count is a single atomic add, so n concurrent callers still get exactly n answers.
func firstN(seen *atomic.Int64, n int) bool {
	return n > 0 && seen.Add(1) <= int64(n)
}

// writeSyncBody sends a 200 from the synchronous route. With ResetMidBody it sends only half of
// it and then drops the connection, which is how a client learns the pod answering it was killed.
func (c *Container) writeSyncBody(w http.ResponseWriter, v any) {
	if !c.opts.ResetMidBody {
		writeJSON(w, http.StatusOK, v)
		return
	}

	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The whole body's length, not the half that follows: the client is left waiting for bytes that
	// never come, and that is what lets it tell a cut body from a short one.
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body[:len(body)/2])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	// Not hijackable (HTTP/2, say). Aborting the handler cuts the response the same way.
	panic(http.ErrAbortHandler)
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
		writeUnknownID(w, di)
		return
	}
	op.polls++
	polls := op.polls
	c.mu.Unlock()

	if c.opts.WrongReplicaEvery > 0 && polls%c.opts.WrongReplicaEvery == 0 {
		// A replica that never saw this operation: the containers' default result store is
		// instance-local, so a round-robin route produces exactly this.
		writeUnknownID(w, di)
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
		"analyzeResult":       c.analyzeResult(di),
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

// analyzeResult builds a result body for the surface being faked, padded when the test wants a
// large one. The two surfaces share no result shape: Document Intelligence returns a Layout
// analysis and Read returns readResults. Serving Read the Layout body hid every code path that
// has to tell the two apart.
func (c *Container) analyzeResult(di bool) any {
	if !di {
		return c.readBody()
	}
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

// readResult is a Computer Vision Read v3.2 analyzeResult. It is a struct and not a map so its
// members keep the order the container writes them in; a map marshals alphabetically, and code
// that copies the container's bytes or compares member order is only tested against the real
// order if the fake produces it.
type readResult struct {
	Version      string     `json:"version"`
	ModelVersion string     `json:"modelVersion"`
	ReadResults  []readPage `json:"readResults"`
	// Padding inflates the body when a test wants a large one. It is a member of the result,
	// because anything appended after it would no longer be a single JSON document.
	Padding string `json:"padding,omitempty"`
}

type readPage struct {
	Page   int        `json:"page"`
	Angle  float64    `json:"angle"`
	Width  int        `json:"width"`
	Height int        `json:"height"`
	Unit   string     `json:"unit"`
	Lines  []readLine `json:"lines"`
}

type readLine struct {
	BoundingBox []int      `json:"boundingBox"`
	Text        string     `json:"text"`
	Words       []readWord `json:"words"`
}

type readWord struct {
	BoundingBox []int   `json:"boundingBox"`
	Text        string  `json:"text"`
	Confidence  float64 `json:"confidence"`
}

// readBody builds the one-page, one-line result the Read fake returns. Bounding boxes are eight
// coordinates, clockwise from the top left, as Read reports them.
func (c *Container) readBody() readResult {
	out := readResult{
		Version:      "3.2.0",
		ModelVersion: "2022-04-30",
		ReadResults: []readPage{{
			Page: 1, Angle: 0, Width: 800, Height: 600, Unit: "pixel",
			Lines: []readLine{{
				BoundingBox: []int{8, 8, 104, 8, 104, 28, 8, 28},
				Text:        "mock line",
				Words: []readWord{
					{BoundingBox: []int{8, 8, 52, 8, 52, 28, 8, 28}, Text: "mock", Confidence: 0.99},
					{BoundingBox: []int{60, 8, 104, 8, 104, 28, 60, 28}, Text: "line", Confidence: 0.98},
				},
			}},
		}},
	}
	if c.opts.PadResultBytes > 0 {
		out.Padding = strings.Repeat("x", c.opts.PadResultBytes)
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

// writeUnknownID mirrors what the real containers answer for an operation id they do not know.
// Document Intelligence drops its wrapper for this one response, alone among its errors; Read
// wraps it and calls it BadArgument, a code its own enum defines for something else entirely.
func writeUnknownID(w http.ResponseWriter, di bool) {
	if di {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"code": "NotFound", "message": "Analyze result does not exist.",
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{
		"code": "BadArgument",
		"message": "Operation ID is invalid, expired or the results matching this " +
			"operationId have been deleted.",
	}})
}

func writeDIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// haproxyBusyPage is HAProxy's stock 503 page. It is HTML, so nothing in it parses as a container
// error object, and a client that expected one would take a retriable condition for a failure.
const haproxyBusyPage = "<html><body><h1>503 Service Unavailable</h1>\n" +
	"No server is available to handle this request.\n</body></html>\n"

// writeHAProxyBusy answers as HAProxy does when no pod can take a request, because every one is at
// its connection cap or none is ready: a 503 with its HTML page and a Retry-After the gateway
// adopts as its backoff.
func writeHAProxyBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(haproxyBusyPage))
}

// containerError returns the code and message a container's error body carries for a transient
// failure with this status. Both containers wrap their errors as {"error":{...}}; what differs is
// the vocabulary. Document Intelligence says ServiceUnavailable for a transient condition, and its
// 429 reuses it. Read's error enum has no such member and must never be sent one, so its 503 is
// Unspecified (docs/api-surface.md §6C).
//
// These bodies are representative, not recorded: no recorded container response exists for most
// of these statuses, and the gateway decides to retry on the status, not the body. What matters
// is that the body is the right shape and that a 500 does not read like the missing-route 500 that
// Sync500Unhandled produces — that one is terminal, and this one is not.
func containerError(status int, di bool) (code, message string) {
	switch {
	case di && (status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests):
		return "ServiceUnavailable", "A transient error occurred. Try again."
	case !di && status == http.StatusServiceUnavailable:
		return "Unspecified", "A transient error occurred. Try again."
	case status >= http.StatusInternalServerError:
		return "InternalServerError", "An unexpected error occurred."
	default:
		return "InvalidRequest", "Invalid request."
	}
}
