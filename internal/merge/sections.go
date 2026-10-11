package merge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
)

// Sections are the one array the merge cannot simply concatenate. Every part's sections[0] is a
// root covering the whole of that part, and a document has one root. So the merged root is built
// from the parts' roots — their elements in order, and one span from the first root's start to
// the last root's end — and the parts' other sections follow it in part order, renumbered so that
// part k's section i ≥ 1 becomes secBase[k] + i − 1.
//
// A part without sections contributes none. If part 0 has none while a later part does, the
// whole document's root could not be told from the parts, so that is a check failure.

// root is a part's root section: its members in order, with their raw values.
type root struct {
	keys   []string
	values []json.RawMessage
	rest   int64 // where the item after the root starts within the file, or -1 if there is none
}

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
func readRoot(k int, f io.ReaderAt, mb jsonx.Member) (*root, error) {
	dec := json.NewDecoder(io.NewSectionReader(f, mb.Start, mb.End-mb.Start))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, checkf("part %d: sections is not an array", k)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, checkf("part %d: the root section is not an object", k)
	}
	r := &root{rest: -1}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, checkf("part %d: malformed root section", k)
		}
		if _, dup := r.get(key); dup {
			return nil, checkf("part %d: the root section has %q twice", k, key)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, checkf("part %d: root section member %q: %v", k, key, err)
		}
		r.keys = append(r.keys, key)
		r.values = append(r.values, v)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, checkf("part %d: malformed root section", k)
	}
	// More skips the whitespace after the root, leaving the decoder's offset on the comma that
	// introduces section 1, if there is one.
	if dec.More() {
		comma := []byte{0}
		at := mb.Start + dec.InputOffset()
		if err := readAt(f, comma, at); err != nil {
			return nil, fmt.Errorf("merge: part %d: read sections: %w", k, err)
		}
		if comma[0] != ',' {
			return nil, checkf("part %d: no comma after the root section", k)
		}
		r.rest = at + 1
	}
	return r, nil
}

// checkSections refuses parts whose roots cannot make the document's root: part 0 without
// sections while a later part has them.
func (m *merger) checkSections() error {
	if m.parts[0].counts["sections"] > 0 {
		return nil
	}
	for _, p := range m.parts[1:] {
		if p.counts["sections"] > 0 {
			return checkf("part 0 has no sections but part %d does", p.k)
		}
	}
	return nil
}

func (m *merger) writeSections(bw *bufio.Writer) error {
	var with []*part
	var roots []*root
	for _, p := range m.parts {
		if p.counts["sections"] == 0 {
			continue
		}
		mb, _, _ := p.member("sections")
		err := withFile(p, func(f *os.File) error {
			r, err := readRoot(p.k, f, mb)
			roots = append(roots, r)
			return err
		})
		if err != nil {
			return err
		}
		with = append(with, p)
	}
	if len(with) == 0 {
		bw.WriteString("[]")
		m.stats.Counts["sections"] = 0
		return nil
	}

	bw.WriteByte('[')
	if err := m.writeRoot(bw, with, roots); err != nil {
		return err
	}
	total := 1
	prefix := []jsonx.PathElem{{Key: "sections"}}
	for i, p := range with {
		want := p.counts["sections"] - 1
		if want == 0 {
			continue
		}
		if roots[i].rest < 0 {
			return checkf("part %d: %d sections counted, but none after the root", p.k, want+1)
		}
		mb, _, _ := p.member("sections")
		bw.WriteByte(',')
		var n int
		err := withFile(p, func(f *os.File) error {
			// The sections after the root, read as an array of their own: an opening bracket,
			// then the original bytes from section 1 to the closing bracket.
			rest := io.MultiReader(strings.NewReader("["),
				io.NewSectionReader(f, roots[i].rest, mb.End-roots[i].rest))
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

// writeRoot writes the merged root: members in part 0's root order, then any first seen in a later
// root; elements from every root in order; one span over all of them; anything else from the
// first root that has it.
func (m *merger) writeRoot(bw *bufio.Writer, with []*part, roots []*root) error {
	var keys []string
	for _, r := range roots {
		for _, k := range r.keys {
			if !containsString(keys, k) {
				keys = append(keys, k)
			}
		}
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
			err = m.writeRootElements(bw, with, roots)
		case "spans":
			err = writeRootSpans(bw, with, roots)
		default:
			for j, r := range roots {
				if v, ok := r.get(key); ok {
					prefix := []jsonx.PathElem{{Key: "sections"}, {IsIndex: true}, {Key: key}}
					if err = jsonx.Rewrite(bw, bytes.NewReader(v), prefix, with[j].visit); err != nil {
						err = jsonErr(with[j].k, "root section "+key, err)
					} else {
						err = *with[j].verr
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

func (m *merger) writeRootElements(bw *bufio.Writer, with []*part, roots []*root) error {
	prefix := []jsonx.PathElem{{Key: "sections"}, {IsIndex: true}, {Key: "elements"}}
	bw.WriteByte('[')
	total := 0
	for j, r := range roots {
		v, ok := r.get("elements")
		if !ok {
			continue
		}
		p := with[j]
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

// spanJSON is one span as written; pointers tell a missing member from a zero.
type spanJSON struct {
	Offset *int64 `json:"offset"`
	Length *int64 `json:"length"`
}

// writeRootSpans writes the merged root's single span: from the start of the first root that has
// a span to the end of the last, in merged offsets.
func writeRootSpans(bw *bufio.Writer, with []*part, roots []*root) error {
	var start, end int64
	found := false
	for j, r := range roots {
		v, ok := r.get("spans")
		if !ok {
			continue
		}
		var spans []spanJSON
		if err := json.Unmarshal(v, &spans); err != nil {
			return checkf("part %d: root section spans: %v", with[j].k, err)
		}
		char := int64(with[j].char)
		for _, s := range spans {
			if s.Offset == nil || s.Length == nil || *s.Offset < 0 || *s.Length < 0 {
				return checkf("part %d: root section span %s is not an offset and a length", with[j].k, v)
			}
			from, to := *s.Offset+char, *s.Offset+*s.Length+char
			if !found {
				start, end, found = from, to, true
			}
			start, end = min(start, from), max(end, to)
		}
	}
	if !found {
		bw.WriteString("[]")
		return nil
	}
	bw.WriteString(`[{"offset":`)
	bw.WriteString(strconv.FormatInt(start, 10))
	bw.WriteString(`,"length":`)
	bw.WriteString(strconv.FormatInt(end-start, 10))
	bw.WriteString(`}]`)
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
