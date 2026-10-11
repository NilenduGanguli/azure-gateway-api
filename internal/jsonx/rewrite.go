package jsonx

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrSyntax reports input that is not well-formed JSON, or that exceeds one of the documented
// limits. A failing reader or writer is reported with its own error instead, so a caller can tell
// a bad document from a bad disk with errors.Is.
var ErrSyntax = errors.New("jsonx: malformed JSON")

// PathElem is one step into a JSON value: an object key, or an array index.
type PathElem struct {
	Key     string
	Index   int
	IsIndex bool
}

// Kind is a scalar's JSON type.
type Kind int

// The scalar kinds a Visitor is shown. Bool covers both true and false.
const (
	String Kind = iota
	Number
	Bool
	Null
	// LongString is a string longer than MaxVisitString. It is reported after it has been
	// copied, with raw nil, and the visitor's return is ignored.
	LongString
)

// Visitor sees every scalar. It returns replacement raw JSON bytes and true to replace, or
// false to copy the original bytes. Strings are passed raw, with quotes and escapes intact.
//
// Strings longer than MaxVisitString are copied without a visit: once such a string has gone
// through, the visitor is told of it as a LongString with raw nil, and whatever it returns is
// ignored. A visitor that must see every value at some path can refuse one there.
//
// path and raw are only valid during the call: both are reused for the next scalar, so the
// visitor must neither keep nor modify them. The replacement is written before the next call,
// so a visitor may build every replacement in one reused buffer. It is written as given; the
// visitor is responsible for it being valid JSON.
type Visitor func(path []PathElem, kind Kind, raw []byte) ([]byte, bool)

// MaxVisitString is the longest string, in raw bytes with its quotes, that is passed to a
// Visitor. Longer strings are streamed through, so a 100 MB content string costs one buffer, and
// reported as a LongString.
// Numbers longer than this are rejected as ErrSyntax: none is a real offset, and a visitor that
// must see every offset cannot be handed a number it would not see.
const MaxVisitString = 4096

const (
	// maxDepth matches encoding/json, so the two accept the same documents.
	maxDepth = 10000
	// The read and write buffers are small because a merge makes several passes over each of
	// hundreds of parts: the per-call cost matters more than the per-byte cost here.
	readBufSize  = 8 << 10
	writeBufSize = 8 << 10
	// maxInterned bounds the member-name cache. The names in these documents are a few dozen
	// identifiers; the cap only stops hostile input from growing the cache without limit.
	maxInterned = 512
)

// Rewrite copies one JSON value from r to w byte for byte, applying visit. The path given to
// the visitor starts with prefix. Whitespace before, inside and after the value is copied too,
// so a visitor that never replaces anything reproduces the input exactly. Anything but
// whitespace after the value is an error. On error, w may have received part of the output.
//
// A nil visit copies and validates.
//
// Memory is bounded whatever the size of the value: a fixed read buffer, at most one scalar
// held for the visitor, and a path as deep as the value is nested. Objects and arrays are never
// materialised.
func Rewrite(w io.Writer, r io.Reader, prefix []PathElem, visit Visitor) error {
	wk := getWalker(r, w, prefix, visit)
	defer putWalker(wk)
	wk.emit = true
	if err := wk.value(); err != nil {
		return err
	}
	if err := wk.finish(); err != nil {
		return err
	}
	return wk.close()
}

// ArrayItems copies the items of the JSON array read from r to w, rewritten, separated by commas
// and without the enclosing brackets. It returns the number of items written. Paths start at
// prefix + the item index (offset by base, so a caller can number items across parts).
//
// The brackets and the whitespace between the brackets and the items are dropped, and a single
// comma is written between items, so the items of several arrays can be spliced into one. Each
// item's own bytes, including whitespace inside it, are kept.
func ArrayItems(w io.Writer, r io.Reader, prefix []PathElem, base int, visit Visitor) (int, error) {
	wk := getWalker(r, w, prefix, visit)
	defer putWalker(wk)
	n, err := wk.items(base)
	if err != nil {
		return n, err
	}
	return n, wk.close()
}

// walker is a byte-level JSON scanner that copies what it consumes to an optional writer.
//
// It reads through its own buffer rather than a bufio.Reader so that copying is a matter of
// remembering where the pending run began: bytes from mark to pos have been consumed but not yet
// written, and they go out in one write when the buffer is refilled, when a scalar must be held
// back for the visitor, or at the end. A document that needs no replacement is therefore copied
// in buffer-sized writes, not a call per byte.
type walker struct {
	src  io.Reader
	buf  []byte
	pos  int   // next unread byte
	end  int   // end of valid data in buf
	base int64 // input offset of buf[0]
	eof  bool
	err  error // sticky: the first read or write failure

	out   *bufio.Writer // nil: output is discarded
	owned bool          // out came from writerPool, so it is flushed and returned
	emit  bool          // consumed bytes are copied to out
	mark  int           // start of the consumed, unwritten run

	visit  Visitor
	path   []PathElem
	scalar []byte // the scalar being held for the visitor, raw
	name   []byte // the member name being read, raw
	keep   int    // what scanString retains, one of keepNone, keepScalar, keepName

	// spilled reports that the string being scanned outgrew MaxVisitString and went out
	// unvisited.
	spilled bool

	names map[string]string // raw member name, quotes included -> decoded name
}

// What scanString retains of the bytes it consumes.
const (
	keepNone   = iota // nothing; the bytes are copied or dropped with everything else
	keepScalar        // into w.scalar for the visitor, spilling past MaxVisitString
	keepName          // into w.name, failing past maxNameBytes
)

var walkerPool = sync.Pool{New: func() any {
	return &walker{buf: make([]byte, readBufSize), names: make(map[string]string)}
}}

var writerPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, writeBufSize) }}

func getWalker(r io.Reader, w io.Writer, prefix []PathElem, visit Visitor) *walker {
	wk := walkerPool.Get().(*walker)
	wk.src = r
	wk.pos, wk.end, wk.mark, wk.base = 0, 0, 0, 0
	wk.eof, wk.err, wk.emit = false, nil, false
	wk.visit = visit
	wk.path = append(wk.path[:0], prefix...)
	wk.scalar = wk.scalar[:0]
	wk.name = wk.name[:0]
	wk.out, wk.owned = outWriter(w)
	return wk
}

func putWalker(wk *walker) {
	if wk.owned {
		wk.out.Reset(nil)
		writerPool.Put(wk.out)
	}
	wk.out, wk.owned, wk.src, wk.visit = nil, false, nil, nil
	if cap(wk.path) > 64 {
		wk.path = nil // a freak nesting depth is not worth keeping in the pool
	}
	walkerPool.Put(wk)
}

// outWriter picks where output goes. A caller's *bufio.Writer is used as it is, so a merge that
// splices many arrays into one buffered file pays for one buffer, not one per call; it is the
// caller's to flush, like any writer. io.Discard and nil skip output altogether.
func outWriter(w io.Writer) (*bufio.Writer, bool) {
	switch bw := w.(type) {
	case nil:
		return nil, false
	case *bufio.Writer:
		return bw, false
	}
	if w == io.Discard {
		return nil, false
	}
	bw := writerPool.Get().(*bufio.Writer)
	bw.Reset(w)
	return bw, true
}

func (w *walker) offset() int64 { return w.base + int64(w.pos) }

func (w *walker) syntax(format string, args ...any) error {
	return fmt.Errorf("%w: %s at offset %d", ErrSyntax, fmt.Sprintf(format, args...), w.offset())
}

// eofError explains running out of input where more was required: a read failure if there was
// one, otherwise truncated input.
func (w *walker) eofError() error {
	if w.err != nil {
		return w.err
	}
	return w.syntax("unexpected end of input")
}

// more makes at least one unread byte available, refilling the buffer if it is exhausted. It
// returns false at the end of the input or after a failure.
func (w *walker) more() bool {
	if w.pos < w.end {
		return true
	}
	if w.eof || w.err != nil {
		return false
	}
	w.flushRun()
	if w.err != nil {
		return false
	}
	w.base += int64(w.end)
	w.pos, w.end, w.mark = 0, 0, 0
	for empty := 0; ; empty++ {
		n, err := w.src.Read(w.buf)
		w.end = n
		if err != nil {
			if err == io.EOF {
				w.eof = true
			} else {
				w.err = fmt.Errorf("jsonx: read: %w", err)
			}
			return n > 0
		}
		if n > 0 {
			return true
		}
		if empty >= 100 {
			w.err = fmt.Errorf("jsonx: read: %w", io.ErrNoProgress)
			return false
		}
	}
}

// flushRun writes the pending run if output is on, and starts a new one at pos.
func (w *walker) flushRun() {
	if w.emit && w.out != nil && w.pos > w.mark && w.err == nil {
		if _, err := w.out.Write(w.buf[w.mark:w.pos]); err != nil {
			w.err = fmt.Errorf("jsonx: write: %w", err)
		}
	}
	w.mark = w.pos
}

// setEmit turns copying of consumed bytes on or off at the current position.
func (w *walker) setEmit(on bool) {
	if on == w.emit {
		return
	}
	w.flushRun()
	w.emit = on
}

// write sends bytes that are not a copy of the input: a replacement, a held scalar, a comma.
func (w *walker) write(p []byte) {
	if w.out == nil || w.err != nil {
		return
	}
	w.flushRun()
	if _, err := w.out.Write(p); err != nil {
		w.err = fmt.Errorf("jsonx: write: %w", err)
	}
}

// close writes what is pending and flushes an owned writer.
func (w *walker) close() error {
	w.flushRun()
	if w.err == nil && w.owned {
		if err := w.out.Flush(); err != nil {
			w.err = fmt.Errorf("jsonx: write: %w", err)
		}
	}
	return w.err
}

// skipSpace consumes whitespace and returns the next byte without consuming it.
func (w *walker) skipSpace() (byte, error) {
	for {
		for w.pos < w.end {
			c := w.buf[w.pos]
			if !isSpace(c) {
				return c, nil
			}
			w.pos++
		}
		if !w.more() {
			return 0, w.eofError()
		}
	}
}

// finish consumes trailing whitespace and requires the input to end there.
func (w *walker) finish() error {
	for {
		for w.pos < w.end {
			if c := w.buf[w.pos]; !isSpace(c) {
				return w.syntax("unexpected %q after the value", c)
			}
			w.pos++
		}
		if !w.more() {
			return w.err
		}
	}
}

// value consumes exactly one JSON value, copying or rewriting it as configured, and leaves the
// path as it found it.
//
// It is iterative: the path doubles as the stack of open containers (an index element is an
// array, a key element an object), so nesting costs one PathElem per level and no recursion.
func (w *walker) value() error {
	floor := len(w.path)
	for {
		c, err := w.skipSpace()
		if err != nil {
			return err
		}
		complete := true // whether a whole value was consumed, as opposed to a container opened
		switch {
		case c == '{' || c == '[':
			if len(w.path)-floor >= maxDepth {
				return w.syntax("nesting deeper than %d levels", maxDepth)
			}
			w.pos++
			w.path = append(w.path, PathElem{IsIndex: c == '['})
			next, err := w.skipSpace()
			if err != nil {
				return err
			}
			switch {
			case c == '{' && next == '}', c == '[' && next == ']':
				w.pos++
				w.path = w.path[:len(w.path)-1]
			case c == '{':
				if err := w.member(); err != nil {
					return err
				}
				complete = false
			default:
				complete = false
			}
		case c == '"':
			err = w.stringValue()
		case c == 't':
			err = w.literal("true", Bool)
		case c == 'f':
			err = w.literal("false", Bool)
		case c == 'n':
			err = w.literal("null", Null)
		case c == '-' || isDigit(c):
			err = w.number()
		default:
			return w.syntax("unexpected %q where a value should start", c)
		}
		if err != nil {
			return err
		}
		if !complete {
			continue // the first member or item of the container just opened
		}
		// A value has ended: close every container it completes, then move to the next sibling.
		for {
			if len(w.path) == floor {
				return nil
			}
			c, err := w.skipSpace()
			if err != nil {
				return err
			}
			top := &w.path[len(w.path)-1]
			if c == ',' {
				w.pos++
				if top.IsIndex {
					top.Index++
				} else if err := w.member(); err != nil {
					return err
				}
				break
			}
			if top.IsIndex && c == ']' || !top.IsIndex && c == '}' {
				w.pos++
				w.path = w.path[:len(w.path)-1]
				continue
			}
			if top.IsIndex {
				return w.syntax("expected ',' or ']' after an array item, found %q", c)
			}
			return w.syntax("expected ',' or '}' after an object member, found %q", c)
		}
	}
}

// member consumes a member name and its colon, and records the name in the innermost path
// element.
func (w *walker) member() error {
	c, err := w.skipSpace()
	if err != nil {
		return err
	}
	if c != '"' {
		return w.syntax("expected a member name, found %q", c)
	}
	name, err := w.readName(w.visit != nil)
	if err != nil {
		return err
	}
	w.path[len(w.path)-1].Key = name
	if c, err = w.skipSpace(); err != nil {
		return err
	}
	if c != ':' {
		return w.syntax("expected ':' after a member name, found %q", c)
	}
	w.pos++
	return nil
}

// readName consumes a member name, copying it like any other input, and returns it decoded when
// decode is set. Names are interned, so a document with a million words costs no allocation per
// "content" or "span" key.
func (w *walker) readName(decode bool) (string, error) {
	w.name = w.name[:0]
	w.keep = keepName
	if err := w.scanString(); err != nil {
		return "", err
	}
	if !decode {
		return "", nil
	}
	if s, ok := w.names[string(w.name)]; ok {
		return s, nil
	}
	var s string
	if bytes.IndexByte(w.name, '\\') < 0 {
		s = string(w.name[1 : len(w.name)-1])
	} else if err := json.Unmarshal(w.name, &s); err != nil {
		return "", w.syntax("member name %s: %v", w.name, err)
	}
	if len(w.names) < maxInterned {
		w.names[string(w.name)] = s
	}
	return s, nil
}

// stringValue consumes a string value. With a visitor it is held back, visited and written,
// unless it outgrows MaxVisitString, in which case it spills: what was held is written, the rest
// is copied through, and the visitor is told of it as a LongString.
func (w *walker) stringValue() error {
	if w.visit == nil {
		w.keep = keepNone
		return w.scanString()
	}
	w.setEmit(false)
	w.scalar = w.scalar[:0]
	w.keep = keepScalar
	w.spilled = false
	if err := w.scanString(); err != nil {
		return err
	}
	if w.spilled {
		w.visit(w.path[:len(w.path):len(w.path)], LongString, nil)
		return w.err
	}
	return w.visitScalar(String)
}

// scanString consumes the string whose opening quote is the current byte, validating escapes
// and rejecting raw control characters as encoding/json does. Runs of ordinary bytes are
// consumed in bulk.
func (w *walker) scanString() error {
	if err := w.take(w.pos + 1); err != nil { // the opening quote
		return err
	}
	for {
		if w.pos == w.end && !w.more() {
			return w.eofError()
		}
		i := w.pos
		for i < w.end {
			if c := w.buf[i]; c == '"' || c == '\\' || c < 0x20 {
				break
			}
			i++
		}
		if err := w.take(i); err != nil {
			return err
		}
		if i == w.end {
			continue
		}
		switch c := w.buf[i]; {
		case c == '"':
			return w.take(i + 1)
		case c < 0x20:
			return w.syntax("control character %#02x in a string", c)
		}
		// A backslash: one escape character, or u and four hex digits.
		if err := w.take(i + 1); err != nil {
			return err
		}
		if !w.more() {
			return w.eofError()
		}
		switch w.buf[w.pos] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			if err := w.take(w.pos + 1); err != nil {
				return err
			}
		case 'u':
			if err := w.take(w.pos + 1); err != nil {
				return err
			}
			for j := 0; j < 4; j++ {
				if !w.more() {
					return w.eofError()
				}
				if !isHex(w.buf[w.pos]) {
					return w.syntax("invalid \\u escape")
				}
				if err := w.take(w.pos + 1); err != nil {
					return err
				}
			}
		default:
			return w.syntax("invalid escape \\%c", w.buf[w.pos])
		}
	}
}

// take consumes buf[pos:i], retaining it as w.keep says.
func (w *walker) take(i int) error {
	run := w.buf[w.pos:i]
	switch w.keep {
	case keepScalar:
		if len(w.scalar)+len(run) > MaxVisitString {
			// Too long to visit. Write what was held and turn copying back on from the start of
			// this run, so the rest of the string streams through like any other input.
			w.write(w.scalar)
			w.scalar = w.scalar[:0]
			w.emit = true
			w.mark = w.pos
			w.keep = keepNone
			w.spilled = true
		} else {
			w.scalar = append(w.scalar, run...)
		}
	case keepName:
		if len(w.name)+len(run) > maxNameBytes {
			return w.syntax("member name longer than %d bytes", maxNameBytes)
		}
		w.name = append(w.name, run...)
	}
	w.pos = i
	return nil
}

// number consumes a number, checking it against the JSON grammar. It stops at the first byte that
// cannot continue the number, which the caller then has to accept as a delimiter.
func (w *walker) number() error {
	hold := w.visit != nil
	if hold {
		w.setEmit(false)
		w.scalar = w.scalar[:0]
	}
	state, n := numStart, 0
	for {
		if w.pos == w.end && !w.more() {
			if w.err != nil {
				return w.err
			}
			break // the end of the input ends the number
		}
		c := w.buf[w.pos]
		next, ok := numberStep(state, c)
		if !ok {
			break
		}
		if n++; n > MaxVisitString {
			return w.syntax("number longer than %d bytes", MaxVisitString)
		}
		if hold {
			w.scalar = append(w.scalar, c)
		}
		state = next
		w.pos++
	}
	if !numberComplete(state) {
		return w.syntax("malformed number")
	}
	if hold {
		return w.visitScalar(Number)
	}
	return nil
}

// literal consumes true, false or null.
func (w *walker) literal(lit string, kind Kind) error {
	hold := w.visit != nil
	if hold {
		w.setEmit(false)
	}
	for i := 0; i < len(lit); i++ {
		if w.pos == w.end && !w.more() {
			return w.eofError()
		}
		if w.buf[w.pos] != lit[i] {
			return w.syntax("invalid literal, expected %s", lit)
		}
		w.pos++
	}
	if hold {
		w.scalar = append(w.scalar[:0], lit...)
		return w.visitScalar(kind)
	}
	return nil
}

// visitScalar offers the held scalar to the visitor, writes the outcome and resumes copying.
//
// The path is passed with its capacity clipped, so a visitor that appends to it gets a copy
// instead of writing into the walker's own stack.
func (w *walker) visitScalar(kind Kind) error {
	if rep, ok := w.visit(w.path[:len(w.path):len(w.path)], kind, w.scalar); ok {
		w.write(rep)
	} else {
		w.write(w.scalar)
	}
	w.setEmit(true)
	return w.err
}

// items consumes an array that is the whole input, writing its items comma-separated without the
// brackets or the whitespace around the items. Output is off on entry and between items.
func (w *walker) items(base int) (int, error) {
	n, err := w.array(base)
	if err != nil {
		return n, err
	}
	return n, w.finish()
}

// array consumes one array as items does, and stops after its closing bracket.
func (w *walker) array(base int) (int, error) {
	c, err := w.skipSpace()
	if err != nil {
		return 0, err
	}
	if c != '[' {
		return 0, w.syntax("expected an array, found %q", c)
	}
	w.pos++
	if c, err = w.skipSpace(); err != nil {
		return 0, err
	}
	if c == ']' {
		w.pos++
		return 0, nil
	}
	w.path = append(w.path, PathElem{Index: base, IsIndex: true})
	n := 0
	for {
		if n > 0 {
			w.write(comma)
		}
		if _, err := w.skipSpace(); err != nil {
			return n, err
		}
		w.setEmit(true)
		if err := w.value(); err != nil {
			return n, err
		}
		w.setEmit(false)
		n++
		c, err := w.skipSpace()
		if err != nil {
			return n, err
		}
		if c == ']' {
			w.pos++
			break
		}
		if c != ',' {
			return n, w.syntax("expected ',' or ']' after an array item, found %q", c)
		}
		w.pos++
		w.path[len(w.path)-1].Index++
	}
	w.path = w.path[:len(w.path)-1]
	return n, nil
}

var comma = []byte{','}

// The states of the number grammar: -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
const (
	numStart = iota
	numSign  // after '-'
	numZero  // a leading 0, which nothing but '.', 'e' or the end may follow
	numInt   // integer digits
	numDot   // after '.'
	numFrac  // fraction digits
	numE     // after 'e'
	numESign // after the exponent's sign
	numExp   // exponent digits
)

// numberStep advances the number grammar by c, or reports that c does not continue the number.
func numberStep(state int, c byte) (int, bool) {
	digit := isDigit(c)
	switch state {
	case numStart:
		if c == '-' {
			return numSign, true
		}
		fallthrough
	case numSign:
		if c == '0' {
			return numZero, true
		}
		if digit {
			return numInt, true
		}
	case numZero:
		if c == '.' {
			return numDot, true
		}
		if c == 'e' || c == 'E' {
			return numE, true
		}
	case numInt:
		if digit {
			return numInt, true
		}
		if c == '.' {
			return numDot, true
		}
		if c == 'e' || c == 'E' {
			return numE, true
		}
	case numDot:
		if digit {
			return numFrac, true
		}
	case numFrac:
		if digit {
			return numFrac, true
		}
		if c == 'e' || c == 'E' {
			return numE, true
		}
	case numE:
		if c == '+' || c == '-' {
			return numESign, true
		}
		if digit {
			return numExp, true
		}
	case numESign, numExp:
		if digit {
			return numExp, true
		}
	}
	return state, false
}

func numberComplete(state int) bool {
	return state == numZero || state == numInt || state == numFrac || state == numExp
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool { return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
