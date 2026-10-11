package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/engine"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
)

// splitPages is the part size these tests split at: small enough that a 40-page document is eight
// parts, so merging, ordering and every rewritten offset are exercised.
const splitPages = 5

// envelopeHead is the start of a succeeded envelope, up to its analyzeResult.
var envelopeHead = regexp.MustCompile(`^\{"status":"succeeded","createdDateTime":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ",` +
	`"lastUpdatedDateTime":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ","analyzeResult":$`)

// newSplitHarness is a gateway in front of paged fakes, splitting at split pages (0: off).
func newSplitHarness(t *testing.T, split int, o harnessOpts) *harness {
	t.Helper()
	o.paged, o.splitPages = true, split
	tweak := o.tweak
	o.tweak = func(c *config.Config) {
		c.DI.MaxInflight, c.Read.MaxInflight, c.QueueDepth = 4, 4, 16
		if tweak != nil {
			tweak(c)
		}
	}
	return newHarness(t, o)
}

// assertEnvelopeAround checks body is exactly a succeeded envelope whose analyzeResult is want,
// byte for byte: the merged parts must be indistinguishable from the container's answer for the
// whole document.
func assertEnvelopeAround(t *testing.T, body, want []byte) {
	t.Helper()
	tail := append(slices.Clone(want), '}')
	if !bytes.HasSuffix(body, tail) {
		var env struct{ AnalyzeResult json.RawMessage }
		_ = json.Unmarshal(body, &env)
		t.Fatalf("analyzeResult differs from the whole document's (%d bytes, want %d):\n got %.300s\nwant %.300s",
			len(env.AnalyzeResult), len(want), env.AnalyzeResult, want)
	}
	if head := body[:len(body)-len(tail)]; !envelopeHead.Match(head) {
		t.Errorf("envelope head is %s, want status, createdDateTime, lastUpdatedDateTime in order", head)
	}
}

// assertPartsCover checks the container saw more than one call, none larger than a part, covering
// exactly pages between them.
func assertPartsCover(t *testing.T, c *mockazure.Container, pages []int) {
	t.Helper()
	calls := c.Calls()
	if len(calls) < 2 {
		t.Fatalf("the container saw %d calls (%v); the document was not split", len(calls), calls)
	}
	seen := map[int]bool{}
	for _, call := range calls {
		if len(call) > splitPages {
			t.Errorf("a call covered %d pages (%v), more than SPLIT_PAGES %d", len(call), call, splitPages)
		}
		for _, p := range call {
			seen[p] = true
		}
	}
	for _, p := range pages {
		if !seen[p] {
			t.Errorf("page %d was never analysed", p)
		}
		delete(seen, p)
	}
	if len(seen) != 0 {
		t.Errorf("pages outside the request were analysed: %v", seen)
	}
}

func seq(from, to int) []int {
	out := make([]int, 0, to-from+1)
	for p := from; p <= to; p++ {
		out = append(out, p)
	}
	return out
}

// TestSplitDocumentEqualsWhole is the splitting design's central promise (§10): a 40-page PDF sent
// through :analyze is cut into parts, each part is its own synchronous call, and the polled
// result is the envelope around exactly what the container would have answered for the whole
// document. The same holds for :syncAnalyze and for Read, and /_gw/jobs reports the parts.
func TestSplitDocumentEqualsWhole(t *testing.T) {
	t.Parallel()
	doc := pdftest.Make(40)
	wantDI := docgen.DI(seq(1, 40), docgen.Original, docgen.Options{})

	t.Run("DI :analyze", func(t *testing.T) {
		t.Parallel()
		h := newSplitHarness(t, splitPages, harnessOpts{})
		loc := h.submit(diAnalyze, doc)
		assertEnvelopeAround(t, h.awaitSucceeded(loc), wantDI)
		assertPartsCover(t, h.di, seq(1, 40))

		// /_gw/jobs reports how many parts the job took, on the job and in the list.
		calls := len(h.di.Calls())
		resp, body := h.do(http.MethodGet, "/_gw/jobs/"+opID(loc), nil)
		var job struct{ UpstreamParts, UpstreamAttempts int }
		if err := json.Unmarshal(body, &job); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("/_gw/jobs/{id}: %d %s", resp.StatusCode, body)
		}
		if job.UpstreamParts != calls || job.UpstreamAttempts != calls {
			t.Errorf("upstreamParts %d, upstreamAttempts %d; want %d each for %d clean part calls",
				job.UpstreamParts, job.UpstreamAttempts, calls, calls)
		}
		_, body = h.do(http.MethodGet, "/_gw/jobs", nil)
		var list struct {
			Jobs []struct {
				ID            string
				UpstreamParts int
			}
		}
		_ = json.Unmarshal(body, &list)
		if len(list.Jobs) != 1 || list.Jobs[0].UpstreamParts != calls {
			t.Errorf("/_gw/jobs = %s, want the one job with upstreamParts %d", body, calls)
		}
	})

	t.Run("DI :syncAnalyze", func(t *testing.T) {
		t.Parallel()
		h := newSplitHarness(t, splitPages, harnessOpts{})
		resp, body := h.do(http.MethodPost, diSync, doc)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", resp.StatusCode, truncate(string(body)))
		}
		// The gateway's sync route answers what its async route serves.
		assertEnvelopeAround(t, body, wantDI)
		assertPartsCover(t, h.di, seq(1, 40))
		if list, _ := h.store.Recent(t.Context(), 10); len(list) != 0 {
			t.Errorf("a synchronous call left %d job rows", len(list))
		}
	})

	t.Run("Read analyze and syncAnalyze", func(t *testing.T) {
		t.Parallel()
		h := newSplitHarness(t, splitPages, harnessOpts{})
		want := docgen.Read(seq(1, 40), docgen.Original)
		assertEnvelopeAround(t, h.awaitSucceeded(h.submit(readAnalyze, doc)), want)
		assertPartsCover(t, h.read, seq(1, 40))

		resp, body := h.do(http.MethodPost, readSync, doc)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("syncAnalyze status %d: %s", resp.StatusCode, truncate(string(body)))
		}
		assertEnvelopeAround(t, body, want)
	})

	t.Run("pages=10-25", func(t *testing.T) {
		t.Parallel()
		h := newSplitHarness(t, splitPages, harnessOpts{})
		loc := h.submit(diAnalyze+"&pages=10-25", doc)
		// The container's answer for the original with pages=10-25: those pages, numbered as
		// they are in the document.
		assertEnvelopeAround(t, h.awaitSucceeded(loc), docgen.DI(seq(10, 25), docgen.Original, docgen.Options{}))
		assertPartsCover(t, h.di, seq(10, 25))
	})
}

// TestSplitRecovers covers the part rules (splitting design §4) end to end: a part whose first
// call hangs is hedged, a part the container refuses only when cut out is re-checked against the
// original, and either way the client gets the whole document's result.
func TestSplitRecovers(t *testing.T) {
	t.Parallel()
	want := docgen.DI(seq(1, 40), docgen.Original, docgen.Options{})

	t.Run("first call of a part hangs", func(t *testing.T) {
		t.Parallel()
		h := newSplitHarness(t, splitPages, harnessOpts{
			diPaged: mockazure.PagedOptions{HangFirst: map[int]bool{38: true}},
		})
		// The hedge delay is HedgeMax until the engine has HedgeMinSample part durations, and the
		// part timeout is a minute; neither suits a test. Fast parts first bring the delay down
		// to HedgeMin. Page 38 is beyond these documents, so they never touch the hang.
		perDoc := 35 / splitPages
		for range (engine.HedgeMinSample + perDoc - 1) / perDoc {
			h.awaitSucceeded(h.submit(diAnalyze, pdftest.Make(35)))
		}
		start := time.Now()
		assertEnvelopeAround(t, h.awaitSucceeded(h.submit(diAnalyze, pdftest.Make(40))), want)
		if took := time.Since(start); took > engine.HedgeMax {
			t.Errorf("the hung part took %v; it should have been hedged after about %v", took, engine.HedgeMin)
		}
		m := h.manager.Engine(jobs.SurfaceDI).Metrics().Snapshot()
		if m.Parts["hedged"] < 1 || m.Parts["hedge_won"] < 1 {
			t.Errorf("part outcomes %v; want the hung part hedged and the hedge winning", m.Parts)
		}
	})

	t.Run("bad content only when split", func(t *testing.T) {
		t.Parallel()
		h := newSplitHarness(t, splitPages, harnessOpts{diPaged: mockazure.PagedOptions{
			BadContent: map[int]bool{38: true}, BadOnlyWhenSplit: true,
		}})
		assertEnvelopeAround(t, h.awaitSucceeded(h.submit(diAnalyze, pdftest.Make(40))), want)
		if m := h.manager.Engine(jobs.SurfaceDI).Metrics().Snapshot(); m.Parts["rechecked"] < 1 {
			t.Errorf("part outcomes %v; want the refused part re-checked", m.Parts)
		}
	})
}

// TestSplitFallsBackToWhole covers the documents splitting must not touch: a PDF the splitter
// cannot parse goes whole, and SPLIT_PAGES 0 turns splitting off. Either way the container sees
// one call.
func TestSplitFallsBackToWhole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		splitPages int
		doc        []byte
		pages      []int
		fallback   string
	}{
		// The fake reads an unparseable document as one page, as it does an image.
		{"unparseable PDF", splitPages, []byte("%PDF-1.7 this is not a parseable PDF"), []int{1}, "unparseable"},
		{"SPLIT_PAGES 0", 0, pdftest.Make(40), seq(1, 40), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSplitHarness(t, tc.splitPages, harnessOpts{})
			body := h.awaitSucceeded(h.submit(diAnalyze, tc.doc))
			assertEnvelopeAround(t, body, docgen.DI(tc.pages, docgen.Original, docgen.Options{}))
			if calls := h.di.Calls(); len(calls) != 1 || !slices.Equal(calls[0], tc.pages) {
				t.Errorf("the container saw %v, want one call covering %v", calls, tc.pages)
			}
			m := h.manager.Engine(jobs.SurfaceDI).Metrics().Snapshot()
			if tc.fallback != "" && m.SplitFallbacks[tc.fallback] != 1 {
				t.Errorf("split fallbacks %v, want one %q", m.SplitFallbacks, tc.fallback)
			}
		})
	}
}

// TestCapacityCappedPodsStillServeEveryJob is the sync-only design's load test (§11): the gateway
// allows 16 calls at once, HAProxy in front of the pods admits 4 and answers the rest 503 with
// Retry-After, and every one of 50 jobs still succeeds, because each busy answer is retried.
func TestCapacityCappedPodsStillServeEveryJob(t *testing.T) {
	t.Parallel()
	const jobsN = 50
	h := newHarness(t, harnessOpts{
		// The latency keeps calls in flight long enough for the cap to bite.
		di: mockazure.Options{Capacity: 4, Latency: 25 * time.Millisecond},
		tweak: func(c *config.Config) {
			c.DI.MaxInflight, c.QueueDepth = 16, jobsN
		},
	})

	// Submitted at once, so the gateway's 16 slots fill together and the cap has calls to refuse.
	locs := make([]string, jobsN)
	errs := make([]error, jobsN)
	var wg sync.WaitGroup
	for i := range jobsN {
		wg.Go(func() {
			resp, err := noRedirect().Post(h.gateway.URL+diAnalyze, "application/pdf",
				strings.NewReader(fmt.Sprintf("%%PDF-1.7 job %d", i)))
			if err != nil {
				errs[i] = err
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				errs[i] = fmt.Errorf("conformance: submit %d answered %d", i, resp.StatusCode)
			}
			locs[i] = resp.Header.Get("Operation-Location")
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	// Polled one after another: the jobs run concurrently in the gateway whatever the client does.
	for _, loc := range locs {
		h.awaitSucceeded(loc)
	}

	if h.di.SyncCalls() <= jobsN {
		t.Errorf("the cap never refused a call (%d calls for %d jobs), so nothing was retried",
			h.di.SyncCalls(), jobsN)
	}
}
