package azschema

import (
	"github.com/rivo/uniseg"

	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// text is a DI result's content, indexed in its stringIndexType.
type text struct {
	s      string
	bounds []int // bounds[i] is the byte offset where unit i starts, -1 inside a surrogate pair; the last is len(s)
}

func newText(s string, u textunit.Unit) *text {
	t := &text{s: s}
	switch u {
	case textunit.UnicodeCodePoint:
		for i := range s {
			t.bounds = append(t.bounds, i)
		}
	case textunit.UTF16CodeUnit:
		for i, r := range s {
			t.bounds = append(t.bounds, i)
			if r > 0xFFFF {
				t.bounds = append(t.bounds, -1) // the low surrogate: not a boundary
			}
		}
	default:
		g := uniseg.NewGraphemes(s)
		for g.Next() {
			from, _ := g.Positions()
			t.bounds = append(t.bounds, from)
		}
	}
	t.bounds = append(t.bounds, len(s))
	return t
}

// len is the content's length in the unit.
func (t *text) len() int { return len(t.bounds) - 1 }

// slice is the text of units [off, off+n), and false when that is not a whole range of units
// inside the content.
func (t *text) slice(off, n int) (string, bool) {
	if off < 0 || n < 0 || off+n > t.len() || t.bounds[off] < 0 || t.bounds[off+n] < 0 {
		return "", false
	}
	return t.s[t.bounds[off]:t.bounds[off+n]], true
}
