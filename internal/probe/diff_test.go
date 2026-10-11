package probe

import (
	"testing"
)

func TestDiff(t *testing.T) {
	cases := []struct {
		name         string
		whole, split string
		want         []Difference
	}{
		{
			name:  "identical documents differ nowhere",
			whole: `{"content":"a b","pages":[{"pageNumber":1,"words":[{"content":"a","span":{"offset":0,"length":1}}]}]}`,
			split: `{"content":"a b","pages":[{"pageNumber":1,"words":[{"content":"a","span":{"offset":0,"length":1}}]}]}`,
		},
		{
			name:  "member order is not a difference",
			whole: `{"a":1,"b":2}`,
			split: `{"b":2,"a":1}`,
		},
		{
			name:  "the same number written differently is not a difference",
			whole: `{"angle":0,"width":8.5}`,
			split: `{"angle":0.0,"width":8.50}`,
		},
		{
			name:  "a paragraph role is document-level",
			whole: `{"paragraphs":[{"content":"A"},{"role":"sectionHeading","content":"B"}]}`,
			split: `{"paragraphs":[{"content":"A"},{"role":"title","content":"B"}]}`,
			want: []Difference{{
				Path: "paragraphs[1].role", Pattern: "paragraphs[*].role", Level: DocumentLevel,
				Whole: `"sectionHeading"`, Split: `"title"`,
			}},
		},
		{
			name:  "a word offset is page-level",
			whole: `{"pages":[{"words":[{"span":{"offset":5,"length":2}}]}]}`,
			split: `{"pages":[{"words":[{"span":{"offset":6,"length":2}}]}]}`,
			want: []Difference{{
				Path: "pages[0].words[0].span.offset", Pattern: "pages[*].words[*].span.offset",
				Level: PageLevel, Whole: "5", Split: "6",
			}},
		},
		{
			name:  "a paragraph's bounding region is page-level",
			whole: `{"paragraphs":[{"boundingRegions":[{"pageNumber":1}]}]}`,
			split: `{"paragraphs":[{"boundingRegions":[{"pageNumber":2}]}]}`,
			want: []Difference{{
				Path: "paragraphs[0].boundingRegions[0].pageNumber", Pattern: "paragraphs[*].boundingRegions[*].pageNumber",
				Level: PageLevel, Whole: "1", Split: "2",
			}},
		},
		{
			name:  "a table's cells are page-level",
			whole: `{"tables":[{"rowCount":1,"cells":[{"content":"x"}]}]}`,
			split: `{"tables":[{"rowCount":1,"cells":[{"content":"y"}]}]}`,
			want: []Difference{{
				Path: "tables[0].cells[0].content", Pattern: "tables[*].cells[*].content",
				Level: PageLevel, Whole: `"x"`, Split: `"y"`,
			}},
		},
		{
			name:  "the table count is document-level",
			whole: `{"tables":[{"rowCount":1},{"rowCount":2}]}`,
			split: `{"tables":[{"rowCount":1}]}`,
			want: []Difference{{
				Path: "tables", Pattern: "tables", Level: DocumentLevel,
				Whole: "2 items", Split: "1 items",
			}},
		},
		{
			name:  "a figure is page-level",
			whole: `{"figures":[{"id":"1.1"}]}`,
			split: `{"figures":[{"id":"1.2"}]}`,
			want: []Difference{{
				Path: "figures[0].id", Pattern: "figures[*].id", Level: PageLevel,
				Whole: `"1.1"`, Split: `"1.2"`,
			}},
		},
		{
			name:  "a Read page is page-level",
			whole: `{"readResults":[{"lines":[{"text":"a"}]}]}`,
			split: `{"readResults":[{"lines":[{"text":"b"}]}]}`,
			want: []Difference{{
				Path: "readResults[0].lines[0].text", Pattern: "readResults[*].lines[*].text",
				Level: PageLevel, Whole: `"a"`, Split: `"b"`,
			}},
		},
		{
			name:  "a member only one side has is a difference",
			whole: `{"styles":[],"content":"x"}`,
			split: `{"content":"x","sections":[]}`,
			want: []Difference{
				{Path: "sections", Pattern: "sections", Level: DocumentLevel, Whole: absentText, Split: "[]"},
				{Path: "styles", Pattern: "styles", Level: DocumentLevel, Whole: "[]", Split: absentText},
			},
		},
		{
			name:  "a type change is a difference",
			whole: `{"content":"x"}`,
			split: `{"content":["x"]}`,
			want: []Difference{{
				Path: "content", Pattern: "content", Level: DocumentLevel, Whole: `"x"`, Split: `["x"]`,
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Diff([]byte(tc.whole), []byte(tc.split))
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d differences, want %d:\n%+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("difference %d:\n got %+v\nwant %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestDiffRejectsInvalidJSON(t *testing.T) {
	if _, err := Diff([]byte(`{"a":`), []byte(`{}`)); err == nil {
		t.Fatal("Diff accepted a truncated document")
	}
	if _, err := Diff([]byte(`{}`), []byte(`{} {}`)); err == nil {
		t.Fatal("Diff accepted trailing data")
	}
}

func TestGroup(t *testing.T) {
	diffs := []Difference{
		{Path: "paragraphs[5].role", Pattern: "paragraphs[*].role", Level: DocumentLevel, Whole: `"a"`, Split: `"b"`},
		{Path: "paragraphs[9].role", Pattern: "paragraphs[*].role", Level: DocumentLevel, Whole: `"a"`, Split: `"b"`},
		{Path: "paragraphs[12].role", Pattern: "paragraphs[*].role", Level: DocumentLevel, Whole: `"a"`, Split: `"b"`},
		{Path: "paragraphs[14].role", Pattern: "paragraphs[*].role", Level: DocumentLevel, Whole: `"a"`, Split: `"b"`},
		{File: "x.pdf", Path: "pages[1].angle", Pattern: "pages[*].angle", Level: PageLevel, Whole: "0", Split: "1"},
	}
	got := Group(diffs)
	if len(got) != 2 {
		t.Fatalf("got %d patterns, want 2: %+v", len(got), got)
	}
	// Page-level patterns come first: they are the ones that fail the gate.
	if got[0].Pattern != "pages[*].angle" || got[0].Level != PageLevel || got[0].Count != 1 {
		t.Errorf("first pattern = %+v", got[0])
	}
	if want := "x.pdf: pages[1].angle: whole 0, split 1"; len(got[0].Examples) != 1 || got[0].Examples[0] != want {
		t.Errorf("page-level examples = %q, want [%q]", got[0].Examples, want)
	}
	if got[1].Pattern != "paragraphs[*].role" || got[1].Count != 4 {
		t.Errorf("second pattern = %+v", got[1])
	}
	if len(got[1].Examples) != maxExamples {
		t.Errorf("got %d examples, want at most %d", len(got[1].Examples), maxExamples)
	}
}
