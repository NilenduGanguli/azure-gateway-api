package docgen_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azschema"
	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

var units = []textunit.Unit{textunit.TextElements, textunit.UnicodeCodePoint, textunit.UTF16CodeUnit}

func pageRange(from, to int) []int {
	var ps []int
	for p := from; p <= to; p++ {
		ps = append(ps, p)
	}
	return ps
}

// pageSets cover tables (p%3), figures (p%4), both (p%12), selection marks (p%5), blank pages
// under Realistic (p%7), neither, and non-consecutive pages.
var pageSets = [][]int{{1}, {3}, {4}, {7}, {12}, pageRange(1, 15), {2, 5, 9, 24, 25}, {6, 7, 8}, {}}

// shapes are the option sets every test runs: the plain shape, and every option real documents
// need.
var shapes = map[string]func(u textunit.Unit) docgen.Options{
	"plain":     func(u textunit.Unit) docgen.Options { return docgen.Options{Unit: u} },
	"realistic": docgen.Realistic,
}

var reference *azschema.Reference

func ref(t *testing.T) *azschema.Reference {
	t.Helper()
	if reference == nil {
		r, err := azschema.Load("../../test/azure-reference")
		if err != nil {
			t.Fatal(err)
		}
		reference = r
	}
	return reference
}

func show(fs []azschema.Finding) string {
	var lines []string
	for i, f := range fs {
		if i == 8 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(fs)-8))
			break
		}
		lines = append(lines, f.String())
	}
	return strings.Join(lines, "\n  ")
}

func numbers(pages []int, n docgen.Numbering) []int {
	out := []int{}
	for i, p := range pages {
		if n == docgen.Local {
			p = i + 1
		}
		out = append(out, p)
	}
	return out
}

// TestDIIsAzureShaped validates every shape against the 2024-11-30 spec, strictly, and against
// what real layout output always satisfies: spans that slice the content to their items' text,
// pages joined by one separator and ending where the content does, pointers that resolve, one
// section tree, figure ids on their page, the service's member order and presence.
func TestDIIsAzureShaped(t *testing.T) {
	for name, shape := range shapes {
		for _, pages := range pageSets {
			for _, u := range units {
				for _, n := range []docgen.Numbering{docgen.Original, docgen.Local} {
					o := shape(u)
					o.TitleOnFirstPage = len(pages)%2 == 1
					b := docgen.DI(pages, n, o)
					var compact bytes.Buffer
					if err := json.Compact(&compact, b); err != nil || !bytes.Equal(compact.Bytes(), b) {
						t.Fatalf("%s %v %v: not compact JSON: %v", name, pages, u, err)
					}
					fs := azschema.Errors(ref(t).Result(azschema.DocumentIntelligence, b, azschema.Expect{
						APIVersion: "2024-11-30", ModelID: "prebuilt-layout", StringIndexType: u.String(),
						ContentFormat: "text", Pages: numbers(pages, n),
					}))
					if len(fs) > 0 {
						t.Errorf("%s pages %v %v numbering %d:\n  %s", name, pages, u, n, show(fs))
					}
				}
			}
		}
	}
}

// TestLegacyIsFormRecognizerShaped: the 2023-07-31 shape validates against that API's spec, which
// has no sections, figures, contentFormat, cell elements or captions.
func TestLegacyIsFormRecognizerShaped(t *testing.T) {
	for _, pages := range pageSets {
		o := docgen.Realistic(textunit.TextElements)
		o.Legacy = true
		b := docgen.DI(pages, docgen.Original, o)
		fs := azschema.Errors(ref(t).Result(azschema.FormRecognizer, b, azschema.Expect{
			APIVersion: docgen.LegacyAPIVersion, Pages: numbers(pages, docgen.Original)}))
		if len(fs) > 0 {
			t.Errorf("pages %v:\n  %s", pages, show(fs))
		}
		for _, absent := range []string{`"sections"`, `"figures"`, `"contentFormat"`, `"elements"`, `"caption"`} {
			if bytes.Contains(b, []byte(absent)) {
				t.Errorf("pages %v: the 2023-07-31 shape has %s", pages, absent)
			}
		}
	}
}

// The decoded shape the remaining tests read.
type (
	span   struct{ Offset, Length int }
	region struct{ PageNumber int }
	item   struct {
		Content         string
		Role            string
		Spans           []span
		BoundingRegions []region
		Elements        []string
	}
	diResult struct {
		Content string
		Pages   []struct {
			PageNumber     int
			Words, Lines   []json.RawMessage
			SelectionMarks []struct{ Span span }
			Spans          []span
		}
		Paragraphs []item
		Tables     []struct {
			Cells   []item
			Caption *item
			Spans   []span
		}
		Figures []struct {
			ID       string
			Spans    []span
			Elements []string
			Caption  *item
		}
		Sections []item
	}
)

func decode(t *testing.T, b []byte) diResult {
	t.Helper()
	var r diResult
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDIContent: each page's paragraphs, in reading order, for each shape.
func TestDIContent(t *testing.T) {
	for _, pages := range [][]int{pageRange(1, 15), {2, 5, 9, 24, 25}} {
		for name, shape := range shapes {
			o := shape(textunit.TextElements)
			r := decode(t, docgen.DI(pages, docgen.Original, o))
			var got, want []string
			for _, p := range r.Paragraphs {
				got = append(got, p.Content)
			}
			for _, p := range pages {
				if o.BlankEvery > 0 && p%o.BlankEvery == 0 {
					continue
				}
				if o.Furniture && p%2 == 0 {
					want = append(want, "Contoso quarterly report")
				}
				want = append(want, fmt.Sprintf("Heading P%d", p), fmt.Sprintf("Line %d.1 naïve 👩‍👩‍👧 text", p),
					fmt.Sprintf("Line %d.2 plain", p))
				if o.SelectionMarks && p%5 == 0 {
					want = append(want, fmt.Sprintf(":selected: Agree %d", p))
				}
				if p%3 == 0 {
					want = append(want, fmt.Sprintf("A%d", p), fmt.Sprintf("B%d", p))
					if o.Captions {
						want = append(want, fmt.Sprintf("Table %d: totals", p))
					}
				}
				if p%4 == 0 {
					if o.Captions {
						want = append(want, fmt.Sprintf("Figure %d: trend", p))
					}
					want = append(want, fmt.Sprintf("Chart %d", p))
				}
				if o.Furniture {
					if p%2 == 1 {
						want = append(want, "Confidential")
					}
					want = append(want, fmt.Sprintf("Page %d", p))
				}
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("%s %v: paragraphs\n%q\nwant\n%q", name, pages, got, want)
			}
			if strings.Join(got, "\n") != strings.ReplaceAll(r.Content, "\n\n", "\n") {
				t.Errorf("%s %v: content is not the paragraphs joined by separators", name, pages)
			}
		}
	}
}

// TestDISectionsLeaveFurnitureOut: as in real output, no section holds or covers a page header,
// footer or number, or a blank page, and the root's spans break around them; everything else a
// page has is held by exactly one section, its own; a caption is its item's caption member and
// not one of its elements.
func TestDISectionsLeaveFurnitureOut(t *testing.T) {
	o := docgen.Realistic(textunit.TextElements)
	pages := pageRange(1, 15)
	r := decode(t, docgen.DI(pages, docgen.Original, o))
	held := map[string]int{}
	for _, s := range r.Sections {
		for _, e := range s.Elements {
			held[e]++
		}
	}
	inRoot := func(sp span) bool {
		for _, rs := range r.Sections[0].Spans {
			if sp.Offset >= rs.Offset && sp.Offset+sp.Length <= rs.Offset+rs.Length {
				return true
			}
		}
		return false
	}
	overlapsRoot := func(sp span) bool {
		for _, rs := range r.Sections[0].Spans {
			if sp.Offset < rs.Offset+rs.Length && rs.Offset < sp.Offset+sp.Length {
				return true
			}
		}
		return false
	}
	inCell := map[string]bool{}
	for _, tb := range r.Tables {
		for _, c := range tb.Cells {
			inCell[c.Elements[0]] = true
		}
	}
	for i, p := range r.Paragraphs {
		ptr := fmt.Sprintf("/paragraphs/%d", i)
		switch p.Role {
		case "pageHeader", "pageFooter", "pageNumber":
			if held[ptr] > 0 || overlapsRoot(p.Spans[0]) {
				t.Errorf("%s %q is in a section", p.Role, p.Content)
			}
		default:
			if !inRoot(p.Spans[0]) {
				t.Errorf("paragraph %q lies outside the root's spans %v", p.Content, r.Sections[0].Spans)
			}
			if strings.HasPrefix(p.Content, "Chart ") || inCell[ptr] {
				if held[ptr] != 0 {
					t.Errorf("%q, a figure's or a cell's, is held by a section", p.Content)
				}
			} else if held[ptr] != 1 {
				t.Errorf("paragraph %q is held by %d sections", p.Content, held[ptr])
			}
		}
	}
	// One section per non-blank page, the root over them; the root's spans break at every page,
	// since every page has a footer or a header, and around blank page 7.
	if len(r.Sections) != 1+len(pages)-2 {
		t.Errorf("%d sections, want the root and one per non-blank page", len(r.Sections))
	}
	if n := len(r.Sections[0].Spans); n != len(pages)-2 {
		t.Errorf("the root has %d spans, want %d", n, len(pages)-2)
	}
	for _, f := range r.Figures {
		if f.Caption == nil || f.Caption.Elements[0] == f.Elements[0] {
			t.Errorf("figure %s caption %+v is not a member apart from its elements %v", f.ID, f.Caption, f.Elements)
		}
	}
	for _, tb := range r.Tables {
		if tb.Caption == nil || !strings.HasPrefix(tb.Caption.Content, "Table ") {
			t.Errorf("table caption %+v", tb.Caption)
		}
	}
	marks := 0
	for _, pg := range r.Pages {
		marks += len(pg.SelectionMarks)
		if pg.PageNumber%7 == 0 && (len(pg.Words) != 0 || len(pg.Lines) != 0 || pg.Spans[0].Length != 0) {
			t.Errorf("page %d is not blank", pg.PageNumber)
		}
	}
	if marks != 3 { // pages 5, 10 and 15
		t.Errorf("%d selection marks, want 3", marks)
	}
}

func TestDIRolesAndRegions(t *testing.T) {
	pages := []int{3, 4, 5}
	for _, title := range []bool{false, true} {
		b := docgen.DI(pages, docgen.Local, docgen.Options{TitleOnFirstPage: title})
		r := decode(t, b)
		var roles []string
		for _, para := range r.Paragraphs {
			if para.Role != "" {
				roles = append(roles, para.Role)
			}
			for _, br := range para.BoundingRegions {
				if br.PageNumber < 1 || br.PageNumber > len(pages) {
					t.Errorf("paragraph %q region %+v", para.Content, br)
				}
			}
		}
		want := "sectionHeading sectionHeading sectionHeading"
		if title {
			want = "title sectionHeading sectionHeading"
		}
		if got := strings.Join(roles, " "); got != want {
			t.Errorf("TitleOnFirstPage=%v: roles %q, want %q", title, got, want)
		}
	}
}

func TestDIIsDeterministic(t *testing.T) {
	for _, pages := range pageSets {
		for _, n := range []docgen.Numbering{docgen.Original, docgen.Local} {
			o := docgen.Realistic(textunit.UTF16CodeUnit)
			o.TitleOnFirstPage = true
			if a, b := docgen.DI(pages, n, o), docgen.DI(pages, n, o); !bytes.Equal(a, b) {
				t.Errorf("pages %v: two calls differ", pages)
			}
			if a, b := docgen.Read(pages, n, docgen.ReadOptions{}), docgen.Read(pages, n, docgen.ReadOptions{}); !bytes.Equal(a, b) {
				t.Errorf("pages %v: two Read calls differ", pages)
			}
		}
	}
}

// renumber maps a docgen.Local result's page numbers and figure ids to the original pages, through the
// rewriter the merge uses. It is the whole difference docgen.Local numbering is allowed to make.
func renumber(t *testing.T, b []byte, pages []int, pageKey string) []byte {
	t.Helper()
	figureID := regexp.MustCompile(`^"(\d+)\.(.+)"$`)
	var out bytes.Buffer
	err := jsonx.Rewrite(&out, bytes.NewReader(b), nil, func(path []jsonx.PathElem, kind jsonx.Kind, raw []byte) ([]byte, bool) {
		n := len(path)
		last := path[n-1]
		switch {
		case kind == jsonx.Number && last.Key == pageKey:
			v, _ := strconv.Atoi(string(raw))
			return []byte(strconv.Itoa(pages[v-1])), true
		case kind == jsonx.String && n == 3 && path[0].Key == "figures" && last.Key == "id":
			m := figureID.FindSubmatch(raw)
			v, _ := strconv.Atoi(string(m[1]))
			return []byte(fmt.Sprintf(`"%d.%s"`, pages[v-1], m[2])), true
		}
		return nil, false
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestLocalNumberingDiffersOnlyInPageNumbers(t *testing.T) {
	for _, pages := range [][]int{{4, 5, 6}, {8, 12, 13, 16}, {2, 5, 9, 24, 25}} {
		for name, shape := range shapes {
			for _, u := range units {
				o := shape(u)
				local, orig := docgen.DI(pages, docgen.Local, o), docgen.DI(pages, docgen.Original, o)
				if bytes.Equal(local, orig) {
					t.Fatalf("pages %v: docgen.Local and docgen.Original are identical", pages)
				}
				if got := renumber(t, local, pages, "pageNumber"); !bytes.Equal(got, orig) {
					t.Errorf("%s pages %v %v: renumbered docgen.Local differs from docgen.Original", name, pages, u)
				}
			}
		}
		for _, image := range []bool{false, true} {
			o := docgen.ReadOptions{Image: image}
			local, orig := docgen.Read(pages, docgen.Local, o), docgen.Read(pages, docgen.Original, o)
			if bytes.Equal(local, orig) {
				t.Fatalf("Read pages %v: docgen.Local and docgen.Original are identical", pages)
			}
			if got := renumber(t, local, pages, "page"); !bytes.Equal(got, orig) {
				t.Errorf("Read pages %v: renumbered docgen.Local differs from docgen.Original", pages)
			}
		}
	}
	// Pages 1..k are numbered the same either way.
	o := docgen.Realistic(textunit.TextElements)
	if !bytes.Equal(docgen.DI(pageRange(1, 5), docgen.Local, o), docgen.DI(pageRange(1, 5), docgen.Original, o)) {
		t.Error("DI pages 1-5: docgen.Local differs from docgen.Original")
	}
}

// TestRead validates Read results against the v3.2 spec and real output: version "3.2.0", the
// 2022-04-30 model's appearance on every line, a PDF in inches and an image in pixels.
func TestRead(t *testing.T) {
	type rpage struct {
		Page  int
		Width float64
		Unit  string
		Lines []struct {
			BoundingBox []json.Number
			Text        string
		}
	}
	pages := []int{2, 7, 8}
	for _, image := range []bool{false, true} {
		for _, n := range []docgen.Numbering{docgen.Original, docgen.Local} {
			b := docgen.Read(pages, n, docgen.ReadOptions{Image: image})
			var compact bytes.Buffer
			if err := json.Compact(&compact, b); err != nil || !bytes.Equal(compact.Bytes(), b) {
				t.Fatalf("not valid compact JSON: %v\n%s", err, b)
			}
			unit := "inch"
			if image {
				unit = "pixel"
			}
			fs := azschema.Errors(ref(t).Result(azschema.Read, b, azschema.Expect{
				ModelVersion: "2022-04-30", Unit: unit, Pages: numbers(pages, n)}))
			if len(fs) > 0 {
				t.Errorf("image=%v numbering %d:\n  %s", image, n, show(fs))
			}
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.UseNumber()
			var r struct{ ReadResults []rpage }
			if err := dec.Decode(&r); err != nil {
				t.Fatal(err)
			}
			for i, rr := range r.ReadResults {
				if want := fmt.Sprintf("Read line %d", pages[i]); rr.Lines[0].Text != want {
					t.Errorf("page %d first line %q, want %q", i, rr.Lines[0].Text, want)
				}
				for _, c := range rr.Lines[0].BoundingBox {
					if isInt := !strings.Contains(string(c), "."); isInt != image {
						t.Errorf("image=%v: coordinate %s", image, c)
					}
				}
			}
		}
	}
}
