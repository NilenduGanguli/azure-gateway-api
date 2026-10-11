package merge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
)

// Sections are the one array the merge cannot simply concatenate. Every part's sections[0] is the
// root of that part's section tree, and a document has one root. So the merged root is built from
// the parts' roots — their elements in order, and their spans shifted into the merged content —
// and the parts' other sections follow it in part order, renumbered so that part k's section
// i ≥ 1 becomes secBase[k] + i − 1.
//
// A root's spans cover the text its sections hold, not the whole part: the service leaves page
// headers, footers and page numbers out of every section, and a blank page's empty text too, so a
// real root often stops short of its part's end or comes in several spans
// (py_layout_multipage_transform's root is [{0,301},{303,319}] around its blank second page).
// The merged root therefore keeps every root's own spans, shifted, and joins two of them only
// where nothing but the page separator lies between: the earlier root runs to the end of its
// part, the next part's root starts at its beginning, and the two parts are adjacent. That is
// what the service does within a document — where only a separator lies between, its spans run
// on (py_layout_multipage_table_span_pdf's root is one span over both pages) — and it never
// covers furniture the service left out.
//
// A part whose leading content has no heading continues the section the parts before it left
// open, and is folded into that section rather than given one of its own (continue.go).
//
// A part without sections contributes none, which is only right when it has no content either: a
// run of blank pages. If any part has sections, a part with content but none would leave its
// paragraphs in no section, and that is a check failure.
//
// The merged root's spans are written by the merge, not copied, so every root's spans are decoded
// strictly (readSpans) and must lie within its part's content (checkRootSpans).

// root is a part's root section: its members in order, with their raw values.
type root struct {
	keys   []string
	values []json.RawMessage
	spans  []span // its spans member, decoded; none if it has no spans member
	rest   int64  // where the item after the root starts within the file, or -1 if there is none
}

// span is one span of a root section, in the part's own offsets.
type span struct{ offset, length int }

func (r *root) get(key string) (json.RawMessage, bool) {
	for i, k := range r.keys {
		if k == key {
			return r.values[i], true
		}
	}
	return nil, false
}

// readRoot decodes the first item of a sections array, keeping member order and raw values.
// The root is a short object — a span and one pointer per top-level element of a few pages — so
// decoding it costs little, unlike the arrays around it.
//
// Its members are listed by jsonx, as the result's are, so that a name written with an escape is
// refused here too: the merged root writes names as they were read (writeName).
func readRoot(k int, f io.ReaderAt, mb jsonx.Member) (*root, error) {
	dec := json.NewDecoder(io.NewSectionReader(f, mb.Start, mb.End-mb.Start))
	tok, err := dec.Token()
	if err != nil {
		return nil, decodeErr(k, "sections", err)
	}
	if tok != json.Delim('[') {
		return nil, checkf("part %d: sections is not an array", k)
	}
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, decodeErr(k, "root section", err)
	}
	if raw[0] != '{' {
		return nil, checkf("part %d: the root section is not an object", k)
	}
	members, err := jsonx.Members(bytes.NewReader(raw), 0, int64(len(raw)))
	if err != nil {
		return nil, jsonErr(k, "root section", err)
	}
	r := &root{rest: -1}
	for _, m := range members {
		if m.Escaped {
			return nil, checkf("part %d: root section member name %q is written with an escape",
				k, m.Name)
		}
		if _, dup := r.get(m.Name); dup {
			return nil, checkf("part %d: the root section has %q twice", k, m.Name)
		}
		r.keys = append(r.keys, m.Name)
		r.values = append(r.values, raw[m.Start:m.End])
	}
	// The decoder's offset is just past the root. What follows, after any whitespace, is the
	// comma that introduces section 1 or the array's closing bracket. It is read directly rather
	// than through the decoder, which cannot report a read failure while peeking ahead.
	c, at, err := nextByte(k, f, mb.Start+dec.InputOffset(), mb.End)
	if err != nil {
		return nil, err
	}
	switch c {
	case ',':
		r.rest = at + 1
	case ']':
	default:
		return nil, checkf("part %d: no comma after the root section", k)
	}
	return r, nil
}

// readSpans decodes a root section's spans strictly: an array of objects, each with the members
// "offset" and "length" exactly once, spelt so, and nothing else, each a canonical non-negative
// integer. encoding/json alone would match member names in any case, ignore unknown members and
// let a repeated one win, and a root read that loosely could be merged into a span the service
// never wrote.
func readSpans(k int, v json.RawMessage) ([]span, error) {
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	// v is one well-formed value, already decoded once, so a token error here is malformed input.
	token := func() (json.Token, error) {
		tok, err := dec.Token()
		if err != nil {
			return nil, checkf("part %d: root section spans: %v", k, err)
		}
		return tok, nil
	}
	tok, err := token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('[') {
		return nil, checkf("part %d: root section spans are not an array", k)
	}
	var spans []span
	for i := 0; dec.More(); i++ {
		if tok, err = token(); err != nil {
			return nil, err
		}
		if tok != json.Delim('{') {
			return nil, checkf("part %d: root section span %d is not an object", k, i)
		}
		var s span
		var seen [2]bool // offset, length
		for dec.More() {
			if tok, err = token(); err != nil {
				return nil, err
			}
			name, _ := tok.(string)
			var at int
			switch name {
			case "offset":
				at = 0
			case "length":
				at = 1
			default:
				return nil, checkf(
					`part %d: root section span %d has %q, not only "offset" and "length"`, k, i, name)
			}
			if seen[at] {
				return nil, checkf("part %d: root section span %d has %q twice", k, i, name)
			}
			seen[at] = true
			if tok, err = token(); err != nil {
				return nil, err
			}
			n, ok := tok.(json.Number)
			x, canonical := parseIndex([]byte(n))
			if !ok || !canonical {
				return nil, checkf(
					"part %d: root section span %d: %s %s is not a non-negative integer",
					k, i, name, shownToken(tok))
			}
			if at == 0 {
				s.offset = x
			} else {
				s.length = x
			}
		}
		for at, name := range []string{"offset", "length"} {
			if !seen[at] {
				return nil, checkf("part %d: root section span %d has no %q", k, i, name)
			}
		}
		if _, err = token(); err != nil { // the span's closing brace
			return nil, err
		}
		spans = append(spans, s)
	}
	return spans, nil
}

// shownToken is a span member's value as a check failure quotes it, in its JSON spelling where
// the decoder kept one.
func shownToken(tok json.Token) string {
	switch v := tok.(type) {
	case json.Number:
		return string(v)
	case string:
		return strconv.Quote(v)
	case json.Delim:
		if v == '[' {
			return "an array"
		}
		return "an object"
	case nil:
		return "null"
	}
	return fmt.Sprint(tok)
}

// decodeErr classifies an error from encoding/json as jsonErr does one from jsonx: input that is
// not well-formed JSON is a check failure, a failure to read it is not.
func decodeErr(k int, what string, err error) error {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typ) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: part %d: %s: %w", ErrCheck, k, what, err)
	}
	return fmt.Errorf("merge: part %d: read %s: %w", k, what, err)
}

// nextByte returns the first byte in f[off:end] that is not JSON whitespace, and its offset.
func nextByte(k int, f io.ReaderAt, off, end int64) (byte, int64, error) {
	var buf [64]byte
	for off < end {
		b := buf[:min(int64(len(buf)), end-off)]
		if err := readAt(f, b, off); err != nil {
			return 0, 0, fmt.Errorf("merge: part %d: read sections: %w", k, err)
		}
		for i, c := range b {
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				return c, off + int64(i), nil
			}
		}
		off += int64(len(b))
	}
	return 0, 0, checkf("part %d: sections ends after the root section", k)
}

// checkSections refuses parts whose sections cannot make the document's: a part with content but
// no sections while another part has them. A part with no content has nothing a section could
// hold, and may have none.
//
// It then reads every part's root section, which the merged root is built from, and checks its
// spans (checkRootSpans). Roots are small, and reading them all before anything is written lets
// a bad one fail the merge while the output is still empty.
func (m *merger) checkSections() error {
	var with *part
	for _, p := range m.parts {
		if p.counts["sections"] > 0 {
			with = p
			break
		}
	}
	if with == nil {
		return nil
	}
	for _, p := range m.parts {
		if p.counts["sections"] == 0 && p.length > 0 {
			return checkf("part %d has content but no sections, while part %d has them", p.k, with.k)
		}
	}
	for _, p := range m.parts {
		if p.counts["sections"] == 0 {
			continue
		}
		mb, _, _ := p.member("sections")
		var r *root
		err := withFile(p, func(f *os.File) error {
			var err error
			r, err = readRoot(p.k, f, mb)
			return err
		})
		if err != nil {
			return err
		}
		if err := m.checkRootSpans(p, r); err != nil {
			return err
		}
		m.with = append(m.with, p)
		m.roots = append(m.roots, r)
	}
	return nil
}

// checkRootSpans decodes a part's root spans and checks them against the part's content: every
// span must lie within it, and a part with content must have one, since its root's spans are what
// the merged root's are built from. Reaching past the content is the one way a root can show that
// the service counts the unit differently from the gateway; the pages' spans are the check for
// that (measureContent), since a root rarely covers the whole part.
func (m *merger) checkRootSpans(p *part, r *root) error {
	if v, ok := r.get("spans"); ok {
		var err error
		if r.spans, err = readSpans(p.k, v); err != nil {
			return err
		}
	}
	if len(r.spans) == 0 {
		if p.length > 0 {
			return checkf("part %d has content but its root section has no span", p.k)
		}
		return nil
	}
	if _, end := r.extent(); end > p.length {
		return checkf("part %d: the root section's spans reach %d %v, past the content's %d: "+
			"the service counts %v differently from the gateway",
			p.k, end, m.unit, p.length, m.unit)
	}
	return nil
}

// extent is where a root's spans start and end, in its part's offsets. It needs a span.
func (r *root) extent() (start, end int) {
	start, end = r.spans[0].offset, r.spans[0].offset+r.spans[0].length
	for _, s := range r.spans[1:] {
		start, end = min(start, s.offset), max(end, s.offset+s.length)
	}
	return start, end
}

func (m *merger) writeSections(bw *bufio.Writer) error {
	if len(m.with) == 0 {
		bw.WriteString("[]")
		m.stats.Counts["sections"] = 0
		return nil
	}

	bw.WriteByte('[')
	if err := m.writeRoot(bw); err != nil {
		return err
	}
	total := 1
	for _, p := range m.with {
		if p.counts["sections"] == 1 {
			continue
		}
		secs, err := readSections(p)
		if err != nil {
			return err
		}
		for i := 1 + p.secDrop; i < len(secs); i++ {
			bw.WriteByte(',')
			if err := m.writeSection(bw, p, i, secs[i], total); err != nil {
				return err
			}
			total++
		}
	}
	bw.WriteByte(']')
	m.stats.Counts["sections"] = total
	return nil
}

// writeRoot writes the merged root: members in an order that keeps every root's own, as the
// result's members are ordered (mergeOrder); elements from every root in order; every root's
// spans, shifted (writeRootSpans); anything else from the first root that has it.
func (m *merger) writeRoot(bw *bufio.Writer) error {
	lists := make([][]string, len(m.roots))
	for j, r := range m.roots {
		lists[j] = r.keys
	}
	keys, ok := mergeOrder(lists)
	if !ok {
		return checkf("the parts' root sections give their members in conflicting orders")
	}
	bw.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			bw.WriteByte(',')
		}
		writeName(bw, key)
		bw.WriteByte(':')
		var err error
		switch key {
		case "elements":
			err = m.writeRootElements(bw)
		case "spans":
			m.writeRootSpans(bw)
		default:
			for j, r := range m.roots {
				if v, ok := r.get(key); ok {
					p := m.with[j]
					prefix := []jsonx.PathElem{{Key: "sections"}, {IsIndex: true}, {Key: key}}
					if err = jsonx.Rewrite(bw, bytes.NewReader(v), prefix, p.visit); err != nil {
						err = jsonErr(p.k, "root section "+key, err)
					} else {
						err = *p.verr
					}
					break
				}
			}
		}
		if err != nil {
			return err
		}
	}
	bw.WriteByte('}')
	return nil
}

// writeRootElements writes the merged root's elements: every root's in part order, less what
// continues an earlier part's section.
func (m *merger) writeRootElements(bw *bufio.Writer) error {
	var lists []continuation
	for j, r := range m.roots {
		p := m.with[j]
		v, ok := r.get("elements")
		if !ok || p.rootMoved {
			continue
		}
		if p.rootDrop > 0 {
			var err error
			if _, v, err = splitArray(p.k, v, p.rootDrop); err != nil {
				return err
			}
		}
		lists = append(lists, continuation{p: p, elements: v})
	}
	return m.writeElements(bw, lists,
		[]jsonx.PathElem{{Key: "sections"}, {IsIndex: true}, {Key: "elements"}})
}

// writeRootSpans writes the merged root's spans: every root's in part order, shifted into merged
// offsets, a part's first span joined to the span before it where only the page separator lies
// between them (appendSpans). A root whose content continues an earlier part's section gives its
// spans to that section instead.
func (m *merger) writeRootSpans(bw *bufio.Writer) {
	var out []span
	for j, r := range m.roots {
		if p := m.with[j]; !p.rootMoved {
			out = m.appendSpans(out, p, r.spans)
		}
	}
	writeSpans(bw, out)
}

// writeSpans writes spans compactly, as the service does.
func writeSpans(bw *bufio.Writer, spans []span) {
	bw.WriteByte('[')
	for i, s := range spans {
		if i > 0 {
			bw.WriteByte(',')
		}
		bw.WriteString(`{"offset":`)
		bw.WriteString(strconv.Itoa(s.offset))
		bw.WriteString(`,"length":`)
		bw.WriteString(strconv.Itoa(s.length))
		bw.WriteByte('}')
	}
	bw.WriteByte(']')
}
