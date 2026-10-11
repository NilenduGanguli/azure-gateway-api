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
