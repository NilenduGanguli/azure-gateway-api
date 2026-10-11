package realcut

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
)

// ErrMalformed is wrapped by every error a cut returns for an input it cannot cut: JSON that does
// not parse, a span outside the content (in the result's stringIndexType), a page, pointer or
// figure id that names nothing in the result, or a plan that does not fit it. The message names
// the first few offending paths.
var ErrMalformed = errors.New("realcut: malformed result")

// ErrInternal is wrapped by the error a cut returns when realcut itself panicked: input it should
// have refused with ErrMalformed got past its checks. The message carries the panic and its stack.
// It is a backstop so that one bad cut fails its own test instead of aborting the test binary;
// realcut's own tests treat it as a bug.
var ErrInternal = errors.New("realcut: internal error")

func malformedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

func internalError(r any) error {
	return fmt.Errorf("%w: panic: %v\n%s", ErrInternal, r, debug.Stack())
}

// maxProblems is how many problems an error spells out; the rest are counted.
const maxProblems = 8

// problems collects what is wrong with an input, so one error names several.
type problems struct {
	list []string
	n    int
}

func (p *problems) addf(format string, args ...any) {
	p.n++
	if len(p.list) < maxProblems {
		p.list = append(p.list, fmt.Sprintf(format, args...))
	}
}

func (p *problems) err() error {
	if p.n == 0 {
		return nil
	}
	msg := strings.Join(p.list, "; ")
	if p.n > len(p.list) {
		msg += fmt.Sprintf("; and %d more", p.n-len(p.list))
	}
	return malformedf("%s", msg)
}

// path is where a node is, formatted only when a problem is reported.
type path []pe

func (p path) String() string {
	if len(p) == 0 {
		return "the result"
	}
	var b strings.Builder
	for _, e := range p {
		if e.idx {
			b.WriteString("[" + strconv.Itoa(e.i) + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(e.key)
	}
	return b.String()
}

func (p path) key(k string) path { return append(p[:len(p):len(p)], pe{key: k}) }
func (p path) at(i int) path     { return append(p[:len(p):len(p)], pe{idx: true, i: i}) }

func rawOf(n *Node) string {
	if n == nil {
		return "(absent)"
	}
	return short(Encode(n))
}

// index parses a canonical non-negative integer: digits only, no sign, no leading zero, as the
// service writes page numbers and pointer indexes.
func index(s string) (int, bool) {
	if s == "" || len(s) > 9 || len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// checkPlan checks that every part has pages, every page is in the result and none is planned
// twice.
func checkPlan(plan [][]int, inResult func(int) bool, p *problems) {
	if len(plan) == 0 {
		p.addf("the plan has no parts")
	}
	seen := map[int]int{}
	for k, pages := range plan {
		if len(pages) == 0 {
			p.addf("part %d of the plan has no pages", k)
		}
		for _, n := range pages {
			if !inResult(n) {
				p.addf("part %d: page %d is not in the result", k, n)
			}
			if k0, dup := seen[n]; dup {
				p.addf("page %d is planned in part %d and again in part %d", n, k0, k)
			}
			seen[n] = k
		}
	}
}

// docMembers are the analyzeResult members the cut splits item by item.
var docMembers = []string{"pages", "paragraphs", "tables", "figures", "keyValuePairs", "sections", "styles"}

// validate checks everything the cut follows before it follows any of it: the members it splits
// are arrays of objects, pages are numbered once each, the plan names pages of the result, every
// span lies within the content in the result's unit, every region is on a page of the result,
// every pointer names an item that exists, and every figure id is "<page>.<n>" on its first
// region's page.
func (c *cutter) validate() error {
	var p problems
	r := c.r
	for _, m := range docMembers {
		v := r.Get(m)
		switch {
		case v == nil && m == "pages":
			p.addf("the result has no pages")
		case v == nil:
		case v.Kind != Arr:
			p.addf("%s is %s, not an array", m, rawOf(v))
		default:
			for i, it := range v.Vals {
				if it.Kind != Obj {
					p.addf("%s[%d] is %s, not an object", m, i, rawOf(it))
				}
			}
		}
	}
	c.pageAt = map[int]int{}
	for i, pg := range r.Get("pages").vals() {
		n, ok := pg.Get("pageNumber").Int()
		if j, dup := c.pageAt[n]; ok && dup {
			p.addf("pages[%d] and pages[%d] are both page %d", j, i, n)
		} else if !ok || n < 1 {
			p.addf("pages[%d].pageNumber %s is not a page number", i, rawOf(pg.Get("pageNumber")))
		} else {
			c.pageAt[n] = i
		}
	}
	checkPlan(c.plan, func(n int) bool { _, ok := c.pageAt[n]; return ok }, &p)
	c.walk(r, nil, &p)
	for i, f := range r.Get("figures").vals() {
		c.checkFigureID(f, i, &p)
	}
	return p.err()
}

// walk checks every span, region and pointer under n.
func (c *cutter) walk(n *Node, at path, p *problems) {
	switch n.Kind {
	case Obj:
		seen := make(map[string]bool, len(n.Keys))
		for i, k := range n.Keys {
			if seen[k] {
				p.addf("%s: member %q appears twice", at, k)
			}
			seen[k] = true
			v, here := n.Vals[i], at.key(k)
			switch k {
			case "span":
				c.checkSpan(v, here, p)
			case "spans", "boundingRegions", "elements":
				if v.Kind != Arr {
					p.addf("%s is %s, not an array", here, rawOf(v))
					continue
				}
				for j, it := range v.Vals {
					switch k {
					case "spans":
						c.checkSpan(it, here.at(j), p)
					case "boundingRegions":
						c.checkRegion(it, here.at(j), p)
					case "elements":
						c.checkPointer(it, here.at(j), p)
					}
				}
			}
			c.walk(v, here, p)
		}
	case Arr:
		for i, v := range n.Vals {
			c.walk(v, at.at(i), p)
		}
	}
}

// checkSpan: an {offset, length} that lies within the content and starts and ends on a unit.
func (c *cutter) checkSpan(s *Node, at path, p *problems) {
	o, ok1 := s.Get("offset").Int()
	l, ok2 := s.Get("length").Int()
	switch {
	case !ok1 || !ok2:
		p.addf("%s %s is not an {offset, length} span", at, rawOf(s))
	case o < 0 || l < 0:
		p.addf("%s {offset %d, length %d} is negative", at, o, l)
	case o > c.t.units()-l:
		p.addf("%s {offset %d, length %d} runs past the end of the content, %d %s long",
			at, o, l, c.t.units(), c.unit)
	default:
		if err := c.t.checkRange(o, o+l); err != nil {
			p.addf("%s {offset %d, length %d}: %s", at, o, l, strings.TrimPrefix(err.Error(), ErrMalformed.Error()+": "))
		}
	}
}

// checkRegion: a region on a page of the result.
func (c *cutter) checkRegion(br *Node, at path, p *problems) {
	n, ok := br.Get("pageNumber").Int()
	if _, in := c.pageAt[n]; !ok || !in {
		p.addf("%s.pageNumber %s is not a page of the result", at, rawOf(br.Get("pageNumber")))
	}
}

// checkPointer: "/member/index" naming an item the result has, in a member the cut renumbers.
func (c *cutter) checkPointer(e *Node, at path, p *problems) {
	s, ok := e.Str()
	if !ok {
		p.addf("%s %s is not a pointer", at, rawOf(e))
		return
	}
	m, j, ok := parsePtr(s)
	switch {
	case !ok:
		p.addf("%s %q is not a pointer /<member>/<index>", at, s)
	case m != "sections" && !isLeafMember(m):
		p.addf("%s %q points into %s, which the cut does not renumber", at, s, m)
	case j >= len(c.r.Get(m).vals()):
		p.addf("%s %q points past the result's %d %s", at, s, len(c.r.Get(m).vals()), m)
	}
}

func isLeafMember(m string) bool {
	for _, l := range leafMembers {
		if l == m {
			return true
		}
	}
	return false
}

// checkFigureID: "<page>.<n>", the page one of the result's and the figure's first region's.
func (c *cutter) checkFigureID(f *Node, i int, p *problems) {
	v := f.Get("id")
	if v == nil {
		return
	}
	s, _ := v.Str()
	page, ok := figurePage(s)
	if !ok {
		p.addf("figures[%d].id %s is not <page>.<n>", i, rawOf(v))
		return
	}
	if _, in := c.pageAt[page]; !in {
		p.addf("figures[%d].id %q names page %d, which is not in the result", i, s, page)
		return
	}
	if q, ok := elementPage(f); ok && q != page {
		p.addf("figures[%d].id %q names page %d, but its first bounding region is on page %d", i, s, page, q)
	}
}

// figurePage is the page of a figure id "<page>.<n>", both canonical, page and n at least 1.
func figurePage(id string) (int, bool) {
	ps, ns, ok := strings.Cut(id, ".")
	page, ok1 := index(ps)
	n, ok2 := index(ns)
	return page, ok && ok1 && ok2 && page >= 1 && n >= 1
}
