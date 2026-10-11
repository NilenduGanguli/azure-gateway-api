package engine

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// ring holds the last HedgeSamples successful part durations of a surface.
type ring struct {
	mu   sync.Mutex
	buf  [HedgeSamples]time.Duration
	n    int // samples held, up to HedgeSamples
	next int // where the next sample goes
}

func (r *ring) add(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = d
	r.next = (r.next + 1) % HedgeSamples
	r.n = min(r.n+1, HedgeSamples)
}

// delay is how long an attempt runs before it is hedged: clamp(HedgeFactor × median, HedgeMin,
// HedgeMax), or HedgeMax until there are HedgeMinSample samples. A median of a few samples could
// be one lucky part, and hedging every part of a busy surface would double its load.
func (r *ring) delay() time.Duration {
	r.mu.Lock()
	if r.n < HedgeMinSample {
		r.mu.Unlock()
		return HedgeMax
	}
	s := slices.Clone(r.buf[:r.n])
	r.mu.Unlock()
	slices.Sort(s)
	median := s[len(s)/2]
	if len(s)%2 == 0 {
		median = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return min(max(HedgeFactor*median, HedgeMin), HedgeMax)
}

// hedged is one part attempt (splitting spec §4, "Hedge"). The caller holds a slot for the first
// call, which this gives back when that call ends.
//
// If the call is still running after the hedge delay and a slot is idle, one duplicate starts.
// The first success wins and the other call is cancelled and waited for, so its file is deleted
// and nothing outlives the attempt. If one call fails while the other runs, the attempt waits for
// the other: a hedge belongs to its attempt, which fails only when both calls do, with the last
// failure.
func (f *fanout) hedged(ctx context.Context, pages []int, call upstream.Call, release func()) upstream.Attempt {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()

	type answer struct {
		a     upstream.Attempt
		hedge bool
	}
	answers := make(chan answer, 2)
	start := func(hedge bool, release func()) {
		go func() {
			a := f.e.o.Client.Do(actx, call)
			release()
			answers <- answer{a, hedge}
		}()
	}
	start(false, release)
	running := 1

	timer := time.NewTimer(f.e.hedgeDelay(pages))
	defer timer.Stop()
	hedgeAt := timer.C

	var last upstream.Attempt
	for {
		select {
		case <-hedgeAt:
			hedgeAt = nil
			if rel, ok := f.e.o.Sched.TryAcquireIdle(); ok {
				f.t.hedges.Add(1)
				f.e.metrics.part(outcomeHedged)
				f.log.Info("engine: hedging a slow part", "pages", pdfsplit.FormatPages(pages))
				start(true, rel)
				running++
			}
		case ans := <-answers:
			running--
			if ans.a.Outcome == upstream.Success {
				cancel()
				for ; running > 0; running-- {
					(<-answers).a.Result.Cleanup()
				}
				if ans.hedge {
					f.e.metrics.part(outcomeHedgeWon)
				}
				f.e.samples.add(ans.a.Took)
				f.e.metrics.partSeconds(ans.a.Took)
				return ans.a
			}
			last = ans.a
			if running == 0 {
				return last
			}
		}
	}
}
