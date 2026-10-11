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
//
// NewPaged builds a third kind for splitting: a fake that recognises the original pages of the
// pdftest document, or part of one, it is sent, answers with docgen's result for exactly those
// pages, and hangs or fails per page (see PagedOptions). cmd/fakeazure serves it on fixed ports
// for end-to-end runs.
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
	// Latency delays each synchronous call the container serves. A call whose client gives up
	// during it ends there, and Close cuts it rather than waiting it out.
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
	// container's own behaviour (RequireAPIKey, Sync and the options above), and a call one of
	// them answers never reaches the container: the key is not checked, Latency does not slow it,
	// and Hits does not count it. ResetMidBody then damages the 200 that behaviour sends.

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

	// NoServerHeader drops the Server: Kestrel header the containers' web server puts on every
	// answer, so a test can tell a header the gateway set itself from one it relayed.
	NoServerHeader bool
}

// Container is a fake Azure container.
type Container struct {
	opts   Options
	server *httptest.Server
	// paged is set on a fake NewPaged built, which answers from docgen for the pages it is sent.
	paged *paged

	// done is closed by Close. It ends every call the container is holding, which would otherwise
	// keep Close waiting until each client gave up.
	done      chan struct{}
	closeOnce sync.Once

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
	// result is the analyzeResult a paged fake worked out when the operation was submitted. A
	// plain fake builds its body when polled, and leaves this nil.
	result []byte
}

// newContainer validates opts and returns a container with no server yet.
func newContainer(opts Options) *Container {
	opts.validate()
	return &Container{opts: opts, ops: map[string]*operation{}, done: make(chan struct{})}
}

// URL returns the container's base URL.
func (c *Container) URL() string { return c.server.URL }

// Close shuts the container down. A call it is still holding is cut, not answered. Closing twice
// is harmless.
func (c *Container) Close() {
	c.closeOnce.Do(func() { close(c.done) })
	c.server.Close()
}

// Hits reports how many analyze calls, synchronous or asynchronous, the container itself served:
// a call HAProxy turned away (Capacity, BusyFirst, StatusFirst) never reached a pod, and one
// refused for its API key was not served, so neither counts.
func (c *Container) Hits() int64 { return c.hits.Load() }

// SyncCalls reports how many synchronous analyze requests were received, whatever became of them:
// a result, a failure mode, a hang, or a rejection for a bad API key. It is the attempt count a
// retry loop is asserted against. Asynchronous submits and polls are not counted.
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
	c := newContainer(opts)
	c.server = httptest.NewServer(c.handler(c.diMux()))
	return c
}

// NewRead starts a fake Computer Vision Read v3.2 container.
func NewRead(opts Options) *Container {
	c := newContainer(opts)
	c.server = httptest.NewServer(c.handler(c.readMux()))
	return c
}

// diMux routes a Document Intelligence container.
func (c *Container) diMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Both families are served, as the containers do: the legacy Form Recognizer one is declared
	// alongside the newer one by a probed layout-4.0 build.
	for _, fam := range []string{"/documentintelligence", "/formrecognizer"} {
		results := fam + "/documentModels/{modelId}/analyzeResults/{resultId}"

		mux.HandleFunc("POST "+fam+"/documentModels/{action}", func(w http.ResponseWriter, r *http.Request) {
			c.serveAction(w, r, fam+"/documentModels/prebuilt-layout/analyzeResults/")
		})
		mux.HandleFunc("GET "+results, func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			c.servePoll(w, r.PathValue("resultId"), true)
		})
		mux.HandleFunc("DELETE "+results, func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			c.serveDelete(w, r.PathValue("resultId"))
		})

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

		// Model-lifecycle operations: a layout container builds no models, so it has none.
		mux.HandleFunc("GET "+fam+"/operations", func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			writeRaw(w, http.StatusOK, []byte(`{"value":[]}`))
		})
		mux.HandleFunc("GET "+fam+"/operations/{operationId}", func(w http.ResponseWriter, r *http.Request) {
			c.record(r.URL.Path)
			writeAPIError(w, http.StatusNotFound, apiError{
				Code: "NotFound", Message: "Resource not found.",
				InnerError: &apiError{Code: "OperationNotFound", Message: "The requested operation " +
					"wasn't found. The identifier is invalid or the operation is expired."},
			})
		})
	}

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

	c.addCommon(mux, true)
	return mux
}

// serveAction answers a POST to a Document Intelligence model: :syncAnalyze, :analyze, or a verb
// the container does not have.
func (c *Container) serveAction(w http.ResponseWriter, r *http.Request, resultPrefix string) {
	c.record(r.URL.Path)
	switch _, verb, _ := strings.Cut(r.PathValue("action"), ":"); verb {
	case "syncAnalyze":
		c.serveSync(w, r, true)
	case "analyze":
		if c.authorized(w, r) {
			c.serveSubmit(w, r, resultPrefix, true)
		}
	default:
		writeDIError(w, http.StatusNotFound, "NotFound", "Resource not found.")
	}
}

// readMux routes a Computer Vision Read v3.2 container.
func (c *Container) readMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /vision/v3.2/read/syncAnalyze", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		c.serveSync(w, r, false)
	})

	mux.HandleFunc("POST /vision/v3.2/read/analyze", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		if !c.authorized(w, r) {
			return
		}
		c.serveSubmit(w, r, "/vision/v3.2/read/analyzeResults/", false)
	})

	mux.HandleFunc("GET /vision/v3.2/read/analyzeResults/{operationId}", func(w http.ResponseWriter, r *http.Request) {
		c.record(r.URL.Path)
		c.servePoll(w, r.PathValue("operationId"), false)
	})

	c.addCommon(mux, false)
	return mux
}

// handler serves mux the way Kestrel would. Every answer names Kestrel, and a request no route
// matches gets Kestrel's bodyless 404, or a bodyless 405 with Allow for a known path and the wrong
// verb, where net/http's mux would have written a line of text.
func (c *Container) handler(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.opts.NoServerHeader {
			w.Header().Set("Server", "Kestrel")
		}
		if _, pattern := mux.Handler(r); pattern == "" {
			w = bodyless{w}
		}
		mux.ServeHTTP(w, r)
	})
}

// bodyless passes on a status and its headers and drops the body: the content headers net/http's
// error helper set are removed before the status goes out, so the answer is an empty one.
type bodyless struct{ http.ResponseWriter }

func (b bodyless) WriteHeader(status int) {
	h := b.Header()
	h.Del("Content-Type")
	h.Del("X-Content-Type-Options")
	h.Set("Content-Length", "0")
	b.ResponseWriter.WriteHeader(status)
}

func (b bodyless) Write(p []byte) (int, error) { return len(p), nil }

// writeNotFound is Kestrel's answer for a request a route matched by path but its constraints
// rejected: a bodyless 404, the same as for a path no route matches.
func writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNotFound)
}

// addCommon routes what every container serves besides analysis: its health, its home page, its
// swagger and its usage records. The gateway relays the last three verbatim, so the bodies here
// are representative, not recorded; what matters is that the routes exist and answer in kind.
func (c *Container) addCommon(mux *http.ServeMux, di bool) {
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

	name, version := "Computer Vision Read", "v3.2"
	if di {
		name, version = "Document Intelligence", "2024-11-30"
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeHTML(w, "<!DOCTYPE html><html><head><title>"+name+" container</title></head>"+
			"<body><h1>The "+name+" container is running.</h1></body></html>")
	})
	swaggerUI := func(w http.ResponseWriter, r *http.Request) {
		writeHTML(w, `<!DOCTYPE html><html><head><title>Swagger UI</title></head>`+
			`<body><div id="swagger-ui"></div></body></html>`)
	}
	mux.HandleFunc("GET /swagger", swaggerUI)
	mux.HandleFunc("GET /swagger/index.html", swaggerUI)
	mux.HandleFunc("GET /swagger/v1/swagger.json", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(w, http.StatusOK, []byte(`{"openapi":"3.0.1","info":{"title":"`+name+
			`","version":"`+version+`"},"paths":{}}`))
	})

	usage := []byte(`{"apiType":"` + strings.ToLower(strings.ReplaceAll(name, " ", "")) +
		`","serviceName":"` + name + `","meters":[]}`)
	mux.HandleFunc("GET /records/usage-logs", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(w, http.StatusOK, usage)
	})
	mux.HandleFunc("GET /records/usage-logs/{month}/{year}", func(w http.ResponseWriter, r *http.Request) {
		// The month and year are integer route constraints, so anything else matches no route.
		month, okMonth := positive(r.PathValue("month"))
		_, okYear := positive(r.PathValue("year"))
		if !okMonth || !okYear || month > 12 {
			writeNotFound(w)
			return
		}
		writeRaw(w, http.StatusOK, usage)
	})
}

// positive parses a positive decimal integer of ASCII digits.
func positive(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, n > 0
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
	// Counted first: SyncCalls is every attempt a client made, whoever answered it.
	c.syncCalls.Add(1)
	// The body is read before anything is answered, so a client still uploading meets the answer
	// it is owed and not a connection closed under it. A paged fake keeps it to work out which
	// pages it was sent.
	var body []byte
	if c.paged != nil {
		body, _ = io.ReadAll(r.Body)
	} else {
		_, _ = io.Copy(io.Discard, r.Body)
	}

	// What HAProxy and the autoscaler do to a call happens in front of the container, so it is
	// decided before anything the container does: its key check, its behaviour and its Latency.
	// The order is the one Options documents.
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

	// The call has reached a pod.
	if !c.authorized(w, r) {
		return
	}
	c.hits.Add(1)

	if c.opts.Latency > 0 {
		// Not a plain sleep: a client that gives up during it is gone, so the call ends and its
		// slot comes back at once, and Close cuts the call rather than waiting it out.
		select {
		case <-time.After(c.opts.Latency):
		case <-r.Context().Done():
			return
		case <-c.done:
			panic(http.ErrAbortHandler)
		}
	}
	if c.paged != nil {
		c.paged.serveSync(c, w, r, body, di)
		return
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
		c.hang(r)
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
		c.writeSyncBody(w, []byte(`{"status":"Failed"}`))
		return
	}
	if c.opts.FailAnalysis {
		c.writeSyncBody(w, failedEnvelope(di))
		return
	}
	if c.opts.SyncBodyBare {
		c.writeSyncBody(w, c.analyzeResult(di))
		return
	}
	c.writeSyncBody(w, envelope("succeeded", syncUpdated, "analyzeResult", c.analyzeResult(di)))
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
func (c *Container) writeSyncBody(w http.ResponseWriter, body []byte) {
	if !c.opts.ResetMidBody {
		writeRaw(w, http.StatusOK, body)
		return
	}

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
func (c *Container) serveSubmit(w http.ResponseWriter, r *http.Request, resultPrefix string, di bool) {
	c.hits.Add(1)
	op := &operation{}
	if c.paged != nil {
		// The analysis is worked out now, as a container starts one on submit; the poll serves it.
		body, _ := io.ReadAll(r.Body)
		a, ok := c.paged.analyse(w, r, body, di)
		if !ok {
			return
		}
		op.result = c.paged.result(a, di)
	} else {
		_, _ = io.Copy(io.Discard, r.Body)
	}
	id := c.addOperation(op)
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
		writeRaw(w, http.StatusOK, envelope("running", syncUpdated, "", nil))
		return
	}
	if c.opts.FailAnalysis {
		writeRaw(w, http.StatusOK, failedEnvelope(di))
		return
	}
	result := op.result
	if result == nil {
		result = c.analyzeResult(di)
	}
	writeRaw(w, http.StatusOK, envelope("succeeded", pollUpdated, "analyzeResult", result))
}

// serveDelete deletes a Document Intelligence result. Afterwards its id is unknown, exactly as an
// id that never existed is.
func (c *Container) serveDelete(w http.ResponseWriter, id string) {
	c.mu.Lock()
	_, ok := c.ops[id]
	delete(c.ops, id)
	c.mu.Unlock()
	if !ok {
		writeUnknownID(w, true)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Container) newOperation() string { return c.addOperation(&operation{}) }

// addOperation stores op under a new id and returns the id.
func (c *Container) addOperation(op *operation) string {
	id := fmt.Sprintf("%08x-0000-4000-8000-%012x", c.seq.Add(1), c.seq.Load())
	c.mu.Lock()
	c.ops[id] = op
	c.mu.Unlock()
	return id
}

// hang holds a call, answering nothing, until its client gives up. If the container is closed
// first the connection is cut instead: returning would send an empty 200, which reads as a result
// with nothing in it.
func (c *Container) hang(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-c.done:
		panic(http.ErrAbortHandler)
	}
}

// The timestamps every envelope carries: when the operation was created, and when it was last
// updated as seen by a synchronous call (or a poll still running) and by a terminal poll.
const (
	createdAt   = "2024-11-30T00:00:00Z"
	syncUpdated = "2024-11-30T00:00:01Z"
	pollUpdated = "2024-11-30T00:00:02Z"
)

// envelope returns an operation envelope with its members in the order the containers write them:
// status, the two timestamps, then the one member that says how it ended, an error or the
// analyzeResult, unless member is empty. value is that member's JSON, copied verbatim.
//
// It is written by hand rather than marshalled from a map, which would order the members
// alphabetically and put analyzeResult first, and so that a result's bytes reach the client
// exactly as they were built.
func envelope(status, updated, member string, value []byte) []byte {
	b := make([]byte, 0, 128+len(value))
	b = append(b, `{"status":"`+status+`","createdDateTime":"`+createdAt+
		`","lastUpdatedDateTime":"`+updated+`"`...)
	if member != "" {
		b = append(b, `,"`+member+`":`...)
		b = append(b, value...)
	}
	return append(b, '}')
}

// invalidContent is Document Intelligence's error for a document it cannot read.
var invalidContent = []byte(`{"code":"InvalidRequest","message":"Invalid request.",` +
	`"innererror":{"code":"InvalidContent","message":"The file is corrupted or format is unsupported."}}`)

// failedEnvelope is the envelope of an analysis that failed. Document Intelligence says why in an
// error member. Read's operation schema has no error member at all, so a failed Read operation is
// the bare envelope.
func failedEnvelope(di bool) []byte {
	if di {
		return envelope("failed", pollUpdated, "error", invalidContent)
	}
	return envelope("failed", pollUpdated, "", nil)
}

// analyzeResult builds a result body for the surface being faked, padded when the test wants a
// large one. The two surfaces share no result shape: Document Intelligence returns a Layout
// analysis and Read returns readResults. Serving Read the Layout body hid every code path that
// has to tell the two apart.
func (c *Container) analyzeResult(di bool) []byte {
	if !di {
		b, _ := json.Marshal(c.readBody())
		return b
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
	b, _ := json.Marshal(out)
	return b
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
	writeRaw(w, status, body)
}

// writeRaw sends body, which is already JSON.
func writeRaw(w http.ResponseWriter, status int, body []byte) {
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

// writeHTML sends a 200 HTML page.
func writeHTML(w http.ResponseWriter, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(page)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, page)
}

// apiError is the error object both containers wrap in {"error": ...}. It is a struct so its
// members are written in the containers' order, code first and innererror last.
type apiError struct {
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	InnerError *apiError `json:"innererror,omitempty"`
}

func writeAPIError(w http.ResponseWriter, status int, e apiError) {
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{e})
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
// adopts as its backoff. The page is HAProxy's own, so it does not name Kestrel.
func writeHAProxyBusy(w http.ResponseWriter) {
	w.Header().Del("Server")
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
