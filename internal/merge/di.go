package merge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// DI writes the merged AnalyzeResult object to w.
//
// unit is the request's stringIndexType; the parts' results must say the same.
//
// The checks that need only the parts' shapes run before anything is written. Those that need a
// value in its place — a pointer past its part's items, a region on a page outside its part —
// run as the value is copied, so on any error w may hold part of the output: write to a
// temporary file and discard it unless the merge succeeds.
func DI(w io.Writer, parts []Part, unit textunit.Unit) (Stats, error) {
	m := newMerger(diProfile, unit)
	if err := m.measure(parts); err != nil {
		return Stats{}, err
	}
	if err := m.checkUnit(); err != nil {
		return Stats{}, err
	}
	if err := m.mapPages(); err != nil {
		return Stats{}, err
	}
	if err := m.checkSections(); err != nil {
		return Stats{}, err
	}
	sep, sepLen, err := m.separator()
	if err != nil {
		return Stats{}, err
	}
	if m.unit == textunit.TextElements {
		// A CR at the end of a part would join the separator's LF into one grapheme cluster, and
		// the merged offsets would be one too high after it.
		for _, p := range m.parts[:len(m.parts)-1] {
			if p.endsCR {
				return Stats{}, checkf("part %d: content ends with CR, which joins the page separator", p.k)
			}
		}
	}
	m.sep = sep
	m.shift(sepLen)
	for _, p := range m.parts {
		v := &diVisitor{p: p}
		p.visit, p.verr = v.visit, &v.err
	}
	if err := m.write(w); err != nil {
		return Stats{}, err
	}
	return m.stats, nil
}

// checkUnit requires the results to be in the unit the caller measures in. Offsets counted in
// one unit and shifted by lengths counted in another would be silently wrong.
func (m *merger) checkUnit() error {
	got := textunit.TextElements // what the service means when the member is absent
	p0 := m.parts[0]
	if mb, _, ok := p0.member("stringIndexType"); ok {
		var name string
		err := withFile(p0, func(f *os.File) error {
			b := make([]byte, mb.End-mb.Start)
			if err := readAt(f, b, mb.Start); err != nil {
				return fmt.Errorf("merge: part 0: read stringIndexType: %w", err)
			}
			if json.Unmarshal(b, &name) != nil {
				return checkf("stringIndexType %s is not a string", b)
			}
			return nil
		})
		if err != nil {
			return err
		}
		u, ok := textunit.Parse(name)
		if !ok {
			return checkf("unknown stringIndexType %q", name)
		}
		got = u
	}
	if got != m.unit {
		return checkf("the results are in %v, not the requested %v", got, m.unit)
	}
	return nil
}

func (m *merger) writeContent(bw *bufio.Writer) error {
	bw.WriteByte('"')
	for k, p := range m.parts {
		if k > 0 {
			bw.Write(m.sep)
		}
		mb, _, _ := p.member("content")
		err := withFile(p, func(f *os.File) error {
			// The string's body, between its quotes, exactly as the service escaped it.
			body := io.NewSectionReader(f, mb.Start+1, mb.End-mb.Start-2)
			if _, err := io.Copy(bw, body); err != nil {
				return fmt.Errorf("merge: part %d: copy content: %w", p.k, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	bw.WriteByte('"')
	return nil
}

// diVisitor renumbers one part's scalars into the merged document (spec §5):
//
//   - a Number at …,"span","offset" or …,"spans",i,"offset": plus the part's content shift;
//   - a Number at "pages",i,"pageNumber" or …,"boundingRegions",i,"pageNumber": the original page;
//   - a String in an "elements" array, "/paragraphs/N" and the like: N plus the items of that
//     member in earlier parts, or for /sections/N the section's merged index;
//   - a String at "figures",i,"id", "<page>.<n>": the page mapped to the original.
//
// Anything at one of those paths that cannot be renumbered exactly — a fractional offset, a page
// outside the part, a pointer past the part's own items or to a member it has no rule for — is a
// check failure rather than a guess. The first is kept in err, since a visitor cannot return one.
type diVisitor struct {
	p   *part
	buf []byte
	err error
}

func (v *diVisitor) visit(path []jsonx.PathElem, kind jsonx.Kind, raw []byte) ([]byte, bool) {
	n := len(path)
	if v.err != nil || n == 0 {
		return nil, false
	}
	last := path[n-1]
	if last.IsIndex {
		if n >= 2 && isKey(path[n-2], "elements") {
			return v.pointer(kind, raw)
		}
		return nil, false
	}
	switch last.Key {
	case "offset":
		if n >= 2 && isKey(path[n-2], "span") || n >= 3 && path[n-2].IsIndex && isKey(path[n-3], "spans") {
			return v.offset(kind, raw)
		}
	case "pageNumber":
		if n >= 3 && path[n-2].IsIndex && (n == 3 && isKey(path[0], "pages") || isKey(path[n-3], "boundingRegions")) {
			return v.page(kind, raw)
		}
	case "id":
		if n == 3 && isKey(path[0], "figures") && path[1].IsIndex {
			return v.figureID(kind, raw)
		}
	}
	return nil, false
}

func (v *diVisitor) fail(format string, args ...any) ([]byte, bool) {
	v.err = checkf("part %d: "+format, append([]any{v.p.k}, args...)...)
	return nil, false
}

// number writes x as the replacement, or keeps the original when it is unchanged.
func (v *diVisitor) number(x, was int) ([]byte, bool) {
	if x == was {
		return nil, false
	}
	v.buf = strconv.AppendInt(v.buf[:0], int64(x), 10)
	return v.buf, true
}

func (v *diVisitor) offset(kind jsonx.Kind, raw []byte) ([]byte, bool) {
	off, ok := parseIndex(raw)
	if kind != jsonx.Number || !ok {
		return v.fail("span offset %s is not a non-negative integer", raw)
	}
	return v.number(off+v.p.char, off)
}

func (v *diVisitor) page(kind jsonx.Kind, raw []byte) ([]byte, bool) {
	n, ok := parseIndex(raw)
	if kind != jsonx.Number || !ok {
		return v.fail("pageNumber %s is not a page number", raw)
	}
	orig, ok := v.p.mapPage(n)
	if !ok {
		return v.fail("pageNumber %d is not one of the part's pages", n)
	}
	return v.number(orig, n)
}

// pointer renumbers "/<member>/<index>".
func (v *diVisitor) pointer(kind jsonx.Kind, raw []byte) ([]byte, bool) {
	if kind != jsonx.String {
		return v.fail("element pointer %s is not a string", raw)
	}
	body := raw[1 : len(raw)-1]
	slash := bytes.LastIndexByte(body, '/')
	if len(body) < 3 || body[0] != '/' || slash < 1 {
		return v.fail("element pointer %s is not a /member/index pointer", raw)
	}
	name, digits := body[1:slash], body[slash+1:]
	which := -1
	for i, member := range pointerMembers {
		if string(name) == member {
			which = i
		}
	}
	i, ok := parseIndex(digits)
	if which < 0 || !ok {
		return v.fail("element pointer %s is not one the merge can renumber", raw)
	}
	if own := v.p.counts[pointerMembers[which]]; i >= own {
		return v.fail("element pointer %s is past the part's %d %s", raw, own, name)
	}
	j := i + v.p.ptrBase[which]
	if which == ptrSections && i > 0 {
		j = v.p.secBase + i - 1
	} else if which == ptrSections {
		j = 0
	}
	if j == i {
		return nil, false
	}
	v.buf = append(v.buf[:0], `"/`...)
	v.buf = append(v.buf, name...)
	v.buf = append(v.buf, '/')
	v.buf = strconv.AppendInt(v.buf, int64(j), 10)
	v.buf = append(v.buf, '"')
	return v.buf, true
}

// figureID maps the page in "<page>.<n>" and keeps <n> as it is.
func (v *diVisitor) figureID(kind jsonx.Kind, raw []byte) ([]byte, bool) {
	if kind != jsonx.String {
		return v.fail("figure id %s is not a string", raw)
	}
	body := raw[1 : len(raw)-1]
	dot := bytes.IndexByte(body, '.')
	page, ok := 0, dot > 0 && dot < len(body)-1
	if ok {
		page, ok = parseIndex(body[:dot])
	}
	if !ok {
		return v.fail(`figure id %s is not "<page>.<n>"`, raw)
	}
	orig, ok := v.p.mapPage(page)
	if !ok {
		return v.fail("figure id %s names a page outside the part", raw)
	}
	if orig == page {
		return nil, false
	}
	v.buf = append(v.buf[:0], '"')
	v.buf = strconv.AppendInt(v.buf, int64(orig), 10)
	v.buf = append(v.buf, body[dot:]...)
	v.buf = append(v.buf, '"')
	return v.buf, true
}
