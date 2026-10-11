package upstream

import (
	"math/rand/v2"
	"time"
)

// The backoff schedule of the sync-only spec, §1: the wait before attempt n+1 is a random 50% to
// 100% of min(maxBackoff, firstBackoff × 2^(n−1)).
const (
	firstBackoff = 2 * time.Second
	maxBackoff   = 60 * time.Second
)

// Backoff returns the wait after attempt number attempt (1 for the first) has failed.
//
// The jitter spreads the retries of jobs that failed together — a pod killed by scale-in fails
// every call it held at once — so they do not all reach HAProxy again at the same moment. jitter
// returns a value in [0, 1]; nil means a random one, and a value outside the range is clamped.
//
// A Retry-After the container or HAProxy sent replaces the computed wait unjittered, because it
// is the other side's own estimate of when it can take more work, and is capped at maxBackoff so a
// misconfigured hour-long value cannot stall a job.
func Backoff(attempt int, retryAfter time.Duration, jitter func() float64) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, maxBackoff)
	}
	// Doubled step by step rather than shifted, so a large attempt number cannot overflow.
	ceiling := firstBackoff
	for n := 1; n < attempt && ceiling < maxBackoff; n++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, maxBackoff)

	if jitter == nil {
		jitter = rand.Float64
	}
	j := jitter()
	switch {
	case !(j >= 0): // also catches NaN
		j = 0
	case j > 1:
		j = 1
	}
	return time.Duration(float64(ceiling) * (0.5 + 0.5*j))
}
