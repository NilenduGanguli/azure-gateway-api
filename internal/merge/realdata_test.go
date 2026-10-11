package merge_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/merge"
	"github.com/NilenduGanguli/azure-gateway-api/internal/realcut"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// These tests put genuine Azure output through the merge. docgen's results are regular by
// construction, so a merge rule that only holds for them (a root section over the whole content,
// say) passes every docgen test and still refuses most real documents. The outputs vendored under
// test/azure-reference were recorded from the cloud service (see its PROVENANCE.md); realcut cuts
// each into the parts Azure would return for its pages, and the merge must give the original back.

const referenceDir = "../../test/azure-reference/real"

// realDI are the vendored Document Intelligence 2024-11-30 prebuilt-layout results.
var realDI = []string{
	"py_layout_multipage_table_span_pdf", // 2 pages; a section and a table continue onto page 2
	"py_layout_multipage_transform",      // 3 pages, page 2 blank; a root of two spans
	"java_layout_with_pages",             // 2 pages, page 2 blank
	"py_layout_figures",                  // pageHeader, pageFooter, pageNumber; captions; nesting
	"js_layout_selection_marks",          // figures 1.1 and 1.2; a trailing pageFooter
	"py_layout_barcodes",                 // barcodes; a trailing pageFooter and pageNumber
	"py_layout_stylefont",                // 23 styles
	"java_layout",                        // pixel units
	"thirdparty_docler_markdown_2page",   // markdown, 2 pages
}

// composedDI are notional multi-page documents joined from genuine one-page results by
// realcut.ComposeDI. No vendored recording has a figure after page 1, so without them the
// real-data tests would pass a merge that stopped renumbering figure ids. Every item is recorded
// output; only the numbering is the merge's, and ComposeDI checks the figure ids against the
// inputs independently of it. figureIDs are the ids the whole must carry, written out by hand.
//
// A root-only part placed after a part that leaves a section open is a continuation whose root
// contributes no spans (continue.go), and realcut cannot cut such a whole back into parts the
// merge accepts; TestContinuationOfARootOnlyPart covers that shape, so these orders avoid it.
var composedDI = []struct {
	results   []string
	figureIDs []string
}{
	// Every figure after page 1; two on one page; a caption; barcodes; selection marks.
	{[]string{"py_layout_stylefont", "py_layout_barcodes", "js_layout_selection_marks", "py_layout_figures"},
		[]string{"2.1", "3.1", "3.2", "4.1"}},
	// A real two-page document (a section and a table continuing onto page 2), then figures.
	{[]string{"py_layout_multipage_table_span_pdf", "js_layout_selection_marks", "py_layout_figures"},
		[]string{"3.1", "3.2", "4.1"}},
	// A blank page before the first figure, and a page without figures between two with them.
	{[]string{"java_layout_with_pages", "py_layout_figures", "java_layout", "js_layout_selection_marks"},
		[]string{"3.1", "5.1", "5.2"}},
	// A root of two spans around a blank page, then figures on pages 4 and 5.
	{[]string{"py_layout_multipage_transform", "py_layout_barcodes", "js_layout_selection_marks"},
		[]string{"4.1", "5.1", "5.2"}},
}

// composed joins the named real results into one whole, and returns it as loadReal does.
func composed(t *testing.T, names []string) (*realcut.Node, []byte) {
	t.Helper()
	var results [][]byte
	for _, name := range names {
		_, b := loadReal(t, diPath(name))
		results = append(results, b)
	}
	whole, _, err := realcut.ComposeDI(t.TempDir(), results, textunit.TextElements)
	if err != nil {
		t.Fatalf("compose %v: %v", names, err)
	}
	r, err := realcut.Parse(whole)
	if err != nil {
		t.Fatal(err)
	}
	return r, whole
}

// realRead are the vendored Read v3.2 results, and the spec's synthetic two-page example.
var realRead = []string{
	"real/read-3.2/net_ReadPdfFileInStreamTest.analyzeResult.json",
	"real/read-3.2/net_ReadFileInStreamTest.analyzeResult.json",
	"real/read-3.2/net_ReadLatestNaturalTest.analyzeResult.json",
	"specs/cv-v3.2/example.SuccessfulGetReadResult.envelope.json",
}

// loadReal reads a vendored result and returns its bare analyzeResult, compact, as the service
// writes it. Two SDK recordings carry the figure id "Sanitized", a test-proxy artefact; the id is
// restored to the "<page>.<n>" the service writes.
func loadReal(t *testing.T, rel string) (*realcut.Node, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(referenceDir, "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	root, err := realcut.Parse(b)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	r := realcut.Bare(root)
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
	return r, realcut.Encode(r)
}

func diPath(name string) string {
	return "real/di-layout-2024-11-30/" + name + ".analyzeResult.json"
}

// compositions returns every way to cut pages 1..n into runs of consecutive pages.
func compositions(n int) [][][]int {
	var out [][][]int
	for mask := 0; mask < 1<<(n-1); mask++ {
		var plan [][]int
		run := []int{1}
		for p := 2; p <= n; p++ {
			if mask&(1<<(p-2)) != 0 {
				plan = append(plan, run)
				run = nil
			}
			run = append(run, p)
		}
		out = append(out, append(plan, run))
	}
	return out
}

// mergeCut writes the parts and merges them.
func mergeCut(t *testing.T, parts [][]byte, plan [][]int, di bool) ([]byte, error) {
	t.Helper()
	var mp []merge.Part
	for k, b := range parts {
		mp = append(mp, writePart(t, b, plan[k], false))
	}
	var out bytes.Buffer
	var err error
	if di {
		_, err = merge.DI(&out, mp, textunit.TextElements)
	} else {
		_, err = merge.Read(&out, mp)
	}
	return out.Bytes(), err
}

// differences names the members of want that got does not reproduce byte for byte, with the
// first few paths in each.
func differences(t *testing.T, want *realcut.Node, got []byte) []string {
	t.Helper()
	g, err := realcut.Parse(got)
	if err != nil {
		t.Fatalf("merged output does not parse: %v", err)
	}
	same, members, wantKeys, gotKeys := realcut.CompareMembers(want, g)
	if same {
		return nil
	}
	var out []string
	if fmt.Sprint(wantKeys) != fmt.Sprint(gotKeys) {
		out = append(out, fmt.Sprintf("member order %v, want %v", gotKeys, wantKeys))
	}
	for _, m := range members {
		if !m.Identical {
			out = append(out, fmt.Sprintf("%s: %d differences, e.g. %+v", m.Member, m.NDiffs,
				m.Diffs[:min(3, len(m.Diffs))]))
		}
	}
	return out
}

// TestMergeReproducesRealDIOutputs cuts every real layout result at every possible set of page
// boundaries and merges the parts: the merge must accept genuine parts — roots that leave out page
// furniture, trailing blank pages, roots of several spans — and give back the original byte for
// byte, sections included, also where a section or a table continues across the cut.
//
// Markdown is the exception, and the gateway refuses outputContentFormat=markdown: a real
// markdown page's span runs over the page-break block that follows it, which a part analysed
// alone does not have, so a merged markdown result is checked only for being accepted.
//
// The composed documents (composedDI) put real figures on pages after the first, so every cut
// moves a figure to a part where its page is numbered differently.
func TestMergeReproducesRealDIOutputs(t *testing.T) {
	type input struct {
		name  string
		want  *realcut.Node
		whole []byte
	}
	var inputs []input
	for _, name := range realDI {
		want, whole := loadReal(t, diPath(name))
		inputs = append(inputs, input{name, want, whole})
	}
	for _, c := range composedDI {
		want, whole := composed(t, c.results)
		inputs = append(inputs, input{"composed:" + strings.Join(c.results, "+"), want, whole})
	}
	for _, in := range inputs {
		name, want, whole := in.name, in.want, in.whole
		pages := len(realcut.Items(want.Get("pages")))
		markdown := bytes.Contains(whole, []byte(`"contentFormat":"markdown"`))
		for _, plan := range compositions(pages) {
			t.Run(fmt.Sprintf("%s/%v", name, plan), func(t *testing.T) {
				parts, notes, err := realcut.CutDI(whole, plan, textunit.TextElements)
				if err != nil {
					t.Fatalf("cut: %v", err)
				}
				got, err := mergeCut(t, parts, plan, true)
				if err != nil {
					t.Fatalf("merge refused genuine parts: %v\ncut notes: %v", err, notes)
				}
				if markdown && len(plan) > 1 {
					return
				}
				if diffs := differences(t, want, got); diffs != nil {
					t.Errorf("merged result differs from the real one:\n  %s\ncut notes: %v",
						strings.Join(diffs, "\n  "), notes)
				}
			})
		}
	}
}

// TestMergeReproducesRealReadOutputs does the same for Read v3.2: pages only, so every plan must
// give the original back.
func TestMergeReproducesRealReadOutputs(t *testing.T) {
	for _, rel := range realRead {
		want, whole := loadReal(t, rel)
		pages := len(realcut.Items(want.Get("readResults")))
		for _, plan := range compositions(pages) {
			t.Run(fmt.Sprintf("%s/%v", filepath.Base(rel), plan), func(t *testing.T) {
				parts, err := realcut.CutRead(whole, plan)
				if err != nil {
					t.Fatalf("cut: %v", err)
				}
				got, err := mergeCut(t, parts, plan, false)
				if err != nil {
					t.Fatalf("merge refused genuine parts: %v", err)
				}
				if diffs := differences(t, want, got); diffs != nil {
					t.Errorf("merged result differs from the real one:\n  %s", strings.Join(diffs, "\n  "))
				}
			})
		}
	}
}

// TestRealResultsAsParts merges genuine one-page results as the parts of a notional document.
// With composedDI, it is the only way to put real figures, captions, selection marks, barcodes,
// styles and page-header, footer and number paragraphs through the renumbering, since no real
// multi-page output has them all. Cut back at the same pages, every part must come back exactly —
// its root section too, so the merged root keeps each part's own spans rather than one extent
// over the furniture between them, and its figure ids, since realcut refuses an id whose page is
// not in the figure's part.
//
// The exception is a part with no heading and no section but its root, whose content continues
// the section the part before it left open (continue.go): realcut gives such a part back as one
// section under its root, the shape a part has when a section crosses a cut, where the service
// gives a part with no heading a root that holds its content directly. Its sections are checked
// by TestContinuationOfARootOnlyPart instead.
func TestRealResultsAsParts(t *testing.T) {
	for _, tc := range []struct {
		names     []string
		continues int // the part whose root-only content continues the open section, or -1
	}{
		{[]string{"py_layout_figures", "js_layout_selection_marks", "py_layout_barcodes"}, 2},
		{[]string{"js_layout_selection_marks", "py_layout_figures"}, -1},
		{[]string{"py_layout_stylefont", "py_layout_figures", "java_layout"}, -1},
		{[]string{"java_layout_with_pages", "py_layout_multipage_transform"}, -1},
	} {
		t.Run(strings.Join(tc.names, "+"), func(t *testing.T) {
			parts, plan := realParts(t, tc.names)
			merged, err := mergeCut(t, parts, plan, true)
			if err != nil {
				t.Fatalf("merge refused genuine results: %v", err)
			}
			back, notes, err := realcut.CutDI(merged, plan, textunit.TextElements)
			if err != nil {
				t.Fatalf("the merged result cannot be cut back at its parts' pages: %v\ncut notes: %v", err, notes)
			}
			for k, name := range tc.names {
				want, err := realcut.Parse(parts[k])
				if err != nil {
					t.Fatalf("part %d (%s): %v", k, name, err)
				}
				got, err := realcut.Parse(back[k])
				if err != nil {
					t.Fatalf("part %d (%s) cut back from the merge does not parse: %v", k, name, err)
				}
				if k == tc.continues {
					want.Del("sections")
					got.Del("sections")
				}
				if diffs := differences(t, want, realcut.Encode(got)); diffs != nil {
					t.Errorf("part %d (%s) does not come back from the merge:\n  %s",
						k, name, strings.Join(diffs, "\n  "))
				}
			}
		})
	}
}

// TestRealFiguresAfterPageOne: in a document joined from real results, a figure's id names the
// page it is on in the whole document, as the service numbers it ("<page>.<n>", the page of its
// first region), and <n> is the figure's number on its own page.
func TestRealFiguresAfterPageOne(t *testing.T) {
	for _, c := range composedDI {
		t.Run(strings.Join(c.results, "+"), func(t *testing.T) {
			r, _ := composed(t, c.results)
			var ids []string
			for i, f := range realcut.Items(r.Get("figures")) {
				id, _ := f.Get("id").Str()
				ids = append(ids, id)
				first, _ := realcut.Items(f.Get("boundingRegions"))[0].Get("pageNumber").Int()
				if !strings.HasPrefix(id, fmt.Sprintf("%d.", first)) {
					t.Errorf("figures[%d] id %q, its first region on page %d", i, id, first)
				}
			}
			if fmt.Sprint(ids) != fmt.Sprint(c.figureIDs) {
				t.Errorf("figure ids %v, want %v", ids, c.figureIDs)
			}
		})
	}
}

// realParts loads real results to be merged as consecutive parts, and plans their pages.
func realParts(t *testing.T, names []string) (parts [][]byte, plan [][]int) {
	t.Helper()
	next := 1
	for _, name := range names {
		r, b := loadReal(t, diPath(name))
		parts = append(parts, b)
		var run []int
		for range realcut.Items(r.Get("pages")) {
			run = append(run, next)
			next++
		}
		plan = append(plan, run)
	}
	return parts, plan
}

// TestContinuationOfARootOnlyPart: py_layout_figures ends inside its section 5, a heading and the
// paragraphs after it; java_layout_with_pages has no heading, so its root holds its content
// directly. Analysed as one document, that content has no heading of its own and stays in
// section 5: the merged section 5 gains java's elements and spans, and the merged root and
// section list are py_layout_figures's.
func TestContinuationOfARootOnlyPart(t *testing.T) {
	parts, plan := realParts(t, []string{"py_layout_figures", "java_layout_with_pages"})
	merged, err := mergeCut(t, parts, plan, true)
	if err != nil {
		t.Fatal(err)
	}
	fig, err := realcut.Parse(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	java, err := realcut.Parse(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	got, err := realcut.Parse(merged)
	if err != nil {
		t.Fatal(err)
	}
	figSecs, gotSecs := realcut.Items(fig.Get("sections")), realcut.Items(got.Get("sections"))
	javaSecs := realcut.Items(java.Get("sections"))
	if len(figSecs) < 6 || len(javaSecs) == 0 || len(realcut.Items(javaSecs[0].Get("spans"))) == 0 {
		t.Fatalf("test data: py_layout_figures has %d sections (want 6 or more), java_layout_with_pages %d (want a root with a span)",
			len(figSecs), len(javaSecs))
	}
	if len(gotSecs) != len(figSecs) {
		t.Fatalf("%d sections, want py_layout_figures's %d", len(gotSecs), len(figSecs))
	}
	if g, w := realcut.Encode(gotSecs[0]), realcut.Encode(figSecs[0]); !bytes.Equal(g, w) {
		t.Errorf("root %s, want py_layout_figures's %s", g, w)
	}
	for i := 1; i < 5; i++ {
		if g, w := realcut.Encode(gotSecs[i]), realcut.Encode(figSecs[i]); !bytes.Equal(g, w) {
			t.Errorf("section %d %s, want %s", i, g, w)
		}
	}
	// Section 5 holds its own elements, then java's, renumbered past py_layout_figures's
	// paragraphs and tables.
	np := len(realcut.Items(fig.Get("paragraphs")))
	nt := len(realcut.Items(fig.Get("tables")))
	var want []string
	for _, e := range realcut.Items(figSecs[5].Get("elements")) {
		s, _ := e.Str()
		want = append(want, s)
	}
	for _, e := range realcut.Items(javaSecs[0].Get("elements")) {
		s, _ := e.Str()
		var member string
		var i int
		if _, err := fmt.Sscanf(strings.ReplaceAll(s, "/", " "), "%s %d", &member, &i); err != nil {
			t.Fatal(err)
		}
		switch member {
		case "paragraphs":
			i += np
		case "tables":
			i += nt
		}
		want = append(want, fmt.Sprintf("/%s/%d", member, i))
	}
	var gotElems []string
	for _, e := range realcut.Items(gotSecs[5].Get("elements")) {
		s, _ := e.Str()
		gotElems = append(gotElems, s)
	}
	if fmt.Sprint(gotElems) != fmt.Sprint(want) {
		t.Errorf("section 5 elements\n%v\nwant\n%v", gotElems, want)
	}
	// Its spans: its own, which stop before py_layout_figures's page footer, and java's root
	// span shifted past the first part's content and the separator.
	content, _ := fig.Get("content").Str()
	shift := textunit.Len(content, textunit.TextElements) + 1
	javaRoot := realcut.Items(javaSecs[0].Get("spans"))[0]
	jo, _ := javaRoot.Get("offset").Int()
	jl, _ := javaRoot.Get("length").Int()
	wantSpans := fmt.Sprintf(`[{"offset":477,"length":819},{"offset":%d,"length":%d}]`, jo+shift, jl)
	if g := string(realcut.Encode(gotSecs[5].Get("spans"))); g != wantSpans {
		t.Errorf("section 5 spans %s, want %s", g, wantSpans)
	}
}

// TestMergeRefusesWhatTheServiceCountsDifferently: the unit self-check measures each part's
// content against the service's own measure of it, the end of its pages' spans. A real part
// whose content is one text element shorter than its pages say is refused before anything is
// shifted.
func TestMergeRefusesWhatTheServiceCountsDifferently(t *testing.T) {
	_, whole := loadReal(t, diPath("py_layout_multipage_table_span_pdf"))
	plan := [][]int{{1}, {2}}
	parts, _, err := realcut.CutDI(whole, plan, textunit.TextElements)
	if err != nil {
		t.Fatal(err)
	}
	// Page 1's last line ends the content; one more character there that the service did not
	// count makes the gateway measure the part one longer than its page spans end.
	parts[0] = replace(t, parts[0], `","pages":[`, ` ","pages":[`)
	_, err = mergeCut(t, parts, plan, true)
	if !errors.Is(err, merge.ErrCheck) || !strings.Contains(err.Error(), "the service counts textElements differently") {
		t.Fatalf("err = %v, want the unit self-check to refuse the part", err)
	}
}

// unwrapLeadingSection gives a real result the other shape its leading untitled content could
// take: the content of its untitled leaf section 1 straight in the root, before the sections that
// follow, and section 1 gone (every later section renumbered one down). No real output has that
// shape — java_layout and js_layout_selection_marks, the two that open with untitled content
// before a heading, wrap it in section 1 — but it is the content a part continuing a section
// opens with all the same, so the merge must treat both shapes alike.
func unwrapLeadingSection(t *testing.T, b []byte) []byte {
	t.Helper()
	r, err := realcut.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	secs := realcut.Items(r.Get("sections"))
	if len(secs) < 2 {
		t.Fatalf("test data: %d sections, want a root and a section 1", len(secs))
	}
	rootElems := realcut.Items(secs[0].Get("elements"))
	if s, _ := rootElems[0].Str(); s != "/sections/1" || len(rootElems) < 2 {
		t.Fatalf("test data: root elements start %q, want /sections/1 and more after it", s)
	}
	lead := realcut.Items(secs[1].Get("elements"))
	for _, e := range lead {
		if s, _ := e.Str(); strings.HasPrefix(s, "/sections/") {
			t.Fatalf("test data: section 1 holds %s: not a leaf", s)
		}
	}
	secs[0].Set("elements", realcut.ArrNode(append(append([]*realcut.Node{}, lead...), rootElems[1:]...)...))
	kept := append([]*realcut.Node{secs[0]}, secs[2:]...)
	for _, s := range kept {
		for i, e := range realcut.Items(s.Get("elements")) {
			str, _ := e.Str()
			if n, ok := strings.CutPrefix(str, "/sections/"); ok {
				var j int
				if _, err := fmt.Sscan(n, &j); err != nil || j < 2 {
					t.Fatalf("test data: pointer %s", str)
				}
				s.Get("elements").Vals[i] = realcut.StrNode(fmt.Sprintf("/sections/%d", j-1))
			}
		}
	}
	r.Set("sections", realcut.ArrNode(kept...))
	return realcut.Encode(r)
}

// TestContinuationBeforeASectionInTheRoot: py_layout_figures ends inside its section 5.
// java_layout and js_layout_selection_marks open with untitled content — a paragraph; a figure and
// a paragraph — and then a section with a title. Following py_layout_figures, that content has no
// heading of its own and continues section 5, whether the part wraps it in an untitled section 1
// (as the service does) or holds it in its root before its first section: merged, the two shapes
// give the same result byte for byte, the content in section 5 and nothing but sections in the
// root after py_layout_figures's.
func TestContinuationBeforeASectionInTheRoot(t *testing.T) {
	for _, name := range []string{"java_layout", "js_layout_selection_marks"} {
		t.Run(name, func(t *testing.T) {
			parts, plan := realParts(t, []string{"py_layout_stylefont", "py_layout_figures", name})
			wrapped, err := mergeCut(t, parts, plan, true)
			if err != nil {
				t.Fatal(err)
			}
			parts[2] = unwrapLeadingSection(t, parts[2])
			unwrapped, err := mergeCut(t, parts, plan, true)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(unwrapped, wrapped) {
				want, err := realcut.Parse(wrapped)
				if err != nil {
					t.Fatal(err)
				}
				if diffs := differences(t, want, unwrapped); diffs != nil {
					t.Errorf("leading content in the root merges differently from the same content "+
						"in section 1:\n  %s", strings.Join(diffs, "\n  "))
				} else {
					t.Errorf("merged results differ in their bytes")
				}
			}
			// Both are the fold: py_layout_figures's section 5 gains the content, and the merged
			// root holds nothing after its first section but sections.
			got, err := realcut.Parse(unwrapped)
			if err != nil {
				t.Fatal(err)
			}
			fig, err := realcut.Parse(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			secs := realcut.Items(got.Get("sections"))
			own := len(realcut.Items(realcut.Items(fig.Get("sections"))[5].Get("elements")))
			if n := len(realcut.Items(secs[5].Get("elements"))); n <= own {
				t.Errorf("section 5 has %d elements, want more than py_layout_figures's %d", n, own)
			}
			seen := false
			for _, e := range realcut.Items(secs[0].Get("elements")) {
				s, _ := e.Str()
				if section := strings.HasPrefix(s, "/sections/"); seen && !section {
					t.Errorf("merged root holds %s after a section", s)
				} else if section {
					seen = true
				}
			}
		})
	}
}
