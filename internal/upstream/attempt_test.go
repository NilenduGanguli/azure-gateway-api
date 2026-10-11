package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
)

// The attempt tests run each row of the sync-only spec's outcome table (§1) against mockazure,
// which models both the containers and the HAProxy in front of them, and against plain httptest
// servers for the responses no fake produces.

const testAPIKey = "test-key"

// newTestClient returns a client for surface aimed at baseURL, and the scratch directory its
// responses are written to.
func newTestClient(t *testing.T, surface azerr.Surface, baseURL string) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	return NewClient(surface, config.Upstream{
		BaseURL: baseURL, APIKey: testAPIKey, MaxInflight: 1, Timeout: time.Minute,
	}, dir, 1<<20), dir
}

// newFake starts the fake container for surface and closes it when the test ends.
func newFake(t *testing.T, surface azerr.Surface, opts mockazure.Options) *mockazure.Container {
	t.Helper()
	start := mockazure.NewDI
	if surface == azerr.SurfaceRead {
		start = mockazure.NewRead
	}
	c := start(opts)
	t.Cleanup(c.Close)
	return c
}

// testDoc is a small re-openable document. opens, when non-nil, counts the calls to Open.
func testDoc(opens *atomic.Int64) Document {
	const body = "%PDF-1.7 test document"
	return Document{
		ContentType: "application/pdf",
		Size:        int64(len(body)),
		Open: func() (io.ReadCloser, error) {
			if opens != nil {
				opens.Add(1)
			}
			return io.NopCloser(strings.NewReader(body)), nil
		},
	}
}

// testCall is the call an engine would make for one whole document on surface.
func testCall(surface azerr.Surface) Call {
	c := Call{
		Query:   url.Values{"api-version": {"2024-11-30"}},
		Body:    testDoc(nil),
		Timeout: 30 * time.Second,
	}
	if surface == azerr.SurfaceDI {
		c.ModelID = "prebuilt-layout"
		c.Prefix = DIPathPrefix
	} else {
		c.Query = url.Values{"language": {"en"}}
	}
	return c
}

// want is what one attempt is expected to have been classified as.
type want struct {
	outcome    Outcome
	reason     string
	status     int
	retryAfter time.Duration
	// The error the attempt carries. errCode empty means "do not check"; errMessage is a substring.
	errCode    string
	errInner   string
	errStatus  int
	errMessage string
}

func checkAttempt(t *testing.T, got Attempt, w want) {
	t.Helper()
	if got.Outcome != w.outcome {
		t.Errorf("outcome %v, want %v (reason %q, status %d, err %v)",
			got.Outcome, w.outcome, got.Reason, got.Status, got.Err)
	}
	if got.Reason != w.reason {
		t.Errorf("reason %q, want %q", got.Reason, w.reason)
	}
	if got.Status != w.status {
		t.Errorf("status %d, want %d", got.Status, w.status)
	}
	if got.RetryAfter != w.retryAfter {
		t.Errorf("retryAfter %v, want %v", got.RetryAfter, w.retryAfter)
	}
	if got.Took < 0 || (got.Status != 0 && got.Took == 0) {
		t.Errorf("took %v; every attempt reports its duration", got.Took)
	}
	switch got.Outcome {
	case Success:
		if got.Err != nil {
			t.Errorf("a success carries error %v", got.Err)
		}
		if got.Result == nil {
			t.Error("a success carries no result")
		}
		return
	case Cancelled:
		// The caller's context is the error; an APIError here could be relayed to a client as if
		// the container had failed.
		if got.Err != nil {
			t.Errorf("a cancelled attempt carries a client-facing error %v", got.Err)
		}
	default:
		if got.Err == nil {
			t.Fatal("a failed attempt carries no error")
		}
	}
	if got.Result != nil {
		t.Error("a failed attempt carries a result")
	}
	if w.errCode != "" && got.Err.Code != w.errCode {
		t.Errorf("error code %q, want %q", got.Err.Code, w.errCode)
	}
	if w.errInner != "" && got.Err.InnerCode != w.errInner {
		t.Errorf("inner code %q, want %q", got.Err.InnerCode, w.errInner)
	}
	if w.errStatus != 0 && got.Err.Status != w.errStatus {
		t.Errorf("error status %d, want %d", got.Err.Status, w.errStatus)
	}
	if w.errMessage != "" && !strings.Contains(got.Err.Message, w.errMessage) {
		t.Errorf("error message %q does not contain %q", got.Err.Message, w.errMessage)
	}
}

// assertNoScratch fails when an attempt left a response file behind. Only a success hands a
// file to its caller.
func assertNoScratch(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("scratch file %s left behind", e.Name())
	}
}

// outcomeCase is one row of an outcome table: either a fake container or a raw handler.
type outcomeCase struct {
	name    string
	surface azerr.Surface
	opts    *mockazure.Options
	handler http.HandlerFunc
	timeout time.Duration
	want    want
}

func runOutcomeCases(t *testing.T, cases []outcomeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var base string
			if tc.opts != nil {
				base = newFake(t, tc.surface, *tc.opts).URL()
			} else {
				srv := httptest.NewServer(tc.handler)
				t.Cleanup(srv.Close)
				base = srv.URL
			}
			client, dir := newTestClient(t, tc.surface, base)
			call := testCall(tc.surface)
			if tc.timeout > 0 {
				call.Timeout = tc.timeout
			}
			got := client.Do(context.Background(), call)
			defer got.Result.Cleanup()
			checkAttempt(t, got, tc.want)
			if got.Outcome != Success {
				assertNoScratch(t, dir)
			}
		})
	}
}

// statusHandler answers every call with status, the given headers and body.
func statusHandler(status int, body string, headers ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// stallMidBody sends a 200's headers and the start of its body, then stalls until the client
// hangs up. The request body is read first: the server notices a client hanging up only once it
// has.
func stallMidBody(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"succeeded","analyzeResult":{`)
	w.(http.Flusher).Flush()
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

// opts lets a table row name fake options inline.
func opts(o mockazure.Options) *mockazure.Options { return &o }

// Plan test 1, row 2: a 200 carrying an analyzeResult is a success, whether the container wraps
// it in an operation envelope or answers the bare AnalyzeResult.
func TestDoSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		surface azerr.Surface
		prefix  string
		opts    mockazure.Options
		bare    bool
		path    string
		member  string // a member only that surface's analyzeResult has
	}{
		{"document intelligence envelope", azerr.SurfaceDI, DIPathPrefix, mockazure.Options{},
			false, "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze", "pages"},
		{"document intelligence bare", azerr.SurfaceDI, DIPathPrefix,
			mockazure.Options{SyncBodyBare: true},
			true, "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze", "pages"},
		{"legacy form recognizer family", azerr.SurfaceDI, LegacyPathPrefix, mockazure.Options{},
			false, "/formrecognizer/documentModels/prebuilt-layout:syncAnalyze", "pages"},
		{"read envelope", azerr.SurfaceRead, "", mockazure.Options{},
			false, "/vision/v3.2/read/syncAnalyze", "readResults"},
		{"read bare", azerr.SurfaceRead, "", mockazure.Options{SyncBodyBare: true},
			true, "/vision/v3.2/read/syncAnalyze", "readResults"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(t, tc.surface, tc.opts)
			client, _ := newTestClient(t, tc.surface, fake.URL())
			call := testCall(tc.surface)
			call.Prefix = tc.prefix

			got := client.Do(context.Background(), call)
			checkAttempt(t, got, want{outcome: Success, status: http.StatusOK})
			if got.Result == nil {
				t.FailNow()
			}
			defer got.Result.Cleanup()

			if got.Result.Bare != tc.bare {
				t.Errorf("Bare %v, want %v", got.Result.Bare, tc.bare)
			}
			raw, err := os.ReadFile(got.Result.Path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Result.Start < 0 || got.Result.End > int64(len(raw)) ||
				got.Result.Start >= got.Result.End {
				t.Fatalf("result range [%d,%d) is outside a %d-byte response",
					got.Result.Start, got.Result.End, len(raw))
			}
			if tc.bare && (got.Result.Start != 0 || got.Result.End != int64(len(raw))) {
				t.Errorf("a bare result spans [%d,%d), want the whole %d-byte response",
					got.Result.Start, got.Result.End, len(raw))
			}
			var result map[string]json.RawMessage
			if err := json.Unmarshal(raw[got.Result.Start:got.Result.End], &result); err != nil {
				t.Fatalf("the result range is not one JSON object: %v", err)
			}
			if _, ok := result[tc.member]; !ok {
				t.Errorf("the result range has no %q member: %s", tc.member,
					raw[got.Result.Start:got.Result.End])
			}
			if _, ok := result["status"]; ok {
				t.Error("the result range includes the envelope's status")
			}
			if got.Result.UpstreamMS < 0 {
				t.Errorf("UpstreamMS %d", got.Result.UpstreamMS)
			}
			if !fake.Called(tc.path) {
				t.Errorf("the container never saw %s; it saw %v", tc.path, fake.Requests())
			}

			path := got.Result.Path
			got.Result.Cleanup()
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Cleanup left the response file: %v", err)
			}
		})
	}
}

// The request an attempt sends: the route, the forwarded query, the gateway's own key and the
// document with its type and length.
func TestDoSendsTheCall(t *testing.T) {
	type seen struct {
		method, path, rawQuery, key, contentType, accept string
		length                                           int64
		body                                             string
	}
	for _, tc := range []struct {
		name    string
		surface azerr.Surface
		call    func(*Call)
		want    seen
	}{
		{"document intelligence", azerr.SurfaceDI, func(c *Call) {
			c.Query = url.Values{"api-version": {"2024-11-30"}, "pages": {"1-3"}}
		}, seen{path: "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze",
			rawQuery: "api-version=2024-11-30&pages=1-3"}},
		{"legacy family and an escaped model id", azerr.SurfaceDI, func(c *Call) {
			c.Prefix = LegacyPathPrefix
			c.ModelID = "my model:v1"
		}, seen{path: "/formrecognizer/documentModels/my%20model%3Av1:syncAnalyze",
			rawQuery: "api-version=2024-11-30"}},
		{"an empty prefix is the current family", azerr.SurfaceDI, func(c *Call) {
			c.Prefix = ""
		}, seen{path: "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze",
			rawQuery: "api-version=2024-11-30"}},
		{"read", azerr.SurfaceRead, func(c *Call) {
			c.Query = url.Values{"language": {"en"}, "readingOrder": {"natural"}}
		}, seen{path: "/vision/v3.2/read/syncAnalyze",
			rawQuery: "language=en&readingOrder=natural"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var got seen
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				got = seen{
					method: r.Method, path: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery,
					key: r.Header.Get(HeaderAPIKey), contentType: r.Header.Get("Content-Type"),
					accept: r.Header.Get("Accept"), length: r.ContentLength, body: string(body),
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"succeeded","analyzeResult":{"content":""}}`)
			}))
			defer srv.Close()

			client, _ := newTestClient(t, tc.surface, srv.URL)
			var opens atomic.Int64
			call := testCall(tc.surface)
			call.Body = testDoc(&opens)
			tc.call(&call)

			res := client.Do(context.Background(), call)
			defer res.Result.Cleanup()
			checkAttempt(t, res, want{outcome: Success, status: http.StatusOK})

			mu.Lock()
			defer mu.Unlock()
			tc.want.method = http.MethodPost
			tc.want.key = testAPIKey
			tc.want.contentType = "application/pdf"
			tc.want.accept = "application/json"
			tc.want.body = "%PDF-1.7 test document"
			tc.want.length = int64(len(tc.want.body))
			if got != tc.want {
				t.Errorf("the container saw\n  %+v\nwant\n  %+v", got, tc.want)
			}
			if n := opens.Load(); n != 1 {
				t.Errorf("Document.Open called %d times for one attempt, want 1", n)
			}
		})
	}
}

// Plan test 2, row 3: a 200 whose status is failed is the analysis failing, not the transport.
func TestDoAnalysisFailed(t *testing.T) {
	runOutcomeCases(t, []outcomeCase{
		{name: "read bare Failed is given a well-formed error", surface: azerr.SurfaceRead,
			opts: opts(mockazure.Options{FailSyncBare: true}),
			want: want{outcome: AnalysisFailed, reason: ReasonAnalysisFailed,
				status: http.StatusOK, errCode: azerr.ReadInternalServerError,
				errMessage: "reported the analysis as failed"}},
		{name: "document intelligence failed envelope relays its error", surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{FailAnalysis: true}),
			want: want{outcome: AnalysisFailed, reason: ReasonAnalysisFailed,
				status: http.StatusOK, errCode: azerr.CodeInvalidRequest,
				errInner: azerr.InnerInvalidContent}},
		{name: "failed wins over a partial analyzeResult", surface: azerr.SurfaceRead,
			handler: statusHandler(http.StatusOK, `{"status":"failed","analyzeResult":`+
				`{"version":"3.2.0","readResults":[],"errors":[{"code":"InvalidImage",`+
				`"message":"The image is unreadable."}]}}`),
			want: want{outcome: AnalysisFailed, reason: ReasonAnalysisFailed,
				status: http.StatusOK, errCode: "InvalidImage", errMessage: "unreadable"}},
	})
}

// Plan test 3, row 4: a container that lacks the synchronous route is a deployment fault, told
// apart from a request-scoped 404 by its body.
func TestDoRouteMissing(t *testing.T) {
	const diMissing = "The document-intelligence container does not serve the synchronous analyze route."
	const readMissing = "The computer-vision-read container does not serve the synchronous analyze route."
	diWant := func(status int) want {
		return want{outcome: RouteMissing, reason: statusReason(status), status: status,
			errCode: azerr.CodeInternalServerError, errStatus: http.StatusInternalServerError,
			errMessage: diMissing}
	}
	runOutcomeCases(t, []outcomeCase{
		{name: "bodyless 404", surface: azerr.SurfaceDI, opts: opts(mockazure.Options{Sync: mockazure.Sync404}),
			want: diWant(http.StatusNotFound)},
		{name: "500 UnhandledEndpointException", surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{Sync: mockazure.Sync500Unhandled}),
			want: diWant(http.StatusInternalServerError)},
		{name: "read bodyless 404", surface: azerr.SurfaceRead,
			opts: opts(mockazure.Options{Sync: mockazure.Sync404}),
			want: want{outcome: RouteMissing, reason: "status_404", status: http.StatusNotFound,
				errCode: azerr.ReadInternalServerError, errMessage: readMissing}},
		{name: "bodyless 405", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusMethodNotAllowed, "", "Allow", "GET"),
			want:    diWant(http.StatusMethodNotAllowed)},
		{name: "bodyless 501", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusNotImplemented, ""),
			want:    diWant(http.StatusNotImplemented)},
		{name: "404 with an HTML body", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusNotFound, "<html>Not Found</html>", "Content-Type", "text/html"),
			want:    diWant(http.StatusNotFound)},
		{name: "404 whose error says no endpoint matched", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusNotFound,
				`{"error":{"code":"NotFound","message":"The request did not match any endpoints."}}`),
			want: diWant(http.StatusNotFound)},
	})
}

// Plan test 4, row 9: any other 4xx is about this request and is relayed in the surface's shape.
func TestDoTerminalClientError(t *testing.T) {
	runOutcomeCases(t, []outcomeCase{
		{name: "404 ModelNotFound", surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{Sync: mockazure.SyncModelNotFound}),
			want: want{outcome: Terminal, reason: "status_404", status: http.StatusNotFound,
				errCode: azerr.CodeNotFound, errInner: azerr.InnerModelNotFound,
				errStatus: http.StatusNotFound, errMessage: "Resource not found."}},
		{name: "400 InvalidArgument", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusBadRequest, `{"error":{"code":"InvalidArgument",`+
				`"message":"Invalid argument.","innererror":{"code":"InvalidParameter",`+
				`"message":"The parameter pages is invalid."}}}`),
			want: want{outcome: Terminal, reason: "status_400", status: http.StatusBadRequest,
				errCode: azerr.CodeInvalidArgument, errInner: azerr.InnerInvalidParameter,
				errStatus: http.StatusBadRequest}},
		{name: "413 HTML keeps no Retry-After", surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{SyncErrorStatus: http.StatusRequestEntityTooLarge}),
			want: want{outcome: Terminal, reason: "status_413", status: http.StatusRequestEntityTooLarge}},
		{name: "read 400 BadArgument", surface: azerr.SurfaceRead,
			handler: statusHandler(http.StatusBadRequest,
				`{"error":{"code":"InvalidImageSize","message":"Image is too small."}}`),
			want: want{outcome: Terminal, reason: "status_400", status: http.StatusBadRequest,
				errCode: azerr.ReadInvalidImageSize, errMessage: "too small"}},
	})
}

// Row 10: anything else is terminal — a 200 that is not a result, a result over the ceiling, a
// status no row names.
func TestDoTerminalOther(t *testing.T) {
	runOutcomeCases(t, []outcomeCase{
		{name: "200 with neither analyzeResult nor failed", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusOK, `{"status":"running"}`),
			want: want{outcome: Terminal, reason: ReasonNoResult, status: http.StatusOK,
				errCode: azerr.CodeInternalServerError}},
		{name: "200 that is not JSON", surface: azerr.SurfaceRead,
			handler: statusHandler(http.StatusOK, "<html>hello</html>"),
			want: want{outcome: Terminal, reason: ReasonNoResult, status: http.StatusOK,
				errCode: azerr.ReadInternalServerError}},
		{name: "a redirect is not followed", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusFound, "", "Location", "http://elsewhere.invalid/"),
			want: want{outcome: Terminal, reason: "status_302", status: http.StatusFound,
				errCode: azerr.CodeInternalServerError, errStatus: http.StatusInternalServerError}},
		{name: "507", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusInsufficientStorage, ""),
			want:    want{outcome: Terminal, reason: "status_507", status: http.StatusInsufficientStorage}},
	})

	t.Run("a result over the ceiling", func(t *testing.T) {
		fake := newFake(t, azerr.SurfaceDI, mockazure.Options{PadResultBytes: 4 << 10})
		dir := t.TempDir()
		client := NewClient(azerr.SurfaceDI, config.Upstream{BaseURL: fake.URL(), Timeout: time.Minute},
			dir, 1<<10)
		got := client.Do(context.Background(), testCall(azerr.SurfaceDI))
		checkAttempt(t, got, want{outcome: Terminal, reason: ReasonTooLarge, status: http.StatusOK,
			errCode: azerr.CodeInternalServerError})
		assertNoScratch(t, dir)
	})

	t.Run("a document model is required", func(t *testing.T) {
		client, _ := newTestClient(t, azerr.SurfaceDI, "http://127.0.0.1:1")
		call := testCall(azerr.SurfaceDI)
		call.ModelID = ""
		checkAttempt(t, client.Do(context.Background(), call), want{outcome: Terminal,
			reason: ReasonGateway, errCode: azerr.CodeInvalidRequest, errStatus: http.StatusBadRequest})
	})

	t.Run("an unreadable stored input", func(t *testing.T) {
		client, _ := newTestClient(t, azerr.SurfaceDI, "http://127.0.0.1:1")
		call := testCall(azerr.SurfaceDI)
		call.Body.Open = func() (io.ReadCloser, error) {
			return nil, errors.New("open /data/blobs/ab/secret.input: no such file")
		}
		got := client.Do(context.Background(), call)
		checkAttempt(t, got, want{outcome: Terminal, reason: ReasonGateway,
			errCode: azerr.CodeInternalServerError})
		if strings.Contains(got.Err.Message, "/data/blobs") {
			t.Errorf("the client-facing error names a PVC path: %q", got.Err.Message)
		}
	})
}

// Plan test 5, row 5: the attempt's own deadline, a 500 and a 504 are retried once per job.
func TestDoRetryOnce(t *testing.T) {
	runOutcomeCases(t, []outcomeCase{
		{name: "plain 500", surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{StatusFirst: 1, StatusFirstCode: http.StatusInternalServerError}),
			want: want{outcome: RetryOnce, reason: "status_500", status: http.StatusInternalServerError,
				errCode: azerr.CodeInternalServerError, errStatus: http.StatusInternalServerError}},
		{name: "504", surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{StatusFirst: 1, StatusFirstCode: http.StatusGatewayTimeout}),
			want: want{outcome: RetryOnce, reason: "status_504", status: http.StatusGatewayTimeout,
				errStatus: http.StatusGatewayTimeout}},
		{name: "read 500", surface: azerr.SurfaceRead,
			opts: opts(mockazure.Options{StatusFirst: 1, StatusFirstCode: http.StatusInternalServerError}),
			want: want{outcome: RetryOnce, reason: "status_500", status: http.StatusInternalServerError,
				errCode: azerr.ReadInternalServerError}},
		{name: "500 with Retry-After", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusInternalServerError, "", "Retry-After", "4"),
			want: want{outcome: RetryOnce, reason: "status_500", status: http.StatusInternalServerError,
				retryAfter: 4 * time.Second}},
		{name: "the attempt deadline", surface: azerr.SurfaceDI,
			opts:    opts(mockazure.Options{Sync: mockazure.SyncHang}),
			timeout: 100 * time.Millisecond,
			want: want{outcome: RetryOnce, reason: ReasonTimeout,
				errCode: azerr.CodeInternalServerError, errMessage: "within the configured timeout"}},
	})

	t.Run("the deadline is the call's, not the configured one", func(t *testing.T) {
		fake := newFake(t, azerr.SurfaceDI, mockazure.Options{Sync: mockazure.SyncHang})
		client, _ := newTestClient(t, azerr.SurfaceDI, fake.URL())
		call := testCall(azerr.SurfaceDI)
		call.Timeout = 100 * time.Millisecond
		got := client.Do(context.Background(), call)
		if got.Took < call.Timeout || got.Took > 10*time.Second {
			t.Errorf("an attempt with a %v deadline took %v", call.Timeout, got.Took)
		}
	})

	t.Run("the deadline also bounds the result download", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stallMidBody(w, r)
		}))
		defer srv.Close()
		client, dir := newTestClient(t, azerr.SurfaceDI, srv.URL)
		call := testCall(azerr.SurfaceDI)
		call.Timeout = 150 * time.Millisecond
		got := client.Do(context.Background(), call)
		checkAttempt(t, got, want{outcome: RetryOnce, reason: ReasonTimeout})
		assertNoScratch(t, dir)
	})
}

// Plan test 6, row 6: a connection that fails, or a body cut short, is retried.
func TestDoConnectionFailures(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		client, dir := newTestClient(t, azerr.SurfaceDI, base)
		got := client.Do(context.Background(), testCall(azerr.SurfaceDI))
		checkAttempt(t, got, want{outcome: Retry, reason: ReasonConnection,
			errCode: azerr.CodeInternalServerError, errMessage: "unreachable"})
		if got.Err != nil && strings.Contains(got.Err.Message, strings.TrimPrefix(base, "http://")) {
			t.Errorf("the client-facing error names the upstream address: %q", got.Err.Message)
		}
		if got.Cause == nil {
			t.Error("the transport cause is not kept for the log")
		}
		assertNoScratch(t, dir)
	})

	runOutcomeCases(t, []outcomeCase{
		{name: "reset mid-body", surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{ResetMidBody: true}),
			want: want{outcome: Retry, reason: ReasonConnection}},
		{name: "read reset mid-body", surface: azerr.SurfaceRead,
			opts: opts(mockazure.Options{ResetMidBody: true}),
			want: want{outcome: Retry, reason: ReasonConnection, errCode: azerr.ReadInternalServerError}},
	})
}

// Plan test 7, row 7: the statuses HAProxy and an overloaded pod answer with are retried, and a
// Retry-After on them is kept for the backoff.
func TestDoRetry(t *testing.T) {
	var cases []outcomeCase
	for _, code := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable} {
		cases = append(cases, outcomeCase{
			name: http.StatusText(code), surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{StatusFirst: 1, StatusFirstCode: code}),
			want: want{outcome: Retry, reason: statusReason(code), status: code, errStatus: code},
		})
	}
	cases = append(cases,
		outcomeCase{name: "read 503", surface: azerr.SurfaceRead,
			opts: opts(mockazure.Options{StatusFirst: 1, StatusFirstCode: http.StatusServiceUnavailable}),
			want: want{outcome: Retry, reason: "status_503", status: http.StatusServiceUnavailable,
				errCode: "Unspecified"}},
		outcomeCase{name: "HAProxy busy page", surface: azerr.SurfaceDI,
			opts: opts(mockazure.Options{BusyFirst: 1}),
			want: want{outcome: Retry, reason: "status_503", status: http.StatusServiceUnavailable,
				retryAfter: time.Second, errCode: azerr.CodeServiceUnavailable}},
		outcomeCase{name: "503 with Retry-After 3", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusServiceUnavailable, "", "Retry-After", "3"),
			want: want{outcome: Retry, reason: "status_503", status: http.StatusServiceUnavailable,
				retryAfter: 3 * time.Second}},
		outcomeCase{name: "an enormous Retry-After does not overflow", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusServiceUnavailable, "", "Retry-After", "99999999999999"),
			want: want{outcome: Retry, reason: "status_503", status: http.StatusServiceUnavailable,
				retryAfter: maxRetryAfter}},
		outcomeCase{name: "an HTTP-date Retry-After is ignored", surface: azerr.SurfaceDI,
			handler: statusHandler(http.StatusServiceUnavailable, "",
				"Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT"),
			want: want{outcome: Retry, reason: "status_503", status: http.StatusServiceUnavailable}},
	)
	runOutcomeCases(t, cases)

	// Capacity needs a call already in flight, so it does not fit the table.
	t.Run("HAProxy at capacity", func(t *testing.T) {
		fake := newFake(t, azerr.SurfaceDI, mockazure.Options{Capacity: 1, Sync: mockazure.SyncHang})
		client, _ := newTestClient(t, azerr.SurfaceDI, fake.URL())

		// One call takes the only slot and holds it.
		ctx, cancel := context.WithCancel(context.Background())
		held := make(chan Attempt, 1)
		go func() { held <- client.Do(ctx, testCall(azerr.SurfaceDI)) }()
		deadline := time.Now().Add(5 * time.Second)
		for fake.SyncCalls() < 1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		// SyncCalls counts the call on arrival; the fake admits it once the body is read.
		time.Sleep(50 * time.Millisecond)

		// Bounded, so a lost race fails the test quickly instead of hanging in the fake.
		call := testCall(azerr.SurfaceDI)
		call.Timeout = 5 * time.Second
		got := client.Do(context.Background(), call)
		cancel()
		<-held
		checkAttempt(t, got, want{outcome: Retry, reason: "status_503",
			status: http.StatusServiceUnavailable, retryAfter: time.Second})
	})
}

// Plan test 8, row 8: a container that degrades to 202 is retried, and its operation is
// abandoned rather than polled.
func TestDoDegraded202(t *testing.T) {
	for _, surface := range []azerr.Surface{azerr.SurfaceDI, azerr.SurfaceRead} {
		t.Run(surface.String(), func(t *testing.T) {
			fake := newFake(t, surface, mockazure.Options{Sync: mockazure.Sync202})
			client, dir := newTestClient(t, surface, fake.URL())

			got := client.Do(context.Background(), testCall(surface))
			checkAttempt(t, got, want{outcome: Retry, reason: ReasonDegraded202,
				status: http.StatusAccepted, errStatus: http.StatusServiceUnavailable})
			if fake.Called("analyzeResults") {
				t.Errorf("the abandoned operation was polled: %v", fake.Requests())
			}
			if n := fake.SyncCalls(); n != 1 {
				t.Errorf("%d synchronous calls, want 1", n)
			}
			assertNoScratch(t, dir)
		})
	}

	t.Run("a 202's Retry-After paces polls, not attempts", func(t *testing.T) {
		srv := httptest.NewServer(statusHandler(http.StatusAccepted, "",
			"Retry-After", "1", "Operation-Location", "http://pod/x/analyzeResults/op"))
		defer srv.Close()
		client, _ := newTestClient(t, azerr.SurfaceDI, srv.URL)
		checkAttempt(t, client.Do(context.Background(), testCall(azerr.SurfaceDI)),
			want{outcome: Retry, reason: ReasonDegraded202, status: http.StatusAccepted})
	})
}

// Plan test 9, row 1: the caller's context ending is a cancellation, checked before everything
// else — even when the transport's own error reads like a timeout, and even when the attempt's
// deadline expired at the same moment.
func TestDoCancelled(t *testing.T) {
	hang := func(t *testing.T) string {
		return newFake(t, azerr.SurfaceDI, mockazure.Options{Sync: mockazure.SyncHang}).URL()
	}
	stallBody := func(t *testing.T) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stallMidBody(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	for _, tc := range []struct {
		name    string
		server  func(*testing.T) string
		ctx     func() (context.Context, context.CancelFunc)
		timeout time.Duration
		cause   error
	}{
		{"cancelled while waiting for the answer", hang, func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(100*time.Millisecond, cancel)
			return ctx, cancel
		}, time.Minute, context.Canceled},
		{"the caller's deadline, which looks like a timeout", hang, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 100*time.Millisecond)
		}, time.Minute, context.DeadlineExceeded},
		{"the caller's deadline and the attempt's together", hang, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 100*time.Millisecond)
		}, 100 * time.Millisecond, context.DeadlineExceeded},
		{"cancelled during the result download", stallBody, func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(100*time.Millisecond, cancel)
			return ctx, cancel
		}, time.Minute, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, dir := newTestClient(t, azerr.SurfaceDI, tc.server(t))
			ctx, cancel := tc.ctx()
			defer cancel()
			call := testCall(azerr.SurfaceDI)
			call.Timeout = tc.timeout

			got := client.Do(ctx, call)
			checkAttempt(t, got, want{outcome: Cancelled, reason: ReasonCancelled})
			if !errors.Is(got.Cause, tc.cause) {
				t.Errorf("cause %v, want %v", got.Cause, tc.cause)
			}
			assertNoScratch(t, dir)
		})
	}

	t.Run("a failing status that lands as the caller leaves", func(t *testing.T) {
		// The 503's headers arrive, then the caller's context ends while its body is still in
		// flight. The answer alone is row 7, but row 1 is checked first.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"ServiceUnavailable",`)
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
			cancel()
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
				_, _ = io.WriteString(w, `"message":"A transient error occurred."}}`)
			}
		}))
		defer srv.Close()
		client, dir := newTestClient(t, azerr.SurfaceDI, srv.URL)
		got := client.Do(ctx, testCall(azerr.SurfaceDI))
		checkAttempt(t, got, want{outcome: Cancelled, reason: ReasonCancelled,
			status: http.StatusServiceUnavailable})
		assertNoScratch(t, dir)
	})
}

// Plan test 10: every attempt opens a connection of its own, so HAProxy balances each one and a
// retry is free to reach a different pod.
func TestDoUsesAFreshConnectionPerAttempt(t *testing.T) {
	var newConns atomic.Int64
	var mu sync.Mutex
	remotes := map[string]bool{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		remotes[r.RemoteAddr] = true
		mu.Unlock()
		if r.Header.Get("Cookie") != "" {
			t.Errorf("an attempt replayed a cookie: %q", r.Header.Get("Cookie"))
		}
		// An affinity cookie, as the OpenShift router sets: a cookie jar would pin the next
		// attempt to this pod.
		http.SetCookie(w, &http.Cookie{Name: "route", Value: "pod-a"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"succeeded","analyzeResult":{"content":""}}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	client, _ := newTestClient(t, azerr.SurfaceDI, srv.URL)
	var opens atomic.Int64
	for i := 0; i < 3; i++ {
		call := testCall(azerr.SurfaceDI)
		call.Body = testDoc(&opens)
		got := client.Do(context.Background(), call)
		checkAttempt(t, got, want{outcome: Success, status: http.StatusOK})
		got.Result.Cleanup()
	}
	if n := newConns.Load(); n != 3 {
		t.Errorf("%d new connections for 3 attempts, want 3", n)
	}
	mu.Lock()
	if len(remotes) != 3 {
		t.Errorf("attempts came from %d distinct remote ports, want 3: %v", len(remotes), remotes)
	}
	mu.Unlock()
	if n := opens.Load(); n != 3 {
		t.Errorf("Document.Open called %d times for 3 attempts, want 3", n)
	}
}

// A fresh transport per attempt must not leave its connection goroutines behind, or a gateway
// that retries for days accumulates them without bound.
func TestDoLeavesNoGoroutines(t *testing.T) {
	cases := []mockazure.Options{
		{},
		{Sync: mockazure.Sync202},
		{StatusFirst: 1000, StatusFirstCode: http.StatusBadGateway},
		{ResetMidBody: true},
		{Sync: mockazure.SyncHang},
	}
	fakes := make([]*mockazure.Container, len(cases))
	for i, o := range cases {
		fakes[i] = newFake(t, azerr.SurfaceDI, o)
	}
	run := func() {
		for _, fake := range fakes {
			client, _ := newTestClient(t, azerr.SurfaceDI, fake.URL())
			call := testCall(azerr.SurfaceDI)
			call.Timeout = 50 * time.Millisecond
			client.Do(context.Background(), call).Result.Cleanup()
		}
	}
	// One warm-up round, so goroutines that live for the whole process are already running.
	run()
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 10; i++ {
		run()
	}
	// Connection goroutines exit shortly after their connection closes, not at once.
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("%d goroutines before 50 attempts and %d after", before, after)
	}
}
