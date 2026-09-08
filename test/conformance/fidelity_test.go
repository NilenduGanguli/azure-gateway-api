package conformance

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// TestDISubmitReturnsExactly202 covers the strictest shared requirement: Python's
// _analyze_document_initial, .NET's StatusCodeClassifier{202}, Java's @ExpectedResponses({202})
// and JS's isUnexpected all accept 202 and nothing else, not even 200 or 201.
func TestDISubmitReturnsExactly202(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit returned %d, want exactly 202", resp.StatusCode)
	}
}

// TestDIOperationLocationShape checks every constraint the four SDK families place on the
// Document Intelligence poll URL at once.
func TestDIOperationLocationShape(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	defer func() { _ = resp.Body.Close() }()

	loc := resp.Header.Get("Operation-Location")
	if loc == "" {
		t.Fatal("no Operation-Location header")
	}

	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Operation-Location is not a valid URL: %v", err)
	}
	if !u.IsAbs() {
		t.Errorf("Operation-Location %q is relative; Python joins a relative value onto its own "+
			"endpoint and produces a doubled /documentintelligence/ path", loc)
	}
	// Python _patch.py, .NET OperationWithId.cs, Java PollingUtils and JS pollingHelper all
	// hard-code [^:]+://[^/]+/documentintelligence/.+/([^?/]+) to derive the operation id.
	if !strings.Contains(u.Path, "/documentintelligence/") {
		t.Errorf("Operation-Location path %q lacks the literal /documentintelligence/ segment", u.Path)
	}
	// Azure.AI.FormRecognizer 4.x discards the URL and counts backwards after Split('/','?'):
	// resultId two segments from the end, modelId four.
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 4 {
		t.Fatalf("Operation-Location path %q has too few segments", u.Path)
	}
	if got := segs[len(segs)-2]; got != "analyzeResults" {
		t.Errorf("second-from-last segment is %q, want analyzeResults", got)
	}
	if got := segs[len(segs)-4]; got != "documentModels" {
		t.Errorf("fourth-from-last segment is %q, want documentModels", got)
	}
	// Python never re-appends api-version; .NET and Java replace it; JS appends it. Emitting it
	// satisfies all four.
	if got := u.Query().Get("api-version"); got != apiVersion {
		t.Errorf("Operation-Location api-version is %q, want %q", got, apiVersion)
	}
	if strings.ContainsAny(loc, "{}| ^") {
		t.Errorf("Operation-Location %q contains a character that raises in Python's str.format "+
			"or makes Java's new URI(path) throw", loc)
	}
}

// TestDISubmitOmitsLocationHeader covers the trap that silently corrupts results: Python's
// get_final_get_url and JS's operation.ts both issue an extra final GET to a Location header
// captured from the 202, then parse that response as the analyze result.
func TestDISubmitOmitsLocationHeader(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	defer func() { _ = resp.Body.Close() }()

	if v := resp.Header.Get("Location"); v != "" {
		t.Errorf("202 carries Location: %q; Python and JS would issue a bogus extra final GET to it", v)
	}
}

// TestRetryAfterIsIntegerSeconds covers a hard failure rather than a slowdown: the Python
// Document Intelligence client runs Retry-After through _deserialize("int", ...), so an HTTP-date
// raises DeserializationError and fails the call outright.
func TestRetryAfterIsIntegerSeconds(t *testing.T) {
	h := newHarness(t, harnessOpts{di: mockazure.Options{PollsBeforeSuccess: 1}})

	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	assertIntegerRetryAfter(t, "202 submit", resp.Header.Get("Retry-After"))
	_ = resp.Body.Close()

	poll := h.get(loc)
	assertIntegerRetryAfter(t, "in-progress poll", poll.Header.Get("Retry-After"))
	_ = poll.Body.Close()
}

func assertIntegerRetryAfter(t *testing.T, where, v string) {
	t.Helper()
	if v == "" {
		t.Errorf("%s has no Retry-After; azure-core then falls back to its 30s default polling "+
			"interval, making a fast gateway look thirty times slower", where)
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Errorf("%s Retry-After is %q, not integer seconds; the Python DI client raises "+
			"DeserializationError on an HTTP-date", where, v)
		return
	}
	if n < 1 {
		t.Errorf("%s Retry-After is %d, want at least 1", where, n)
	}
}

// TestNoRetryAfterOnNonRetriableErrors covers azure-core's RetryPolicy.is_retry, which retries
// *any* response >= 400 carrying Retry-After, bypassing its method allowlist, up to 10 times. A
// 404 with the header turns one client call into eleven.
func TestNoRetryAfterOnNonRetriableErrors(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	cases := []struct{ name, path string }{
		{"di unknown result id", "/documentintelligence/documentModels/prebuilt-layout/analyzeResults/" +
			"00000000-0000-4000-8000-000000000000?api-version=" + apiVersion},
		{"read unknown operation id", "/vision/v3.2/read/analyzeResults/00000000-0000-4000-8000-000000000000"},
		{"unrouted path", "/documentintelligence/nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.get(tc.path)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode < 400 {
				t.Fatalf("expected an error status, got %d", resp.StatusCode)
			}
			if v := resp.Header.Get("Retry-After"); v != "" {
				t.Errorf("%d response carries Retry-After %q; azure-core would retry it 10 times",
					resp.StatusCode, v)
			}
		})
	}
}

// TestUnknownIDMatchesTheContainersNotTheSpec checks the error shapes a probe found real
// containers emitting, which are not the ones their published contracts describe.
//
// Document Intelligence wraps every error except this one, which comes back flat — the divergence
// Microsoft has acknowledged. Computer Vision Read wraps everything, including the errors its own
// Ocr.json models as flat. The gateway reproduces both, because a client being unable to tell the
// gateway from the container is the entire product.
func TestUnknownIDMatchesTheContainersNotTheSpec(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	t.Run("document intelligence is flat here", func(t *testing.T) {
		resp := h.get("/documentintelligence/documentModels/prebuilt-layout/analyzeResults/" +
			"00000000-0000-4000-8000-000000000000?api-version=" + apiVersion)
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("got %d, want 404", resp.StatusCode)
		}
		body := decode(t, resp)
		if _, wrapped := body["error"]; wrapped {
			t.Error("wrapped; the container returns this one error flat")
		}
		if body["code"] != "NotFound" {
			t.Errorf("code is %v, want NotFound", body["code"])
		}
		if body["message"] != "Analyze result does not exist." {
			t.Errorf("message is %q, want the container's own wording", body["message"])
		}
	})

	t.Run("read is wrapped", func(t *testing.T) {
		resp := h.get("/vision/v3.2/read/analyzeResults/00000000-0000-4000-8000-000000000000")
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("got %d, want 404", resp.StatusCode)
		}
		body := decode(t, resp)
		inner, wrapped := body["error"].(map[string]any)
		if !wrapped {
			t.Fatalf("flat; the container wraps every Read error. got %v", body)
		}
		// The Read enum contains no not-found code at all; the container answers BadArgument.
		if inner["code"] != "BadArgument" {
			t.Errorf("code is %v, want BadArgument", inner["code"])
		}
	})
}

// TestDocumentedCompatRestoresTheSpecShapes covers ERROR_COMPAT=documented, for a client written
// against the published SDK models rather than against these containers.
func TestDocumentedCompatRestoresTheSpecShapes(t *testing.T) {
	azerr.SetCompat(azerr.CompatDocumented)
	t.Cleanup(func() { azerr.SetCompat(azerr.CompatObserved) })

	h := newHarness(t, harnessOpts{})

	resp := h.get("/documentintelligence/documentModels/prebuilt-layout/analyzeResults/" +
		"00000000-0000-4000-8000-000000000000?api-version=" + apiVersion)
	di := decode(t, resp)
	_ = resp.Body.Close()
	if _, wrapped := di["error"]; !wrapped {
		t.Errorf("documented mode must wrap Document Intelligence errors, got %v", di)
	}

	resp = h.get("/vision/v3.2/read/analyzeResults/00000000-0000-4000-8000-000000000000")
	rd := decode(t, resp)
	_ = resp.Body.Close()
	if _, wrapped := rd["error"]; wrapped {
		t.Errorf("documented mode must leave Read errors flat, got %v", rd)
	}
	if rd["code"] == nil {
		t.Errorf("documented Read error needs a top-level code, got %v", rd)
	}
}

// TestReadOperationLocationIsBare covers the Read SDK's lack of a poller: callers follow
// Microsoft's canonical samples, which do split("/")[-1] in Python and Substring(len-36) +
// Guid.Parse in .NET. A query string or trailing slash breaks both.
func TestReadOperationLocationIsBare(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	resp := h.post(readAnalyze, "fake-image-bytes")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit returned %d, want exactly 202", resp.StatusCode)
	}
	loc := resp.Header.Get("Operation-Location")
	if loc == "" {
		t.Fatal("no Operation-Location header")
	}
	if strings.Contains(loc, "?") {
		t.Errorf("Operation-Location %q has a query string; the Python sample's split(\"/\")[-1] "+
			"then percent-encodes it into the path and 404s", loc)
	}
	if strings.HasSuffix(loc, "/") {
		t.Errorf("Operation-Location %q has a trailing slash", loc)
	}
	if strings.Contains(loc, "#") {
		t.Errorf("Operation-Location %q has a fragment", loc)
	}
	last := loc[strings.LastIndex(loc, "/")+1:]
	if len(last) != ids.Len {
		t.Errorf("trailing id %q is %d chars, want %d; the .NET sample does Substring(len-36) "+
			"then Guid.Parse", last, len(last), ids.Len)
	}
	if !ids.Valid(last) {
		t.Errorf("trailing id %q does not parse as a GUID", last)
	}
}

// TestSucceededEnvelopeNestsAnalyzeResult covers .NET's AnalyzeResult.FromLroResponse, which does
// GetProperty("analyzeResult") on the terminal body and throws if it is absent, and the Python
// and JS pollers, which would issue a bogus extra GET if resourceLocation were present.
func TestSucceededEnvelopeNestsAnalyzeResult(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	body := h.pollUntil(loc, "succeeded")
	if body["status"] != "succeeded" {
		t.Fatalf("status is %v, want succeeded", body["status"])
	}
	if _, ok := body["analyzeResult"]; !ok {
		t.Error("terminal body has no analyzeResult; .NET's GetProperty(\"analyzeResult\") throws")
	}
	if _, ok := body["resourceLocation"]; ok {
		t.Error("terminal body carries resourceLocation; Python, Java and JS would issue an " +
			"extra final GET to it and parse that response as the result")
	}
	for _, k := range []string{"createdDateTime", "lastUpdatedDateTime"} {
		if _, ok := body[k]; !ok {
			t.Errorf("terminal body is missing %s", k)
		}
	}
}

// TestStatusStringsAreExact covers spelling, which unlike casing is not forgiven anywhere:
// "cancelled" is terminal in JS only, and "skipped" in none of the four.
func TestStatusStringsAreExact(t *testing.T) {
	h := newHarness(t, harnessOpts{di: mockazure.Options{PollsBeforeSuccess: 2}})
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	allowed := map[string]bool{"notStarted": true, "running": true, "succeeded": true, "failed": true}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		poll := h.get(loc)
		body := decode(t, poll)
		_ = poll.Body.Close()

		status, _ := body["status"].(string)
		if !allowed[status] {
			t.Fatalf("emitted status %q; only notStarted, running, succeeded and failed are "+
				"terminal-safe across all four SDK families", status)
		}
		if status == "succeeded" || status == "failed" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("operation never reached a terminal status")
}

// TestFailedAnalysisIsHTTP200 covers the dominant failure mode on both surfaces: an analysis
// failure is reported at HTTP 200 with status "failed", not as an HTTP error.
func TestFailedAnalysisIsHTTP200(t *testing.T) {
	h := newHarness(t, harnessOpts{di: mockazure.Options{FailAnalysis: true}})
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	poll := h.get(loc)
	defer func() { _ = poll.Body.Close() }()
	if poll.StatusCode != http.StatusOK {
		// pollUntil would have failed already, but assert explicitly for a clear message.
		t.Fatalf("failed operation returned HTTP %d, want 200", poll.StatusCode)
	}
	body := h.pollUntil(loc, "failed")
	if body["status"] != "failed" {
		t.Fatalf("status is %v, want failed", body["status"])
	}
	if _, ok := body["error"]; !ok {
		t.Error("failed DI envelope has no error object")
	}
}

// TestPollBodyIsAlwaysJSONWithStatus covers Python's BadResponse on an empty body, .NET's
// treatment of a zero-length stream as failure, and Java's NPE.
func TestPollBodyIsAlwaysJSONWithStatus(t *testing.T) {
	h := newHarness(t, harnessOpts{di: mockazure.Options{PollsBeforeSuccess: 1}})
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()

	poll := h.get(loc)
	body := decode(t, poll)
	_ = poll.Body.Close()
	if _, ok := body["status"]; !ok {
		t.Error("in-progress poll body has no top-level status")
	}
	if ct := poll.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("poll Content-Type is %q, want application/json", ct)
	}
	if poll.Header.Get("Content-Length") == "" {
		t.Error("poll has no Content-Length")
	}
}

// TestNeverRedirects covers .NET, which builds its handler with AllowAutoRedirect=false and
// surfaces any 3xx as a terminal failure on both the submit and the poll.
func TestNeverRedirects(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	for _, path := range []string{
		diAnalyze, readAnalyze,
		"/documentintelligence/documentModels/prebuilt-layout/analyzeResults/" +
			"00000000-0000-4000-8000-000000000000?api-version=" + apiVersion,
		"/vision/v3.2/read/analyzeResults/00000000-0000-4000-8000-000000000000",
	} {
		var resp *http.Response
		if strings.Contains(path, "analyzeResults") {
			resp = h.get(path)
		} else {
			resp = h.post(path, "body")
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			t.Errorf("%s returned %d; .NET treats any 3xx as terminal", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// TestNeverCompressesUnlessAsked covers .NET, which sends no Accept-Encoding at all and cannot
// decompress, and Node, which accepts gzip and deflate but never br.
func TestNeverCompressesUnlessAsked(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	resp := h.post(diAnalyze, "%PDF-1.7 fake")
	loc := resp.Header.Get("Operation-Location")
	_ = resp.Body.Close()
	h.pollUntil(loc, "succeeded")

	req, _ := http.NewRequest(http.MethodGet, loc, nil)
	req.Header.Del("Accept-Encoding")
	// Go's transport adds gzip automatically unless the header is set explicitly; setting it to
	// identity reproduces a .NET client, which advertises nothing and cannot decompress.
	req.Header.Set("Accept-Encoding", "identity")
	got, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	defer func() { _ = got.Body.Close() }()

	if enc := got.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		t.Errorf("response is %s-encoded although the client asked for identity", enc)
	}
}

// TestAdmissionReturns429WithRetryAfter covers the configured backpressure: with QueueDepth 0 a
// submit is refused as soon as every worker is busy. The JS SDK retries a 429 only when a
// retry-after header is present, so it is mandatory here.
func TestAdmissionReturns429WithRetryAfter(t *testing.T) {
	h := newHarness(t, harnessOpts{
		di: mockazure.Options{Latency: 2 * time.Second},
		tweak: func(c *config.Config) {
			c.DI.MaxInflight = 1
			c.QueueDepth = 0
		},
	})

	first := h.post(diAnalyze, "%PDF-1.7 fake")
	_ = first.Body.Close()
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first submit returned %d, want 202", first.StatusCode)
	}

	// Give the worker a moment to claim the only slot.
	time.Sleep(150 * time.Millisecond)

	second := h.post(diAnalyze, "%PDF-1.7 fake")
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second submit returned %d, want 429 while the only worker is busy", second.StatusCode)
	}
	assertIntegerRetryAfter(t, "429", second.Header.Get("Retry-After"))

	body := decode(t, second)
	if body["error"] == nil {
		t.Error("429 on the DI surface must use the wrapped error shape")
	}
}

// TestChunkedRequestBodyAccepted covers SDKs streaming a file-like body, which send
// Transfer-Encoding: chunked with no Content-Length.
func TestChunkedRequestBodyAccepted(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("%PDF-1.7 streamed without a length"))
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, h.gateway.URL+diAnalyze, pr)
	req.Header.Set("Content-Type", "application/pdf")
	req.ContentLength = -1 // force chunked

	resp, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("chunked submit: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chunked submit returned %d, want 202", resp.StatusCode)
	}
}
