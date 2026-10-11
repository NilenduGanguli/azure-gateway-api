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
