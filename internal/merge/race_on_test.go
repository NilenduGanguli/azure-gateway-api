//go:build race

package merge_test

// raceEnabled reports whether the race detector instruments this test binary. Its shadow memory
// and instrumentation roughly triple what an allocation count measures, so bounds on allocation
// are asserted only without it.
const raceEnabled = true
