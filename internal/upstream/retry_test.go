package upstream

import (
	"testing"
	"time"
)

// Plan test 11: the wait before the next attempt is a random 50–100% of min(60s, 2s × 2^(n−1)),
// and a Retry-After replaces it, capped at 60s.
func TestBackoff(t *testing.T) {
	low := func() float64 { return 0 }
	high := func() float64 { return 1 }
	mid := func() float64 { return 0.5 }

	for _, tc := range []struct {
		name       string
		attempt    int
		retryAfter time.Duration
		jitter     func() float64
		want       time.Duration
	}{
		{"attempt 1, low jitter", 1, 0, low, 1 * time.Second},
		{"attempt 1, high jitter", 1, 0, high, 2 * time.Second},
		{"attempt 2, low jitter", 2, 0, low, 2 * time.Second},
		{"attempt 2, high jitter", 2, 0, high, 4 * time.Second},
		{"attempt 3, low jitter", 3, 0, low, 4 * time.Second},
		{"attempt 3, high jitter", 3, 0, high, 8 * time.Second},
		{"attempt 4, low jitter", 4, 0, low, 8 * time.Second},
		{"attempt 4, high jitter", 4, 0, high, 16 * time.Second},
		{"attempt 5, low jitter", 5, 0, low, 16 * time.Second},
		{"attempt 5, high jitter", 5, 0, high, 32 * time.Second},
		{"attempt 6 reaches the cap, low jitter", 6, 0, low, 30 * time.Second},
		{"attempt 6 reaches the cap, high jitter", 6, 0, high, 60 * time.Second},
		{"attempt 7 stays at the cap, low jitter", 7, 0, low, 30 * time.Second},
		{"attempt 7 stays at the cap, high jitter", 7, 0, high, 60 * time.Second},
		{"mid jitter", 3, 0, mid, 6 * time.Second},
		{"a large attempt does not overflow", 70, 0, high, 60 * time.Second},
		{"attempt 0 is treated as the first", 0, 0, high, 2 * time.Second},
		{"jitter above 1 is clamped", 1, 0, func() float64 { return 7 }, 2 * time.Second},
		{"jitter below 0 is clamped", 1, 0, func() float64 { return -3 }, 1 * time.Second},
		{"Retry-After replaces the computed wait", 5, 3 * time.Second, high, 3 * time.Second},
		{"Retry-After is not jittered", 1, 3 * time.Second, low, 3 * time.Second},
		{"Retry-After is capped", 1, 120 * time.Second, low, 60 * time.Second},
		{"Retry-After at the cap", 2, 60 * time.Second, low, 60 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Backoff(tc.attempt, tc.retryAfter, tc.jitter); got != tc.want {
				t.Errorf("Backoff(%d, %v) = %v, want %v", tc.attempt, tc.retryAfter, got, tc.want)
			}
		})
	}
}

// A nil jitter source falls back to a random one rather than panicking, and stays in bounds.
func TestBackoffDefaultJitter(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := Backoff(2, 0, nil)
		if got < 2*time.Second || got > 4*time.Second {
			t.Fatalf("Backoff(2) with the default jitter = %v, want within [2s, 4s]", got)
		}
	}
}
