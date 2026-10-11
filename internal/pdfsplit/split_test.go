package pdfsplit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// writeFile stores data in a fresh temporary directory and returns the file's path.
func writeFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// originalPages parses a PDF and maps each page's MediaBox width back to the page number pdftest
// gave it in the original.
func originalPages(t *testing.T, data []byte) []int {
	t.Helper()
	pdf, err := api.ReadAndValidate(context.Background(), bytes.NewReader(data), newConfig())
	if err != nil {
		t.Fatalf("written PDF does not parse: %v", err)
	}
	dims, err := pdf.PageDims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pages := make([]int, len(dims))
	for i, d := range dims {
		pages[i] = pdftest.PageOf(d.Width)
	}
	return pages
}

// encrypt returns data encrypted with the given passwords. An empty userPW gives owner-only
// ("permissions") encryption, which opens without a password.
func encrypt(t *testing.T, data []byte, userPW, ownerPW string) []byte {
	t.Helper()
	conf := newConfig()
	conf.UserPW, conf.OwnerPW = userPW, ownerPW
	var out bytes.Buffer
	if err := api.Encrypt(context.Background(), bytes.NewReader(data), &out, conf); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mustOpen(t *testing.T, s *Splitter, path string) *Document {
	t.Helper()
	doc, err := s.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return doc
}

// slotsInUse reports how many of the splitter's slots are held.
func slotsInUse(s *Splitter) int { return len(s.sem) }

func TestSplitAndRecut(t *testing.T) {
	ctx := context.Background()
	s := NewSplitter(2, time.Minute)
	doc := mustOpen(t, s, writeFile(t, pdftest.Make(12)))
	defer doc.Close()
	if doc.PageCount != 12 {
		t.Fatalf("PageCount = %d, want 12", doc.PageCount)
	}

	partPath := filepath.Join(t.TempDir(), "part-4-6.pdf")
	f, err := os.Create(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.WritePart(ctx, []int{4, 5, 6}, f); err != nil {
		t.Fatalf("WritePart: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := originalPages(t, data); !slices.Equal(got, []int{4, 5, 6}) {
		t.Fatalf("part holds original pages %v, want [4 5 6]", got)
	}

	var half bytes.Buffer
	if err := s.Recut(ctx, partPath, []int{2}, &half); err != nil {
		t.Fatalf("Recut: %v", err)
	}
	if got := originalPages(t, half.Bytes()); !slices.Equal(got, []int{5}) {
		t.Fatalf("recut part holds original pages %v, want [5]", got)
	}
	if n := slotsInUse(s); n != 1 {
		t.Fatalf("%d slots in use after Recut, want 1 (the open document)", n)
	}
}

// TestEveryPartComesFromOneParse cuts all of a plan's parts from one parsed model, as the engine
// does. pdfcpu updates the model as it reads objects for a cut, so each part must still hold
// exactly its own pages however many cuts came before it.
func TestEveryPartComesFromOneParse(t *testing.T) {
	s := NewSplitter(1, time.Minute)
	doc := mustOpen(t, s, writeFile(t, pdftest.Make(23)))
	defer doc.Close()

	for _, part := range Plan(seq(1, 23), 5) {
		var buf bytes.Buffer
		if err := doc.WritePart(context.Background(), part.Pages, &buf); err != nil {
			t.Fatalf("WritePart(%s): %v", part.Label(), err)
		}
		if got := originalPages(t, buf.Bytes()); !slices.Equal(got, part.Pages) {
			t.Fatalf("part %s holds original pages %v", part.Label(), got)
		}
	}
}

func TestOpenFailuresReleaseTheSlot(t *testing.T) {
	plain := pdftest.Make(4)
	cases := []struct {
		name string
		data []byte // nil: no file at all
		want error
	}{
		{"garbage", []byte("this is not a PDF, only some bytes pretending to be one"), ErrUnparseable},
		{"header then garbage", append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("garbage "), 64)...), ErrUnparseable},
		{"empty file", []byte{}, ErrUnparseable},
		{"user password", encrypt(t, plain, "user", "owner"), ErrEncrypted},
		{"owner password only", encrypt(t, plain, "", "owner"), ErrEncrypted},
		{"missing file", nil, fs.ErrNotExist},
	}

	// One slot, so a failure that kept it would also block the final Open.
	s := NewSplitter(1, time.Minute)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing.pdf")
			if tc.data != nil {
				path = writeFile(t, tc.data)
			}
			doc, err := s.Open(context.Background(), path)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
			if doc != nil {
				t.Fatal("a failed Open returned a document")
			}
			if n := slotsInUse(s); n != 0 {
				t.Fatalf("%d slots still held after a failed Open", n)
			}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	doc, err := s.Open(ctx, writeFile(t, plain))
	if err != nil {
		t.Fatalf("Open after the failures: %v", err)
	}
	doc.Close()
}

func TestRecutFailuresReleaseTheSlot(t *testing.T) {
	s := NewSplitter(1, time.Minute)
	err := s.Recut(context.Background(), writeFile(t, []byte("not a PDF")), []int{1}, io.Discard)
	if !errors.Is(err, ErrUnparseable) {
		t.Fatalf("Recut of garbage = %v, want ErrUnparseable", err)
	}
	if err := s.Recut(context.Background(), writeFile(t, pdftest.Make(3)), []int{4}, io.Discard); err == nil {
		t.Fatal("Recut of page 4 of a 3-page part succeeded")
	}
	if n := slotsInUse(s); n != 0 {
		t.Fatalf("%d slots held after failed Recuts", n)
	}
}

func TestOpenOverBudget(t *testing.T) {
	s := NewSplitter(1, time.Nanosecond)
	_, err := s.Open(context.Background(), writeFile(t, pdftest.Make(200)))
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("Open = %v, want ErrBudget", err)
	}
	if n := slotsInUse(s); n != 0 {
		t.Fatalf("%d slots held after running out of budget", n)
	}
}

// TestBudgetCoversEveryCut checks there is one deadline per document, set as Open starts its
// work: a cut after it has passed fails even though the cut itself would be quick.
func TestBudgetCoversEveryCut(t *testing.T) {
	const budget = time.Minute
	s := NewSplitter(1, budget)
	before := time.Now()
	doc := mustOpen(t, s, writeFile(t, pdftest.Make(6)))
	after := time.Now()
	defer doc.Close()

	if doc.deadline.Before(before.Add(budget)) || doc.deadline.After(after.Add(budget)) {
		t.Fatalf("deadline %v is not one budget after Open (%v..%v)",
			doc.deadline, before.Add(budget), after.Add(budget))
	}
	if err := doc.WritePart(context.Background(), []int{1, 2, 3}, io.Discard); err != nil {
		t.Fatalf("WritePart within the budget: %v", err)
	}

	doc.deadline = time.Now().Add(-time.Millisecond) // the budget runs out between two cuts
	err := doc.WritePart(context.Background(), []int{4, 5, 6}, io.Discard)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("WritePart after the deadline = %v, want ErrBudget", err)
	}
}

func TestCallerContextIsReturnedUnchanged(t *testing.T) {
	s := NewSplitter(1, time.Minute)
	path := writeFile(t, pdftest.Make(3))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	// A dead context must neither parse nor keep the slot, even when one is free.
	if _, err := s.Open(cancelled, path); err != context.Canceled {
		t.Fatalf("Open with a cancelled context = %v, want context.Canceled", err)
	}
	if n := slotsInUse(s); n != 0 {
		t.Fatalf("%d slots held after a cancelled Open", n)
	}

	doc := mustOpen(t, s, path)
	defer doc.Close()
	if err := doc.WritePart(cancelled, []int{1}, io.Discard); err != context.Canceled {
		t.Errorf("WritePart with a cancelled context = %v, want context.Canceled", err)
	}
	// The caller's own deadline belongs to the caller; it is not the split budget.
	if err := doc.WritePart(expired, []int{1}, io.Discard); err != context.DeadlineExceeded {
		t.Errorf("WritePart past the caller's deadline = %v, want context.DeadlineExceeded", err)
	}
	if err := doc.WritePart(context.Background(), []int{1}, io.Discard); err != nil {
		t.Errorf("WritePart after the caller's failures: %v", err)
	}
}

func TestWritePartRejectsBadPageLists(t *testing.T) {
	s := NewSplitter(1, time.Minute)
	doc := mustOpen(t, s, writeFile(t, pdftest.Make(12)))
	defer doc.Close()

	for _, pages := range [][]int{nil, {0}, {-1}, {13}, {1, 13}, {3, 2}, {2, 2}} {
		err := doc.WritePart(context.Background(), pages, io.Discard)
		if err == nil {
			t.Errorf("WritePart(%v) succeeded", pages)
			continue
		}
		if errors.Is(err, ErrUnparseable) || errors.Is(err, ErrBudget) || errors.Is(err, ErrEncrypted) {
			t.Errorf("WritePart(%v) = %v; a bad request must not be blamed on the document", pages, err)
		}
	}
	if err := doc.WritePart(context.Background(), []int{12}, io.Discard); err != nil {
		t.Errorf("WritePart after the rejected requests: %v", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWritePartReportsTheWritersOwnError(t *testing.T) {
	s := NewSplitter(1, time.Minute)
	doc := mustOpen(t, s, writeFile(t, pdftest.Make(3)))
	defer doc.Close()

	diskFull := errors.New("no space left on device")
	err := doc.WritePart(context.Background(), []int{1, 2}, failingWriter{diskFull})
	if !errors.Is(err, diskFull) {
		t.Fatalf("WritePart = %v, want the writer's error", err)
	}
	if errors.Is(err, ErrUnparseable) {
		t.Fatalf("a full disk was blamed on the document: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := NewSplitter(2, time.Minute)
	path := writeFile(t, pdftest.Make(3))
	a := mustOpen(t, s, path)
	b := mustOpen(t, s, path)

	a.Close()
	a.Close() // must not give back a second slot: that one is b's
	if n := slotsInUse(s); n != 1 {
		t.Fatalf("%d slots in use after closing one of two documents twice, want 1", n)
	}
	if err := a.WritePart(context.Background(), []int{1}, io.Discard); err == nil {
		t.Fatal("WritePart on a closed document succeeded")
	}
	b.Close()
	if n := slotsInUse(s); n != 0 {
		t.Fatalf("%d slots in use after closing everything", n)
	}
}

func TestOpenWaitsForAFreeSlot(t *testing.T) {
	const budget = time.Minute
	s := NewSplitter(2, budget)
	path := writeFile(t, pdftest.Make(3))
	a := mustOpen(t, s, path)
	b := mustOpen(t, s, path)
	defer b.Close()

	type result struct {
		doc *Document
		err error
	}
	third := make(chan result, 1)
	go func() {
		doc, err := s.Open(context.Background(), path)
		third <- result{doc, err}
	}()

	select {
	case r := <-third:
		t.Fatalf("a third Open returned while both slots were held (err %v)", r.err)
	case <-time.After(50 * time.Millisecond):
	}

	freed := time.Now()
	a.Close()
	select {
	case r := <-third:
		if r.err != nil {
			t.Fatalf("third Open: %v", r.err)
		}
		defer r.doc.Close()
		// The budget limits the work on a document, not the wait for a slot.
		if r.doc.deadline.Before(freed.Add(budget)) {
			t.Errorf("the wait for a slot counted against the budget: deadline %v, slot freed at %v",
				r.doc.deadline, freed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the third Open is still blocked after a slot was freed")
	}
}

func TestWaitingOpenHonoursCancellation(t *testing.T) {
	s := NewSplitter(1, time.Minute)
	path := writeFile(t, pdftest.Make(3))
	held := mustOpen(t, s, path)
	defer held.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		doc, err := s.Open(ctx, path)
		if doc != nil {
			doc.Close()
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		t.Fatalf("Open returned while the only slot was held (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-errc:
		if err != context.Canceled {
			t.Fatalf("cancelled Open = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled Open is still waiting")
	}
	if n := slotsInUse(s); n != 1 {
		t.Fatalf("%d slots in use, want 1 (the held document)", n)
	}
}

// TestGuardTurnsPanicsIntoErrors exercises the recover wrapper directly: pdfcpu cannot be made to
// panic on demand.
func TestGuardTurnsPanicsIntoErrors(t *testing.T) {
	cases := []struct {
		name string
		fn   func() error
	}{
		{"panic with a value", func() error { panic("boom") }},
		{"panic with an error", func() error { panic(errors.New("fault")) }},
		{"runtime error", func() error {
			pages := []int{}
			return fmt.Errorf("page %d", pages[len(pages)]) // index out of range
		}},
	}
	for _, tc := range cases {
		err := guard(tc.fn)
		if !errors.Is(err, ErrPanic) || !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s: guard = %v, want an ErrPanic, which is also an ErrUnparseable", tc.name, err)
		}
	}

	plain := errors.New("plain")
	if err := guard(func() error { return plain }); err != plain {
		t.Errorf("guard changed a returned error into %v", err)
	}
	if err := guard(func() error { return nil }); err != nil {
		t.Errorf("guard of a clean call = %v", err)
	}
}

func TestClassify(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(live)
	cancel()
	expired, cancelExpired := context.WithDeadline(live, time.Now().Add(-time.Second))
	defer cancelExpired()
	parseErr := errors.New("xref table: corrupt")

	cases := []struct {
		name      string
		caller, c context.Context
		err       error
		want      error
	}{
		{"success", live, live, nil, nil},
		{"the caller's cancellation beats everything", cancelled, cancelled, parseErr, context.Canceled},
		{"the caller's deadline stays the caller's", expired, expired, context.DeadlineExceeded, context.DeadlineExceeded},
		{"the document's deadline is the budget", live, expired, context.DeadlineExceeded, ErrBudget},
		{"the budget beats a parse error", live, expired, parseErr, ErrBudget},
		{"user password", live, live, fmt.Errorf("read context: %w", pdfcpu.ErrWrongPassword), ErrEncrypted},
		{"unsupported encryption", live, live, fmt.Errorf("x: %w", pdfcpu.ErrUnsupportedEncryptionFeature), ErrEncrypted},
		{"malformed encryption", live, live, errors.Join(pdfcpu.ErrMalformedEncryption, parseErr), ErrEncrypted},
		{"panic", live, live, fmt.Errorf("%w: boom", ErrPanic), ErrPanic},
		{"anything else", live, live, parseErr, ErrUnparseable},
	}
	for _, tc := range cases {
		got := classify(tc.caller, tc.c, tc.err)
		if tc.want == nil {
			if got != nil {
				t.Errorf("%s: classify = %v, want nil", tc.name, got)
			}
			continue
		}
		if !errors.Is(got, tc.want) {
			t.Errorf("%s: classify = %v, want %v", tc.name, got, tc.want)
		}
	}
	if errors.Is(ErrBudget, context.DeadlineExceeded) {
		t.Error("ErrBudget must not match the caller's context.DeadlineExceeded")
	}
}

// TestOpenNeedsNoWritableHome guards the production environment. The image runs as nonroot on a
// read-only root filesystem, where pdfcpu's default configuration panics because it cannot create
// its config.yml. Pointing HOME at a regular file makes that directory uncreatable for any user,
// root included. pdfcpu caches its default configuration per process, so the check runs in a
// fresh copy of this test binary.
func TestOpenNeedsNoWritableHome(t *testing.T) {
	if os.Getenv("PDFSPLIT_HOME_IS_A_FILE") == "1" {
		s := NewSplitter(1, time.Minute)
		doc := mustOpen(t, s, writeFile(t, pdftest.Make(6)))
		defer doc.Close()
		if err := doc.WritePart(context.Background(), []int{2, 3}, io.Discard); err != nil {
			t.Fatalf("WritePart: %v", err)
		}
		return
	}

	home := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(home, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenNeedsNoWritableHome$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "PDFSPLIT_HOME_IS_A_FILE=1", "HOME="+home, "XDG_CONFIG_HOME=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("splitting with an unwritable HOME failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("--- PASS: TestOpenNeedsNoWritableHome")) {
		t.Fatalf("the child process did not run the check:\n%s", out)
	}
}

// makeWithDests is pdftest.Make with one named destination per page: the catalog's /Names /Dests
// tree maps "dNNN" to [page NNN /Fit], as in a document with an outline or internal links.
func makeWithDests(n int) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	page := func(i int) int { return 5 + 2*(i-1) } // object number of page i; its content follows

	var kids, names bytes.Buffer
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&kids, "%d 0 R ", page(i))
		fmt.Fprintf(&names, "(d%03d) [%d 0 R /Fit] ", i, page(i))
	}
	b.WriteString("%PDF-1.7\n")
	obj("<< /Type /Catalog /Pages 2 0 R /Names << /Dests 4 0 R >> >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	obj(fmt.Sprintf("<< /Names [%s] >>", names.String()))
	for i := 1; i <= n; i++ {
		stream := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (PAGE %d) Tj ET", i)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d 800] "+
			"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", pdftest.BaseWidth+i, page(i)+1))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

// namedDests lists a PDF's named destinations, each with its target.
func namedDests(t *testing.T, data []byte) []string {
	t.Helper()
	ctx := context.Background()
	pdf, err := api.ReadAndValidate(ctx, bytes.NewReader(data), newConfig())
	if err != nil {
		t.Fatalf("PDF does not parse: %v", err)
	}
	tree := pdf.Names["Dests"]
	if tree == nil {
		return nil
	}
	entries, err := tree.KeyList(ctx) // "name target" pairs
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// TestCutsDoNotLeakNamedDestinations is a regression test. pdfcpu carries named destinations into
// a part by patching and pruning the source model's /Dests tree in place, so each cut saw what the
// previous one left: after cutting 1-4, the part 5-8 held only d001, pointing at the font object,
// and 9-12 held none. Parts are analysed for their pages alone, so they carry no named
// destinations at all.
func TestCutsDoNotLeakNamedDestinations(t *testing.T) {
	data := makeWithDests(12)
	if got := namedDests(t, data); len(got) != 12 {
		t.Fatalf("fixture has named destinations %v, want 12", got)
	}

	s := NewSplitter(1, time.Minute)
	doc := mustOpen(t, s, writeFile(t, data))
	defer doc.Close()

	for _, pages := range [][]int{{1, 2, 3, 4}, {5, 6, 7, 8}, {9, 10, 11, 12}} {
		var buf bytes.Buffer
		if err := doc.WritePart(context.Background(), pages, &buf); err != nil {
			t.Fatalf("WritePart(%v): %v", pages, err)
		}
		if got := originalPages(t, buf.Bytes()); !slices.Equal(got, pages) {
			t.Errorf("part %v holds original pages %v", pages, got)
		}
		if dests := namedDests(t, buf.Bytes()); len(dests) > 0 {
			t.Errorf("part %v carries named destinations %v", pages, dests)
		}
	}
}

// failingReader serves its data for a few reads and then fails every read, as a disk or network
// filesystem can part-way through a parse.
type failingReader struct {
	*bytes.Reader
	reads, failAfter int
	err              error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.reads++; r.reads > r.failAfter {
		return 0, r.err
	}
	return r.Reader.Read(p)
}

func TestReadErrorsAreNotBlamedOnTheDocument(t *testing.T) {
	ioErr := errors.New("input/output error")
	data := pdftest.Make(200) // large enough that every case below fails before the parse is done
	for _, failAfter := range []int{0, 2, 5} {
		src := &failingReader{Reader: bytes.NewReader(data), failAfter: failAfter, err: ioErr}
		ctx := context.Background()
		_, err := readPDF(ctx, ctx, src)
		if !errors.Is(err, ioErr) {
			t.Errorf("failing after %d reads: readPDF = %v, want the reader's error", failAfter, err)
		}
		if errors.Is(err, ErrUnparseable) {
			t.Errorf("failing after %d reads: a read error was blamed on the document: %v", failAfter, err)
		}
	}
}

// TestWritePartStaysFailedAfterAPanic checks that a document whose cut panicked is not cut again:
// pdfcpu may have stopped half-way through updating the model.
func TestWritePartStaysFailedAfterAPanic(t *testing.T) {
	s := NewSplitter(1, time.Minute)
	doc := mustOpen(t, s, writeFile(t, pdftest.Make(6)))
	defer doc.Close()

	real := cut
	t.Cleanup(func() { cut = real })
	cut = func(context.Context, *model.Context, []int, io.Writer) error { panic("half-way through") }
	if err := doc.WritePart(context.Background(), []int{1}, io.Discard); !errors.Is(err, ErrPanic) {
		t.Fatalf("WritePart with a panicking cut = %v, want ErrPanic", err)
	}

	cut = real
	if err := doc.WritePart(context.Background(), []int{2}, io.Discard); !errors.Is(err, ErrPanic) {
		t.Fatalf("WritePart after a panic = %v, want the panic's ErrPanic again", err)
	}
	doc.Close()
	if n := slotsInUse(s); n != 0 {
		t.Fatalf("%d slots held after closing the failed document", n)
	}
}

// objectFailingReader fails every read that starts at one offset, where pdfcpu seeks to load an
// object, and serves every other read normally. onFail, if set, runs as the read fails.
type objectFailingReader struct {
	*bytes.Reader
	at     int64
	err    error
	onFail func()
}

func (r *objectFailingReader) Read(p []byte) (int, error) {
	if pos, _ := r.Reader.Seek(0, io.SeekCurrent); pos == r.at {
		if r.onFail != nil {
			r.onFail()
		}
		return 0, r.err
	}
	return r.Reader.Read(p)
}

// TestReadPDFNeverAcceptsAParseThatLostARead is a regression test. In relaxed validation pdfcpu
// logs and skips an object it fails to load, so an I/O error inside one object left it nil and
// the parse succeeded: losing page 10's content stream gave a part with a blank page 10, which
// Azure would have analysed as empty.
func TestReadPDFNeverAcceptsAParseThatLostARead(t *testing.T) {
	data := pdftest.Make(12)
	ioErr := errors.New("input/output error")
	for _, object := range []string{
		"23 0 obj", // page 10's content stream
		"6 0 obj",  // page 2's page object
	} {
		at := bytes.Index(data, []byte("\n"+object+"\n"))
		if at < 0 {
			t.Fatalf("fixture has no %q", object)
		}
		src := &objectFailingReader{Reader: bytes.NewReader(data), at: int64(at + 1), err: ioErr}
		ctx := context.Background()
		pdf, err := readPDF(ctx, ctx, src)
		if !errors.Is(err, ioErr) {
			pages := -1
			if pdf != nil {
				pages = pdf.PageCount
			}
			t.Errorf("a read failing at %q: readPDF = %v (pages %d), want the reader's error", object, err, pages)
		}
		if errors.Is(err, ErrUnparseable) {
			t.Errorf("a read failing at %q was blamed on the document: %v", object, err)
		}
	}

	// A caller that gives up as the read fails still hears about its own cancellation first.
	at := bytes.Index(data, []byte("\n23 0 obj\n")) + 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &objectFailingReader{Reader: bytes.NewReader(data), at: int64(at), err: ioErr, onFail: cancel}
	if _, err := readPDF(ctx, ctx, src); err != context.Canceled {
		t.Errorf("a read failing as the caller cancels: readPDF = %v, want context.Canceled", err)
	}
}
