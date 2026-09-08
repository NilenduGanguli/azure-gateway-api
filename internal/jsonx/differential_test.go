package jsonx

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

// referenceRange is the encoding/json implementation this package used to use. It is correct but
// materialises every scalar, which is why it was replaced — it stays here as the oracle the byte
// scanner is checked against.
func referenceRange(doc string, key string) (start, end int64, found bool, err error) {
	dec := json.NewDecoder(strings.NewReader(doc))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false, ErrNotObject
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return 0, 0, false, err
		}
		name, ok := kt.(string)
		if !ok {
			return 0, 0, false, fmt.Errorf("member name was %T", kt)
		}
		afterName := dec.InputOffset()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, false, err
		}
		valueEnd := dec.InputOffset()
		if name == key {
			// Advance past the colon and whitespace to the value's first byte.
			i := afterName
			for i < valueEnd {
				c := doc[i]
				if c == ':' || c == ' ' || c == '\t' || c == '\r' || c == '\n' {
					i++
					continue
				}
				break
			}
			return i, valueEnd, true, nil
		}
	}
	return 0, 0, false, nil
}

// TestScannerAgreesWithEncodingJSON checks the byte scanner against the tokenizer it replaced, on
// every shape these containers are known to produce plus a randomised corpus.
func TestScannerAgreesWithEncodingJSON(t *testing.T) {
	docs := []string{
		`{"analyzeResult":{"a":1}}`,
		`{"a":1,"analyzeResult":{"x":[1,2,{"y":"z"}]},"b":2}`,
		`{"status":"succeeded","analyzeResult":[1,2,3]}`,
		`{"n":-12.5e3,"analyzeResult":null}`,
		`{"analyzeResult":true,"z":false}`,
		`{"analyzeResult":"a plain string"}`,
		`{"analyzeResult":"with \"escaped\" quotes and a \\ backslash"}`,
		`{"analyzeResult":"braces } { and brackets ] [ inside a string"}`,
		`{"a":{"analyzeResult":"NESTED"},"analyzeResult":{"real":1}}`,
		`{"a":"\"analyzeResult\":{}","analyzeResult":{"real":1}}`,
		`{"a":[{"analyzeResult":0}],"analyzeResult":{"real":1}}`,
		"{\n  \"analyzeResult\" \t:\r\n  {\"pretty\": \"printed\"}\n}",
		`{"analyzeResult"` + strings.Repeat(" ", 300) + `:{"a":1}}`,
		`{"analyzeResult":` + strings.Repeat(" ", 300) + `{"a":1}}`,
		`{"unicode":"\u007b\u005d","analyzeResult":{"a":1}}`,
		`{"empty":{},"analyzeResult":{}}`,
		`{"emptyArr":[],"analyzeResult":[]}`,
		`{"analyzeResult":{"deep":{"deeper":{"deepest":[[[{"x":1}]]]}}}}`,
		`{"missing":1}`,
		`{}`,
		`{"analyzeResult":1e-7}`,
		`{"a":1e-7,"analyzeResult":2}`,
	}

	// A randomised corpus of well-formed objects, so the agreement is not just over cases I thought of.
	rng := rand.New(rand.NewSource(20260908))
	for i := 0; i < 400; i++ {
		docs = append(docs, randomObject(rng, 0))
	}

	for _, doc := range docs {
		for _, key := range []string{"analyzeResult", "status", "figures", "missing", ""} {
			wantStart, wantEnd, wantFound, wantErr := referenceRange(doc, key)
			gotStart, gotEnd, gotFound, gotErr := ValueRange(
				strings.NewReader(doc), int64(len(doc)), key)

			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("key %q doc %s\n reference err=%v\n scanner   err=%v",
					key, truncate(doc), wantErr, gotErr)
			}
			if wantErr != nil {
				continue
			}
			if gotFound != wantFound {
				t.Fatalf("key %q doc %s: found=%v, reference says %v",
					key, truncate(doc), gotFound, wantFound)
			}
			if !wantFound {
				continue
			}
			if gotStart != wantStart || gotEnd != wantEnd {
				t.Fatalf("key %q doc %s: range [%d,%d) = %q, reference [%d,%d) = %q",
					key, truncate(doc), gotStart, gotEnd, doc[gotStart:gotEnd],
					wantStart, wantEnd, doc[wantStart:wantEnd])
			}
			// Whatever comes out is spliced into a new envelope verbatim, so it must stand alone.
			if !json.Valid([]byte(doc[gotStart:gotEnd])) {
				t.Fatalf("key %q doc %s: extracted %q is not valid JSON",
					key, truncate(doc), doc[gotStart:gotEnd])
			}
		}
	}
}

func randomObject(rng *rand.Rand, depth int) string {
	var b strings.Builder
	b.WriteByte('{')
	n := rng.Intn(4)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pad(rng))
		fmt.Fprintf(&b, "%q", randomKey(rng))
		b.WriteString(pad(rng))
		b.WriteByte(':')
		b.WriteString(pad(rng))
		b.WriteString(randomValue(rng, depth))
	}
	b.WriteString(pad(rng))
	b.WriteByte('}')
	return b.String()
}

func randomKey(rng *rand.Rand) string {
	keys := []string{"analyzeResult", "status", "figures", "content", "a", "b", `esc"key`, `back\slash`}
	return keys[rng.Intn(len(keys))]
}

func randomValue(rng *rand.Rand, depth int) string {
	if depth > 3 {
		return "1"
	}
	switch rng.Intn(8) {
	case 0:
		return "null"
	case 1:
		return "true"
	case 2:
		return fmt.Sprintf("%d", rng.Intn(1000)-500)
	case 3:
		return "-1.5e-3"
	case 4:
		return fmt.Sprintf("%q", `str with } ] " \ and \u0041`)
	case 5:
		return randomObject(rng, depth+1)
	case 6:
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < rng.Intn(3); i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(randomValue(rng, depth+1))
		}
		b.WriteByte(']')
		return b.String()
	default:
		return `"plain"`
	}
}

// pad emits a random run of one whitespace character.
func pad(rng *rand.Rand) string {
	const ws = " \t\n\r"
	i := rng.Intn(len(ws))
	return strings.Repeat(ws[i:i+1], rng.Intn(3))
}

func truncate(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// repeatingReader synthesises a large document without allocating it.
type repeatingReader struct {
	head, unit, tail string
	reps             int64
	size             int64
}

func (r *repeatingReader) ReadAt(p []byte, off int64) (int, error) {
	hl, ul := int64(len(r.head)), int64(len(r.unit))
	var n int
	for n < len(p) {
		if off >= r.size {
			return n, io.EOF
		}
		switch {
		case off < hl:
			p[n] = r.head[off]
		case off < hl+r.reps*ul:
			p[n] = r.unit[(off-hl)%ul]
		default:
			p[n] = r.tail[off-hl-r.reps*ul]
		}
		n++
		off++
	}
	return n, nil
}

// TestScanDoesNotMaterialiseLargeScalars is the regression test for the memory claim.
//
// The tokenizer-based implementation grew its buffer until an entire scalar fit and then allocated
// the decoded string on top, measured at roughly three times the string's size. The largest member
// of a real analyzeResult is `content`, the whole document's text as one JSON string, so four
// concurrent workers on large scans were enough to OOM a pod.
func TestScanDoesNotMaterialiseLargeScalars(t *testing.T) {
	const unit = "The quick brown fox jumps over the lazy dog 0123456789 ABCDEFGHIJ. "
	const reps = 400000 // ~26 MB inside a single JSON string

	r := &repeatingReader{
		head: `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","content":"`,
		unit: unit,
		tail: `","figures":[{"id":"1.1"}]}}`,
		reps: reps,
	}
	r.size = int64(len(r.head)) + reps*int64(len(unit)) + int64(len(r.tail))

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	start, end, found, err := ValueRange(r, r.size, "analyzeResult")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	runtime.ReadMemStats(&after)

	if end-start < reps*int64(len(unit)) {
		t.Fatalf("range [%d,%d) is too small to contain the content string", start, end)
	}

	allocated := int64(after.TotalAlloc - before.TotalAlloc)
	stringBytes := reps * int64(len(unit))
	// The scan needs its read buffer and nothing proportional to the document. A tenth of the
	// string's size is a generous ceiling that the old implementation exceeded by ~30x.
	if allocated > stringBytes/10 {
		t.Errorf("scanning a %d MB string allocated %d MB; the scan must not materialise scalars",
			stringBytes/(1<<20), allocated/(1<<20))
	}
	t.Logf("scanned %d MB document, allocated %d KB", r.size/(1<<20), allocated/1024)
}
