package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
)

const (
	// frAPIVersion is the newest version the legacy Form Recognizer clients speak. The gateway must
	// echo whichever version a client sent, not its own default.
	frAPIVersion = "2023-07-31"
	diFam        = "/documentintelligence"
	frFam        = "/formrecognizer"
	guidPattern  = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
)

// endpointCase is one request against one route and everything a client could observe about the
// answer. Paths may name {di}, {fr} and {read} — the operations the test submits — and {unknown},
// an id no operation has.
type endpointCase struct {
	name    string
	method  string
	path    string
	body    []byte
	headers []string // name/value pairs added to the request
	status  int
	// retryAfter is the exact Retry-After value expected, "" for none. It is checked on every row
	// because a Retry-After on a non-retriable status makes azure-core retry it ten times.
	retryAfter string
	want       map[string]string         // headers that must equal these values
	match      map[string]*regexp.Regexp // headers that must match these patterns
	absent     []string                  // headers that must not be sent
	admin      bool                      // a /_gw route: the gateway's own, not a container's
	check      func(t *testing.T, resp *http.Response, body []byte)
	// after runs once the row has passed, to remember what later rows need.
	after func(resp *http.Response, body []byte)
}

// TestEveryEndpoint walks every route the gateway serves, and the ones it deliberately does not, in
// the order a client meets them: submit, poll while running, poll after success, the result-file
// and metadata routes, the container's root routes, the admin surface, the errors, and DELETE.
//
// Every Azure-route answer must name Kestrel exactly once, echo x-ms-client-request-id, and carry
// no apim-request-id, because a client must not be able to tell the gateway from a pod by its
// headers. Every /_gw answer must not name Kestrel: the admin surface does not pretend to be a
// container.
func TestEveryEndpoint(t *testing.T) {
	// The containers take a second per call, so the operations are still running when the first
	// polls arrive and there is a window to observe them in.
	const latency = time.Second
	h := newHarness(t, harnessOpts{
		di:   mockazure.Options{Latency: latency},
		read: mockazure.Options{Latency: latency},
		tweak: func(c *config.Config) {
			c.DI.MaxInflight, c.Read.MaxInflight, c.QueueDepth = 8, 8, 8
		},
	})
	gw := regexp.QuoteMeta(h.gateway.URL)
	ops := map[string]string{"{unknown}": ids.New()}
	remember := func(key string) func(*http.Response, []byte) {
		return func(resp *http.Response, _ []byte) {
			ops[key] = opID(resp.Header.Get("Operation-Location"))
		}
	}
	pdf := []byte("%PDF-1.7 conformance")
	results := func(fam, id, version string) string {
		return fam + "/documentModels/prebuilt-layout/analyzeResults/" + id + "?api-version=" + version
	}

	submitted := []endpointCase{
		{
			name: "DI :analyze", method: http.MethodPost, body: pdf,
			path:   diFam + "/documentModels/prebuilt-layout:analyze?api-version=" + apiVersion,
			status: http.StatusAccepted, retryAfter: "1",
			want: map[string]string{"Content-Length": "0"},
			// Fidelity checklist 11: absolute, the literal family segment, modelId four and
			// resultId two segments from the end, ?api-version present, nothing to escape.
			match: map[string]*regexp.Regexp{"Operation-Location": regexp.MustCompile(
				`^` + gw + `/documentintelligence/documentModels/prebuilt-layout/analyzeResults/` +
					guidPattern + `\?api-version=` + apiVersion + `$`)},
			absent: []string{"Location"},
			check:  bodyless,
			after:  remember("{di}"),
		},
		{
			name: "FR :analyze", method: http.MethodPost, body: pdf,
			path:   frFam + "/documentModels/prebuilt-layout:analyze?api-version=" + frAPIVersion,
			status: http.StatusAccepted, retryAfter: "1",
			want: map[string]string{"Content-Length": "0"},
			match: map[string]*regexp.Regexp{"Operation-Location": regexp.MustCompile(
				`^` + gw + `/formrecognizer/documentModels/prebuilt-layout/analyzeResults/` +
					guidPattern + `\?api-version=` + frAPIVersion + `$`)},
			absent: []string{"Location"},
			check:  bodyless,
			after:  remember("{fr}"),
		},
		{
			name: "Read analyze", method: http.MethodPost, body: pdf,
			path:   "/vision/v3.2/read/analyze",
			status: http.StatusAccepted, retryAfter: "1",
			want: map[string]string{"Content-Length": "0"},
			// Fidelity checklist 12: a bare lowercase GUID, no query, no trailing slash.
			match: map[string]*regexp.Regexp{"Operation-Location": regexp.MustCompile(
				`^` + gw + `/vision/v3\.2/read/analyzeResults/` + guidPattern + `$`)},
			absent: []string{"Location"},
			check:  bodyless,
			after:  remember("{read}"),
		},
	}

	running := []endpointCase{
		{name: "DI poll GET running", method: http.MethodGet, path: results(diFam, "{di}", apiVersion),
			status: http.StatusOK, retryAfter: "1", check: inProgressEnvelope},
		{name: "DI poll HEAD running", method: http.MethodHead, path: results(diFam, "{di}", apiVersion),
			status: http.StatusOK, retryAfter: "1", check: headOnly},
		{name: "FR poll GET running", method: http.MethodGet, path: results(frFam, "{fr}", frAPIVersion),
			status: http.StatusOK, retryAfter: "1", check: inProgressEnvelope},
		{name: "FR poll HEAD running", method: http.MethodHead, path: results(frFam, "{fr}", frAPIVersion),
			status: http.StatusOK, retryAfter: "1", check: headOnly},
		{name: "Read poll GET running", method: http.MethodGet, path: "/vision/v3.2/read/analyzeResults/{read}",
			status: http.StatusOK, retryAfter: "1", check: inProgressEnvelope},
		{name: "Read poll HEAD running", method: http.MethodHead, path: "/vision/v3.2/read/analyzeResults/{read}",
			status: http.StatusOK, retryAfter: "1", check: headOnly},
	}

	done := []endpointCase{
		{name: "DI poll GET succeeded", method: http.MethodGet, path: results(diFam, "{di}", apiVersion),
			status: http.StatusOK, check: succeededEnvelope("pages")},
		{name: "DI poll HEAD succeeded", method: http.MethodHead, path: results(diFam, "{di}", apiVersion),
			status: http.StatusOK, check: headOnly},
		// Python omits api-version on a poll; .NET and Java replace it. Any form must work.
		{name: "DI poll GET without api-version", method: http.MethodGet,
			path:   diFam + "/documentModels/prebuilt-layout/analyzeResults/{di}",
			status: http.StatusOK, check: succeededEnvelope("pages")},
		{name: "FR poll GET succeeded", method: http.MethodGet, path: results(frFam, "{fr}", frAPIVersion),
			status: http.StatusOK, check: succeededEnvelope("pages")},
		{name: "FR poll HEAD succeeded", method: http.MethodHead, path: results(frFam, "{fr}", frAPIVersion),
			status: http.StatusOK, check: headOnly},
		{name: "Read poll GET succeeded", method: http.MethodGet, path: "/vision/v3.2/read/analyzeResults/{read}",
			status: http.StatusOK, check: succeededEnvelope("readResults")},
		{name: "Read poll HEAD succeeded", method: http.MethodHead, path: "/vision/v3.2/read/analyzeResults/{read}",
			status: http.StatusOK, check: headOnly},

		// Synchronous analysis: 200 with the result, and no operation to poll.
		{name: "DI :syncAnalyze", method: http.MethodPost, body: pdf,
			path:   diFam + "/documentModels/prebuilt-layout:syncAnalyze?api-version=" + apiVersion,
			status: http.StatusOK, absent: []string{"Operation-Location", "Location"},
			check: succeededEnvelope("pages")},
		{name: "FR :syncAnalyze", method: http.MethodPost, body: pdf,
			path:   frFam + "/documentModels/prebuilt-layout:syncAnalyze?api-version=" + frAPIVersion,
			status: http.StatusOK, absent: []string{"Operation-Location", "Location"},
			check: succeededEnvelope("pages")},
		{name: "Read syncAnalyze", method: http.MethodPost, body: pdf, path: "/vision/v3.2/read/syncAnalyze",
			status: http.StatusOK, absent: []string{"Operation-Location", "Location"},
			check: succeededEnvelope("readResults")},

		// Result files: no job ever has them, because every analysis is synchronous upstream.
		{name: "DI pdf of a known id", method: http.MethodGet,
			path:   diFam + "/documentModels/prebuilt-layout/analyzeResults/{di}/pdf?api-version=" + apiVersion,
			status: http.StatusNotFound, check: diWrapped("NotFound", "OperationNotFound")},
		{name: "DI figure of a known id", method: http.MethodGet,
			path:   diFam + "/documentModels/prebuilt-layout/analyzeResults/{di}/figures/1.1?api-version=" + apiVersion,
			status: http.StatusNotFound, check: diWrapped("NotFound", "OperationNotFound")},
		{name: "FR pdf of a known id", method: http.MethodGet,
			path:   frFam + "/documentModels/prebuilt-layout/analyzeResults/{fr}/pdf?api-version=" + frAPIVersion,
			status: http.StatusNotFound, check: diWrapped("NotFound", "OperationNotFound")},
		{name: "DI pdf of an unknown id", method: http.MethodGet,
			path:   diFam + "/documentModels/prebuilt-layout/analyzeResults/{unknown}/pdf?api-version=" + apiVersion,
			status: http.StatusNotFound, check: diFlatNotFound},
		{name: "DI figure of an unknown id", method: http.MethodGet,
			path:   diFam + "/documentModels/prebuilt-layout/analyzeResults/{unknown}/figures/1.1?api-version=" + apiVersion,
			status: http.StatusNotFound, check: diFlatNotFound},
		{name: "FR figure of an unknown id", method: http.MethodGet,
			path:   frFam + "/documentModels/prebuilt-layout/analyzeResults/{unknown}/figures/1.1?api-version=" + frAPIVersion,
			status: http.StatusNotFound, check: diFlatNotFound},
	}

	// The metadata and model-lifecycle routes are relayed on both families, at the container's own
	// status: the fake has no model-lifecycle operations, so a named one is its wrapped 404.
	for _, fam := range []struct{ prefix, version string }{{diFam, apiVersion}, {frFam, frAPIVersion}} {
		q := "?api-version=" + fam.version
		done = append(done,
			endpointCase{name: fam.prefix + " info", method: http.MethodGet, path: fam.prefix + "/info" + q,
				status: http.StatusOK, check: jsonHas("customDocumentModels")},
			endpointCase{name: fam.prefix + " documentModels", method: http.MethodGet,
				path: fam.prefix + "/documentModels" + q, status: http.StatusOK, check: jsonHas("value")},
			endpointCase{name: fam.prefix + " documentModels/prebuilt-layout", method: http.MethodGet,
				path: fam.prefix + "/documentModels/prebuilt-layout" + q, status: http.StatusOK,
				check: jsonEquals("modelId", "prebuilt-layout")},
			endpointCase{name: fam.prefix + " operations", method: http.MethodGet,
				path: fam.prefix + "/operations" + q, status: http.StatusOK, check: jsonHas("value")},
			endpointCase{name: fam.prefix + " operations/{id}", method: http.MethodGet,
				path: fam.prefix + "/operations/{unknown}" + q, status: http.StatusNotFound,
				check: diWrapped("NotFound", "OperationNotFound")},
			endpointCase{name: fam.prefix + " :analyzeBatch", method: http.MethodPost, body: pdf,
				path:   fam.prefix + "/documentModels/prebuilt-layout:analyzeBatch" + q,
				status: http.StatusBadRequest, check: diWrapped("InvalidRequest", "")},
			endpointCase{name: fam.prefix + " unknown action", method: http.MethodPost, body: pdf,
				path:   fam.prefix + "/documentModels/prebuilt-layout:bogus" + q,
				status: http.StatusNotFound, check: diWrapped("NotFound", "")},
			endpointCase{name: fam.prefix + " :analyze without api-version", method: http.MethodPost,
				body: pdf, path: fam.prefix + "/documentModels/prebuilt-layout:analyze",
				status: http.StatusBadRequest, check: diWrapped("InvalidArgument", "ParameterMissing")},
			endpointCase{name: fam.prefix + " :syncAnalyze without api-version", method: http.MethodPost,
				body: pdf, path: fam.prefix + "/documentModels/prebuilt-layout:syncAnalyze",
				status: http.StatusBadRequest, check: diWrapped("InvalidArgument", "ParameterMissing")},
			endpointCase{name: fam.prefix + " poll of an unknown id", method: http.MethodGet,
				path: results(fam.prefix, "{unknown}", fam.version), status: http.StatusNotFound,
				check: diFlatNotFound},
			endpointCase{name: fam.prefix + " wrong verb on :analyze", method: http.MethodPut,
				path:   fam.prefix + "/documentModels/prebuilt-layout:analyze" + q,
				status: http.StatusMethodNotAllowed, want: map[string]string{"Allow": "GET, HEAD, POST"}},
			endpointCase{name: fam.prefix + " wrong verb on info", method: http.MethodPost, body: pdf,
				path: fam.prefix + "/info" + q, status: http.StatusMethodNotAllowed,
				want: map[string]string{"Allow": "GET, HEAD"}},
		)
	}

	done = append(done,
		// Read's remaining routes and the ones it does not have.
		endpointCase{name: "Read poll of an unknown id", method: http.MethodGet,
			path: "/vision/v3.2/read/analyzeResults/{unknown}", status: http.StatusNotFound,
			check: readWrapped("BadArgument")},
		endpointCase{name: "Read operations alias", method: http.MethodGet,
			path: "/vision/v3.2/read/operations/{unknown}", status: http.StatusNotFound, check: bodyless},
		endpointCase{name: "Read wrong verb on analyze", method: http.MethodPut,
			path: "/vision/v3.2/read/analyze", status: http.StatusMethodNotAllowed,
			want: map[string]string{"Allow": "POST"}},
		endpointCase{name: "Read DELETE a result", method: http.MethodDelete,
			path: "/vision/v3.2/read/analyzeResults/{read}", status: http.StatusMethodNotAllowed,
			want: map[string]string{"Allow": "GET, HEAD"}},

		// The container's own root routes, relayed to the DI container.
		endpointCase{name: "home page", method: http.MethodGet, path: "/", status: http.StatusOK,
			check: textContains("text/html", "Document Intelligence container")},
		endpointCase{name: "swagger", method: http.MethodGet, path: "/swagger", status: http.StatusOK,
			check: textContains("text/html", "swagger-ui")},
		endpointCase{name: "swagger document", method: http.MethodGet, path: "/swagger/v1/swagger.json",
			status: http.StatusOK, check: jsonHas("openapi")},
		endpointCase{name: "usage records", method: http.MethodGet, path: "/records/usage-logs",
			status: http.StatusOK, check: jsonHas("meters")},
		endpointCase{name: "usage records for a month", method: http.MethodGet,
			path: "/records/usage-logs/10/2026", status: http.StatusOK, check: jsonHas("meters")},
		endpointCase{name: "usage records for a non-numeric month", method: http.MethodGet,
			path: "/records/usage-logs/xx/yy", status: http.StatusNotFound, check: bodyless},
		endpointCase{name: "container ready", method: http.MethodGet, path: "/ready", status: http.StatusOK,
			check: exactJSON(`{"ready":"ready"}`)},
		endpointCase{name: "container status", method: http.MethodGet, path: "/status", status: http.StatusOK,
			check: jsonEquals("apiStatus", "Valid")},

		// Paths no container routes.
		endpointCase{name: "unrouted root path", method: http.MethodGet, path: "/nope",
			status: http.StatusNotFound, check: bodyless},
		endpointCase{name: "unrouted family path", method: http.MethodGet, path: diFam + "/nope",
			status: http.StatusNotFound, check: bodyless},
		// Fidelity checklist 23: the container does not serve Image Analysis.
		endpointCase{name: "image analysis", method: http.MethodPost, body: pdf,
			path: "/computervision/imageanalysis:analyze", status: http.StatusNotFound, check: bodyless},

		// The gateway's own surface.
		endpointCase{name: "admin live", method: http.MethodGet, path: "/_gw/live", admin: true,
			status: http.StatusOK, check: exactJSON(`{"status":"ok"}`)},
		endpointCase{name: "admin ready", method: http.MethodGet, path: "/_gw/ready", admin: true,
			status: http.StatusOK, check: exactJSON(`{"status":"ready"}`)},
		endpointCase{name: "admin health", method: http.MethodGet, path: "/_gw/health", admin: true,
			status: http.StatusOK,
			check:  jsonHas("status", "uptime", "build", "store", "capacity", "upstreams", "jobs")},
		endpointCase{name: "admin version", method: http.MethodGet, path: "/_gw/version", admin: true,
			status: http.StatusOK, check: jsonKeysInOrder("version", "commit", "built", "go")},
		endpointCase{name: "admin config", method: http.MethodGet, path: "/_gw/config", admin: true,
			status: http.StatusOK,
			check:  jsonHas("splitPages", "diMaxInflight", "readMaxInflight", "queueDepth", "diEnabled")},
		endpointCase{name: "admin jobs", method: http.MethodGet, path: "/_gw/jobs", admin: true,
			status: http.StatusOK, check: jsonHas("jobs", "count")},
		endpointCase{name: "admin job", method: http.MethodGet, path: "/_gw/jobs/{read}", admin: true,
			status: http.StatusOK, check: jsonHas("id", "surface", "status", "upstreamAttempts", "upstreamParts")},
		endpointCase{name: "admin unknown job", method: http.MethodGet, path: "/_gw/jobs/{unknown}",
			admin: true, status: http.StatusNotFound, check: jsonHas("error")},
		endpointCase{name: "admin metrics", method: http.MethodGet, path: "/_gw/metrics", admin: true,
			status: http.StatusOK, check: textContains("text/plain", "# TYPE gateway_jobs_total gauge")},

		// DELETE last, because it ends the operations the rows above use.
		endpointCase{name: "DI DELETE", method: http.MethodDelete, path: results(diFam, "{di}", apiVersion),
			status: http.StatusNoContent, check: noContent},
		endpointCase{name: "DI poll after DELETE", method: http.MethodGet,
			path: results(diFam, "{di}", apiVersion), status: http.StatusNotFound, check: diFlatNotFound},
		endpointCase{name: "FR DELETE", method: http.MethodDelete, path: results(frFam, "{fr}", frAPIVersion),
			status: http.StatusNoContent, check: noContent},
		endpointCase{name: "FR poll after DELETE", method: http.MethodGet,
			path: results(frFam, "{fr}", frAPIVersion), status: http.StatusNotFound, check: diFlatNotFound},
		endpointCase{name: "DI DELETE an unknown id", method: http.MethodDelete,
			path: results(diFam, "{unknown}", apiVersion), status: http.StatusNotFound, check: diFlatNotFound},
	)

	runCases := func(cases []endpointCase) {
		t.Helper()
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) { runEndpointCase(t, h, tc, ops) })
		}
	}
	runCases(submitted)
	if t.Failed() {
		t.FailNow() // every later row needs the operations
	}
	runCases(running)
	h.awaitSucceeded(h.gateway.URL + results(diFam, ops["{di}"], apiVersion))
	h.awaitSucceeded(h.gateway.URL + results(frFam, ops["{fr}"], frAPIVersion))
	h.awaitSucceeded(h.gateway.URL + "/vision/v3.2/read/analyzeResults/" + ops["{read}"])
	runCases(done)
}

// runEndpointCase sends one row's request and checks everything the row and the route's surface
// promise.
func runEndpointCase(t *testing.T, h *harness, tc endpointCase, ops map[string]string) {
	t.Helper()
	path := tc.path
	for k, v := range ops {
		path = strings.ReplaceAll(path, k, v)
	}
	cid := ids.New()
	headers := append([]string{"x-ms-client-request-id", cid}, tc.headers...)
	resp, body := h.do(tc.method, path, tc.body, headers...)

	if resp.StatusCode != tc.status {
		t.Fatalf("%s %s: status %d, want %d\n%s", tc.method, path, resp.StatusCode, tc.status,
			truncate(string(body)))
	}
	if got := resp.Header.Get("Retry-After"); got != tc.retryAfter {
		t.Errorf("Retry-After = %q, want %q: it belongs only on a 202, an in-progress poll, a 429 "+
			"and a 503", got, tc.retryAfter)
	}
	for k, v := range tc.want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for k, re := range tc.match {
		if got := resp.Header.Get(k); !re.MatchString(got) {
			t.Errorf("%s = %q, want a match for %s", k, got, re)
		}
	}
	for _, k := range tc.absent {
		if got := resp.Header.Get(k); got != "" {
			t.Errorf("%s = %q, want none", k, got)
		}
	}
	if tc.admin {
		if got := resp.Header.Values("Server"); len(got) != 0 {
			t.Errorf("Server = %q on the gateway's own surface, which does not pretend to be a "+
				"container", got)
		}
	} else {
		if got := resp.Header.Values("Server"); len(got) != 1 || got[0] != "Kestrel" {
			t.Errorf("Server = %q, want exactly one Kestrel, as every container answer has", got)
		}
		if got := resp.Header.Get("apim-request-id"); got != "" {
			t.Errorf("apim-request-id = %q; the containers do not send it", got)
		}
		if got := resp.Header.Get("x-ms-client-request-id"); got != cid {
			t.Errorf("x-ms-client-request-id echoed as %q, want %q", got, cid)
		}
	}
	if tc.check != nil {
		tc.check(t, resp, body)
	}
	if tc.after != nil && !t.Failed() {
		tc.after(resp, body)
	}
}

// bodyless is Kestrel's answer to a path it does not route: no body, no content type.
func bodyless(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	if len(body) != 0 {
		t.Errorf("body is %q, want none", truncate(string(body)))
	}
	if got := resp.Header.Get("Content-Length"); got != "0" {
		t.Errorf("Content-Length = %q, want 0", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q on an empty body", got)
	}
}

// noContent is a 204: no body, and — as RFC 9110 requires of a 204, and net/http and Kestrel both
// enforce — no Content-Length either.
func noContent(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	if len(body) != 0 {
		t.Errorf("204 carries a %d-byte body", len(body))
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Errorf("204 carries Content-Length %q", got)
	}
}

// headOnly is a HEAD answer: the GET's headers, no body.
func headOnly(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	if len(body) != 0 {
		t.Errorf("HEAD returned a %d-byte body", len(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want the GET's application/json", ct)
	}
	if resp.ContentLength <= 0 {
		t.Errorf("Content-Length = %d, want the GET body's length", resp.ContentLength)
	}
}

// inProgressEnvelope is a poll of an operation that has not finished: the three envelope members
// in the container's order and nothing else, because a null analyzeResult makes .NET's
// FromLroResponse throw.
func inProgressEnvelope(t *testing.T, _ *http.Response, body []byte) {
	t.Helper()
	assertKeys(t, body, "status", "createdDateTime", "lastUpdatedDateTime")
	var env struct{ Status string }
	_ = json.Unmarshal(body, &env)
	if env.Status != "notStarted" && env.Status != "running" {
		t.Errorf("status = %q, want notStarted or running", env.Status)
	}
}

// succeededEnvelope is a terminal envelope whose analyzeResult carries member.
func succeededEnvelope(member string) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, resp *http.Response, body []byte) {
		t.Helper()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		assertKeys(t, body, "status", "createdDateTime", "lastUpdatedDateTime", "analyzeResult")
		var env struct {
			Status              string
			CreatedDateTime     string
			LastUpdatedDateTime string
			AnalyzeResult       map[string]json.RawMessage
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("envelope does not decode: %v", err)
		}
		if env.Status != "succeeded" {
			t.Errorf("status = %q, want succeeded", env.Status)
		}
		for _, ts := range []string{env.CreatedDateTime, env.LastUpdatedDateTime} {
			if _, err := time.Parse(time.RFC3339, ts); err != nil {
				t.Errorf("timestamp %q is not RFC 3339: %v", ts, err)
			}
		}
		if _, ok := env.AnalyzeResult[member]; !ok {
			t.Errorf("analyzeResult has no %s: %s", member, truncate(string(body)))
		}
	}
}

// diFlatNotFound is the Document Intelligence container's one unwrapped error: an unknown or
// expired result id.
func diFlatNotFound(t *testing.T, _ *http.Response, body []byte) {
	t.Helper()
	exactJSON(`{"code":"NotFound","message":"Analyze result does not exist."}`)(t, nil, body)
}

// diWrapped is a wrapped Document Intelligence error with these codes; inner "" means none is
// checked.
func diWrapped(code, inner string) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, _ *http.Response, body []byte) {
		t.Helper()
		var e struct {
			Error *struct {
				Code       string
				Message    string
				InnerError *struct{ Code string } `json:"innererror"`
			}
		}
		if err := json.Unmarshal(body, &e); err != nil || e.Error == nil {
			t.Fatalf("not a wrapped Document Intelligence error: %s", truncate(string(body)))
		}
		if e.Error.Code != code {
			t.Errorf("error.code = %q, want %q", e.Error.Code, code)
		}
		if e.Error.Message == "" {
			t.Error("error.message is empty")
		}
		if inner != "" && (e.Error.InnerError == nil || e.Error.InnerError.Code != inner) {
			t.Errorf("error.innererror = %+v, want code %q", e.Error.InnerError, inner)
		}
	}
}

// readWrapped is a Read error: the container wraps every one, whatever its own contract says.
func readWrapped(code string) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, _ *http.Response, body []byte) {
		t.Helper()
		var e struct {
			Error *struct{ Code, Message string }
		}
		if err := json.Unmarshal(body, &e); err != nil || e.Error == nil {
			t.Fatalf("not a wrapped Read error: %s", truncate(string(body)))
		}
		if e.Error.Code != code || e.Error.Message == "" {
			t.Errorf("error = %+v, want code %q with a message", *e.Error, code)
		}
	}
}

// jsonHas checks a JSON object body carries each member.
func jsonHas(members ...string) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, _ *http.Response, body []byte) {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("body is not a JSON object (%v): %s", err, truncate(string(body)))
		}
		for _, k := range members {
			if _, ok := m[k]; !ok {
				t.Errorf("body has no %q: %s", k, truncate(string(body)))
			}
		}
	}
}

// jsonEquals checks one string member of a JSON object body.
func jsonEquals(member, want string) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, _ *http.Response, body []byte) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("body is not a JSON object (%v): %s", err, truncate(string(body)))
		}
		if m[member] != want {
			t.Errorf("%s = %v, want %q", member, m[member], want)
		}
	}
}

// jsonKeysInOrder checks a JSON object body's members, in order.
func jsonKeysInOrder(keys ...string) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, _ *http.Response, body []byte) {
		t.Helper()
		assertKeys(t, body, keys...)
	}
}

// exactJSON checks the body is exactly these bytes.
func exactJSON(want string) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, _ *http.Response, body []byte) {
		t.Helper()
		if string(body) != want {
			t.Errorf("body = %s, want %s", truncate(string(body)), want)
		}
	}
}

// textContains checks a relayed non-JSON body's type and that it carries text.
func textContains(contentType, text string) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, resp *http.Response, body []byte) {
		t.Helper()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, contentType) {
			t.Errorf("Content-Type = %q, want %s", ct, contentType)
		}
		if !bytes.Contains(body, []byte(text)) {
			t.Errorf("body does not contain %q: %s", text, truncate(string(body)))
		}
	}
}

// assertKeys checks a JSON object's top-level members are exactly keys, in that order. Member
// order is part of what a byte-preserving client sees, and a Go map would have sorted it.
func assertKeys(t *testing.T, body []byte, keys ...string) {
	t.Helper()
	got, err := topKeys(body)
	if err != nil {
		t.Fatalf("body is not a JSON object (%v): %s", err, truncate(string(body)))
	}
	if strings.Join(got, ",") != strings.Join(keys, ",") {
		t.Errorf("members = %v, want %v", got, keys)
	}
}

// topKeys lists a JSON object's top-level members in order.
func topKeys(body []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("conformance: body starts with %v, not an object", tok)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}
