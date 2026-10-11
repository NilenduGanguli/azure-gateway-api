package merge_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/merge"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// diParts generates a DI part for each page run, physically split, and lets edit change a part's
// JSON before it is written.
func diParts(t *testing.T, runs [][]int, edit func(i int, b []byte) []byte) []merge.Part {
	t.Helper()
	var parts []merge.Part
	for i, pages := range runs {
		b := docgen.DI(pages, docgen.Local, docgen.Options{})
		if edit != nil {
			b = edit(i, b)
		}
		parts = append(parts, writePart(t, b, pages, false))
	}
	return parts
}

// replace is a strings.Replace on JSON bytes that insists the old text is there, so a test cannot
// pass by editing nothing.
func replace(t *testing.T, b []byte, old, new string) []byte {
	t.Helper()
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("test bug: %q not found", old)
	}
	return bytes.Replace(b, []byte(old), []byte(new), 1)
}

func TestDIChecks(t *testing.T) {
	two := [][]int{{1, 2, 3}, {4, 5, 6}}
	cases := []struct {
		name  string
		parts func(t *testing.T) []merge.Part
		unit  textunit.Unit
	}{
		{"mismatched stringIndexType", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return docgen.DI(two[1], docgen.Local, docgen.Options{Unit: textunit.UTF16CodeUnit})
				}
				return b
			})
		}, textunit.TextElements},
		{"stringIndexType is not the requested unit", func(t *testing.T) []merge.Part {
			return diParts(t, two, nil)
		}, textunit.UnicodeCodePoint},
		{"apiVersion differs", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"2024-11-30"`, `"2023-07-31"`)
				}
				return b
			})
		}, 0},
		{"contentFormat differs", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"contentFormat":"text"`, `"contentFormat":"markdown"`)
				}
				return b
			})
		}, 0},
		{"unknown contentFormat", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(_ int, b []byte) []byte {
				return replace(t, b, `"contentFormat":"text"`, `"contentFormat":"html"`)
			})
		}, 0},
		{"missing page", func(t *testing.T) []merge.Part {
			parts := diParts(t, [][]int{{1, 2}, {4, 5, 6}}, nil)
			parts[0].Pages = []int{1, 2, 3}
			return parts
		}, 0},
		{"extra page", func(t *testing.T) []merge.Part {
			parts := diParts(t, [][]int{{1, 2, 3}, {4, 5, 6}}, nil)
			parts[0].Pages = []int{1, 2}
			return parts
		}, 0},
		{"page duplicated within a part", func(t *testing.T) []merge.Part {
			// Original numbering: a Local result numbers by position and would hide the repeat.
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 0 {
					return docgen.DI([]int{1, 2, 2}, docgen.Original, docgen.Options{})
				}
				return b
			})
		}, 0},
		{"page duplicated across parts", func(t *testing.T) []merge.Part {
			return diParts(t, [][]int{{1, 2, 3}, {3, 4, 5}}, nil)
		}, 0},
		{"parts out of order", func(t *testing.T) []merge.Part {
			return diParts(t, [][]int{{4, 5, 6}, {1, 2, 3}}, nil)
		}, 0},
		{"pages returned out of order", func(t *testing.T) []merge.Part {
			parts := diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return docgen.DI([]int{5, 4, 6}, docgen.Original, docgen.Options{})
				}
				return b
			})
			return parts
		}, 0},
		{"planned pages not ascending", func(t *testing.T) []merge.Part {
			parts := diParts(t, two, nil)
			parts[1].Pages = []int{4, 6, 5}
			return parts
		}, 0},
		{"returned pages neither local nor original", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return docgen.DI([]int{7, 8, 9}, docgen.Original, docgen.Options{})
				}
				return b
			})
		}, 0},
		{"unknown object member differs", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				return replace(t, b, `"contentFormat":"text"}`, fmt.Sprintf(`"contentFormat":"text","x":{"a":%d}}`, i))
			})
		}, 0},
		{"unknown member only in a later part", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"contentFormat":"text"}`, `"contentFormat":"text","x":true}`)
				}
				return b
			})
		}, 0},
		{"array in one part, object in another", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				v := `[]`
				if i == 1 {
					v = `{}`
				}
				return replace(t, b, `"contentFormat":"text"}`, `"contentFormat":"text","x":`+v+`}`)
			})
		}, 0},
		{"pointer past the part's paragraphs", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"/paragraphs/0"`, `"/paragraphs/99"`)
				}
				return b
			})
		}, 0},
		{"pointer the merge cannot renumber", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"/paragraphs/0"`, `"/pages/0/words/1"`)
				}
				return b
			})
		}, 0},
		{"figure id without a page", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"id":"1.1"`, `"id":"fig-1"`)
				}
				return b
			})
		}, 0},
		{"bounding region on a page outside the part", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"boundingRegions":[{"pageNumber":1,`, `"boundingRegions":[{"pageNumber":4,`)
				}
				return b
			})
		}, 0},
		{"span offset that is not an integer", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"span":{"offset":0,`, `"span":{"offset":0.0,`)
				}
				return b
			})
		}, 0},
		{"page without a pageNumber", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `{"pageNumber":1,"angle":0`, `{"angle":0`)
				}
				return b
			})
		}, 0},
		{"malformed part", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return b[:len(b)-1]
				}
				return b
			})
		}, 0},
		{"part is not an object", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return []byte(`[` + string(b) + `]`)
				}
				return b
			})
		}, 0},
		{"content that is not a string", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"content":"`, `"content":null,"c":"`)
				}
				return b
			})
		}, 0},
		{"content ending in CR, which would join the separator", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 0 {
					return replace(t, b, `\nB3"`, `\nB3\r"`)
				}
				return b
			})
		}, textunit.TextElements},
		{"later part has sections, first has none", func(t *testing.T) []merge.Part {
			return diParts(t, two, func(i int, b []byte) []byte {
				if i == 0 {
					return withoutSections(t, b)
				}
				return b
			})
		}, 0},
		{"no parts", func(t *testing.T) []merge.Part { return nil }, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := merge.DI(&out, tc.parts(t), tc.unit)
			if !errors.Is(err, merge.ErrCheck) {
				t.Fatalf("err = %v, want ErrCheck", err)
			}
			t.Log(err)
		})
	}
}

// withoutSections drops the sections member and every pointer to a section from a docgen result.
func withoutSections(t *testing.T, b []byte) []byte {
	t.Helper()
	i := bytes.Index(b, []byte(`,"sections":[`))
	j := bytes.Index(b, []byte(`,"contentFormat"`))
	if i < 0 || j < i {
		t.Fatal("test bug: no sections member")
	}
	return append(append([]byte(nil), b[:i]...), b[j:]...)
}

func TestIOErrorsAreNotCheckFailures(t *testing.T) {
	parts := diParts(t, [][]int{{1, 2, 3}, {4, 5, 6}}, nil)
	parts[1].Path = filepath.Join(t.TempDir(), "missing.json")
	_, err := merge.DI(io.Discard, parts, 0)
	if err == nil || errors.Is(err, merge.ErrCheck) {
		t.Errorf("missing part file: err = %v, want a plain error", err)
	}

	parts = diParts(t, [][]int{{1, 2, 3}, {4, 5, 6}}, nil)
	boom := errors.New("disk full")
	_, err = merge.DI(failingWriter{boom}, parts, 0)
	if !errors.Is(err, boom) || errors.Is(err, merge.ErrCheck) {
		t.Errorf("failing writer: err = %v, want the writer's error", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestReadChecks(t *testing.T) {
	read := func(t *testing.T, runs [][]int, edit func(i int, b []byte) []byte) []merge.Part {
		var parts []merge.Part
		for i, pages := range runs {
			b := docgen.Read(pages, docgen.Local)
			if edit != nil {
				b = edit(i, b)
			}
			parts = append(parts, writePart(t, b, pages, false))
		}
		return parts
	}
	two := [][]int{{1, 2, 3}, {4, 5, 6}}
	for name, parts := range map[string]func(t *testing.T) []merge.Part{
		"version differs": func(t *testing.T) []merge.Part {
			return read(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"version":"3.2.0"`, `"version":"3.2.1"`)
				}
				return b
			})
		},
		"modelVersion differs": func(t *testing.T) []merge.Part {
			return read(t, two, func(i int, b []byte) []byte {
				if i == 1 {
					return replace(t, b, `"2022-04-30"`, `"2021-04-12"`)
				}
				return b
			})
		},
		"missing page": func(t *testing.T) []merge.Part {
			p := read(t, [][]int{{1, 2}, {4, 5, 6}}, nil)
			p[0].Pages = []int{1, 2, 3}
			return p
		},
		"duplicated page": func(t *testing.T) []merge.Part {
			return read(t, [][]int{{1, 2, 3}, {3, 4}}, nil)
		},
		"out of order": func(t *testing.T) []merge.Part {
			return read(t, [][]int{{4, 5, 6}, {1, 2, 3}}, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := merge.Read(io.Discard, parts(t)); !errors.Is(err, merge.ErrCheck) {
				t.Errorf("err = %v, want ErrCheck", err)
			}
		})
	}
}

// markdownPart builds a minimal markdown DI part by hand: one page span per page, and a paragraph
// per page whose span the merge has to shift past the page-break separators.
func markdownPart(content string, pages int) []byte {
	var ps, paras []string
	for i := 0; i < pages; i++ {
		ps = append(ps, fmt.Sprintf(`{"pageNumber":%d,"spans":[{"offset":%d,"length":4}]}`, i+1, i*24))
		paras = append(paras, fmt.Sprintf(`{"content":"# P","spans":[{"offset":%d,"length":3}]}`, i*24))
	}
	return []byte(`{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"textElements",` +
		`"content":"` + content + `","pages":[` + strings.Join(ps, ",") + `],"paragraphs":[` +
		strings.Join(paras, ",") + `],"contentFormat":"markdown"}`)
}

func TestMarkdownSeparator(t *testing.T) {
	cases := []struct {
		name        string
		part0       string // raw content of the first part, two pages
		part1       string // raw content of the second part, one page
		wantBetween string // the raw separator the merge must write
	}{
		{"upper-case escapes, as .NET writes them", `# P1\n\u003C!-- PageBreak --\u003E\n# P2`, `# P3`,
			`\n\u003C!-- PageBreak --\u003E\n`},
		{"lower-case escapes", `# P1\n\u003c!-- PageBreak --\u003e\n# P2`, `# P3`,
			`\n\u003c!-- PageBreak --\u003e\n`},
		{"raw angle brackets", `# P1\n<!-- PageBreak -->\n# P2`, `# P3`, `\n<!-- PageBreak -->\n`},
		{"no evidence anywhere: raw", `# P1 and P2`, `# P3`, `\n<!-- PageBreak -->\n`},
		{"evidence only in a later part", `# P1 and P2`, `# P3 \u003Ctable\u003E`,
			`\n\u003C!-- PageBreak --\u003E\n`},
		{"an escaped backslash is not an escape", `# P1 \\u003C and P2`, `# P3 <b>`,
			`\n<!-- PageBreak -->\n`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := []merge.Part{
				writePart(t, markdownPart(tc.part0, 2), []int{1, 2}, false),
				writePart(t, markdownPart(tc.part1, 1), []int{3}, false),
			}
			var out bytes.Buffer
			if _, err := merge.DI(&out, parts, textunit.TextElements); err != nil {
				t.Fatal(err)
			}
			wantRaw := `"content":"` + tc.part0 + tc.wantBetween + tc.part1 + `"`
			if !strings.Contains(out.String(), wantRaw) {
				t.Fatalf("merged content is not %s:\n%s", wantRaw, out.String())
			}
			var r struct {
				Content string
				Pages   []struct {
					PageNumber int
					Spans      []struct{ Offset, Length int }
				}
				Paragraphs []struct {
					Spans []struct{ Offset, Length int }
				}
			}
			if err := json.Unmarshal(out.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			var p0 string
			_ = json.Unmarshal([]byte(`"`+tc.part0+`"`), &p0)
			sep := "\n<!-- PageBreak -->\n"
			if !strings.Contains(r.Content, sep) {
				t.Errorf("decoded content %q has no page break", r.Content)
			}
			// The third page starts after part 0's content and one separator, in grapheme
			// clusters; everything in this test is ASCII so that is also its byte offset.
			want := len([]rune(p0)) + len(sep)
			if got := r.Pages[2].Spans[0].Offset; got != want || r.Paragraphs[2].Spans[0].Offset != want {
				t.Errorf("third page offset %d (paragraph %d), want %d", got, r.Paragraphs[2].Spans[0].Offset, want)
			}
			if r.Content[want:want+2] != "# " || r.Pages[2].PageNumber != 3 {
				t.Errorf("third page does not start at its offset: %q", r.Content[want:])
			}
		})
	}
}

// TestUnknownArraysAreConcatenatedAndShifted covers a member the merge has no rule for: it is an
// array, so its items are concatenated in part order, and the structural rules still apply inside
// them. A member that first appears in a later part goes after part 0's members.
func TestUnknownArraysAreConcatenatedAndShifted(t *testing.T) {
	runs := [][]int{{1, 2, 3}, {4, 5}, {6, 7, 8}}
	parts := diParts(t, runs, func(i int, b []byte) []byte {
		switch i {
		case 0:
			return replace(t, b, `"contentFormat":"text"}`,
				`"contentFormat":"text","warnings":[{"code":"w0","spans":[{"offset":3,"length":1}]}]}`)
		case 2:
			b = replace(t, b, `"contentFormat":"text"}`,
				`"contentFormat":"text","warnings":[{"code":"w2a","span":{"offset":5,"length":1}},`+
					`{"code":"w2b","boundingRegions":[{"pageNumber":2,"polygon":[]}]}]}`)
			return replace(t, b, `"apiVersion"`, `"styles":[{"spans":[{"offset":0,"length":4}],"confidence":1},{"spans":[]}],"apiVersion"`)
		}
		return b
	})
	var out bytes.Buffer
	st, err := merge.DI(&out, parts, textunit.TextElements)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Content  string
		Warnings []struct {
			Code            string
			Span            struct{ Offset int }
			Spans           []struct{ Offset int }
			BoundingRegions []struct{ PageNumber int }
		}
		Styles []struct{ Spans []struct{ Offset int } }
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	// Where the third part's content starts in the merged content.
	whole := docgen.DI(pageRange(1, 8), docgen.Original, docgen.Options{})
	var w struct{ Content string }
	_ = json.Unmarshal(whole, &w)
	third := strings.Index(w.Content, "Heading P6")
	shift := textunit.Len(w.Content[:third], textunit.TextElements)

	if len(r.Warnings) != 3 || r.Warnings[0].Code != "w0" || r.Warnings[1].Code != "w2a" || r.Warnings[2].Code != "w2b" {
		t.Fatalf("warnings %+v", r.Warnings)
	}
	if r.Warnings[0].Spans[0].Offset != 3 || r.Warnings[1].Span.Offset != 5+shift {
		t.Errorf("warning offsets %d and %d, want 3 and %d", r.Warnings[0].Spans[0].Offset,
			r.Warnings[1].Span.Offset, 5+shift)
	}
	if r.Warnings[2].BoundingRegions[0].PageNumber != 7 {
		t.Errorf("warning region on page %d, want 7", r.Warnings[2].BoundingRegions[0].PageNumber)
	}
	if len(r.Styles) != 2 || r.Styles[0].Spans[0].Offset != shift {
		t.Errorf("styles %+v, want the third part's two, shifted by %d", r.Styles, shift)
	}
	if st.Counts["warnings"] != 3 || st.Counts["styles"] != 2 {
		t.Errorf("counts %v", st.Counts)
	}
	if r.Content != w.Content {
		t.Errorf("content differs from the whole document's")
	}
	// warnings first appears in part 0, after contentFormat; styles only in part 2, so it is
	// appended after everything part 0 has.
	order := memberOrder(t, out.Bytes())
	if want := "apiVersion modelId stringIndexType content pages paragraphs tables figures sections contentFormat warnings styles"; order != want {
		t.Errorf("member order %q, want %q", order, want)
	}
}

func memberOrder(t *testing.T, b []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return strings.Join(names, " ")
}

func TestEqualUnknownObjectMemberIsKept(t *testing.T) {
	parts := diParts(t, [][]int{{1, 2, 3}, {4, 5, 6}}, func(_ int, b []byte) []byte {
		return replace(t, b, `"contentFormat":"text"}`, `"contentFormat":"text","x":{"a":[1, 2]}}`)
	})
	var out bytes.Buffer
	if _, err := merge.DI(&out, parts, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(out.Bytes(), []byte(`"contentFormat":"text","x":{"a":[1, 2]}}`)) {
		t.Errorf("unknown member not kept verbatim: %s", out.Bytes()[len(out.Bytes())-60:])
	}
}

// TestPartWithoutSectionsContributesNothing: a part whose result has no sections adds no sections
// and no root elements, and the others still renumber around it.
func TestPartWithoutSectionsContributesNothing(t *testing.T) {
	runs := [][]int{{1, 2}, {3, 4}, {5, 6}}
	parts := diParts(t, runs, func(i int, b []byte) []byte {
		if i == 1 {
			return withoutSections(t, b)
		}
		return b
	})
	var out bytes.Buffer
	st, err := merge.DI(&out, parts, 0)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Content    string
		Paragraphs []struct{ Content string }
		Sections   []struct {
			Spans    []struct{ Offset, Length int }
			Elements []string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Sections) != 5 || st.Counts["sections"] != 5 {
		t.Fatalf("%d sections (stats %d), want a root and four pages'", len(r.Sections), st.Counts["sections"])
	}
	if got := strings.Join(r.Sections[0].Elements, " "); got != "/sections/1 /sections/2 /sections/3 /sections/4" {
		t.Errorf("root elements %s", got)
	}
	if sp := r.Sections[0].Spans; len(sp) != 1 || sp[0].Offset != 0 ||
		sp[0].Length != textunit.Len(r.Content, textunit.TextElements) {
		t.Errorf("root spans %+v", sp)
	}
	// Section 3 is page 5's: its first element must be page 5's heading.
	var idx int
	if _, err := fmt.Sscanf(r.Sections[3].Elements[0], "/paragraphs/%d", &idx); err != nil {
		t.Fatal(err)
	}
	if r.Paragraphs[idx].Content != "Heading P5" {
		t.Errorf("section 3 starts with %q, want page 5's heading", r.Paragraphs[idx].Content)
	}
}

func TestSectionsAbsentEverywhere(t *testing.T) {
	parts := diParts(t, [][]int{{1, 2, 3}, {4, 5, 6}}, func(_ int, b []byte) []byte {
		return withoutSections(t, b)
	})
	var out bytes.Buffer
	if _, err := merge.DI(&out, parts, 0); err != nil {
		t.Fatal(err)
	}
	if want := withoutSections(t, docgen.DI(pageRange(1, 6), docgen.Original, docgen.Options{})); !bytes.Equal(out.Bytes(), want) {
		sameBytes(t, out.Bytes(), want)
	}
}

// hashWriter hashes and counts what is written, so a large merged result can be compared with the
// whole document without holding either in memory.
type hashWriter struct {
	h interface {
		io.Writer
		Sum([]byte) []byte
	}
	n int64
}

func (w *hashWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return w.h.Write(p)
}

// TestMergeMemoryIsBounded is the memory claim: merging is a scan with a few buffers per pass, so
// allocation does not grow with the result. A merge that materialised the parts' arrays would
// allocate at least the result's size.
//
// docgen pages are small, about 2.5 KB against tens of kilobytes for a real page, so parts of 100
// docgen pages stand in for the 3–5 page parts of a real document of the same size.
func TestMergeMemoryIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("generates and merges about 60 MB")
	}
	const pages, perPart = 20000, 100
	opts := docgen.Options{Unit: textunit.UTF16CodeUnit}
	whole := docgen.DI(pageRange(1, pages), docgen.Original, opts)
	want := sha256.Sum256(whole)
	wholeSize := len(whole)
	whole = nil

	var parts []merge.Part
	for p := 1; p <= pages; p += perPart {
		run := pageRange(p, min(p+perPart-1, pages))
		parts = append(parts, writePart(t, docgen.DI(run, docgen.Local, opts), run, false))
	}

	out := &hashWriter{h: sha256.New()}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	st, err := merge.DI(out, parts, opts.Unit)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.h.Sum(nil); !bytes.Equal(got, want[:]) || out.n != int64(wholeSize) || st.Bytes != out.n {
		t.Fatalf("merged %d bytes (stats %d), whole is %d; hashes equal: %v",
			out.n, st.Bytes, wholeSize, bytes.Equal(got, want[:]))
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 32<<20 {
		t.Errorf("merging %d MB in %d parts allocated %d MB; the merge must not grow with the result",
			wholeSize>>20, len(parts), allocated>>20)
	}
	t.Logf("merged %d MB from %d parts, allocated %d KB", wholeSize>>20, len(parts), allocated>>10)
}

// TestMergeClosesPartFiles checks that every part file is closed by the time a merge returns,
// whether it succeeds or fails: a gateway merging hundreds of parts per job would otherwise run
// out of descriptors.
func TestMergeClosesPartFiles(t *testing.T) {
	open := func() int {
		// Names only: statting the entries trips over the descriptor reading the directory.
		d, err := os.Open("/dev/fd")
		if err != nil {
			t.Skip("no /dev/fd to count descriptors with")
		}
		defer d.Close()
		names, err := d.Readdirnames(-1)
		if err != nil {
			t.Skip("cannot list /dev/fd:", err)
		}
		return len(names)
	}
	good := diParts(t, plan(23, 5), nil)
	bad := diParts(t, plan(23, 5), func(i int, b []byte) []byte {
		if i == 3 {
			return replace(t, b, `"2024-11-30"`, `"2023-07-31"`)
		}
		return b
	})
	before := open()
	if _, err := merge.DI(io.Discard, good, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := merge.DI(io.Discard, bad, 0); !errors.Is(err, merge.ErrCheck) {
		t.Fatalf("err = %v, want ErrCheck", err)
	}
	if _, err := merge.Read(io.Discard, good); !errors.Is(err, merge.ErrCheck) {
		t.Fatalf("Read of DI parts: err = %v, want ErrCheck", err)
	}
	if after := open(); after != before {
		t.Errorf("%d descriptors open before the merges, %d after", before, after)
	}
}
