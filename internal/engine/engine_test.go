package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
	"github.com/NilenduGanguli/azure-gateway-api/internal/sched"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

const apiVersion = "2024-11-30"

// newFake starts a paged fake and closes it when the test ends.
func newFake(t *testing.T, o mockazure.PagedOptions) *mockazure.Container {
	t.Helper()
	c := mockazure.NewPaged(o)
	t.Cleanup(c.Close)
	return c
}

// newEngine builds an engine against baseURL the way the plan's tests describe: a real scheduler
// and splitter, SPLIT_PAGES 5, a temporary work root and jitter fixed at 0. Backoff is shortened
// so retries cost milliseconds, not the seconds production waits.
func newEngine(t *testing.T, surface azerr.Surface, baseURL string, limit int) *Engine {
	t.Helper()
	client := upstream.NewClient(surface, config.Upstream{
		BaseURL: baseURL, MaxInflight: limit, Timeout: time.Minute,
	}, t.TempDir(), 64<<20)
	e := New(Options{
		Client:       client,
		Sched:        sched.New(limit),
		Splitter:     pdfsplit.NewSplitter(2, time.Minute),
		SplitPages:   5,
		WorkRoot:     filepath.Join(t.TempDir(), "work"),
		WholeTimeout: 30 * time.Second,
		Jitter:       func() float64 { return 0 },
	})
	e.backoff = func(int, time.Duration) time.Duration { return time.Millisecond }
	return e
}

// writeInput stores body as a job's input on the "PVC".
func writeInput(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// input is a DI job for body.
func input(t *testing.T, job, contentType string, body []byte, query url.Values) Input {
	t.Helper()
	q := url.Values{"api-version": {apiVersion}}
	for k, v := range query {
		q[k] = v
	}
	return Input{
		JobID: job, ContentType: contentType, Path: writeInput(t, body), Size: int64(len(body)),
		ModelID: "prebuilt-layout", Query: q,
	}
}

// pdfInput is a DI job for an n-page test PDF.
func pdfInput(t *testing.T, job string, n int, query url.Values) Input {
	t.Helper()
	return input(t, job, "application/pdf", pdftest.Make(n), query)
}

// seq is first..last.
func seq(first, last int) []int {
	var out []int
	for p := first; p <= last; p++ {
		out = append(out, p)
	}
	return out
}

// result reads the bare AnalyzeResult an output holds.
func result(t *testing.T, out *Output) []byte {
	t.Helper()
	f, err := os.Open(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.NewSectionReader(f, out.Start, out.End-out.Start))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// run runs the engine and fails the test on an error.
func run(t *testing.T, e *Engine, in Input) *Output {
	t.Helper()
	out, err := e.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(out.Cleanup)
	return out
}

// wantDI checks that out is docgen's whole-document result for pages.
func wantDI(t *testing.T, out *Output, pages []int) {
	t.Helper()
	got, want := result(t, out), docgen.DI(pages, docgen.Original, docgen.Options{})
	if !bytes.Equal(got, want) {
		t.Fatalf("result differs from the whole document's (%d bytes, want %d):\n got %.300s\nwant %.300s",
			len(got), len(want), got, want)
	}
}

// sizes lists the page counts of calls, in order.
func sizes(calls [][]int) []int {
	out := make([]int, len(calls))
	for i, c := range calls {
		out[i] = len(c)
	}
	return out
}

// proxy fronts target with a hook that sees, and may change, every request first, and records
// every request body.
type proxy struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newProxy(t *testing.T, target string, hook func(r *http.Request, n int)) *proxy {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{}
	rp := httputil.NewSingleHostReverseProxy(u)
	var n atomic.Int64
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies = append(p.bodies, body)
		p.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if hook != nil {
			hook(r, int(n.Add(1)))
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *proxy) Bodies() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.bodies)
}

func TestWhole(t *testing.T) {
	t.Run("small PDF", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		out := run(t, e, pdfInput(t, "job-small", 3, nil))
		wantDI(t, out, seq(1, 3))
		if got := fake.Calls(); len(got) != 1 {
			t.Errorf("calls %v, want one", got)
		}
		if out.Parts != 1 || out.Attempts != 1 || out.Fallback != "" {
			t.Errorf("output parts=%d attempts=%d fallback=%q, want 1, 1, none",
				out.Parts, out.Attempts, out.Fallback)
		}
	})

	t.Run("JPEG", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		out := run(t, e, input(t, "job-jpeg", "image/jpeg", []byte("\xff\xd8\xff\xe0 not really a JPEG"), nil))
		wantDI(t, out, []int{1})
		if got := fake.Calls(); len(got) != 1 {
			t.Errorf("calls %v, want one", got)
		}
	})

	t.Run("urlSource", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		p := newProxy(t, fake.URL(), nil)
		e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
		body := []byte(`{"urlSource": "https://example.com/big.pdf"}`)
		out := run(t, e, input(t, "job-url", "application/json", body, nil))
		wantDI(t, out, []int{1})
		bodies := p.Bodies()
		if len(bodies) != 1 || !bytes.Equal(bodies[0], body) {
			t.Errorf("forwarded %q, want the client's body once, byte for byte", bodies)
		}
	})

	t.Run("splitting off", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		e.o.SplitPages = 0
		out := run(t, e, pdfInput(t, "job-off", 23, nil))
		wantDI(t, out, seq(1, 23))
		if got := sizes(fake.Calls()); !slices.Equal(got, []int{23}) {
			t.Errorf("call sizes %v, want the whole document once", got)
		}
	})

	t.Run("unparseable PDF", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		out := run(t, e, input(t, "job-bad", "application/pdf", []byte("%PDF-1.7\ngarbage garbage"), nil))
		if out.Fallback != "unparseable" {
			t.Errorf("fallback %q, want unparseable", out.Fallback)
		}
		wantDI(t, out, []int{1})
		if got := e.Metrics().Snapshot().SplitFallbacks["unparseable"]; got != 1 {
			t.Errorf("unparseable fallbacks %d, want 1", got)
		}
	})
}

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        func(t *testing.T) Input
		want      []int
		wantSizes []int
	}{
		{"23 pages", func(t *testing.T) Input { return pdfInput(t, "job-23", 23, nil) },
			seq(1, 23), []int{5, 5, 5, 4, 4}},
		{"pages=3-14", func(t *testing.T) Input {
			return pdfInput(t, "job-sel", 23, url.Values{"pages": {"3-14"}})
		}, seq(3, 14), []int{4, 4, 4}},
		{"base64Source", func(t *testing.T) Input {
			body := `{"base64Source":"` + base64.StdEncoding.EncodeToString(pdftest.Make(12)) + `"}`
			return input(t, "job-b64", "application/json", []byte(body), nil)
		}, seq(1, 12), []int{4, 4, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(t, mockazure.PagedOptions{})
			e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
			in := tc.in(t)
			out := run(t, e, in)
			wantDI(t, out, tc.want)
			got := sizes(fake.Calls())
			slices.Sort(got)
			want := slices.Clone(tc.wantSizes)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("call sizes %v, want %v", got, want)
			}
			if out.Parts != len(tc.wantSizes) || out.Attempts != len(tc.wantSizes) {
				t.Errorf("parts=%d attempts=%d, want %d each", out.Parts, out.Attempts, len(tc.wantSizes))
			}
			snap := e.Metrics().Snapshot()
			if snap.Parts["succeeded"] != int64(len(tc.wantSizes)) || snap.JobParts.Count != 1 ||
				snap.MergeSeconds.Count != 1 || snap.PartSeconds.Count != int64(len(tc.wantSizes)) {
				t.Errorf("metrics %+v", snap)
			}
			// The merged result lives in the job's work folder until Cleanup.
			dir := filepath.Join(e.o.WorkRoot, in.JobID)
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("work folder: %v", err)
			}
			out.Cleanup()
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("work folder after Cleanup: %v", err)
			}
		})
	}
}

func TestRead(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{Surface: mockazure.Read})
	e := newEngine(t, azerr.SurfaceRead, fake.URL(), 4)
	in := pdfInput(t, "job-read", 12, nil)
	in.ModelID, in.Query = "", nil
	out := run(t, e, in)
	if got, want := result(t, out), docgen.Read(seq(1, 12), docgen.Original); !bytes.Equal(got, want) {
		t.Fatalf("result differs from the whole document's:\n got %.300s\nwant %.300s", got, want)
	}
	if out.Parts != 3 {
		t.Errorf("parts %d, want 3", out.Parts)
	}
}

// A page that never answers costs the job PartAttempts at every size: 4 pages, then 2, then 1,
// and then the job fails with the timeout.
func TestHangHalvesDownToOnePage(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{Hang: map[int]bool{9: true}})
	e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
	e.partTimeout = 200 * time.Millisecond
	_, err := e.Run(context.Background(), pdfInput(t, "job-hang", 12, nil))
	var apiErr *azerr.APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "timeout") {
		t.Fatalf("Run error %v, want the attempt's timeout error", err)
	}
	var covering [][]int
	for _, c := range fake.Calls() {
		if slices.Contains(c, 9) {
			covering = append(covering, c)
		}
	}
	want := [][]int{
		{9, 10, 11, 12}, {9, 10, 11, 12}, {9, 10, 11, 12},
		{9, 10}, {9, 10}, {9, 10},
		{9}, {9}, {9},
	}
	if !slices.EqualFunc(covering, want, slices.Equal) {
		t.Errorf("calls covering page 9: %v, want %v", covering, want)
	}
	snap := e.Metrics().Snapshot()
	if snap.Parts["halved"] != 2 || snap.Exhausted != 1 || snap.Retries["timeout"] != 6 {
		t.Errorf("metrics parts=%v exhausted=%d retries=%v", snap.Parts, snap.Exhausted, snap.Retries)
	}
	if _, err := os.Stat(filepath.Join(e.o.WorkRoot, "job-hang")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("work folder after a failed job: %v", err)
	}
}

func TestHedgeWins(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{HangFirst: map[int]bool{5: true}})
	e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
	// Only the hung part is hedged early, so a slow machine cannot hedge the others too.
	e.hedgeDelay = func(pages []int) time.Duration {
		if slices.Contains(pages, 5) {
			return 50 * time.Millisecond
		}
		return time.Hour
	}
	out := run(t, e, pdfInput(t, "job-hedge", 12, nil))
	wantDI(t, out, seq(1, 12))
	if out.Hedges != 1 || out.Attempts != 3 {
		t.Errorf("hedges=%d attempts=%d, want 1 and 3", out.Hedges, out.Attempts)
	}
	snap := e.Metrics().Snapshot()
	if snap.Parts["hedged"] != 1 || snap.Parts["hedge_won"] != 1 {
		t.Errorf("parts metrics %v", snap.Parts)
	}
	if e.Sched().Stats().InUse != 0 {
		t.Errorf("slots still held: %+v", e.Sched().Stats())
	}
}

func TestRetries(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{FailFirst: map[int]int{4: 2}})
	e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
	out := run(t, e, pdfInput(t, "job-retry", 12, nil))
	wantDI(t, out, seq(1, 12))
	if out.Attempts != 5 || out.Halvings != 0 {
		t.Errorf("attempts=%d halvings=%d, want 5 and 0", out.Attempts, out.Halvings)
	}
	snap := e.Metrics().Snapshot()
	if snap.Retries["status_503"] != 2 || snap.Parts["retried"] != 2 {
		t.Errorf("retries %v parts %v", snap.Retries, snap.Parts)
	}
}

func TestWholeRetriesAndExhaustion(t *testing.T) {
	t.Run("retried until served", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{FailFirst: map[int]int{1: 3}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		out := run(t, e, pdfInput(t, "job-w", 3, nil))
		wantDI(t, out, seq(1, 3))
		if out.Attempts != 4 {
			t.Errorf("attempts %d, want 4", out.Attempts)
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{FailFirst: map[int]int{1: 99}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		_, err := e.Run(context.Background(), pdfInput(t, "job-x", 3, nil))
		var apiErr *azerr.APIError
		if !errors.As(err, &apiErr) || !strings.HasSuffix(apiErr.Message, "(after 5 attempts)") {
			t.Fatalf("Run error %v, want the last error after 5 attempts", err)
		}
		if got := len(fake.Calls()); got != WholeAttempts {
			t.Errorf("%d calls, want %d", got, WholeAttempts)
		}
		if e.Metrics().Snapshot().Exhausted != 1 {
			t.Errorf("exhausted not counted")
		}
	})
	t.Run("timeouts retried once", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{Hang: map[int]bool{1: true}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		e.o.WholeTimeout = 100 * time.Millisecond
		_, err := e.Run(context.Background(), pdfInput(t, "job-t", 3, nil))
		var apiErr *azerr.APIError
		if !errors.As(err, &apiErr) || !strings.HasSuffix(apiErr.Message, "(after 2 attempts)") {
			t.Fatalf("Run error %v, want the timeout after 2 attempts", err)
		}
	})
}

func TestRecheck(t *testing.T) {
	t.Run("artefact of splitting", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{BadContent: map[int]bool{6: true}, BadOnlyWhenSplit: true})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		out := run(t, e, pdfInput(t, "job-recheck", 12, nil))
		wantDI(t, out, seq(1, 12))
		if got := e.Metrics().Snapshot().Parts["rechecked"]; got != 1 {
			t.Errorf("rechecked %d, want 1", got)
		}
		if out.Attempts != 4 {
			t.Errorf("attempts %d, want 4 (3 parts and a re-check)", out.Attempts)
		}
	})
	t.Run("genuine failure", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{BadContent: map[int]bool{6: true}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		_, err := e.Run(context.Background(), pdfInput(t, "job-bad", 12, nil))
		var apiErr *azerr.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest ||
			apiErr.InnerCode != azerr.InnerInvalidContent {
			t.Fatalf("Run error %#v, want the container's InvalidContent", err)
		}
		var rechecked bool
		for _, c := range fake.Calls() {
			rechecked = rechecked || slices.Equal(c, []int{5, 6, 7, 8})
		}
		if !rechecked {
			t.Errorf("calls %v: no re-check of pages 5-8", fake.Calls())
		}
	})
}

// A part whose result does not cover its pages fails the merge's checks, and the document is
// analysed whole.
func TestMergeCheckFallsBackToWhole(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{})
	p := newProxy(t, fake.URL(), func(r *http.Request, n int) {
		if n == 1 {
			q := r.URL.Query()
			q.Set("pages", "1-2")
			r.URL.RawQuery = q.Encode()
		}
	})
	e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
	out := run(t, e, pdfInput(t, "job-merge", 12, nil))
	if out.Fallback != "merge_check" || out.Parts != 1 {
		t.Errorf("fallback=%q parts=%d, want merge_check and 1", out.Fallback, out.Parts)
	}
	wantDI(t, out, seq(1, 12))
	if got := e.Metrics().Snapshot().SplitFallbacks["merge_check"]; got != 1 {
		t.Errorf("merge_check fallbacks %d, want 1", got)
	}
}

// A small job is served between a large job's parts, not after all of them.
func TestFairness(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{Latency: 50 * time.Millisecond})
	e := newEngine(t, azerr.SurfaceDI, fake.URL(), 2)
	big, small := pdfInput(t, "job-big", 30, nil), pdfInput(t, "job-small", 1, nil)

	var order []string
	var mu sync.Mutex
	done := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		out, err := e.Run(context.Background(), big)
		if err != nil {
			t.Errorf("big job: %v", err)
			return
		}
		out.Cleanup()
		done("big")
	}()
	// The small job arrives once the big one's parts are queued behind its first two.
	deadline := time.Now().Add(10 * time.Second)
	for e.Sched().Stats().Waiting != 4 {
		if time.Now().After(deadline) {
			t.Fatalf("the big job never queued its parts: %+v", e.Sched().Stats())
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		defer wg.Done()
		out, err := e.Run(context.Background(), small)
		if err != nil {
			t.Errorf("small job: %v", err)
			return
		}
		out.Cleanup()
		done("small")
	}()
	wg.Wait()

	if !slices.Equal(order, []string{"small", "big"}) {
		t.Errorf("finished in order %v, want the small job first", order)
	}
	calls := fake.Calls()
	at := slices.IndexFunc(calls, func(c []int) bool { return len(c) == 1 })
	if at < 0 || at > 3 {
		t.Errorf("the small job's call is call %d of %v, want it before the big job's 4th part", at, sizes(calls))
	}
}

// Cancelling a job mid-split stops every call, leaves no goroutine behind and removes the work
// folder.
func TestCancellation(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{Hang: map[int]bool{5: true}})
	e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
	before := runtime.NumGoroutine()

	in := pdfInput(t, "job-cancel", 12, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := e.Run(ctx, in)
		errc <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.ContainsFunc(fake.Calls(), func(c []int) bool { return slices.Contains(c, 5) }) {
		if time.Now().After(deadline) {
			t.Fatal("the hung part was never sent")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(e.o.WorkRoot, "job-cancel")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("work folder after cancellation: %v", err)
	}
	if s := e.Sched().Stats(); s.InUse != 0 || s.Waiting != 0 {
		t.Errorf("scheduler after cancellation: %+v", s)
	}
	for deadline := time.Now().Add(5 * time.Second); runtime.NumGoroutine() > before; {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines, %d before:\n%s", runtime.NumGoroutine(), before,
				buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Past the job deadline the job fails with a timeout error, not the context's.
func TestJobDeadline(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{Hang: map[int]bool{1: true}})
	e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
	e.jobDeadline = 200 * time.Millisecond
	_, err := e.Run(context.Background(), pdfInput(t, "job-deadline", 3, nil))
	var apiErr *azerr.APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "job deadline") {
		t.Fatalf("Run error %v, want the job deadline's error", err)
	}
}

func TestHedgeDelay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		samples []time.Duration
		want    time.Duration
	}{
		{"too few samples", []time.Duration{time.Second}, HedgeMax},
		{"clamped low", repeat(100*time.Millisecond, HedgeMinSample), HedgeMin},
		{"four times the median", repeat(2*time.Second, HedgeMinSample), 8 * time.Second},
		{"clamped high", repeat(time.Minute, HedgeMinSample), HedgeMax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r ring
			for _, d := range tc.samples {
				r.add(d)
			}
			if got := r.delay(); got != tc.want {
				t.Errorf("delay %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("keeps the last samples", func(t *testing.T) {
		var r ring
		for range HedgeSamples {
			r.add(time.Minute)
		}
		for range HedgeSamples/2 + 1 {
			r.add(time.Second)
		}
		if got := r.delay(); got != HedgeMin {
			t.Errorf("delay %v, want %v once most samples are fast", got, HedgeMin)
		}
	})
}

func repeat(d time.Duration, n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = d
	}
	return out
}

func TestMetricsHistogram(t *testing.T) {
	m := NewMetrics()
	for _, s := range []float64{0.1, 0.3, 1, 400} {
		m.partSeconds(time.Duration(s * float64(time.Second)))
	}
	h := m.Snapshot().PartSeconds
	if !slices.Equal(h.Bounds, []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}) {
		t.Fatalf("bounds %v", h.Bounds)
	}
	if want := []int64{1, 2, 3, 3, 3, 3, 3, 3, 3, 3}; !slices.Equal(h.Counts, want) {
		t.Errorf("counts %v, want %v (cumulative)", h.Counts, want)
	}
	if h.Count != 4 || h.Sum < 401.39 || h.Sum > 401.41 {
		t.Errorf("count %d sum %v", h.Count, h.Sum)
	}
	snap := m.Snapshot()
	for _, k := range []string{"succeeded", "retried", "halved", "hedged", "hedge_won", "rechecked"} {
		if _, ok := snap.Parts[k]; !ok {
			t.Errorf("parts outcome %q missing", k)
		}
	}
	for _, k := range []string{"unparseable", "encrypted", "budget", "panic", "pages_param", "merge_check"} {
		if _, ok := snap.SplitFallbacks[k]; !ok {
			t.Errorf("fallback reason %q missing", k)
		}
	}
}

func TestPagesParamFallback(t *testing.T) {
	fake := newFake(t, mockazure.PagedOptions{})
	p := newProxy(t, fake.URL(), nil)
	e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
	// Page 40 does not exist, so the document goes whole and Azure answers about it.
	_, err := e.Run(context.Background(), pdfInput(t, "job-pages", 12, url.Values{"pages": {"1-40"}}))
	var apiErr *azerr.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("Run error %v, want the container's 400", err)
	}
	if got := e.Metrics().Snapshot().SplitFallbacks["pages_param"]; got != 1 {
		t.Errorf("pages_param fallbacks %d, want 1", got)
	}
	if got := len(p.Bodies()); got != 1 {
		t.Errorf("%d calls, want the whole document once", got)
	}
}
