package realcut_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/realcut"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// smallDI is a two-page layout result in the service's shape, small enough to break by hand:
// content "Title\nHello world" (page 1, units 0-17), "\n", "Cell\nFigure caption" (page 2,
// units 18-37); a table and a figure on page 2; a root section and one under it.
const smallDI = `{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"textElements",` +
	`"content":"Title\nHello world\nCell\nFigure caption",` +
	`"pages":[` +
	`{"pageNumber":1,"words":[{"content":"Title","span":{"offset":0,"length":5}},{"content":"Hello","span":{"offset":6,"length":5}}],"spans":[{"offset":0,"length":17}]},` +
	`{"pageNumber":2,"words":[{"content":"Cell","span":{"offset":18,"length":4}}],"spans":[{"offset":18,"length":19}]}],` +
	`"paragraphs":[` +
	`{"role":"title","boundingRegions":[{"pageNumber":1,"polygon":[0,0,1,0,1,1,0,1]}],"spans":[{"offset":0,"length":5}],"content":"Title"},` +
	`{"boundingRegions":[{"pageNumber":1,"polygon":[0,1,1,1,1,2,0,2]}],"spans":[{"offset":6,"length":11}],"content":"Hello world"},` +
	`{"boundingRegions":[{"pageNumber":2,"polygon":[0,0,1,0,1,1,0,1]}],"spans":[{"offset":18,"length":4}],"content":"Cell"},` +
	`{"boundingRegions":[{"pageNumber":2,"polygon":[0,1,1,1,1,2,0,2]}],"spans":[{"offset":23,"length":14}],"content":"Figure caption"}],` +
	`"tables":[{"rowCount":1,"columnCount":1,"cells":[{"rowIndex":0,"columnIndex":0,"content":"Cell",` +
	`"boundingRegions":[{"pageNumber":2,"polygon":[0,0,1,0,1,1,0,1]}],"spans":[{"offset":18,"length":4}],"elements":["/paragraphs/2"]}],` +
	`"boundingRegions":[{"pageNumber":2,"polygon":[0,0,1,0,1,1,0,1]}],"spans":[{"offset":18,"length":4}]}],` +
	`"figures":[{"id":"2.1","boundingRegions":[{"pageNumber":2,"polygon":[0,1,1,1,1,2,0,2]}],"spans":[{"offset":23,"length":14}],"elements":["/paragraphs/3"]}],` +
	`"sections":[{"spans":[{"offset":0,"length":37}],"elements":["/paragraphs/0","/sections/1"]},` +
	`{"spans":[{"offset":6,"length":31}],"elements":["/paragraphs/1","/tables/0","/figures/0"]}],` +
	`"styles":[{"confidence":1,"spans":[{"offset":0,"length":5}],"isHandwritten":false}],` +
	`"contentFormat":"text"}`

// smallRead is a two-page Read v3.2 result.
const smallRead = `{"version":"3.2.0","modelVersion":"2022-04-30","readResults":[` +
	`{"page":1,"angle":0,"width":8.5,"height":11,"unit":"inch","lines":[]},` +
	`{"page":2,"angle":0,"width":8.5,"height":11,"unit":"inch","lines":[]}]}`

// smallUTF16 is a one-page result counted in UTF-16 code units: the emoji is two of them.
const smallUTF16 = `{"stringIndexType":"utf16CodeUnit","content":"😀 a",` +
	`"pages":[{"pageNumber":1,"words":[{"content":"a","span":{"offset":3,"length":1}}],"spans":[{"offset":0,"length":4}]}],` +
	`"paragraphs":[],"sections":[],"styles":[]}`

// with returns base with old replaced by new, which must occur exactly once.
func with(t testing.TB, base, old, new string) string {
	t.Helper()
	if n := strings.Count(base, old); n != 1 {
		t.Fatalf("test bug: %q occurs %d times", old, n)
	}
	return strings.Replace(base, old, new, 1)
}

// mustBeMalformed checks that a cut refused its input with a descriptive ErrMalformed, and never
// through the panic backstop.
func mustBeMalformed(t *testing.T, parts [][]byte, err error, want string) {
	t.Helper()
	switch {
	case err == nil:
		t.Fatalf("no error, %d parts; want one saying %q", len(parts), want)
	case errors.Is(err, realcut.ErrInternal):
		t.Fatalf("realcut panicked instead of refusing the input: %v", err)
	case !errors.Is(err, realcut.ErrMalformed):
		t.Errorf("err = %v, want it to wrap ErrMalformed", err)
	case !strings.Contains(err.Error(), want):
		t.Errorf("err = %v\nwant it to say %q", err, want)
	}
	if parts != nil {
		t.Errorf("%d parts alongside the error, want none", len(parts))
	}
}

func TestCutDIOfAWellFormedResult(t *testing.T) {
	parts, notes, err := realcut.CutDI([]byte(smallDI), [][]int{{1}, {2}}, textunit.TextElements)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("%d parts, want 2", len(parts))
	}
	p2, err := realcut.Parse(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := p2.Get("content").Str(); c != "Cell\nFigure caption" {
		t.Errorf("part 1 content %q", c)
	}
	if id, _ := realcut.Items(p2.Get("figures"))[0].Get("id").Str(); id != "1.1" {
		t.Errorf("part 1 figure id %q, want 1.1", id)
	}
	t.Logf("notes: %v", notes)
	if _, err := realcut.CutRead([]byte(smallRead), [][]int{{1}, {2}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := realcut.CutDI([]byte(smallUTF16), [][]int{{1}}, textunit.UTF16CodeUnit); err != nil {
		t.Fatal(err)
	}
}

// TestCutDIRefusesMalformedResults feeds CutDI results it cannot follow — spans past or before
// the content, references to pages, items and figures that do not exist, plans that do not fit —
// and wants a descriptive error for each, never a panic. A merge that shifts offsets wrongly makes
// exactly such results, and a panic there would abort every test after it.
func TestCutDIRefusesMalformedResults(t *testing.T) {
	two := [][]int{{1}, {2}}
	for _, tc := range []struct {
		name   string
		result string
		plan   [][]int
		unit   textunit.Unit
		want   string
	}{
		// spans, in the result's unit
		{"page span past the content", with(t, smallDI, `"spans":[{"offset":18,"length":19}]}]`, `"spans":[{"offset":18,"length":20}]}]`), two, 0,
			"pages[1].spans[0] {offset 18, length 20} runs past the end of the content, 37 textElements long"},
		{"span shifted one past the content", with(t, smallDI, `"spans":[{"offset":23,"length":14}],"content"`, `"spans":[{"offset":24,"length":14}],"content"`), two, 0,
			"paragraphs[3].spans[0] {offset 24, length 14} runs past the end"},
		{"word span past the content", with(t, smallDI, `"span":{"offset":18,"length":4}`, `"span":{"offset":40,"length":4}`), two, 0,
			"pages[1].words[0].span {offset 40, length 4} runs past the end"},
		{"negative offset", with(t, smallDI, `"spans":[{"offset":6,"length":11}]`, `"spans":[{"offset":-6,"length":11}]`), two, 0,
			"paragraphs[1].spans[0] {offset -6, length 11} is negative"},
		{"negative length", with(t, smallDI, `"spans":[{"offset":6,"length":11}]`, `"spans":[{"offset":6,"length":-11}]`), two, 0,
			"is negative"},
		{"huge length", with(t, smallDI, `"spans":[{"offset":6,"length":11}]`, `"spans":[{"offset":6,"length":9223372036854775807}]`), two, 0,
			"runs past the end"},
		{"span not an object", with(t, smallDI, `"span":{"offset":0,"length":5}`, `"span":5`), two, 0,
			"pages[0].words[0].span 5 is not an {offset, length} span"},
		{"offset not an integer", with(t, smallDI, `"span":{"offset":0,"length":5}`, `"span":{"offset":"0","length":5}`), two, 0,
			"is not an {offset, length} span"},
		{"spans not an array", with(t, smallDI, `"styles":[{"confidence":1,"spans":[{"offset":0,"length":5}]`, `"styles":[{"confidence":1,"spans":{"offset":0,"length":5}`), two, 0,
			"styles[0].spans is {"},
		{"span inside a surrogate pair", with(t, smallUTF16, `"span":{"offset":3,"length":1}`, `"span":{"offset":1,"length":1}`), [][]int{{1}}, textunit.UTF16CodeUnit,
			"cut inside a surrogate pair"},
		// pointers
		{"pointer past its member", with(t, smallDI, `"elements":["/paragraphs/2"]`, `"elements":["/paragraphs/9"]`), two, 0,
			`tables[0].cells[0].elements[0] "/paragraphs/9" points past the result's 4 paragraphs`},
		{"negative pointer", with(t, smallDI, `"elements":["/paragraphs/2"]`, `"elements":["/paragraphs/-1"]`), two, 0,
			`"/paragraphs/-1" is not a pointer /<member>/<index>`},
		{"pointer with a leading zero", with(t, smallDI, `"elements":["/paragraphs/2"]`, `"elements":["/paragraphs/02"]`), two, 0,
			"is not a pointer"},
		{"pointer of three segments", with(t, smallDI, `"elements":["/paragraphs/2"]`, `"elements":["/tables/0/cells"]`), two, 0,
			"is not a pointer"},
		{"pointer into a member the cut does not renumber", with(t, smallDI, `"elements":["/paragraphs/2"]`, `"elements":["/words/0"]`), two, 0,
			`"/words/0" points into words`},
		{"section pointer past the sections", with(t, smallDI, `"/sections/1"`, `"/sections/7"`), two, 0,
			`sections[0].elements[1] "/sections/7" points past the result's 2 sections`},
		{"pointer not a string", with(t, smallDI, `"elements":["/paragraphs/3"]`, `"elements":[3]`), two, 0,
			"figures[0].elements[0] 3 is not a pointer"},
		// pages
		{"region on a page the result does not have", with(t, smallDI, `"polygon":[0,1,1,1,1,2,0,2]}],"spans":[{"offset":6`, `"polygon":[0,1,1,1,1,2,0,2]},{"pageNumber":3}],"spans":[{"offset":6`), two, 0,
			"paragraphs[1].boundingRegions[1].pageNumber 3 is not a page of the result"},
		{"region without a page", with(t, smallDI, `"boundingRegions":[{"pageNumber":1,"polygon":[0,0,1,0,1,1,0,1]}]`, `"boundingRegions":[{"polygon":[0,0,1,0,1,1,0,1]}]`), two, 0,
			"paragraphs[0].boundingRegions[0].pageNumber (absent) is not a page of the result"},
		{"plan page the result does not have", smallDI, [][]int{{1}, {3}}, 0,
			"part 1: page 3 is not in the result"},
		{"page numbered twice", with(t, smallDI, `{"pageNumber":2,"words"`, `{"pageNumber":1,"words"`), [][]int{{1}}, 0,
			"pages[0] and pages[1] are both page 1"},
		{"page number not a number", with(t, smallDI, `{"pageNumber":2,"words"`, `{"pageNumber":"2","words"`), [][]int{{1}}, 0,
			`pages[1].pageNumber "2" is not a page number`},
		// figure ids
		{"figure id not page.n", with(t, smallDI, `"id":"2.1"`, `"id":"x"`), two, 0, `figures[0].id "x" is not <page>.<n>`},
		{"figure id without n", with(t, smallDI, `"id":"2.1"`, `"id":"2."`), two, 0, "is not <page>.<n>"},
		{"figure id without page", with(t, smallDI, `"id":"2.1"`, `"id":".1"`), two, 0, "is not <page>.<n>"},
		{"figure id with a leading zero", with(t, smallDI, `"id":"2.1"`, `"id":"02.1"`), two, 0, "is not <page>.<n>"},
		{"figure id numbered from zero", with(t, smallDI, `"id":"2.1"`, `"id":"2.0"`), two, 0, "is not <page>.<n>"},
		{"figure id empty", with(t, smallDI, `"id":"2.1"`, `"id":""`), two, 0, "is not <page>.<n>"},
		{"figure id a number", with(t, smallDI, `"id":"2.1"`, `"id":2.1`), two, 0, "figures[0].id 2.1 is not <page>.<n>"},
		{"figure id on a page the result does not have", with(t, smallDI, `"id":"2.1"`, `"id":"9.1"`), two, 0,
			`figures[0].id "9.1" names page 9, which is not in the result`},
		{"figure id on another page than the figure", with(t, smallDI, `"id":"2.1"`, `"id":"1.1"`), two, 0,
			`figures[0].id "1.1" names page 1, but its first bounding region is on page 2`},
		// content and unit
		{"content not a string", with(t, smallDI, `"content":"Title\nHello world\nCell\nFigure caption"`, `"content":37`), two, 0,
			"content is not a JSON string"},
		{"content missing", with(t, smallDI, `"content":"Title\nHello world\nCell\nFigure caption",`, ``), two, 0,
			"the result has no content"},
		{"content with a broken escape", with(t, smallDI, `Figure caption",`, `Figure \u12xy",`), two, 0,
			`bad \u escape`},
		{"unit other than the result's", smallDI, two, textunit.UnicodeCodePoint,
			"the result counts offsets in textElements, but the cut was asked for unicodeCodePoint"},
		{"unknown unit", with(t, smallDI, `"stringIndexType":"textElements"`, `"stringIndexType":"bytes"`), two, 0,
			`stringIndexType "bytes" is not a unit`},
		// shape
		{"not JSON", smallDI[:100], two, 0, "ojson"},
		{"not an object", `[1,2]`, two, 0, "the result is [1,2], not an object"},
		{"pages missing", with(t, smallDI, `"pages":[`, `"notPages":[`), two, 0, "the result has no pages"},
		{"paragraphs not an array", with(t, smallDI, `"paragraphs":[`, `"paragraphs":7,"x":[`), two, 0, "paragraphs is 7, not an array"},
		{"paragraph not an object", with(t, smallDI, `"paragraphs":[`, `"paragraphs":[7,`), two, 0, "paragraphs[0] is 7, not an object"},
		{"member twice", with(t, smallDI, `"contentFormat":"text"}`, `"contentFormat":"text","paragraphs":[]}`), two, 0,
			`member "paragraphs" appears twice`},
		// plan
		{"no parts", smallDI, nil, 0, "the plan has no parts"},
		{"empty part", smallDI, [][]int{{1}, {}}, 0, "part 1 of the plan has no pages"},
		{"page in two parts", smallDI, [][]int{{1}, {1, 2}}, 0, "page 1 is planned in part 0 and again in part 1"},
		{"parts out of order", smallDI, [][]int{{2}, {1}}, 0, "part 0's text [18,37) overlaps part 1's [0,17)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, _, err := realcut.CutDI([]byte(tc.result), tc.plan, tc.unit)
			mustBeMalformed(t, parts, err, tc.want)
		})
	}
}

// TestCutMapsAFigureIDToItsPart: the cut inverts the merge's figure-id rule, so a figure's
// "<page>.<n>" must name a page of the part the figure is cut into. validate already ties the id
// to the figure's first region (TestCutDIRefusesMalformedResults); a figure with no region is
// placed by its span, and its id is then the only page it names. An id on any page but one of its
// own part's is an error, not a local number made up for it: localOf answers 0 for a page outside
// the plan and another part's local number for a page of another part, so a merge that left such
// an id unmapped would come back unchanged and pass.
func TestCutMapsAFigureIDToItsPart(t *testing.T) {
	noRegion := func(id string) string {
		s := with(t, smallDI, `"figures":[{"id":"2.1","boundingRegions":[{"pageNumber":2,"polygon":[0,1,1,1,1,2,0,2]}],`,
			`"figures":[{"id":"2.1",`)
		return with(t, s, `"id":"2.1"`, `"id":"`+id+`"`)
	}
	for _, tc := range []struct {
		name    string
		result  string
		plan    [][]int
		wantID  string // the figure's id in the last part
		wantErr string
	}{
		{"its page cut alone", smallDI, [][]int{{1}, {2}}, "1.1", ""},
		{"one part", with(t, smallDI, `"id":"2.1"`, `"id":"2.3"`), [][]int{{1, 2}}, "2.3", ""},
		{"a pages= selection", smallDI, [][]int{{2}}, "1.1", ""},
		{"placed by its span", noRegion("2.1"), [][]int{{1}, {2}}, "1.1", ""},
		{"placed by its span, its id on another part's page", noRegion("1.1"), [][]int{{1}, {2}}, "",
			`part 1: figure id "1.1" names page 1, which is in part 0, not this part (pages [2])`},
		{"placed by its span, its id on a page outside the plan", noRegion("1.1"), [][]int{{2}}, "",
			`part 0: figure id "1.1" names page 1, which is in no part of the plan, not this part (pages [2])`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, _, err := realcut.CutDI([]byte(tc.result), tc.plan, textunit.TextElements)
			if tc.wantErr != "" {
				mustBeMalformed(t, parts, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			last, err := realcut.Parse(parts[len(parts)-1])
			if err != nil {
				t.Fatal(err)
			}
			figs := realcut.Items(last.Get("figures"))
			if len(figs) != 1 {
				t.Fatalf("the last part has %d figures, want 1", len(figs))
			}
			if got, _ := figs[0].Get("id").Str(); got != tc.wantID {
				t.Errorf("cut id %q, want %q", got, tc.wantID)
			}
		})
	}
}

func TestCutReadRefusesMalformedResults(t *testing.T) {
	two := [][]int{{1}, {2}}
	for _, tc := range []struct {
		name   string
		result string
		plan   [][]int
		want   string
	}{
		{"not JSON", smallRead[:40], two, "ojson"},
		{"no readResults", with(t, smallRead, `"readResults"`, `"results"`), two, "readResults is (absent), not an array"},
		{"readResults not an array", `{"readResults":{"page":1}}`, two, `readResults is {"page":1}, not an array`},
		{"page not a number", with(t, smallRead, `{"page":2,`, `{"page":"2",`), [][]int{{1}}, `readResults[1].page "2" is not a page number`},
		{"page numbered twice", with(t, smallRead, `{"page":2,`, `{"page":1,`), [][]int{{1}}, "readResults[0] and readResults[1] are both page 1"},
		{"plan page the result does not have", smallRead, [][]int{{1}, {3}}, "part 1: page 3 is not in the result"},
		{"empty part", smallRead, [][]int{{}, {1, 2}}, "part 0 of the plan has no pages"},
		{"page in two parts", smallRead, [][]int{{1, 2}, {2}}, "page 2 is planned in part 0 and again in part 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := realcut.CutRead([]byte(tc.result), tc.plan)
			mustBeMalformed(t, parts, err, tc.want)
		})
	}
}

// FuzzCut: whatever the bytes, a cut gives parts that parse or an ErrMalformed — never a panic,
// and never the backstop that turns one into ErrInternal.
func FuzzCut(f *testing.F) {
	for _, s := range []string{smallDI, smallRead, smallUTF16,
		with(f, smallDI, `"length":19}]}]`, `"length":20}]}]`),
		with(f, smallDI, `"/paragraphs/2"`, `"/paragraphs/9"`),
		with(f, smallDI, `"/sections/1"`, `"/sections/-1"`),
		with(f, smallDI, `"id":"2.1"`, `"id":"9.x"`),
		with(f, smallDI, `"contentFormat":"text"`, `"contentFormat":"markdown"`),
		`{"content":"\\","pages":[]}`, `{"content":"\u","pages":[]}`, `{"content":5}`, `null`, `""`,
	} {
		f.Add([]byte(s), byte(0))
	}
	plans := [][][]int{{{1}, {2}}, {{1, 2}}, {{2}}, {{1}}, {{2}, {1}}}
	f.Fuzz(func(t *testing.T, b []byte, which byte) {
		plan := plans[int(which)%len(plans)]
		for _, unit := range []textunit.Unit{textunit.TextElements, textunit.UnicodeCodePoint, textunit.UTF16CodeUnit} {
			parts, _, err := realcut.CutDI(b, plan, unit)
			checkFuzzed(t, "CutDI", parts, plan, err)
		}
		parts, err := realcut.CutRead(b, plan)
		checkFuzzed(t, "CutRead", parts, plan, err)
	})
}

func checkFuzzed(t *testing.T, what string, parts [][]byte, plan [][]int, err error) {
	t.Helper()
	switch {
	case errors.Is(err, realcut.ErrInternal):
		t.Fatalf("%s panicked: %v", what, err)
	case err != nil && !errors.Is(err, realcut.ErrMalformed):
		t.Fatalf("%s: err = %v, want ErrMalformed", what, err)
	case err == nil && len(parts) != len(plan):
		t.Fatalf("%s: %d parts for %d planned", what, len(parts), len(plan))
	case err == nil:
		for k, p := range parts {
			if _, perr := realcut.Parse(p); perr != nil {
				t.Fatalf("%s: part %d does not parse: %v\n%s", what, k, perr, p)
			}
		}
	}
}

// FuzzCutFields keeps the result well-formed JSON and fuzzes the numbers the cut follows: a page's
// span, a word's span, a cell's pointer, a paragraph's region page and a figure id's page. Every
// combination cuts or is refused with ErrMalformed; one inside the result cuts.
func FuzzCutFields(f *testing.F) {
	f.Add(18, 19, 6, 11, 2, 1, 2, byte(0))  // as written
	f.Add(18, 20, 6, 11, 2, 1, 2, byte(0))  // page span one past the content
	f.Add(19, 19, 6, 11, 2, 1, 2, byte(0))  // shifted by one
	f.Add(18, 19, -6, 11, 2, 1, 2, byte(1)) // negative offset
	f.Add(18, 19, 6, 11, 9, 1, 2, byte(2))  // pointer out of range
	f.Add(18, 19, 6, 11, -1, 1, 2, byte(3)) // negative pointer
	f.Add(18, 19, 6, 11, 2, 7, 2, byte(4))  // region page out of range
	f.Add(18, 19, 6, 11, 2, 1, 0, byte(0))  // figure id page 0
	f.Add(18, 19, 6, 11, 2, 1, 5, byte(0))  // figure id page out of range
	plans := [][][]int{{{1}, {2}}, {{1, 2}}, {{2}}, {{1}}, {{2}, {1}}}
	f.Fuzz(func(t *testing.T, pageOff, pageLen, wordOff, wordLen, ptr, regionPage, figPage int, which byte) {
		s := with(t, smallDI, `"spans":[{"offset":18,"length":19}]}]`,
			fmt.Sprintf(`"spans":[{"offset":%d,"length":%d}]}]`, pageOff, pageLen))
		s = with(t, s, `"span":{"offset":6,"length":5}`, fmt.Sprintf(`"span":{"offset":%d,"length":%d}`, wordOff, wordLen))
		s = with(t, s, `"elements":["/paragraphs/2"]`, fmt.Sprintf(`"elements":["/paragraphs/%d"]`, ptr))
		s = with(t, s, `{"pageNumber":1,"polygon":[0,1,1,1,1,2,0,2]}`, fmt.Sprintf(`{"pageNumber":%d,"polygon":[0,1,1,1,1,2,0,2]}`, regionPage))
		s = with(t, s, `"id":"2.1"`, fmt.Sprintf(`"id":"%d.1"`, figPage))
		plan := plans[int(which)%len(plans)]
		parts, _, err := realcut.CutDI([]byte(s), plan, textunit.TextElements)
		checkFuzzed(t, "CutDI", parts, plan, err)
		inside := pageOff == 18 && pageLen == 19 && wordOff >= 0 && wordLen >= 0 && wordOff <= 37-wordLen &&
			ptr >= 0 && ptr < 4 && regionPage == 1 && figPage == 2 && int(which)%len(plans) != 4
		if inside && err != nil {
			t.Fatalf("a well-formed result was refused: %v", err)
		}
	})
}
