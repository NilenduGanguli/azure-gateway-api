package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// readRange returns ra[start:end] as a string.
func readRange(t *testing.T, ra io.ReaderAt, start, end int64) string {
	t.Helper()
	b := make([]byte, end-start)
	if _, err := ra.ReadAt(b, start); err != nil {
		t.Fatalf("ReadAt(%d, %d): %v", start, end, err)
	}
	return string(b)
}

func TestMembersRangesRoundTrip(t *testing.T) {
	obj := " {\"a\" : 1 , \"b\":[1,2],\n\"c\":{\"d\":\"e\"}," +
		"\"s\":\"\\\"q\\\"\",\"\\u006e\":null,\"e\":{}} \n"
	// Embed the object between unrelated bytes, as a member range inside a larger file would be.
	buf := []byte("xxxx" + obj + "yyyy")
	ra := bytes.NewReader(buf)
	start, end := int64(4), int64(4+len(obj))

	ms, err := Members(ra, start, end)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	want := []struct {
		name, value string
		items       int
	}{
		{"a", `1`, -1}, {"b", `[1,2]`, 2}, {"c", `{"d":"e"}`, -1}, {"s", `"\"q\""`, -1}, {"n", `null`, -1},
		{"e", `{}`, -1},
	}
	if len(ms) != len(want) {
		t.Fatalf("got %d members %+v, want %d", len(ms), ms, len(want))
	}
	for i, w := range want {
		if ms[i].Name != w.name {
			t.Errorf("member %d name %q, want %q", i, ms[i].Name, w.name)
		}
		if got := readRange(t, ra, ms[i].Start, ms[i].End); got != w.value {
			t.Errorf("member %q range [%d,%d) = %q, want %q", w.name, ms[i].Start, ms[i].End, got, w.value)
		}
		if ms[i].Items != w.items {
			t.Errorf("member %q has %d items, want %d", w.name, ms[i].Items, w.items)
		}
	}

	empty, err := Members(strings.NewReader(` {} `), 0, 4)
	if err != nil || len(empty) != 0 {
		t.Errorf("Members({}) = %v, %v", empty, err)
	}
}

// TestMembersAgreeWithEncodingJSON checks the ranges against encoding/json's decoder offsets, the
// same oracle the existing scanner is held to.
func TestMembersAgreeWithEncodingJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 500; i++ {
		doc := randomObject(rng, 0)
		ms, err := Members(strings.NewReader(doc), 0, int64(len(doc)))
		if err != nil {
			t.Fatalf("Members(%s): %v", truncate(doc), err)
		}
		dec := json.NewDecoder(strings.NewReader(doc))
		if _, err := dec.Token(); err != nil {
			t.Fatal(err)
		}
		for j := 0; dec.More(); j++ {
			tok, _ := dec.Token()
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				t.Fatal(err)
			}
			if j >= len(ms) {
				t.Fatalf("Members missed %q in %s", tok, truncate(doc))
			}
			if ms[j].Name != tok.(string) || doc[ms[j].Start:ms[j].End] != string(raw) {
				t.Fatalf("member %d of %s: got %q=%q, want %q=%q", j, truncate(doc), ms[j].Name,
					doc[ms[j].Start:ms[j].End], tok, raw)
			}
			if dec.InputOffset() != ms[j].End {
				t.Fatalf("member %q ends at %d, decoder says %d", tok, ms[j].End, dec.InputOffset())
			}
			items := -1
			var arr []json.RawMessage
			if raw[0] == '[' && json.Unmarshal(raw, &arr) == nil {
				items = len(arr)
			}
			if ms[j].Items != items {
				t.Fatalf("member %q of %s has %d items, want %d", tok, truncate(doc), ms[j].Items, items)
			}
		}
	}
}

// TestMembersReportEscapedNames: a name written with any escape decodes to bytes that are not
// its raw ones, which a caller copying names must know. Escapes in values do not count.
func TestMembersReportEscapedNames(t *testing.T) {
	doc := `{"plain":"\u0041","\u0062":1,"c\"d":2,"e\/":3,"f\\":4,"naïve":5}`
	ms, err := Members(strings.NewReader(doc), 0, int64(len(doc)))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name    string
		escaped bool
	}{{"plain", false}, {"b", true}, {`c"d`, true}, {"e/", true}, {`f\`, true}, {"naïve", false}}
	if len(ms) != len(want) {
		t.Fatalf("got %d members, want %d", len(ms), len(want))
	}
	for i, w := range want {
		if ms[i].Name != w.name || ms[i].Escaped != w.escaped {
			t.Errorf("member %d: name %q escaped %v, want %q and %v",
				i, ms[i].Name, ms[i].Escaped, w.name, w.escaped)
		}
	}
}

func TestMembersRejects(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want error
	}{
		{`[1]`, ErrNotObject},
		{`"s"`, ErrNotObject},
		{`{"a":1`, ErrSyntax},
		{`{"a":1}x`, ErrSyntax},
		{`{"a" 1}`, ErrSyntax},
		{`{"a":[1,}`, ErrSyntax},
		{``, ErrSyntax},
	} {
		_, err := Members(strings.NewReader(tc.in), 0, int64(len(tc.in)))
		if !errors.Is(err, tc.want) {
			t.Errorf("Members(%q) err = %v, want %v", tc.in, err, tc.want)
		}
	}
	if _, err := Members(strings.NewReader(`{}`), 2, 1); err == nil {
		t.Error("an inverted range was accepted")
	}
}

// TestVisitMembers: the members chosen are walked with their visitor, paths starting at the
// member's name, and nothing else is shown to it.
func TestVisitMembers(t *testing.T) {
	doc := `{"a":[{"n":1}, {"n":"x","m":[true]}],"b":{"n":3},"c":[5],"d":"` +
		strings.Repeat("y", MaxVisitString) + `","e":[]}`
	var seen []string
	visit := func(path []PathElem, kind Kind, raw []byte) ([]byte, bool) {
		seen = append(seen, pathString(path)+"="+string(raw))
		return []byte(`"replaced"`), true // nothing is written, so this is ignored
	}
	var asked []string
	ms, err := VisitMembers(strings.NewReader(doc), 0, int64(len(doc)), func(name string) Visitor {
		asked = append(asked, name)
		if name == "a" || name == "d" {
			return visit
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(asked, " "); got != "a b c d e" {
		t.Errorf("asked for %q, want every member once", got)
	}
	if got := strings.Join(seen, " "); got != `a,0,n=1 a,1,n="x" a,1,m,0=true d=` {
		t.Errorf("visited %s", got)
	}
	var items []int
	for _, m := range ms {
		items = append(items, m.Items)
	}
	if fmt.Sprint(items) != "[2 -1 1 -1 0]" {
		t.Errorf("items %v, want [2 -1 1 -1 0]", items)
	}
	if got := readRange(t, strings.NewReader(doc), ms[0].Start, ms[0].End); got != `[{"n":1}, {"n":"x","m":[true]}]` {
		t.Errorf("range of a: %q", got)
	}

	// Errors inside a visited member are reported like any other.
	bad := `{"a":[{"n":1},]}`
	if _, err := VisitMembers(strings.NewReader(bad), 0, int64(len(bad)),
		func(string) Visitor { return visit }); !errors.Is(err, ErrSyntax) {
		t.Errorf("VisitMembers(%s) err = %v, want ErrSyntax", bad, err)
	}
}

func TestCountItems(t *testing.T) {
	for _, tc := range []struct {
		in string
		n  int
	}{
		{`[]`, 0},
		{` [ ] `, 0},
		{`[1]`, 1},
		{` [ 1 , [2,3] , {"a":[4,5,6]} ,"x,y"] `, 4},
		{`[[],[],{}]`, 3},
	} {
		buf := []byte("--" + tc.in + "--")
		n, err := CountItems(bytes.NewReader(buf), 2, int64(2+len(tc.in)))
		if err != nil || n != tc.n {
			t.Errorf("CountItems(%q) = %d, %v; want %d", tc.in, n, err, tc.n)
		}
	}
	for _, in := range []string{`{}`, `[1,2`, `[1,]`, `1`, `[1]]`} {
		if _, err := CountItems(strings.NewReader(in), 0, int64(len(in))); !errors.Is(err, ErrSyntax) {
			t.Errorf("CountItems(%q) err = %v, want ErrSyntax", in, err)
		}
	}
}
