package merge_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/merge"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// handPara is a paragraph of a hand-built result: the page it is on (1-based, among the pages the
// result is built from), its text, and its role, "" for none.
type handPara struct {
	page int
	text string
	role string
}

// handDI builds a minimal Document Intelligence text result by hand: the pages' texts joined by
// "\n" as the service joins them, a span over every page's text (a blank page an empty one), a
// paragraph for each handPara, found in its page's text after the paragraph before it, and the
// sections given, root first. The texts are ASCII, so an offset is a byte offset.
//
// It builds a part as the service would answer it alone and the whole document the merge must
// give back from the same pieces, so the two can be compared byte for byte.
func handDI(t *testing.T, pages []string, paras []handPara, sections string) []byte {
	t.Helper()
	starts := make([]int, len(pages))
	var content strings.Builder
	var ps []string
	for i, text := range pages {
		if i > 0 {
			content.WriteByte('\n')
		}
		starts[i] = content.Len()
		content.WriteString(text)
		ps = append(ps, fmt.Sprintf(`{"pageNumber":%d,"spans":[{"offset":%d,"length":%d}]}`,
			i+1, starts[i], len(text)))
	}
	var items []string
	from := make([]int, len(pages)) // where the next paragraph on each page is looked for
	for _, p := range paras {
		at := strings.Index(pages[p.page-1][from[p.page-1]:], p.text)
		if at < 0 {
			t.Fatalf("test bug: %q is not on page %d after offset %d", p.text, p.page, from[p.page-1])
		}
		at += from[p.page-1]
		from[p.page-1] = at + len(p.text)
		role := ""
		if p.role != "" {
			role = fmt.Sprintf(`"role":%q,`, p.role)
		}
		items = append(items, fmt.Sprintf(
			`{"spans":[{"offset":%d,"length":%d}],"boundingRegions":[{"pageNumber":%d,"polygon":[0,0,1,0,1,1,0,1]}],%s"content":%q}`,
			starts[p.page-1]+at, len(p.text), p.page, role, p.text))
	}
	return []byte(`{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"textElements",` +
		`"content":` + fmt.Sprintf("%q", content.String()) + `,"pages":[` + strings.Join(ps, ",") +
		`],"paragraphs":[` + strings.Join(items, ",") + `],"styles":[],"sections":` + sections +
		`,"contentFormat":"text"}`)
}

// TestLeadingRunInTheRoot: a part whose root opens with untitled content and goes on to a
// section continues the section the part before it left open, as a part whose untitled content
// is wrapped in a section 1 does (TestContinuationBeforeASectionInTheRoot has that on real data).
// The run is the root's leading elements up to its first section or heading paragraph; it moves
// into the open section, and its spans are the root's clipped to where the run starts and ends,
// so a run over a blank page comes in two spans, as a real root over one does
// (py_layout_multipage_transform). The merge must give back the whole document as the service
// would have analysed it.
//
// Page 1 is the same in every case: a heading and a paragraph, one section under the root, left
// open at the cut.
func TestLeadingRunInTheRoot(t *testing.T) {
	page1 := []string{"Intro\nBody one"}
	paras1 := []handPara{{1, "Intro", "sectionHeading"}, {1, "Body one", ""}}
	part0 := handDI(t, page1, paras1,
		`[{"spans":[{"offset":0,"length":14}],"elements":["/sections/1"]},`+
			`{"spans":[{"offset":0,"length":14}],"elements":["/paragraphs/0","/paragraphs/1"]}]`)
	for _, tc := range []struct {
		name          string
		pages         []string
		paras         []handPara
		part, whole   string // the later part's sections, alone; the whole document's
		wholeElements int    // the paragraphs the whole holds, to check the test's own arithmetic
	}{
		{
			// The run stops at the title, which stays in the root with the section after it.
			name:  "a run that stops at a heading paragraph",
			pages: []string{"Body two\nPart Two\nChapter\nText"},
			paras: []handPara{{1, "Body two", ""}, {1, "Part Two", "title"},
				{1, "Chapter", "sectionHeading"}, {1, "Text", ""}},
			part: `[{"spans":[{"offset":0,"length":30}],"elements":["/paragraphs/0","/paragraphs/1","/sections/1"]},` +
				`{"spans":[{"offset":18,"length":12}],"elements":["/paragraphs/2","/paragraphs/3"]}]`,
			whole: `[{"spans":[{"offset":0,"length":45}],"elements":["/sections/1","/paragraphs/3","/sections/2"]},` +
				`{"spans":[{"offset":0,"length":23}],"elements":["/paragraphs/0","/paragraphs/1","/paragraphs/2"]},` +
				`{"spans":[{"offset":33,"length":12}],"elements":["/paragraphs/4","/paragraphs/5"]}]`,
			wholeElements: 6,
		},
		{
			// Pages 2 to 4, page 3 blank: the root is two spans around it, and so is the run.
			name:  "a run across a blank page",
			pages: []string{"Body two", "", "Tail\nChapter\nText"},
			paras: []handPara{{1, "Body two", ""}, {3, "Tail", ""},
				{3, "Chapter", "sectionHeading"}, {3, "Text", ""}},
			part: `[{"spans":[{"offset":0,"length":8},{"offset":10,"length":17}],"elements":["/paragraphs/0","/paragraphs/1","/sections/1"]},` +
				`{"spans":[{"offset":15,"length":12}],"elements":["/paragraphs/2","/paragraphs/3"]}]`,
			whole: `[{"spans":[{"offset":0,"length":23},{"offset":25,"length":17}],"elements":["/sections/1","/sections/2"]},` +
				`{"spans":[{"offset":0,"length":23},{"offset":25,"length":4}],"elements":["/paragraphs/0","/paragraphs/1","/paragraphs/2","/paragraphs/3"]},` +
				`{"spans":[{"offset":30,"length":12}],"elements":["/paragraphs/4","/paragraphs/5"]}]`,
			wholeElements: 6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			later := len(page1) + 1
			var wholeParas []handPara
			wholeParas = append(wholeParas, paras1...)
			for _, p := range tc.paras {
				wholeParas = append(wholeParas, handPara{p.page + len(page1), p.text, p.role})
			}
			if len(wholeParas) != tc.wholeElements {
				t.Fatalf("test bug: %d paragraphs", len(wholeParas))
			}
			part1 := handDI(t, tc.pages, tc.paras, tc.part)
			whole := handDI(t, append(append([]string{}, page1...), tc.pages...), wholeParas, tc.whole)
			parts := []merge.Part{
				writePart(t, part0, []int{1}, false),
				writePart(t, part1, pageRange(later, later+len(tc.pages)-1), false),
			}
			var out bytes.Buffer
			if _, err := merge.DI(&out, parts, textunit.TextElements); err != nil {
				t.Fatal(err)
			}
			sameBytes(t, out.Bytes(), whole)
		})
	}
}

// TestLeadingRunNamingNoItemIsRefused: the run's spans are read from the items its pointers
// name, so a pointer past the part's items cannot be placed, and is refused before anything is
// written rather than left for the copy to find.
func TestLeadingRunNamingNoItemIsRefused(t *testing.T) {
	part0 := handDI(t, []string{"Intro\nBody one"},
		[]handPara{{1, "Intro", "sectionHeading"}, {1, "Body one", ""}},
		`[{"spans":[{"offset":0,"length":14}],"elements":["/sections/1"]},`+
			`{"spans":[{"offset":0,"length":14}],"elements":["/paragraphs/0","/paragraphs/1"]}]`)
	part1 := handDI(t, []string{"Body two\nChapter"},
		[]handPara{{1, "Body two", ""}, {1, "Chapter", "sectionHeading"}},
		`[{"spans":[{"offset":0,"length":16}],"elements":["/paragraphs/7","/sections/1"]},`+
			`{"spans":[{"offset":9,"length":7}],"elements":["/paragraphs/1"]}]`)
	var out bytes.Buffer
	_, err := merge.DI(&out, []merge.Part{
		writePart(t, part0, []int{1}, false),
		writePart(t, part1, []int{2}, false),
	}, textunit.TextElements)
	if !errors.Is(err, merge.ErrCheck) || !strings.Contains(err.Error(), `"/paragraphs/7" names no item of the part`) {
		t.Fatalf("err = %v, want the run's pointer past the part's paragraphs refused", err)
	}
	if out.Len() != 0 {
		t.Errorf("%d bytes written before the refusal", out.Len())
	}
}

// handPart is one later part of a hand-built document: its pages' texts, its paragraphs, and its
// sections as the service would give them for those pages alone.
type handPart struct {
	pages    []string
	paras    []handPara
	sections string
}

// TestContinuationOfANestedSection: the first part leaves two sections open, Chapter inside Part.
// Content without a heading after the cut continues Chapter, the deepest, whatever shape it comes
// in, and Part's spans grow with it as Chapter's do, as the root's do: a real section's spans are
// its elements' extent, its sub-sections included (py_layout_figures's section 2 spans its
// sections 3 and 4).
//
// What the later part says of its own headings is kept relative to the sections it continues: a
// heading inside its untitled leading section stays inside the section that one continues; a
// heading inside an untitled wrapper — a section that opens with a section, the shape a part has
// when the whole document's nested sections are cut at the page — stays inside the open section
// at the wrapper's depth. Where the whole document would have nested such a heading is not in the
// part, and the wholes below are the ones that rule gives. A chain of wrappers deeper than the
// sections left open has nothing to continue at its top, and is left as the part gives it, as is
// a wrapper that opens with a section other than the next in the part's numbering: their
// "wholes" are the merge's output, not documents the service would write.
func TestContinuationOfANestedSection(t *testing.T) {
	page1 := []string{"Part\nChapter\nBody one"}
	paras1 := []handPara{{1, "Part", "sectionHeading"}, {1, "Chapter", "sectionHeading"}, {1, "Body one", ""}}
	part0 := handDI(t, page1, paras1,
		`[{"spans":[{"offset":0,"length":21}],"elements":["/sections/1"]},`+
			`{"spans":[{"offset":0,"length":21}],"elements":["/paragraphs/0","/sections/2"]},`+
			`{"spans":[{"offset":5,"length":16}],"elements":["/paragraphs/1","/paragraphs/2"]}]`)
	body := []string{"Body two\nMore"}
	bodyParas := []handPara{{1, "Body two", ""}, {1, "More", ""}}
	// Two untitled paragraphs after the cut, in Chapter; Part's and the root's spans grow too.
	continued := `[{"spans":[{"offset":0,"length":35}],"elements":["/sections/1"]},` +
		`{"spans":[{"offset":0,"length":35}],"elements":["/paragraphs/0","/sections/2"]},` +
		`{"spans":[{"offset":5,"length":30}],"elements":["/paragraphs/1","/paragraphs/2","/paragraphs/3","/paragraphs/4"]}]`
	for _, tc := range []struct {
		name          string
		later         []handPart
		whole         string
		wholeElements int
	}{
		{
			name: "an untitled section 1",
			later: []handPart{{body, bodyParas,
				`[{"spans":[{"offset":0,"length":13}],"elements":["/sections/1"]},` +
					`{"spans":[{"offset":0,"length":13}],"elements":["/paragraphs/0","/paragraphs/1"]}]`}},
			whole:         continued,
			wholeElements: 5,
		},
		{
			name: "a root-only part",
			later: []handPart{{body, bodyParas,
				`[{"spans":[{"offset":0,"length":13}],"elements":["/paragraphs/0","/paragraphs/1"]}]`}},
			whole:         continued,
			wholeElements: 5,
		},
		{
			// Next is a section of the root's in the part, so it is one of the root's after Part.
			name: "an untitled run in the root",
			later: []handPart{{[]string{"Body two\nMore\nNext\nText"},
				[]handPara{{1, "Body two", ""}, {1, "More", ""}, {1, "Next", "sectionHeading"}, {1, "Text", ""}},
				`[{"spans":[{"offset":0,"length":23}],"elements":["/paragraphs/0","/paragraphs/1","/sections/1"]},` +
					`{"spans":[{"offset":14,"length":9}],"elements":["/paragraphs/2","/paragraphs/3"]}]`}},
			whole: `[{"spans":[{"offset":0,"length":45}],"elements":["/sections/1","/sections/3"]},` +
				`{"spans":[{"offset":0,"length":35}],"elements":["/paragraphs/0","/sections/2"]},` +
				`{"spans":[{"offset":5,"length":30}],"elements":["/paragraphs/1","/paragraphs/2","/paragraphs/3","/paragraphs/4"]},` +
				`{"spans":[{"offset":36,"length":9}],"elements":["/paragraphs/5","/paragraphs/6"]}]`,
			wholeElements: 7,
		},
		{
			// Sub is inside the untitled section in the part, so it is inside Chapter merged.
			name: "an untitled section 1 with a section of its own",
			later: []handPart{{[]string{"Body two\nSub\nText"},
				[]handPara{{1, "Body two", ""}, {1, "Sub", "sectionHeading"}, {1, "Text", ""}},
				`[{"spans":[{"offset":0,"length":17}],"elements":["/sections/1"]},` +
					`{"spans":[{"offset":0,"length":17}],"elements":["/paragraphs/0","/sections/2"]},` +
					`{"spans":[{"offset":9,"length":8}],"elements":["/paragraphs/1","/paragraphs/2"]}]`}},
			whole: `[{"spans":[{"offset":0,"length":39}],"elements":["/sections/1"]},` +
				`{"spans":[{"offset":0,"length":39}],"elements":["/paragraphs/0","/sections/2"]},` +
				`{"spans":[{"offset":5,"length":34}],"elements":["/paragraphs/1","/paragraphs/2","/paragraphs/3","/sections/3"]},` +
				`{"spans":[{"offset":31,"length":8}],"elements":["/paragraphs/4","/paragraphs/5"]}]`,
			wholeElements: 6,
		},
		{
			// Then a part with no heading continues Sub, the deepest section the two parts before
			// it leave open, and every section above it, Chapter and Part from the first part.
			name: "a root-only part after a sub-section folded in",
			later: []handPart{
				{[]string{"Body two\nSub\nText"},
					[]handPara{{1, "Body two", ""}, {1, "Sub", "sectionHeading"}, {1, "Text", ""}},
					`[{"spans":[{"offset":0,"length":17}],"elements":["/sections/1"]},` +
						`{"spans":[{"offset":0,"length":17}],"elements":["/paragraphs/0","/sections/2"]},` +
						`{"spans":[{"offset":9,"length":8}],"elements":["/paragraphs/1","/paragraphs/2"]}]`},
				{[]string{"Tail"}, []handPara{{1, "Tail", ""}},
					`[{"spans":[{"offset":0,"length":4}],"elements":["/paragraphs/0"]}]`},
			},
			whole: `[{"spans":[{"offset":0,"length":44}],"elements":["/sections/1"]},` +
				`{"spans":[{"offset":0,"length":44}],"elements":["/paragraphs/0","/sections/2"]},` +
				`{"spans":[{"offset":5,"length":39}],"elements":["/paragraphs/1","/paragraphs/2","/paragraphs/3","/sections/3"]},` +
				`{"spans":[{"offset":31,"length":13}],"elements":["/paragraphs/4","/paragraphs/5","/paragraphs/6"]}]`,
			wholeElements: 7,
		},
		{
			// The whole document's tree cut at the page: Part and Chapter, untitled, wrap the text.
			name: "a chain of untitled sections",
			later: []handPart{{body, bodyParas,
				`[{"spans":[{"offset":0,"length":13}],"elements":["/sections/1"]},` +
					`{"spans":[{"offset":0,"length":13}],"elements":["/sections/2"]},` +
					`{"spans":[{"offset":0,"length":13}],"elements":["/paragraphs/0","/paragraphs/1"]}]`}},
			whole:         continued,
			wholeElements: 5,
		},
		{
			// Part, untitled, wraps Next: Next is Part's, after Chapter, which it does not continue.
			name: "an untitled wrapper around a section with a heading",
			later: []handPart{{[]string{"Next\nText"},
				[]handPara{{1, "Next", "sectionHeading"}, {1, "Text", ""}},
				`[{"spans":[{"offset":0,"length":9}],"elements":["/sections/1"]},` +
					`{"spans":[{"offset":0,"length":9}],"elements":["/sections/2"]},` +
					`{"spans":[{"offset":0,"length":9}],"elements":["/paragraphs/0","/paragraphs/1"]}]`}},
			whole: `[{"spans":[{"offset":0,"length":31}],"elements":["/sections/1"]},` +
				`{"spans":[{"offset":0,"length":31}],"elements":["/paragraphs/0","/sections/2","/sections/3"]},` +
				`{"spans":[{"offset":5,"length":16}],"elements":["/paragraphs/1","/paragraphs/2"]},` +
				`{"spans":[{"offset":22,"length":9}],"elements":["/paragraphs/3","/paragraphs/4"]}]`,
			wholeElements: 5,
		},
		{
			// Three untitled sections deep, below two open sections: left as the part gives it.
			name: "a chain deeper than the sections left open",
			later: []handPart{{body, bodyParas,
				`[{"spans":[{"offset":0,"length":13}],"elements":["/sections/1"]},` +
					`{"spans":[{"offset":0,"length":13}],"elements":["/sections/2"]},` +
					`{"spans":[{"offset":0,"length":13}],"elements":["/sections/3"]},` +
					`{"spans":[{"offset":0,"length":13}],"elements":["/paragraphs/0","/paragraphs/1"]}]`}},
			whole: `[{"spans":[{"offset":0,"length":35}],"elements":["/sections/1","/sections/3"]},` +
				`{"spans":[{"offset":0,"length":21}],"elements":["/paragraphs/0","/sections/2"]},` +
				`{"spans":[{"offset":5,"length":16}],"elements":["/paragraphs/1","/paragraphs/2"]},` +
				`{"spans":[{"offset":22,"length":13}],"elements":["/sections/4"]},` +
				`{"spans":[{"offset":22,"length":13}],"elements":["/sections/5"]},` +
				`{"spans":[{"offset":22,"length":13}],"elements":["/paragraphs/3","/paragraphs/4"]}]`,
			wholeElements: 5,
		},
		{
			// An untitled section 1 that opens with its section 3, not 2: no real tree numbers a
			// section's first sub-section other than next, so which section the chain goes on to
			// is not known, and the part is left as it gives it rather than folded through a guess
			// at section 2.
			name: "a wrapper around a section other than the next",
			later: []handPart{{[]string{"Body two\nMore\nNext\nText"},
				[]handPara{{1, "Body two", ""}, {1, "More", ""}, {1, "Next", "sectionHeading"}, {1, "Text", ""}},
				`[{"spans":[{"offset":0,"length":23}],"elements":["/sections/1"]},` +
					`{"spans":[{"offset":0,"length":23}],"elements":["/sections/3","/sections/2"]},` +
					`{"spans":[{"offset":14,"length":9}],"elements":["/paragraphs/2","/paragraphs/3"]},` +
					`{"spans":[{"offset":0,"length":13}],"elements":["/paragraphs/0","/paragraphs/1"]}]`}},
			whole: `[{"spans":[{"offset":0,"length":45}],"elements":["/sections/1","/sections/3"]},` +
				`{"spans":[{"offset":0,"length":21}],"elements":["/paragraphs/0","/sections/2"]},` +
				`{"spans":[{"offset":5,"length":16}],"elements":["/paragraphs/1","/paragraphs/2"]},` +
				`{"spans":[{"offset":22,"length":23}],"elements":["/sections/5","/sections/4"]},` +
				`{"spans":[{"offset":36,"length":9}],"elements":["/paragraphs/5","/paragraphs/6"]},` +
				`{"spans":[{"offset":22,"length":13}],"elements":["/paragraphs/3","/paragraphs/4"]}]`,
			wholeElements: 7,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := []merge.Part{writePart(t, part0, []int{1}, false)}
			pages := append([]string{}, page1...)
			wholeParas := append([]handPara{}, paras1...)
			for _, hp := range tc.later {
				first := len(pages) + 1
				for _, p := range hp.paras {
					wholeParas = append(wholeParas, handPara{p.page + len(pages), p.text, p.role})
				}
				pages = append(pages, hp.pages...)
				parts = append(parts, writePart(t, handDI(t, hp.pages, hp.paras, hp.sections),
					pageRange(first, len(pages)), false))
			}
			if len(wholeParas) != tc.wholeElements {
				t.Fatalf("test bug: %d paragraphs", len(wholeParas))
			}
			var out bytes.Buffer
			if _, err := merge.DI(&out, parts, textunit.TextElements); err != nil {
				t.Fatal(err)
			}
			sameBytes(t, out.Bytes(), handDI(t, pages, wholeParas, tc.whole))
		})
	}
}

// TestContinuationOfATreeThatLoopsBack: the first part's Chapter ends with a pointer back up to
// Part, which already holds it. No real tree does this, but a part's sections are only read, not
// trusted, and the path of open sections is followed through each last element: going round
// again would put Part on the path twice, below Chapter, and a root-only part after the cut would
// go into Part and give it its spans twice. The path stops at the first section it has already
// passed, so the content continues Chapter, the deepest section reached, and each section on the
// path takes it once.
func TestContinuationOfATreeThatLoopsBack(t *testing.T) {
	page1 := []string{"Part\nChapter\nBody one"}
	paras1 := []handPara{{1, "Part", "sectionHeading"}, {1, "Chapter", "sectionHeading"}, {1, "Body one", ""}}
	part0 := handDI(t, page1, paras1,
		`[{"spans":[{"offset":0,"length":21}],"elements":["/sections/1"]},`+
			`{"spans":[{"offset":0,"length":21}],"elements":["/paragraphs/0","/sections/2"]},`+
			`{"spans":[{"offset":5,"length":16}],"elements":["/paragraphs/1","/paragraphs/2","/sections/1"]}]`)
	body := []string{"Body two\nMore"}
	bodyParas := []handPara{{1, "Body two", ""}, {1, "More", ""}}
	part1 := handDI(t, body, bodyParas,
		`[{"spans":[{"offset":0,"length":13}],"elements":["/paragraphs/0","/paragraphs/1"]}]`)
	whole := handDI(t, append(append([]string{}, page1...), body...),
		append(append([]handPara{}, paras1...), handPara{2, "Body two", ""}, handPara{2, "More", ""}),
		`[{"spans":[{"offset":0,"length":35}],"elements":["/sections/1"]},`+
			`{"spans":[{"offset":0,"length":35}],"elements":["/paragraphs/0","/sections/2"]},`+
			`{"spans":[{"offset":5,"length":30}],"elements":["/paragraphs/1","/paragraphs/2","/sections/1","/paragraphs/3","/paragraphs/4"]}]`)
	var out bytes.Buffer
	if _, err := merge.DI(&out, []merge.Part{
		writePart(t, part0, []int{1}, false),
		writePart(t, part1, []int{2}, false),
	}, textunit.TextElements); err != nil {
		t.Fatal(err)
	}
	sameBytes(t, out.Bytes(), whole)
}
