package merge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
)

// A section runs from its heading to the next heading at its level or above, so a cut that falls
// inside a section leaves the next part starting with text that has no heading of its own. The
// service, seeing that part alone, cannot know which section the text belongs to and gives it
// one of its own; analysing the whole document, it would have kept the text in the section the
// previous page left open. The merge restores that: a part's leading content without a heading is
// a continuation, and it is folded into the sections open at the end of the parts before it:
// content into the deepest last section of their tree, an untitled wrapper into the open section
// at its own depth.
//
// The sections left open are a path down that tree: the root's last element when it is a section,
// that section's last element when it is one, and so on to the deepest. In the whole document the
// sections on the path above the one a continuation goes into hold it too, as the section they
// hold, and a real section's spans are its elements' extent, sub-sections included
// (py_layout_figures's section 2 spans its sections 3 and 4, thirdparty_docler_markdown_2page's
// section 2 its section 3). So a continuation's spans are appended to every section on the path
// above it as well, and the root keeps every part's own spans whatever becomes of its elements
// (py_layout_multipage_table_span_pdf's root is one span over both pages).
//
// A continuation comes in one of three shapes:
//
//   - the part's root starts with section 1, and section 1 has no heading: its first element is
//     not a heading paragraph. Section 1 is the continuation, and it disappears into the deepest
//     open section, which takes its elements and spans. A sub-section pointer among those
//     elements comes along, renumbered, and the sub-section becomes the open section's, as the
//     whole document nests it: thirdparty_docler_markdown_2page's page 2 opens with two untitled
//     paragraphs and a section, all three held by the section page 1 leaves open.
//     Section 1 may instead open with section 2, and section 2 with section 3: untitled sections
//     that wrap the ones below them, the shape a part has when a nested section crosses the cut.
//     Such a wrapper continues the open section at its own depth — section 1 the root's last
//     section, section 2 that one's — and takes its sub-section pointers there, less the one to
//     the next section of the chain, which disappears with it. The chain ends at a section that
//     opens with content, which continues the deepest open section, or at one that opens with a
//     heading, which stays a section of its own inside the last wrapper's;
//   - the part's root starts with elements that are neither sections nor heading paragraphs and
//     goes on to a section: that run, up to the first section or heading, is the continuation,
//     and it moves into the deepest open section. Its spans are the root's, clipped to where the
//     run's own elements start and end, as every real section's spans are its elements' extent;
//   - the part has no section but its root, and the root's first element is not a heading
//     paragraph: everything the root holds is the continuation, as a part with no heading at all
//     is, and it moves into the deepest open section; the root contributes no elements, and its
//     spans, which are the content's, stay the merged root's too.
//
// py_layout_multipage_table_span_pdf is the evidence: its second section, a heading on page 1 and
// a table that continues onto page 2, is one section with one span over both pages, while each
// page analysed alone would end or start a section at the page break. The first two shapes are
// one: the service wraps untitled content that opens a document before a heading in a section 1
// of its own (java_layout, js_layout_selection_marks), and no real root holds an untitled element
// before a section, nor any element after one; were a part to come back unwrapped, leaving the
// run in the root would give the merged root content after a section, which no real root has.
// A heading paragraph ends the run: the service keeps a title in the root itself
// (py_layout_figures's root opens with its title), so it stays there with what follows it.
//
// Content without a heading can only continue the deepest open section, since a section holds its
// own elements before its sub-sections. Where a heading in the part would have nested in the
// whole document — under the section left open, or beside it — the part alone cannot say: the
// service gives no heading levels. The merge keeps the part's own nesting, relative to the
// sections its untitled ones continue, which is the whole document's wherever the part's tree is
// the whole document's cut at the page.
//
// A heading is a paragraph whose role is title or sectionHeading. Anything else — a part whose
// leading content has a heading, a chain of untitled sections deeper than the path left open, no
// section left open because the previous parts hold their content in the root directly, an open
// section without spans or elements to extend — is left as the parts give it.

// continuation is content of a part that continues a section open before the part.
type continuation struct {
	p        *part
	elements json.RawMessage // a JSON array of element pointers, in p's numbering; nil for spans only
	spans    []span          // in p's offsets
}

// lead is how a part's leading content continues the path of sections left open before it.
type lead struct {
	// conts[j] is what the open section at depth j+1 takes: elements and spans where the content
	// or a wrapper is folded into it, spans alone where it only holds the section that takes them.
	// A zero continuation (nil p) takes nothing.
	conts []continuation
	// into[i-1] is the depth of the open section the part's section i was folded into.
	into []int
}

// content folds content without a heading into the deepest open section, and its spans into the
// sections from depth from+1 down to it.
func (l *lead) content(c continuation, from int) {
	d := len(l.conts)
	for j := from; j < d-1; j++ {
		l.conts[j] = continuation{p: c.p, spans: c.spans}
	}
	l.conts[d-1] = c
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

// planContinuations decides, part by part, which parts' leading content continues the sections
// left open before them, and which sections those are. It runs before anything is written, once
// every root is read, and holds only the continuations themselves.
func (m *merger) planContinuations() error {
	var open []sectionAt // the path of sections left open, the root's own first; empty for none
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
		var l *lead
		if len(open) > 0 && p.k > 0 && len(rootElems) > 0 && !p.isHeading(rootElems[0]) {
			if l, err = m.leadingContinuation(p, r, secs, rootElems, len(open)); err != nil {
				return err
			}
		}
		if l != nil {
			for d, at := range open {
				if c := l.conts[d]; c.p != nil {
					m.folds[at] = append(m.folds[at], c)
				}
			}
		}
		if p.rootMoved {
			continue // the open sections stay open
		}
		path, err := m.openPath(p, secs, rootElems)
		if err != nil {
			return err
		}
		switch {
		case len(path) > 0:
			open = reopen(p, open, l, path)
		case len(rootElems) > p.rootDrop:
			open = nil // the part ends in its root, which closes every section
		}
	}
	return nil
}

// reopen is the path left open after part p, from the sections path its tree ends in: a section
// of p's folded into an open section stands for that section and the ones above it.
func reopen(p *part, open []sectionAt, l *lead, path []int) []sectionAt {
	var next []sectionAt
	for _, i := range path {
		if l != nil && i <= len(l.into) {
			next = slices.Clone(open[:l.into[i-1]])
			continue
		}
		next = append(next, sectionAt{p.k, i})
	}
	return next
}

// leadingContinuation returns how part p's leading content continues the depth sections left
// open before it, or nil if it is no continuation. It marks the part accordingly: its leading
// sections dropped with the root's pointer to the first, the root's leading run dropped, or all
// of its root's content moved.
func (m *merger) leadingContinuation(p *part, r *root, secs []json.RawMessage, rootElems []string, depth int) (*lead, error) {
	l := &lead{conts: make([]continuation, depth)}
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
		l.content(continuation{p: p, elements: v, spans: r.spans}, 0)
		return l, nil
	}
	i, ok := sectionIndex(rootElems[0])
	if !ok {
		c, err := m.leadingRun(p, r, rootElems)
		if c == nil || err != nil {
			return nil, err
		}
		l.content(*c, 0)
		return l, nil
	}
	if i != 1 {
		return nil, nil
	}
	return m.leadingSections(p, secs, l)
}

// chainLink is one of the untitled sections a part opens with.
type chainLink struct {
	elements json.RawMessage
	spans    []span
	wrapper  bool // it opens with the next section of the chain
}

// leadingSections fills l with the chain of untitled sections part p opens with, from section 1:
// wrappers, each opening with the next, then a section that opens with content. It is no
// continuation when section 1 has a heading, when the chain is deeper than the open path, or when
// a section on it cannot be read as one — without spans or elements, or wrapping a section that
// is not the next in the part's numbering, as every real section's first sub-section is.
func (m *merger) leadingSections(p *part, secs []json.RawMessage, l *lead) (*lead, error) {
	depth := len(l.conts)
	var chain []chainLink
	for i := 1; ; i++ {
		if i >= len(secs) {
			return nil, nil
		}
		what := "section " + strconv.Itoa(i)
		s, err := readObject(p.k, secs[i], what)
		if err != nil {
			return nil, err
		}
		ev, hasElems := s.get("elements")
		sv, hasSpans := s.get("spans")
		if !hasElems || !hasSpans {
			return nil, nil
		}
		elems, err := pointers(p.k, ev, what)
		if err != nil {
			return nil, err
		}
		if len(elems) == 0 {
			return nil, nil
		}
		if p.isHeading(elems[0]) {
			if len(chain) == 0 {
				return nil, nil
			}
			break // a section of its own, inside the last wrapper's
		}
		if i > depth {
			return nil, nil
		}
		spans, err := readSpans(p.k, sv)
		if err != nil {
			return nil, err
		}
		if err := m.spansWithin(p, spans, what); err != nil {
			return nil, err
		}
		next, wrapper := sectionIndex(elems[0])
		chain = append(chain, chainLink{elements: ev, spans: spans, wrapper: wrapper})
		if !wrapper {
			break
		}
		if next != i+1 {
			return nil, nil
		}
	}
	n := len(chain)
	for i, c := range chain {
		if i < n-1 { // its pointer to the next link, which is folded too
			var err error
			if _, c.elements, err = splitArray(p.k, c.elements, 1); err != nil {
				return nil, err
			}
		}
		cont := continuation{p: p, elements: c.elements, spans: c.spans}
		if c.wrapper {
			l.conts[i] = cont
			l.into = append(l.into, i+1)
			continue
		}
		l.content(cont, i)
		l.into = append(l.into, depth)
	}
	p.secDrop, p.rootDrop = n, 1
	return l, nil
}

// leadingRun returns the continuation a root opens with when its first element is neither a
// section nor a heading, and a section follows: the elements before the first section or
// heading. Nothing it can place — no section after the run, no span over it — is a continuation.
func (m *merger) leadingRun(p *part, r *root, rootElems []string) (*continuation, error) {
	isSection := func(e string) bool { _, ok := sectionIndex(e); return ok }
	n := 0
	for n < len(rootElems) && !isSection(rootElems[n]) && !p.isHeading(rootElems[n]) {
		n++
	}
	if !slices.ContainsFunc(rootElems[n:], isSection) {
		return nil, nil
	}
	lo, hi, err := m.extent(p, rootElems[:n])
	if err != nil || lo < 0 {
		return nil, err
	}
	var spans []span
	for _, s := range r.spans {
		if a, b := max(s.offset, lo), min(s.offset+s.length, hi); a < b {
			spans = append(spans, span{a, b - a})
		}
	}
	if len(spans) == 0 {
		return nil, nil
	}
	v, _ := r.get("elements")
	run, _, err := splitArray(p.k, v, n)
	if err != nil {
		return nil, err
	}
	p.rootDrop = n
	return &continuation{p: p, elements: run, spans: spans}, nil
}

// extent is where the items that leaf pointers ptrs name start and end in part p's content, by
// their own spans, or -1, -1 when they have none.
func (m *merger) extent(p *part, ptrs []string) (lo, hi int, err error) {
	want := map[string][]int{}
	for _, ptr := range ptrs {
		name, i, ok := leafPointer(ptr)
		if !ok || i >= p.counts[name] {
			return 0, 0, checkf("part %d: root element pointer %q names no item of the part", p.k, ptr)
		}
		want[name] = append(want[name], i)
	}
	lo, hi = -1, -1
	for name, idx := range want {
		mb, _, _ := p.member(name)
		last := slices.Max(idx)
		err := withFile(p, func(f *os.File) error {
			dec := json.NewDecoder(io.NewSectionReader(f, mb.Start, mb.End-mb.Start))
			if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
				return checkf("part %d: %s is not an array", p.k, name)
			}
			for i := 0; i <= last; i++ {
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return decodeErr(p.k, name, err)
				}
				if !slices.Contains(idx, i) {
					continue
				}
				o, err := readObject(p.k, raw, name+"["+strconv.Itoa(i)+"]")
				if err != nil {
					return err
				}
				sv, ok := o.get("spans")
				if !ok {
					continue
				}
				spans, err := readSpans(p.k, sv)
				if err != nil {
					return err
				}
				for _, s := range spans {
					if lo < 0 || s.offset < lo {
						lo = s.offset
					}
					hi = max(hi, s.offset+s.length)
				}
			}
			return nil
		})
		if err != nil {
			return 0, 0, err
		}
	}
	return lo, hi, nil
}

// leafPointer parses "/paragraphs/N", "/tables/N", "/figures/N" or "/keyValuePairs/N".
func leafPointer(ptr string) (string, int, bool) {
	for _, name := range pointerMembers {
		if n, ok := strings.CutPrefix(ptr, "/"+name+"/"); ok && name != "sections" {
			i, ok := parseIndex([]byte(n))
			return name, i, ok
		}
	}
	return "", 0, false
}

// openPath follows part p's tree from its root through each last element that is a section, and
// returns the sections it passes, the root's first. It is empty when the root's own last element
// is not a section, and when a section on the way cannot be extended — without spans or elements —
// since no fold could reach below it: such a section closes everything and opens nothing.
func (m *merger) openPath(p *part, secs []json.RawMessage, rootElems []string) ([]int, error) {
	elems := rootElems
	var path []int
	for range secs { // a well-formed tree is no deeper than it has sections
		if len(elems) == 0 {
			break
		}
		i, ok := sectionIndex(elems[len(elems)-1])
		if !ok || i < 1 || i >= len(secs) || slices.Contains(path, i) {
			break
		}
		what := "section " + strconv.Itoa(i)
		s, err := readObject(p.k, secs[i], what)
		if err != nil {
			return nil, err
		}
		ev, hasElems := s.get("elements")
		if _, hasSpans := s.get("spans"); !hasElems || !hasSpans {
			return nil, nil
		}
		if elems, err = pointers(p.k, ev, what); err != nil {
			return nil, err
		}
		path = append(path, i)
	}
	return path, nil
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
// continuations folded into it: their elements, and their spans, which a section above the one
// that takes the elements gets too.
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
// by its own part. A continuation without elements, which extends a section's spans alone, adds
// none.
func (m *merger) writeElements(bw *bufio.Writer, lists []continuation, prefix []jsonx.PathElem) error {
	bw.WriteByte('[')
	total := 0
	for _, l := range lists {
		if l.elements == nil {
			continue
		}
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

// splitArray splits a JSON array into its first n items and the rest.
func splitArray(k int, raw json.RawMessage, n int) (head, tail json.RawMessage, err error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, checkf("part %d: root section elements are not an array", k)
	}
	join := func(items []json.RawMessage) json.RawMessage {
		out := []byte{'['}
		for i, it := range items {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, it...)
		}
		return append(out, ']')
	}
	n = min(n, len(items))
	return join(items[:n]), join(items[n:]), nil
}
