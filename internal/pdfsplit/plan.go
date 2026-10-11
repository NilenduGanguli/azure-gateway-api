package pdfsplit

import (
	"fmt"
	"slices"
)

// Part is a run of original pages analysed together, ascending.
//
// The pages are the original document's numbers, which need not be consecutive when the client's
// pages= skipped some. The merge maps each part's results back through this list.
type Part struct{ Pages []int }

// Label is FormatPages(p.Pages), used in logs and file names.
func (p Part) Label() string { return FormatPages(p.Pages) }

// Plan cuts pages into k = ceil(n/max) parts; the first n mod k get one page more.
// With n <= max it returns one part, and an empty selection gives none. max < 1 panics (a
// programming error).
//
// Balancing, rather than filling parts to max and leaving a remainder, keeps every part between
// ceil(max/2) and max pages once a document is split at all: 6 pages become 3+3, never 5+1, so
// no part costs an Azure call for a sliver of the document. Parts never share memory with pages.
func Plan(pages []int, max int) []Part {
	if max < 1 {
		panic(fmt.Sprintf("pdfsplit: Plan needs max >= 1, got %d", max))
	}
	n := len(pages)
	if n == 0 {
		return nil
	}
	k := (n-1)/max + 1 // ceil(n/max); (n+max-1)/max overflows for a huge max
	size, extra := n/k, n%k

	parts := make([]Part, 0, k)
	start := 0
	for i := 0; i < k; i++ {
		end := start + size
		if i < extra {
			end++
		}
		parts = append(parts, Part{Pages: slices.Clone(pages[start:end])})
		start = end
	}
	return parts
}

// Halve splits a part of n >= 2 pages into ceil(n/2) and the rest. A single page cannot be
// halved, so n < 2 panics (a programming error): a single page that keeps failing fails the job.
func (p Part) Halve() (Part, Part) {
	n := len(p.Pages)
	if n < 2 {
		panic(fmt.Sprintf("pdfsplit: cannot halve a part of %d pages", n))
	}
	h := (n + 1) / 2
	return Part{Pages: slices.Clone(p.Pages[:h])}, Part{Pages: slices.Clone(p.Pages[h:])}
}
