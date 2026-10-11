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
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
)

// Sections are the one array the merge cannot simply concatenate. Every part's sections[0] is a
// root covering the whole of that part, and a document has one root. So the merged root is built
// from the parts' roots — their elements in order, and one span from part 0's root start to the
// last root's end — and the parts' other sections follow it in part order, renumbered so that
// part k's section i ≥ 1 becomes secBase[k] + i − 1.
//
// A part without sections contributes none, which is only right when it has no content either.
// If part 0 has none while a later part does, the whole document's root could not be told from
// the parts; if a later part has content but no sections while part 0 has them, its paragraphs
// would be in no section and the root's span would not cover them. Both are check failures.
//
// The merged root's span is written by the merge, not copied, so every root's spans are decoded
// strictly (readSpans) and must fit its part's content. A root that is one span over the whole
// part is also the service's own measure of that part's content, and it must equal the gateway's
// (checkRootSpans): there the merge sees how the service counts the unit, before it shifts every
// offset by its own count.

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

// checkSections refuses parts whose sections cannot make the document's: part 0 without sections
// while a later part has them, or a later part with content but no sections while part 0 has
// them. A part with no content has nothing a section could hold, and may have none.
//
// It then reads every part's root section, which the merged root is built from, and checks its
// spans (checkRootSpans). Roots are small, and reading them all before anything is written lets
// a bad one fail the merge while the output is still empty.
func (m *merger) checkSections() error {
	if m.parts[0].counts["sections"] == 0 {
		for _, p := range m.parts[1:] {
			if p.counts["sections"] > 0 {
				return checkf("part 0 has no sections but part %d does", p.k)
			}
		}
		return nil
	}
	for _, p := range m.parts[1:] {
		if p.counts["sections"] == 0 && p.length > 0 {
			return checkf("part %d has content but no sections, while part 0 has them", p.k)
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
	// m.with[0] is part 0: the first check let no part have sections unless part 0 has them.
	if len(m.roots[0].spans) == 0 {
		for j, r := range m.roots[1:] {
			if len(r.spans) > 0 {
				return checkf("part 0's root section has no span to start the merged root's, "+
					"but part %d's has", m.with[j+1].k)
			}
		}
	}
	return nil
}

// checkRootSpans decodes a part's root spans and checks them against the part's content.
//
// Every span must lie within the content. A part with content must have a span, since its root is
// what the merged root's span is built from. And a single span from offset 0 is the service's
// length of the content in the unit, which must be the gateway's: the service and the gateway
// segment text with different Unicode versions (uniseg implements 15.0; a .NET runtime may
// implement 15.1, whose rule GB9c joins Devanagari conjuncts into one grapheme cluster), and
// where they disagree every offset the merge shifts past that part would be wrong.
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
	if s := r.spans[0]; len(r.spans) == 1 && s.offset == 0 && s.length != p.length {
		return checkf("part %d: the root section spans %d %v, but the content is %d long: "+
			"the service counts %v differently from the gateway",
			p.k, s.length, m.unit, p.length, m.unit)
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
	prefix := []jsonx.PathElem{{Key: "sections"}}
	for i, p := range m.with {
		want := p.counts["sections"] - 1
		if want == 0 {
			continue
		}
		if m.roots[i].rest < 0 {
			return checkf("part %d: %d sections counted, but none after the root", p.k, want+1)
		}
		mb, _, _ := p.member("sections")
		bw.WriteByte(',')
		var n int
		err := withFile(p, func(f *os.File) error {
			// The sections after the root, read as an array of their own: an opening bracket,
			// then the original bytes from section 1 to the closing bracket.
			rest := io.MultiReader(strings.NewReader("["),
				io.NewSectionReader(f, m.roots[i].rest, mb.End-m.roots[i].rest))
			var err error
			n, err = m.items(bw, p, rest, prefix, p.secBase)
			return err
		})
		if err != nil {
			return err
		}
		if n != want {
			return checkf("part %d: %d sections after the root, %d counted", p.k, n, want)
		}
		total += n
	}
	bw.WriteByte(']')
	m.stats.Counts["sections"] = total
	return nil
}

// writeRoot writes the merged root: members in an order that keeps every root's own, as the
// result's members are ordered (mergeOrder); elements from every root in order; one span over all
// of them; anything else from the first root that has it.
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

func (m *merger) writeRootElements(bw *bufio.Writer) error {
	prefix := []jsonx.PathElem{{Key: "sections"}, {IsIndex: true}, {Key: "elements"}}
	bw.WriteByte('[')
	total := 0
	for j, r := range m.roots {
		v, ok := r.get("elements")
		if !ok {
			continue
		}
		p := m.with[j]
		n, err := jsonx.CountItems(bytes.NewReader(v), 0, int64(len(v)))
		if err != nil {
			return jsonErr(p.k, "root section elements", err)
		}
		if n == 0 {
			continue
		}
		if total > 0 {
			bw.WriteByte(',')
		}
		if _, err := m.items(bw, p, bytes.NewReader(v), prefix, total); err != nil {
			return err
		}
		total += n
	}
	bw.WriteByte(']')
	return nil
}

// writeRootSpans writes the merged root's single span: from where part 0's root starts to where
// the last root with a span ends, in merged offsets. checkSections made sure that part 0's root
// has a span if any root does, and that every span lies within its part's content, so the end is
// never before the start.
func (m *merger) writeRootSpans(bw *bufio.Writer) {
	last := -1
	for j, r := range m.roots {
		if len(r.spans) > 0 {
			last = j
		}
	}
	if last < 0 {
		bw.WriteString("[]")
		return
	}
	start, _ := m.roots[0].extent() // part 0's offsets are the merged ones
	_, end := m.roots[last].extent()
	end += m.with[last].char
	bw.WriteString(`[{"offset":`)
	bw.WriteString(strconv.Itoa(start))
	bw.WriteString(`,"length":`)
	bw.WriteString(strconv.Itoa(end - start))
	bw.WriteString(`}]`)
}
