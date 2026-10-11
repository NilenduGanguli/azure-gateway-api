package merge

import (
	"io"
	"strconv"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// Read writes the merged Read analyzeResult object to w.
//
// A Read v3.2 result has no content, spans or cross-references: readResults holds one item per
// page and nothing else refers to them. So the merge concatenates readResults in part order with
// each page mapped to the original, and checks that version and modelVersion agree. As with DI,
// on any error w may hold part of the output.
func Read(w io.Writer, parts []Part) (Stats, error) {
	m := newMerger(readProfile, textunit.TextElements)
	if err := m.measure(parts); err != nil {
		return Stats{}, err
	}
	if err := m.mapPages(); err != nil {
		return Stats{}, err
	}
	for _, p := range m.parts {
		v := &readVisitor{p: p}
		p.visit, p.verr = v.visit, &v.err
	}
	if err := m.write(w); err != nil {
		return Stats{}, err
	}
	return m.stats, nil
}

// readVisitor maps "readResults",i,"page" to the original page.
type readVisitor struct {
	p   *part
	buf []byte
	err error
}

func (v *readVisitor) visit(path []jsonx.PathElem, kind jsonx.Kind, raw []byte) ([]byte, bool) {
	if v.err != nil || len(path) != 3 || !isKey(path[0], "readResults") || !path[1].IsIndex ||
		!isKey(path[2], "page") {
		return nil, false
	}
	n, ok := parseIndex(raw)
	orig, mapped := v.p.mapPage(n)
	if kind != jsonx.Number || !ok || !mapped {
		v.err = checkf("part %d: readResults page %s is not one of the part's pages",
			v.p.k, shown(kind, raw))
		return nil, false
	}
	if orig == n {
		return nil, false
	}
	v.buf = strconv.AppendInt(v.buf[:0], int64(orig), 10)
	return v.buf, true
}
