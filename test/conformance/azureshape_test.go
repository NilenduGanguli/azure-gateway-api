package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azschema"
	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
	"github.com/NilenduGanguli/azure-gateway-api/internal/realcut"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// These tests hold the gateway's analyze responses to Microsoft's own description of them. Every
// body a client can get from an analyze route — the poll envelope while running, when succeeded
// and when failed, and the synchronous body — for whole and split documents, on the Document
// Intelligence, Form Recognizer and Read routes, is validated against the official spec vendored
// under test/azure-reference: strictly, so a member the spec does not declare, a value outside a
// closed enum or an integer written as a float fails; then against the invariants real service
// output keeps; then against the member order of real outputs. internal/azschema holds the rules
// and calibrates them on the real outputs. The paged fakes answer in docgen's realistic shape —
// page furniture outside every section, blank pages, captions, selection marks — so a split
// document's parts are as irregular as a real document's.

var (
	azureRefOnce sync.Once
	azureRef     *azschema.Reference
	azureRefErr  error
)

func reference(t *testing.T) *azschema.Reference {
	t.Helper()
	azureRefOnce.Do(func() { azureRef, azureRefErr = azschema.Load("../azure-reference") })
	if azureRefErr != nil {
		t.Fatal(azureRefErr)
	}
	return azureRef
}

func showFindings(fs []azschema.Finding) string {
	var lines []string
	for i, f := range fs {
		if i == 10 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(fs)-10))
			break
		}
		lines = append(lines, f.String())
	}
	return strings.Join(lines, "\n  ")
}

// assertAzureShaped validates one body.
func assertAzureShaped(t *testing.T, what string, f azschema.Family, body []byte, e azschema.Expect) {
	t.Helper()
	if fs := azschema.Errors(reference(t).Envelope(f, body, e)); len(fs) > 0 {
		t.Errorf("%s is not shaped as Azure's:\n  %s\nbody: %.300s", what, showFindings(fs), body)
	}
}

// analyzeResultOf returns an envelope's analyzeResult bytes as served.
func analyzeResultOf(t *testing.T, body []byte) []byte {
	t.Helper()
	var env struct{ AnalyzeResult json.RawMessage }
	if err := json.Unmarshal(body, &env); err != nil || env.AnalyzeResult == nil {
		t.Fatalf("no analyzeResult in %.200s", body)
	}
	return env.AnalyzeResult
}

// shapeRoute is one analyze route and what its results must say.
type shapeRoute struct {
	name         string
	family       azschema.Family
	submit, sync string
	expect       azschema.Expect
}

var shapeRoutes = []shapeRoute{
	{"document intelligence", azschema.DocumentIntelligence,
		"/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30",
		"/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30",
		azschema.Expect{APIVersion: "2024-11-30", ModelID: "prebuilt-layout",
			StringIndexType: "textElements", ContentFormat: "text"}},
	{"form recognizer 2023-07-31", azschema.FormRecognizer,
		"/formrecognizer/documentModels/prebuilt-layout:analyze?api-version=2023-07-31",
		"/formrecognizer/documentModels/prebuilt-layout:syncAnalyze?api-version=2023-07-31",
		azschema.Expect{APIVersion: "2023-07-31", ModelID: "prebuilt-layout", StringIndexType: "textElements"}},
	{"read", azschema.Read, "/vision/v3.2/read/analyze", "/vision/v3.2/read/syncAnalyze",
		azschema.Expect{ModelVersion: "2022-04-30", Unit: "inch"}},
}

// realisticHarness is a gateway splitting at split pages (0: off) in front of paged fakes that
// answer in docgen's realistic shape, slowly enough that a poll right after the 202 sees the
// operation running.
func realisticHarness(t *testing.T, split int) *harness {
	t.Helper()
	doc := docgen.Realistic(0)
	return newSplitHarness(t, split, harnessOpts{
		diPaged:   mockazure.PagedOptions{Doc: doc, Latency: 150 * time.Millisecond},
		readPaged: mockazure.PagedOptions{Latency: 150 * time.Millisecond},
	})
}

// withQuery appends query parameters to a path that may already carry some.
func withQuery(path, query string) string {
	switch {
	case query == "":
		return path
	case strings.Contains(path, "?"):
		return path + "&" + query
	}
	return path + "?" + query
}

func pagesFrom(first, last int) []int {
	var out []int
	for p := first; p <= last; p++ {
		out = append(out, p)
	}
	return out
}

// TestResponsesAreAzureShaped: whole documents (no split, and documents too short to split) and
// split ones, a pages= selection, on every route, by poll and synchronously. The merged result
// must also be byte for byte the whole document's, and the synchronous one the asynchronous one's.
func TestResponsesAreAzureShaped(t *testing.T) {
	whole, split := realisticHarness(t, 0), realisticHarness(t, 5)
	running := 0 // first polls that saw the operation still running
	defer func() {
		if running == 0 {
			t.Error("no first poll saw a running operation, so no running envelope was validated")
		}
	}()
	for _, rt := range shapeRoutes {
		for _, tc := range []struct {
			name   string
			pages  int    // of the pdftest document
			query  string // added to the route
			expect []int  // the pages the result must give
		}{
			{"1 page", 1, "", pagesFrom(1, 1)},
			{"6 pages", 6, "", pagesFrom(1, 6)},
			{"23 pages", 23, "", pagesFrom(1, 23)},
			{"pages=3-14 of 23", 23, "pages=3-14", pagesFrom(3, 14)},
		} {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				doc := pdftest.Make(tc.pages)
				e := rt.expect
				e.Pages = tc.expect
				var results [][]byte
				for _, h := range []*harness{whole, split} {
					loc := h.submit(withQuery(rt.submit, tc.query), doc)
					_, first := h.do(http.MethodGet, loc, nil)
					assertAzureShaped(t, "the first poll", rt.family, first, e)
					if !bytes.Contains(first, []byte(`"analyzeResult"`)) {
						running++
					}
					final := h.awaitSucceeded(loc)
					assertAzureShaped(t, "the final poll", rt.family, final, e)
					resp, sync := h.do(http.MethodPost, withQuery(rt.sync, tc.query), doc)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("sync: status %d: %.300s", resp.StatusCode, sync)
					}
					assertAzureShaped(t, "the synchronous body", rt.family, sync, e)
					if a, s := analyzeResultOf(t, final), analyzeResultOf(t, sync); !bytes.Equal(a, s) {
						t.Errorf("the synchronous result differs from the polled one")
					}
					results = append(results, analyzeResultOf(t, final))
				}
				if !bytes.Equal(results[0], results[1]) {
					t.Errorf("the split document's merged result differs from the whole document's")
				}
			})
		}
	}
}

// TestUnitsAreAzureShaped: each stringIndexType, split, where every merged offset is counted in
// the unit asked for.
func TestUnitsAreAzureShaped(t *testing.T) {
	h := realisticHarness(t, 2)
	rt := shapeRoutes[0]
	for _, unit := range []string{"textElements", "unicodeCodePoint", "utf16CodeUnit"} {
		t.Run(unit, func(t *testing.T) {
			e := rt.expect
			e.StringIndexType, e.Pages = unit, pagesFrom(1, 9)
			resp, body := h.do(http.MethodPost, rt.sync+"&stringIndexType="+unit, pdftest.Make(9))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %.300s", resp.StatusCode, body)
			}
			assertAzureShaped(t, "the synchronous body", rt.family, body, e)
		})
	}
}

// TestReadImageIsAzureShaped: Read measures an image in pixels.
func TestReadImageIsAzureShaped(t *testing.T) {
	h := realisticHarness(t, 5)
	png := []byte("\x89PNG\r\n\x1a\nnot really an image")
	resp, data := h.do(http.MethodPost, "/vision/v3.2/read/analyze", png, "Content-Type", "image/png")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	body := h.awaitSucceeded(resp.Header.Get("Operation-Location"))
	assertAzureShaped(t, "the final poll", azschema.Read, body,
		azschema.Expect{ModelVersion: "2022-04-30", Unit: "pixel", Pages: []int{1}})
}

// TestFailedOperationsAreAzureShaped: a failed operation is a 200 envelope; the DI one carries
// an error object, and the Read one none, since ReadOperationResult declares no such member.
func TestFailedOperationsAreAzureShaped(t *testing.T) {
	h := newSplitHarness(t, 0, harnessOpts{
		diPaged:   mockazure.PagedOptions{Doc: docgen.Realistic(0), BadContent: map[int]bool{2: true}},
		readPaged: mockazure.PagedOptions{BadContent: map[int]bool{2: true}},
	})
	for _, rt := range shapeRoutes {
		t.Run(rt.name, func(t *testing.T) {
			loc := h.submit(rt.submit, pdftest.Make(3))
			if body := h.pollUntil(loc, "failed"); body["status"] != "failed" {
				t.Fatalf("status %v, want failed", body["status"])
			}
			_, body := h.do(http.MethodGet, loc, nil)
			assertAzureShaped(t, "the failed poll", rt.family, body, rt.expect)
			var env map[string]json.RawMessage
			_ = json.Unmarshal(body, &env)
			if _, has := env["error"]; has != (rt.family != azschema.Read) {
				t.Errorf("error member present: %v, want it only on Document Intelligence", has)
			}
		})
	}
}

// realWhole reads a vendored real result as the service wrote it, compact, with the figure ids a
// test proxy sanitized restored to "<page>.<n>".
func realWhole(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../azure-reference", rel))
	if err != nil {
		t.Fatal(err)
	}
	n, err := realcut.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	r := realcut.Bare(n)
	perPage := map[int]int{}
	for i, f := range realcut.Items(r.Get("figures")) {
		regions := realcut.Items(f.Get("boundingRegions"))
		if len(regions) == 0 {
			t.Fatalf("%s: figures[%d] has no bounding region", rel, i)
		}
		page, _ := regions[0].Get("pageNumber").Int()
		perPage[page]++
		if id, _ := f.Get("id").Str(); id == "Sanitized" {
			f.Set("id", realcut.StrNode(fmt.Sprintf("%d.%d", page, perPage[page])))
		}
	}
	return realcut.Encode(r)
}

// composedWhole joins vendored real DI results into one notional multi-page document
// (realcut.ComposeDI), so that real figures sit on pages after the first: no vendored recording
// has one there. ComposeDI checks the figure ids against the inputs independently of the merge.
func composedWhole(t *testing.T, names ...string) []byte {
	t.Helper()
	var results [][]byte
	for _, name := range names {
		results = append(results, realWhole(t, "real/di-layout-2024-11-30/"+name+".analyzeResult.json"))
	}
	whole, _, err := realcut.ComposeDI(t.TempDir(), results, textunit.TextElements)
	if err != nil {
		t.Fatalf("compose %v: %v", names, err)
	}
	return whole
}

// TestRealOutputThroughTheGateway replays recorded service output through the gateway end to
// end: the fake answers each part the gateway sends with the real result cut to that part's pages
// (internal/realcut), and the gateway's merged answer, polled and synchronous, must be the real
// whole-document result byte for byte, and Azure-shaped. One page per part puts a cut at every
// page boundary: a blank page, a root of two spans, a section and a table that continue onto the
// next page, and — in the documents joined from real one-page results — figures on pages 2 to 5,
// whose ids the gateway must give back on their pages in the whole document.
func TestRealOutputThroughTheGateway(t *testing.T) {
	for _, tc := range []struct {
		rel     string
		family  azschema.Family
		compose []string // when set, rel is a name and the whole is these results joined
	}{
		{"real/di-layout-2024-11-30/py_layout_multipage_table_span_pdf.analyzeResult.json", azschema.DocumentIntelligence, nil},
		{"real/di-layout-2024-11-30/py_layout_multipage_transform.analyzeResult.json", azschema.DocumentIntelligence, nil},
		{"real/di-layout-2024-11-30/java_layout_with_pages.analyzeResult.json", azschema.DocumentIntelligence, nil},
		{"composed/figures-on-pages-2-3-4", azschema.DocumentIntelligence,
			[]string{"py_layout_stylefont", "py_layout_barcodes", "js_layout_selection_marks", "py_layout_figures"}},
		{"composed/figures-after-a-blank-page", azschema.DocumentIntelligence,
			[]string{"java_layout_with_pages", "py_layout_figures", "java_layout", "js_layout_selection_marks"}},
		{"specs/cv-v3.2/example.SuccessfulGetReadResult.envelope.json", azschema.Read, nil},
	} {
		var whole []byte
		if tc.compose != nil {
			whole = composedWhole(t, tc.compose...)
		} else {
			whole = realWhole(t, tc.rel)
		}
		di := tc.family == azschema.DocumentIntelligence
		// replay runs on the fake's goroutine: it reports a cut it cannot make with t.Errorf
		// (safe from any goroutine) and answers nothing, which the fake turns into a 500.
		replay := func(t *testing.T) func([]int, docgen.Numbering, bool) []byte {
			return func(pages []int, _ docgen.Numbering, isDI bool) []byte {
				if isDI != di {
					return nil
				}
				var parts [][]byte
				var err error
				if di {
					parts, _, err = realcut.CutDI(whole, [][]int{pages}, textunit.TextElements)
				} else {
					parts, err = realcut.CutRead(whole, [][]int{pages})
				}
				if err != nil {
					t.Errorf("realcut cannot cut pages %v out of %s: %v", pages, tc.rel, err)
					return nil
				}
				return parts[0]
			}
		}
		n, _ := realcut.Parse(whole)
		pages := len(realcut.Items(n.Get("pages")))
		route := shapeRoutes[0]
		e := azschema.Expect{Pages: pagesFrom(1, pages)}
		if !di {
			pages = len(realcut.Items(n.Get("readResults")))
			route, e = shapeRoutes[2], azschema.Expect{Pages: pagesFrom(1, pages)}
		}
		for _, split := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/split %d", filepath.Base(tc.rel), split), func(t *testing.T) {
				h := newSplitHarness(t, split, harnessOpts{
					diPaged:   mockazure.PagedOptions{Replay: replay(t)},
					readPaged: mockazure.PagedOptions{Replay: replay(t)},
				})
				doc := pdftest.Make(pages)
				final := h.awaitSucceeded(h.submit(route.submit, doc))
				resp, sync := h.do(http.MethodPost, route.sync, doc)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("sync: status %d: %.300s", resp.StatusCode, sync)
				}
				for what, body := range map[string][]byte{"polled": final, "synchronous": sync} {
					if fs := azschema.Errors(reference(t).Envelope(tc.family, body, e)); len(fs) > 0 &&
						!strings.Contains(tc.rel, "SuccessfulGetReadResult") { // synthetic: says "v3.2"
						t.Errorf("the %s body is not shaped as Azure's:\n  %s", what, showFindings(fs))
					}
					if got := analyzeResultOf(t, body); !bytes.Equal(got, whole) {
						g, err := realcut.Parse(got)
						if err != nil {
							t.Errorf("the %s result does not parse: %v", what, err)
							continue
						}
						w, _ := realcut.Parse(whole) // parsed by realWhole or ComposeDI
						_, members, _, _ := realcut.CompareMembers(w, g)
						var diffs []string
						for _, m := range members {
							if !m.Identical {
								diffs = append(diffs, fmt.Sprintf("%s %+v", m.Member, m.Diffs[:min(2, len(m.Diffs))]))
							}
						}
						t.Errorf("the %s result is not the real whole-document result: %v", what, diffs)
					}
				}
				// Split at one page, every call the container sees is one page: a merge check
				// failure would have sent the whole document too, with the same answer.
				c := h.di
				if !di {
					c = h.read
				}
				for _, call := range c.Calls() {
					if split > 0 && len(call) != 1 {
						t.Errorf("the container analysed pages %v together: the split fell back to the whole document", call)
					}
				}
			})
		}
	}
}
