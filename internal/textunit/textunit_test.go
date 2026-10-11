package textunit

import (
	"strings"
	"testing"
)

func TestLen(t *testing.T) {
	cases := []struct {
		name                     string
		s                        string
		elements, points, utf16s int
	}{
		{"ascii", "abc", 3, 3, 3},
		{"decomposed e acute", "é", 1, 2, 2},
		{"precomposed e acute", "é", 1, 1, 1},
		{"zwj family", "\U0001F469\u200d\U0001F469\u200d\U0001F467", 1, 5, 8},
		{"flag", "\U0001F1EE\U0001F1F3", 1, 2, 4},
		{"newline", "\n", 1, 1, 1},
		{"crlf is one element", "\r\n", 1, 2, 2},
		{"empty", "", 0, 0, 0},
		{"mixed line", "Line 3.1 naïve \U0001F469\u200d\U0001F469\u200d\U0001F467 text", 21, 25, 28},
		// Invalid UTF-8 decodes as U+FFFD, one unit per bad byte in every unit, as Go and .NET do
		// when they meet it in a string.
		{"invalid byte", "a\xffb", 3, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, u := range []struct {
				unit Unit
				want int
			}{
				{TextElements, tc.elements},
				{UnicodeCodePoint, tc.points},
				{UTF16CodeUnit, tc.utf16s},
			} {
				if got := Len(tc.s, u.unit); got != u.want {
					t.Errorf("Len(%q, %v) = %d, want %d", tc.s, u.unit, got, u.want)
				}
			}
		})
	}
}

// TestLenIsAdditiveAcrossNewlines is the property the merge relies on: a part's offsets are shifted
// by the length of everything before it, which is only right if the length of a concatenation
// joined by "\n" is the sum of the lengths. A newline is a control, so no grapheme cluster
// straddles it — except CR LF, which the merge refuses separately.
func TestLenIsAdditiveAcrossNewlines(t *testing.T) {
	pieces := []string{
		"abc",
		"́ starts with a combining mark",
		"ends with a zwj \u200d",
		"\U0001F1EE",
		"\U0001F1F3 a lone regional indicator each side",
		"\U0001F469\u200d\U0001F469\u200d\U0001F467",
		"",
	}
	for _, u := range []Unit{TextElements, UnicodeCodePoint, UTF16CodeUnit} {
		sum := 0
		for i, p := range pieces {
			if i > 0 {
				sum += Len("\n", u)
			}
			sum += Len(p, u)
		}
		if got := Len(strings.Join(pieces, "\n"), u); got != sum {
			t.Errorf("%v: Len(joined) = %d, sum of parts = %d", u, got, sum)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Unit
		ok   bool
	}{
		{"", TextElements, true},
		{"textElements", TextElements, true},
		{"unicodeCodePoint", UnicodeCodePoint, true},
		{"utf16CodeUnit", UTF16CodeUnit, true},
		{"utf8CodeUnit", 0, false},
		{"TextElements", 0, false},
		{" textElements", 0, false},
	}
	for _, tc := range cases {
		got, ok := Parse(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("Parse(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestStringRoundTrips checks that String gives back the wire value Parse accepts, since docgen
// writes it into stringIndexType and merge compares the two.
func TestStringRoundTrips(t *testing.T) {
	for _, u := range []Unit{TextElements, UnicodeCodePoint, UTF16CodeUnit} {
		back, ok := Parse(u.String())
		if !ok || back != u {
			t.Errorf("Parse(%q) = %v, %v; want %v", u.String(), back, ok, u)
		}
	}
	if got := Unit(99).String(); got != "Unit(99)" {
		t.Errorf("Unit(99).String() = %q", got)
	}
}
