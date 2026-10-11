package merge_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/merge"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

var units = []textunit.Unit{textunit.TextElements, textunit.UnicodeCodePoint, textunit.UTF16CodeUnit}

func pageRange(from, to int) []int {
	ps := make([]int, 0, to-from+1)
	for p := from; p <= to; p++ {
		ps = append(ps, p)
	}
	return ps
}

// plan cuts pages 1..n into k = ceil(n/size) consecutive runs, the first n%k of them one page
// longer, as the splitter does. It is local so this package does not depend on the splitter.
func plan(n, size int) [][]int {
	k := (n + size - 1) / size
	var parts [][]int
	p := 1
	for i := 0; i < k; i++ {
		c := n / k
		if i < n%k {
			c++
		}
		parts = append(parts, pageRange(p, p+c-1))
		p += c
	}
	return parts
}

// writePart stores one part's result in its own file, optionally inside an operation envelope so
// that Start and End have to be honoured.
func writePart(t testing.TB, data []byte, pages []int, envelope bool) merge.Part {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("part-%d-%d.json", pages[0], len(pages)))
	var start int64
	if envelope {
		head := `{"status":"succeeded","createdDateTime":"2024-11-30T00:00:00Z","analyzeResult":`
		start = int64(len(head))
		data = append(append([]byte(head), data...), `,"x":[1]}`...)
	}
	end := int64(len(data))
	if envelope {
		end -= int64(len(`,"x":[1]}`))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return merge.Part{Path: path, Start: start, End: end, Pages: pages}
}

// sameBytes fails with the first point of difference, which is far more useful than two
// kilobytes of JSON side by side.
func sameBytes(t testing.TB, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	from := max(0, i-120)
	t.Fatalf("merged result differs at byte %d of %d (want %d bytes)\n got: …%s…\nwant: …%s…",
		i, len(got), len(want), got[from:min(len(got), i+80)], want[from:min(len(want), i+80)])
}

// counts returns the item count of each top-level array in a result.
func counts(t testing.TB, b []byte) map[string]int {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for k, v := range m {
		var items []json.RawMessage
		if len(v) > 0 && v[0] == '[' && json.Unmarshal(v, &items) == nil {
			out[k] = len(items)
		}
	}
	return out
}

// numberings decides how each part numbers its pages: physically split parts (Local), pages=
// re-checks on the original (Original), and a mix of the two.
var numberings = map[string]func(i int) docgen.Numbering{
	"local":    func(int) docgen.Numbering { return docgen.Local },
	"original": func(int) docgen.Numbering { return docgen.Original },
	"mixed": func(i int) docgen.Numbering {
		if i == 0 {
			return docgen.Local
		}
		return docgen.Original
	},
}

func TestDIRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 6, 7, 11, 23} {
		for _, u := range units {
			opts := docgen.Options{Unit: u}
			whole := docgen.DI(pageRange(1, n), docgen.Original, opts)
			for name, numbering := range numberings {
				t.Run(fmt.Sprintf("%d pages/%v/%s", n, u, name), func(t *testing.T) {
					var parts []merge.Part
					for i, pages := range plan(n, 5) {
						parts = append(parts, writePart(t, docgen.DI(pages, numbering(i), opts), pages, i%2 == 1))
					}
					var out bytes.Buffer
					st, err := merge.DI(&out, parts, u)
					if err != nil {
						t.Fatalf("merge.DI: %v", err)
					}
					sameBytes(t, out.Bytes(), whole)
					if st.Pages != n || st.Bytes != int64(len(whole)) {
						t.Errorf("stats pages=%d bytes=%d, want %d and %d", st.Pages, st.Bytes, n, len(whole))
					}
					for member, c := range counts(t, whole) {
						if st.Counts[member] != c {
							t.Errorf("stats count %s=%d, want %d", member, st.Counts[member], c)
						}
					}
				})
			}
		}
	}
}

// TestDIRoundTripAnyPlan merges plans the splitter would not make, down to one page per part and
// with pages skipped, as a client's pages= selection would leave them.
func TestDIRoundTripAnyPlan(t *testing.T) {
	selected := []int{2, 3, 4, 8, 9, 12, 13, 16, 20, 21, 24}
	for _, u := range units {
		opts := docgen.Options{Unit: u}
		whole := docgen.DI(selected, docgen.Original, opts)
		for _, size := range []int{1, 2, 3, 4} {
			var parts []merge.Part
			for i := 0; i < len(selected); i += size {
				pages := selected[i:min(i+size, len(selected))]
				parts = append(parts, writePart(t, docgen.DI(pages, numberings["mixed"](i), opts), pages, false))
			}
			var out bytes.Buffer
			if _, err := merge.DI(&out, parts, u); err != nil {
				t.Fatalf("%v size %d: %v", u, size, err)
			}
			sameBytes(t, out.Bytes(), whole)
		}
	}
}

// TestPrettyPrintedParts merges parts written with whitespace everywhere JSON allows it. Values
// keep their own whitespace, so the merged result equals the whole document once compacted. It
// exercises whitespace between members, around items and after the root section.
func TestPrettyPrintedParts(t *testing.T) {
	for _, u := range units {
		opts := docgen.Options{Unit: u}
		whole := docgen.DI(pageRange(1, 11), docgen.Original, opts)
		var parts []merge.Part
		for i, pages := range plan(11, 5) {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, docgen.DI(pages, docgen.Local, opts), "", " \t "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteString("\n\n")
			parts = append(parts, writePart(t, pretty.Bytes(), pages, i == 1))
		}
		var out, compact bytes.Buffer
		if _, err := merge.DI(&out, parts, u); err != nil {
			t.Fatalf("%v: %v", u, err)
		}
		if err := json.Compact(&compact, out.Bytes()); err != nil {
			t.Fatalf("%v: merged result is not JSON: %v", u, err)
		}
		sameBytes(t, compact.Bytes(), whole)
	}
}

// TestArraysAbsentFromSomeParts: the service may leave out an empty array altogether. A part
// without it adds no items, pointers in later parts still count the items that do exist, and a
// member first seen in a later part goes after part 0's members.
func TestArraysAbsentFromSomeParts(t *testing.T) {
	runs := [][]int{{1, 2, 3}, {4, 5, 6}, {7, 8}}
	var parts []merge.Part
	for i, pages := range runs {
		b := docgen.DI(pages, docgen.Local, docgen.Options{})
		switch i {
		case 0: // pages 1-3 have no figure
			b = bytes.Replace(b, []byte(`,"figures":[]`), nil, 1)
		case 2: // pages 7-8 have no table
			b = bytes.Replace(b, []byte(`,"tables":[]`), nil, 1)
		}
		parts = append(parts, writePart(t, b, pages, false))
	}
	var out bytes.Buffer
	st, err := merge.DI(&out, parts, textunit.TextElements)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	whole := docgen.DI(pageRange(1, 8), docgen.Original, docgen.Options{})
	if err := json.Unmarshal(whole, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("members %d, want %d", len(got), len(want))
	}
	for name, v := range want {
		if !bytes.Equal(got[name], v) {
			t.Errorf("member %s differs:\n got %s\nwant %s", name, got[name], v)
		}
	}
	if st.Counts["figures"] != 2 || st.Counts["tables"] != 2 {
		t.Errorf("counts %v", st.Counts)
	}
	if order := memberOrder(t, out.Bytes()); order != "apiVersion modelId stringIndexType content "+
		"pages paragraphs tables sections contentFormat figures" {
		t.Errorf("member order %q", order)
	}
}

func TestReadRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 6, 7, 11, 23} {
		whole := docgen.Read(pageRange(1, n), docgen.Original)
		for name, numbering := range numberings {
			t.Run(fmt.Sprintf("%d pages/%s", n, name), func(t *testing.T) {
				var parts []merge.Part
				for i, pages := range plan(n, 5) {
					parts = append(parts, writePart(t, docgen.Read(pages, numbering(i)), pages, i%2 == 0))
				}
				var out bytes.Buffer
				st, err := merge.Read(&out, parts)
				if err != nil {
					t.Fatalf("merge.Read: %v", err)
				}
				sameBytes(t, out.Bytes(), whole)
				if st.Pages != n || st.Bytes != int64(len(whole)) || st.Counts["readResults"] != n {
					t.Errorf("stats %+v", st)
				}
			})
		}
	}
}

// TestDocumentLevelDifferencesAreLeftAlone: docgen's TitleOnFirstPage imitates a judgement the
// service makes for the whole document. Each part makes it for itself, so the merged result has a
// title per part where the whole document has one. The merge has no rule for this, deliberately:
// rules for document-level differences come from measuring the real service (spec §6), and
// everything else must still be exact.
func TestDocumentLevelDifferencesAreLeftAlone(t *testing.T) {
	opts := docgen.Options{TitleOnFirstPage: true}
	whole := docgen.DI(pageRange(1, 11), docgen.Original, opts)
	var parts []merge.Part
	for _, pages := range plan(11, 5) {
		parts = append(parts, writePart(t, docgen.DI(pages, docgen.Local, opts), pages, false))
	}
	var out bytes.Buffer
	if _, err := merge.DI(&out, parts, textunit.TextElements); err != nil {
		t.Fatal(err)
	}
	// Pages 5 and 9 open the second and third parts.
	want := bytes.Replace(whole, []byte(`"role":"sectionHeading","content":"Heading P5"`),
		[]byte(`"role":"title","content":"Heading P5"`), 1)
	want = bytes.Replace(want, []byte(`"role":"sectionHeading","content":"Heading P9"`),
		[]byte(`"role":"title","content":"Heading P9"`), 1)
	sameBytes(t, out.Bytes(), want)
}

// BenchmarkMergeDI merges 100 parts of about 1 MB each, the spec's benchmark.
func BenchmarkMergeDI(b *testing.B) {
	const parts, perPart = 100, 400 // docgen pages are about 2.5 KB
	var ps []merge.Part
	var size int64
	for i := 0; i < parts; i++ {
		run := pageRange(i*perPart+1, (i+1)*perPart)
		p := writePart(b, docgen.DI(run, docgen.Local, docgen.Options{}), run, false)
		size += p.End - p.Start
		ps = append(ps, p)
	}
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := merge.DI(io.Discard, ps, textunit.TextElements); err != nil {
			b.Fatal(err)
		}
	}
}
