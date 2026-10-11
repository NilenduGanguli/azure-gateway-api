package merge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
)

// A section runs from its heading to the next heading at its level or above, so a cut that falls
// inside a section leaves the next part starting with text that has no heading of its own. The
// service, seeing that part alone, cannot know which section the text belongs to and gives it
// one of its own; analysing the whole document, it would have kept the text in the section the
// previous page left open. The merge restores that: a part's leading content without a heading is
// a continuation, and it is folded into the section open at the end of the parts before it — the
// deepest last section of their tree.
//
// A continuation comes in one of two shapes:
//
//   - the part's root starts with section 1, a section of elements only (no sections inside it)
//     whose first element is not a heading paragraph: section 1 is the continuation, and it
//     disappears into the open section;
//   - the part has no section but its root, and the root's first element is not a heading
//     paragraph: everything the root holds is the continuation, as a part with no heading at all
//     is, and the root contributes no elements or spans of its own.
//
// py_layout_multipage_table_span_pdf is the evidence: its second section, a heading on page 1 and
// a table that continues onto page 2, is one section with one span over both pages, while each
// page analysed alone would end or start a section at the page break.
//
// A heading is a paragraph whose role is title or sectionHeading. Anything else — a part whose
// leading content has a heading, a leading section with sections inside it, no section left open
// because the previous parts hold their content in the root directly, an open section without
// spans or elements to extend — is left as the parts give it.

// continuation is a part's leading content that continues the section open before the part.
type continuation struct {
	p        *part
	elements json.RawMessage // a JSON array of element pointers, in p's numbering
	spans    []span          // in p's offsets
}

// sectionAt names a section of a part by its index in the part's own sections array (≥ 1).
type sectionAt struct{ k, i int }

// headingCollector records which of a part's paragraphs are headings, as its paragraphs are walked.
type headingCollector struct {
	heads map[int]bool
}

func (c *headingCollector) visit(path []jsonx.PathElem, kind jsonx.Kind, raw []byte) ([]byte, bool) {
	if len(path) == 3 && path[1].IsIndex && isKey(path[2], "role") && kind == jsonx.String &&
		(string(raw) == `"title"` || string(raw) == `"sectionHeading"`) {
		if c.heads == nil {
			c.heads = map[int]bool{}
		}
		c.heads[path[1].Index] = true
	}
	return nil, false
}

// isHeading reports whether an element pointer of part p names one of its heading paragraphs.
func (p *part) isHeading(ptr string) bool {
	n, ok := strings.CutPrefix(ptr, "/paragraphs/")
	if !ok {
		return false
	}
	i, ok := parseIndex([]byte(n))
	return ok && p.heads[i]
}

// object is a JSON object's members in order, with their raw values.
type object struct {
	keys   []string
	values []json.RawMessage
}

func (o *object) get(key string) (json.RawMessage, bool) {
	for i, k := range o.keys {
		if k == key {
			return o.values[i], true
		}
	}
	return nil, false
}

// readObject lists the members of the JSON object raw, refusing a name written with an escape
// (writeName could not copy it) or given twice.
func readObject(k int, raw json.RawMessage, what string) (*object, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, checkf("part %d: %s is not an object", k, what)
	}
	members, err := jsonx.Members(bytes.NewReader(raw), 0, int64(len(raw)))
	if err != nil {
		return nil, jsonErr(k, what, err)
	}
	o := &object{}
	for _, m := range members {
		if m.Escaped {
			return nil, checkf("part %d: %s member name %q is written with an escape", k, what, m.Name)
		}
		if _, dup := o.get(m.Name); dup {
			return nil, checkf("part %d: %s has %q twice", k, what, m.Name)
		}
		o.keys = append(o.keys, m.Name)
		o.values = append(o.values, raw[m.Start:m.End])
	}
	return o, nil
}

// readSections decodes a part's sections array into its items' raw values. A part's sections are
// a few pages' worth of pointers, so holding one part's at a time costs little.
func readSections(p *part) ([]json.RawMessage, error) {
	mb, _, _ := p.member("sections")
	var secs []json.RawMessage
	err := withFile(p, func(f *os.File) error {
		dec := json.NewDecoder(io.NewSectionReader(f, mb.Start, mb.End-mb.Start))
		tok, err := dec.Token()
		if err != nil {
			return decodeErr(p.k, "sections", err)
		}
		if tok != json.Delim('[') {
			return checkf("part %d: sections is not an array", p.k)
		}
		for dec.More() {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return decodeErr(p.k, "sections", err)
			}
			secs = append(secs, raw)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(secs) != p.counts["sections"] {
		return nil, checkf("part %d: %d sections read, %d counted", p.k, len(secs), p.counts["sections"])
	}
	return secs, nil
}

// pointers decodes an elements array.
func pointers(k int, raw json.RawMessage, what string) ([]string, error) {
	var ptrs []string
	if err := json.Unmarshal(raw, &ptrs); err != nil {
		return nil, checkf("part %d: %s elements are not an array of pointers", k, what)
	}
	return ptrs, nil
}

// sectionIndex parses "/sections/N".
func sectionIndex(ptr string) (int, bool) {
	n, ok := strings.CutPrefix(ptr, "/sections/")
	if !ok {
		return 0, false
	}
	return parseIndex([]byte(n))
}

// planContinuations decides, part by part, which parts' leading content continues the section
// open before them, and which section that is. It runs before anything is written, once every
// root is read, and holds only the continuations themselves.
func (m *merger) planContinuations() error {
	var open *sectionAt // the section the next part's leading content would continue
	for j, p := range m.with {
		r := m.roots[j]
		secs, err := readSections(p)
		if err != nil {
			return err
		}
		var rootElems []string
		if v, ok := r.get("elements"); ok {
			if rootElems, err = pointers(p.k, v, "root section"); err != nil {
				return err
			}
		}
		if open != nil && p.k > 0 && len(rootElems) > 0 && !p.isHeading(rootElems[0]) {
			c, err := m.leadingContinuation(p, r, secs, rootElems)
			if err != nil {
				return err
			}
			if c != nil {
				m.folds[*open] = append(m.folds[*open], *c)
			}
		}
		if p.rootMoved {
			continue // the open section stays open
		}
		next, err := m.lastOpenSection(p, secs, rootElems)
		if err != nil {
			return err
		}
		if next != nil && next.i == 1 && p.secDrop == 1 {
			continue // the part ends in its continuation, which went into the open section
		}
		if next != nil || len(rootElems) > p.secDrop {
			open = next
		}
	}
	return nil
}

// leadingContinuation returns part p's continuation, or nil if its leading content is not one.
// It marks the part accordingly: section 1 dropped, or its root's content moved.
func (m *merger) leadingContinuation(p *part, r *root, secs []json.RawMessage, rootElems []string) (*continuation, error) {
	if len(secs) == 1 {
		// No section but the root: the root's content continues the open section, unless it
		// holds a section pointer, which a part with one section cannot resolve anyway.
		for _, e := range rootElems {
			if _, ok := sectionIndex(e); ok {
				return nil, nil
			}
		}
		v, _ := r.get("elements")
		p.rootMoved = true
		return &continuation{p: p, elements: v, spans: r.spans}, nil
	}
	if i, ok := sectionIndex(rootElems[0]); !ok || i != 1 {
		return nil, nil
	}
	s1, err := readObject(p.k, secs[1], "section 1")
	if err != nil {
		return nil, err
	}
	ev, hasElems := s1.get("elements")
	sv, hasSpans := s1.get("spans")
	if !hasElems || !hasSpans {
		return nil, nil
	}
	elems, err := pointers(p.k, ev, "section 1")
	if err != nil {
		return nil, err
	}
	if len(elems) == 0 || p.isHeading(elems[0]) {
		return nil, nil
	}
	for _, e := range elems {
		if _, ok := sectionIndex(e); ok {
			return nil, nil
		}
	}
	spans, err := readSpans(p.k, sv)
	if err != nil {
		return nil, err
	}
	if err := m.spansWithin(p, spans, "section 1"); err != nil {
		return nil, err
	}
	p.secDrop = 1
	return &continuation{p: p, elements: ev, spans: spans}, nil
}

// lastOpenSection follows part p's tree from its root through each last element that is a
// section, and returns the section it ends at, or nil when the root's own last element is not a
// section. A section the merge cannot extend — without spans or elements — closes nothing and
// opens nothing: nil.
func (m *merger) lastOpenSection(p *part, secs []json.RawMessage, rootElems []string) (*sectionAt, error) {
	elems := rootElems
	at := 0
	for range secs { // a well-formed tree is no deeper than it has sections
		if len(elems) == 0 {
			break
		}
		i, ok := sectionIndex(elems[len(elems)-1])
		if !ok || i < 1 || i >= len(secs) || i == at {
			break
		}
		s, err := readObject(p.k, secs[i], "section "+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		ev, hasElems := s.get("elements")
		if _, hasSpans := s.get("spans"); !hasElems || !hasSpans {
			return nil, nil
		}
		if elems, err = pointers(p.k, ev, "section "+strconv.Itoa(i)); err != nil {
			return nil, err
		}
		at = i
	}
	if at == 0 {
		return nil, nil
	}
	return &sectionAt{p.k, at}, nil
}

// spansWithin checks that spans the merge will shift lie within the part's content.
func (m *merger) spansWithin(p *part, spans []span, what string) error {
	for _, s := range spans {
		if s.offset+s.length > p.length {
			return checkf("part %d: %s's spans reach %d %v, past the content's %d: "+
				"the service counts %v differently from the gateway",
				p.k, what, s.offset+s.length, m.unit, p.length, m.unit)
		}
	}
	return nil
}

// appendSpans appends part p's spans, shifted into merged offsets, to out. Its first span is
// joined to the last one in out when only the page separator lies between them: that span ends
// where the previous part's content does, and this one starts where p's does.
func (m *merger) appendSpans(out []span, p *part, spans []span) []span {
	for _, s := range spans {
		s.offset += p.char
		if n := len(out); n > 0 && p.k > 0 && s.offset == p.char {
			prev := m.parts[p.k-1]
			if last := &out[n-1]; last.offset+last.length == prev.char+prev.length {
				last.length = s.offset + s.length - last.offset
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// writeSection writes one of part p's sections after the root, renumbered, and extended with the
// continuations folded into it.
func (m *merger) writeSection(bw *bufio.Writer, p *part, i int, raw json.RawMessage, index int) error {
	prefix := []jsonx.PathElem{{Key: "sections"}, {IsIndex: true, Index: index}}
	folds := m.folds[sectionAt{p.k, i}]
	if len(folds) == 0 {
		if err := jsonx.Rewrite(bw, bytes.NewReader(raw), prefix, p.visit); err != nil {
			return jsonErr(p.k, "sections", err)
		}
		return *p.verr
	}
	s, err := readObject(p.k, raw, "section "+strconv.Itoa(i))
	if err != nil {
		return err
	}
	bw.WriteByte('{')
	for n, key := range s.keys {
		if n > 0 {
			bw.WriteByte(',')
		}
		writeName(bw, key)
		bw.WriteByte(':')
		v := s.values[n]
		switch key {
		case "spans":
			own, err := readSpans(p.k, v)
			if err != nil {
				return err
			}
			if err := m.spansWithin(p, own, "section "+strconv.Itoa(i)); err != nil {
				return err
			}
			out := m.appendSpans(nil, p, own)
			for _, c := range folds {
				out = m.appendSpans(out, c.p, c.spans)
			}
			writeSpans(bw, out)
		case "elements":
			err = m.writeElements(bw, append([]continuation{{p: p, elements: v}}, folds...),
				append(prefix, jsonx.PathElem{Key: "elements"}))
		default:
			if err = jsonx.Rewrite(bw, bytes.NewReader(v), append(prefix, jsonx.PathElem{Key: key}),
				p.visit); err != nil {
				err = jsonErr(p.k, "section "+key, err)
			} else {
				err = *p.verr
			}
		}
		if err != nil {
			return err
		}
	}
	bw.WriteByte('}')
	return nil
}

// writeElements writes one elements array made of several parts' element arrays, each renumbered
// by its own part.
func (m *merger) writeElements(bw *bufio.Writer, lists []continuation, prefix []jsonx.PathElem) error {
	bw.WriteByte('[')
	total := 0
	for _, l := range lists {
		n, err := jsonx.CountItems(bytes.NewReader(l.elements), 0, int64(len(l.elements)))
		if err != nil {
			return jsonErr(l.p.k, "section elements", err)
		}
		if n == 0 {
			continue
		}
		if total > 0 {
			bw.WriteByte(',')
		}
		if _, err := m.items(bw, l.p, bytes.NewReader(l.elements), prefix, total); err != nil {
			return err
		}
		total += n
	}
	bw.WriteByte(']')
	return nil
}

// withoutFirst is a JSON array without its first item.
func withoutFirst(k int, raw json.RawMessage) (json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, checkf("part %d: root section elements are not an array", k)
	}
	if len(items) == 0 {
		return raw, nil
	}
	out := []byte{'['}
	for i, it := range items[1:] {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, it...)
	}
	return append(out, ']'), nil
}
