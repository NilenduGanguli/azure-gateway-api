package docgen_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rivo/uniseg"

	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// The decoded shape of a DI result, strict: decoding rejects any member not declared here, so a
// misspelt member name in the generator fails the tests.
type (
	span struct {
		Offset int `json:"offset"`
		Length int `json:"length"`
	}
	region struct {
		PageNumber int       `json:"pageNumber"`
		Polygon    []float64 `json:"polygon"`
	}
	word struct {
		Content    string    `json:"content"`
		Polygon    []float64 `json:"polygon"`
		Confidence float64   `json:"confidence"`
		Span       span      `json:"span"`
	}
	line struct {
		Content string    `json:"content"`
		Polygon []float64 `json:"polygon"`
		Spans   []span    `json:"spans"`
	}
	page struct {
		PageNumber int     `json:"pageNumber"`
		Angle      float64 `json:"angle"`
		Width      float64 `json:"width"`
		Height     float64 `json:"height"`
		Unit       string  `json:"unit"`
		Words      []word  `json:"words"`
		Lines      []line  `json:"lines"`
		Spans      []span  `json:"spans"`
	}
	paragraph struct {
		Role            string   `json:"role"`
		Content         string   `json:"content"`
		BoundingRegions []region `json:"boundingRegions"`
		Spans           []span   `json:"spans"`
	}
	cell struct {
		RowIndex        int      `json:"rowIndex"`
		ColumnIndex     int      `json:"columnIndex"`
		Content         string   `json:"content"`
		BoundingRegions []region `json:"boundingRegions"`
		Spans           []span   `json:"spans"`
		Elements        []string `json:"elements"`
	}
	table struct {
		RowCount        int      `json:"rowCount"`
		ColumnCount     int      `json:"columnCount"`
		Cells           []cell   `json:"cells"`
		BoundingRegions []region `json:"boundingRegions"`
		Spans           []span   `json:"spans"`
	}
	figure struct {
		ID              string   `json:"id"`
		BoundingRegions []region `json:"boundingRegions"`
		Spans           []span   `json:"spans"`
		Elements        []string `json:"elements"`
	}
	section struct {
		Spans    []span   `json:"spans"`
		Elements []string `json:"elements"`
	}
	diResult struct {
		APIVersion      string      `json:"apiVersion"`
		ModelID         string      `json:"modelId"`
		StringIndexType string      `json:"stringIndexType"`
		Content         string      `json:"content"`
		Pages           []page      `json:"pages"`
		Paragraphs      []paragraph `json:"paragraphs"`
		Tables          []table     `json:"tables"`
		Figures         []figure    `json:"figures"`
		Sections        []section   `json:"sections"`
		ContentFormat   string      `json:"contentFormat"`
	}
)

var units = []textunit.Unit{textunit.TextElements, textunit.UnicodeCodePoint, textunit.UTF16CodeUnit}

func pageRange(from, to int) []int {
	var ps []int
	for p := from; p <= to; p++ {
		ps = append(ps, p)
	}
	return ps
}

// pageSets cover tables (p%3), figures (p%4), both (p%12), neither, and non-consecutive pages.
var pageSets = [][]int{{1}, {3}, {4}, {12}, pageRange(1, 13), {2, 5, 9, 24, 25}, {}}

func decodeDI(t *testing.T, b []byte) diResult {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r diResult
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return r
}

// slice returns the text at [off, off+n) of s, counted in u, and whether that range is inside s
// and on unit boundaries.
func slice(s string, off, n int, u textunit.Unit) (string, bool) {
	if off < 0 || n < 0 {
		return "", false
	}
	// bounds[i] is the byte offset where unit i starts; the final entry is len(s).
	var bounds []int
	switch u {
	case textunit.TextElements:
		g := uniseg.NewGraphemes(s)
		for g.Next() {
			from, _ := g.Positions()
			bounds = append(bounds, from)
		}
	case textunit.UnicodeCodePoint:
		for i := range s {
			bounds = append(bounds, i)
		}
	case textunit.UTF16CodeUnit:
		for i, r := range s {
			bounds = append(bounds, i)
			if r > 0xFFFF {
				bounds = append(bounds, -1) // the second half of a surrogate pair: not a boundary
			}
		}
	}
	bounds = append(bounds, len(s))
	if off+n >= len(bounds) || bounds[off] < 0 || bounds[off+n] < 0 {
		return "", false
	}
	return s[bounds[off]:bounds[off+n]], true
}

func spanText(t *testing.T, r diResult, sp span, u textunit.Unit, what string) string {
	t.Helper()
	text, ok := slice(r.Content, sp.Offset, sp.Length, u)
	if !ok {
		t.Fatalf("%s: span %+v is outside the content (%d %v long)", what, sp,
			textunit.Len(r.Content, u), u)
	}
	return text
}

func TestDIIsValidCompactAndOrdered(t *testing.T) {
	for _, pages := range pageSets {
		for _, u := range units {
			b := docgen.DI(pages, docgen.Original, docgen.Options{Unit: u})
			if !json.Valid(b) {
				t.Fatalf("pages %v %v: invalid JSON:\n%s", pages, u, b)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, b); err != nil || !bytes.Equal(compact.Bytes(), b) {
				t.Errorf("pages %v %v: output is not compact", pages, u)
			}
			ms, err := jsonx.Members(bytes.NewReader(b), 0, int64(len(b)))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, m := range ms {
				names = append(names, m.Name)
			}
			want := "apiVersion modelId stringIndexType content pages paragraphs tables figures sections contentFormat"
			if got := strings.Join(names, " "); got != want {
				t.Errorf("member order %q, want %q", got, want)
			}
			r := decodeDI(t, b)
			if r.APIVersion != "2024-11-30" || r.ModelID != "prebuilt-layout" ||
				r.StringIndexType != u.String() || r.ContentFormat != "text" {
				t.Errorf("scalars %q %q %q %q", r.APIVersion, r.ModelID, r.StringIndexType, r.ContentFormat)
			}
		}
	}
}

func TestDIContentAndSpans(t *testing.T) {
	for _, pages := range pageSets {
		for _, u := range units {
			r := decodeDI(t, docgen.DI(pages, docgen.Original, docgen.Options{Unit: u}))
			if len(r.Pages) != len(pages) {
				t.Fatalf("%d pages, want %d", len(r.Pages), len(pages))
			}
			var pageTexts []string
			for i, pg := range r.Pages {
				p := pages[i]
				var lines []string
				for _, ln := range pg.Lines {
					lines = append(lines, ln.Content)
					if got := spanText(t, r, ln.Spans[0], u, "line"); got != ln.Content {
						t.Errorf("line span text %q, content %q", got, ln.Content)
					}
				}
				want := []string{fmt.Sprintf("Heading P%d", p), fmt.Sprintf("Line %d.1 naïve 👩‍👩‍👧 text", p),
					fmt.Sprintf("Line %d.2 plain", p)}
				if p%3 == 0 {
					want = append(want, fmt.Sprintf("A%d", p), fmt.Sprintf("B%d", p))
				}
				if p%4 == 0 {
					want = append(want, fmt.Sprintf("Figure %d", p))
				}
				if strings.Join(lines, "|") != strings.Join(want, "|") {
					t.Errorf("page %d lines %q, want %q", p, lines, want)
				}
				pageText := strings.Join(lines, "\n")
				pageTexts = append(pageTexts, pageText)
				if got := spanText(t, r, pg.Spans[0], u, "page"); got != pageText {
					t.Errorf("page %d span text %q, want %q", p, got, pageText)
				}
				var words []string
				for _, w := range pg.Words {
					words = append(words, w.Content)
					if got := spanText(t, r, w.Span, u, "word"); got != w.Content {
						t.Errorf("word span text %q, content %q", got, w.Content)
					}
					if len(w.Polygon) != 8 || w.Confidence <= 0 || w.Confidence > 1 {
						t.Errorf("word %q polygon %v confidence %v", w.Content, w.Polygon, w.Confidence)
					}
				}
				if got, want := strings.Join(words, " "), strings.Join(strings.Fields(strings.Join(lines, " ")), " "); got != want {
					t.Errorf("page %d words %q, want %q", p, got, want)
				}
				if pg.Width != 8.5 || pg.Height != 11 || pg.Unit != "inch" || pg.Angle != 0 {
					t.Errorf("page %d geometry %+v", p, pg)
				}
			}
			if want := strings.Join(pageTexts, "\n"); r.Content != want {
				t.Errorf("content %q, want %q", r.Content, want)
			}
			for _, para := range r.Paragraphs {
				if got := spanText(t, r, para.Spans[0], u, "paragraph"); got != para.Content {
					t.Errorf("paragraph span text %q, content %q", got, para.Content)
				}
			}
			for _, tb := range r.Tables {
				for _, c := range tb.Cells {
					if got := spanText(t, r, c.Spans[0], u, "cell"); got != c.Content {
						t.Errorf("cell span text %q, content %q", got, c.Content)
					}
				}
				want := tb.Cells[0].Content + "\n" + tb.Cells[1].Content
				if got := spanText(t, r, tb.Spans[0], u, "table"); got != want {
					t.Errorf("table span text %q, want %q", got, want)
				}
			}
			for _, f := range r.Figures {
				if got := spanText(t, r, f.Spans[0], u, "figure"); !strings.HasPrefix(got, "Figure ") {
					t.Errorf("figure span text %q", got)
				}
			}
			root := r.Sections[0].Spans
			if len(root) != 1 || root[0].Offset != 0 || root[0].Length != textunit.Len(r.Content, u) {
				t.Errorf("root span %+v, content is %d long", root, textunit.Len(r.Content, u))
			}
			for i, pg := range r.Pages {
				if got := r.Sections[i+1].Spans; len(got) != 1 || got[0] != pg.Spans[0] {
					t.Errorf("section %d spans %+v, page spans %+v", i+1, got, pg.Spans)
				}
			}
		}
	}
}

var pointer = regexp.MustCompile(`^/(paragraphs|tables|figures|sections)/(\d+)$`)

func TestDIPointersResolve(t *testing.T) {
	for _, pages := range pageSets {
		r := decodeDI(t, docgen.DI(pages, docgen.Original, docgen.Options{}))
		resolve := func(ptr string) (string, int) {
			t.Helper()
			m := pointer.FindStringSubmatch(ptr)
			if m == nil {
				t.Fatalf("malformed pointer %q", ptr)
			}
			i, _ := strconv.Atoi(m[2])
			n := map[string]int{"paragraphs": len(r.Paragraphs), "tables": len(r.Tables),
				"figures": len(r.Figures), "sections": len(r.Sections)}[m[1]]
			if i >= n {
				t.Fatalf("pointer %q past the end of %d %s", ptr, n, m[1])
			}
			return m[1], i
		}
		if len(r.Sections) != len(pages)+1 {
			t.Fatalf("%d sections for %d pages", len(r.Sections), len(pages))
		}
		for i, ptr := range r.Sections[0].Elements {
			if ptr != fmt.Sprintf("/sections/%d", i+1) {
				t.Errorf("root element %d is %q", i, ptr)
			}
		}
		if len(r.Sections[0].Elements) != len(pages) {
			t.Errorf("root has %d elements for %d pages", len(r.Sections[0].Elements), len(pages))
		}
		tables, figures := 0, 0
		for i, p := range pages {
			var got []string
			for _, ptr := range r.Sections[i+1].Elements {
				kind, j := resolve(ptr)
				switch kind {
				case "paragraphs":
					got = append(got, r.Paragraphs[j].Content)
				case "tables":
					got = append(got, "table:"+r.Tables[j].Cells[0].Content)
					tables++
				case "figures":
					got = append(got, "figure:"+r.Figures[j].ID)
					figures++
				default:
					t.Errorf("section %d points at %q", i+1, ptr)
				}
			}
			want := []string{fmt.Sprintf("Heading P%d", p), fmt.Sprintf("Line %d.1 naïve 👩‍👩‍👧 text", p),
				fmt.Sprintf("Line %d.2 plain", p)}
			if p%3 == 0 {
				want = append(want, fmt.Sprintf("table:A%d", p))
			}
			if p%4 == 0 {
				want = append(want, fmt.Sprintf("figure:%d.1", p))
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("page %d section elements resolve to %q, want %q", p, got, want)
			}
		}
		if tables != len(r.Tables) || figures != len(r.Figures) {
			t.Errorf("sections reach %d of %d tables and %d of %d figures", tables, len(r.Tables),
				figures, len(r.Figures))
		}
		for _, tb := range r.Tables {
			for _, c := range tb.Cells {
				if _, j := resolve(c.Elements[0]); len(c.Elements) != 1 || r.Paragraphs[j].Content != c.Content {
					t.Errorf("cell %q elements %v", c.Content, c.Elements)
				}
			}
		}
		for _, f := range r.Figures {
			_, j := resolve(f.Elements[0])
			if want := "Figure " + strings.TrimSuffix(f.ID, ".1"); r.Paragraphs[j].Content != want {
				t.Errorf("figure %s caption %q, want %q", f.ID, r.Paragraphs[j].Content, want)
			}
		}
	}
}

func TestDIRolesAndRegions(t *testing.T) {
	pages := []int{3, 4, 5}
	for _, title := range []bool{false, true} {
		r := decodeDI(t, docgen.DI(pages, docgen.Local, docgen.Options{TitleOnFirstPage: title}))
		var roles []string
		for _, para := range r.Paragraphs {
			if para.Role != "" {
				roles = append(roles, para.Role)
			}
			for _, br := range para.BoundingRegions {
				if br.PageNumber < 1 || br.PageNumber > len(pages) || len(br.Polygon) != 8 {
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
			o := docgen.Options{Unit: textunit.UTF16CodeUnit, TitleOnFirstPage: true}
			if a, b := docgen.DI(pages, n, o), docgen.DI(pages, n, o); !bytes.Equal(a, b) {
				t.Errorf("pages %v: two calls differ", pages)
			}
			if a, b := docgen.Read(pages, n), docgen.Read(pages, n); !bytes.Equal(a, b) {
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
		for _, u := range units {
			o := docgen.Options{Unit: u}
			local, orig := docgen.DI(pages, docgen.Local, o), docgen.DI(pages, docgen.Original, o)
			if bytes.Equal(local, orig) {
				t.Fatalf("pages %v: docgen.Local and docgen.Original are identical", pages)
			}
			if got := renumber(t, local, pages, "pageNumber"); !bytes.Equal(got, orig) {
				t.Errorf("pages %v %v: renumbered docgen.Local differs from docgen.Original:\n%s\n%s", pages, u, got, orig)
			}
		}
		local, orig := docgen.Read(pages, docgen.Local), docgen.Read(pages, docgen.Original)
		if bytes.Equal(local, orig) {
			t.Fatalf("Read pages %v: docgen.Local and docgen.Original are identical", pages)
		}
		if got := renumber(t, local, pages, "page"); !bytes.Equal(got, orig) {
			t.Errorf("Read pages %v: renumbered docgen.Local differs from docgen.Original:\n%s\n%s", pages, got, orig)
		}
	}
	// Pages 1..k are numbered the same either way.
	if !bytes.Equal(docgen.DI(pageRange(1, 5), docgen.Local, docgen.Options{}), docgen.DI(pageRange(1, 5), docgen.Original, docgen.Options{})) {
		t.Error("DI pages 1-5: docgen.Local differs from docgen.Original")
	}
}

func TestRead(t *testing.T) {
	type rword struct {
		BoundingBox []int   `json:"boundingBox"`
		Text        string  `json:"text"`
		Confidence  float64 `json:"confidence"`
	}
	type rline struct {
		BoundingBox []int   `json:"boundingBox"`
		Text        string  `json:"text"`
		Words       []rword `json:"words"`
	}
	type readResult struct {
		Page   int     `json:"page"`
		Angle  float64 `json:"angle"`
		Width  int     `json:"width"`
		Height int     `json:"height"`
		Unit   string  `json:"unit"`
		Lines  []rline `json:"lines"`
	}
	var r struct {
		Version      string       `json:"version"`
		ModelVersion string       `json:"modelVersion"`
		ReadResults  []readResult `json:"readResults"`
	}
	pages := []int{2, 7, 8}
	for _, n := range []docgen.Numbering{docgen.Original, docgen.Local} {
		b := docgen.Read(pages, n)
		var compact bytes.Buffer
		if err := json.Compact(&compact, b); err != nil || !bytes.Equal(compact.Bytes(), b) {
			t.Fatalf("not valid compact JSON: %v\n%s", err, b)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		ms, _ := jsonx.Members(bytes.NewReader(b), 0, int64(len(b)))
		if len(ms) != 3 || ms[0].Name != "version" || ms[1].Name != "modelVersion" || ms[2].Name != "readResults" {
			t.Errorf("members %+v", ms)
		}
		if r.Version != "3.2.0" || r.ModelVersion != "2022-04-30" || len(r.ReadResults) != len(pages) {
			t.Fatalf("result %+v", r)
		}
		for i, rr := range r.ReadResults {
			p := pages[i]
			wantPage := p
			if n == docgen.Local {
				wantPage = i + 1
			}
			if rr.Page != wantPage || rr.Width != 600+p || rr.Height != 800 || rr.Unit != "pixel" {
				t.Errorf("read result %d: %+v", i, rr)
			}
			want := fmt.Sprintf("Read line %d", p)
			if len(rr.Lines) != 1 || rr.Lines[0].Text != want || len(rr.Lines[0].BoundingBox) != 8 {
				t.Fatalf("lines %+v", rr.Lines)
			}
			var words []string
			for _, w := range rr.Lines[0].Words {
				words = append(words, w.Text)
				if len(w.BoundingBox) != 8 || w.Confidence != 0.99 {
					t.Errorf("word %+v", w)
				}
			}
			if strings.Join(words, " ") != want {
				t.Errorf("words %q, want %q", words, want)
			}
		}
	}
}
