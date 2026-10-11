package engine

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// workdir is one job's work folder, ${DATA_DIR}/work/<jobId>. It holds the decoded base64Source,
// the part PDFs, the part results and the merged result, so everything a job leaves on the PVC
// is in one place: removing the folder removes all of it, and the jobs sweeper removes the
// folders of jobs that no longer run, which covers a crash.
type workdir struct {
	dir string
}

// newWorkdir creates the job's work folder, empty but for the job's input. Whatever an earlier run
// of the same job left there (a crash, then recovery) is removed first: its parts may be from a
// different plan.
//
// input is the job's stored document. A client's :syncAnalyze saves its body inside this very
// folder before the engine runs (splitting spec §1), so emptying the folder must keep that one
// file, or the document is deleted before it is read. An :analyze input lives in the blob store,
// outside the folder, and is never touched here. The file is matched by identity rather than by
// name, so a relative and an absolute DATA_DIR cannot disagree about it.
func newWorkdir(root, job, input string) (*workdir, error) {
	// The id becomes a path element, so anything that could leave the root is refused rather
	// than cleaned: job ids are minted by the gateway and never look like this.
	if job == "" || job == "." || job == ".." || strings.ContainsAny(job, `/\`) {
		return nil, fmt.Errorf("engine: job id %q cannot name a work folder", job)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("engine: create work root: %w", err)
	}
	dir := filepath.Join(root, job)
	err := os.Mkdir(dir, 0o750)
	if err == nil {
		return &workdir{dir: dir}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("engine: create work folder: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("engine: read stale work folder: %w", err)
	}
	keep, _ := os.Stat(input) // a missing input is the engine's to report, when it reads it
	for _, ent := range entries {
		if keep != nil {
			if info, err := ent.Info(); err == nil && os.SameFile(keep, info) {
				continue
			}
		}
		if err := os.RemoveAll(filepath.Join(dir, ent.Name())); err != nil {
			return nil, fmt.Errorf("engine: remove stale work file: %w", err)
		}
	}
	return &workdir{dir: dir}, nil
}

// path names a file in the folder.
func (w *workdir) path(name string) string { return filepath.Join(w.dir, name) }

// partName names a part's file by its original page range. Pages are disjoint between parts and
// a half is a strict prefix or suffix of its part, so first and last together are unique among
// every part a job ever cuts, and short whatever pages= selected.
func partName(prefix string, pages []int, ext string) string {
	return prefix + "-" + strconv.Itoa(pages[0]) + "-" + strconv.Itoa(pages[len(pages)-1]) + ext
}

// remove deletes the folder and everything in it.
func (w *workdir) remove() { _ = os.RemoveAll(w.dir) }

// writeFile writes path with fn, removing it again on any error: a cut or a merge that fails may
// have written part of its output, which must never be mistaken for a whole file.
func writeFile(path string, fn func(w io.Writer) error) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("engine: create %s: %w", filepath.Base(path), err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("engine: close %s: %w", filepath.Base(path), cerr)
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	bw := bufio.NewWriterSize(f, 64<<10)
	if err := fn(bw); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("engine: write %s: %w", filepath.Base(path), err)
	}
	return nil
}
