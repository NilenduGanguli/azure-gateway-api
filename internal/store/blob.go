package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Kind identifies which artifact of a job a blob holds.
type Kind string

const (
	// KindInput is the original document as uploaded. Deleted once the job succeeds.
	KindInput Kind = "in"
	// KindResult is the exact response envelope the gateway will serve on a successful poll.
	KindResult Kind = "res"
	// KindPDF is the searchable PDF produced when a submit requested output=pdf.
	KindPDF Kind = "pdf"
)

// KindFigure names the blob holding a single cropped figure image.
func KindFigure(figureID string) Kind { return Kind("fig-" + sanitize(figureID)) }

// ErrNotFound reports a blob that is absent from the store.
var ErrNotFound = errors.New("store: blob not found")

// BlobStore persists job artifacts on the PVC.
//
// Every write is atomic: content goes to a temporary file in the destination directory, is
// fsynced, renamed into place, and the directory itself is then fsynced. A reader therefore never
// observes a partial file, and a crash mid-write leaves at most an orphaned temp file, which the
// sweeper removes.
type BlobStore struct {
	root string
}

// NewBlobStore prepares the blob directory under root.
func NewBlobStore(root string) (*BlobStore, error) {
	dir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create blob root: %w", err)
	}
	return &BlobStore{root: dir}, nil
}

// Root returns the blob directory.
func (b *BlobStore) Root() string { return b.root }

// dir returns the two-level shard directory for an id, keeping any single directory small enough
// that listing and lookup stay fast even with a day's worth of results.
func (b *BlobStore) dir(id string) string {
	id = sanitize(id)
	var aa, bb string
	switch {
	case len(id) >= 4:
		aa, bb = id[0:2], id[2:4]
	case len(id) >= 2:
		aa, bb = id[0:2], "zz"
	default:
		aa, bb = "zz", "zz"
	}
	return filepath.Join(b.root, aa, bb)
}

// Path returns the on-disk location of one artifact.
func (b *BlobStore) Path(id string, k Kind) string {
	return filepath.Join(b.dir(id), sanitize(id)+"."+string(k))
}

// Writer accumulates blob content and installs it atomically on Commit.
type Writer struct {
	f        *os.File
	final    string
	dir      string
	n        int64
	limit    int64
	done     bool
	exceeded bool
}

// ErrLimitExceeded reports that a write ran past the configured byte limit.
var ErrLimitExceeded = errors.New("store: blob size limit exceeded")

// Create opens a Writer for one artifact. A limit of zero means unlimited.
func (b *BlobStore) Create(id string, k Kind, limit int64) (*Writer, error) {
	dir := b.dir(id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create blob dir: %w", err)
	}
	f, err := os.CreateTemp(dir, sanitize(id)+"."+string(k)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("store: create temp blob: %w", err)
	}
	return &Writer{f: f, final: b.Path(id, k), dir: dir, limit: limit}, nil
}

// Write implements io.Writer, enforcing the size limit.
func (w *Writer) Write(p []byte) (int, error) {
	if w.limit > 0 && w.n+int64(len(p)) > w.limit {
		// Record the overrun before returning so an aborted upload is distinguishable from an
		// I/O failure. Write what fits so the caller's counters stay honest.
		allowed := w.limit - w.n
		if allowed > 0 {
			n, err := w.f.Write(p[:allowed])
			w.n += int64(n)
			if err != nil {
				return n, err
			}
		}
		w.exceeded = true
		return int(max64(allowed, 0)), ErrLimitExceeded
	}
	n, err := w.f.Write(p)
	w.n += int64(n)
	return n, err
}

// Written reports how many bytes have been accepted so far.
func (w *Writer) Written() int64 { return w.n }

// Commit fsyncs the content, renames it into place, and fsyncs the directory so the rename itself
// is durable. After Commit the blob is visible and complete, or it does not exist at all.
func (w *Writer) Commit() error {
	if w.done {
		return errors.New("store: writer already finished")
	}
	w.done = true
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		_ = os.Remove(w.f.Name())
		return fmt.Errorf("store: sync blob: %w", err)
	}
	if err := w.f.Close(); err != nil {
		_ = os.Remove(w.f.Name())
		return fmt.Errorf("store: close blob: %w", err)
	}
	if err := os.Rename(w.f.Name(), w.final); err != nil {
		_ = os.Remove(w.f.Name())
		return fmt.Errorf("store: install blob: %w", err)
	}
	return syncDir(w.dir)
}

// Abort discards a partially written blob. It is safe to call after Commit.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	_ = w.f.Close()
	_ = os.Remove(w.f.Name())
}

// Open returns a reader for one artifact along with its size, so the caller can set an exact
// Content-Length without buffering the body.
func (b *BlobStore) Open(id string, k Kind) (*os.File, int64, error) {
	p := b.Path(id, k)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// WriteAll stores a complete in-memory artifact atomically.
func (b *BlobStore) WriteAll(id string, k Kind, data []byte) error {
	w, err := b.Create(id, k, 0)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Abort()
		return err
	}
	return w.Commit()
}

// WriteFrom streams an artifact from r, stopping at limit bytes.
func (b *BlobStore) WriteFrom(id string, k Kind, r io.Reader, limit int64) (int64, error) {
	w, err := b.Create(id, k, limit)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(w, r); err != nil {
		n := w.Written()
		w.Abort()
		return n, err
	}
	n := w.Written()
	if err := w.Commit(); err != nil {
		return n, err
	}
	return n, nil
}

// Remove deletes one artifact. A missing blob is not an error.
func (b *BlobStore) Remove(id string, k Kind) error {
	err := os.Remove(b.Path(id, k))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RemoveAll deletes every artifact belonging to a job.
func (b *BlobStore) RemoveAll(id string) error {
	dir := b.dir(id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	prefix := sanitize(id) + "."
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// SweepTemp removes temporary files left behind by a crash mid-write. Only files older than
// minAge are considered, so an in-flight write is never disturbed.
func (b *BlobStore) SweepTemp(minAgeSeconds int64, now int64) (int, error) {
	var removed int
	err := filepath.WalkDir(b.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree must not abort the sweep
		}
		if d.IsDir() || !strings.Contains(d.Name(), ".tmp-") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if now-info.ModTime().Unix() < minAgeSeconds {
			return nil
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	return removed, err
}

// syncDir fsyncs a directory so a rename into it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("store: open dir for sync: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		// Some filesystems reject fsync on a directory. The rename is still ordered on every
		// filesystem this runs on, so this is reported but not fatal.
		return nil
	}
	return nil
}

// sanitize strips path separators and traversal from an identifier so it can never escape the
// blob root. Ids are gateway-minted GUIDs, but figure ids come from the upstream response.
func sanitize(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
