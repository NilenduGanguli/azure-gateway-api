// Package sched shares a surface's Azure call slots fairly between jobs.
//
// Every call the gateway makes to Azure holds one slot for as long as it runs: a whole document,
// one part of a split document, a re-check, or a hedged duplicate. A plain semaphore serves
// callers in arrival order, and a 2,000-page document puts hundreds of parts in line at once, so
// a 2-page job arriving behind them would wait for all of them. Here waiting callers are grouped
// by job and a freed slot goes to the next job in round-robin order, first come first served
// within a job. A job running alone still gets every slot, because turns go only to jobs that are
// waiting.
//
// Hedges must not undo that. A hedge duplicates a call that is merely slow, so it may only use
// capacity that nobody is waiting for: TryAcquireIdle never takes a slot a waiting call could
// have had.
//
// Both rules rest on how a slot changes hands. A release chooses the next waiter and grants it the
// slot within one critical section, so a slot is never free while a call waits for one: a
// newcomer or a hedge cannot take a slot meant for a waiter, and a waiter that gives up finds
// itself either still queued or already granted, never in between.
package sched

import (
	"context"
	"slices"
	"sync"
)

// Scheduler hands out a fixed number of slots. Waiting callers are grouped by job; a freed slot
// goes to the next job, in round-robin order, that has a waiter (FIFO within a job).
//
// Create one with New.
type Scheduler struct {
	mu    sync.Mutex
	limit int
	inUse int

	// ring holds the jobs that have a caller waiting, in turn order: ring[0] is served next. A
	// job that is served and still has callers waiting goes to the back, and so does a job whose
	// first caller starts waiting. Every waiting job is therefore served once per round, however
	// many callers it has queued and however often other jobs come and go. (Putting newcomers
	// anywhere ahead of the back would let jobs that keep leaving and coming back starve a job
	// with a standing backlog.)
	ring []string
	// waiters holds each ring job's waiting callers in arrival order; waiting counts them all.
	waiters map[string][]*waiter
	waiting int
}

// waiter is one blocked Acquire. It is granted by a token in ch, which has room for the one token
// a waiter can ever receive, so a grant never blocks while mu is held.
type waiter struct {
	ch chan struct{}
}

// Stats is a snapshot of a Scheduler, taken at one instant. Waiting is non-zero only while every
// slot is in use.
type Stats struct {
	InUse   int // slots held, by Acquire and TryAcquireIdle callers alike
	Waiting int // Acquire calls blocked for want of a slot
	Limit   int // slots in all
}

// New returns a Scheduler with limit slots. A limit below 1 is treated as 1: a misconfigured
// limit should slow calls down to one at a time, not stop them.
func New(limit int) *Scheduler {
	if limit < 1 {
		limit = 1
	}
	return &Scheduler{limit: limit, waiters: map[string][]*waiter{}}
}

// Acquire blocks until a slot is granted for job or ctx is done. release is idempotent, and nil
// when err is non-nil, so check err before deferring it.
//
// A ctx that has already ended gets its error even when a slot is free, and so does one that
// ends just as its grant arrives: the call it would start is bound to be cancelled, and the slot
// is better spent on the next waiter.
func (s *Scheduler) Acquire(ctx context.Context, job string) (release func(), err error) {
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		return nil, ctx.Err()
	}
	// A call waits only while every slot is in use (see grantLocked), so a free slot can be taken
	// at once. Checking waiting as well states the rule outright: a newcomer never overtakes a
	// waiter.
	if s.inUse < s.limit && s.waiting == 0 {
		s.inUse++
		s.mu.Unlock()
		return s.releaseFunc(), nil
	}
	w := &waiter{ch: make(chan struct{}, 1)}
	if len(s.waiters[job]) == 0 {
		s.ring = append(s.ring, job)
	}
	s.waiters[job] = append(s.waiters[job], w)
	s.waiting++
	s.mu.Unlock()

	select {
	case <-w.ch:
		if ctx.Err() == nil {
			return s.releaseFunc(), nil
		}
		// The grant and the end of ctx arrived together. Hand the slot on.
		s.release()
		return nil, ctx.Err()
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		select {
		case <-w.ch:
			// Granted after ctx ended but before the lock was ours. The grant already took this
			// waiter out of the queue, so the slot is passed on here or it is lost for good.
			s.freeLocked()
		default:
			s.dequeueLocked(job, w)
		}
		return nil, ctx.Err()
	}
}

// TryAcquireIdle grants a slot only when one is free and nobody is waiting. Hedges use it: a
// hedge duplicates a call that is merely slow, so it may use spare capacity, but never a slot a
// waiting call could have had. It never blocks. release is nil when ok is false.
func (s *Scheduler) TryAcquireIdle() (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse >= s.limit || s.waiting > 0 {
		return nil, false
	}
	s.inUse++
	return s.releaseFunc(), true
}

// Stats reports the scheduler's state, all three numbers read at the same instant.
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{InUse: s.inUse, Waiting: s.waiting, Limit: s.limit}
}

// releaseFunc returns the release for one granted slot. It is idempotent, so a release that is
// both called early and deferred frees its slot once, and never a slot granted to another call.
func (s *Scheduler) releaseFunc() func() {
	var once sync.Once
	return func() { once.Do(s.release) }
}

// release frees one granted slot.
func (s *Scheduler) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freeLocked()
}

// freeLocked takes back one slot and grants it on to the next waiter, if any.
func (s *Scheduler) freeLocked() {
	// Each grant is freed exactly once, so inUse is positive here. The guard keeps a broken count
	// from going negative, which would let one call more than the limit through from then on.
	if s.inUse > 0 {
		s.inUse--
	}
	s.grantLocked()
}

// grantLocked hands free slots to waiting callers, one per job in turn, until it runs out of
// either. Every path that frees a slot ends here before mu is released, so outside the lock a call
// is waiting only while every slot is in use.
//
// A grant completes under mu: the waiter leaves its queue, its token is buffered and the slot is
// counted in one step. A waiter that gives up therefore finds itself either still queued or
// already granted, never in between.
func (s *Scheduler) grantLocked() {
	for s.inUse < s.limit && len(s.ring) > 0 {
		job := s.ring[0]
		q := s.waiters[job]
		w := q[0]
		q[0] = nil // let a finished waiter be collected
		q = q[1:]
		s.waiting--
		w.ch <- struct{}{}
		s.inUse++

		// Pass the turn on: the job leaves the ring if none of its callers is left waiting, and
		// otherwise goes to the back.
		if len(q) == 0 {
			delete(s.waiters, job)
			s.ring = slices.Delete(s.ring, 0, 1)
		} else {
			s.waiters[job] = q
			copy(s.ring, s.ring[1:])
			s.ring[len(s.ring)-1] = job
		}
	}
}

// dequeueLocked removes a waiter that gave up before it was granted. If it was its job's last
// waiter, the job leaves the ring. Otherwise the job keeps its place, and its next waiter
// inherits the turn.
func (s *Scheduler) dequeueLocked(job string, w *waiter) {
	q := s.waiters[job]
	i := slices.Index(q, w)
	if i < 0 {
		return // not reached: under mu, a waiter that has not been granted is still queued
	}
	s.waiting--
	if len(q) == 1 {
		delete(s.waiters, job)
		s.ring = slices.DeleteFunc(s.ring, func(j string) bool { return j == job })
		return
	}
	s.waiters[job] = slices.Delete(q, i, i+1)
}
