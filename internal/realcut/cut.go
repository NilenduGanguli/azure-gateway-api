package realcut

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// Bare returns the analyzeResult of an envelope, or the node itself when it is already bare.
func Bare(root *Node) *Node {
	if ar := root.Get("analyzeResult"); ar != nil {
		return ar
	}
	return root
}

// pointer members, in the merge's pointer order.
var leafMembers = []string{"paragraphs", "tables", "figures", "keyValuePairs"}

// cutter holds one whole result being cut.
type cutter struct {
	r        *Node
	unit     textunit.Unit
	t        *text
	markdown bool
	plan     [][]int
	partOf   map[int]int // original page -> part index
	localOf  map[int]int // original page -> local page number within its part
	a, b     []int       // per part: content range [a, b) in units of the whole
	notes    []string

	// per leaf member: each item's part and local index
	itemPart  map[string][]int
	itemLocal map[string][]int

	// sections
	secContains [][]bool // [section][part]
	secLocal    []map[int]int

	pageAt map[int]int // page number -> index in pages
	err    error       // the first thing the cut could not follow; validate should have refused it
}

func (c *cutter) notef(format string, args ...any) {
	c.notes = append(c.notes, fmt.Sprintf(format, args...))
}

// failf records something the cut cannot follow. validate refuses such input up front, so this
// only guards the lookups that would otherwise index out of range.
func (c *cutter) failf(format string, args ...any) {
	if c.err == nil {
		c.err = malformedf(format, args...)
	}
}

// CutDI cuts a whole Document Intelligence analyzeResult (bare or enveloped) into one bare,
// compact analyzeResult per part of plan, numbered Local (pages 1..k), as Azure would answer if
// each part's pages had been sent as their own PDF. notes lists every judgement the cut had to
// make and anything in the input the cut could not place.
//
// Input the cut cannot follow — a span past the content or not on a unit of the result's
// stringIndexType, a region on a page the result does not have, a pointer past its member, a
// malformed figure id, a plan naming pages the result does not have — is an error wrapping
// ErrMalformed that names where, never a panic.
func CutDI(whole []byte, plan [][]int, unit textunit.Unit) (parts [][]byte, notes []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			parts, err = nil, internalError(r)
		}
	}()
	root, err := Parse(whole)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	c := &cutter{r: Bare(root), unit: unit, plan: plan, partOf: map[int]int{}, localOf: map[int]int{},
		itemPart: map[string][]int{}, itemLocal: map[string][]int{}}
	if c.r.Kind != Obj {
		return nil, nil, malformedf("the result is %s, not an object", rawOf(c.r))
	}
	if v := c.r.Get("stringIndexType"); v != nil {
		s, isStr := v.Str()
		if u, ok := textunit.Parse(s); !ok || !isStr {
			return nil, nil, malformedf("stringIndexType %s is not a unit", rawOf(v))
		} else if u != unit {
			return nil, nil, malformedf("the result counts offsets in %s, but the cut was asked for %s", u, unit)
		}
	}
	if f, ok := c.r.Get("contentFormat").Str(); ok && f == "markdown" {
		c.markdown = true
	}
	content := c.r.Get("content")
	if content == nil {
		return nil, nil, malformedf("the result has no content")
	}
	if c.t, err = newText(content.Raw, unit); err != nil {
		return nil, nil, err
	}
	if err := c.validate(); err != nil {
		return nil, nil, err
	}
	for k, pages := range plan {
		for i, p := range pages {
			c.partOf[p] = k
			c.localOf[p] = i + 1
		}
	}
	if err := c.ranges(); err != nil {
		return nil, c.notes, err
	}
	c.assignLeaves()
	c.assignSections()
	for k := range plan {
		parts = append(parts, Encode(c.part(k)))
	}
	if c.err != nil {
		return nil, c.notes, c.err
	}
	return parts, c.notes, nil
}

func spanOf(s *Node) (int, int) {
	o, _ := s.Get("offset").Int()
	l, _ := s.Get("length").Int()
	return o, l
}

// ranges finds each part's content range from its pages' spans.
func (c *cutter) ranges() error {
	pages := c.r.Get("pages").vals()
	byNum := map[int]*Node{}
	maxPage := 0
	for _, p := range pages {
		n, _ := p.Get("pageNumber").Int()
		byNum[n] = p
		maxPage = max(maxPage, n)
	}
	for k, pp := range c.plan {
		lo, hi := -1, -1
		for _, n := range pp {
			p := byNum[n]
			if p == nil {
				return malformedf("page %d is not in the result", n)
			}
			for _, s := range p.Get("spans").vals() {
				o, l := spanOf(s)
				if lo < 0 || o < lo {
					lo = o
				}
				hi = max(hi, o+l)
			}
		}
		if lo < 0 {
			return malformedf("part %d: pages %v have no spans", k, pp)
		}
		if c.markdown && pp[len(pp)-1] < maxPage {
			// A markdown page's span runs to the next page's first element and so holds the
			// "<!-- PageBreak -->" block; a part analysed alone has no next page. Keep the part's
			// text up to and including the newline after its last element, as a single-page
			// markdown result ends (real 1-page results end with exactly one "\n").
			s, err := c.t.str(lo, hi)
			if err != nil {
				return fmt.Errorf("part %d: %w", k, err)
			}
			i := strings.LastIndex(s, "<!-- PageBreak -->")
			if i < 0 || strings.Trim(s[i+len("<!-- PageBreak -->"):], "\n") != "" {
				return malformedf("part %d: markdown page span does not end with a page break: %q", k, s[max(0, len(s)-40):])
			}
			j := i
			for j > 0 && s[j-1] == '\n' {
				j--
			}
			cut := j + 1 // keep one newline
			c.notef("part %d: markdown: dropped %q from the end of the part's text (the whole document's page-break block)", k, s[cut:])
			if hi, err = c.t.unitOf(c.t.unitAt[lo] + cut); err != nil {
				return fmt.Errorf("part %d: markdown: %w", k, err)
			}
		}
		c.a = append(c.a, lo)
		c.b = append(c.b, hi)
	}
	for k := 1; k < len(c.plan); k++ {
		if c.b[k-1] > c.a[k] {
			return malformedf("part %d's text [%d,%d) overlaps part %d's [%d,%d): the plan's parts must follow each other in the content",
				k-1, c.a[k-1], c.b[k-1], k, c.a[k], c.b[k])
		}
		if c.markdown {
			continue
		}
		gap, err := c.t.str(c.b[k-1], c.a[k])
		if err != nil {
			return fmt.Errorf("between part %d and %d: %w", k-1, k, err)
		}
		if gap != "\n" {
			c.notef("text: whole content between part %d and %d is %q, not one \\n", k-1, k, gap)
		}
	}
	for k := range c.plan {
		raw, err := c.t.str(c.a[k], c.b[k])
		if err != nil {
			return fmt.Errorf("part %d: %w", k, err)
		}
		if n := textunit.Len(raw, c.unit); n != c.b[k]-c.a[k] {
			c.notef("part %d: content is %d units alone but %d in the whole", k, n, c.b[k]-c.a[k])
		}
	}
	return nil
}

// pagePart is the part an original page is in, or -1 when the plan does not cover it.
func (c *cutter) pagePart(p int) int {
	if k, ok := c.partOf[p]; ok {
		return k
	}
	return -1
}

// elementPage is the original page an element is on: its first bounding region's.
func elementPage(n *Node) (int, bool) {
	br := n.Get("boundingRegions")
	if br == nil || len(br.Vals) == 0 {
		return 0, false
	}
	return br.Vals[0].Get("pageNumber").Int()
}

func (c *cutter) assignLeaves() {
	for _, m := range leafMembers {
		arr := c.r.Get(m)
		if arr == nil {
			continue
		}
		count := map[int]int{}
		for i, it := range arr.Vals {
			p, ok := elementPage(it)
			k := -1
			if ok {
				if pk, in := c.partOf[p]; in {
					k = pk
				} else {
					k = -2 // on a page outside the plan (a single-part cut): in no part
				}
			} else if sp := it.Get("spans"); sp != nil && len(sp.Vals) > 0 {
				o, _ := spanOf(sp.Vals[0])
				for kk := range c.plan {
					if o >= c.a[kk] && o <= c.b[kk] {
						k = kk
					}
				}
			}
			if k == -2 {
				c.itemPart[m] = append(c.itemPart[m], -1)
				c.itemLocal[m] = append(c.itemLocal[m], -1)
				continue
			}
			if k < 0 {
				c.notef("%s[%d]: no page and no span; put in part 0", m, i)
				k = 0
			}
			// Elements whose regions or spans reach outside their part cross a cut.
			for _, br := range it.Get("boundingRegions").vals() {
				if q, _ := br.Get("pageNumber").Int(); c.pagePart(q) != k {
					c.notef("%s[%d] has a region on page %d, outside its part %d: it crosses a cut", m, i, q, k)
				}
			}
			for _, s := range it.Get("spans").vals() {
				o, l := spanOf(s)
				if o < c.a[k] || o+l > c.b[k] {
					c.notef("%s[%d] span {%d,%d} reaches outside its part %d [%d,%d)", m, i, o, l, k, c.a[k], c.b[k])
				}
			}
			c.itemPart[m] = append(c.itemPart[m], k)
			c.itemLocal[m] = append(c.itemLocal[m], count[k])
			count[k]++
		}
	}
}

func (n *Node) vals() []*Node {
	if n == nil {
		return nil
	}
	return n.Vals
}

// parsePtr splits "/member/index": one member name, and a canonical non-negative index.
func parsePtr(s string) (string, int, bool) {
	m, i, ok := strings.Cut(strings.TrimPrefix(s, "/"), "/")
	if !ok || !strings.HasPrefix(s, "/") || m == "" || strings.Contains(i, "/") {
		return "", 0, false
	}
	n, ok := index(i)
	return m, n, ok
}

// assignSections works out, per part, which sections hold something of the part, and their local
// numbering: the root is 0 and the included sections follow in the whole document's order.
func (c *cutter) assignSections() {
	secs := c.r.Get("sections").vals()
	c.secContains = make([][]bool, len(secs))
	done := make([]bool, len(secs))
	var visit func(i int)
	visit = func(i int) {
		if done[i] {
			return
		}
		done[i] = true
		c.secContains[i] = make([]bool, len(c.plan))
		for _, e := range secs[i].Get("elements").vals() {
			s, _ := e.Str()
			m, j, ok := parsePtr(s)
			if !ok {
				c.notef("sections[%d]: element %s is not a pointer", i, s)
				continue
			}
			if m == "sections" {
				if j >= len(secs) {
					c.failf("sections[%d]: %s points past the %d sections", i, s, len(secs))
					continue
				}
				visit(j)
				for k := range c.plan {
					c.secContains[i][k] = c.secContains[i][k] || c.secContains[j][k]
				}
				continue
			}
			parts := c.itemPart[m]
			if j >= len(parts) {
				c.notef("sections[%d]: %s points past %s", i, s, m)
				continue
			}
			if parts[j] >= 0 {
				c.secContains[i][parts[j]] = true
			}
		}
	}
	for i := range secs {
		visit(i)
	}
	c.secLocal = make([]map[int]int, len(c.plan))
	for k := range c.plan {
		c.secLocal[k] = map[int]int{0: 0}
		n := 1
		for i := 1; i < len(secs); i++ {
			if c.secContains[i][k] {
				c.secLocal[k][i] = n
				n++
			}
		}
	}
	for i := 1; i < len(secs); i++ {
		var in []int
		for k := range c.plan {
			if c.secContains[i][k] {
				in = append(in, k)
			}
		}
		if len(in) > 1 {
			c.notef("sections[%d] holds elements of parts %v: it crosses a cut, so each part has its own copy", i, in)
		}
	}
}

// clipSpans keeps the spans that overlap [a, b), clipped to it; a zero-length span is kept when
// it lies within [a, b].
func clipSpans(spans *Node, a, b int) *Node {
	out := ArrNode()
	for _, s := range spans.vals() {
		o, l := spanOf(s)
		if l == 0 {
			if o >= a && o <= b {
				out.Vals = append(out.Vals, s.Clone())
			}
			continue
		}
		lo, hi := max(o, a), min(o+l, b)
		if lo >= hi {
			continue
		}
		ns := s.Clone()
		ns.Set("offset", NumNode(lo))
		ns.Set("length", NumNode(hi-lo))
		out.Vals = append(out.Vals, ns)
	}
	return out
}

// part builds part k's analyzeResult.
func (c *cutter) part(k int) *Node {
	out := ObjNode()
	a, b := c.a[k], c.b[k]
	for i, key := range c.r.Keys {
		v := c.r.Vals[i]
		switch key {
		case "content":
			raw, err := c.t.rawSlice(a, b)
			if err != nil {
				c.failf("part %d: content: %v", k, err)
			}
			out.Set(key, RawStrNode(raw))
		case "pages":
			arr := ArrNode()
			for _, p := range v.Vals {
				n, _ := p.Get("pageNumber").Int()
				if pk, ok := c.partOf[n]; !ok || pk != k {
					continue
				}
				np := p.Clone()
				if c.markdown {
					np.Set("spans", clipSpans(np.Get("spans"), a, b))
				}
				arr.Vals = append(arr.Vals, c.rewrite(np, []pe{{key: "pages"}, {idx: true}}, k))
			}
			out.Set(key, arr)
		case "paragraphs", "tables", "figures", "keyValuePairs":
			arr := ArrNode()
			for j, it := range v.Vals {
				if j >= len(c.itemPart[key]) {
					c.failf("%s[%d] was not assigned a part", key, j)
					break
				}
				if c.itemPart[key][j] == k {
					arr.Vals = append(arr.Vals, c.rewrite(it.Clone(), []pe{{key: key}, {idx: true}}, k))
				}
			}
			if len(arr.Vals) == 0 && key == "figures" {
				continue // real 2024-11-30 results leave figures out when there are none
			}
			out.Set(key, arr)
		case "styles":
			arr := ArrNode()
			for _, st := range v.Vals {
				sp := clipSpans(st.Get("spans"), a, b)
				if len(sp.Vals) == 0 {
					continue
				}
				ns := st.Clone()
				ns.Set("spans", sp)
				arr.Vals = append(arr.Vals, c.rewrite(ns, []pe{{key: "styles"}, {idx: true}}, k))
			}
			out.Set(key, arr) // real results always carry styles, [] included
		case "sections":
			out.Set(key, c.sections(k))
		default:
			if v.Kind == Arr && len(v.Vals) > 0 {
				c.notef("member %s (%d items) has no cut rule; copied whole into every part", key, len(v.Vals))
			}
			out.Set(key, v.Clone())
		}
	}
	return out
}

func (c *cutter) inPart(ptr string, k int) bool {
	m, j, ok := parsePtr(ptr)
	if !ok {
		return false
	}
	if m == "sections" {
		return j < len(c.secContains) && c.secContains[j] != nil && c.secContains[j][k]
	}
	parts := c.itemPart[m]
	return j < len(parts) && parts[j] == k
}

// sections builds part k's sections: a root whose elements are the part's top-level elements,
// then every section holding something of the part, each with the part's elements only and its
// spans clipped to the part's text.
func (c *cutter) sections(k int) *Node {
	secs := c.r.Get("sections").vals()
	arr := ArrNode()
	any := false
	for i := range secs {
		if i > 0 && c.secContains[i][k] {
			any = true
		}
	}
	if len(secs) > 0 && c.secContains[0][k] {
		any = true
	}
	if !any {
		c.notef("part %d holds nothing any section holds; its sections are []", k)
		return arr
	}
	for i, s := range secs {
		if i > 0 && !c.secContains[i][k] {
			continue
		}
		ns := s.Clone()
		if sp := ns.Get("spans"); sp != nil {
			ns.Set("spans", clipSpans(sp, c.a[k], c.b[k]))
		}
		if el := ns.Get("elements"); el != nil {
			kept := ArrNode()
			for _, e := range el.Vals {
				str, _ := e.Str()
				if c.inPart(str, k) {
					kept.Vals = append(kept.Vals, e)
				}
			}
			ns.Set("elements", kept)
		}
		arr.Vals = append(arr.Vals, c.rewrite(ns, []pe{{key: "sections"}, {idx: true}}, k))
	}
	return arr
}

type pe struct {
	key string
	idx bool
	i   int // the index, when idx; set only where a path is reported
}

// rewrite applies to part k the inverse of the merge's renumbering (merge.diVisitor): offsets
// minus the part's start, page numbers local, element pointers to the part's own arrays, and
// figure ids on the local page.
func (c *cutter) rewrite(n *Node, path []pe, k int) *Node {
	switch n.Kind {
	case Obj:
		for i, key := range n.Keys {
			p := append(slices.Clone(path), pe{key: key})
			v := n.Vals[i]
			if v.Kind == Scalar {
				n.Vals[i] = c.scalar(v, p, k)
			} else {
				n.Vals[i] = c.rewrite(v, p, k)
			}
		}
	case Arr:
		for i, v := range n.Vals {
			p := append(slices.Clone(path), pe{idx: true})
			if v.Kind == Scalar {
				n.Vals[i] = c.scalar(v, p, k)
			} else {
				n.Vals[i] = c.rewrite(v, p, k)
			}
		}
	}
	return n
}

func (c *cutter) scalar(v *Node, path []pe, k int) *Node {
	n := len(path)
	last := path[n-1]
	if last.idx {
		if n >= 2 && path[n-2].key == "elements" && !path[n-2].idx {
			s, _ := v.Str()
			m, j, ok := parsePtr(s)
			if !ok {
				c.notef("pointer %s not parsed", s)
				return v
			}
			var local int
			if m == "sections" {
				l, ok := c.secLocal[k][j]
				if !ok {
					c.notef("part %d: pointer %s to a section outside the part", k, s)
					return v
				}
				local = l
			} else {
				if j >= len(c.itemPart[m]) {
					c.failf("part %d: pointer %s points past %s", k, s, m)
					return v
				}
				if c.itemPart[m][j] != k {
					c.notef("part %d: pointer %s to an item in part %d", k, s, c.itemPart[m][j])
				}
				local = c.itemLocal[m][j]
			}
			return StrNode("/" + m + "/" + strconv.Itoa(local))
		}
		return v
	}
	switch last.key {
	case "offset":
		if n >= 2 && path[n-2].key == "span" && !path[n-2].idx || n >= 3 && path[n-2].idx && path[n-3].key == "spans" {
			o, _ := v.Int()
			if o < c.a[k] || o > c.b[k] {
				c.notef("part %d: offset %d at %v outside [%d,%d]", k, o, path, c.a[k], c.b[k])
			}
			return NumNode(o - c.a[k])
		}
	case "pageNumber":
		if n >= 3 && path[n-2].idx && (n == 3 && path[0].key == "pages" || path[n-3].key == "boundingRegions") {
			p, _ := v.Int()
			if c.pagePart(p) != k {
				c.notef("part %d: pageNumber %d at %v is in part %d", k, p, path, c.pagePart(p))
			}
			return NumNode(c.localOf[p])
		}
	case "id":
		if n == 3 && path[0].key == "figures" && path[1].idx {
			s, _ := v.Str()
			p, ok := figurePage(s)
			if !ok {
				c.failf("figure id %q is not <page>.<n>", s)
				return v
			}
			return StrNode(strconv.Itoa(c.localOf[p]) + s[strings.IndexByte(s, '.'):])
		}
	}
	return v
}

// CutRead cuts a whole Read v3.2 analyzeResult into parts numbered Local. A result whose
// readResults are not pages numbered once each, or a plan naming pages it does not have, is an
// error wrapping ErrMalformed, never a panic.
func CutRead(whole []byte, plan [][]int) (parts [][]byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			parts, err = nil, internalError(r)
		}
	}()
	root, err := Parse(whole)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	r := Bare(root)
	if err := validateRead(r, plan); err != nil {
		return nil, err
	}
	for _, pages := range plan {
		out := ObjNode()
		for i, key := range r.Keys {
			v := r.Vals[i]
			if key != "readResults" {
				out.Set(key, v.Clone())
				continue
			}
			arr := ArrNode()
			for _, p := range v.Vals {
				n, _ := p.Get("page").Int()
				if j := slices.Index(pages, n); j >= 0 {
					np := p.Clone()
					np.Set("page", NumNode(j+1))
					arr.Vals = append(arr.Vals, np)
				}
			}
			out.Set(key, arr)
		}
		parts = append(parts, Encode(out))
	}
	return parts, nil
}

// Items is n's items, or none for a nil node.
func Items(n *Node) []*Node { return n.vals() }

// validateRead checks a Read result's readResults and the plan against them.
func validateRead(r *Node, plan [][]int) error {
	var p problems
	if r.Kind != Obj {
		return malformedf("the result is %s, not an object", rawOf(r))
	}
	rr := r.Get("readResults")
	if rr == nil || rr.Kind != Arr {
		return malformedf("readResults is %s, not an array", rawOf(rr))
	}
	pageAt := map[int]int{}
	for i, pg := range rr.Vals {
		n, ok := pg.Get("page").Int()
		if j, dup := pageAt[n]; ok && dup {
			p.addf("readResults[%d] and readResults[%d] are both page %d", j, i, n)
		} else if !ok || n < 1 {
			p.addf("readResults[%d].page %s is not a page number", i, rawOf(pg.Get("page")))
		} else {
			pageAt[n] = i
		}
	}
	checkPlan(plan, func(n int) bool { _, ok := pageAt[n]; return ok }, &p)
	return p.err()
}
