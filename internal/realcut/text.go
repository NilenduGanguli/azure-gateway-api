package realcut

import (
	"fmt"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// text is a JSON string's content with a map from offsets in a unit to positions in the raw
// (escaped) body, so a part's content can be sliced out of the service's own bytes.
type text struct {
	raw     []byte // the string body, between the quotes, as written
	decoded string
	rawAt   []int // rawAt[i] is the raw position of the escape or byte that produced decoded byte i
	unitAt  []int // unitAt[u] is the decoded byte offset where unit u starts; unitAt[len] = len(decoded)
}

func newText(quoted []byte, u textunit.Unit) (*text, error) {
	t := &text{raw: quoted[1 : len(quoted)-1]}
	var dec []byte
	r := t.raw
	for i := 0; i < len(r); {
		start := i
		if r[i] != '\\' {
			dec = append(dec, r[i])
			t.rawAt = append(t.rawAt, start)
			i++
			continue
		}
		var out []byte
		switch r[i+1] {
		case 'n':
			out = []byte{'\n'}
		case 't':
			out = []byte{'\t'}
		case 'r':
			out = []byte{'\r'}
		case 'b':
			out = []byte{'\b'}
		case 'f':
			out = []byte{'\f'}
		case '"', '\\', '/':
			out = []byte{r[i+1]}
		case 'u':
			v, err := strconv.ParseUint(string(r[i+2:i+6]), 16, 32)
			if err != nil {
				return nil, fmt.Errorf("bad escape at %d", i)
			}
			ru := rune(v)
			i += 6
			if utf16.IsSurrogate(ru) && i+6 <= len(r) && r[i] == '\\' && r[i+1] == 'u' {
				v2, _ := strconv.ParseUint(string(r[i+2:i+6]), 16, 32)
				ru = utf16.DecodeRune(ru, rune(v2))
				i += 6
			}
			out = utf8.AppendRune(nil, ru)
			for range out {
				t.rawAt = append(t.rawAt, start)
			}
			dec = append(dec, out...)
			continue
		default:
			return nil, fmt.Errorf("bad escape at %d", i)
		}
		dec = append(dec, out...)
		t.rawAt = append(t.rawAt, start)
		i += 2
	}
	t.rawAt = append(t.rawAt, len(r))
	t.decoded = string(dec)

	s := t.decoded
	switch u {
	case textunit.UnicodeCodePoint:
		for i := range s {
			t.unitAt = append(t.unitAt, i)
		}
	case textunit.UTF16CodeUnit:
		for i, ru := range s {
			t.unitAt = append(t.unitAt, i)
			if ru > 0xFFFF {
				t.unitAt = append(t.unitAt, -1) // the low surrogate: not a cut point
			}
		}
	default:
		g := uniseg.NewGraphemes(s)
		for g.Next() {
			a, _ := g.Positions()
			t.unitAt = append(t.unitAt, a)
		}
	}
	t.unitAt = append(t.unitAt, len(s))
	return t, nil
}

// units is the content's length in the unit.
func (t *text) units() int { return len(t.unitAt) - 1 }

// rawSlice is the raw body of units [a, b).
func (t *text) rawSlice(a, b int) []byte {
	return t.raw[t.rawAt[t.unitAt[a]]:t.rawAt[t.unitAt[b]]]
}

// str is the decoded text of units [a, b).
func (t *text) str(a, b int) string { return t.decoded[t.unitAt[a]:t.unitAt[b]] }
