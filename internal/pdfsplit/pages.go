package pdfsplit

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// PageRange is one term of Azure's pages grammar.
type PageRange struct{ First, Last int }

// ParsePages parses "1-3,5,7-9" (whitespace around terms tolerated). ok is false for anything
// else, including open ranges, zero, or First > Last.
//
// A client's pages= value decides which pages are split, so anything this parser is unsure of
// is rejected rather than guessed at: the document is then sent whole with the client's value
// untouched, and Azure gives its own answer about it.
func ParsePages(spec string) (ranges []PageRange, ok bool) {
	terms := strings.Split(spec, ",")
	ranges = make([]PageRange, 0, len(terms))
	for _, term := range terms {
		first, last, isRange := strings.Cut(term, "-")
		a, ok := pageNumber(first)
		if !ok {
			return nil, false
		}
		b := a
		if isRange {
			if b, ok = pageNumber(last); !ok || b < a {
				return nil, false
			}
		}
		ranges = append(ranges, PageRange{First: a, Last: b})
	}
	return ranges, true
}

// pageNumber parses one page number: ASCII digits, optionally surrounded by whitespace, at
// least 1. strconv.Atoi alone would also take a sign.
func pageNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// Select expands ranges into ascending, de-duplicated page numbers, all within 1..count. ok is
// false when there are no ranges, or when any range is malformed or reaches past count.
//
// Ranges are sorted and merged rather than marked page by page, so the work is bounded by count
// however many overlapping ranges a client sends.
func Select(ranges []PageRange, count int) (pages []int, ok bool) {
	if len(ranges) == 0 {
		return nil, false
	}
	for _, r := range ranges {
		if r.First < 1 || r.First > r.Last || r.Last > count {
			return nil, false
		}
	}
	sorted := slices.Clone(ranges)
	slices.SortFunc(sorted, func(a, b PageRange) int { return cmp.Compare(a.First, b.First) })

	next := 1 // the lowest page not selected yet
	for _, r := range sorted {
		for p := max(r.First, next); p <= r.Last; p++ {
			pages = append(pages, p)
		}
		next = max(next, r.Last+1)
	}
	return pages, true
}

// FormatPages renders pages in the grammar, collapsing runs: [3 4 5 9] -> "3-5,9".
func FormatPages(pages []int) string {
	var b strings.Builder
	for i := 0; i < len(pages); {
		j := i
		for j+1 < len(pages) && pages[j+1] == pages[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(pages[i]))
		if j > i {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(pages[j]))
		}
		i = j + 1
	}
	return b.String()
}
