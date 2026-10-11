package sched

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// patience bounds every wait in these tests. A correct scheduler answers in microseconds, so this
// is generous on purpose, for -race on a loaded machine: reaching it means something hung.
const patience = 5 * time.Second

// waitFor polls cond until it holds, failing the test if it does not within patience.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for delay := 10 * time.Microsecond; !cond(); delay = min(2*delay, time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(delay)
	}
}

// recv takes one value from ch, failing the test if none arrives within patience.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(patience):
	}
	t.Fatalf("timed out waiting for %s", what)
	var zero T
	return zero
}

// outcome is what a background Acquire returned.
type outcome struct {
	release func()
	err     error
}

// mustAcquire takes a slot that is free now.
func mustAcquire(t *testing.T, s *Scheduler, job string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	release, err := s.Acquire(ctx, job)
	if err != nil {
		t.Fatalf("Acquire(%q) failed with %v, want a slot (Stats() = %+v)", job, err, s.Stats())
	}
	return release
}

// enqueue starts an Acquire that has to wait, and returns once it is queued, so a test controls
// the order of arrivals exactly. The Acquire's outcome arrives on the returned channel.
func enqueue(t *testing.T, ctx context.Context, s *Scheduler, job string) <-chan outcome {
	t.Helper()
	queued := s.Stats().Waiting + 1
	ch := make(chan outcome, 1)
	go func() {
		release, err := s.Acquire(ctx, job)
		ch <- outcome{release, err}
	}()
	waitFor(t, fmt.Sprintf("an Acquire for %q to queue", job), func() bool {
		return s.Stats().Waiting == queued
	})
	return ch
}

// line is a one-slot scheduler whose slot is taken, with named waiters queued behind it, so a
// test can free the slot one grant at a time and see exactly who is served. A waiter is named
// after its job and its place among that job's waiters: "a1", "a2", "b1".
type line struct {
	t        *testing.T
	s        *Scheduler
	holder   func() // releases the slot from whoever holds it now
	grants   chan grant
	refusals chan refusal
	cancels  map[string]context.CancelFunc
	joined   map[string]int
}

// grant is a waiter whose Acquire succeeded.
type grant struct {
	name    string
	release func()
}

// refusal is a waiter whose Acquire returned an error.
type refusal struct {
	name string
	err  error
}

func newLine(t *testing.T) *line {
	t.Helper()
	s := New(1)
	return &line{
		t:        t,
		s:        s,
		holder:   mustAcquire(t, s, "holder"),
		grants:   make(chan grant, 64),
		refusals: make(chan refusal, 64),
		cancels:  map[string]context.CancelFunc{},
		joined:   map[string]int{},
	}
}

// join queues one waiter for job and returns its name. It returns only once the waiter is
// queued, so the order of joins is the order of arrival.
func (l *line) join(job string) string {
	l.t.Helper()
	l.joined[job]++
	name := fmt.Sprintf("%s%d", job, l.joined[job])
	ctx, cancel := context.WithCancel(context.Background())
	l.t.Cleanup(cancel)
	l.cancels[name] = cancel
	queued := l.s.Stats().Waiting + 1
	go func() {
		release, err := l.s.Acquire(ctx, job)
		if err != nil {
			l.refusals <- refusal{name, err}
			return
		}
		l.grants <- grant{name, release}
	}()
	waitFor(l.t, name+" to queue", func() bool { return l.s.Stats().Waiting == queued })
	return name
}

// next frees the slot and reports which waiter it went to.
func (l *line) next() string {
	l.t.Helper()
	l.holder()
	g := recv(l.t, l.grants, "the freed slot to be granted")
	if st := l.s.Stats(); st.InUse != 1 {
		l.t.Fatalf("%s was granted the only slot, yet InUse = %d", g.name, st.InUse)
	}
	l.holder = g.release
	return g.name
}

// drain serves every remaining waiter, frees the slot, and returns who was served, in order.
func (l *line) drain() []string {
	l.t.Helper()
	var order []string
	for l.s.Stats().Waiting > 0 {
		order = append(order, l.next())
	}
	l.holder()
	if st := l.s.Stats(); st != (Stats{InUse: 0, Waiting: 0, Limit: 1}) {
		l.t.Fatalf("after draining, Stats() = %+v, want nothing in use or waiting", st)
	}
	return order
}

// giveUp cancels one waiter's context and waits for its Acquire to return.
func (l *line) giveUp(name string) {
	l.t.Helper()
	l.cancels[name]()
	r := recv(l.t, l.refusals, name+" to give up")
	if r.name != name || !errors.Is(r.err, context.Canceled) {
		l.t.Fatalf("%s gave up with %v, want %s to give up with context.Canceled",
			r.name, r.err, name)
	}
}

func TestLimit(t *testing.T) {
	s := New(2)
	first := mustAcquire(t, s, "a")
	second := mustAcquire(t, s, "b")
	if st := s.Stats(); st != (Stats{InUse: 2, Waiting: 0, Limit: 2}) {
		t.Fatalf("with both slots taken, Stats() = %+v", st)
	}

	third := enqueue(t, context.Background(), s, "c")
	select {
	case <-third:
		t.Fatal("a third Acquire was granted while both slots were in use")
	default:
	}

	first()
	got := recv(t, third, "the third Acquire")
	if got.err != nil {
		t.Fatalf("the third Acquire failed after a release: %v", got.err)
	}
	if st := s.Stats(); st != (Stats{InUse: 2, Waiting: 0, Limit: 2}) {
		t.Fatalf("after the freed slot was handed over, Stats() = %+v", st)
	}
	second()
	got.release()
	if st := s.Stats(); st != (Stats{InUse: 0, Waiting: 0, Limit: 2}) {
		t.Fatalf("after every release, Stats() = %+v", st)
	}
}

// TestRoundRobin pins the fairness rule: a freed slot goes to the next job in turn, so a small job
// is never queued behind every call of a large one.
func TestRoundRobin(t *testing.T) {
	tests := []struct {
		name     string
		arrivals []string // the job of each waiter, in arrival order
		want     []string // the order the waiters are served
	}{
		{
			name:     "a small job is served before a big job's remaining calls",
			arrivals: []string{"a", "a", "a", "b"},
			want:     []string{"a1", "b1", "a2", "a3"},
		},
		{
			name:     "three jobs take turns",
			arrivals: []string{"a", "a", "b", "b", "c"},
			want:     []string{"a1", "b1", "c1", "a2", "b2"},
		},
		{
			name:     "turns follow the order jobs started waiting",
			arrivals: []string{"b", "a", "b", "a"},
			want:     []string{"b1", "a1", "b2", "a2"},
		},
		{
			name:     "a job alone gets every turn",
			arrivals: []string{"a", "a", "a"},
			want:     []string{"a1", "a2", "a3"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newLine(t)
			for _, job := range tc.arrivals {
				l.join(job)
			}
			if got := l.drain(); !slices.Equal(got, tc.want) {
				t.Errorf("served %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLateJobWaitsOneTurnPerJob is the situation the fairness rule exists for: a small job that
// arrives while a big one is mid-flight with a long backlog waits one turn per job ahead of it,
// not for the backlog. Here big's turn has come round again by the time small arrives, so small
// waits for exactly one of big's calls.
func TestLateJobWaitsOneTurnPerJob(t *testing.T) {
	const backlog = 20
	l := newLine(t)
	for range backlog {
		l.join("big")
	}
	if got := l.next(); got != "big1" {
		t.Fatalf("served %s first, want big1", got)
	}
	l.join("small")

	want := []string{"big2", "small1"}
	for i := 3; i <= backlog; i++ {
		want = append(want, fmt.Sprintf("big%d", i))
	}
	if got := l.drain(); !slices.Equal(got, want) {
		t.Errorf("served %v, want %v", got, want)
	}
}

// TestBackloggedJobIsNotStarved: jobs that keep coming back one call at a time must not lock out a
// job with a standing backlog. A job that starts waiting joins the back of the round, so the
// backlogged job is still served once per round, however the others come and go.
func TestBackloggedJobIsNotStarved(t *testing.T) {
	l := newLine(t)
	for range 4 {
		l.join("a")
	}
	for _, job := range []string{"x", "y", "z"} {
		l.join(job)
	}

	var got []string
	for range 16 {
		name := l.next()
		got = append(got, name)
		// x, y and z ask again as soon as they are served, as a job whose calls run one at a
		// time does.
		if job := strings.TrimRight(name, "0123456789"); job != "a" {
			l.join(job)
		}
	}
	want := []string{
		"a1", "x1", "y1", "z1",
		"a2", "x2", "y2", "z2",
		"a3", "x3", "y3", "z3",
		"a4", "x4", "y4", "z4",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("served %v, want %v", got, want)
	}
	l.drain()
}

func TestFIFOWithinJob(t *testing.T) {
	l := newLine(t)
	var want []string
	for range 5 {
		want = append(want, l.join("a"))
	}
	if got := l.drain(); !slices.Equal(got, want) {
		t.Errorf("served %v, want arrival order %v", got, want)
	}
}

// TestCancelledWaiterLeavesQueue checks that a waiter that gives up is forgotten cleanly: the
// slot goes to the next waiter in turn, and nothing is left counted as waiting. The cases put the
// cancelled waiter at each position that matters, above all the one about to be served.
func TestCancelledWaiterLeavesQueue(t *testing.T) {
	tests := []struct {
		name     string
		arrivals []string // the job of each waiter, in arrival order
		served   int      // grants made before the cancellation
		cancel   string   // the waiter that gives up
		want     []string // the order the remaining waiters are served
	}{
		{
			name:     "next to be served, with more of its job behind it",
			arrivals: []string{"a", "a"},
			cancel:   "a1",
			want:     []string{"a2"},
		},
		{
			name:     "next to be served, its job keeping the turn for its next waiter",
			arrivals: []string{"a", "a", "b"},
			cancel:   "a1",
			want:     []string{"a2", "b1"},
		},
		{
			name:     "next to be served, its job's only waiter",
			arrivals: []string{"a", "b"},
			cancel:   "a1",
			want:     []string{"b1"},
		},
		{
			name:     "next to be served, partway through a round",
			arrivals: []string{"a", "a", "b", "c"},
			served:   1,
			cancel:   "b1",
			want:     []string{"c1", "a2"},
		},
		{
			name:     "in a job already served this round",
			arrivals: []string{"a", "a", "b", "c"},
			served:   1,
			cancel:   "a2",
			want:     []string{"b1", "c1"},
		},
		{
			name:     "behind the head of its job",
			arrivals: []string{"a", "a", "a"},
			cancel:   "a2",
			want:     []string{"a1", "a3"},
		},
		{
			name:     "the last job in the round",
			arrivals: []string{"a", "b"},
			cancel:   "b1",
			want:     []string{"a1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newLine(t)
			for _, job := range tc.arrivals {
				l.join(job)
			}
			for range tc.served {
				l.next()
			}
			before := l.s.Stats().Waiting
			l.giveUp(tc.cancel)
			if got := l.s.Stats().Waiting; got != before-1 {
				t.Fatalf("after %s gave up, Waiting = %d, want %d", tc.cancel, got, before-1)
			}
			if got := l.drain(); !slices.Equal(got, tc.want) {
				t.Errorf("after %s gave up, served %v, want %v", tc.cancel, got, tc.want)
			}
		})
	}
}

// TestCancelAfterGrantReturnsSlot races a grant against the waiter's cancellation. Whichever wins,
// the slot must reach the next waiter: a slot granted to a waiter that has already given up, and
// then dropped, would shrink the scheduler for good.
func TestCancelAfterGrantReturnsSlot(t *testing.T) {
	s := New(1)
	for i := range 1000 {
		holder := mustAcquire(t, s, "holder")
		ctx, cancel := context.WithCancel(context.Background())
		racer := enqueue(t, ctx, s, "racer")
		bystander := enqueue(t, context.Background(), s, "bystander")

		// Free the slot and cancel the racer in each order, and concurrently, so every
		// interleaving of the grant with the cancellation comes up.
		switch i % 3 {
		case 0:
			holder()
			cancel()
		case 1:
			cancel()
			holder()
		default:
			go holder()
			cancel()
		}

		got := recv(t, racer, "the racer's Acquire")
		switch {
		case got.err == nil:
			// The grant won outright: the racer holds the slot and the bystander still waits.
			if st := s.Stats(); st != (Stats{InUse: 1, Waiting: 1, Limit: 1}) {
				t.Fatalf("iteration %d: the racer holds the slot, yet Stats() = %+v", i, st)
			}
			got.release()
		case !errors.Is(got.err, context.Canceled):
			t.Fatalf("iteration %d: the racer failed with %v, want context.Canceled", i, got.err)
		}

		// Either way, the slot must reach the bystander.
		b := recv(t, bystander, "the bystander's Acquire")
		if b.err != nil {
			t.Fatalf("iteration %d: the bystander failed: %v", i, b.err)
		}
		b.release()
		if st := s.Stats(); st != (Stats{InUse: 0, Waiting: 0, Limit: 1}) {
			t.Fatalf("iteration %d: after every release, Stats() = %+v, want an idle scheduler",
				i, st)
		}
	}
}

// TestAcquireWithDoneContext: a caller whose context has already ended gets its error even with a
// slot free. Granting would only start a call that is bound to be cancelled.
func TestAcquireWithDoneContext(t *testing.T) {
	s := New(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Acquire(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire with a cancelled context returned %v, want context.Canceled", err)
	}
	if st := s.Stats(); st != (Stats{InUse: 0, Waiting: 0, Limit: 1}) {
		t.Fatalf("after the refused Acquire, Stats() = %+v, want an idle scheduler", st)
	}
}

// TestIdleOnlyWhenNoWaiters pins the hedging rule: a hedge may only use capacity nobody is waiting
// for, so duplicating a slow call can never delay another call.
func TestIdleOnlyWhenNoWaiters(t *testing.T) {
	tests := []struct {
		name    string
		held    int // slots taken, of 2
		waiting int // calls queued behind them
		want    bool
	}{
		{name: "nothing in use", held: 0, want: true},
		{name: "a slot free and nobody waiting", held: 1, want: true},
		{name: "every slot in use", held: 2, want: false},
		{name: "a call waiting", held: 2, waiting: 1, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(2)
			for range tc.held {
				t.Cleanup(mustAcquire(t, s, "busy"))
			}
			for range tc.waiting {
				enqueue(t, t.Context(), s, "queued")
			}
			release, ok := s.TryAcquireIdle()
			if ok != tc.want {
				t.Fatalf("TryAcquireIdle() ok = %v, want %v", ok, tc.want)
			}
			if !ok {
				return
			}
			if got := s.Stats().InUse; got != tc.held+1 {
				t.Errorf("with the hedge holding a slot, InUse = %d, want %d", got, tc.held+1)
			}
			release()
			if got := s.Stats().InUse; got != tc.held {
				t.Errorf("after the hedge's release, InUse = %d, want %d", got, tc.held)
			}
		})
	}

	t.Run("a freed slot goes to the waiter, never to a hedge", func(t *testing.T) {
		s := New(1)
		holder := mustAcquire(t, s, "a")
		waiter := enqueue(t, context.Background(), s, "b")

		// Hedge continuously across the moment the slot frees. The slot must pass straight to
		// the waiter, with no instant at which it looks idle.
		var tries, stolen atomic.Int64
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					tries.Add(1)
					if release, ok := s.TryAcquireIdle(); ok {
						stolen.Add(1)
						release()
					}
				}
			})
		}
		var once sync.Once
		stopHedging := func() {
			once.Do(func() { close(stop) })
			wg.Wait()
		}
		t.Cleanup(stopHedging)
		waitFor(t, "the hedges to start", func() bool { return tries.Load() >= 1000 })

		holder()
		got := recv(t, waiter, "the waiter's Acquire")
		stopHedging()
		if n := stolen.Load(); n != 0 {
			t.Fatalf("hedges took the slot %d times while a call was waiting for it", n)
		}
		if got.err != nil {
			t.Fatalf("the waiter failed: %v", got.err)
		}
		got.release()

		// With nobody waiting, the free slot is a hedge's to take.
		release, ok := s.TryAcquireIdle()
		if !ok {
			t.Fatal("TryAcquireIdle failed with the slot free and nobody waiting")
		}
		release()
	})
}

// TestReleaseIdempotent guards the deferred-release pattern: a release that runs twice, say on an
// early-return path and again in a defer, must not free a slot that is no longer its own.
func TestReleaseIdempotent(t *testing.T) {
	tests := []struct {
		name string
		take func(t *testing.T, s *Scheduler) (release func())
	}{
		{"granted at once", func(t *testing.T, s *Scheduler) func() {
			return mustAcquire(t, s, "a")
		}},
		{"granted after waiting", func(t *testing.T, s *Scheduler) func() {
			h1, h2 := mustAcquire(t, s, "h"), mustAcquire(t, s, "h")
			queued := enqueue(t, context.Background(), s, "a")
			h1()
			got := recv(t, queued, "the queued Acquire")
			if got.err != nil {
				t.Fatalf("the queued Acquire failed: %v", got.err)
			}
			h2()
			return got.release
		}},
		{"a hedge", func(t *testing.T, s *Scheduler) func() {
			release, ok := s.TryAcquireIdle()
			if !ok {
				t.Fatal("TryAcquireIdle failed on an idle scheduler")
			}
			return release
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(2)
			release := tc.take(t, s)
			if got := s.Stats().InUse; got != 1 {
				t.Fatalf("holding one slot, InUse = %d", got)
			}

			// Twice at once, then once more.
			var wg sync.WaitGroup
			wg.Go(release)
			wg.Go(release)
			wg.Wait()
			release()
			if got := s.Stats().InUse; got != 0 {
				t.Fatalf("after releasing one slot three times, InUse = %d, want 0", got)
			}

			// The limit still holds, and the stale release cannot free a slot that now belongs to
			// another call.
			a, b := mustAcquire(t, s, "a"), mustAcquire(t, s, "b")
			release()
			third := enqueue(t, context.Background(), s, "c")
			if st := s.Stats(); st != (Stats{InUse: 2, Waiting: 1, Limit: 2}) {
				t.Fatalf("with both slots retaken, Stats() = %+v", st)
			}
			a()
			got := recv(t, third, "the third Acquire")
			if got.err != nil {
				t.Fatalf("the third Acquire failed: %v", got.err)
			}
			b()
			got.release()
			if st := s.Stats(); st != (Stats{InUse: 0, Waiting: 0, Limit: 2}) {
				t.Fatalf("after every release, Stats() = %+v, want an idle scheduler", st)
			}
		})
	}
}

// TestStress runs many jobs' calls against one scheduler at once, with hedges and give-ups mixed
// in. Every holder counts itself in after Acquire and out before release, so an over-grant shows
// directly, and Stats is sampled throughout to check its numbers never disagree. At the end the
// scheduler must be idle: no slot leaked, no waiter stranded.
func TestStress(t *testing.T) {
	const (
		limit      = 4
		goroutines = 50
		acquires   = 20
		jobs       = 5
	)
	s := New(limit)
	var holding, peak atomic.Int64
	enter := func() {
		n := holding.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				return
			}
		}
	}
	leave := func() { holding.Add(-1) }

	// A call may wait only while every slot is in use; a snapshot showing a free slot and a
	// waiting call at once means a release and its hand-over came apart.
	var bad atomic.Pointer[Stats]
	sampling, stopSampling := context.WithCancel(t.Context())
	defer stopSampling()
	var sampler sync.WaitGroup
	sampler.Go(func() {
		for sampling.Err() == nil {
			st := s.Stats()
			if st.InUse < 0 || st.InUse > st.Limit || st.Waiting < 0 ||
				(st.Waiting > 0 && st.InUse < st.Limit) {
				bad.CompareAndSwap(nil, &st)
			}
			// Yield, or on a single CPU the sampler spends whole time slices the run needs.
			runtime.Gosched()
		}
	})

	unexpected := make(chan error, goroutines*acquires)
	var wg sync.WaitGroup
	for g := range goroutines {
		job := fmt.Sprintf("job-%d", g%jobs)
		wg.Go(func() {
			for i := range acquires {
				ctx, cancel := context.Background(), context.CancelFunc(func() {})
				if i%4 == 3 {
					// Give up after a moment, often just as the grant arrives.
					ctx, cancel = context.WithTimeout(ctx, rand.N(time.Millisecond))
				}
				release, err := s.Acquire(ctx, job)
				cancel()
				if err != nil {
					if !errors.Is(err, context.DeadlineExceeded) {
						unexpected <- err
					}
					continue
				}
				enter()
				var hedge func()
				if i%5 == 0 {
					if r, ok := s.TryAcquireIdle(); ok {
						enter()
						hedge = r
					}
				}
				time.Sleep(rand.N(time.Millisecond))
				if hedge != nil {
					leave()
					hedge()
				}
				leave()
				release()
			}
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the run did not finish; Stats() = %+v", s.Stats())
	}
	stopSampling()
	sampler.Wait()
	if st := bad.Load(); st != nil {
		t.Errorf("Stats() returned %+v during the run, whose numbers contradict each other", *st)
	}
	close(unexpected)
	for err := range unexpected {
		t.Errorf("Acquire failed unexpectedly: %v", err)
	}
	if p := peak.Load(); p > limit {
		t.Errorf("%d calls held slots at once; the limit is %d", p, limit)
	}
	if st := s.Stats(); st != (Stats{InUse: 0, Waiting: 0, Limit: limit}) {
		t.Errorf("after the run, Stats() = %+v, want an idle scheduler", st)
	}
	t.Logf("peak concurrency %d of %d", peak.Load(), limit)
}

func TestLimitBelowOne(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("New(%d)", limit), func(t *testing.T) {
			s := New(limit)
			if st := s.Stats(); st != (Stats{InUse: 0, Waiting: 0, Limit: 1}) {
				t.Fatalf("Stats() = %+v, want an idle scheduler with one slot", st)
			}
			first := mustAcquire(t, s, "a")
			second := enqueue(t, context.Background(), s, "b")
			first()
			got := recv(t, second, "the second Acquire")
			if got.err != nil {
				t.Fatalf("the second Acquire failed: %v", got.err)
			}
			got.release()
			if st := s.Stats(); st != (Stats{InUse: 0, Waiting: 0, Limit: 1}) {
				t.Fatalf("after every release, Stats() = %+v, want an idle scheduler", st)
			}
		})
	}
}

// BenchmarkAcquireRelease measures the scheduler's own cost per call: a grant and release with a
// slot free, and with more callers than slots, where every grant is a hand-over to a waiter.
func BenchmarkAcquireRelease(b *testing.B) {
	ctx := context.Background()
	b.Run("idle", func(b *testing.B) {
		s := New(16)
		b.ReportAllocs()
		for b.Loop() {
			release, err := s.Acquire(ctx, "job")
			if err != nil {
				b.Fatal(err)
			}
			release()
		}
	})
	b.Run("contended", func(b *testing.B) {
		s := New(2)
		var callers atomic.Int64
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			job := fmt.Sprintf("job-%d", callers.Add(1)%4)
			for pb.Next() {
				release, err := s.Acquire(ctx, job)
				if err != nil {
					b.Error(err)
					return
				}
				release()
			}
		})
	})
}
