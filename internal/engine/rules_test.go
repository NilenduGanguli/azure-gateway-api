package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// withMaxResultBytes swaps the engine's client for one whose result ceiling is limit.
func withMaxResultBytes(t *testing.T, e *Engine, baseURL string, limit int64) {
	t.Helper()
	e.o.Client = upstream.NewClient(e.o.Client.Surface(), config.Upstream{
		BaseURL: baseURL, MaxInflight: 4, Timeout: time.Minute,
	}, t.TempDir(), limit)
}

// MAX_RESULT_BYTES caps what the gateway stores and serves for a document, however it was
// analysed: a split document whose merged result passes the ceiling fails exactly as the same
// document sent whole does, rather than serving up to N parts' worth of ceiling.
func TestMergedResultObeysMaxResultBytes(t *testing.T) {
	// Each part's answer is a third of the document's and fits; the merged result does not.
	limit := int64(len(docgen.DI(seq(1, 12), docgen.Original, docgen.Options{}))) - 1

	wholeFake := newFake(t, mockazure.PagedOptions{})
	whole := newEngine(t, azerr.SurfaceDI, wholeFake.URL(), 4)
	withMaxResultBytes(t, whole, wholeFake.URL(), limit)
	whole.o.SplitPages = 0
	_, wholeErr := whole.Run(context.Background(), pdfInput(t, "job-whole", 12, nil))
	var wantErr *azerr.APIError
	if !errors.As(wholeErr, &wantErr) {
		t.Fatalf("whole Run error %v, want the too-large error", wholeErr)
	}

	fake := newFake(t, mockazure.PagedOptions{})
	e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
	withMaxResultBytes(t, e, fake.URL(), limit)
	_, err := e.Run(context.Background(), pdfInput(t, "job-split", 12, nil))
	var apiErr *azerr.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != wantErr.Status || apiErr.Message != wantErr.Message {
		t.Fatalf("split Run error %v, want the whole document's %v", err, wantErr)
	}
	if got := sizes(fake.Calls()); !slices.Equal(got, []int{4, 4, 4}) {
		t.Errorf("call sizes %v, want the three parts and no whole-document fallback", got)
	}
	if got := e.Metrics().Snapshot().SplitFallbacks["merge_check"]; got != 0 {
		t.Errorf("merge_check fallbacks %d, want none: a result too large whole is too large split", got)
	}
	if _, err := os.Stat(filepath.Join(e.o.WorkRoot, "job-split")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("work folder after a failed job: %v", err)
	}
}

// bindLikeASPNET rewrites every case variant of the named query parameters to the name the fake
// reads, keeping every value, as the container's model binding treats them.
func bindLikeASPNET(names ...string) func(r *http.Request, n int) {
	return func(r *http.Request, _ int) {
		q := r.URL.Query()
		for k, vs := range q {
			for _, name := range names {
				if k != name && strings.EqualFold(k, name) {
					delete(q, k)
					q[name] = append(q[name], vs...)
				}
			}
		}
		r.URL.RawQuery = q.Encode()
	}
}

// queryRecorder records every query a proxy forwards, before any hook changes it.
type queryRecorder struct {
	mu      sync.Mutex
	queries []url.Values
}

func (q *queryRecorder) hook(next func(r *http.Request, n int)) func(r *http.Request, n int) {
	return func(r *http.Request, n int) {
		q.mu.Lock()
		q.queries = append(q.queries, r.URL.Query())
		q.mu.Unlock()
		if next != nil {
			next(r, n)
		}
	}
}

func (q *queryRecorder) all() []url.Values {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.queries)
}

// keysFold lists the query's keys that equal name under case folding.
func keysFold(q url.Values, name string) []string {
	var out []string
	for k := range q {
		if strings.EqualFold(k, name) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// The container binds query parameters by name without regard to case, so the engine must too:
// Pages= is the client's page selection, and no part may carry it.
func TestPagesParamAnyCase(t *testing.T) {
	t.Run("Pages selects the pages that are split", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		var rec queryRecorder
		p := newProxy(t, fake.URL(), rec.hook(bindLikeASPNET("pages")))
		e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
		out := run(t, e, pdfInput(t, "job-Pages", 12, url.Values{"Pages": {"3-10"}}))
		wantDI(t, out, seq(3, 10))
		got := sizes(fake.Calls())
		slices.Sort(got)
		if !slices.Equal(got, []int{4, 4}) {
			t.Errorf("call sizes %v, want two 4-page parts of pages 3-10", got)
		}
		for _, q := range rec.all() {
			if k := keysFold(q, "pages"); len(k) > 0 {
				t.Errorf("a part was sent %v=%v; a part is its own document", k, q[k[0]])
			}
		}
	})

	t.Run("Pages selecting few pages is sent whole as the client sent it", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		var rec queryRecorder
		p := newProxy(t, fake.URL(), rec.hook(bindLikeASPNET("pages")))
		e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
		out := run(t, e, pdfInput(t, "job-few", 6, url.Values{"Pages": {"1-3"}}))
		wantDI(t, out, seq(1, 3))
		qs := rec.all()
		if len(qs) != 1 || qs[0].Get("Pages") != "1-3" {
			t.Errorf("queries %v, want one whole call with the client's Pages=1-3", qs)
		}
	})

	t.Run("two spellings are sent whole", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		var rec queryRecorder
		p := newProxy(t, fake.URL(), rec.hook(nil))
		e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
		q := url.Values{"pages": {"1-6"}, "PAGES": {"7-12"}}
		_, _ = e.Run(context.Background(), pdfInput(t, "job-two", 12, q))
		if got := e.Metrics().Snapshot().SplitFallbacks["pages_param"]; got != 1 {
			t.Errorf("pages_param fallbacks %d, want 1", got)
		}
		qs := rec.all()
		if len(qs) != 1 || !slices.Equal(keysFold(qs[0], "pages"), []string{"PAGES", "pages"}) {
			t.Errorf("queries %v, want one whole call with both spellings as sent", qs)
		}
	})

	t.Run("a re-check replaces every spelling", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{BadContent: map[int]bool{6: true}, BadOnlyWhenSplit: true})
		var rec queryRecorder
		p := newProxy(t, fake.URL(), rec.hook(bindLikeASPNET("pages")))
		e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
		out := run(t, e, pdfInput(t, "job-recheck", 12, url.Values{"Pages": {"1-12"}}))
		wantDI(t, out, seq(1, 12))
		var rechecks int
		for _, q := range rec.all() {
			if k := keysFold(q, "pages"); len(k) > 0 {
				rechecks++
				if !slices.Equal(k, []string{"pages"}) || len(q["pages"]) != 1 {
					t.Errorf("re-check query %v, want exactly one pages value", q)
				}
			}
		}
		if rechecks != 1 {
			t.Errorf("%d calls carried pages, want the one re-check", rechecks)
		}
	})
}

// stringIndexType decides the merge's offset unit, so it is read the way the container binds it,
// and a value the merge cannot honour sends the document whole before any part is analysed.
func TestStringIndexTypeAnyCase(t *testing.T) {
	t.Run("StringIndexType is the merge's unit", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{})
		p := newProxy(t, fake.URL(), bindLikeASPNET("stringIndexType"))
		e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
		q := url.Values{"StringIndexType": {"utf16CodeUnit"}}
		out := run(t, e, pdfInput(t, "job-unit", 12, q))
		want := docgen.DI(seq(1, 12), docgen.Original, docgen.Options{Unit: textunit.UTF16CodeUnit})
		if got := result(t, out); !bytes.Equal(got, want) {
			t.Fatalf("result differs from the whole document's in utf16CodeUnit:\n got %.300s\nwant %.300s",
				got, want)
		}
		if out.Parts != 3 || out.Fallback != "" {
			t.Errorf("parts=%d fallback=%q, want 3 and none", out.Parts, out.Fallback)
		}
	})

	for _, tc := range []struct {
		name  string
		query url.Values
	}{
		{"unknown value", url.Values{"stringIndexType": {"bytes"}}},
		{"two spellings", url.Values{"stringIndexType": {"textElements"}, "StringIndexType": {"utf16CodeUnit"}}},
		{"two values", url.Values{"stringIndexType": {"textElements", "utf16CodeUnit"}}},
	} {
		t.Run(tc.name+" is sent whole, before any part", func(t *testing.T) {
			fake := newFake(t, mockazure.PagedOptions{})
			p := newProxy(t, fake.URL(), nil)
			e := newEngine(t, azerr.SurfaceDI, p.URL, 4)
			in := pdfInput(t, "job-sit", 12, tc.query)
			whole, _ := os.ReadFile(in.Path)
			// The container answers about the value itself; the gateway only has to ask it once,
			// with the document as the client sent it.
			_, _ = e.Run(context.Background(), in)
			if bodies := p.Bodies(); len(bodies) != 1 || !bytes.Equal(bodies[0], whole) {
				t.Errorf("%d calls, want the whole document once", len(bodies))
			}
			if got := e.Metrics().Snapshot().SplitFallbacks["string_index_type"]; got != 1 {
				t.Errorf("string_index_type fallbacks %d, want 1", got)
			}
		})
	}
}

// A single page that exhausts its part attempts on a retriable failure (a timeout, a 503, a lost
// connection) is not yet Azure failing the document: the part timeout is far shorter than the
// whole document's, and it also covers time queued in HAProxy. So the page is re-checked against
// the original under the whole-document rules, and only that answer can fail the job.
func TestSinglePageExhaustionIsRechecked(t *testing.T) {
	t.Run("served by the re-check", func(t *testing.T) {
		// Page 9 fails its first nine calls: three at 4 pages, three at 2, three alone.
		fake := newFake(t, mockazure.PagedOptions{FailFirst: map[int]int{9: 9}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		out := run(t, e, pdfInput(t, "job-slow-page", 12, nil))
		wantDI(t, out, seq(1, 12))
		snap := e.Metrics().Snapshot()
		if snap.Parts["rechecked"] != 1 || snap.Exhausted != 0 {
			t.Errorf("rechecked=%d exhausted=%d, want 1 and 0", snap.Parts["rechecked"], snap.Exhausted)
		}
	})

	t.Run("fails only when the re-check fails", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{Hang: map[int]bool{9: true}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		e.partTimeout = 200 * time.Millisecond
		e.o.WholeTimeout = 300 * time.Millisecond
		_, err := e.Run(context.Background(), pdfInput(t, "job-hung-page", 12, nil))
		var apiErr *azerr.APIError
		if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "timeout") ||
			strings.Contains(apiErr.Message, "attempts") {
			t.Fatalf("Run error %v, want the re-check's timeout, its message as it was", err)
		}
		var rechecks int
		for _, c := range fake.Calls() {
			if slices.Equal(c, []int{9}) {
				rechecks++
			}
		}
		// Three part attempts on page 9 alone, then two re-check attempts of the original.
		if rechecks != 5 {
			t.Errorf("%d calls of page 9 alone, want 3 part attempts and 2 re-check attempts", rechecks)
		}
	})
}

// A failed job still reports what it cost, so the job record and the logs say how hard the
// gateway tried: the calls made, the parts planned, and the last status a container answered.
func TestFailedRunReportsItsCounts(t *testing.T) {
	t.Run("whole", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{FailFirst: map[int]int{1: 99}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		out, err := e.Run(context.Background(), pdfInput(t, "job-x", 3, nil))
		if err == nil || out == nil {
			t.Fatalf("Run = %v, %v; want the failure and its counts", out, err)
		}
		if out.Attempts != WholeAttempts || out.Parts != 1 || out.UpstreamStatus != http.StatusServiceUnavailable {
			t.Errorf("attempts=%d parts=%d status=%d, want %d, 1, 503", out.Attempts, out.Parts,
				out.UpstreamStatus, WholeAttempts)
		}
		if out.Path != "" {
			t.Errorf("a failed run's output names a result: %q", out.Path)
		}
		out.Cleanup()
	})
	t.Run("split", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{BadContent: map[int]bool{6: true}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		out, err := e.Run(context.Background(), pdfInput(t, "job-bad", 12, nil))
		if err == nil || out == nil {
			t.Fatalf("Run = %v, %v; want the failure and its counts", out, err)
		}
		// Three parts, and the re-check of the failed one; a part cancelled by the failure may or
		// may not have been counted, so the attempts are bounded rather than exact.
		if out.Parts != 3 || out.Attempts < 2 || out.Attempts > 4 || out.UpstreamStatus != http.StatusBadRequest {
			t.Errorf("attempts=%d parts=%d status=%d, want 2-4, 3, 400", out.Attempts, out.Parts, out.UpstreamStatus)
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		fake := newFake(t, mockazure.PagedOptions{Hang: map[int]bool{1: true}})
		e := newEngine(t, azerr.SurfaceDI, fake.URL(), 4)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		out, err := e.Run(ctx, pdfInput(t, "job-stop", 3, nil))
		if !errors.Is(err, context.DeadlineExceeded) || out != nil {
			t.Errorf("Run = %v, %v; want the context's error and no output: the job is resumed", out, err)
		}
	})
}
