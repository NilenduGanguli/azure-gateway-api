package jsonx

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

// neverReplace visits every scalar and keeps it.
func neverReplace([]PathElem, Kind, []byte) ([]byte, bool) { return nil, false }

// pathString renders a path as comma-separated keys and indexes, e.g. "pages,0,span,offset".
func pathString(path []PathElem) string {
	parts := make([]string, len(path))
	for i, p := range path {
		if p.IsIndex {
			parts[i] = strconv.Itoa(p.Index)
		} else {
			parts[i] = p.Key
		}
	}
	return strings.Join(parts, ",")
}

// rewrite runs Rewrite into a buffer and fails the test on error.
func rewrite(t *testing.T, in string, prefix []PathElem, visit Visitor) string {
	t.Helper()
	var out bytes.Buffer
	if err := Rewrite(&out, strings.NewReader(in), prefix, visit); err != nil {
		t.Fatalf("Rewrite(%s): %v", truncate(in), err)
	}
	return out.String()
}

// identityCorpus covers the shapes that are easy to get subtly wrong when copying byte for byte.
var identityCorpus = []string{
	`{}`,
	`[]`,
	`{"a":{},"b":[],"c":[{}],"d":[[]],"e":{"f":{}}}`,
	`"plain"`,
	`"escapes \" \\ \/ \b \f \n \r \t é é 😀"`,
	`"raw é 😀 👩‍👩‍👧"`,
	`""`,
	`-0.0`, `1e-07`, `12345678901234567890`, `-12.5E+3`, `0`, `-0`, `0.5e-0`, `1E2`,
	`true`, `false`, `null`,
	"  {\n  \"pages\" : [ {\"pageNumber\":1, \"words\" : [ ] } ] ,\r\n\t\"x\":null }  \n",
	" \t\r\n[ 1 ,\n 2\t,3 ] \t\r\n",
	`[[[[[1]]],[[2,[3,[4]]]]]]`,
	`{"nested":{"deeper":{"deepest":[1,{"k":"v"},[true,false,null]]}}}`,
	`[1,-1,1.5,-1.5e10,1E2,"s",true,false,null,{},[]]`,
	`{"a":1,"a\"b":2,"":3,"\\":4}`,
	`{"content":"Line 1\nLine 2 \"quoted\" \\ back","spans":[{"offset":0,"length":6}]}`,
}

func TestRewriteIdentity(t *testing.T) {
	docs := append([]string(nil), identityCorpus...)
	rng := rand.New(rand.NewSource(20261010))
	for i := 0; i < 300; i++ {
		docs = append(docs, randomObject(rng, 0))
	}
	for _, doc := range docs {
		for name, visit := range map[string]Visitor{"never replaces": neverReplace, "nil": nil} {
			if got := rewrite(t, doc, nil, visit); got != doc {
				t.Fatalf("%s visitor changed the bytes:\n in: %q\nout: %q", name, doc, got)
			}
		}
	}
}

// TestRewriteSurvivesTinyReads puts a buffer refill at every byte boundary, inside escapes, names,
// held scalars and spilling strings, and checks that the output does not depend on where the
// reads fell.
func TestRewriteSurvivesTinyReads(t *testing.T) {
	docs := append([]string(nil), identityCorpus...)
	docs = append(docs, `{"k":"`+strings.Repeat(`a\"é`, 1500)+`","n":[1,2.5e3,-0],"t":true}`)
	visit := func(path []PathElem, kind Kind, raw []byte) ([]byte, bool) {
		if kind == Number || kind == Bool {
			return append(append([]byte(`"`), raw...), '"'), true
		}
		return nil, false
	}
	for _, doc := range docs {
		want := rewrite(t, doc, nil, visit)
		for name, r := range map[string]io.Reader{
			"one byte": iotest.OneByteReader(strings.NewReader(doc)),
			"half":     iotest.HalfReader(strings.NewReader(doc)),
			"data+EOF": iotest.DataErrReader(strings.NewReader(doc)),
		} {
			var out bytes.Buffer
			if err := Rewrite(&out, r, nil, visit); err != nil {
				t.Fatalf("%s reads of %s: %v", name, truncate(doc), err)
			}
			if out.String() != want {
				t.Fatalf("%s reads of %s gave %s, want %s",
					name, truncate(doc), truncate(out.String()), truncate(want))
			}
		}
		if trimmed := strings.TrimSpace(doc); strings.HasPrefix(trimmed, "[") {
			var whole, tiny bytes.Buffer
			n1, err1 := ArrayItems(&whole, strings.NewReader(doc), nil, 0, visit)
			n2, err2 := ArrayItems(&tiny, iotest.OneByteReader(strings.NewReader(doc)), nil, 0, visit)
			if err1 != nil || err2 != nil || n1 != n2 || whole.String() != tiny.String() {
				t.Fatalf("ArrayItems with one-byte reads of %s: %d %q %v vs %d %q %v",
					truncate(doc), n2, tiny.String(), err2, n1, whole.String(), err1)
			}
		}
	}
}

// TestRewriteReplacesOnlyTargetedBytes rewrites every offset inside a span object and checks that
// nothing else moved: not the offsets in spans arrays, not a top-level offset, not the word
// "offset" appearing as a value, and not the whitespace around any of them.
func TestRewriteReplacesOnlyTargetedBytes(t *testing.T) {
	in := "{\"pages\":[{\"pageNumber\":1,\"spans\":[{\"offset\":404,\"length\":9}],\n" +
		" \"words\":[{\"content\":\"offset\",\"span\":{\"offset\":101,\"length\":3}},\n" +
		"  {\"span\" : { \"length\":4 , \"offset\" : 202 }}]}],\"offset\":505,\n" +
		"\"span\":{\"offset\":303},\"deep\":[[{\"span\":{\"offset\":606}}]]}"
	want := in
	for _, n := range []string{"101", "202", "303", "606"} {
		want = strings.Replace(want, n, "9"+n, 1)
	}
	visit := func(path []PathElem, kind Kind, raw []byte) ([]byte, bool) {
		n := len(path)
		if kind == Number && n >= 2 && path[n-1].Key == "offset" && !path[n-2].IsIndex &&
			path[n-2].Key == "span" {
			return append([]byte("9"), raw...), true
		}
		return nil, false
	}
	if got := rewrite(t, in, nil, visit); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRewritePaths(t *testing.T) {
	doc := `{"pages":[{"words":[{"span":{"offset":10}},{"span":{"offset":11}},` +
		`{"span":{"offset":12,"length":5}}]}],"z":[true,null,"s"]}`
	type seen struct {
		path string
		kind Kind
	}
	for _, tc := range []struct {
		prefix []PathElem
		lead   string
	}{
		{nil, ""},
		{[]PathElem{{Key: "analyzeResult"}}, "analyzeResult,"},
		{[]PathElem{{Key: "parts"}, {Index: 3, IsIndex: true}}, "parts,3,"},
	} {
		got := map[string]seen{}
		rewrite(t, doc, tc.prefix, func(path []PathElem, kind Kind, raw []byte) ([]byte, bool) {
			got[string(raw)] = seen{pathString(path), kind}
			return nil, false
		})
		want := map[string]seen{
			"10":   {tc.lead + "pages,0,words,0,span,offset", Number},
			"11":   {tc.lead + "pages,0,words,1,span,offset", Number},
			"12":   {tc.lead + "pages,0,words,2,span,offset", Number},
			"5":    {tc.lead + "pages,0,words,2,span,length", Number},
			"true": {tc.lead + "z,0", Bool},
			"null": {tc.lead + "z,1", Null},
			`"s"`:  {tc.lead + "z,2", String},
		}
		for raw, w := range want {
			if got[raw] != w {
				t.Errorf("prefix %q: %s seen as %+v, want %+v", tc.lead, raw, got[raw], w)
			}
		}
		if len(got) != len(want) {
			t.Errorf("prefix %q: visited %d scalars, want %d: %v", tc.lead, len(got), len(want), got)
		}
	}
}

// TestRewriteDecodesEscapedKeys checks that a path holds the member name, not its spelling.
func TestRewriteDecodesEscapedKeys(t *testing.T) {
	var got string
	rewrite(t, `{"span":{"offset":1}}`, nil, func(path []PathElem, _ Kind, _ []byte) ([]byte, bool) {
		got = pathString(path)
		return nil, false
	})
	if got != "span,offset" {
		t.Errorf("path = %q, want span,offset", got)
	}
}

func TestRewriteWritesReplacementsVerbatim(t *testing.T) {
	in := `{"a":"x","b":1,"c":true,"d":null}`
	got := rewrite(t, in, nil, func(path []PathElem, kind Kind, raw []byte) ([]byte, bool) {
		switch path[0].Key {
		case "a":
			return []byte(`{"was":"x"}`), true
		case "b":
			return []byte(`"one"`), true
		case "c":
			return []byte(`false`), true
		}
		return nil, false
	})
	if want := `{"a":{"was":"x"},"b":"one","c":false,"d":null}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestRewriteCopiesLongStringsUnvisited: a string longer than MaxVisitString is streamed through
// intact, and the visitor only hears of it afterwards, as a LongString with no bytes, so that it
// can refuse one where it needs to see the value. Its replacement is ignored.
func TestRewriteCopiesLongStringsUnvisited(t *testing.T) {
	long := strings.Repeat("x", 10<<10)
	// Escapes near and across the limit must not confuse where the string ends.
	escaped := strings.Repeat(`ab\"\\é`, 1200)
	in := `{"a":"` + long + `","b":"short","c":"` + escaped + `","d":[1]}`
	var visited []string
	got := rewrite(t, in, nil, func(path []PathElem, kind Kind, raw []byte) ([]byte, bool) {
		switch kind {
		case LongString:
			if raw != nil {
				t.Errorf("%s: LongString with %d bytes", pathString(path), len(raw))
			}
			visited = append(visited, pathString(path)+"(long)")
			return []byte(`"IGNORED"`), true
		case String:
			visited = append(visited, pathString(path))
			return []byte(`"SHORT"`), true
		}
		visited = append(visited, pathString(path))
		return nil, false
	})
	if want := strings.Replace(in, `"short"`, `"SHORT"`, 1); got != want {
		t.Errorf("long strings were not copied intact")
	}
	if fmt.Sprint(visited) != "[a(long) b c(long) d,0]" {
		t.Errorf("visited %v, want a and c as long strings, b and d,0 as themselves", visited)
	}

	// The limit is on the raw string, quotes included: exactly MaxVisitString bytes is visited.
	for _, tc := range []struct {
		size    int
		visited bool
	}{{MaxVisitString - 1, true}, {MaxVisitString, true}, {MaxVisitString + 1, false}} {
		s := `"` + strings.Repeat("y", tc.size-2) + `"`
		var saw, long bool
		if got := rewrite(t, s, nil, func(_ []PathElem, kind Kind, raw []byte) ([]byte, bool) {
			if kind == LongString {
				long = true
				return nil, false
			}
			saw = true
			if string(raw) != s {
				t.Errorf("size %d: visitor got %d bytes", tc.size, len(raw))
			}
			return nil, false
		}); got != s {
			t.Errorf("size %d: output changed", tc.size)
		}
		if saw != tc.visited || long == tc.visited {
			t.Errorf("raw size %d: visited=%v long=%v, want visited=%v", tc.size, saw, long, tc.visited)
		}
	}
}

func TestRewriteRejectsMalformedJSON(t *testing.T) {
	bad := []string{
		``, ` `, `{`, `{"a"`, `{"a":`, `{"a":1`, `{"a":1,`, `[1,2`, `"abc`, `"a\`, `"\u12`,
		`"\u12G4"`, `tru`, `nul`, `fals`, `trUe`, `-`, `1.`, `1e`, `1e+`, `.5`, `+1`, `01`, `-01`,
		`NaN`, `{"a":1}x`, `[1] [2]`, `{"a" 1}`, `{"a":1,}`, `[1,]`, `[,1]`, `{,}`, `{1:2}`,
		`[01]`, `"\x"`, "\"a\tb\"", "\"a\nb\"", `{"a":1}}`, `[1]]`, `[1 2]`, `{"a":1 "b":2}`,
		`[}`, `{]`, `{"a"}`, `1 2`, `"a" "b"`, `nullx`, `[1,2]x`, "\xef\xbb\xbf{}",
	}
	for _, in := range bad {
		if json.Valid([]byte(in)) {
			t.Fatalf("test bug: %q is valid JSON", in)
		}
		err := Rewrite(io.Discard, strings.NewReader(in), nil, neverReplace)
		if err == nil {
			t.Errorf("Rewrite(%q) succeeded", in)
			continue
		}
		if !errors.Is(err, ErrSyntax) || !strings.HasPrefix(err.Error(), "jsonx: ") {
			t.Errorf("Rewrite(%q) = %v; want a jsonx: ErrSyntax", in, err)
		}
	}
}

func TestRewriteLimits(t *testing.T) {
	deep := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	// encoding/json allows 10000 levels; so does the rewriter.
	if err := Rewrite(io.Discard, strings.NewReader(deep(10000)), nil, neverReplace); err != nil {
		t.Errorf("10000 levels: %v", err)
	}
	for name, in := range map[string]string{
		"nesting":     deep(10001),
		"long number": strings.Repeat("1", MaxVisitString+1),
		"long name":   `{"` + strings.Repeat("k", 5000) + `":1}`,
	} {
		err := Rewrite(io.Discard, strings.NewReader(in), nil, neverReplace)
		if !errors.Is(err, ErrSyntax) {
			t.Errorf("%s: err = %v, want ErrSyntax", name, err)
		}
	}
}

// failingReader yields its data and then an error.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRewritePropagatesIOErrors(t *testing.T) {
	boom := errors.New("boom")
	err := Rewrite(io.Discard, &failingReader{data: []byte(`{"a":[1,2`), err: boom}, nil, neverReplace)
	if !errors.Is(err, boom) || errors.Is(err, ErrSyntax) {
		t.Errorf("read error: got %v, want the reader's error and not ErrSyntax", err)
	}
	// A reader error after a complete value still surfaces: the trailing check reads on.
	err = Rewrite(io.Discard, &failingReader{data: []byte(`{"a":1}`), err: boom}, nil, nil)
	if !errors.Is(err, boom) {
		t.Errorf("read error after the value: got %v", err)
	}
	big := `{"a":"` + strings.Repeat("z", 100<<10) + `"}`
	err = Rewrite(failingWriter{boom}, strings.NewReader(big), nil, neverReplace)
	if !errors.Is(err, boom) {
		t.Errorf("write error: got %v, want the writer's error", err)
	}
}

func TestArrayItems(t *testing.T) {
	cases := []struct {
		name, in, want string
		n              int
	}{
		{"empty", `[]`, ``, 0},
		{"empty with space", " [ \n ] ", ``, 0},
		{"two", `[1,2]`, `1,2`, 2},
		{"spaced", `[ 1 , 2 ]`, `1,2`, 2},
		{"items keep their own bytes",
			"[ {\"a\" : 1} ,\n[ 2 , 3 ] ,\"s\"\t]", "{\"a\" : 1},[ 2 , 3 ],\"s\"", 3},
		{"one", `[{"x":[]}]`, `{"x":[]}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			n, err := ArrayItems(&out, strings.NewReader(tc.in), nil, 0, neverReplace)
			if err != nil {
				t.Fatalf("ArrayItems: %v", err)
			}
			if n != tc.n || out.String() != tc.want {
				t.Errorf("got %d items %q, want %d items %q", n, out.String(), tc.n, tc.want)
			}
		})
	}
}

func TestArrayItemsNumbersFromBase(t *testing.T) {
	var paths []string
	var out bytes.Buffer
	in := `[{"x":1},{"x":2},3]`
	n, err := ArrayItems(&out, strings.NewReader(in), []PathElem{{Key: "pages"}}, 10,
		func(path []PathElem, _ Kind, raw []byte) ([]byte, bool) {
			paths = append(paths, pathString(path)+"="+string(raw))
			return nil, false
		})
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := strings.Join(paths, " "); got != "pages,10,x=1 pages,11,x=2 pages,12=3" {
		t.Errorf("paths %s", got)
	}
}

func TestArrayItemsRejects(t *testing.T) {
	for _, in := range []string{`{}`, `"[1]"`, `[1,2`, `[1,]`, `[1] x`, `[1][2]`, ``} {
		_, err := ArrayItems(io.Discard, strings.NewReader(in), nil, 0, nil)
		if !errors.Is(err, ErrSyntax) {
			t.Errorf("ArrayItems(%q) err = %v, want ErrSyntax", in, err)
		}
	}
}

// TestBufferedWriterIsUsedDirectly checks that a *bufio.Writer is written to without another layer
// of buffering, and is left for the caller to flush like any other writer would be.
func TestBufferedWriterIsUsedDirectly(t *testing.T) {
	var out bytes.Buffer
	bw := bufio.NewWriter(&out)
	if err := Rewrite(bw, strings.NewReader(`{"a":1}`), nil, nil); err != nil {
		t.Fatal(err)
	}
	if bw.Buffered() != len(`{"a":1}`) || out.Len() != 0 {
		t.Errorf("buffered=%d written=%d; want everything still in the caller's buffer",
			bw.Buffered(), out.Len())
	}
}

// TestRewriteAgreesWithEncodingJSON checks validity against encoding/json on random documents and
// random corruptions of them, and that ArrayItems and CountItems agree with a decode into
// []json.RawMessage, whose elements are each item's exact bytes.
func TestRewriteAgreesWithEncodingJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 3000; i++ {
		doc := []byte(randomValue(rng, 0))
		if rng.Intn(4) == 0 {
			doc = []byte(pad(rng) + "[" + randomValue(rng, 1) + pad(rng) + "," + pad(rng) +
				randomValue(rng, 1) + pad(rng) + "]" + pad(rng))
		}
		switch rng.Intn(4) {
		case 1:
			doc = doc[:rng.Intn(len(doc)+1)]
		case 2:
			at := rng.Intn(len(doc) + 1)
			const noise = "{}[],:\"\\ 0-e.tn\x01"
			doc = append(doc[:at:at], append([]byte{noise[rng.Intn(len(noise))]}, doc[at:]...)...)
		case 3:
			if len(doc) > 0 {
				at := rng.Intn(len(doc))
				doc = append(doc[:at:at], doc[at+1:]...)
			}
		}
		checkAgainstEncodingJSON(t, doc)
	}
}

func checkAgainstEncodingJSON(t *testing.T, doc []byte) {
	t.Helper()
	valid := json.Valid(doc)
	var out bytes.Buffer
	err := Rewrite(&out, bytes.NewReader(doc), nil, neverReplace)
	if valid != (err == nil) {
		t.Fatalf("json.Valid=%v but Rewrite err=%v for %q", valid, err, doc)
	}
	if err != nil {
		return
	}
	if !bytes.Equal(out.Bytes(), doc) {
		t.Fatalf("identity failed for %q: %q", doc, out.Bytes())
	}
	// Only arrays: encoding/json also unmarshals null into a slice.
	var items []json.RawMessage
	lead := bytes.TrimLeft(doc, " \t\r\n")
	if len(lead) == 0 || lead[0] != '[' || json.Unmarshal(doc, &items) != nil {
		return
	}
	raws := make([][]byte, len(items))
	for i, it := range items {
		raws[i] = it
	}
	out.Reset()
	n, err := ArrayItems(&out, bytes.NewReader(doc), nil, 0, neverReplace)
	if err != nil || n != len(items) || !bytes.Equal(out.Bytes(), bytes.Join(raws, []byte(","))) {
		t.Fatalf("ArrayItems(%q) = %d, %q, %v; want %d, %q", doc, n, out.Bytes(), err,
			len(items), bytes.Join(raws, []byte(",")))
	}
	if c, err := CountItems(bytes.NewReader(doc), 0, int64(len(doc))); err != nil || c != len(items) {
		t.Fatalf("CountItems(%q) = %d, %v; want %d", doc, c, err, len(items))
	}
}

func FuzzRewrite(f *testing.F) {
	for _, doc := range identityCorpus {
		f.Add([]byte(doc))
	}
	f.Add([]byte(`[1,2`))
	f.Add([]byte(`{"a":[1,{"b":"é"}]} x`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		if len(doc) > MaxVisitString {
			t.Skip("longer inputs can hit the documented name and number limits")
		}
		checkAgainstEncodingJSON(t, doc)
	})
}

// synthDocument builds a Document Intelligence-shaped JSON document of about size bytes: pages of
// words with polygons, confidences and spans, plus a content string.
func synthDocument(size int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"apiVersion":"2024-11-30","content":"`)
	b.WriteString(strings.Repeat("word ", size/50))
	b.WriteString(`","pages":[`)
	offset := 0
	for p := 0; b.Len() < size; p++ {
		if p > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"pageNumber":%d,"angle":0,"width":8.5,"height":11,"unit":"inch","words":[`, p+1)
		for w := 0; w < 200; w++ {
			if w > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"content":"word","polygon":[%d.25,1.5,%d.75,1.5,%d.75,2,%d.25,2],`+
				`"confidence":0.987,"span":{"offset":%d,"length":4}}`, w, w, w, w, offset)
			offset += 5
		}
		fmt.Fprintf(&b, `],"spans":[{"offset":%d,"length":1000}]}`, offset-1000)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// shiftOffsets is the kind of visitor the merge uses: it rewrites span offsets with a reused
// buffer, so the visitor itself allocates nothing.
type shiftOffsets struct {
	buf   []byte
	delta int // bytes the output grows by
}

func (s *shiftOffsets) visit(path []PathElem, kind Kind, raw []byte) ([]byte, bool) {
	n := len(path)
	if kind != Number || n < 2 || path[n-1].Key != "offset" {
		return nil, false
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil, false
	}
	s.buf = strconv.AppendInt(s.buf[:0], v+1000000, 10)
	s.delta += len(s.buf) - len(raw)
	return s.buf, true
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// TestRewriteMemoryIsBounded is the memory claim: rewriting is a scan with a few fixed buffers, so
// a 10 MB document costs the same few allocations as a small one. A rewriter that materialised
// the document or its scalars would allocate at least the document's size.
func TestRewriteMemoryIsBounded(t *testing.T) {
	doc := synthDocument(10 << 20)
	s := &shiftOffsets{buf: make([]byte, 0, 32)}
	out := &countingWriter{}
	visit := s.visit // a method value allocates; take it outside the measurement

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := Rewrite(out, bytes.NewReader(doc), nil, visit)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(doc) + s.delta); out.n != want {
		t.Fatalf("wrote %d bytes, want %d", out.n, want)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 4<<20 {
		t.Errorf("rewriting %d MB allocated %d KB; the rewriter must not grow with the input",
			len(doc)>>20, allocated>>10)
	}
	t.Logf("rewrote %d MB, allocated %d KB", len(doc)>>20, allocated>>10)
}

// BenchmarkRewrite100MB rewrites a 100 MB document, and fails if a rewrite allocates 64 MiB or
// more. TestRewriteMemoryIsBounded holds every test run to a tighter bound on 10 MB.
func BenchmarkRewrite100MB(b *testing.B) {
	doc := synthDocument(100 << 20)
	s := &shiftOffsets{buf: make([]byte, 0, 32)}
	visit := s.visit
	out := &countingWriter{}
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Rewrite(out, bytes.NewReader(doc), nil, visit); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	runtime.ReadMemStats(&after)
	if perOp := (after.TotalAlloc - before.TotalAlloc) / uint64(b.N); perOp >= 64<<20 {
		b.Fatalf("rewriting %d MB allocated %d MB", len(doc)>>20, perOp>>20)
	}
}
