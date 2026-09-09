// Package store persists gateway jobs and their artifacts on the PVC.
//
// The durability contract is set by the client-facing 202: once the gateway has told a client
// "your operation id is X", that id must resolve after any crash short of losing the volume. So
// the submit path is ordered strictly as
//
//	stream body to blob -> fsync -> INSERT job -> COMMIT -> then, and only then, 202
//
// and the writer runs with synchronous=FULL. SQLite's own docs are explicit that WAL with
// synchronous=NORMAL "might roll back following a power loss or system crash", which would hand a
// client an operation id that no longer exists.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the binary static and cgo-free
)

// Status is the client-visible lifecycle state of a job. The four values are exactly the strings
// both Azure surfaces emit; nothing else may reach a client.
type Status string

const (
	StatusNotStarted Status = "notStarted"
	StatusRunning    Status = "running"
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
)

// UpstreamMode records which strategy actually serviced a job, for diagnostics and for the
// admin surface. It is how an operator sees whether :syncAnalyze is working on their image.
type UpstreamMode string

const (
	// ModeSync means the container's synchronous analyze route returned the result directly.
	ModeSync UpstreamMode = "sync"
	// ModeDegraded means the sync route answered 202 and the gateway had to poll with affinity.
	ModeDegraded UpstreamMode = "degraded-202"
	// ModeAsyncFallback means the sync route was absent and the gateway used :analyze + polling.
	ModeAsyncFallback UpstreamMode = "async-fallback"
)

// Job is one client-visible operation.
type Job struct {
	ID          string
	Surface     string // "di" | "read"
	ModelID     string
	APIVersion  string
	Status      Status
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
	Query       string // canonical upstream query string
	ContentType string
	InputPath   string
	InputBytes  int64
	ResultPath  string
	ResultBytes int64
	ErrorJSON   string
	Attempts    int
	LeaseOwner  string
	LeaseUntil  time.Time
	Mode        UpstreamMode
	UpstreamOp  string
	UpStatus    int
	UpstreamMS  int64
	ClientReqID string
	// Prefix is the path family the client used, so the upstream call and any later result-file
	// fetch use the same one.
	Prefix string
	// HasPDF and Figures record which extra artifacts were persisted alongside the result.
	HasPDF  bool
	Figures string // comma-separated figure ids
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id                TEXT PRIMARY KEY,
  surface           TEXT NOT NULL,
  model_id          TEXT NOT NULL DEFAULT '',
  api_version       TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  expires_at        INTEGER NOT NULL,
  request_query     TEXT NOT NULL DEFAULT '',
  content_type      TEXT NOT NULL DEFAULT '',
  input_path        TEXT NOT NULL DEFAULT '',
  input_bytes       INTEGER NOT NULL DEFAULT 0,
  result_path       TEXT NOT NULL DEFAULT '',
  result_bytes      INTEGER NOT NULL DEFAULT 0,
  error_json        TEXT NOT NULL DEFAULT '',
  attempts          INTEGER NOT NULL DEFAULT 0,
  lease_owner       TEXT NOT NULL DEFAULT '',
  lease_until       INTEGER NOT NULL DEFAULT 0,
  upstream_mode     TEXT NOT NULL DEFAULT '',
  upstream_op_url   TEXT NOT NULL DEFAULT '',
  upstream_status   INTEGER NOT NULL DEFAULT 0,
  upstream_ms       INTEGER NOT NULL DEFAULT 0,
  client_request_id TEXT NOT NULL DEFAULT '',
  has_pdf           INTEGER NOT NULL DEFAULT 0,
  figures           TEXT NOT NULL DEFAULT '',
  prefix            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_status  ON jobs(status, created_at);
CREATE INDEX IF NOT EXISTS jobs_expiry  ON jobs(expires_at);
CREATE INDEX IF NOT EXISTS jobs_surface ON jobs(surface, status);
`

// Store owns the job database and the blob store.
//
// Writes go through a single connection. SQLite permits one writer at a time regardless, so
// serialising in the pool converts lock contention into an orderly queue and makes
// synchronous=FULL's cost predictable.
type Store struct {
	w    *sql.DB
	r    *sql.DB
	Blob *BlobStore
	dir  string
}

// Options configures Open.
type Options struct {
	DataDir        string
	AllowNetworkFS bool
	// ReadPoolSize bounds concurrent readers. Polls are the hot path and are read-only.
	ReadPoolSize int
}

// Open prepares the data directory, opens the database, applies the schema, and returns a Store.
//
// It refuses to start on a network filesystem unless explicitly allowed: WAL depends on shared
// memory and POSIX advisory locks that NFS, CIFS and friends do not provide reliably, and silent
// corruption is a far worse outcome than a clear startup failure.
func Open(opts Options) (*Store, error) {
	if opts.ReadPoolSize <= 0 {
		opts.ReadPoolSize = 8
	}
	if err := os.MkdirAll(opts.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	if net, name := networkFS(opts.DataDir); net && !opts.AllowNetworkFS {
		return nil, fmt.Errorf(
			"store: DATA_DIR %s is on a %s filesystem, where SQLite WAL mode is unsafe; "+
				"use a block-backed volume (RWO ext4/xfs) or set ALLOW_NETWORK_FS=true to override",
			opts.DataDir, name)
	}

	blob, err := NewBlobStore(opts.DataDir)
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(opts.DataDir, "gateway.db")

	// synchronous=FULL on the writer: the 202 is a durability promise. Transactions survive an
	// application crash under either setting, but only FULL survives node power loss.
	writeDSN := dsn(dbPath, "FULL")
	readDSN := dsn(dbPath, "NORMAL")

	w, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	if _, err := w.Exec(schema); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	r, err := sql.Open("sqlite", readDSN)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	r.SetMaxOpenConns(opts.ReadPoolSize)
	r.SetMaxIdleConns(opts.ReadPoolSize)

	return &Store{w: w, r: r, Blob: blob, dir: opts.DataDir}, nil
}

func dsn(path, sync string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous("+sync+")")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(ON)")
	return "file:" + path + "?" + q.Encode()
}

// Close releases both connection pools.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// DataDir returns the root of the persistent volume.
func (s *Store) DataDir() string { return s.dir }

// DiskUsage reports the fraction of the volume in use, in [0,1]. It returns ok=false on
// platforms or filesystems where usage cannot be determined, so callers skip eviction rather
// than acting on a wrong number.
func (s *Store) DiskUsage() (used float64, ok bool) {
	free, total, err := diskUsage(s.dir)
	if err != nil || total == 0 {
		return 0, false
	}
	return float64(total-free) / float64(total), true
}

// Create inserts a new job and commits durably. It must be called before the 202 is written.
func (s *Store) Create(ctx context.Context, j *Job) error {
	_, err := s.w.ExecContext(ctx, `
		INSERT INTO jobs (id, surface, model_id, api_version, status, created_at, updated_at,
		                  expires_at, request_query, content_type, input_path, input_bytes,
		                  client_request_id, prefix)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Surface, j.ModelID, j.APIVersion, string(j.Status),
		j.CreatedAt.Unix(), j.UpdatedAt.Unix(), j.ExpiresAt.Unix(),
		j.Query, j.ContentType, j.InputPath, j.InputBytes, j.ClientReqID, j.Prefix)
	if err != nil {
		return fmt.Errorf("store: create job: %w", err)
	}
	return nil
}

const jobColumns = `id, surface, model_id, api_version, status, created_at, updated_at,
	expires_at, request_query, content_type, input_path, input_bytes, result_path, result_bytes,
	error_json, attempts, lease_owner, lease_until, upstream_mode, upstream_op_url,
	upstream_status, upstream_ms, client_request_id, has_pdf, figures, prefix`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var created, updated, expires, lease int64
	var status, mode string
	var hasPDF int
	err := sc.Scan(&j.ID, &j.Surface, &j.ModelID, &j.APIVersion, &status, &created, &updated,
		&expires, &j.Query, &j.ContentType, &j.InputPath, &j.InputBytes, &j.ResultPath,
		&j.ResultBytes, &j.ErrorJSON, &j.Attempts, &j.LeaseOwner, &lease, &mode, &j.UpstreamOp,
		&j.UpStatus, &j.UpstreamMS, &j.ClientReqID, &hasPDF, &j.Figures, &j.Prefix)
	if err != nil {
		return nil, err
	}
	j.Status = Status(status)
	j.Mode = UpstreamMode(mode)
	j.CreatedAt = time.Unix(created, 0).UTC()
	j.UpdatedAt = time.Unix(updated, 0).UTC()
	j.ExpiresAt = time.Unix(expires, 0).UTC()
	j.LeaseUntil = time.Unix(lease, 0).UTC()
	j.HasPDF = hasPDF != 0
	return &j, nil
}

// ErrNoJob reports that no job exists with the requested id.
var ErrNoJob = errors.New("store: job not found")

// Get returns one job by id. Polls take this path, so it uses the read pool.
func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	row := s.r.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoJob
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job: %w", err)
	}
	return j, nil
}

// MarkRunning transitions a job to running and takes a lease on it.
func (s *Store) MarkRunning(ctx context.Context, id, owner string, leaseUntil time.Time, now time.Time) error {
	_, err := s.w.ExecContext(ctx, `
		UPDATE jobs SET status = ?, lease_owner = ?, lease_until = ?, updated_at = ?,
		                attempts = attempts + 1
		WHERE id = ?`,
		string(StatusRunning), owner, leaseUntil.Unix(), now.Unix(), id)
	return err
}

// Heartbeat extends the lease on a running job so the recovery sweeper does not reclaim work
// that is still progressing.
func (s *Store) Heartbeat(ctx context.Context, id string, leaseUntil time.Time) error {
	_, err := s.w.ExecContext(ctx,
		`UPDATE jobs SET lease_until = ? WHERE id = ? AND status = ?`,
		leaseUntil.Unix(), id, string(StatusRunning))
	return err
}

// Succeed records a completed job. The result envelope is already on disk at resultPath.
// It reports ok=false when no row matched, which means the job was deleted while it ran — by a
// client DELETE or by disk-pressure eviction. The caller must then discard the artifacts it just
// wrote, or they stay on the volume with nothing referencing them.
func (s *Store) Succeed(ctx context.Context, id, resultPath string, resultBytes int64,
	mode UpstreamMode, upstreamMS int64, hasPDF bool, figures string, now time.Time) (bool, error) {
	pdf := 0
	if hasPDF {
		pdf = 1
	}
	res, err := s.w.ExecContext(ctx, `
		UPDATE jobs SET status = ?, result_path = ?, result_bytes = ?, upstream_mode = ?,
		                upstream_ms = ?, has_pdf = ?, figures = ?, updated_at = ?,
		                lease_owner = '', lease_until = 0, input_path = ''
		WHERE id = ?`,
		string(StatusSucceeded), resultPath, resultBytes, string(mode), upstreamMS,
		pdf, figures, now.Unix(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Fail records a terminal failure with the Azure-shaped error object to serve to the client.
// Like Succeed, it reports ok=false when the row is already gone.
func (s *Store) Fail(ctx context.Context, id, errorJSON string, mode UpstreamMode,
	upstreamStatus int, upstreamMS int64, now time.Time) (bool, error) {
	res, err := s.w.ExecContext(ctx, `
		UPDATE jobs SET status = ?, error_json = ?, upstream_mode = ?, upstream_status = ?,
		                upstream_ms = ?, updated_at = ?, lease_owner = '', lease_until = 0
		WHERE id = ?`,
		string(StatusFailed), errorJSON, string(mode), upstreamStatus, upstreamMS, now.Unix(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// upstream_op_url is read back into Job.UpstreamOp but is never written, and deliberately so.
//
// It was added to let a restart resume polling a degraded 202 instead of re-submitting. That
// cannot work here: the containers keep their result store instance-local, and polls only find
// an operation because the job's own connection pool and cookie jar pin them to the replica that
// owns it. Neither survives the process. A resumed poll would land on an arbitrary replica, spend
// its blind-poll budget on 404s, and then fail a job that a re-submit would have completed — so
// recovery re-submits from the persisted input, and the column stays inert. It is kept rather
// than dropped because migrating an existing PVC database is a worse trade than an unused column.

// Requeue returns a job to notStarted so it can be picked up again after a crash.
//
// It takes a lease even though no worker holds the job yet. That lease is what stops the periodic
// reclaim pass from picking the same row up again in the window between requeueing it and a worker
// marking it running — which would run the analysis twice against the container.
func (s *Store) Requeue(ctx context.Context, id string, now, leaseUntil time.Time) error {
	_, err := s.w.ExecContext(ctx, `
		UPDATE jobs SET status = ?, lease_owner = ?, lease_until = ?, updated_at = ?
		WHERE id = ?`,
		string(StatusNotStarted), "requeued", leaseUntil.Unix(), now.Unix(), id)
	return err
}

// Orphaned returns jobs that stopped progressing while this process was running: notStarted jobs
// that were never picked up, and running jobs whose lease expired because their worker died.
//
// The lease filter is what makes this safe to run periodically — it will not reclaim a job that
// another worker in this process is still heartbeating. TTL-expired jobs are excluded for the same
// reason as in Abandoned.
// notStartedBefore excludes rows a live submit may still be handing to a worker, and the lease
// check excludes rows this process has already requeued but not yet started, so a periodic pass
// cannot run the same analysis a second time.
func (s *Store) Orphaned(ctx context.Context, now, notStartedBefore time.Time, limit int) ([]*Job, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE ((status = ? AND created_at < ? AND lease_until < ?)
		       OR (status = ? AND lease_until < ?))
		  AND expires_at >= ?
		ORDER BY created_at ASC LIMIT ?`,
		string(StatusNotStarted), notStartedBefore.Unix(), now.Unix(),
		string(StatusRunning), now.Unix(),
		now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectJobs(rows)
}

// Abandoned returns every job left unfinished by a previous process, ignoring leases entirely.
//
// It is only correct at startup, and only because the gateway is single-pod by design: a process
// that has just booted holds no leases, so any row still marked running was abandoned by whatever
// died. Applying the lease filter here was a real bug — a crash leaves lease_until minutes in the
// future, so the boot pass skipped exactly the jobs it existed to rescue and their clients polled
// a running status forever.
// Jobs past their TTL are excluded: their results are unfetchable by definition, so re-running
// them would spend container capacity — and Azure billing — producing output no client can ever
// read. That matters most after a long outage, when every stored job has expired at once.
func (s *Store) Abandoned(ctx context.Context, now time.Time, limit int) ([]*Job, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE status IN (?, ?) AND expires_at >= ?
		ORDER BY created_at ASC LIMIT ?`,
		string(StatusNotStarted), string(StatusRunning), now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectJobs(rows)
}

// Expired returns jobs past their TTL that are safe to delete, oldest first.
//
// A running job is excluded only while its LEASE is live. Deleting one out from under an active
// worker leaves that worker writing a result into a blob store with no row to reference it, and
// the artifacts it goes on to fetch are unreachable from that moment — so the live lease is the
// guard, and once it lapses the worker is presumed dead and the row becomes collectable.
//
// Waiting instead for the recovery pass to make such a job terminal would strand it: Abandoned and
// Orphaned both filter on expires_at >= now, so an already-expired running job is never reclaimed
// and its row and blobs would sit on the PVC forever. An expired notStarted job is collected
// unconditionally — nothing has begun, so there is nothing to pull out from under.
func (s *Store) Expired(ctx context.Context, now time.Time, limit int) ([]*Job, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE expires_at < ?
		  AND (status IN (?, ?) OR (status = ? AND lease_until < ?) OR status = ?)
		ORDER BY expires_at ASC LIMIT ?`,
		now.Unix(),
		string(StatusSucceeded), string(StatusFailed),
		string(StatusRunning), now.Unix(),
		string(StatusNotStarted),
		limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectJobs(rows)
}

// EvictionCandidates returns completed jobs to drop when the volume is near full.
//
// Only terminal jobs are eligible — evicting a running job would break a promise still in flight —
// and already-expired ones are offered first, because a poll on those returns 404 either way.
// Only once those are exhausted does this start on results a client could still legitimately
// fetch, which is a real cost and is logged as such by the caller.
func (s *Store) EvictionCandidates(ctx context.Context, now time.Time, limit int) ([]*Job, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE status IN (?, ?)
		ORDER BY (expires_at >= ?) ASC, updated_at ASC
		LIMIT ?`,
		string(StatusSucceeded), string(StatusFailed), now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectJobs(rows)
}

// Recent returns the most recently updated jobs, for the admin surface.
func (s *Store) Recent(ctx context.Context, limit int) ([]*Job, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectJobs(rows)
}

func collectJobs(rows *sql.Rows) ([]*Job, error) {
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Delete removes a job row and every artifact belonging to it.
//
// The row goes first, and deliberately so. Removing blobs first meant a partial failure — one
// unlinked result and one stubborn figure — left the row behind pointing at a result that no
// longer existed, and a poll on it then produced a 500 with no top-level status, which is the
// shape that crashes Java clients outright. Row-first inverts the failure: the worst case is an
// orphaned blob, which SweepOrphans reclaims and which no client can observe.
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.w.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Blob.RemoveAll(id)
}

// Exists reports whether a job row is present.
func (s *Store) Exists(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.r.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id = ? LIMIT 1`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		// The error is returned rather than folded into "absent". This is the only guard on a
		// destructive path: reporting a live job's row as missing made the orphan sweeper delete
		// every artifact of a succeeded, unexpired job whose result a client was still entitled
		// to fetch.
		return false, fmt.Errorf("store: job exists: %w", err)
	}
	return n == 1, nil
}

// Counts summarises job states for the admin surface and metrics.
type Counts struct {
	NotStarted int `json:"notStarted"`
	Running    int `json:"running"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	Total      int `json:"total"`
}

// Stats returns per-surface job counts.
func (s *Store) Stats(ctx context.Context) (map[string]*Counts, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT surface, status, COUNT(*) FROM jobs GROUP BY surface, status`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]*Counts{}
	for rows.Next() {
		var surface, status string
		var n int
		if err := rows.Scan(&surface, &status, &n); err != nil {
			return nil, err
		}
		c, ok := out[surface]
		if !ok {
			c = &Counts{}
			out[surface] = c
		}
		switch Status(status) {
		case StatusNotStarted:
			c.NotStarted = n
		case StatusRunning:
			c.Running = n
		case StatusSucceeded:
			c.Succeeded = n
		case StatusFailed:
			c.Failed = n
		}
		c.Total += n
	}
	return out, rows.Err()
}

// Ping verifies the database is reachable, for the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.w.PingContext(ctx); err != nil {
		return err
	}
	return s.r.PingContext(ctx)
}
