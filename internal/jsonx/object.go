package jsonx

import (
	"fmt"
	"io"
)

// Member is one top-level member of an object: its name and the byte range of its value.
type Member struct {
	Name       string
	Start, End int64
}

// Members lists the top-level members of the object occupying ra[start:end], in order.
//
// Name is the decoded member name. [Start, End) is the value's exact byte range in ra, without the
// whitespace around it, ready for io.NewSectionReader. Every value is walked and validated but
// nothing is retained, so a 500 MB member costs a scan. Whitespace may surround the object, but
// anything else in the range is an error.
func Members(ra io.ReaderAt, start, end int64) ([]Member, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("jsonx: invalid range [%d,%d)", start, end)
	}
	w := getWalker(io.NewSectionReader(ra, start, end-start), nil, nil, nil)
	defer putWalker(w)
	w.base = start

	c, err := w.skipSpace()
	if err != nil {
		return nil, err
	}
	if c != '{' {
		return nil, fmt.Errorf("%w (found %q at offset %d)", ErrNotObject, c, w.offset())
	}
	w.pos++
	if c, err = w.skipSpace(); err != nil {
		return nil, err
	}
	var members []Member
	if c == '}' {
		w.pos++
		return members, w.finish()
	}
	for {
		if c, err = w.skipSpace(); err != nil {
			return nil, err
		}
		if c != '"' {
			return nil, w.syntax("expected a member name, found %q", c)
		}
		name, err := w.readName(true)
		if err != nil {
			return nil, err
		}
		if c, err = w.skipSpace(); err != nil {
			return nil, err
		}
		if c != ':' {
			return nil, w.syntax("expected ':' after a member name, found %q", c)
		}
		w.pos++
		if _, err := w.skipSpace(); err != nil {
			return nil, err
		}
		valueStart := w.offset()
		if err := w.value(); err != nil {
			return nil, err
		}
		members = append(members, Member{Name: name, Start: valueStart, End: w.offset()})

		if c, err = w.skipSpace(); err != nil {
			return nil, err
		}
		switch c {
		case ',':
			w.pos++
		case '}':
			w.pos++
			return members, w.finish()
		default:
			return nil, w.syntax("expected ',' or '}' after an object member, found %q", c)
		}
	}
}

// CountItems counts the items of the array at ra[start:end] without materialising them. The
// array is validated; whitespace may surround it, but anything else in the range is an error.
func CountItems(ra io.ReaderAt, start, end int64) (int, error) {
	if start < 0 || end < start {
		return 0, fmt.Errorf("jsonx: invalid range [%d,%d)", start, end)
	}
	w := getWalker(io.NewSectionReader(ra, start, end-start), nil, nil, nil)
	defer putWalker(w)
	w.base = start
	n, err := w.items(0)
	if err != nil {
		return 0, err
	}
	return n, nil
}
