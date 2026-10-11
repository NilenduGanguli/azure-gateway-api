package engine

import (
	"maps"
	"slices"
	"sync"
	"time"
)

// histogramBounds are the upper bounds of every engine histogram (splitting spec §8). One set
// serves seconds and part counts alike: a part takes about 2 s and a whole document up to
// minutes, and a job is 1 to a few hundred parts.
var histogramBounds = []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

// The label values the admin renders even before they are first counted, so a dashboard sees a
// zero series rather than a missing one.
var (
	partOutcomes    = []string{outcomeSucceeded, outcomeRetried, outcomeHalved, outcomeHedged, outcomeHedgeWon, outcomeRechecked}
	fallbackReasons = []string{fallbackUnparseable, fallbackEncrypted, fallbackBudget, fallbackPanic, fallbackPagesParam, fallbackMergeCheck}
)

// Part outcomes, gateway_parts_total{outcome}.
const (
	outcomeSucceeded = "succeeded"
	outcomeRetried   = "retried"
	outcomeHalved    = "halved"
	outcomeHedged    = "hedged"
	outcomeHedgeWon  = "hedge_won"
	outcomeRechecked = "rechecked"
)

// Split fallback reasons, gateway_split_fallback_total{reason}.
const (
	fallbackUnparseable = "unparseable"
	fallbackEncrypted   = "encrypted"
	fallbackBudget      = "budget"
	fallbackPanic       = "panic"
	fallbackPagesParam  = "pages_param"
	fallbackMergeCheck  = "merge_check"
)

// Metrics counts what the engine does, for the admin's /_gw/metrics. It is safe for concurrent
// use. One engine serves one surface, so the admin labels a snapshot with that surface.
type Metrics struct {
	mu             sync.Mutex
	partSecs       histogram
	jobParts       histogram
	mergeSecs      histogram
	parts          map[string]int64
	splitFallbacks map[string]int64
	retries        map[string]int64
	exhausted      int64
}

// NewMetrics returns empty metrics.
func NewMetrics() *Metrics {
	m := &Metrics{
		parts:          map[string]int64{},
		splitFallbacks: map[string]int64{},
		retries:        map[string]int64{},
	}
	for _, h := range []*histogram{&m.partSecs, &m.jobParts, &m.mergeSecs} {
		h.counts = make([]int64, len(histogramBounds))
	}
	for _, k := range partOutcomes {
		m.parts[k] = 0
	}
	for _, k := range fallbackReasons {
		m.splitFallbacks[k] = 0
	}
	return m
}

// MetricsSnapshot is the metrics at one instant.
type MetricsSnapshot struct {
	PartSeconds, JobParts, MergeSeconds Histogram
	// Parts counts part events by outcome: succeeded, retried, halved, hedged, hedge_won,
	// rechecked.
	Parts map[string]int64
	// SplitFallbacks counts documents sent whole although splitting was on, by reason:
	// unparseable, encrypted, budget, panic, pages_param, merge_check.
	SplitFallbacks map[string]int64
	// Retries counts attempts that were retried, by upstream.Attempt.Reason.
	Retries map[string]int64
	// Exhausted counts jobs that failed because their attempts ran out.
	Exhausted int64
}

// Histogram is a fixed-bucket histogram with Prometheus "le" semantics.
type Histogram struct {
	Bounds []float64 // upper bounds, ascending
	Counts []int64   // cumulative: observations <= Bounds[i]
	Sum    float64
	Count  int64
}

// histogram keeps per-bucket counts; Snapshot makes them cumulative.
type histogram struct {
	counts []int64 // observations in (Bounds[i-1], Bounds[i]]
	sum    float64
	count  int64
}

func (h *histogram) observe(v float64) {
	h.sum += v
	h.count++
	if i, _ := slices.BinarySearch(histogramBounds, v); i < len(histogramBounds) {
		h.counts[i]++
	}
}

func (h *histogram) snapshot() Histogram {
	out := Histogram{
		Bounds: slices.Clone(histogramBounds),
		Counts: make([]int64, len(h.counts)),
		Sum:    h.sum,
		Count:  h.count,
	}
	var run int64
	for i, c := range h.counts {
		run += c
		out.Counts[i] = run
	}
	return out
}

// Snapshot copies the metrics; the caller may keep and change it.
func (m *Metrics) Snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MetricsSnapshot{
		PartSeconds:    m.partSecs.snapshot(),
		JobParts:       m.jobParts.snapshot(),
		MergeSeconds:   m.mergeSecs.snapshot(),
		Parts:          maps.Clone(m.parts),
		SplitFallbacks: maps.Clone(m.splitFallbacks),
		Retries:        maps.Clone(m.retries),
		Exhausted:      m.exhausted,
	}
}

func (m *Metrics) partSeconds(d time.Duration) {
	m.mu.Lock()
	m.partSecs.observe(d.Seconds())
	m.mu.Unlock()
}

func (m *Metrics) jobPartCount(n int) {
	m.mu.Lock()
	m.jobParts.observe(float64(n))
	m.mu.Unlock()
}

func (m *Metrics) mergeSeconds(d time.Duration) {
	m.mu.Lock()
	m.mergeSecs.observe(d.Seconds())
	m.mu.Unlock()
}

func (m *Metrics) part(outcome string) {
	m.mu.Lock()
	m.parts[outcome]++
	m.mu.Unlock()
}

func (m *Metrics) fallback(reason string) {
	m.mu.Lock()
	m.splitFallbacks[reason]++
	m.mu.Unlock()
}

func (m *Metrics) retry(reason string) {
	m.mu.Lock()
	m.retries[reason]++
	m.mu.Unlock()
}

func (m *Metrics) exhaust() {
	m.mu.Lock()
	m.exhausted++
	m.mu.Unlock()
}
