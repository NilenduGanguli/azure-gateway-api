package mockazure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/merge"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// pagedRoute is one synchronous analyze route of a paged fake, with its asynchronous sibling.
type pagedRoute struct {
	name         string
	surface      Surface
	sync, submit string
}

var pagedRoutes = []pagedRoute{
	{"document intelligence", DI,
		"/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30",
		"/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30"},
	{"legacy form recognizer family", DI,
		"/formrecognizer/documentModels/prebuilt-layout:syncAnalyze?api-version=2023-07-31",
		"/formrecognizer/documentModels/prebuilt-layout:analyze?api-version=2023-07-31"},
	{"computer vision read", Read, "/vision/v3.2/read/syncAnalyze", "/vision/v3.2/read/analyze"},
}

// withQuery appends query parameters to a path that may already carry some.
func withQuery(path, query string) string {
	if query == "" {
		return path
	}
	if strings.Contains(path, "?") {
		return path + "&" + query
	}
	return path + "?" + query
}

// partOf cuts pages out of an n-page test PDF with the gateway's own splitter, so the part is
// exactly what the gateway would send.
func partOf(t *testing.T, n int, pages ...int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(path, pdftest.Make(n), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := pdfsplit.NewSplitter(1, time.Minute).Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open the test PDF: %v", err)
	}
	defer doc.Close()
	var part bytes.Buffer
	if err := doc.WritePart(context.Background(), pages, &part); err != nil {
		t.Fatalf("cut pages %v: %v", pages, err)
	}
	return part.Bytes()
}

// letterPDF returns an n-page PDF whose pages are all US Letter, as an ordinary document's are:
// nothing about them names an original page. It rewrites pdftest's MediaBoxes in place, keeping
// every byte offset, so the cross-reference table stays right.
func letterPDF(n int) []byte {
	return regexp.MustCompile(`/MediaBox \[0 0 6\d\d 800\]`).
		ReplaceAll(pdftest.Make(n), []byte("/MediaBox [0 0 612 792]"))
}

// postDoc POSTs body with the given content type and reads the whole answer.
func postDoc(t *testing.T, c *Container, path, contentType string, body []byte, header ...string) syncReply {
	t.Helper()
	r, err := tryPostDoc(context.Background(), c, path, contentType, body, header...)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return r
}

// tryPostDoc is postDoc for goroutines: it never calls t.
func tryPostDoc(ctx context.Context, c *Container, path, contentType string, body []byte,
	header ...string) (syncReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL()+path, bytes.NewReader(body))
	if err != nil {
		return syncReply{}, err
	}
	req.Header.Set("Content-Type", contentType)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := newPerAttemptClient().Do(req)
	if err != nil {
		return syncReply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	return syncReply{status: resp.StatusCode, header: resp.Header, length: resp.ContentLength,
		body: raw, err: err}, nil
}

// expected is the analyzeResult docgen gives for pages on the route's surface.
func expected(s Surface, pages []int, n docgen.Numbering, o docgen.Options) []byte {
	if s == DI {
		return docgen.DI(pages, n, o)
	}
	return docgen.Read(pages, n)
}

// succeeded wraps a result in the envelope the synchronous route answers with, written out here
// rather than taken from the code under test.
func succeeded(result []byte) string {
	return `{"status":"succeeded","createdDateTime":"2024-11-30T00:00:00Z",` +
		`"lastUpdatedDateTime":"2024-11-30T00:00:01Z","analyzeResult":` + string(result) + `}`
}

// wantSucceeded asserts a 200 whose body is exactly the envelope around want.
func wantSucceeded(t *testing.T, r syncReply, want []byte) {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("status %d, want 200: %.300s", r.status, r.body)
	}
	if got := string(r.body); got != succeeded(want) {
		t.Errorf("body differs from the envelope around docgen's result\n got: %.400s\nwant: %.400s",
			got, succeeded(want))
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
}

func wantCalls(t *testing.T, c *Container, want ...[]int) {
	t.Helper()
	got := c.Calls()
	if !slices.EqualFunc(got, want, slices.Equal[[]int]) {
		t.Errorf("Calls is %v, want %v", got, want)
	}
}

func pageRange(from, to int) []int {
	var ps []int
	for p := from; p <= to; p++ {
		ps = append(ps, p)
	}
	return ps
}

func TestPagedWholeDocumentUsesOriginalNumbering(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface})
			defer c.Close()

			r := postDoc(t, c, rt.sync, "application/pdf", pdftest.Make(12))
			wantSucceeded(t, r, expected(rt.surface, pageRange(1, 12), docgen.Original, docgen.Options{}))
			wantCalls(t, c, pageRange(1, 12))
		})
	}
}

func TestPagedPartUsesLocalNumbering(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface})
			defer c.Close()

			r := postDoc(t, c, rt.sync, "application/pdf", partOf(t, 12, 4, 5, 6))
			wantSucceeded(t, r, expected(rt.surface, []int{4, 5, 6}, docgen.Local, docgen.Options{}))
			wantCalls(t, c, []int{4, 5, 6})
		})
	}
}

// pages= selects page numbers of the body, as the service does, and the result is numbered by
// original page. That is the gateway's re-check: the original sent with a part's pages.
func TestPagedPagesParameter(t *testing.T) {
	for _, rt := range pagedRoutes {
		for _, tc := range []struct {
			name  string
			body  func(t *testing.T) []byte
			pages string
			want  []int
		}{
			{"a range of the original", func(*testing.T) []byte { return pdftest.Make(12) }, "4-6", []int{4, 5, 6}},
			{"terms out of order", func(*testing.T) []byte { return pdftest.Make(12) }, "9,2-3", []int{2, 3, 9}},
			{"every page", func(*testing.T) []byte { return pdftest.Make(12) }, "1-12", pageRange(1, 12)},
			{"a page of a part", func(t *testing.T) []byte { return partOf(t, 12, 4, 5, 6) }, "2", []int{5}},
		} {
			t.Run(rt.name+" "+tc.name, func(t *testing.T) {
				c := NewPaged(PagedOptions{Surface: rt.surface})
				defer c.Close()

				r := postDoc(t, c, withQuery(rt.sync, "pages="+tc.pages), "application/pdf", tc.body(t))
				wantSucceeded(t, r, expected(rt.surface, tc.want, docgen.Original, docgen.Options{}))
				wantCalls(t, c, tc.want)
			})
		}
	}
}

func TestPagedRejectsPagesItCannotSelect(t *testing.T) {
	for _, rt := range pagedRoutes {
		for _, pages := range []string{"0", "13", "4-13", "abc", "3-1", "1,"} {
			t.Run(rt.name+" "+pages, func(t *testing.T) {
				c := NewPaged(PagedOptions{Surface: rt.surface})
				defer c.Close()

				r := postDoc(t, c, withQuery(rt.sync, "pages="+pages), "application/pdf", pdftest.Make(12))
				if r.status != http.StatusBadRequest {
					t.Fatalf("status %d, want 400: %s", r.status, r.body)
				}
				var body struct {
					Error struct {
						Code       string `json:"code"`
						InnerError struct {
							Code string `json:"code"`
						} `json:"innererror"`
					} `json:"error"`
				}
				if err := json.Unmarshal(r.body, &body); err != nil {
					t.Fatalf("body is not an error object (%v): %s", err, r.body)
				}
				want, wantInner := "InvalidPageRange", ""
				if rt.surface == DI {
					want, wantInner = "InvalidArgument", "InvalidParameter"
				}
				if body.Error.Code != want || body.Error.InnerError.Code != wantInner {
					t.Errorf("error %s, want %s/%s", r.body, want, wantInner)
				}
				wantCalls(t, c)
			})
		}
	}
}

func TestPagedStringIndexType(t *testing.T) {
	const sync = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	for _, tc := range []struct {
		name  string
		doc   textunit.Unit
		query string
		want  textunit.Unit
	}{
		{"absent", textunit.TextElements, "", textunit.TextElements},
		{"textElements", textunit.TextElements, "stringIndexType=textElements", textunit.TextElements},
		{"unicodeCodePoint", textunit.TextElements, "stringIndexType=unicodeCodePoint", textunit.UnicodeCodePoint},
		{"utf16CodeUnit", textunit.TextElements, "stringIndexType=utf16CodeUnit", textunit.UTF16CodeUnit},
		{"absent, with a default", textunit.UTF16CodeUnit, "", textunit.UTF16CodeUnit},
		{"named over the default", textunit.UTF16CodeUnit, "stringIndexType=unicodeCodePoint",
			textunit.UnicodeCodePoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: DI, Doc: docgen.Options{Unit: tc.doc}})
			defer c.Close()

			r := postDoc(t, c, withQuery(sync, tc.query), "application/pdf", partOf(t, 9, 7, 8, 9))
			wantSucceeded(t, r, docgen.DI([]int{7, 8, 9}, docgen.Local, docgen.Options{Unit: tc.want}))
		})
	}

	t.Run("unknown unit", func(t *testing.T) {
		c := NewPaged(PagedOptions{Surface: DI})
		defer c.Close()
		r := postDoc(t, c, withQuery(sync, "stringIndexType=bytes"), "application/pdf", pdftest.Make(2))
		if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"InvalidArgument"`) ||
			!strings.Contains(string(r.body), "stringIndexType") {
			t.Errorf("status %d, body %s; want a 400 InvalidArgument naming stringIndexType", r.status, r.body)
		}
		wantCalls(t, c)
	})
}

func TestPagedTitleOnFirstPage(t *testing.T) {
	const sync = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	opts := docgen.Options{TitleOnFirstPage: true}
	c := NewPaged(PagedOptions{Surface: DI, Doc: opts})
	defer c.Close()

	// Each part's first page is judged a title, as a part analysed alone would be; the whole
	// document has only one. That is the document-level difference compare-split must report.
	wantSucceeded(t, postDoc(t, c, sync, "application/pdf", pdftest.Make(6)),
		docgen.DI(pageRange(1, 6), docgen.Original, opts))
	r := postDoc(t, c, sync, "application/pdf", partOf(t, 6, 4, 5, 6))
	wantSucceeded(t, r, docgen.DI([]int{4, 5, 6}, docgen.Local, opts))
	if !strings.Contains(string(r.body), `"role":"title"`) {
		t.Error("the part's first heading is not a title")
	}
}

// A body the fake cannot place in a pdftest document is analysed by position: its pages are
// pages 1..n, and anything that is not a PDF it can read is one page.
func TestPagedBodiesItCannotIdentify(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nnot really an image")
	for _, rt := range pagedRoutes {
		for _, tc := range []struct {
			name, contentType string
			body              []byte
			want              []int
		}{
			{"an image", "image/png", png, []int{1}},
			{"a PDF that does not parse", "application/pdf", []byte("%PDF-1.7 fake"), []int{1}},
			{"an ordinary PDF", "application/pdf", letterPDF(3), []int{1, 2, 3}},
			{"bytes sent as octet-stream", "application/octet-stream", pdftest.Make(4), pageRange(1, 4)},
		} {
			t.Run(rt.name+" "+tc.name, func(t *testing.T) {
				c := NewPaged(PagedOptions{Surface: rt.surface})
				defer c.Close()

				r := postDoc(t, c, rt.sync, tc.contentType, tc.body)
				wantSucceeded(t, r, expected(rt.surface, tc.want, docgen.Original, docgen.Options{}))
				wantCalls(t, c, tc.want)
			})
		}
	}
}

// Document Intelligence also takes a JSON body: base64Source carries the document, urlSource
// names one the container would fetch, which a fake cannot.
func TestPagedJSONBodies(t *testing.T) {
	const sync = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	for _, tc := range []struct {
		name string
		body string
		want []int
	}{
		{"base64Source", `{"base64Source":"` + base64.StdEncoding.EncodeToString(partOf(t, 8, 5, 6, 7, 8)) + `"}`,
			[]int{5, 6, 7, 8}},
		{"urlSource", `{"urlSource":"https://example.invalid/doc.pdf"}`, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: DI})
			defer c.Close()

			r := postDoc(t, c, sync, "application/json", []byte(tc.body))
			wantSucceeded(t, r, docgen.DI(tc.want, docgen.Local, docgen.Options{}))
			wantCalls(t, c, tc.want)
		})
	}
}

// A hang page holds every call that covers it until the client gives up, and only those calls.
func TestPagedHangBlocksUntilTheRequestEnds(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface, Hang: map[int]bool{5: true}})
			defer c.Close()

			// Cut here, not in the goroutine: partOf may call t.Fatal, which only the test's own
			// goroutine may.
			part := partOf(t, 8, 4, 5, 6)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			held := make(chan error, 1)
			go func() {
				_, err := tryPostDoc(ctx, c, rt.sync, "application/pdf", part)
				held <- err
			}()
			waitUntil(t, "the hung call to be recorded", func() bool { return len(c.Calls()) == 1 })

			// Another part is answered while the first is still held.
			wantSucceeded(t, postDoc(t, c, rt.sync, "application/pdf", partOf(t, 8, 7, 8)),
				expected(rt.surface, []int{7, 8}, docgen.Local, docgen.Options{}))
			select {
			case err := <-held:
				t.Fatalf("the call covering page 5 ended (%v) before its client gave up", err)
			default:
			}

			cancel()
			if err := <-held; !errors.Is(err, context.Canceled) {
				t.Errorf("hung call ended with %v, want context canceled", err)
			}
			waitUntil(t, "the abandoned call to leave", func() bool { return c.inflight.Load() == 0 })

			// It hangs every time, not just the first.
			ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel2()
			if _, err := tryPostDoc(ctx2, c, withQuery(rt.sync, "pages=5"), "application/pdf",
				pdftest.Make(8)); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("second call covering page 5 ended with %v, want it to hang", err)
			}
			wantCalls(t, c, []int{4, 5, 6}, []int{7, 8}, []int{5})
		})
	}
}

// HangFirst hangs only the first call covering a page: a retry or a hedge of it is answered, even
// while the first is still held.
func TestPagedHangFirstHangsOnlyTheFirstCall(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface, HangFirst: map[int]bool{5: true, 6: true}})
			defer c.Close()
			part := partOf(t, 8, 4, 5, 6)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			held := make(chan error, 1)
			go func() {
				_, err := tryPostDoc(ctx, c, rt.sync, "application/pdf", part)
				held <- err
			}()
			waitUntil(t, "the first call to be recorded", func() bool { return len(c.Calls()) == 1 })

			want := expected(rt.surface, []int{4, 5, 6}, docgen.Local, docgen.Options{})
			wantSucceeded(t, postDoc(t, c, rt.sync, "application/pdf", part), want) // the hedge
			// Page 6 hung in that first call too, so a call covering it alone is answered.
			wantSucceeded(t, postDoc(t, c, withQuery(rt.sync, "pages=6"), "application/pdf",
				pdftest.Make(8)), expected(rt.surface, []int{6}, docgen.Original, docgen.Options{}))

			cancel()
			if err := <-held; !errors.Is(err, context.Canceled) {
				t.Errorf("first call ended with %v, want context canceled", err)
			}
			wantSucceeded(t, postDoc(t, c, rt.sync, "application/pdf", part), want) // a retry
		})
	}
}

// FailFirst counts, per page, the calls covering it that answer 503 before it is served. A call
// fails while any page it covers still has failures left, and uses one of each such page's.
func TestPagedFailFirstCountsDown(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface, FailFirst: map[int]int{3: 1, 4: 2}})
			defer c.Close()
			part := partOf(t, 6, 3, 4)

			// A part not covering a failing page is untouched.
			wantSucceeded(t, postDoc(t, c, rt.sync, "application/pdf", partOf(t, 6, 5, 6)),
				expected(rt.surface, []int{5, 6}, docgen.Local, docgen.Options{}))

			for n := 1; n <= 2; n++ {
				r := postDoc(t, c, rt.sync, "application/pdf", part)
				if r.status != http.StatusServiceUnavailable {
					t.Fatalf("call %d: status %d, want 503", n, r.status)
				}
				var body containerErrorBody
				if err := json.Unmarshal(r.body, &body); err != nil || body.Error.Code == "" {
					t.Errorf("call %d: not a container error (%v): %s", n, err, r.body)
				}
				if got := r.header.Get("Retry-After"); got != "" {
					t.Errorf("call %d: Retry-After %q; a container sends none", n, got)
				}
			}
			wantSucceeded(t, postDoc(t, c, rt.sync, "application/pdf", part),
				expected(rt.surface, []int{3, 4}, docgen.Local, docgen.Options{}))
			wantCalls(t, c, []int{5, 6}, []int{3, 4}, []int{3, 4}, []int{3, 4})
		})
	}
}

// BadContent answers a terminal error for calls covering the page. With BadOnlyWhenSplit only a
// split part is refused, so the gateway's re-check, the original sent with the part's pages,
// succeeds, as does the whole document.
func TestPagedBadContent(t *testing.T) {
	for _, rt := range pagedRoutes {
		for _, onlySplit := range []bool{false, true} {
			name := rt.name + " always"
			if onlySplit {
				name = rt.name + " only when split"
			}
			t.Run(name, func(t *testing.T) {
				c := NewPaged(PagedOptions{Surface: rt.surface, BadContent: map[int]bool{7: true},
					BadOnlyWhenSplit: onlySplit})
				defer c.Close()

				wantBad := func(label string, r syncReply) {
					t.Helper()
					if r.status != http.StatusBadRequest {
						t.Fatalf("%s: status %d, want 400: %s", label, r.status, r.body)
					}
					want := `{"error":{"code":"InvalidImageFormat","message":"Input data is not a valid image."}}`
					if rt.surface == DI {
						want = `{"error":{"code":"InvalidRequest","message":"Invalid request.",` +
							`"innererror":{"code":"InvalidContent",` +
							`"message":"The file is corrupted or format is unsupported."}}}`
					}
					if string(r.body) != want {
						t.Errorf("%s: body\n%s\nwant\n%s", label, r.body, want)
					}
				}

				wantBad("part", postDoc(t, c, rt.sync, "application/pdf", partOf(t, 12, 5, 6, 7, 8)))
				wantSucceeded(t, postDoc(t, c, rt.sync, "application/pdf", partOf(t, 12, 9, 10)),
					expected(rt.surface, []int{9, 10}, docgen.Local, docgen.Options{}))

				recheck := postDoc(t, c, withQuery(rt.sync, "pages=5-8"), "application/pdf", pdftest.Make(12))
				whole := postDoc(t, c, rt.sync, "application/pdf", pdftest.Make(12))
				if onlySplit {
					wantSucceeded(t, recheck, expected(rt.surface, []int{5, 6, 7, 8}, docgen.Original,
						docgen.Options{}))
					wantSucceeded(t, whole, expected(rt.surface, pageRange(1, 12), docgen.Original,
						docgen.Options{}))
				} else {
					wantBad("re-check", recheck)
					wantBad("whole", whole)
				}
			})
		}
	}
}

// The asynchronous route serves the same analysis as the synchronous one, so the probe can check
// that a container's sync and async results agree. Failure injection is for synchronous calls,
// which are all the gateway makes, so it neither applies to nor is counted for the async route.
func TestPagedAsyncRouteServesTheSameResult(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface, Hang: map[int]bool{2: true},
				FailFirst: map[int]int{3: 5}, BadContent: map[int]bool{4: true}})
			defer c.Close()

			sub := postDoc(t, c, withQuery(rt.submit, "pages=2-4"), "application/pdf", pdftest.Make(6))
			loc := sub.header.Get("Operation-Location")
			if sub.status != http.StatusAccepted || loc == "" {
				t.Fatalf("submit: status %d, Operation-Location %q", sub.status, loc)
			}
			poll := fetch(t, http.MethodGet, loc)
			want := `{"status":"succeeded","createdDateTime":"2024-11-30T00:00:00Z",` +
				`"lastUpdatedDateTime":"2024-11-30T00:00:02Z","analyzeResult":` +
				string(expected(rt.surface, []int{2, 3, 4}, docgen.Original, docgen.Options{})) + `}`
			if poll.status != http.StatusOK || string(poll.body) != want {
				t.Errorf("poll: status %d\n got: %.300s\nwant: %.300s", poll.status, poll.body, want)
			}
			wantCalls(t, c)
			if got := c.SyncCalls(); got != 0 {
				t.Errorf("SyncCalls is %d after an asynchronous call", got)
			}
		})
	}
}

func TestPagedBareBody(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface, SyncBodyBare: true})
			defer c.Close()
			r := postDoc(t, c, rt.sync, "application/pdf", partOf(t, 5, 2, 3))
			want := expected(rt.surface, []int{2, 3}, docgen.Local, docgen.Options{})
			if r.status != http.StatusOK || !bytes.Equal(r.body, want) {
				t.Errorf("status %d, body %.200s; want docgen's bare result", r.status, r.body)
			}
		})
	}
}

// The paged fake sits behind the same HAProxy as the plain one, and checks the same key.
func TestPagedFrontDoorAndKey(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface, APIKey: "secret", BusyFirst: 1,
				StatusFirst: 1, StatusFirstCode: http.StatusGatewayTimeout})
			defer c.Close()
			doc := pdftest.Make(3)

			wantHAProxyBusy(t, "call 1", postDoc(t, c, rt.sync, "application/pdf", doc))
			if r := postDoc(t, c, rt.sync, "application/pdf", doc); r.status != http.StatusGatewayTimeout {
				t.Errorf("call 2: status %d, want 504", r.status)
			}
			if r := postDoc(t, c, rt.sync, "application/pdf", doc); r.status != http.StatusUnauthorized {
				t.Errorf("call 3 without the key: status %d, want 401", r.status)
			}
			wantSucceeded(t, postDoc(t, c, rt.sync, "application/pdf", doc,
				"Ocp-Apim-Subscription-Key", "secret"),
				expected(rt.surface, pageRange(1, 3), docgen.Original, docgen.Options{}))

			// Only the call the container served analysed anything.
			wantCalls(t, c, pageRange(1, 3))
			if c.SyncCalls() != 4 || c.Hits() != 1 {
				t.Errorf("SyncCalls %d, Hits %d; want 4 and 1", c.SyncCalls(), c.Hits())
			}
		})
	}
}

func TestPagedCapacity(t *testing.T) {
	const sync = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	c := NewPaged(PagedOptions{Surface: DI, Capacity: 1, Hang: map[int]bool{1: true}})
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := make(chan error, 1)
	go func() {
		_, err := tryPostDoc(ctx, c, sync, "application/pdf", pdftest.Make(2))
		held <- err
	}()
	waitUntil(t, "the hung call to take the only slot", func() bool { return c.inflight.Load() == 1 })
	wantHAProxyBusy(t, "second call", postDoc(t, c, sync, "application/pdf", partOf(t, 2, 2)))
	cancel()
	<-held
	waitUntil(t, "the slot to come back", func() bool { return c.inflight.Load() == 0 })
	wantSucceeded(t, postDoc(t, c, sync, "application/pdf", partOf(t, 2, 2)),
		docgen.DI([]int{2}, docgen.Local, docgen.Options{}))
}

func TestPagedLatency(t *testing.T) {
	const latency = 300 * time.Millisecond
	c := NewPaged(PagedOptions{Surface: Read, Latency: latency})
	defer c.Close()
	begun := time.Now()
	postDoc(t, c, "/vision/v3.2/read/syncAnalyze", "application/pdf", pdftest.Make(1))
	if took := time.Since(begun); took < latency {
		t.Errorf("answered in %v, before the %v latency", took, latency)
	}
}

// A client that gives up does not change what it sent. The fake still works out the pages of a
// call whose client has left, rather than taking an unparsed body for page 1: that would list the
// call under the wrong pages and spend page 1's injected failures on it.
func TestPagedIdentifiesTheSentPagesAfterTheClientHasLeft(t *testing.T) {
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, rt.sync, nil)
			r.Header.Set("Content-Type", "application/pdf")
			p := &paged{}
			a, ok := p.analyse(httptest.NewRecorder(), r, partOf(t, 9, 7, 8, 9), rt.surface == DI)
			if !ok {
				t.Fatal("analyse refused the call")
			}
			if !slices.Equal(a.pages, []int{7, 8, 9}) || !a.split || a.numbering != docgen.Local {
				t.Errorf("analysed pages %v (split %v, numbering %v), want the part [7 8 9], Local",
					a.pages, a.split, a.numbering)
			}
		})
	}
}

// A call whose client gives up while Latency holds it ends there: it is not listed and spends no
// injected failure or hang, so a test that cancels calls (a hedge's loser, a part that timed out)
// sees only the calls the container actually analysed. Its Capacity slot comes straight back.
func TestPagedCallAbandonedDuringLatencyUsesNothing(t *testing.T) {
	const latency = 400 * time.Millisecond
	for _, rt := range pagedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := NewPaged(PagedOptions{Surface: rt.surface, Latency: latency,
				FailFirst: map[int]int{1: 1, 7: 1}, HangFirst: map[int]bool{8: true}})
			defer c.Close()
			part := partOf(t, 9, 7, 8, 9)

			ctx, cancel := context.WithCancel(context.Background())
			held := make(chan error, 1)
			go func() {
				_, err := tryPostDoc(ctx, c, rt.sync, "application/pdf", part)
				held <- err
			}()
			waitUntil(t, "the call to reach the container", func() bool { return c.Hits() == 1 })
			cancel()
			if err := <-held; !errors.Is(err, context.Canceled) {
				t.Fatalf("abandoned call ended with %v, want context canceled", err)
			}
			waitUntil(t, "the abandoned call's slot to come back", func() bool {
				return c.inflight.Load() == 0
			})
			wantCalls(t, c)

			// Page 7 still owes its failure and page 8 its hang; page 1 was never touched.
			if r := postDoc(t, c, rt.sync, "application/pdf", partOf(t, 9, 7)); r.status !=
				http.StatusServiceUnavailable {
				t.Errorf("first call covering page 7: status %d, want its 503", r.status)
			}
			if r := postDoc(t, c, withQuery(rt.sync, "pages=1"), "application/pdf",
				pdftest.Make(9)); r.status != http.StatusServiceUnavailable {
				t.Errorf("first call covering page 1: status %d, want its 503", r.status)
			}
			hang, stop := context.WithTimeout(context.Background(), latency+300*time.Millisecond)
			defer stop()
			if _, err := tryPostDoc(hang, c, withQuery(rt.sync, "pages=8"), "application/pdf",
				pdftest.Make(9)); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("first call covering page 8 ended with %v, want it to hang", err)
			}
			wantCalls(t, c, []int{7}, []int{1}, []int{8})
		})
	}
}

// Closing the container does not wait out the Latency of a call it is holding: the call is cut,
// on a plain fake and a paged one alike.
func TestCloseCutsCallsWaitingOutLatency(t *testing.T) {
	const latency = 3 * time.Second
	for _, tc := range []struct {
		name string
		make func() *Container
	}{
		{"plain", func() *Container { return NewDI(Options{Latency: latency}) }},
		{"paged", func() *Container { return NewPaged(PagedOptions{Surface: DI, Latency: latency}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.make()
			doc := pdftest.Make(1)
			held := make(chan error, 1)
			go func() {
				r, err := tryPostDoc(context.Background(), c, pagedRoutes[0].sync, "application/pdf", doc)
				if err == nil {
					err = r.err
					if err == nil {
						err = errors.New("answered with status " + http.StatusText(r.status))
					}
				}
				held <- err
			}()
			waitUntil(t, "the call to reach the container", func() bool { return c.Hits() == 1 })

			begun := time.Now()
			c.Close()
			if took := time.Since(begun); took > latency/2 {
				t.Errorf("Close took %v: it waited out the call's %v latency", took, latency)
			}
			if err := <-held; err == nil || strings.HasPrefix(err.Error(), "answered") {
				t.Errorf("the call ended with %v, want the connection cut", err)
			}
		})
	}
}

// Closing the container ends the calls it is holding. They are cut, not answered: an empty 200
// would read as a result with nothing in it.
func TestPagedCloseEndsHungCalls(t *testing.T) {
	const sync = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	c := NewPaged(PagedOptions{Surface: DI, Hang: map[int]bool{1: true}})

	held := make(chan error, 1)
	go func() {
		r, err := tryPostDoc(context.Background(), c, sync, "application/pdf", pdftest.Make(1))
		if err == nil {
			err = r.err
			if err == nil {
				err = errors.New("answered with status " + http.StatusText(r.status))
			}
		}
		held <- err
	}()
	waitUntil(t, "the call to hang", func() bool { return len(c.Calls()) == 1 })

	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close is still waiting for the hung call")
	}
	if err := <-held; err == nil || strings.HasPrefix(err.Error(), "answered") {
		t.Errorf("hung call ended with %v, want the connection cut", err)
	}
	c.Close() // twice is harmless
}

func TestPagedCallsReturnsACopy(t *testing.T) {
	c := NewPaged(PagedOptions{Surface: Read})
	defer c.Close()
	postDoc(t, c, "/vision/v3.2/read/syncAnalyze", "application/pdf", pdftest.Make(2))
	c.Calls()[0][0] = 99
	wantCalls(t, c, []int{1, 2})

	plain := NewDI(Options{})
	defer plain.Close()
	if got := plain.Calls(); got != nil {
		t.Errorf("a plain fake's Calls is %v, want nil", got)
	}
}

// The options a fake cannot honour fail at construction, where the mistake can be named.
func TestNewPagedRejectsBadOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    PagedOptions
	}{
		{"unknown surface", PagedOptions{Surface: Surface(7)}},
		{"StatusFirst without an error status", PagedOptions{StatusFirst: 1, StatusFirstCode: 200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("NewPaged accepted the options")
				}
			}()
			NewPaged(tc.o).Close()
		})
	}
}

func TestListenPagedServesOnTheGivenAddress(t *testing.T) {
	c, err := ListenPaged("127.0.0.1:0", PagedOptions{Surface: DI})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.HasPrefix(c.URL(), "http://127.0.0.1:") {
		t.Errorf("URL is %q", c.URL())
	}
	wantSucceeded(t, postDoc(t, c, pagedRoutes[0].sync, "application/pdf", pdftest.Make(2)),
		docgen.DI(pageRange(1, 2), docgen.Original, docgen.Options{}))

	// The address is taken now, so a second listener on it fails, and says why.
	if _, err := ListenPaged(strings.TrimPrefix(c.URL(), "http://"), PagedOptions{}); err == nil ||
		!strings.Contains(err.Error(), "mockazure") {
		t.Errorf("second listener on the same address: %v", err)
	}
}

// A fake listening on every interface (fakeazure -di :5001) gives a URL a client on this host can
// use, not the wildcard address it listens on: the URL is printed for the gateway's settings and
// is where Operation-Location sends a poll.
// A wildcard listener's URL names the loopback address of its own family. An IPv6 wildcard,
// which Go also uses for ":PORT", is reached through [::1]: on macOS another process may bind
// 127.0.0.1 on the same port, since a specific address outranks the wildcard, and the URL would
// then reach that process instead of the fake.
func TestListenPagedOnEveryInterfaceGivesALoopbackURL(t *testing.T) {
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0"} {
		t.Run(addr, func(t *testing.T) {
			c, err := ListenPaged(addr, PagedOptions{Surface: DI})
			if err != nil {
				t.Skipf("this host cannot listen on %s: %v", addr, err)
			}
			defer c.Close()
			want := `^http://127\.0\.0\.1:[1-9][0-9]*$`
			if host, _, _ := net.SplitHostPort(c.server.Listener.Addr().String()); host == "::" {
				want = `^http://\[::1\]:[1-9][0-9]*$`
			}
			if !regexp.MustCompile(want).MatchString(c.URL()) {
				t.Fatalf("URL is %q, want it to match %s", c.URL(), want)
			}
			sub := postDoc(t, c, pagedRoutes[0].submit, "application/pdf", pdftest.Make(2))
			loc := sub.header.Get("Operation-Location")
			if sub.status != http.StatusAccepted || !strings.HasPrefix(loc, c.URL()+"/") {
				t.Fatalf("submit: status %d, Operation-Location %q; want it under %s", sub.status, loc,
					c.URL())
			}
			if poll := fetch(t, http.MethodGet, loc); poll.status != http.StatusOK {
				t.Errorf("poll of %s: status %d", loc, poll.status)
			}
		})
	}
}

// The fake's contract with the splitting tests: the gateway's merge of its answers for a split
// document's parts is its answer for the whole document, byte for byte. A re-check, the original
// sent with a part's pages, stands in for that part in the same merge.
func TestPagedPartsMergeIntoTheWholeDocument(t *testing.T) {
	const n = 12
	for _, rt := range pagedRoutes {
		for _, unit := range []textunit.Unit{textunit.TextElements, textunit.UTF16CodeUnit} {
			if rt.surface == Read && unit != textunit.TextElements {
				continue
			}
			t.Run(rt.name+" "+unit.String(), func(t *testing.T) {
				c := NewPaged(PagedOptions{Surface: rt.surface, SyncBodyBare: true})
				defer c.Close()
				sync := withQuery(rt.sync, "stringIndexType="+unit.String())
				if rt.surface == Read {
					sync = rt.sync
				}

				var parts []merge.Part
				for i, part := range pdfsplit.Plan(pageRange(1, n), 5) {
					// The second part comes from a re-check of the original, the rest from cuts.
					r := postDoc(t, c, sync, "application/pdf", partOf(t, n, part.Pages...))
					if i == 1 {
						r = postDoc(t, c, withQuery(sync, "pages="+part.Label()), "application/pdf",
							pdftest.Make(n))
					}
					if r.status != http.StatusOK {
						t.Fatalf("part %v: status %d: %s", part.Pages, r.status, r.body)
					}
					path := filepath.Join(t.TempDir(), "part.json")
					if err := os.WriteFile(path, r.body, 0o600); err != nil {
						t.Fatal(err)
					}
					parts = append(parts, merge.Part{Path: path, End: int64(len(r.body)), Pages: part.Pages})
				}

				var merged bytes.Buffer
				var err error
				if rt.surface == DI {
					_, err = merge.DI(&merged, parts, unit)
				} else {
					_, err = merge.Read(&merged, parts)
				}
				if err != nil {
					t.Fatalf("merge: %v", err)
				}
				whole := postDoc(t, c, sync, "application/pdf", pdftest.Make(n))
				if !bytes.Equal(merged.Bytes(), whole.body) {
					t.Errorf("merged parts differ from the whole document\nmerged: %.300s\n whole: %.300s",
						merged.Bytes(), whole.body)
				}
			})
		}
	}
}
