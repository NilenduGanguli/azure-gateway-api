package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
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
	want := []struct{ name, value string }{
		{"a", `1`}, {"b", `[1,2]`}, {"c", `{"d":"e"}`}, {"s", `"\"q\""`}, {"n", `null`}, {"e", `{}`},
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
