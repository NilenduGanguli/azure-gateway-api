// Package textunit measures text in the units Document Intelligence uses for span offsets.
//
// Every `span.offset` and `span.length` in an AnalyzeResult indexes into its `content`, counted in
// the unit the request named with `stringIndexType`. Merging part results shifts each part's
// offsets by the length of everything before it, so that length must be counted exactly as the
// service counts it: one unit off and every span after the first part points at the wrong text.
package textunit

import (
	"strconv"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// Unit is a stringIndexType: what one step of an offset counts.
type Unit int

const (
	// TextElements counts extended grapheme clusters per UAX #29, as .NET's StringInfo does. It is
	// Azure's default, so a result without a stringIndexType is in this unit.
	TextElements Unit = iota
	// UnicodeCodePoint counts code points, which are Go runes.
	UnicodeCodePoint
	// UTF16CodeUnit counts UTF-16 code units: two for a code point above U+FFFF, one otherwise.
	UTF16CodeUnit
)

// Parse maps a stringIndexType value to its Unit. The empty string is TextElements, because that
// is what the service assumes when the parameter is absent. Unknown values give ok=false.
//
// Matching is exact. The service echoes the canonical spelling in its results, and a caller that
// cannot be sure of the unit must not guess one: a wrong unit silently corrupts every offset.
func Parse(v string) (Unit, bool) {
	switch v {
	case "", "textElements":
		return TextElements, true
	case "unicodeCodePoint":
		return UnicodeCodePoint, true
	case "utf16CodeUnit":
		return UTF16CodeUnit, true
	}
	return 0, false
}

// String returns the unit's stringIndexType value as it appears on the wire.
func (u Unit) String() string {
	switch u {
	case TextElements:
		return "textElements"
	case UnicodeCodePoint:
		return "unicodeCodePoint"
	case UTF16CodeUnit:
		return "utf16CodeUnit"
	}
	return "Unit(" + strconv.Itoa(int(u)) + ")"
}

// Len returns the length of s in u. An unknown unit counts as TextElements, the service default.
//
// Invalid UTF-8 counts one unit per bad byte in every unit, as if each were U+FFFD. Results from
// the service are valid UTF-8, so this only keeps the three counts consistent with each other.
func Len(s string, u Unit) int {
	switch u {
	case UnicodeCodePoint:
		return utf8.RuneCountInString(s)
	case UTF16CodeUnit:
		n := 0
		for _, r := range s {
			if r > 0xFFFF {
				n += 2
			} else {
				n++
			}
		}
		return n
	default:
		return uniseg.GraphemeClusterCount(s)
	}
}
