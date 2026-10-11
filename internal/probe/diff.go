package probe

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Level says who decides a difference between a whole and a split analysis.
//
// The distinction is the go-live gate of the splitting spec (§6). What the service decides page
// by page must come out of a split identical to the whole document's, or the merge is wrong; what
// it decides over the whole document (which heading is the title, how sections nest across a
// split point) can legitimately differ, and each such pattern gets a merge rule or the user's
// explicit acceptance.
type Level string

const (
	// PageLevel is a difference in something a page's own analysis decides: anything under
	// pages or readResults, a bounding region, a figure, a table's cells.
	PageLevel Level = "page-level"
	// DocumentLevel is everything else: paragraph roles, sections, the table count, styles, and
	// content at part boundaries.
	DocumentLevel Level = "document-level"
)

// Difference is one value that differs between the whole and the split analysis.
type Difference struct {
	// File is the document compared, when there were several; Diff leaves it empty.
	File string
	// Path is where the difference is, with real indices: paragraphs[5].role.
	Path string
	// Pattern is Path with every index written [*]: paragraphs[*].role.
	Pattern string
	Level   Level
	// Whole and Split render the two values as JSON, absentText where a side has no such member,
	// or "<n> items" where an array's lengths differ.
	Whole, Split string
}

// absentText renders the side of a difference that has no such member.
const absentText = "(absent)"

// maxExamples bounds the examples a pattern keeps: enough to find the cases, few enough that a
// corpus of thousands of differences still prints a readable summary.
const maxExamples = 3

// maxShown bounds one rendered value in an example.
const maxShown = 80

// Diff compares two JSON documents value by value and returns where they differ, in path order.
//
// Member order is ignored: JSON objects are unordered and the merge's member order is checked by
// its own tests. Numbers are compared by value, so 8.5 and 8.50 are the same number. An array
// whose lengths differ is one difference at the array, and its common prefix is still compared
// item by item.
func Diff(whole, split []byte) ([]Difference, error) {
	w, err := decodeJSON(whole)
	if err != nil {
		return nil, fmt.Errorf("probe: decode the whole result: %w", err)
	}
	s, err := decodeJSON(split)
	if err != nil {
		return nil, fmt.Errorf("probe: decode the split result: %w", err)
	}
	var out []Difference
	walk(nil, w, s, &out)
	return out, nil
}

// decodeJSON decodes exactly one JSON value, keeping numbers as written.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// segment is one step of a path: an object member or an array index.
type segment struct {
	key   string
	index int // -1 for a member
}

// absent stands for the missing side of a member only one document has.
type absent struct{}

func walk(path []segment, w, s any, out *[]Difference) {
	switch wv := w.(type) {
	case map[string]any:
		sv, ok := s.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(wv)+len(sv))
		for k := range wv {
			keys = append(keys, k)
		}
		for k := range sv {
			if _, dup := wv[k]; !dup {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			a, inW := wv[k]
			b, inS := sv[k]
			if !inW {
				a = absent{}
			}
			if !inS {
				b = absent{}
			}
			walk(append(path, segment{key: k, index: -1}), a, b, out)
		}
		return
	case []any:
		sv, ok := s.([]any)
		if !ok {
			break
		}
		if len(wv) != len(sv) {
			*out = append(*out, newDifference(path,
				strconv.Itoa(len(wv))+" items", strconv.Itoa(len(sv))+" items"))
		}
		for i := range min(len(wv), len(sv)) {
			walk(append(path, segment{index: i}), wv[i], sv[i], out)
		}
		return
	}
	if !sameScalar(w, s) {
		*out = append(*out, newDifference(path, render(w), render(s)))
	}
}

// sameScalar compares two values that are not both objects or both arrays.
func sameScalar(w, s any) bool {
	switch wv := w.(type) {
	case json.Number:
		sv, ok := s.(json.Number)
		if !ok {
			return false
		}
		if wv == sv {
			return true
		}
		a, errA := wv.Float64()
		b, errB := sv.Float64()
		return errA == nil && errB == nil && a == b
	case string, bool, nil:
		return w == s
	}
	// An object or array against anything else, or a member against absent.
	return false
}

func newDifference(path []segment, whole, split string) Difference {
	var concrete, pattern strings.Builder
	keys := make([]string, 0, len(path))
	for _, seg := range path {
		if seg.index >= 0 {
			concrete.WriteString("[" + strconv.Itoa(seg.index) + "]")
			pattern.WriteString("[*]")
			continue
		}
		if concrete.Len() > 0 {
			concrete.WriteByte('.')
			pattern.WriteByte('.')
		}
		concrete.WriteString(seg.key)
		pattern.WriteString(seg.key)
		keys = append(keys, seg.key)
	}
	p := concrete.String()
	if p == "" {
		p = "(root)"
	}
	pat := pattern.String()
	if pat == "" {
		pat = "(root)"
	}
	return Difference{Path: p, Pattern: pat, Level: levelOf(keys), Whole: whole, Split: split}
}

// levelOf labels a path by its member names, per the splitting spec §6: anything under pages
// (readResults on Read), a bounding region, a figure, or a table's cells is page-level.
func levelOf(keys []string) Level {
	if len(keys) == 0 {
		return DocumentLevel
	}
	switch keys[0] {
	case "pages", "readResults", "figures":
		return PageLevel
	}
	if slices.Contains(keys, "boundingRegions") {
		return PageLevel
	}
	if keys[0] == "tables" && slices.Contains(keys, "cells") {
		return PageLevel
	}
	return DocumentLevel
}

// render shows a value in an example, as JSON and bounded.
func render(v any) string {
	if _, ok := v.(absent); ok {
		return absentText
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	s := strings.TrimSuffix(b.String(), "\n")
	if len(s) > maxShown {
		s = s[:maxShown] + "…"
	}
	return s
}

// Pattern is every difference at one path pattern, with the first few as examples.
type Pattern struct {
	Pattern  string   `json:"pattern"`
	Level    Level    `json:"level"`
	Count    int      `json:"count"`
	Examples []string `json:"examples"`
}

// Group gathers differences by pattern: page-level patterns first, since they are the ones that
// fail the gate, then the most frequent, then by name.
func Group(diffs []Difference) []Pattern {
	byPattern := map[string]*Pattern{}
	var order []*Pattern
	for _, d := range diffs {
		key := string(d.Level) + "\x00" + d.Pattern
		p := byPattern[key]
		if p == nil {
			p = &Pattern{Pattern: d.Pattern, Level: d.Level, Examples: []string{}}
			byPattern[key] = p
			order = append(order, p)
		}
		p.Count++
		if len(p.Examples) < maxExamples {
			p.Examples = append(p.Examples, example(d))
		}
	}
	out := make([]Pattern, len(order))
	for i, p := range order {
		out[i] = *p
	}
	slices.SortStableFunc(out, func(a, b Pattern) int {
		if a.Level != b.Level {
			if a.Level == PageLevel {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(b.Count, a.Count); c != 0 {
			return c
		}
		return cmp.Compare(a.Pattern, b.Pattern)
	})
	return out
}

// example renders one difference for a report.
func example(d Difference) string {
	s := d.Path + ": whole " + d.Whole + ", split " + d.Split
	if d.File != "" {
		s = d.File + ": " + s
	}
	return s
}
