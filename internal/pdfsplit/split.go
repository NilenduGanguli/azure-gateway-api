// Package pdfsplit cuts a large PDF into small parts that the Azure containers analyse in
// parallel (splitting spec §1–§2).
//
// It covers the PDF side of splitting: whether a body is a PDF at all (IsPDF), Azure's pages=
// grammar (ParsePages, Select, FormatPages), balanced parts (Plan, Halve), and the cut itself
// (Splitter).
//
// A document is parsed once and every part is cut from that one model. A part copies the original
// page objects and their resources, so each page renders exactly as it did in the original. Parts
// carry no named destinations (see readPDF).
//
// Uploads are untrusted, and parsing is where that shows: a large file holds its object graph in
// memory, a crafted one can keep pdfcpu busy, and a malformed one can make it panic. So a Splitter
// bounds how many documents are open at once, gives each a time budget, and recovers panics.
// Every failure here has the same remedy, which is to send the document whole, so callers only
// tell the reasons apart for metrics.
package pdfsplit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

var (
	// ErrEncrypted reports an encrypted PDF, including one that opens without a password.
	ErrEncrypted = errors.New("pdfsplit: document is encrypted")
	// ErrUnparseable reports a file pdfcpu cannot read.
	ErrUnparseable = errors.New("pdfsplit: document cannot be parsed")
	// ErrBudget reports that parsing and cutting one document ran past the splitter's budget.
	ErrBudget = errors.New("pdfsplit: split budget exceeded")
	// ErrPanic is the ErrUnparseable for a recovered pdfcpu panic, kept distinct so the fallback
	// can be counted as "panic" (splitting spec §8). errors.Is(err, ErrUnparseable) holds for it.
	ErrPanic = fmt.Errorf("%w: pdfcpu panicked", ErrUnparseable)
)

// encryptionErrors are pdfcpu's answers for an encrypted file it cannot open as given.
var encryptionErrors = []error{
	pdfcpu.ErrWrongPassword,                // a user (open) password is set
	pdfcpu.ErrUnsupportedEncryptionFeature, // e.g. certificate security
	pdfcpu.ErrMalformedEncryption,          // an /Encrypt dictionary pdfcpu cannot use
	pdfcpu.ErrEncrypted,
}

// newConfig returns the pdfcpu configuration for one document.
//
// It must be the stateless configuration. model.NewDefaultConfiguration creates pdfcpu/config.yml
// under the user's config directory on first use and panics when it cannot. The gateway runs as
// nonroot on a read-only root filesystem (OPERATIONS.md), so every split would fall back to whole.
// Relaxed validation accepts the minor spec violations common in real-world PDFs.
func newConfig() *model.Configuration {
	conf := model.NewStatelessConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	return conf
}

// Splitter bounds concurrent parses and the time each may take.
//
// Each open Document holds one slot until Close. A parsed model keeps the file's objects in
// memory, so the slots are what bound the gateway's memory for splitting, whatever the load.
type Splitter struct {
	sem    chan struct{} // one token per open Document
	budget time.Duration
}

// NewSplitter allows concurrency documents open at once, each with budget to parse and cut.
// A concurrency below 1 or a budget that is not positive is a programming error and panics.
func NewSplitter(concurrency int, budget time.Duration) *Splitter {
	if concurrency < 1 || budget <= 0 {
		panic(fmt.Sprintf("pdfsplit: NewSplitter needs concurrency >= 1 and budget > 0, got %d and %v",
			concurrency, budget))
	}
	return &Splitter{sem: make(chan struct{}, concurrency), budget: budget}
}

// Document is a parsed PDF holding one concurrency slot until Close.
type Document struct {
	// PageCount is the number of pages in the document.
	PageCount int

	owner    *Splitter
	deadline time.Time // the end of the budget, shared by Open and every WritePart

	// mu serialises cuts and Close, and guards the fields below. A cut writes into the shared
	// model, so every cut sees what the earlier ones left behind. pdfcpu caches the objects it
	// decodes, which is harmless. It also carries named destinations into a part by patching and
	// pruning the model's /Dests tree in place, which gave later parts destinations pointing at the
	// wrong objects; readPDF removes that tree. A cut that panicked may have left the model
	// half-changed, so failed then refuses every later cut.
	mu     sync.Mutex
	pdf    *model.Context // nil once closed
	file   *os.File       // the model's source, which pdfcpu keeps a reader on
	failed error          // the ErrPanic of a cut that panicked
}

// Open parses the file at path within the budget. Encrypted PDFs give ErrEncrypted; parse
// failures and recovered pdfcpu panics give ErrUnparseable (a panic as ErrPanic); a budget
// overrun gives ErrBudget. It waits for a slot (honouring ctx), and returns ctx.Err() unchanged
// when ctx ends first.
//
// The budget starts once the slot is held. It bounds the work on this document, not the queue in
// front of it, which ctx already bounds. It is one deadline, covering Open and every later
// WritePart on the Document.
//
// An encrypted PDF is refused even when it opens without a password (owner-only encryption):
// the spec splits only unencrypted documents, and a document sent whole has its encryption
// handled by Azure as it always was.
func (s *Splitter) Open(ctx context.Context, path string) (*Document, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	doc, err := s.parse(ctx, path, time.Now().Add(s.budget))
	if err != nil {
		s.release()
		return nil, err
	}
	return doc, nil
}

// acquire waits for a slot.
func (s *Splitter) acquire(ctx context.Context) error {
	// select picks at random among ready cases, so without this check a caller that has already
	// given up could still take a free slot and start parsing.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Splitter) release() { <-s.sem }

// parse reads the PDF at path into a Document that must be done by deadline. The caller holds a
// slot, which the Document takes over.
func (s *Splitter) parse(ctx context.Context, path string, deadline time.Time) (*Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pdfsplit: %w", err)
	}
	c, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	pdf, err := readPDF(ctx, c, f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Document{PageCount: pdf.PageCount, owner: s, deadline: deadline, pdf: pdf, file: f}, nil
}

// readPDF parses rs under c, which is ctx bounded by the document's deadline. A failure of rs
// itself is returned as such, whatever pdfcpu made of it: an I/O error is not the document's
// fault.
func readPDF(ctx, c context.Context, rs io.ReadSeeker) (*model.Context, error) {
	src := &errReader{rs: rs}
	var pdf *model.Context
	err := guard(func() (err error) {
		if pdf, err = api.ReadAndValidate(c, src, newConfig()); err != nil {
			return err
		}
		if pdf.Encrypt != nil || pdf.E != nil {
			return ErrEncrypted
		}
		// pdfcpu carries named destinations into a part by rewriting this tree in place, so each
		// cut would inherit the previous cuts' edits. Azure analyses a part's pages, not its
		// links, and without the tree an internal link dangles the same way in every part, so the
		// parts go without it. Nothing on a page renders differently.
		delete(pdf.Names, "Dests")
		return nil
	})
	if src.err != nil {
		// Never accept a parse that lost a read, even one pdfcpu calls a success: in relaxed
		// validation it logs and skips an object it fails to load, so a page can lose its content
		// and the part reach Azure blank.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr // cancellation is classified first, as everywhere
		}
		return nil, fmt.Errorf("pdfsplit: read: %w", src.err)
	}
	if err != nil {
		return nil, classify(ctx, c, err)
	}
	return pdf, nil
}

// WritePart writes the given original pages (1-based) as a standalone PDF to w, within the
// document's remaining budget.
//
// pages must be non-empty and strictly ascending within 1..PageCount, as a Part's are; anything
// else is the caller's mistake and is rejected before pdfcpu sees it. Errors are classified as for
// Open, except that a failure of w itself is returned as such: a full disk is not the document's
// fault.
//
// On any error w may already hold part of the output, so the caller must discard it. Once a cut
// has panicked, every later WritePart on the Document returns that same ErrPanic.
func (d *Document) WritePart(ctx context.Context, pages []int, w io.Writer) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pdf == nil {
		return errors.New("pdfsplit: document is closed")
	}
	if d.failed != nil {
		return d.failed
	}
	if err := checkPages(pages, d.PageCount); err != nil {
		return err
	}

	c, cancel := context.WithDeadline(ctx, d.deadline)
	defer cancel()
	dst := &errWriter{w: w}
	err := guard(func() error { return cut(c, d.pdf, pages, dst) })
	if errors.Is(err, ErrPanic) {
		d.failed = err
	}
	if err != nil && dst.err != nil && ctx.Err() == nil {
		return fmt.Errorf("pdfsplit: write part: %w", dst.err)
	}
	return classify(ctx, c, err)
}

// cut extracts pages from pdf into a new PDF written to w. It is a variable so tests can make it
// panic, which pdfcpu cannot be made to do on demand.
var cut = func(c context.Context, pdf *model.Context, pages []int, w io.Writer) error {
	part, err := pdfcpu.ExtractPages(c, pdf, pages, true)
	if err != nil {
		return err
	}
	return api.WriteContext(c, part, w)
}

// checkPages validates a cut's page list against a document of count pages.
func checkPages(pages []int, count int) error {
	if len(pages) == 0 {
		return errors.New("pdfsplit: no pages to write")
	}
	for i, p := range pages {
		if p < 1 || p > count {
			return fmt.Errorf("pdfsplit: page %d is outside 1..%d", p, count)
		}
		if i > 0 && p <= pages[i-1] {
			return fmt.Errorf("pdfsplit: page %d follows page %d; pages must ascend", p, pages[i-1])
		}
	}
	return nil
}

// Close releases the parsed model and the slot. Safe to call twice. It waits for a WritePart in
// progress to finish.
func (d *Document) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pdf == nil {
		return
	}
	d.pdf = nil
	_ = d.file.Close()
	d.owner.release()
}

// Recut writes pages (1-based within src) of an existing part file to w; used for halving.
//
// Halving happens after the original's model has been released, and a part file is a few pages,
// so parsing it again is cheap. Recut takes a slot and a fresh budget of its own: it is Open
// followed by one WritePart.
//
// It must not be called while holding a Document from the same Splitter. It waits for a slot of
// its own, so once every slot is held by callers doing that, none of them can proceed. Close the
// original's Document first, as soon as its parts are written.
func (s *Splitter) Recut(ctx context.Context, srcPath string, local []int, w io.Writer) error {
	doc, err := s.Open(ctx, srcPath)
	if err != nil {
		return err
	}
	defer doc.Close()
	return doc.WritePart(ctx, local, w)
}

// guard runs fn, turning a panic into an ErrPanic error.
//
// pdfcpu turns its own fault panics into errors at its API boundary but re-panics anything else,
// such as an index out of range on a malformed file, and pdfcpu.ExtractPages and api.WriteContext
// have no such boundary at all. A bad upload must cost a whole-document fallback, not the process.
func guard(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()
	return fn()
}

// classify maps the outcome of pdfcpu work done under c, which is ctx bounded by the document's
// deadline, onto this package's errors.
func classify(ctx, c context.Context, err error) error {
	if err == nil {
		return nil
	}
	// Read c before ctx. c ends whenever ctx does, so c having ended while ctx is still live at the
	// later read means the document's deadline ended it, not the caller.
	budgetSpent := c.Err() != nil
	switch {
	case ctx.Err() != nil:
		// The caller's own cancellation or deadline, unchanged. Callers classify cancellation on
		// their context first, and a shutdown says nothing about the document.
		return ctx.Err()
	case budgetSpent:
		return ErrBudget
	case errors.Is(err, ErrPanic):
		return err
	case isEncryption(err):
		return ErrEncrypted
	default:
		return fmt.Errorf("%w: %v", ErrUnparseable, err)
	}
}

func isEncryption(err error) bool {
	if errors.Is(err, ErrEncrypted) {
		return true
	}
	for _, target := range encryptionErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// errReader remembers the source's first read error, so that an I/O failure during a parse is
// reported as itself rather than classified as a fault in the document. io.EOF is how pdfcpu
// finds the end of the file, so it is not an error here. Seek errors are not recorded either:
// seeking a file does no I/O, and only fails on an offset taken from a corrupt document.
type errReader struct {
	rs  io.ReadSeeker
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.rs.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

func (e *errReader) Seek(offset int64, whence int) (int64, error) { return e.rs.Seek(offset, whence) }

// errWriter remembers the destination's first error, so that a failed write is reported as itself
// rather than classified as a fault in the document.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}
