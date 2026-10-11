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
	for _, f := range realcut.Items(r.Get("figures")) {
		page, _ := realcut.Items(f.Get("boundingRegions"))[0].Get("pageNumber").Int()
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
func TestMergeReproducesRealDIOutputs(t *testing.T) {
	for _, name := range realDI {
		want, whole := loadReal(t, diPath(name))
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
// It is the only way to put real figures, captions, selection marks, barcodes, styles and
// page-header, footer and number paragraphs through the renumbering, since no real multi-page
// output has them all. Cut back at the same pages, every part must come back exactly — its root
// section too, so the merged root keeps each part's own spans rather than one extent over the
// furniture between them.
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
			back, _, err := realcut.CutDI(merged, plan, textunit.TextElements)
			if err != nil {
				t.Fatalf("cut back: %v", err)
			}
			for k, name := range tc.names {
				want, _ := realcut.Parse(parts[k])
				got, _ := realcut.Parse(back[k])
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
	fig, _ := realcut.Parse(parts[0])
	java, _ := realcut.Parse(parts[1])
	got, err := realcut.Parse(merged)
	if err != nil {
		t.Fatal(err)
	}
	figSecs, gotSecs := realcut.Items(fig.Get("sections")), realcut.Items(got.Get("sections"))
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
	for _, e := range realcut.Items(realcut.Items(java.Get("sections"))[0].Get("elements")) {
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
	javaRoot := realcut.Items(realcut.Items(java.Get("sections"))[0].Get("spans"))[0]
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
