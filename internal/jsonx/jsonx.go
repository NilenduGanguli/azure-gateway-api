// Package jsonx locates the byte range of a member inside a large JSON object without holding the
// value in memory.
//
// Azure documents a maximum OCR JSON response of 500 MB. The gateway needs the analyzeResult
// member out of an upstream response so it can compose its own envelope around it. Decoding into a
// json.RawMessage would buffer that whole member per in-flight job, so a pod running four workers
// could hold gigabytes. Instead the upstream body is streamed to a temp file and this package finds
// where the member's value starts and ends, letting the caller copy the range straight through with
// an io.SectionReader.
//
// The scan is byte-level rather than encoding/json's tokenizer, and that is the whole point.
// json.Decoder.Token() streams the four structural delimiters but materialises every other token:
// it grows its internal buffer until an entire scalar fits, then allocates the decoded Go string on
// top. The largest member of a Document Intelligence analyzeResult is `content`, the extracted text
// of the whole document as one JSON string, so a tokenizer-based skip spiked roughly three times
// that string's size in heap — measured at 309 MB peak for a 107 MB string — and four concurrent
// workers were enough to OOM a pod. Counting bytes never allocates more than a member name.
package jsonx

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// ErrNotObject reports that the input's top level is not a JSON object.
var ErrNotObject = errors.New("jsonx: top-level value is not an object")

// maxNameBytes caps a member name. Names in these payloads are short identifiers; anything longer
// is malformed input that must not be allocated.
const maxNameBytes = 4 << 10

// scanner reads a JSON document byte by byte, tracking the absolute offset.
type scanner struct {
	r   *bufio.Reader
	off int64
}

func newScanner(r io.Reader) *scanner {
	return &scanner{r: bufio.NewReaderSize(r, 64<<10)}
}

func (s *scanner) next() (byte, error) {
	c, err := s.r.ReadByte()
	if err != nil {
		return 0, err
	}
	s.off++
	return c, nil
}

// unread pushes the last byte back, so a delimiter that terminated a scalar is not consumed.
func (s *scanner) unread() error {
	if err := s.r.UnreadByte(); err != nil {
		return err
	}
	s.off--
	return nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// skipSpace consumes whitespace and returns the next byte without consuming it.
func (s *scanner) skipSpace() (byte, error) {
	for {
		c, err := s.next()
		if err != nil {
			return 0, err
		}
		if isSpace(c) {
			continue
		}
		if err := s.unread(); err != nil {
			return 0, err
		}
		return c, nil
	}
}

// scanString consumes a string that has already had its opening quote read, honouring escapes.
//
// Only the escape marker itself needs interpreting: a backslash means the next byte cannot end the
// string. \uXXXX needs no special handling, since none of those four hex digits can be a quote.
func (s *scanner) scanString(into []byte) ([]byte, error) {
	for {
		c, err := s.next()
		if err != nil {
			return into, err
		}
		switch c {
		case '"':
			return into, nil
		case '\\':
			esc, err := s.next()
			if err != nil {
				return into, err
			}
			if into != nil && len(into) < maxNameBytes {
				into = append(into, c, esc)
			}
		default:
			if into != nil && len(into) < maxNameBytes {
				into = append(into, c)
			}
		}
	}
}

// skipValue consumes exactly one JSON value and returns the offset just past it.
//
// Nothing is retained: composites are walked with a depth counter and scalars are counted out to
// their terminator, so a 500 MB member costs a scan and no allocation.
func (s *scanner) skipValue() (int64, error) {
	c, err := s.skipSpace()
	if err != nil {
		return 0, err
	}
	switch c {
	case '"':
		if _, err := s.next(); err != nil { // consume the opening quote
			return 0, err
		}
		if _, err := s.scanString(nil); err != nil {
			return 0, err
		}
		return s.off, nil

	case '{', '[':
		depth := 0
		for {
			b, err := s.next()
			if err != nil {
				return 0, err
			}
			switch b {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return s.off, nil
				}
			case '"':
				if _, err := s.scanString(nil); err != nil {
					return 0, err
				}
			}
		}

	default:
		// A number, true, false or null: everything up to the first structural byte or space.
		for {
			b, err := s.next()
			if errors.Is(err, io.EOF) {
				return s.off, nil
			}
			if err != nil {
				return 0, err
			}
			if b == ',' || b == '}' || b == ']' || isSpace(b) {
				if err := s.unread(); err != nil {
					return 0, err
				}
				return s.off, nil
			}
		}
	}
}

// MemberRange scans the top-level object in r for key and returns the half-open byte range
// [start, end) of its value, where start is the value's own first byte.
//
// found is false when the object parses correctly but has no such member. When a name appears more
// than once the first occurrence wins: these containers do not emit duplicate keys, and scanning
// for a later one would mean walking the whole document after the value has already been found.
func MemberRange(r io.Reader, key string) (start, end int64, found bool, err error) {
	s := newScanner(r)

	c, err := s.skipSpace()
	if err != nil {
		return 0, 0, false, fmt.Errorf("jsonx: read opening token: %w", err)
	}
	if c != '{' {
		return 0, 0, false, ErrNotObject
	}
	if _, err := s.next(); err != nil {
		return 0, 0, false, err
	}

	name := make([]byte, 0, 64)
	for {
		c, err := s.skipSpace()
		if err != nil {
			return 0, 0, false, fmt.Errorf("jsonx: read member name: %w", err)
		}
		if c == '}' {
			return 0, 0, false, nil
		}
		if c == ',' {
			if _, err := s.next(); err != nil {
				return 0, 0, false, err
			}
			continue
		}
		if c != '"' {
			return 0, 0, false, fmt.Errorf("jsonx: expected a member name, found %q", c)
		}
		if _, err := s.next(); err != nil { // opening quote
			return 0, 0, false, err
		}
		name = name[:0]
		name, err = s.scanString(name)
		if err != nil {
			return 0, 0, false, fmt.Errorf("jsonx: read member name: %w", err)
		}

		if c, err = s.skipSpace(); err != nil {
			return 0, 0, false, err
		}
		if c != ':' {
			return 0, 0, false, fmt.Errorf("jsonx: expected ':' after a member name, found %q", c)
		}
		if _, err := s.next(); err != nil {
			return 0, 0, false, err
		}

		// The value begins at the first non-whitespace byte after the colon. JSON allows unlimited
		// whitespace there, so this is found by scanning rather than assumed.
		if _, err := s.skipSpace(); err != nil {
			return 0, 0, false, err
		}
		valueStart := s.off
		valueEnd, err := s.skipValue()
		if err != nil {
			return 0, 0, false, fmt.Errorf("jsonx: walk member value: %w", err)
		}
		if string(name) == key {
			return valueStart, valueEnd, true, nil
		}
	}
}

// ValueRange returns the exact byte range of a member's value in ra.
//
// It is the form callers actually want: the result can be handed straight to io.NewSectionReader.
func ValueRange(ra io.ReaderAt, size int64, key string) (start, end int64, found bool, err error) {
	return MemberRange(io.NewSectionReader(ra, 0, size), key)
}

// IsObject reports whether the first non-whitespace byte of lead opens a JSON object. It tells an
// AnalyzeOperation envelope from a bare AnalyzeResult, when a container's synchronous route is
// undocumented and either shape is possible.
func IsObject(lead []byte) bool {
	for _, c := range lead {
		if isSpace(c) {
			continue
		}
		return c == '{'
	}
	return false
}
