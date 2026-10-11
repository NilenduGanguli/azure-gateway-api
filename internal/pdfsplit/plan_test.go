package pdfsplit

import (
	"math"
	"slices"
	"testing"
)

// seq returns first..last.
func seq(first, last int) []int {
	var s []int
	for p := first; p <= last; p++ {
		s = append(s, p)
	}
	return s
}

func sizes(parts []Part) []int {
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i] = len(p.Pages)
	}
	return out
}

func TestPlanBalancesParts(t *testing.T) {
	cases := []struct {
		n    int
		want []int
	}{
		{1, []int{1}},
		{2, []int{2}},
		{3, []int{3}},
		{4, []int{4}},
		{5, []int{5}},
		{6, []int{3, 3}},
		{7, []int{4, 3}},
		{11, []int{4, 4, 3}},
		{16, []int{4, 4, 4, 4}},
		{21, []int{5, 4, 4, 4, 4}},
	}
	for _, tc := range cases {
		if got := sizes(Plan(seq(1, tc.n), 5)); !slices.Equal(got, tc.want) {
			t.Errorf("Plan(1..%d, 5) sizes = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func TestPlanLargeDocument(t *testing.T) {
	parts := Plan(seq(1, 2000), 5)
	if len(parts) != 400 {
		t.Fatalf("got %d parts, want 400", len(parts))
	}
	for i, p := range parts {
		if len(p.Pages) != 5 {
			t.Fatalf("part %d has %d pages, want 5", i, len(p.Pages))
		}
	}
}

// TestPlanInvariants checks the whole contract over a range of sizes: k = ceil(n/max) parts that
// cover the input in order, sizes that differ by at most one with the larger parts first, and,
// once a document is split at all, every part between ceil(max/2) and max pages.
func TestPlanInvariants(t *testing.T) {
	for max := 1; max <= 8; max++ {
		for n := 1; n <= 60; n++ {
			pages := seq(1, n)
			parts := Plan(pages, max)

			if want := (n + max - 1) / max; len(parts) != want {
				t.Fatalf("Plan(1..%d, %d): %d parts, want %d", n, max, len(parts), want)
			}
			var all []int
			for _, p := range parts {
				all = append(all, p.Pages...)
			}
			if !slices.Equal(all, pages) {
				t.Fatalf("Plan(1..%d, %d) concatenates to %v", n, max, all)
			}
			got := sizes(parts)
			for i := 1; i < len(got); i++ {
				if got[i] > got[i-1] || got[0]-got[i] > 1 {
					t.Fatalf("Plan(1..%d, %d) sizes %v are not balanced", n, max, got)
				}
			}
			if n <= max {
				continue
			}
			lo := (max + 1) / 2
			for _, size := range got {
				if size < lo || size > max {
					t.Fatalf("Plan(1..%d, %d) sizes %v leave %d..%d", n, max, got, lo, max)
				}
			}
		}
	}
}

func TestPlanKeepsOriginalPageNumbers(t *testing.T) {
	pages := []int{1, 2, 3, 10, 11, 12, 13}
	parts := Plan(pages, 5)

	want := [][]int{{1, 2, 3, 10}, {11, 12, 13}}
	if len(parts) != len(want) {
		t.Fatalf("got %d parts, want %d", len(parts), len(want))
	}
	for i := range want {
		if !slices.Equal(parts[i].Pages, want[i]) {
			t.Errorf("part %d = %v, want %v", i, parts[i].Pages, want[i])
		}
	}

	// Parts outlive the slice they were planned from, so they must not share its memory.
	pages[0] = 99
	if parts[0].Pages[0] != 1 {
		t.Error("a part aliases the caller's slice")
	}
}

func TestPlanHugeMaxIsOnePart(t *testing.T) {
	parts := Plan([]int{1, 2}, math.MaxInt)
	if len(parts) != 1 || !slices.Equal(parts[0].Pages, []int{1, 2}) {
		t.Fatalf("Plan([1 2], MaxInt) = %v, want one part [1 2]", parts)
	}
}

func TestPlanEmptySelectionHasNoParts(t *testing.T) {
	if parts := Plan(nil, 5); len(parts) != 0 {
		t.Errorf("Plan(nil, 5) = %v, want no parts", parts)
	}
}

func TestPlanPanicsOnNonPositiveMax(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Plan with max 0 did not panic")
		}
	}()
	Plan([]int{1, 2}, 0)
}

func TestHalve(t *testing.T) {
	cases := []struct {
		pages, first, second []int
	}{
		{[]int{1, 2, 3, 4, 5}, []int{1, 2, 3}, []int{4, 5}},
		{[]int{7, 8}, []int{7}, []int{8}},
		{[]int{3, 9, 10}, []int{3, 9}, []int{10}},
	}
	for _, tc := range cases {
		a, b := Part{Pages: tc.pages}.Halve()
		if !slices.Equal(a.Pages, tc.first) || !slices.Equal(b.Pages, tc.second) {
			t.Errorf("Halve(%v) = %v + %v, want %v + %v", tc.pages, a.Pages, b.Pages, tc.first, tc.second)
		}
	}
}

func TestHalvePanicsOnASinglePage(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Halve of one page did not panic")
		}
	}()
	Part{Pages: []int{4}}.Halve()
}

func TestPartLabel(t *testing.T) {
	if got := (Part{Pages: []int{3, 4, 5, 9}}).Label(); got != "3-5,9" {
		t.Errorf("Label = %q, want %q", got, "3-5,9")
	}
}
