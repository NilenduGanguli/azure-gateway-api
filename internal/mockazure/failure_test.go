package mockazure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// syncRoute names one synchronous analyze route and the constructor of the fake that serves it.
// The legacy /formrecognizer family is listed on its own because NewDI registers a second copy of
// the handler for it: a failure mode wired into only one copy would pass every other test here.
type syncRoute struct {
	name  string
	make  func(Options) *Container
	path  string
	codes map[string]bool // the error codes this surface may send
}

// The error vocabularies, from docs/api-surface.md §6. Read's enum has no ServiceUnavailable or
// NotFound, and the document says never to emit them on that surface.
var (
	diErrorCodes = codeSet("InvalidRequest", "InvalidArgument", "Unauthorized", "Forbidden", "NotFound",
		"MethodNotAllowed", "Conflict", "UnsupportedMediaType", "InternalServerError", "ServiceUnavailable")
	readErrorCodes = codeSet("InvalidImageFormat", "UnsupportedMediaType", "InvalidImageUrl",
		"InternalServerError", "InvalidImageSize", "BadArgument", "NotSupportedLanguage", "FailedToProcess",
		"Unspecified", "StorageException", "InvalidPageRange", "FailedToDownloadImage", "InvalidImage",
		"UnsupportedImageFormat", "InvalidImageDimension", "InvalidReadingOrder", "InvalidRequest")
)

func codeSet(codes ...string) map[string]bool {
	set := make(map[string]bool, len(codes))
	for _, code := range codes {
		set[code] = true
	}
	return set
}

const (
	diSyncPath     = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	legacySyncPath = "/formrecognizer/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	readSyncPath   = "/vision/v3.2/read/syncAnalyze"
)

var syncRoutes = []syncRoute{
	{"document intelligence", NewDI, diSyncPath, diErrorCodes},
	{"legacy form recognizer family", NewDI, legacySyncPath, diErrorCodes},
	{"computer vision read", NewRead, readSyncPath, readErrorCodes},
}

// newPerAttemptClient returns a client that opens a connection per call, as the gateway's
// attempts do. Reusing a connection the fake has just torn down would fail the next call for a
// reason that is not the behaviour under test.
func newPerAttemptClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   30 * time.Second,
	}
}

// syncReply is one response read to the end.
type syncReply struct {
	status int
	header http.Header
	length int64 // the Content-Length the headers promised; -1 when absent
	body   []byte
	err    error // from reading the body: non-nil only when the connection was cut
}

// trySync POSTs a document and reads the answer. It never calls t, so goroutines may use it.
func trySync(cl *http.Client, c *Container, path string) (syncReply, error) {
	resp, err := cl.Post(c.URL()+path, "application/pdf", strings.NewReader("%PDF-1.7 fake"))
	if err != nil {
		return syncReply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	return syncReply{
		status: resp.StatusCode, header: resp.Header, length: resp.ContentLength,
		body: body, err: readErr,
	}, nil
}

func callSync(t *testing.T, cl *http.Client, c *Container, path string) syncReply {
	t.Helper()
	r, err := trySync(cl, c, path)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return r
}

// waitUntil polls until cond holds. Counters the handlers update are the only way to know a request
// has reached a given point, and a fixed sleep there is a flake waiting to happen.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// containerErrorBody is the wrapped error object both containers produce.
type containerErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// wantHAProxyBusy asserts r is what HAProxy sends when no server can take a request: a 503 whose
// body is an HTML page, not a container error object, carrying Retry-After: 1.
func wantHAProxyBusy(t *testing.T, label string, r syncReply) {
	t.Helper()
	if r.status != http.StatusServiceUnavailable {
		t.Errorf("%s: status %d, want 503", label, r.status)
	}
	if got := r.header.Get("Retry-After"); got != "1" {
		t.Errorf("%s: Retry-After %q, want 1; the gateway adopts it as its backoff", label, got)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("%s: Content-Type %q, want text/html", label, ct)
	}
	if json.Valid(r.body) {
		t.Errorf("%s: HAProxy's page is HTML but the body parses as JSON: %s", label, r.body)
	}
	if !strings.Contains(strings.ToLower(string(r.body)), "<html") {
		t.Errorf("%s: body is not an HTML page: %q", label, r.body)
	}
}

func TestBusyFirstAnswersLikeHAProxyThenServes(t *testing.T) {
	for _, rt := range syncRoutes {
		t.Run(rt.name, func(t *testing.T) {
			c := rt.make(Options{BusyFirst: 2})
			defer c.Close()
			cl := newPerAttemptClient()

			for n := 1; n <= 2; n++ {
				wantHAProxyBusy(t, fmt.Sprintf("call %d", n), callSync(t, cl, c, rt.path))
			}

			r := callSync(t, cl, c, rt.path)
			if r.status != http.StatusOK {
				t.Fatalf("call 3: status %d, want 200 once the first two were turned away", r.status)
			}
			var env map[string]any
			if err := json.Unmarshal(r.body, &env); err != nil || env["analyzeResult"] == nil {
				t.Errorf("call 3: not a result (%v): %s", err, r.body)
			}
			if got := c.SyncCalls(); got != 3 {
				t.Errorf("SyncCalls is %d, want 3", got)
			}
		})
	}
}

func TestStatusFirstAnswersWithAContainerShapedError(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504, 408, 429} {
		for _, rt := range syncRoutes {
			t.Run(fmt.Sprintf("%d on %s", code, rt.name), func(t *testing.T) {
				c := rt.make(Options{StatusFirst: 1, StatusFirstCode: code})
				defer c.Close()
				cl := newPerAttemptClient()

				first := callSync(t, cl, c, rt.path)
				if first.status != code {
					t.Fatalf("call 1: status %d, want %d", first.status, code)
				}
				if ct := first.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Errorf("call 1: Content-Type %q, want application/json", ct)
				}
				var body containerErrorBody
				if err := json.Unmarshal(first.body, &body); err != nil {
					t.Fatalf("call 1: body is not an error object (%v): %s", err, first.body)
				}
				if body.Error.Code == "" || body.Error.Message == "" {
					t.Errorf("call 1: error object lacks a code or a message: %s", first.body)
				}
				if !rt.codes[body.Error.Code] {
					t.Errorf("call 1: code %q is not in this surface's error vocabulary: %s",
						body.Error.Code, first.body)
				}
				// A 500 that reads like a missing route is terminal, not retriable. These are
				// ordinary transient failures and must not be mistaken for one.
				reads := strings.ToLower(body.Error.Code + body.Error.Message)
				if strings.Contains(reads, "unhandledendpoint") {
					t.Errorf("call 1: error reads like a missing endpoint: %s", first.body)
				}
				// A container does not send Retry-After. Without one the gateway's own backoff is
				// what a test exercises.
				if got := first.header.Get("Retry-After"); got != "" {
					t.Errorf("call 1: Retry-After %q on a container error", got)
				}

				if second := callSync(t, cl, c, rt.path); second.status != http.StatusOK {
					t.Errorf("call 2: status %d, want 200", second.status)
				}
				if got := c.SyncCalls(); got != 2 {
					t.Errorf("SyncCalls is %d, want 2", got)
				}
			})
		}
	}
}

func TestResetMidBodyDropsTheConnectionHalfWayThrough(t *testing.T) {
	// 1 MiB of padding checks that a body too large for one write is cut as cleanly as a small one.
	for _, pad := range []int{0, 1 << 20} {
		for _, rt := range syncRoutes {
			t.Run(fmt.Sprintf("%s pad %d", rt.name, pad), func(t *testing.T) {
				c := rt.make(Options{ResetMidBody: true, PadResultBytes: pad})
				defer c.Close()

				r := callSync(t, newPerAttemptClient(), c, rt.path)
				if r.status != http.StatusOK {
					t.Fatalf("status %d, want 200: the reset happens after the headers are out", r.status)
				}
				if r.length <= 0 {
					t.Fatalf("Content-Length is %d; the headers must promise the whole body", r.length)
				}
				if r.err == nil {
					t.Fatalf("read all %d bytes without error; the body was not cut", len(r.body))
				}
				if !errors.Is(r.err, io.ErrUnexpectedEOF) && !errors.Is(r.err, syscall.ECONNRESET) {
					t.Errorf("read error is %v, want an unexpected EOF or a connection reset", r.err)
				}
				if want := r.length / 2; int64(len(r.body)) != want {
					t.Errorf("client received %d bytes, want half of the promised %d", len(r.body), r.length)
				}
				if got := c.SyncCalls(); got != 1 {
					t.Errorf("SyncCalls is %d, want 1", got)
				}
			})
		}
	}
}

// A response writer that cannot be hijacked (HTTP/2 is one) still has to end up with a cut body,
// so the handler is aborted instead. httptest.NewServer always serves HTTP/1.1, so this branch is
// only reachable by handing the writer in directly.
func TestResetMidBodyAbortsTheHandlerWhenTheConnectionCannotBeHijacked(t *testing.T) {
	c := &Container{opts: Options{ResetMidBody: true}}
	rec := httptest.NewRecorder() // a Flusher, but not a Hijacker

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		c.writeSyncBody(rec, []byte(`{"status":"succeeded"}`))
	}()

	if recovered != http.ErrAbortHandler {
		t.Fatalf("recovered %v, want http.ErrAbortHandler", recovered)
	}
	res := rec.Result()
	if res.StatusCode != http.StatusOK || res.ContentLength <= 0 {
		t.Errorf("status %d, Content-Length %d; want a 200 promising the whole body",
			res.StatusCode, res.ContentLength)
	}
	if got, want := int64(rec.Body.Len()), res.ContentLength/2; got != want {
		t.Errorf("wrote %d bytes before aborting, want half of %d", got, res.ContentLength)
	}
}

func TestCapacityRejectsOnlyCallsAboveTheCap(t *testing.T) {
	const capacity, callers = 2, 4
	for _, rt := range syncRoutes {
		t.Run(rt.name, func(t *testing.T) {
			// Long enough that all four callers arrive while the first two are still in flight.
			c := rt.make(Options{Capacity: capacity, Latency: 400 * time.Millisecond})
			defer c.Close()
			cl := newPerAttemptClient()

			replies := make([]syncReply, callers)
			errs := make([]error, callers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					replies[i], errs[i] = trySync(cl, c, rt.path)
				}()
			}
			close(start)
			wg.Wait()

			ok, busy := 0, 0
			for i, r := range replies {
				if errs[i] != nil {
					t.Fatalf("caller %d: %v", i, errs[i])
				}
				switch r.status {
				case http.StatusOK:
					ok++
				case http.StatusServiceUnavailable:
					busy++
					wantHAProxyBusy(t, fmt.Sprintf("caller %d", i), r)
				default:
					t.Errorf("caller %d: unexpected status %d", i, r.status)
				}
			}
			if ok != capacity || busy != callers-capacity {
				t.Fatalf("%d succeeded and %d were turned away, want %d and %d",
					ok, busy, capacity, callers-capacity)
			}

			// Once the admitted calls have finished the slots are free again.
			if r := callSync(t, cl, c, rt.path); r.status != http.StatusOK {
				t.Errorf("call after the burst: status %d, want 200", r.status)
			}
			if got := c.SyncCalls(); got != callers+1 {
				t.Errorf("SyncCalls is %d, want %d", got, callers+1)
			}
		})
	}
}

// Capacity limits calls in flight, not calls made: one at a time never exceeds a cap of one, so
// every slot must be given back on the way out.
func TestCapacityIsNotACallBudget(t *testing.T) {
	c := NewDI(Options{Capacity: 1})
	defer c.Close()
	cl := newPerAttemptClient()

	for n := 1; n <= 5; n++ {
		if r := callSync(t, cl, c, diSyncPath); r.status != http.StatusOK {
			t.Fatalf("sequential call %d: status %d, want 200; a slot was not released", n, r.status)
		}
	}
}

// A hung call stays in flight until its client gives up. This pins both halves without any timing:
// the second call can only be answered at all by being turned away, and the slot only comes back
// when the client disconnects.
func TestCapacityTurnsAwayCallsWhileOthersAreStillInFlight(t *testing.T) {
	c := NewDI(Options{Capacity: 1, Sync: SyncHang})
	defer c.Close()
	cl := newPerAttemptClient()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL()+diSyncPath,
			strings.NewReader("%PDF-1.7 fake"))
		if err != nil {
			held <- err
			return
		}
		resp, err := cl.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		held <- err
	}()
	waitUntil(t, "the first call to take the only slot",
		func() bool { return c.inflight.Load() == 1 })

	wantHAProxyBusy(t, "second call", callSync(t, cl, c, diSyncPath))

	cancel()
	if err := <-held; !errors.Is(err, context.Canceled) {
		t.Errorf("hung call ended with %v, want context canceled", err)
	}
	waitUntil(t, "the abandoned call to release its slot",
		func() bool { return c.inflight.Load() == 0 })
}

// Neither answer reaches the container, so a slow container does not make a busy router slow. The
// margin is wide on purpose: a correct fake answers in milliseconds, a wrong one in Latency.
func TestBusyAndStatusAnswersAreNotSlowedByLatency(t *testing.T) {
	const latency = 3 * time.Second
	c := NewDI(Options{
		Latency: latency, BusyFirst: 1, StatusFirst: 1, StatusFirstCode: http.StatusBadGateway,
	})
	defer c.Close()
	cl := newPerAttemptClient()

	for n := 1; n <= 2; n++ {
		begun := time.Now()
		callSync(t, cl, c, diSyncPath)
		if took := time.Since(begun); took > latency/2 {
			t.Errorf("call %d took %v; an answer given ahead of the container must not wait for Latency (%v)",
				n, took, latency)
		}
	}
}

// Thirty callers race for three slots. Each admission is one compare-and-swap, so no interleaving
// can let a fourth in. The admitted calls hang, which makes the outcome independent of timing: a
// call is turned away only when three others are in flight, and none of them can leave.
func TestCapacityNeverAdmitsMoreThanTheCapUnderContention(t *testing.T) {
	const capacity, callers = 3, 30
	c := NewDI(Options{Capacity: capacity, Sync: SyncHang})
	defer c.Close()
	cl := newPerAttemptClient()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var turnedAway atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL()+diSyncPath,
				strings.NewReader("%PDF-1.7 fake"))
			if err != nil {
				return
			}
			resp, err := cl.Do(req)
			if err != nil {
				return // a hung call, ended by the cancel below
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusServiceUnavailable {
				turnedAway.Add(1)
			}
		}()
	}
	close(start)

	waitUntil(t, "every call over the cap to be turned away",
		func() bool { return turnedAway.Load() == callers-capacity })
	if got := c.inflight.Load(); got != capacity {
		t.Errorf("%d calls in flight, want exactly %d", got, capacity)
	}

	cancel()
	wg.Wait()
	waitUntil(t, "the abandoned calls to release their slots",
		func() bool { return c.inflight.Load() == 0 })
}

// The HTTP-level test above cannot reliably provoke a check-then-add race: the window is
// nanoseconds wide, and it exists only while slots are still free. So the admission step is raced
// directly, as many short rounds on a fresh container, each released by a barrier. Slots are never
// given back, so a correct compare-and-swap admits exactly Capacity callers whatever the schedule,
// and a racy check-then-add admits more.
func TestAdmitNeverExceedsCapacityWhenRaced(t *testing.T) {
	const capacity, workers, rounds = 3, 8, 5000
	for round := range rounds {
		c := &Container{opts: Options{Capacity: capacity}}
		var admitted atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if c.admit() {
					admitted.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()

		if got := admitted.Load(); got != capacity {
			t.Fatalf("round %d: %d callers admitted, want exactly %d", round, got, capacity)
		}
		if got := c.inflight.Load(); got != capacity {
			t.Fatalf("round %d: %d in flight, want %d", round, got, capacity)
		}
	}
}

// With no Capacity set nothing is ever turned away, however many calls are in flight.
func TestAdmitWithoutCapacityAdmitsEveryone(t *testing.T) {
	c := &Container{}
	for range 1000 {
		if !c.admit() {
			t.Fatal("a call was turned away although Capacity is zero")
		}
	}
	if got := c.inflight.Load(); got != 1000 {
		t.Errorf("%d in flight, want 1000", got)
	}
}

// firstN is shared by every handler goroutine. Raced directly, it must say yes exactly n times: a
// read-then-write counter would say it more often, and so hand a test more failures than it asked
// for. The dangerous interleavings exist only while the count is still below n, so the race is run
// as many short rounds on a fresh counter, each released by a barrier, rather than one long one.
func TestFirstNSaysYesExactlyNTimesWhenRaced(t *testing.T) {
	const n, workers, rounds = 3, 8, 5000
	for round := range rounds {
		var seen, yes atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if firstN(&seen, n) {
					yes.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := yes.Load(); got != n {
			t.Fatalf("round %d: firstN said yes %d times, want exactly %d", round, got, n)
		}
	}

	var seen atomic.Int64
	if firstN(&seen, 0) {
		t.Error("firstN said yes for n = 0; the mode is off")
	}
}

func TestSyncCallsCountsEveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"result", Options{}},
		{"degraded to 202", Options{Sync: Sync202}},
		{"route absent", Options{Sync: Sync404}},
		{"unhandled endpoint", Options{Sync: Sync500Unhandled}},
		{"model not found", Options{Sync: SyncModelNotFound}},
		{"bare failed status", Options{FailSyncBare: true}},
		{"failed analysis", Options{FailAnalysis: true}},
		{"html error status", Options{SyncErrorStatus: http.StatusRequestEntityTooLarge}},
		{"busy", Options{BusyFirst: 1}},
		{"container error", Options{StatusFirst: 1, StatusFirstCode: http.StatusBadGateway}},
		{"connection reset", Options{ResetMidBody: true}},
		// Rejected before the container looks at the request, but the client still made the call.
		{"unauthorized", Options{RequireAPIKey: "secret"}},
	} {
		for _, rt := range syncRoutes {
			t.Run(tc.name+" on "+rt.name, func(t *testing.T) {
				c := rt.make(tc.opts)
				defer c.Close()

				if _, err := trySync(newPerAttemptClient(), c, rt.path); err != nil {
					t.Fatalf("POST: %v", err)
				}
				if got := c.SyncCalls(); got != 1 {
					t.Errorf("SyncCalls is %d, want 1", got)
				}
			})
		}
	}

	t.Run("hung", func(t *testing.T) {
		c := NewDI(Options{Sync: SyncHang})
		defer c.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL()+diSyncPath,
				strings.NewReader("%PDF-1.7 fake"))
			if err != nil {
				return
			}
			if resp, err := newPerAttemptClient().Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
		waitUntil(t, "the hung call to arrive", func() bool { return c.SyncCalls() == 1 })
	})
}

// The asynchronous routes are not synchronous calls: they are not counted, and none of the
// failure modes touches them.
func TestFailureModesLeaveTheAsyncRouteAlone(t *testing.T) {
	opts := Options{
		BusyFirst: 3, StatusFirst: 3, StatusFirstCode: http.StatusBadGateway,
		Capacity: 1, ResetMidBody: true,
	}
	for _, tc := range []struct {
		name   string
		make   func(Options) *Container
		submit string
	}{
		{"document intelligence", NewDI,
			"/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30"},
		{"computer vision read", NewRead, "/vision/v3.2/read/analyze"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.make(opts)
			defer c.Close()
			cl := newPerAttemptClient()

			resp, err := cl.Post(c.URL()+tc.submit, "application/pdf", strings.NewReader("%PDF-1.7 fake"))
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			_ = resp.Body.Close()
			loc := resp.Header.Get("Operation-Location")
			if resp.StatusCode != http.StatusAccepted || loc == "" {
				t.Fatalf("submit: status %d, Operation-Location %q; want 202 and a location",
					resp.StatusCode, loc)
			}
			poll, err := cl.Get(loc)
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
			defer func() { _ = poll.Body.Close() }()
			body, err := io.ReadAll(poll.Body)
			if err != nil || poll.StatusCode != http.StatusOK {
				t.Fatalf("poll: status %d, read error %v; want a complete 200", poll.StatusCode, err)
			}
			if !strings.Contains(string(body), `"succeeded"`) {
				t.Errorf("poll did not report success: %s", body)
			}

			if got := c.SyncCalls(); got != 0 {
				t.Errorf("SyncCalls is %d after only asynchronous calls, want 0", got)
			}
			if got := c.Hits(); got != 1 {
				t.Errorf("Hits is %d, want 1: it counts analyze calls of either kind", got)
			}
		})
	}
}

// The modes are checked in the order Options documents, and each hands its leftover calls to the
// next, ending in the behaviour that was there before them.
func TestFailureModesRunInPrecedenceOrder(t *testing.T) {
	c := NewDI(Options{
		BusyFirst: 1, StatusFirst: 1, StatusFirstCode: http.StatusBadGateway, Sync: Sync404,
	})
	defer c.Close()
	cl := newPerAttemptClient()

	wantHAProxyBusy(t, "call 1", callSync(t, cl, c, diSyncPath))

	second := callSync(t, cl, c, diSyncPath)
	var body containerErrorBody
	if second.status != http.StatusBadGateway || json.Unmarshal(second.body, &body) != nil ||
		body.Error.Code == "" {
		t.Errorf("call 2: status %d, body %q; want a 502 container error", second.status, second.body)
	}

	third := callSync(t, cl, c, diSyncPath)
	if third.status != http.StatusNotFound || len(third.body) != 0 {
		t.Errorf("call 3: status %d, body %q; want the Sync404 behaviour (bodyless 404)",
			third.status, third.body)
	}
	if got := c.SyncCalls(); got != 3 {
		t.Errorf("SyncCalls is %d, want 3", got)
	}
}

// Many callers at once must still get exactly n failures: the counters are shared by every
// handler goroutine.
func TestFirstNModesAreExactUnderConcurrency(t *testing.T) {
	const callers = 12
	for _, tc := range []struct {
		name    string
		opts    Options
		failing int
		status  int
	}{
		{"BusyFirst", Options{BusyFirst: 3}, 3, http.StatusServiceUnavailable},
		{"StatusFirst", Options{StatusFirst: 4, StatusFirstCode: http.StatusGatewayTimeout}, 4,
			http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewDI(tc.opts)
			defer c.Close()
			cl := newPerAttemptClient()

			statuses := make([]int, callers)
			errs := make([]error, callers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					r, err := trySync(cl, c, diSyncPath)
					statuses[i], errs[i] = r.status, err
				}()
			}
			close(start)
			wg.Wait()

			failed, served := 0, 0
			for i, status := range statuses {
				if errs[i] != nil {
					t.Fatalf("caller %d: %v", i, errs[i])
				}
				switch status {
				case tc.status:
					failed++
				case http.StatusOK:
					served++
				default:
					t.Errorf("caller %d: unexpected status %d", i, status)
				}
			}
			if failed != tc.failing || served != callers-tc.failing {
				t.Errorf("%d failed and %d were served, want %d and %d",
					failed, served, tc.failing, callers-tc.failing)
			}
			if got := c.SyncCalls(); got != callers {
				t.Errorf("SyncCalls is %d, want %d", got, callers)
			}
		})
	}
}

// StatusFirst without a usable status would panic inside a handler, where the client sees only a
// dropped connection. Failing at construction names the mistake.
func TestStatusFirstNeedsAnErrorStatus(t *testing.T) {
	for _, code := range []int{0, 99, 200, 302, 600} {
		for _, mk := range []func(Options) *Container{NewDI, NewRead} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("StatusFirstCode %d was accepted; it is not an error status", code)
					}
				}()
				mk(Options{StatusFirst: 1, StatusFirstCode: code}).Close()
			}()
		}
	}

	// With the mode off the code is never used, so it is not validated.
	NewDI(Options{StatusFirstCode: 200}).Close()
}
