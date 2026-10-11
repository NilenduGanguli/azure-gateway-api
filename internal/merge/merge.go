// Package merge stitches the results of a split document's parts into the one result the service
// would have returned for the whole document.
//
// A large PDF is analysed as parts of a few pages each. Everything the service decides page by
// page — words, lines, selection marks, the tables and figures on a page, page geometry — is the
// same in a part's result as in the whole document's, except for where it is numbered from: span
// offsets count from the start of the part's own content, pages are numbered within the part,
// and /paragraphs/N style pointers index the part's own arrays. The merge renumbers exactly
// those, per the splitting spec's §5, and copies every other byte as the service wrote it.
//
// It is a streaming rewrite. Each part is measured (member ranges, content length, item counts,
// page numbers), then its arrays are copied through jsonx with a visitor that shifts the targeted
// scalars. Memory is a few buffers per pass whatever the size of the result; nothing but each
// part's content, its root section and the member names is ever decoded.
//
// Before writing anything the merge checks that the parts can be merged exactly: same API
// version, model, unit and content format; non-array members identical; the pages returned
// covering the planned pages once each, in order. References that can only be judged in place —
// a pointer past its part's items, a region on a page outside its part, an offset that is not an
// integer — are checked as they are copied. A failure wraps ErrCheck, and the caller discards the
// output and analyses the whole document instead, so a split never produces a result the service
// would not.
package merge

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// ErrCheck wraps every safety-check failure; the engine falls back to a whole-document analysis.
//
// Malformed part JSON is a check failure too: a part the merge cannot read exactly must not fail
// a document the service could analyse whole. Errors opening or reading part files, or writing
// the output, are returned as they are.
var ErrCheck = errors.New("merge: safety check failed")

// Part is one part's analyzeResult on disk.
type Part struct {
	Path       string
	Start, End int64 // byte range of the bare AnalyzeResult object
	Pages      []int // original pages this part covered, ascending
}

// Stats describes a merged result.
type Stats struct {
	Pages  int            // pages in the merged result
	Bytes  int64          // bytes written
	Counts map[string]int // items in each top-level array of the merged result
}

// profile is what differs between the two surfaces.
type profile struct {
	di        bool     // Document Intelligence: content, spans, pointers and sections to merge
	pageArray string   // the array with one item per page
	pageKey   string   // the page number member of its items
	agree     []string // members that must agree across parts, checked first to name them
}

var (
	diProfile = profile{di: true, pageArray: "pages", pageKey: "pageNumber",
		agree: []string{"apiVersion", "modelId", "stringIndexType", "contentFormat"}}
	readProfile = profile{pageArray: "readResults", pageKey: "page",
		agree: []string{"version", "modelVersion"}}
)

// The members whose items element pointers name, in pointer order.
const (
	ptrParagraphs = iota
	ptrTables
	ptrFigures
	ptrSections
	ptrKeyValuePairs
	numPointers
)

var pointerMembers = [numPointers]string{"paragraphs", "tables", "figures", "sections", "keyValuePairs"}

// part is one part as measured.
type part struct {
	Part
	k       int
	members []jsonx.Member
	isArray []bool         // parallel to members
	counts  map[string]int // items in each array member
	pages   []int          // page numbers as the part's result gives them, in order
	local   bool           // numbered 1..len(Pages), as a physically split part is

	// Document Intelligence only.
	length int     // content length in the unit
	endsCR bool    // content ends with CR, which would join the separator's LF
	lt     ltStyle // how the content writes '<' and '>', if it shows

	// Shifts, set once every part is measured.
	char    int              // content length of all earlier parts, separators included
	ptrBase [numPointers]int // items of each pointer member in earlier parts
	secBase int              // the merged index of this part's section 1

	visit jsonx.Visitor
	verr  *error // the visitor's first check failure
}

func (p *part) member(name string) (jsonx.Member, int, bool) {
	for i, m := range p.members {
		if m.Name == name {
			return m, i, true
		}
	}
	return jsonx.Member{}, -1, false
}

// mapPage maps a page number as the part's result gives it to the original page.
func (p *part) mapPage(n int) (int, bool) {
	if p.local {
		if n < 1 || n > len(p.Pages) {
			return 0, false
		}
		return p.Pages[n-1], true
	}
	for _, q := range p.Pages {
		if q == n {
			return n, true
		}
	}
	return 0, false
}

// merger holds one merge's state.
type merger struct {
	prof  profile
	unit  textunit.Unit
	parts []*part
	order []string        // merged member order
	array map[string]bool // whether each member is an array
	sep   []byte          // the raw page separator between parts' content
	stats Stats

	// Reused across parts so that measuring hundreds of them allocates nothing per part.
	cmpA, cmpB []byte
	content    []byte
}

func newMerger(prof profile, unit textunit.Unit) *merger {
	return &merger{
		prof: prof, unit: unit, array: map[string]bool{},
		cmpA: make([]byte, 4<<10), cmpB: make([]byte, 4<<10),
		stats: Stats{Counts: map[string]int{}},
	}
}

// checkf builds a check failure.
func checkf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCheck, fmt.Sprintf(format, args...))
}

// jsonErr classifies an error from jsonx: input that is not well-formed JSON is a check failure,
// anything else (a read or write failure) is not.
func jsonErr(k int, what string, err error) error {
	if errors.Is(err, jsonx.ErrSyntax) || errors.Is(err, jsonx.ErrNotObject) {
		return fmt.Errorf("%w: part %d: %s: %w", ErrCheck, k, what, err)
	}
	return fmt.Errorf("merge: part %d: %s: %w", k, what, err)
}

// withFile opens a part's file for the duration of fn.
//
// Files are opened per pass rather than held for the whole merge, so a 2,000-page document in
// 400 parts never needs 400 descriptors at once.
func withFile(p *part, fn func(f *os.File) error) error {
	f, err := os.Open(p.Path)
	if err != nil {
		return fmt.Errorf("merge: part %d: %w", p.k, err)
	}
	defer f.Close()
	return fn(f)
}

// readAt fills b from ra at off, treating EOF on a full read as success.
func readAt(ra io.ReaderAt, b []byte, off int64) error {
	n, err := ra.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// countWriter counts what reaches the destination.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// write streams the merged object: members in part 0's order, then any first seen later.
func (m *merger) write(w io.Writer) error {
	cw := &countWriter{w: w}
	bw := bufio.NewWriterSize(cw, 64<<10)
	bw.WriteByte('{')
	for i, name := range m.order {
		if i > 0 {
			bw.WriteByte(',')
		}
		writeName(bw, name)
		bw.WriteByte(':')
		var err error
		switch {
		case m.prof.di && name == "content":
			err = m.writeContent(bw)
		case m.prof.di && name == "sections" && m.array[name]:
			err = m.writeSections(bw)
		case m.array[name]:
			err = m.writeArray(bw, name)
		default:
			// Every part has this member with identical bytes; the checks made sure of it.
			err = m.copyMember(bw, m.parts[0], name)
		}
		if err != nil {
			return err
		}
	}
	bw.WriteByte('}')
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("merge: write: %w", err)
	}
	m.stats.Bytes = cw.n
	return nil
}

// writeName writes a member name as a JSON string. Names in these results are plain identifiers;
// only what JSON requires is escaped.
func writeName(bw *bufio.Writer, name string) {
	const hex = "0123456789abcdef"
	bw.WriteByte('"')
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c == '"' || c == '\\':
			bw.WriteByte('\\')
			bw.WriteByte(c)
		case c < 0x20:
			bw.WriteString(`\u00`)
			bw.WriteByte(hex[c>>4])
			bw.WriteByte(hex[c&0xf])
		default:
			bw.WriteByte(c)
		}
	}
	bw.WriteByte('"')
}

// writeArray concatenates every part's items of an array member, in part order, renumbering each
// part's items with its visitor.
func (m *merger) writeArray(bw *bufio.Writer, name string) error {
	prefix := []jsonx.PathElem{{Key: name}}
	bw.WriteByte('[')
	total := 0
	for _, p := range m.parts {
		mb, _, ok := p.member(name)
		if !ok || p.counts[name] == 0 {
			continue
		}
		if total > 0 {
			bw.WriteByte(',')
		}
		var n int
		err := withFile(p, func(f *os.File) error {
			var err error
			n, err = m.items(bw, p, io.NewSectionReader(f, mb.Start, mb.End-mb.Start), prefix, total)
			return err
		})
		if err != nil {
			return err
		}
		if n != p.counts[name] {
			return checkf("part %d: %s has %d items, but %d were counted", p.k, name, n, p.counts[name])
		}
		total += n
	}
	bw.WriteByte(']')
	m.stats.Counts[name] = total
	return nil
}

// items copies the items of the array read from r through the part's visitor.
func (m *merger) items(bw *bufio.Writer, p *part, r io.Reader, prefix []jsonx.PathElem, base int) (int, error) {
	n, err := jsonx.ArrayItems(bw, r, prefix, base, p.visit)
	if err != nil {
		return n, jsonErr(p.k, prefix[0].Key, err)
	}
	return n, *p.verr
}

// copyMember copies a member's value as it is.
func (m *merger) copyMember(bw *bufio.Writer, p *part, name string) error {
	mb, _, _ := p.member(name)
	return withFile(p, func(f *os.File) error {
		if _, err := io.Copy(bw, io.NewSectionReader(f, mb.Start, mb.End-mb.Start)); err != nil {
			return fmt.Errorf("merge: part %d: copy %s: %w", p.k, name, err)
		}
		return nil
	})
}
