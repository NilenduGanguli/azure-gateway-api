// Package docgen generates deterministic Document Intelligence and Read results for any set of
// pages, as if those pages had been analysed together.
//
// It models what a split-and-merge has to get right: a result for pages 4–6 must be what the
// service would return for pages 4–6 of the whole document, with offsets counted from the start
// of that result's own content, pages numbered 1–3 when the part was physically cut out (Local)
// or 4–6 when the original was analysed with pages= (Original), and every cross-reference numbered
// within the result. So merging the parts of a document must reproduce the whole document byte
// for byte, which tests the merge mechanics exactly. It says nothing about how faithful the merge
// is to the real service on document-level judgements; the probe measures that.
//
// Each page holds every kind of reference the merge renumbers: word, line, page, paragraph,
// table, cell, figure and section spans; pageNumbers in pages and bounding regions; figure ids;
// and /paragraphs, /tables, /figures and /sections pointers. Its text mixes a precomposed letter
// and a ZWJ emoji sequence, which count differently in each stringIndexType, so a merge that
// counts in the wrong unit shifts offsets wrongly.
//
// The JSON is written by hand with strconv rather than encoding/json, so the bytes are fixed by
// this file alone and stay compact.
package docgen

import (
	"strconv"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// Numbering selects how the result numbers its pages.
type Numbering int

const (
	Original Numbering = iota // whole document, or a pages= request on the original
	Local                     // a physically split part: pages numbered 1..k
)

// Options shapes a DI result.
type Options struct {
	Unit textunit.Unit
	// TitleOnFirstPage gives the first analysed page's heading role "title" instead of
	// "sectionHeading", imitating a document-level judgement so compare-split has a difference
	// to report.
	TitleOnFirstPage bool
}

// The line kinds on a page, in the order they appear.
const (
	kindHeading = iota
	kindBody
	kindCell
	kindCaption
)

type span struct{ off, n int }

type wordModel struct {
	text string
	span span
}

type lineModel struct {
	kind  int
	text  string
	span  span
	words []wordModel
	para  int // index of the paragraph this line is
}

type pageModel struct {
	orig, num int // original page number, and the number this result gives it
	span      span
	lines     []lineModel
	table     int // index into tables, or -1
	figure    int // index into figures, or -1
	section   int // index into sections
}

// model is a DI result before it is written: every offset and index already settled.
type model struct {
	content    string
	total      int // content length in the unit
	pages      []pageModel
	paragraphs []*lineModel // every line is a paragraph
	tables     []int        // index into pages of each table
	figures    []int        // index into pages of each figure
}

func build(pages []int, n Numbering, unit textunit.Unit) model {
	var m model
	var content strings.Builder
	sep := textunit.Len("\n", unit)
	pos := 0
	m.pages = make([]pageModel, len(pages))
	for i, p := range pages {
		pg := &m.pages[i]
		pg.orig, pg.num, pg.table, pg.figure, pg.section = p, p, -1, -1, i+1
		if n == Local {
			pg.num = i + 1
		}
		texts := []string{"Heading P" + itoa(p), "Line " + itoa(p) + ".1 naïve 👩‍👩‍👧 text",
			"Line " + itoa(p) + ".2 plain"}
		kinds := []int{kindHeading, kindBody, kindBody}
		if p%3 == 0 {
			texts = append(texts, "A"+itoa(p), "B"+itoa(p))
			kinds = append(kinds, kindCell, kindCell)
			pg.table = len(m.tables)
			m.tables = append(m.tables, i)
		}
		if p%4 == 0 {
			texts = append(texts, "Figure "+itoa(p))
			kinds = append(kinds, kindCaption)
			pg.figure = len(m.figures)
			m.figures = append(m.figures, i)
		}

		if i > 0 {
			content.WriteByte('\n')
			pos += sep
		}
		pg.span.off = pos
		pg.lines = make([]lineModel, len(texts))
		for j, text := range texts {
			if j > 0 {
				content.WriteByte('\n')
				pos += sep
			}
			ln := &pg.lines[j]
			ln.kind, ln.text, ln.para = kinds[j], text, len(m.paragraphs)
			ln.span = span{pos, textunit.Len(text, unit)}
			// A word's offset is the length of the line before it, measured rather than summed,
			// so it stays exact even where a token would join a grapheme cluster with its space.
			at := 0
			for _, tok := range strings.Split(text, " ") {
				ln.words = append(ln.words, wordModel{tok,
					span{pos + textunit.Len(text[:at], unit), textunit.Len(tok, unit)}})
				at += len(tok) + 1
			}
			pos += ln.span.n
			content.WriteString(text)
			m.paragraphs = append(m.paragraphs, ln)
		}
		pg.span.n = pos - pg.span.off
	}
	m.content = content.String()
	m.total = pos
	return m
}

// DI returns the bare AnalyzeResult JSON for the given original pages analysed together.
func DI(pages []int, n Numbering, o Options) []byte {
	m := build(pages, n, o.Unit)
	var b out
	b.raw(`{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":`)
	b.str(o.Unit.String())
	b.raw(`,"content":`)
	b.str(m.content)

	b.raw(`,"pages":[`)
	for i := range m.pages {
		pg := &m.pages[i]
		b.comma(i)
		b.raw(`{"pageNumber":`)
		b.int(pg.num)
		b.raw(`,"angle":0,"width":8.5,"height":11,"unit":"inch","words":[`)
		nw := 0
		for j := range pg.lines {
			for k, w := range pg.lines[j].words {
				b.comma(nw)
				nw++
				b.raw(`{"content":`)
				b.str(w.text)
				b.raw(`,"polygon":`)
				b.polygon(wordBox(pg.orig, j, k))
				b.raw(`,"confidence":`)
				b.float(float64(900+(pg.orig*7+j*3+k*11)%100) / 1000)
				b.raw(`,"span":`)
				b.span(w.span)
				b.raw(`}`)
			}
		}
		b.raw(`],"lines":[`)
		for j, ln := range pg.lines {
			b.comma(j)
			b.raw(`{"content":`)
			b.str(ln.text)
			b.raw(`,"polygon":`)
			b.polygon(lineBox(pg.orig, j))
			b.raw(`,"spans":[`)
			b.span(ln.span)
			b.raw(`]}`)
		}
		b.raw(`],"spans":[`)
		b.span(pg.span)
		b.raw(`]}`)
	}

	b.raw(`],"paragraphs":[`)
	np := 0
	for i := range m.pages {
		pg := &m.pages[i]
		for j, ln := range pg.lines {
			b.comma(np)
			np++
			b.raw(`{`)
			if ln.kind == kindHeading {
				role := "sectionHeading"
				if o.TitleOnFirstPage && i == 0 {
					role = "title"
				}
				b.raw(`"role":`)
				b.str(role)
				b.raw(`,`)
			}
			b.raw(`"content":`)
			b.str(ln.text)
			b.raw(`,"boundingRegions":`)
			b.regions(pg.num, lineBox(pg.orig, j))
			b.raw(`,"spans":[`)
			b.span(ln.span)
			b.raw(`]}`)
		}
	}

	b.raw(`],"tables":[`)
	for t, pi := range m.tables {
		pg := &m.pages[pi]
		b.comma(t)
		b.raw(`{"rowCount":1,"columnCount":2,"cells":[`)
		first := -1
		for j, ln := range pg.lines {
			if ln.kind != kindCell {
				continue
			}
			if first < 0 {
				first = j
			} else {
				b.raw(`,`)
			}
			b.raw(`{"rowIndex":0,"columnIndex":`)
			b.int(j - first)
			b.raw(`,"content":`)
			b.str(ln.text)
			b.raw(`,"boundingRegions":`)
			b.regions(pg.num, lineBox(pg.orig, j))
			b.raw(`,"spans":[`)
			b.span(ln.span)
			b.raw(`],"elements":[`)
			b.pointer("paragraphs", ln.para)
			b.raw(`]}`)
		}
		a, c := pg.lines[first], pg.lines[first+1]
		b.raw(`],"boundingRegions":`)
		b.regions(pg.num, union(lineBox(pg.orig, first), lineBox(pg.orig, first+1)))
		b.raw(`,"spans":[`)
		b.span(span{a.span.off, c.span.off + c.span.n - a.span.off})
		b.raw(`]}`)
	}

	b.raw(`],"figures":[`)
	for f, pi := range m.figures {
		pg := &m.pages[pi]
		caption := pg.lines[len(pg.lines)-1] // the caption is the page's last line
		b.comma(f)
		b.raw(`{"id":`)
		b.str(itoa(pg.num) + ".1")
		b.raw(`,"boundingRegions":`)
		b.regions(pg.num, figureBox(pg.orig))
		b.raw(`,"spans":[`)
		b.span(caption.span)
		b.raw(`],"elements":[`)
		b.pointer("paragraphs", caption.para)
		b.raw(`]}`)
	}

	b.raw(`],"sections":[{"spans":[`)
	b.span(span{0, m.total})
	b.raw(`],"elements":[`)
	for i := range m.pages {
		b.comma(i)
		b.pointer("sections", m.pages[i].section)
	}
	b.raw(`]}`)
	for i := range m.pages {
		pg := &m.pages[i]
		b.raw(`,{"spans":[`)
		b.span(pg.span)
		b.raw(`],"elements":[`)
		ne := 0
		for _, ln := range pg.lines {
			if ln.kind == kindHeading || ln.kind == kindBody {
				b.comma(ne)
				ne++
				b.pointer("paragraphs", ln.para)
			}
		}
		if pg.table >= 0 {
			b.raw(`,`)
			b.pointer("tables", pg.table)
		}
		if pg.figure >= 0 {
			b.raw(`,`)
			b.pointer("figures", pg.figure)
		}
		b.raw(`]}`)
	}
	b.raw(`],"contentFormat":"text"}`)
	return b.b
}

// Read returns the bare Read v3.2 analyzeResult JSON for the given pages.
func Read(pages []int, n Numbering) []byte {
	var b out
	b.raw(`{"version":"3.2.0","modelVersion":"2022-04-30","readResults":[`)
	for i, p := range pages {
		num := p
		if n == Local {
			num = i + 1
		}
		b.comma(i)
		b.raw(`{"page":`)
		b.int(num)
		b.raw(`,"angle":0,"width":`)
		b.int(600 + p)
		b.raw(`,"height":800,"unit":"pixel","lines":[{"boundingBox":`)
		b.box(10+p%10, 20+p%10, 360+p%10, 50+p%10)
		b.raw(`,"text":`)
		b.str("Read line " + itoa(p))
		b.raw(`,"words":[`)
		for k, w := range []string{"Read", "line", itoa(p)} {
			b.comma(k)
			x := 10 + 120*k + p%10
			b.raw(`{"boundingBox":`)
			b.box(x, 20+p%10, x+100, 50+p%10)
			b.raw(`,"text":`)
			b.str(w)
			b.raw(`,"confidence":0.99}`)
		}
		b.raw(`]}]}`)
	}
	b.raw(`]}`)
	return b.b
}

// Geometry, in hundredths of an inch so that every coordinate prints as a short decimal. It
// depends only on the original page and the position on it, never on numbering, so the same
// page has the same geometry in every result.

type box struct{ x0, y0, x1, y1 int }

func lineBox(p, line int) box {
	x, y := 100+p%7, 100+30*line+p%5
	return box{x, y, x + 600, y + 20}
}

func wordBox(p, line, word int) box {
	l := lineBox(p, line)
	x := l.x0 + 110*word
	return box{x, l.y0, x + 100, l.y1}
}

func figureBox(p int) box { return box{150 + p%7, 400, 650 + p%7, 700} }

func union(a, b box) box {
	return box{min(a.x0, b.x0), min(a.y0, b.y0), max(a.x1, b.x1), max(a.y1, b.y1)}
}

func itoa(v int) string { return strconv.Itoa(v) }

// out appends compact JSON.
type out struct{ b []byte }

func (o *out) raw(s string) { o.b = append(o.b, s...) }
func (o *out) int(v int)    { o.b = strconv.AppendInt(o.b, int64(v), 10) }

func (o *out) float(v float64) { o.b = strconv.AppendFloat(o.b, v, 'f', -1, 64) }

// comma separates array items: nothing before the first.
func (o *out) comma(i int) {
	if i > 0 {
		o.b = append(o.b, ',')
	}
}

// str writes s as a JSON string, escaping only what JSON requires.
func (o *out) str(s string) {
	o.b = append(o.b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			o.b = append(o.b, '\\', c)
		case c == '\n':
			o.b = append(o.b, '\\', 'n')
		case c < 0x20:
			o.b = append(o.b, '\\', 'u', '0', '0', "0123456789abcdef"[c>>4], "0123456789abcdef"[c&0xf])
		default:
			o.b = append(o.b, c)
		}
	}
	o.b = append(o.b, '"')
}

func (o *out) span(s span) {
	o.raw(`{"offset":`)
	o.int(s.off)
	o.raw(`,"length":`)
	o.int(s.n)
	o.raw(`}`)
}

func (o *out) pointer(member string, i int) {
	o.raw(`"/` + member + `/`)
	o.int(i)
	o.raw(`"`)
}

// polygon writes a box as DI's eight clockwise coordinates in inches.
func (o *out) polygon(bx box) {
	o.b = append(o.b, '[')
	for i, v := range [8]int{bx.x0, bx.y0, bx.x1, bx.y0, bx.x1, bx.y1, bx.x0, bx.y1} {
		o.comma(i)
		o.float(float64(v) / 100)
	}
	o.b = append(o.b, ']')
}

func (o *out) regions(pageNumber int, bx box) {
	o.raw(`[{"pageNumber":`)
	o.int(pageNumber)
	o.raw(`,"polygon":`)
	o.polygon(bx)
	o.raw(`}]`)
}

// box writes Read's eight integer pixel coordinates.
func (o *out) box(x0, y0, x1, y1 int) {
	o.b = append(o.b, '[')
	for i, v := range [8]int{x0, y0, x1, y0, x1, y1, x0, y1} {
		o.comma(i)
		o.int(v)
	}
	o.b = append(o.b, ']')
}
