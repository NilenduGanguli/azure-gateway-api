package azschema

import (
	"fmt"

	"github.com/NilenduGanguli/azure-gateway-api/internal/realcut"
)

// OrderModel is the member order of real outputs, kind of object by kind of object: for each
// pair of members seen together, the order they were seen in.
type OrderModel struct {
	before  map[string]map[[2]string]bool
	example map[string][]string
}

func newOrderModel() *OrderModel {
	return &OrderModel{before: map[string]map[[2]string]bool{}, example: map[string][]string{}}
}

func (m *OrderModel) learn(kind string, o *realcut.Node, _ string) {
	if m.before[kind] == nil {
		m.before[kind] = map[[2]string]bool{}
	}
	for i, a := range o.Keys {
		for _, b := range o.Keys[i+1:] {
			m.before[kind][[2]string{a, b}] = true
		}
	}
	if len(o.Keys) > len(m.example[kind]) {
		m.example[kind] = o.Keys
	}
}

// check reports each pair of members o gives in the order real outputs never do.
func (m *OrderModel) check(kind string, o *realcut.Node, path string) []Finding {
	var out []Finding
	seen := m.before[kind]
	for i, a := range o.Keys {
		for _, b := range o.Keys[i+1:] {
			if seen[[2]string{b, a}] && !seen[[2]string{a, b}] {
				out = append(out, Finding{Error, "key-order", path, fmt.Sprintf(
					"%s: %q before %q; real outputs write %v", kind, a, b, m.example[kind])})
			}
		}
	}
	return out
}

// walkDI shows every object of a DI analyzeResult to visit, with its kind.
func walkDI(r *realcut.Node, visit func(kind string, o *realcut.Node, path string)) {
	if r == nil || r.Kind != realcut.Obj {
		return
	}
	each := func(member, kind string, inner func(o *realcut.Node, path string)) {
		for i, it := range realcut.Items(r.Get(member)) {
			if it.Kind != realcut.Obj {
				continue
			}
			path := fmt.Sprintf("/%s/%d", member, i)
			visit(kind, it, path)
			if inner != nil {
				inner(it, path)
			}
		}
	}
	regions := func(o *realcut.Node, path string) {
		for i, br := range realcut.Items(o.Get("boundingRegions")) {
			if br.Kind == realcut.Obj {
				visit("boundingRegion", br, fmt.Sprintf("%s/boundingRegions/%d", path, i))
			}
		}
	}
	caption := func(o *realcut.Node, path string) {
		if c := o.Get("caption"); c != nil && c.Kind == realcut.Obj {
			visit("caption", c, path+"/caption")
		}
	}
	visit("analyzeResult", r, "")
	each("pages", "page", func(p *realcut.Node, path string) {
		for _, sub := range []struct{ member, kind string }{
			{"words", "word"}, {"lines", "line"}, {"selectionMarks", "selectionMark"}, {"barcodes", "barcode"},
		} {
			for i, it := range realcut.Items(p.Get(sub.member)) {
				if it.Kind == realcut.Obj {
					visit(sub.kind, it, fmt.Sprintf("%s/%s/%d", path, sub.member, i))
					if sp := it.Get("span"); sp != nil && sp.Kind == realcut.Obj {
						visit("span", sp, fmt.Sprintf("%s/%s/%d/span", path, sub.member, i))
					}
				}
			}
		}
	})
	each("paragraphs", "paragraph", regions)
	each("tables", "table", func(t *realcut.Node, path string) {
		regions(t, path)
		caption(t, path)
		for i, c := range realcut.Items(t.Get("cells")) {
			if c.Kind == realcut.Obj {
				visit("cell", c, fmt.Sprintf("%s/cells/%d", path, i))
			}
		}
	})
	each("figures", "figure", func(f *realcut.Node, path string) {
		regions(f, path)
		caption(f, path)
	})
	each("sections", "section", nil)
	each("styles", "style", nil)
}

// walkRead shows every object of a Read analyzeResult to visit, with its kind.
func walkRead(r *realcut.Node, visit func(kind string, o *realcut.Node, path string)) {
	if r == nil || r.Kind != realcut.Obj {
		return
	}
	visit("read.analyzeResult", r, "")
	for i, p := range realcut.Items(r.Get("readResults")) {
		if p.Kind != realcut.Obj {
			continue
		}
		pp := fmt.Sprintf("/readResults/%d", i)
		visit("read.page", p, pp)
		for j, l := range realcut.Items(p.Get("lines")) {
			if l.Kind != realcut.Obj {
				continue
			}
			lp := fmt.Sprintf("%s/lines/%d", pp, j)
			visit("read.line", l, lp)
			if a := l.Get("appearance"); a != nil && a.Kind == realcut.Obj {
				visit("read.appearance", a, lp+"/appearance")
				if s := a.Get("style"); s != nil && s.Kind == realcut.Obj {
					visit("read.style", s, lp+"/appearance/style")
				}
			}
			for k, w := range realcut.Items(l.Get("words")) {
				if w.Kind == realcut.Obj {
					visit("read.word", w, fmt.Sprintf("%s/words/%d", lp, k))
				}
			}
		}
	}
}
