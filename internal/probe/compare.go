package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/engine"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/sched"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// The probe-local engine's limits. The probe analyses one document at a time, so the scheduler
// only bounds its parts in flight, and the splitter is the gateway's own memory bound.
const (
	compareSlots       = 4
	compareSplitters   = 2
	compareSplitBudget = 60 * time.Second
	// wholeAttempts is how often the whole analysis is tried through transient failures, as the
	// gateway would try it.
	wholeAttempts = engine.WholeAttempts
)

// ReportFile is the report --compare-split writes into the folder it compared.
const ReportFile = "compare-report.json"

// CompareReport is what --compare-split writes to <dir>/compare-report.json.
type CompareReport struct {
	SplitPages int              `json:"splitPages"`
	Files      []FileComparison `json:"files"`
	// Patterns gathers every file's differences.
	Patterns      []Pattern `json:"patterns"`
	PageLevel     int       `json:"pageLevelDifferences"`
	DocumentLevel int       `json:"documentLevelDifferences"`
	// Failed counts the files that could not be compared: a document the gate has not measured
	// does not pass it.
	Failed int `json:"failed"`
}

// Passed reports whether the corpus passes the gate's automatic half: no page-level difference
// and no document left unmeasured. Document-level patterns need a merge rule or the user's
// acceptance, which no program can give.
func (r *CompareReport) Passed() bool { return r.PageLevel == 0 && r.Failed == 0 }

// FileComparison is one document on one surface.
type FileComparison struct {
	File    string `json:"file"`
	Surface string `json:"surface"`
	// Parts is how many results the split analysis merged: 1 when it was sent whole.
	Parts int `json:"parts"`
	// Fallback is why the engine sent a document it could have split whole, or empty.
	Fallback      string    `json:"fallback,omitempty"`
	PageLevel     int       `json:"pageLevelDifferences"`
	DocumentLevel int       `json:"documentLevelDifferences"`
	Patterns      []Pattern `json:"patterns,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// CompareSplit analyses every PDF in dir whole and split on every configured surface, diffs the
// two results, writes <dir>/compare-report.json and prints a summary (splitting spec §6).
//
// The whole analysis is one Client.Do, as a client's :syncAnalyze would make it; the split one is
// the gateway's own engine (planner, splitter, part calls, merge) linked in, so what is measured is
// exactly what production would answer. The two run one after the other, never together, so they
// do not compete for the container.
func CompareSplit(ctx context.Context, cfg *config.Config, in Inputs, dir string, out io.Writer) (*CompareReport, error) {
	ts := targets(cfg)
	if len(ts) == 0 {
		return nil, errors.New("probe: no surface is configured: set DI_UPSTREAM_URL and/or READ_UPSTREAM_URL")
	}
	if cfg.SplitPages <= 0 {
		return nil, errors.New("probe: SPLIT_PAGES is 0, so nothing would be split; set the value the gateway will run with")
	}
	files, err := pdfFiles(dir)
	if err != nil {
		return nil, err
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	scratch, err := os.MkdirTemp("", "gateway-compare-*")
	if err != nil {
		return nil, fmt.Errorf("probe: create a scratch folder: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	rep := &CompareReport{SplitPages: cfg.SplitPages, Files: []FileComparison{}}
	var all []Difference
	splitter := pdfsplit.NewSplitter(compareSplitters, compareSplitBudget)
	for _, t := range ts {
		up, s := cfg.DI, azerr.SurfaceDI
		if !t.di {
			up, s = cfg.Read, azerr.SurfaceRead
		}
		client := upstream.NewClient(s, up, scratch, cfg.MaxResultBytes)
		eng := engine.New(engine.Options{
			Client:       client,
			Sched:        sched.New(compareSlots),
			Splitter:     splitter,
			SplitPages:   cfg.SplitPages,
			WorkRoot:     filepath.Join(scratch, "work"),
			WholeTimeout: timeout,
		})
		c := comparer{t: t, client: client, engine: eng, timeout: timeout}
		for i, f := range files {
			fc, diffs, err := c.compare(ctx, filepath.Join(dir, f), fmt.Sprintf("compare-%s-%d", t.name, i))
			if err != nil {
				return nil, err
			}
			fc.File = f
			for j := range diffs {
				diffs[j].File = t.name + "/" + f
			}
			all = append(all, diffs...)
			rep.Files = append(rep.Files, fc)
			rep.PageLevel += fc.PageLevel
			rep.DocumentLevel += fc.DocumentLevel
			if fc.Error != "" {
				rep.Failed++
			}
		}
	}
	rep.Patterns = Group(all)
	if rep.Patterns == nil {
		rep.Patterns = []Pattern{}
	}

	b, err := encodeIndented(rep)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ReportFile)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("probe: write %s: %w", path, err)
	}
	summarize(out, rep, ts, len(files), path)
	return rep, nil
}

// pdfFiles lists the PDFs in dir by name, sorted.
func pdfFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("probe: read %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("probe: no *.pdf file in %s", dir)
	}
	slices.Sort(files)
	return files, nil
}

// comparer compares documents on one surface.
type comparer struct {
	t       target
	client  *upstream.Client
	engine  *engine.Engine
	timeout time.Duration
}

// compare analyses one document whole and split and diffs the results. A document that cannot be
// analysed either way is reported in the comparison's Error; only the caller leaving is an error.
func (c comparer) compare(ctx context.Context, path, job string) (FileComparison, []Difference, error) {
	fc := FileComparison{Surface: c.t.name}
	info, err := os.Stat(path)
	if err != nil {
		fc.Error = "cannot read the file: " + err.Error()
		return fc, nil, nil
	}
	var query url.Values
	if c.t.di {
		query = url.Values{"api-version": {apiVersion}}
	}

	whole, err := c.whole(ctx, path, info.Size(), query)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fc, nil, cerr
		}
		fc.Error = "whole analysis failed: " + err.Error()
		return fc, nil, nil
	}
	defer whole.Cleanup()

	split, err := c.engine.Run(ctx, engine.Input{
		JobID: job, ContentType: "application/pdf", Path: path, Size: info.Size(),
		ModelID: modelID, Query: query,
	})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fc, nil, cerr
		}
		fc.Error = "split analysis failed: " + err.Error()
		return fc, nil, nil
	}
	defer split.Cleanup()
	fc.Parts, fc.Fallback = split.Parts, split.Fallback

	a, err := readRange(whole.Path, whole.Start, whole.End)
	if err != nil {
		return fc, nil, err
	}
	b, err := readRange(split.Path, split.Start, split.End)
	if err != nil {
		return fc, nil, err
	}
	diffs, err := Diff(a, b)
	if err != nil {
		fc.Error = err.Error()
		return fc, nil, nil
	}
	for _, d := range diffs {
		if d.Level == PageLevel {
			fc.PageLevel++
		} else {
			fc.DocumentLevel++
		}
	}
	fc.Patterns = Group(diffs)
	return fc, diffs, nil
}

// whole analyses the document as one call, retrying transient failures as the gateway would.
func (c comparer) whole(ctx context.Context, path string, size int64, query url.Values) (*upstream.Result, error) {
	call := upstream.Call{
		ModelID: modelID, Query: query, Timeout: c.timeout,
		Body: upstream.Document{ContentType: "application/pdf", Size: size,
			Open: func() (io.ReadCloser, error) { return os.Open(path) }},
	}
	for n := 1; ; n++ {
		a := c.client.Do(ctx, call)
		switch a.Outcome {
		case upstream.Success:
			return a.Result, nil
		case upstream.Cancelled:
			return nil, ctx.Err()
		case upstream.Retry, upstream.RetryOnce:
			if n < wholeAttempts {
				if err := sleep(ctx, upstream.Backoff(n, a.RetryAfter, rand.Float64)); err != nil {
					return nil, err
				}
				continue
			}
		}
		return nil, attemptError(a)
	}
}

// attemptError describes a failed attempt for the report. The cause can name internal addresses,
// which is fine in an operator's own report.
func attemptError(a upstream.Attempt) error {
	msg := a.Outcome.String()
	if a.Status != 0 {
		msg += fmt.Sprintf(", HTTP %d", a.Status)
	}
	if a.Err != nil {
		msg += ": " + a.Err.Message
	}
	if a.Cause != nil {
		msg += " (" + a.Cause.Error() + ")"
	}
	return errors.New(msg)
}

// readRange reads [start, end) of a file: an analyzeResult inside a saved answer.
func readRange(path string, start, end int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("probe: open result: %w", err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, end-start)
	if _, err := f.ReadAt(b, start); err != nil {
		return nil, fmt.Errorf("probe: read result: %w", err)
	}
	return b, nil
}

// summarize prints the report as text.
func summarize(w io.Writer, rep *CompareReport, ts []target, files int, path string) {
	r := &reporter{w: w}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.name
	}
	r.line("compare-split: %d PDF(s) on %s, SPLIT_PAGES=%d", files, strings.Join(names, ", "), rep.SplitPages)
	r.line("")
	for _, f := range rep.Files {
		switch {
		case f.Error != "":
			r.line("  %-5s %-40s FAILED: %s", f.Surface, f.File, f.Error)
		case f.Parts <= 1:
			why := "within SPLIT_PAGES"
			if f.Fallback != "" {
				why = "fallback: " + f.Fallback
			}
			r.line("  %-5s %-40s not split (%s); %d page-level, %d document-level", f.Surface, f.File, why,
				f.PageLevel, f.DocumentLevel)
		default:
			r.line("  %-5s %-40s %d parts; %d page-level, %d document-level", f.Surface, f.File, f.Parts,
				f.PageLevel, f.DocumentLevel)
		}
	}
	if len(rep.Patterns) > 0 {
		r.section("Differences by pattern")
		for _, p := range rep.Patterns {
			r.line("  %-15s %s: %d differences", p.Level, p.Pattern, p.Count)
			for _, e := range p.Examples {
				r.line("                    %s", e)
			}
		}
	}
	r.line("")
	switch {
	case rep.Failed > 0:
		r.line("FAIL: %d document(s) could not be compared; the gate needs every one measured.", rep.Failed)
	case rep.PageLevel > 0:
		r.line("FAIL: %d page-level difference(s). The merge must reproduce every page exactly.", rep.PageLevel)
	case rep.DocumentLevel > 0:
		r.line("PASS: no page-level difference. Each document-level pattern above needs a merge rule")
		r.line("      that reproduces the whole-document answer, or the user's explicit acceptance.")
	default:
		r.line("PASS: the split results equal the whole results.")
	}
	r.line("Report: %s", path)
}
