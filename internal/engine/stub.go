// stub.go is a whole-document stand-in for the engine; the integration replaces it with the full engine.

// Package engine analyses one document against a container: whole, or split into parts that are
// scheduled, retried and merged. This stand-in sends every document whole.
package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/sched"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// The splitting spec's §7 constants.
const (
	PartTimeout    = 60 * time.Second
	PartAttempts   = 3
	WholeAttempts  = 5
	JobDeadline    = 60 * time.Minute
	HedgeMin       = 5 * time.Second
	HedgeMax       = 30 * time.Second
	HedgeFactor    = 4
	HedgeSamples   = 200
	HedgeMinSample = 20
)

// Options configures an Engine.
type Options struct {
	Client       *upstream.Client
	Sched        *sched.Scheduler
	Splitter     *pdfsplit.Splitter
	SplitPages   int
	WorkRoot     string        // ${DATA_DIR}/work
	WholeTimeout time.Duration // per-attempt timeout for a document sent whole
	Metrics      *Metrics      // nil: New creates one
	Log          *slog.Logger
	Now          func() time.Time
	Jitter       func() float64 // nil: rand.Float64
}

// Engine runs documents through one surface's container.
type Engine struct {
	o Options
}

// New builds an Engine.
func New(o Options) *Engine {
	if o.Metrics == nil {
		o.Metrics = NewMetrics()
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Jitter == nil {
		o.Jitter = rand.Float64
	}
	return &Engine{o: o}
}

// Metrics returns the engine's counters.
func (e *Engine) Metrics() *Metrics { return e.o.Metrics }

// Sched returns the scheduler bounding the engine's upstream calls.
func (e *Engine) Sched() *sched.Scheduler { return e.o.Sched }

// Client returns the engine's upstream client.
func (e *Engine) Client() *upstream.Client { return e.o.Client }

// Input is one document to analyse.
type Input struct {
	JobID       string
	ContentType string
	Path        string // stored input on the PVC
	Size        int64
	ModelID     string     // DI
	Prefix      string     // DI family
	Query       url.Values // forwarded, policy-checked; may contain pages
}

// Output is the analyzeResult on disk.
type Output struct {
	Path       string
	Start, End int64 // the bare AnalyzeResult object inside Path
	Bare       bool
	Attempts   int
	Parts      int
	Halvings   int
	Hedges     int
	Fallback   string
	UpstreamMS int64

	cleanup func()
	once    sync.Once
}

// Cleanup removes the output's files. It is safe to call more than once, and on nil.
func (o *Output) Cleanup() {
	if o == nil {
		return
	}
	o.once.Do(func() {
		if o.cleanup != nil {
			o.cleanup()
		}
	})
}

// Run sends the document whole: up to WholeAttempts attempts, the RetryOnce class at most twice.
// Errors are *azerr.APIError, or ctx's error on shutdown.
func (e *Engine) Run(ctx context.Context, in Input) (*Output, error) {
	ctx, cancel := context.WithTimeout(ctx, JobDeadline)
	defer cancel()

	body := upstream.Document{
		ContentType: in.ContentType,
		Size:        in.Size,
		Open:        func() (io.ReadCloser, error) { return os.Open(in.Path) },
	}
	retryOnce := 0
	for n := 1; ; n++ {
		release, err := e.o.Sched.Acquire(ctx, in.JobID)
		if err != nil {
			return nil, err
		}
		a := e.o.Client.Do(ctx, upstream.Call{ModelID: in.ModelID, Prefix: in.Prefix,
			Query: in.Query, Body: body, Timeout: e.o.WholeTimeout})
		release()

		switch a.Outcome {
		case upstream.Success:
			res := a.Result
			e.o.Metrics.observeJob(1)
			return &Output{Path: res.Path, Start: res.Start, End: res.End, Bare: res.Bare,
				Attempts: n, Parts: 1, UpstreamMS: res.UpstreamMS, cleanup: res.Cleanup}, nil
		case upstream.Cancelled:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, context.Canceled
		case upstream.AnalysisFailed, upstream.Terminal, upstream.RouteMissing:
			return nil, a.Err
		case upstream.RetryOnce:
			retryOnce++
		}
		e.o.Metrics.retry(a.Reason)
		if n >= WholeAttempts || retryOnce >= 2 {
			e.o.Metrics.exhausted()
			return nil, afterAttempts(a.Err, n)
		}
		t := time.NewTimer(upstream.Backoff(n, a.RetryAfter, e.o.Jitter))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// afterAttempts is the last attempt's error, saying how many attempts were spent.
func afterAttempts(last *azerr.APIError, n int) *azerr.APIError {
	out := *last
	out.Message = fmt.Sprintf("%s (after %d attempts)", last.Message, n)
	return &out
}

// histogramBounds are the fixed upper bounds every engine histogram uses.
var histogramBounds = []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

// Metrics are an engine's counters. All methods are safe for concurrent use.
type Metrics struct {
	mu sync.Mutex
	s  MetricsSnapshot
}

// NewMetrics returns zeroed counters.
func NewMetrics() *Metrics {
	return &Metrics{s: MetricsSnapshot{
		PartSeconds:    newHistogram(),
		JobParts:       newHistogram(),
		MergeSeconds:   newHistogram(),
		Parts:          map[string]int64{},
		SplitFallbacks: map[string]int64{},
		Retries:        map[string]int64{},
	}}
}

// MetricsSnapshot is a copy of the counters at one instant.
type MetricsSnapshot struct {
	PartSeconds, JobParts, MergeSeconds Histogram
	Parts                               map[string]int64 // outcome: succeeded, retried, halved, hedged, hedge_won, rechecked
	SplitFallbacks                      map[string]int64 // reason: unparseable, encrypted, budget, panic, pages_param, merge_check
	Retries                             map[string]int64 // reason from upstream.Attempt.Reason
	Exhausted                           int64
}

// Histogram is a fixed-bucket histogram.
type Histogram struct {
	Bounds []float64 // upper bounds, ascending: 0.25,0.5,1,2,5,10,30,60,120,300
	Counts []int64   // CUMULATIVE count per bound (Prometheus le semantics), len == len(Bounds)
	Sum    float64
	Count  int64
}

func newHistogram() Histogram {
	return Histogram{Bounds: append([]float64(nil), histogramBounds...),
		Counts: make([]int64, len(histogramBounds))}
}

func (h *Histogram) observe(v float64) {
	for i, b := range h.Bounds {
		if v <= b {
			h.Counts[i]++
		}
	}
	h.Sum += v
	h.Count++
}

func (h Histogram) clone() Histogram {
	return Histogram{Bounds: append([]float64(nil), h.Bounds...),
		Counts: append([]int64(nil), h.Counts...), Sum: h.Sum, Count: h.Count}
}

// Snapshot copies the counters.
func (m *Metrics) Snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := func(src map[string]int64) map[string]int64 {
		out := make(map[string]int64, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	return MetricsSnapshot{
		PartSeconds:    m.s.PartSeconds.clone(),
		JobParts:       m.s.JobParts.clone(),
		MergeSeconds:   m.s.MergeSeconds.clone(),
		Parts:          cp(m.s.Parts),
		SplitFallbacks: cp(m.s.SplitFallbacks),
		Retries:        cp(m.s.Retries),
		Exhausted:      m.s.Exhausted,
	}
}

func (m *Metrics) observeJob(parts int) {
	m.mu.Lock()
	m.s.JobParts.observe(float64(parts))
	m.mu.Unlock()
}

func (m *Metrics) retry(reason string) {
	m.mu.Lock()
	m.s.Retries[reason]++
	m.mu.Unlock()
}

func (m *Metrics) exhausted() {
	m.mu.Lock()
	m.s.Exhausted++
	m.mu.Unlock()
}
