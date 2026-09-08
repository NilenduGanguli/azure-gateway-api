package jsonx

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValueRangeExtractsMemberExactly(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		key  string
		want string
	}{
		{"object member", `{"a":1,"analyzeResult":{"x":[1,2,{"y":"z"}]},"b":2}`, "analyzeResult", `{"x":[1,2,{"y":"z"}]}`},
		{"first member", `{"analyzeResult":{"k":true},"tail":0}`, "analyzeResult", `{"k":true}`},
		{"last member", `{"head":0,"analyzeResult":[1,2,3]}`, "analyzeResult", `[1,2,3]`},
		{"string member", `{"status":"succeeded","other":1}`, "status", `"succeeded"`},
		{"whitespace around colon", "{\"status\" \t:\n \"running\"}", "status", `"running"`},
		{"key appears nested first", `{"a":{"status":"inner"},"status":"outer"}`, "status", `"outer"`},
		{"escaped braces in strings", `{"s":"}{\"","status":"ok"}`, "status", `"ok"`},
		{"number member", `{"n":-12.5e3,"z":1}`, "n", `-12.5e3`},
		{"null member", `{"n":null}`, "n", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := strings.NewReader(tc.doc)
			start, end, found, err := ValueRange(r, int64(len(tc.doc)), tc.key)
			if err != nil {
				t.Fatalf("ValueRange: %v", err)
			}
			if !found {
				t.Fatalf("member %q not found", tc.key)
			}
			got := tc.doc[start:end]
			if got != tc.want {
				t.Errorf("extracted %q, want %q", got, tc.want)
			}
			// Whatever comes out must be valid JSON on its own, since it is spliced into a new
			// envelope verbatim.
			if !json.Valid([]byte(got)) {
				t.Errorf("extracted range is not valid JSON: %q", got)
			}
		})
	}
}

func TestValueRangeMissingMember(t *testing.T) {
	doc := `{"a":1}`
	_, _, found, err := ValueRange(strings.NewReader(doc), int64(len(doc)), "analyzeResult")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("reported a member that is not present")
	}
}

func TestValueRangeRejectsNonObject(t *testing.T) {
	doc := `[1,2,3]`
	_, _, _, err := ValueRange(strings.NewReader(doc), int64(len(doc)), "x")
	if err == nil {
		t.Fatal("expected an error for a non-object top level")
	}
}

func TestIsObject(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{"  \n\t{", true},
		{`[1]`, false},
		{`"str"`, false},
		{``, false},
	} {
		if got := IsObject([]byte(tc.in)); got != tc.want {
			t.Errorf("IsObject(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestValueRangeSurvivesLongSeparatorWhitespace is a regression test.
//
// The separator scan once used a fixed 64-byte lookahead. JSON permits unlimited whitespace
// between a member name and its value, so a pretty-printed response with more than that produced
// a range still containing the colon — and the envelope composed around it was invalid JSON that
// no client could parse.
func TestValueRangeSurvivesLongSeparatorWhitespace(t *testing.T) {
	for _, ws := range []int{0, 1, 63, 64, 65, 100, 2000} {
		pad := strings.Repeat(" ", ws)
		for _, doc := range []string{
			`{"analyzeResult"` + pad + `:{"a":1}}`, // whitespace before the colon
			`{"analyzeResult":` + pad + `{"a":1}}`, // whitespace after the colon
			`{"analyzeResult"` + pad + `:` + pad + `{"a":1}}`,
		} {
			start, end, found, err := ValueRange(strings.NewReader(doc), int64(len(doc)), "analyzeResult")
			if err != nil || !found {
				t.Fatalf("ws=%d: found=%v err=%v", ws, found, err)
			}
			got := doc[start:end]
			if got != `{"a":1}` {
				t.Errorf("ws=%d: extracted %q, want %q", ws, got, `{"a":1}`)
			}
			// The composed envelope is what a client actually receives.
			composed := `{"status":"succeeded","analyzeResult":` + got + `}`
			if !json.Valid([]byte(composed)) {
				t.Errorf("ws=%d: composed envelope is not valid JSON: %q", ws, composed)
			}
		}
	}
}

// TestValueRangeIgnoresNestedAndQuotedOccurrences checks that only a genuine top-level member
// matches, not the same name nested inside an earlier value or appearing inside a string.
func TestValueRangeIgnoresNestedAndQuotedOccurrences(t *testing.T) {
	for _, doc := range []string{
		`{"a":{"analyzeResult":"NESTED"},"analyzeResult":{"real":1}}`,
		`{"a":"\"analyzeResult\":{}","analyzeResult":{"real":1}}`,
		`{"a":[{"analyzeResult":0}],"analyzeResult":{"real":1}}`,
	} {
		start, end, found, err := ValueRange(strings.NewReader(doc), int64(len(doc)), "analyzeResult")
		if err != nil || !found {
			t.Fatalf("found=%v err=%v for %s", found, err, doc)
		}
		if got := doc[start:end]; got != `{"real":1}` {
			t.Errorf("extracted %q from %s, want the top-level member", got, doc)
		}
	}
}
