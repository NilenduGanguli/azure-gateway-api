// Package jsonx locates the byte range of a member inside a large JSON object without holding
// the value in memory.
//
// Azure documents a maximum OCR JSON response of 500 MB. The gateway needs the analyzeResult
// member out of an upstream response so it can compose its own envelope around it. Decoding into
// a json.RawMessage would buffer that whole member per in-flight job, so a pod running four
// workers could hold gigabytes. Instead the upstream body is streamed to a temp file and this
// package finds where the member's value starts and ends, letting the caller copy the range
// straight through with an io.SectionReader.
//
// Scanning uses encoding/json's own tokenizer rather than hand-rolled parsing: Token() streams
// without buffering composite values, and InputOffset() reports where each token ended.
package jsonx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrNotObject reports that the input's top level is not a JSON object.
var ErrNotObject = errors.New("jsonx: top-level value is not an object")

// MemberRange scans the top-level object in r for key and returns the half-open byte range
// [start, end) of its value.
//
// r is read from its current position, which must be the start of the JSON document. found is
// false when the object parses correctly but has no such member.
func MemberRange(r io.Reader, key string) (start, end int64, found bool, err error) {
	dec := json.NewDecoder(r)

	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, fmt.Errorf("jsonx: read opening token: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false, ErrNotObject
	}

	for dec.More() {
		// At this position the next token is a member name.
		kt, err := dec.Token()
		if err != nil {
			return 0, 0, false, fmt.Errorf("jsonx: read member name: %w", err)
		}
		name, ok := kt.(string)
		if !ok {
			return 0, 0, false, fmt.Errorf("jsonx: member name was %T, not a string", kt)
		}

		// InputOffset now sits just past the closing quote of the name. The value begins after
		// the colon and any whitespace; skipValue reports where it ends.
		afterName := dec.InputOffset()
		valueEnd, err := skipValue(dec)
		if err != nil {
			return 0, 0, false, err
		}
		if name == key {
			return afterName, valueEnd, true, nil
		}
	}
	return 0, 0, false, nil
}

// skipValue consumes exactly one JSON value from dec and returns the offset just past it.
//
// Composite values are walked token by token, so a 500 MB member costs a scan but no allocation.
func skipValue(dec *json.Decoder) (int64, error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, fmt.Errorf("jsonx: read value: %w", err)
	}
	depth := 0
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		depth = 1
	}
	for depth > 0 {
		t, err := dec.Token()
		if err != nil {
			return 0, fmt.Errorf("jsonx: walk value: %w", err)
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return dec.InputOffset(), nil
}

// TrimValueStart advances start past the colon and whitespace separating a member name from its
// value, given the raw bytes at that offset.
//
// MemberRange returns the offset just past the member name, which is the cheapest position for
// the tokenizer to report. Callers that want the value's own first byte pass the leading bytes of
// the range here.
func TrimValueStart(lead []byte) int64 {
	var i int
	for i < len(lead) {
		switch lead[i] {
		case ' ', '\t', '\r', '\n', ':':
			i++
			continue
		}
		break
	}
	return int64(i)
}

// ValueRange combines MemberRange with TrimValueStart, returning the exact byte range of a
// member's value in ra.
//
// It is the form callers actually want: the result can be handed straight to io.NewSectionReader.
func ValueRange(ra io.ReaderAt, size int64, key string) (start, end int64, found bool, err error) {
	start, end, found, err = MemberRange(io.NewSectionReader(ra, 0, size), key)
	if err != nil || !found {
		return 0, 0, found, err
	}

	// The gap between the member name and its value is a colon plus whitespace; a short read is
	// enough to skip it.
	const lookahead = 64
	n := end - start
	if n > lookahead {
		n = lookahead
	}
	lead := make([]byte, n)
	if _, err := ra.ReadAt(lead, start); err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, false, fmt.Errorf("jsonx: read value lead: %w", err)
	}
	start += TrimValueStart(lead)
	if start > end {
		return 0, 0, false, errors.New("jsonx: value range is empty after trimming separator")
	}
	return start, end, true, nil
}

// IsObject reports whether the first non-whitespace byte of lead opens a JSON object. It is used
// to tell an AnalyzeOperation envelope from a bare AnalyzeResult when a container's synchronous
// route is undocumented and either shape is possible.
func IsObject(lead []byte) bool {
	for _, c := range lead {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}
