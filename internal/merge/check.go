package merge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// measure reads every part's shape and checks that the parts agree with each other.
//
// Part 0's file stays open throughout, so each later part's non-array members can be compared
// with part 0's straight from disk.
func (m *merger) measure(parts []Part) error {
	if len(parts) == 0 {
		return checkf("no parts")
	}
	for k, pt := range parts {
		if err := validPages(k, pt.Pages); err != nil {
			return err
		}
	}
	m.parts = make([]*part, len(parts))
	for k := range parts {
		m.parts[k] = &part{Part: parts[k], k: k}
	}
	return withFile(m.parts[0], func(f0 *os.File) error {
		for _, p := range m.parts {
			var err error
			if p.k == 0 {
				err = m.measurePart(p, f0, f0)
			} else {
				err = withFile(p, func(f *os.File) error { return m.measurePart(p, f, f0) })
			}
			if err != nil {
				return err
			}
		}
		return m.plan()
	})
}

// validPages checks a part's planned pages: at least one, positive, strictly ascending.
func validPages(k int, pages []int) error {
	if len(pages) == 0 {
		return checkf("part %d covers no pages", k)
	}
	for i, p := range pages {
		if p < 1 || i > 0 && p <= pages[i-1] {
			return checkf("part %d: planned pages %v are not ascending page numbers", k, pages)
		}
	}
	return nil
}

func (m *merger) measurePart(p *part, f, f0 io.ReaderAt) error {
	members, err := jsonx.Members(f, p.Start, p.End)
	if err != nil {
		return jsonErr(p.k, "read the result", err)
	}
	p.members = members
	p.isArray = make([]bool, len(members))
	p.counts = map[string]int{}
	for i, mb := range members {
		for _, prev := range members[:i] {
			if prev.Name == mb.Name {
				return checkf("part %d: member %q appears twice", p.k, mb.Name)
			}
		}
		first := m.cmpA[:1]
		if err := readAt(f, first, mb.Start); err != nil {
			return fmt.Errorf("merge: part %d: read %s: %w", p.k, mb.Name, err)
		}
		p.isArray[i] = first[0] == '['
		if p.isArray[i] && mb.Name != m.prof.pageArray {
			n, err := jsonx.CountItems(f, mb.Start, mb.End)
			if err != nil {
				return jsonErr(p.k, "count "+mb.Name, err)
			}
			p.counts[mb.Name] = n
		}
	}

	pm, i, ok := p.member(m.prof.pageArray)
	if !ok || !p.isArray[i] {
		return checkf("part %d has no %s array", p.k, m.prof.pageArray)
	}
	if p.pages, err = collectPages(p.k, f, pm, m.prof.pageArray, m.prof.pageKey); err != nil {
		return err
	}
	p.counts[m.prof.pageArray] = len(p.pages)

	if m.prof.di {
		if err := m.measureContent(p, f); err != nil {
			return err
		}
	}
	if p.k > 0 {
		return m.agrees(p, f, f0)
	}
	return nil
}

// collectPages reads the page number of every item of a part's page array, in order.
//
// Every item must carry exactly one, so the page count is the item count; an item with no
// scalars at all escapes this and is caught when the items are copied and counted.
func collectPages(k int, f io.ReaderAt, mb jsonx.Member, array, key string) ([]int, error) {
	var pages []int
	items := 0
	var bad error
	visit := func(path []jsonx.PathElem, kind jsonx.Kind, raw []byte) ([]byte, bool) {
		if len(path) < 2 || !path[1].IsIndex {
			return nil, false
		}
		i := path[1].Index
		items = max(items, i+1)
		if len(path) != 3 || !isKey(path[2], key) || bad != nil {
			return nil, false
		}
		v, ok := parseIndex(raw)
		switch {
		case kind != jsonx.Number || !ok || v < 1:
			bad = checkf("part %d: %s[%d].%s is %s, not a page number", k, array, i, key, raw)
		case i != len(pages):
			bad = checkf("part %d: %s[%d] has a second %s, or %s[%d] has none",
				k, array, i, key, array, len(pages))
		default:
			pages = append(pages, v)
		}
		return nil, false
	}
	r := io.NewSectionReader(f, mb.Start, mb.End-mb.Start)
	if err := jsonx.Rewrite(io.Discard, r, []jsonx.PathElem{{Key: array}}, visit); err != nil {
		if bad == nil {
			bad = jsonErr(k, array, err)
		}
	}
	if bad != nil {
		return nil, bad
	}
	if items != len(pages) {
		return nil, checkf("part %d: %s[%d] has no %s", k, array, len(pages), key)
	}
	return pages, nil
}

// measureContent decodes a part's content to measure it in the unit. Parts are a few pages, so
// this holds a few pages of text at a time, never the document's.
func (m *merger) measureContent(p *part, f io.ReaderAt) error {
	mb, _, ok := p.member("content")
	if !ok {
		return checkf("part %d has no content", p.k)
	}
	size := int(mb.End - mb.Start)
	if cap(m.content) < size {
		m.content = make([]byte, size)
	}
	raw := m.content[:size]
	if err := readAt(f, raw, mb.Start); err != nil {
		return fmt.Errorf("merge: part %d: read content: %w", p.k, err)
	}
	var s string
	if raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return checkf("part %d: content is not a string", p.k)
	}
	p.length = textunit.Len(s, m.unit)
	p.endsCR = strings.HasSuffix(s, "\r")
	p.lt = ltEvidence(raw)
	return nil
}

// agrees checks part p's non-array members against part 0's: the same names, and the same bytes.
// Content is the one exception: it is the text being merged.
func (m *merger) agrees(p *part, f, f0 io.ReaderAt) error {
	p0 := m.parts[0]
	same := func(name string) (bool, error) {
		a, ai, aok := p0.member(name)
		b, bi, bok := p.member(name)
		if !aok || !bok {
			return aok == bok, nil
		}
		if p0.isArray[ai] || p.isArray[bi] {
			return p0.isArray[ai] == p.isArray[bi], nil
		}
		return m.sameBytes(f0, a, f, b)
	}
	// The named members first, so that the failure says which.
	for _, name := range m.prof.agree {
		ok, err := same(name)
		if err != nil {
			return fmt.Errorf("merge: part %d: compare %s: %w", p.k, name, err)
		}
		if !ok {
			return checkf("part %d: %s differs from part 0's", p.k, name)
		}
	}
	for _, q := range []*part{p0, p} {
		for i, mb := range q.members {
			if q.isArray[i] || m.prof.di && mb.Name == "content" {
				continue
			}
			ok, err := same(mb.Name)
			if err != nil {
				return fmt.Errorf("merge: part %d: compare %s: %w", p.k, mb.Name, err)
			}
			if !ok {
				return checkf("part %d: member %q differs from part 0's", p.k, mb.Name)
			}
		}
	}
	return nil
}

// sameBytes compares two byte ranges in fixed-size pieces.
func (m *merger) sameBytes(fa io.ReaderAt, a jsonx.Member, fb io.ReaderAt, b jsonx.Member) (bool, error) {
	if a.End-a.Start != b.End-b.Start {
		return false, nil
	}
	for off := int64(0); off < a.End-a.Start; off += int64(len(m.cmpA)) {
		n := min(int64(len(m.cmpA)), a.End-a.Start-off)
		x, y := m.cmpA[:n], m.cmpB[:n]
		if err := readAt(fa, x, a.Start+off); err != nil {
			return false, err
		}
		if err := readAt(fb, y, b.Start+off); err != nil {
			return false, err
		}
		if !bytes.Equal(x, y) {
			return false, nil
		}
	}
	return true, nil
}

// plan fixes the merged member order — part 0's, then names first seen in later parts — and
// checks that a member is an array in every part or in none.
func (m *merger) plan() error {
	for _, p := range m.parts {
		for i, mb := range p.members {
			arr, seen := m.array[mb.Name]
			if !seen {
				m.order = append(m.order, mb.Name)
				m.array[mb.Name] = p.isArray[i]
			} else if arr != p.isArray[i] {
				return checkf("member %q is an array in one part and not in another", mb.Name)
			}
		}
	}
	return nil
}

// mapPages decides how each part numbers its pages and checks that, mapped to original pages,
// the parts return exactly their planned pages, in order, with every part after the previous.
//
// A result numbered 1..k is a physically split part, and page i is the part's i-th planned page.
// Otherwise the result must be numbered with the original pages, as a pages= re-check on the
// original is. The two can only be confused when the planned pages are 1..k, and then they agree.
func (m *merger) mapPages() error {
	last := 0
	for _, p := range m.parts {
		if p.Pages[0] <= last {
			return checkf("part %d starts at page %d, not after page %d where part %d ends",
				p.k, p.Pages[0], last, p.k-1)
		}
		last = p.Pages[len(p.Pages)-1]

		p.local = true
		for _, n := range p.pages {
			if n > len(p.Pages) {
				p.local = false
				break
			}
		}
		if len(p.pages) != len(p.Pages) {
			return checkf("part %d: the result has %d pages, but %d were planned", p.k, len(p.pages), len(p.Pages))
		}
		for i, n := range p.pages {
			if got, ok := p.mapPage(n); !ok || got != p.Pages[i] {
				return checkf("part %d: result page %d (numbered %d) is not planned page %d",
					p.k, i+1, n, p.Pages[i])
			}
		}
		m.stats.Pages += len(p.Pages)
	}
	return nil
}

// shift sets each part's offsets: the content and items of all earlier parts.
func (m *merger) shift(sepLen int) {
	char, sec := 0, 1
	var ptr [numPointers]int
	for _, p := range m.parts {
		p.char = char
		char += p.length + sepLen
		p.ptrBase = ptr
		for i, name := range pointerMembers {
			ptr[i] += p.counts[name]
		}
		p.secBase = sec
		if n := p.counts["sections"]; n > 0 {
			sec += n - 1 // every part has its own root; only the merged root survives
		}
	}
}

// parseIndex parses a canonical non-negative integer: digits only, no sign, no leading zero.
// Offsets, page numbers and pointer indexes are always written so; anything else means the
// merge would be guessing.
func parseIndex(b []byte) (int, bool) {
	if len(b) == 0 || len(b) > 18 || len(b) > 1 && b[0] == '0' {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func isKey(e jsonx.PathElem, key string) bool { return !e.IsIndex && e.Key == key }
