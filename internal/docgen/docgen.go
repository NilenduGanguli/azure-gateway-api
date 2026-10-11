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
// The bodies are shaped as the real service writes them, checked against the official spec and
// real recorded outputs (test/azure-reference, internal/azschema): members in the service's
// order (pages, tables, paragraphs, styles, contentFormat, sections, figures; a paragraph's spans
// before its role and content), styles always present, figures left out when there are none, and
// a figure's caption as its caption member, outside its elements. Options add what real
// documents have and the merge must not trip over: page headers, footers and page numbers that no
// section holds, blank pages, captions and selection marks. Realistic turns them all on.
//
// Each page holds every kind of reference the merge renumbers: word, line, page, paragraph,
// table, cell, figure, caption, selection mark and section spans; pageNumbers in pages and
// bounding regions; figure ids; and /paragraphs, /tables, /figures and /sections pointers. Its
// text mixes a precomposed letter and a ZWJ emoji sequence, which count differently in each
// stringIndexType, so a merge that counts in the wrong unit shifts offsets wrongly.
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
	// Furniture gives pages the paragraphs the service keeps out of every section: a pageHeader
	// at the top of even pages, a pageFooter at the foot of odd ones, and a pageNumber last on
	// every page. The root section's spans then stop short of them, as real roots do.
	Furniture bool
	// BlankEvery makes every page whose number is a multiple of it blank, as a scanned empty page
	// is: no words, no lines, one empty span, and no section. Zero makes none.
	BlankEvery int
	// Captions gives every table a caption after it and every figure one before it, each its own
	// paragraph and the item's caption member, as the service writes them.
	Captions bool
	// SelectionMarks puts a checked box at the start of a paragraph on every fifth page.
	SelectionMarks bool
	// Legacy answers in the shape of api-version 2023-07-31, the /formrecognizer family: no
	// contentFormat, sections or figures, and no elements or captions on tables. The paragraphs
	// stay; that API has them too.
	Legacy bool
}

// LegacyAPIVersion is the /formrecognizer family's API version, which Options.Legacy answers.
const LegacyAPIVersion = "2023-07-31"

// Realistic is every option real documents need: furniture, a blank seventh page, captions and
// selection marks.
func Realistic(u textunit.Unit) Options {
	return Options{Unit: u, Furniture: true, BlankEvery: 7, Captions: true, SelectionMarks: true}
}

// The paragraph kinds on a page, in the order they appear.
const (
	kindHeader = iota // pageHeader
	kindHeading
	kindBody
	kindMark // a paragraph that starts with a selection mark
	kindCell
	kindTableCaption
	kindFigureCaption
	kindFigureText
	kindFooter     // pageFooter
	kindPageNumber // pageNumber
)

// furniture reports whether a kind is one no section holds.
func furniture(kind int) bool {
	return kind == kindHeader || kind == kindFooter || kind == kindPageNumber
}

// markText is the text a selection mark occupies in the content; the line after it does not
// include it.
const markText = ":selected:"

type span struct{ off, n int }

func (s span) end() int { return s.off + s.n }

type wordModel struct {
	text string
	span span
}

type paraModel struct {
	kind  int
	role  string
	text  string
	span  span
	line  string // the line's text: the paragraph's, less any selection mark
	lspan span
	words []wordModel
	at    int // its line's position on the page, for geometry
	page  int // index into model.pages
}

type pageModel struct {
	orig, num int // original page number, and the number this result gives it
	blank     bool
	span      span
	paras     []int // indexes into model.paras
	table     int   // index into tables, or -1
	figure    int   // index into figures, or -1
	section   int   // index into sections, or -1 for a blank page
}

// model is a DI result before it is written: every offset and index already settled.
type model struct {
	content string
	pages   []pageModel
	paras   []paraModel
	tables  []int // index into pages of each table
	figures []int // index into pages of each figure
	nsec    int   // sections, the root included
}

func (o Options) blank(p int) bool { return o.BlankEvery > 0 && p%o.BlankEvery == 0 }

// pageParas lists a page's paragraphs: kind, role and text.
func pageParas(p int, title bool, o Options) []paraModel {
	add := func(ps []paraModel, kind int, role, text string) []paraModel {
		return append(ps, paraModel{kind: kind, role: role, text: text})
	}
	var ps []paraModel
	if o.Furniture && p%2 == 0 {
		ps = add(ps, kindHeader, "pageHeader", "Contoso quarterly report")
	}
	heading := "sectionHeading"
	if title {
		heading = "title"
	}
	ps = add(ps, kindHeading, heading, "Heading P"+itoa(p))
	ps = add(ps, kindBody, "", "Line "+itoa(p)+".1 naïve 👩‍👩‍👧 text")
	ps = add(ps, kindBody, "", "Line "+itoa(p)+".2 plain")
	if o.SelectionMarks && p%5 == 0 {
		ps = add(ps, kindMark, "", markText+" Agree "+itoa(p))
	}
	if p%3 == 0 {
		ps = add(ps, kindCell, "", "A"+itoa(p))
		ps = add(ps, kindCell, "", "B"+itoa(p))
		if o.Captions {
			ps = add(ps, kindTableCaption, "", "Table "+itoa(p)+": totals")
		}
	}
	if p%4 == 0 {
		if o.Captions {
			ps = add(ps, kindFigureCaption, "", "Figure "+itoa(p)+": trend")
		}
		ps = add(ps, kindFigureText, "", "Chart "+itoa(p))
	}
	if o.Furniture {
		if p%2 == 1 {
			ps = add(ps, kindFooter, "pageFooter", "Confidential")
		}
		ps = add(ps, kindPageNumber, "pageNumber", "Page "+itoa(p))
	}
	return ps
}

func build(pages []int, n Numbering, o Options) model {
	var m model
	var content strings.Builder
	unit := o.Unit
	sep := textunit.Len("\n", unit)
	pos := 0
	title := o.TitleOnFirstPage
	m.nsec = 1
	m.pages = make([]pageModel, len(pages))
	for i, p := range pages {
		pg := &m.pages[i]
		pg.orig, pg.num, pg.table, pg.figure, pg.section = p, p, -1, -1, -1
		if n == Local {
			pg.num = i + 1
		}
		if i > 0 {
			content.WriteByte('\n')
			pos += sep
		}
		pg.span.off = pos
		if o.blank(p) {
			pg.blank = true
			continue
		}
		pg.section = m.nsec
		m.nsec++
		for j, pr := range pageParas(p, title, o) {
			if j > 0 {
				content.WriteByte('\n')
				pos += sep
			}
			pr.at, pr.page = j, i
			pr.span = span{pos, textunit.Len(pr.text, unit)}
			pr.line, pr.lspan = pr.text, pr.span
			if pr.kind == kindMark {
				skip := textunit.Len(markText+" ", unit)
				pr.line = pr.text[len(markText)+1:]
				pr.lspan = span{pos + skip, pr.span.n - skip}
			}
			// A word's offset is the length of the line before it, measured rather than summed,
			// so it stays exact even where a token would join a grapheme cluster with its space.
			at := 0
			for _, tok := range strings.Split(pr.line, " ") {
				pr.words = append(pr.words, wordModel{tok,
					span{pr.lspan.off + textunit.Len(pr.line[:at], unit), textunit.Len(tok, unit)}})
				at += len(tok) + 1
			}
			switch pr.kind {
			case kindCell:
				if pg.table < 0 {
					pg.table = len(m.tables)
					m.tables = append(m.tables, i)
				}
			case kindFigureText:
				pg.figure = len(m.figures)
				m.figures = append(m.figures, i)
			}
			pos += pr.span.n
			content.WriteString(pr.text)
			pg.paras = append(pg.paras, len(m.paras))
			m.paras = append(m.paras, pr)
		}
		title = false
		pg.span.n = pos - pg.span.off
	}
	m.content = content.String()
	return m
}

// find returns the index of the page's first paragraph of a kind, or -1.
func (m *model) find(pg *pageModel, kind int) int {
	for _, i := range pg.paras {
		if m.paras[i].kind == kind {
			return i
		}
	}
	return -1
}

// runs joins the spans of the paragraphs a section holds into the spans the service writes for
// it: one run while only a separator lies between two paragraphs, a new one past anything else
// (furniture, a blank page).
func (m *model) runs(paras []int, sep int) []span {
	var out []span
	for _, i := range paras {
		s := m.paras[i].span
		if n := len(out); n > 0 && out[n-1].end()+sep == s.off {
			out[n-1].n = s.end() - out[n-1].off
			continue
		}
		out = append(out, s)
	}
	return out
}

// sectionElements lists what a page's section holds, in reading order: its heading and body
// paragraphs, the table and its caption, the figure's caption and the figure. Cells and a figure's
// own text belong to the table and the figure.
func (m *model) sectionElements(pg *pageModel) []string {
	var out []string
	for _, i := range pg.paras {
		switch m.paras[i].kind {
		case kindHeading, kindBody, kindMark, kindTableCaption, kindFigureCaption:
			out = append(out, pointer("paragraphs", i))
		case kindCell:
			if t := pointer("tables", pg.table); len(out) == 0 || out[len(out)-1] != t {
				out = append(out, t)
			}
		case kindFigureText:
			out = append(out, pointer("figures", pg.figure))
		}
	}
	return out
}

// DI returns the bare AnalyzeResult JSON for the given original pages analysed together.
func DI(pages []int, n Numbering, o Options) []byte {
	m := build(pages, n, o)
	sep := textunit.Len("\n", o.Unit)
	var b out
	b.raw(`{"apiVersion":`)
	if o.Legacy {
		b.str(LegacyAPIVersion)
	} else {
		b.str("2024-11-30")
	}
	b.raw(`,"modelId":"prebuilt-layout","stringIndexType":`)
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
		for _, pi := range pg.paras {
			pr := &m.paras[pi]
			for k, w := range pr.words {
				b.comma(nw)
				nw++
				b.raw(`{"content":`)
				b.str(w.text)
				b.raw(`,"polygon":`)
				b.polygon(wordBox(pg.orig, pr.at, k))
				b.raw(`,"confidence":`)
				b.float(float64(900+(pg.orig*7+pr.at*3+k*11)%100) / 1000)
				b.raw(`,"span":`)
				b.span(w.span)
				b.raw(`}`)
			}
		}
		b.raw(`]`)
		if mi := m.find(pg, kindMark); mi >= 0 {
			pr := &m.paras[mi]
			b.raw(`,"selectionMarks":[{"state":"selected","polygon":`)
			b.polygon(markBox(pg.orig, pr.at))
			b.raw(`,"confidence":0.991,"span":`)
			b.span(span{pr.span.off, textunit.Len(markText, o.Unit)})
			b.raw(`}]`)
		}
		b.raw(`,"lines":[`)
		for j, pi := range pg.paras {
			pr := &m.paras[pi]
			b.comma(j)
			b.raw(`{"content":`)
			b.str(pr.line)
			b.raw(`,"polygon":`)
			b.polygon(lineBox(pg.orig, pr.at))
			b.raw(`,"spans":[`)
			b.span(pr.lspan)
			b.raw(`]}`)
		}
		b.raw(`],"spans":[`)
		b.span(pg.span)
		b.raw(`]}`)
	}

	b.raw(`],"tables":[`)
	for t, pi := range m.tables {
		pg := &m.pages[pi]
		first := m.find(pg, kindCell)
		b.comma(t)
		b.raw(`{"rowCount":1,"columnCount":2,"cells":[`)
		for c := 0; c < 2; c++ {
			pr := &m.paras[first+c]
			b.comma(c)
			b.raw(`{"rowIndex":0,"columnIndex":`)
			b.int(c)
			b.raw(`,"content":`)
			b.str(pr.text)
			b.raw(`,"boundingRegions":`)
			b.regions(pg.num, lineBox(pg.orig, pr.at))
			b.raw(`,"spans":[`)
			b.span(pr.span)
			b.raw(`]`)
			if !o.Legacy {
				b.raw(`,"elements":[`)
				b.pointer("paragraphs", first+c)
				b.raw(`]`)
			}
			b.raw(`}`)
		}
		a, c := &m.paras[first], &m.paras[first+1]
		b.raw(`],"boundingRegions":`)
		b.regions(pg.num, union(lineBox(pg.orig, a.at), lineBox(pg.orig, c.at)))
		b.raw(`,"spans":[`)
		b.span(span{a.span.off, c.span.end() - a.span.off})
		b.raw(`]`)
		if ci := m.find(pg, kindTableCaption); ci >= 0 && !o.Legacy {
			b.raw(`,"caption":`)
			b.caption(&m, pg, ci)
		}
		b.raw(`}`)
	}

	b.raw(`],"paragraphs":[`)
	for i := range m.paras {
		pr := &m.paras[i]
		pg := &m.pages[pr.page]
		b.comma(i)
		b.raw(`{"spans":[`)
		b.span(pr.span)
		b.raw(`],"boundingRegions":`)
		b.regions(pg.num, lineBox(pg.orig, pr.at))
		if pr.role != "" {
			b.raw(`,"role":`)
			b.str(pr.role)
		}
		b.raw(`,"content":`)
		b.str(pr.text)
		b.raw(`}`)
	}

	b.raw(`],"styles":[]`)
	if o.Legacy {
		b.raw(`}`)
		return b.b
	}
	b.raw(`,"contentFormat":"text","sections":[`)
	if m.nsec > 1 {
		var covered []int
		for i := range m.pages {
			for _, pi := range m.pages[i].paras {
				if !furniture(m.paras[pi].kind) {
					covered = append(covered, pi)
				}
			}
		}
		b.raw(`{"spans":[`)
		for i, s := range m.runs(covered, sep) {
			b.comma(i)
			b.span(s)
		}
		b.raw(`],"elements":[`)
		ne := 0
		for i := range m.pages {
			if pg := &m.pages[i]; !pg.blank {
				b.comma(ne)
				ne++
				b.pointer("sections", pg.section)
			}
		}
		b.raw(`]}`)
		for i := range m.pages {
			pg := &m.pages[i]
			if pg.blank {
				continue
			}
			var own []int
			for _, pi := range pg.paras {
				if !furniture(m.paras[pi].kind) {
					own = append(own, pi)
				}
			}
			b.raw(`,{"spans":[`)
			for j, s := range m.runs(own, sep) {
				b.comma(j)
				b.span(s)
			}
			b.raw(`],"elements":[`)
			for j, e := range m.sectionElements(pg) {
				b.comma(j)
				b.raw(`"` + e + `"`)
			}
			b.raw(`]}`)
		}
	}
	b.raw(`]`)

	if len(m.figures) > 0 {
		b.raw(`,"figures":[`)
		for f, pi := range m.figures {
			pg := &m.pages[pi]
			text := m.find(pg, kindFigureText)
			b.comma(f)
			b.raw(`{"id":`)
			b.str(itoa(pg.num) + ".1")
			b.raw(`,"boundingRegions":`)
			b.regions(pg.num, figureBox(pg.orig))
			b.raw(`,"spans":[`)
			b.span(m.paras[text].span)
			b.raw(`],"elements":[`)
			b.pointer("paragraphs", text)
			b.raw(`]`)
			if ci := m.find(pg, kindFigureCaption); ci >= 0 {
				b.raw(`,"caption":`)
				b.caption(&m, pg, ci)
			}
			b.raw(`}`)
		}
		b.raw(`]`)
	}
	b.raw(`}`)
	return b.b
}

// caption writes a table's or a figure's caption member.
func (o *out) caption(m *model, pg *pageModel, para int) {
	pr := &m.paras[para]
	o.raw(`{"content":`)
	o.str(pr.text)
	o.raw(`,"boundingRegions":`)
	o.regions(pg.num, lineBox(pg.orig, pr.at))
	o.raw(`,"spans":[`)
	o.span(pr.span)
	o.raw(`],"elements":[`)
	o.pointer("paragraphs", para)
	o.raw(`]}`)
}

// ReadOptions shapes a Read result.
type ReadOptions struct {
	// Image answers as for an image: pages in pixels, with integer coordinates. The default is a
	// PDF's answer, in inches with up to four decimals, as the service measures PDFs.
	Image bool
}

// Read returns the bare Read v3.2 analyzeResult JSON for the given pages, as the 2022-04-30
// model writes it: every line with its appearance.
func Read(pages []int, n Numbering, o ReadOptions) []byte {
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
		if o.Image {
			b.raw(`,"angle":0,"width":`)
			b.int(600 + p)
			b.raw(`,"height":800,"unit":"pixel","lines":[`)
		} else {
			b.raw(`,"angle":0,"width":8.5,"height":11,"unit":"inch","lines":[`)
		}
		for j, text := range []string{"Read line " + itoa(p), "Total " + itoa(p*7)} {
			b.comma(j)
			b.raw(`{"boundingBox":`)
			b.readBox(o.Image, readLineBox(p, j))
			b.raw(`,"text":`)
			b.str(text)
			b.raw(`,"appearance":{"style":{"name":"other","confidence":`)
			b.float(float64(800+(p*13+j*7)%200) / 1000)
			b.raw(`}},"words":[`)
			for k, w := range strings.Split(text, " ") {
				b.comma(k)
				b.raw(`{"boundingBox":`)
				b.readBox(o.Image, readWordBox(p, j, k))
				b.raw(`,"text":`)
				b.str(w)
				b.raw(`,"confidence":`)
				b.float(float64(950+(p*3+j*5+k*11)%50) / 1000)
				b.raw(`}`)
			}
			b.raw(`]}`)
		}
		b.raw(`]}`)
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

func markBox(p, line int) box {
	l := lineBox(p, line)
	return box{l.x0 - 30, l.y0, l.x0 - 10, l.y1}
}

func figureBox(p int) box { return box{150 + p%7, 600, 650 + p%7, 900} }

func union(a, b box) box {
	return box{min(a.x0, b.x0), min(a.y0, b.y0), max(a.x1, b.x1), max(a.y1, b.y1)}
}

// Read geometry, in ten-thousandths of an inch for a PDF; an image's pixels are a twentieth of
// that, rounded down.
func readLineBox(p, line int) box {
	x, y := 10000+137*(p%10), 15000+4000*line+91*(p%7)
	return box{x, y, x + 40000, y + 2104}
}

func readWordBox(p, line, word int) box {
	l := readLineBox(p, line)
	x := l.x0 + 12500*word
	return box{x, l.y0, x + 11000, l.y1}
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

func pointer(member string, i int) string { return "/" + member + "/" + itoa(i) }

func (o *out) pointer(member string, i int) {
	o.raw(`"` + pointer(member, i) + `"`)
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

// readBox writes Read's eight coordinates: inches for a PDF, integer pixels for an image.
func (o *out) readBox(image bool, bx box) {
	o.b = append(o.b, '[')
	for i, v := range [8]int{bx.x0, bx.y0, bx.x1, bx.y0, bx.x1, bx.y1, bx.x0, bx.y1} {
		o.comma(i)
		if image {
			o.int(v / 20)
		} else {
			o.float(float64(v) / 10000)
		}
	}
	o.b = append(o.b, ']')
}
