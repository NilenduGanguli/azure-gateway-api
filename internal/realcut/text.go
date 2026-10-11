package realcut

import (
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// text is a JSON string's content with a map from offsets in a unit to positions in the raw
// (escaped) body, so a part's content can be sliced out of the service's own bytes.
type text struct {
	unit    textunit.Unit
	raw     []byte // the string body, between the quotes, as written
	decoded string
	rawAt   []int // rawAt[i] is the raw position of the escape or byte that produced decoded byte i
	unitAt  []int // unitAt[u] is the decoded byte offset where unit u starts; unitAt[len] = len(decoded)
}

// newText decodes a JSON string scalar, quotes included. A malformed one is an error, never a
// panic: a string that is not quoted, a backslash with nothing after it, a \u with fewer than four
// hex digits.
func newText(quoted []byte, u textunit.Unit) (*text, error) {
	if len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
		return nil, malformedf("content is not a JSON string")
	}
	t := &text{unit: u, raw: quoted[1 : len(quoted)-1]}
	var dec []byte
	r := t.raw
	hex4 := func(i int) (rune, bool) { // the four hex digits of the \u escape at i
		if i+6 > len(r) {
			return 0, false
		}
		v, err := strconv.ParseUint(string(r[i+2:i+6]), 16, 32)
		return rune(v), err == nil
	}
	for i := 0; i < len(r); {
		start := i
		if r[i] != '\\' {
			dec = append(dec, r[i])
			t.rawAt = append(t.rawAt, start)
			i++
			continue
		}
		if i+1 >= len(r) {
			return nil, malformedf("content: unfinished escape at byte %d", i)
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
			ru, ok := hex4(i)
			if !ok {
				return nil, malformedf("content: bad \\u escape at byte %d", i)
			}
			i += 6
			if utf16.IsSurrogate(ru) && i+1 < len(r) && r[i] == '\\' && r[i+1] == 'u' {
				lo, ok := hex4(i)
				if !ok {
					return nil, malformedf("content: bad \\u escape at byte %d", i)
				}
				ru = utf16.DecodeRune(ru, lo)
				i += 6
			}
			out = utf8.AppendRune(nil, ru)
			for range out {
				t.rawAt = append(t.rawAt, start)
			}
			dec = append(dec, out...)
			continue
		default:
			return nil, malformedf("content: bad escape at byte %d", i)
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

// checkRange says why units [a, b) cannot be cut out of the content, or nil when they can: both
// ends within it, in order, and on a cut point (not inside a surrogate pair).
func (t *text) checkRange(a, b int) error {
	n := t.units()
	switch {
	case a < 0 || b < 0:
		return malformedf("units [%d,%d): negative", a, b)
	case a > n || b > n:
		return malformedf("units [%d,%d) run past the content's %d %s", a, b, n, t.unit)
	case a > b:
		return malformedf("units [%d,%d) end before they start", a, b)
	case t.unitAt[a] < 0 || t.unitAt[b] < 0:
		return malformedf("units [%d,%d) cut inside a surrogate pair", a, b)
	}
	return nil
}

// rawSlice is the raw body of units [a, b).
func (t *text) rawSlice(a, b int) ([]byte, error) {
	if err := t.checkRange(a, b); err != nil {
		return nil, err
	}
	return t.raw[t.rawAt[t.unitAt[a]]:t.rawAt[t.unitAt[b]]], nil
}

// str is the decoded text of units [a, b).
func (t *text) str(a, b int) (string, error) {
	if err := t.checkRange(a, b); err != nil {
		return "", err
	}
	return t.decoded[t.unitAt[a]:t.unitAt[b]], nil
}

// unitOf is the unit that starts at decoded byte off.
func (t *text) unitOf(off int) (int, error) {
	if off >= 0 {
		// unitAt ascends but for the -1 of each low surrogate, so it is scanned, not searched.
		for i, at := range t.unitAt {
			if at == off {
				return i, nil
			}
			if at > off {
				break
			}
		}
	}
	return 0, malformedf("byte %d of the content is not the start of a %s unit", off, t.unit)
}
