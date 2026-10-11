package engine

import (
	"bufio"
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

// newWorkdir creates the job's work folder, empty. A folder left by an earlier run of the same
// job (a crash, then recovery) is removed first: its parts may be from a different plan.
func newWorkdir(root, job string) (*workdir, error) {
	// The id becomes a path element, so anything that could leave the root is refused rather
	// than cleaned: job ids are minted by the gateway and never look like this.
	if job == "" || job == "." || job == ".." || strings.ContainsAny(job, `/\`) {
		return nil, fmt.Errorf("engine: job id %q cannot name a work folder", job)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("engine: create work root: %w", err)
	}
	dir := filepath.Join(root, job)
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("engine: remove stale work folder: %w", err)
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		return nil, fmt.Errorf("engine: create work folder: %w", err)
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
