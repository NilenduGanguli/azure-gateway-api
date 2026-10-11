package pdfsplit

import (
	"slices"
	"testing"
)

func TestParsePagesAcceptsAzureGrammar(t *testing.T) {
	cases := []struct {
		spec string
		want []PageRange
	}{
		{"1", []PageRange{{1, 1}}},
		{"1-3", []PageRange{{1, 3}}},
		{"1-3,5,7-9", []PageRange{{1, 3}, {5, 5}, {7, 9}}},
		{" 2 - 4 , 6 ", []PageRange{{2, 4}, {6, 6}}},
		{"4-4", []PageRange{{4, 4}}},
		// Terms keep the client's order; Select is what sorts and de-duplicates.
		{"9,2-3", []PageRange{{9, 9}, {2, 3}}},
	}
	for _, tc := range cases {
		got, ok := ParsePages(tc.spec)
		if !ok || !slices.Equal(got, tc.want) {
			t.Errorf("ParsePages(%q) = %v, %v; want %v, true", tc.spec, got, ok, tc.want)
		}
	}
}

func TestParsePagesRejectsEverythingElse(t *testing.T) {
	for _, spec := range []string{
		"0", "3-1", "1-", "a", "", "1,,2",
		"-3", "0-2", "1-2-3", "+1", "1.5", "1 2", "1,2,", ",1", " ",
		"99999999999999999999", // overflows int
	} {
		if got, ok := ParsePages(spec); ok {
			t.Errorf("ParsePages(%q) = %v, true; want it rejected", spec, got)
		}
	}
}

func TestSelectExpandsRangesWithinTheDocument(t *testing.T) {
	cases := []struct {
		name   string
		ranges []PageRange
		count  int
		want   []int
		ok     bool
	}{
		{"one range", []PageRange{{2, 4}}, 10, []int{2, 3, 4}, true},
		{"overlaps de-duplicated", []PageRange{{1, 5}, {3, 7}, {5, 5}}, 10, []int{1, 2, 3, 4, 5, 6, 7}, true},
		{"ascending whatever the order given", []PageRange{{7, 9}, {1, 3}, {5, 5}}, 10, []int{1, 2, 3, 5, 7, 8, 9}, true},
		{"last page included", []PageRange{{9, 10}}, 10, []int{9, 10}, true},
		{"past the last page", []PageRange{{9, 11}}, 10, nil, false},
		{"one bad range spoils the selection", []PageRange{{1, 2}, {11, 11}}, 10, nil, false},
		{"page zero", []PageRange{{0, 2}}, 10, nil, false},
		{"reversed range", []PageRange{{3, 1}}, 10, nil, false},
		{"no ranges", nil, 10, nil, false},
		{"document without pages", []PageRange{{1, 1}}, 0, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Select(tc.ranges, tc.count)
			if ok != tc.ok || !slices.Equal(got, tc.want) {
				t.Errorf("Select(%v, %d) = %v, %v; want %v, %v", tc.ranges, tc.count, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestFormatPagesCollapsesRuns(t *testing.T) {
	cases := []struct {
		pages []int
		want  string
	}{
		{[]int{1}, "1"},
		{[]int{1, 2, 3}, "1-3"},
		{[]int{3, 4, 5, 9}, "3-5,9"},
		{[]int{2, 4}, "2,4"},
		{[]int{1, 2, 4, 5}, "1-2,4-5"},
		{[]int{1, 3, 4, 5, 7}, "1,3-5,7"},
		{nil, ""},
	}
	for _, tc := range cases {
		got := FormatPages(tc.pages)
		if got != tc.want {
			t.Errorf("FormatPages(%v) = %q, want %q", tc.pages, got, tc.want)
		}
		if len(tc.pages) == 0 {
			continue
		}
		// A terminal part is re-checked by sending pages=FormatPages(part.Pages) with the original
		// document, so the rendering must read back as exactly the same selection.
		ranges, ok := ParsePages(got)
		if !ok {
			t.Errorf("ParsePages rejects FormatPages output %q", got)
			continue
		}
		if back, ok := Select(ranges, tc.pages[len(tc.pages)-1]); !ok || !slices.Equal(back, tc.pages) {
			t.Errorf("%q reads back as %v, %v; want %v", got, back, ok, tc.pages)
		}
	}
}
