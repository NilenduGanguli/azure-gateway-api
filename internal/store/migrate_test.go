package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// legacySchema is the schema gateways applied before versioning from commit ef625fe on, frozen
// here as the version-1 fixture. It is a copy rather than a read of migrations/0001_initial.sql on
// purpose: volumes in service hold exactly this text, so if 0001 were ever edited in place, a fresh
// volume would drift from a migrated one and TestSchemaFromScratchEqualsMigrated would fail.
const legacySchema = `
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

// prePrefixSchema is the schema gateways applied before commit ef625fe added the prefix column,
// the only other schema ever released: legacySchema without its last column. CREATE TABLE IF NOT
// EXISTS never added the column to those tables afterwards, so such a volume predates versioning
// without being at version 1.
var prePrefixSchema = strings.Replace(legacySchema,
	"  figures           TEXT NOT NULL DEFAULT '',\n  prefix            TEXT NOT NULL DEFAULT ''\n",
	"  figures           TEXT NOT NULL DEFAULT ''\n", 1)

// openVolume opens a Store over dir and closes it when the test ends. Tests that reopen the same
// volume close it earlier themselves, which is safe because Close is idempotent.
func openVolume(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(Options{DataDir: dir, AllowNetworkFS: true})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// rawDB opens a database file directly, bypassing Open and every migration, the way an operator's
// sqlite3 shell would. It sets no busy timeout, so a lock a failed Open left behind surfaces as an
// immediate SQLITE_BUSY rather than a ten-second stall.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		name).Scan(&n); err != nil {
		t.Fatalf("look up table %s: %v", name, err)
	}
	return n > 0
}

func jobCount(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count job %s: %v", id, err)
	}
	return n
}

// schemaObject is one sqlite_master row. rootpage is left out: it records allocation order, not
// the schema.
type schemaObject struct {
	Type, Name, Table string
	SQL               sql.NullString // NULL for the automatic primary-key index
}

func schemaOf(t *testing.T, db *sql.DB) []schemaObject {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE name != 'sqlite_sequence' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.Type, &o.Name, &o.Table, &o.SQL); err != nil {
			t.Fatalf("scan schema: %v", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	return out
}

// backups lists every file in dir named like a backup: the copies migrate takes, any partial copy,
// and whatever an operator saved alongside them. It is deliberately broader than what migrate
// manages, so a test sees anything left behind.
func backups(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), dbName+".v") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// withMigration adds m to the migrations this binary carries, for the rest of the test.
func withMigration(t *testing.T, m migration) {
	t.Helper()
	saved := migrations
	// The full slice expression forces append to copy, so the package's own array is never written.
	migrations = append(migrations[:len(migrations):len(migrations)], m)
	t.Cleanup(func() { migrations = saved })
}

// nextMigration builds a synthetic migration one past the newest this binary carries.
func nextMigration(name, body string) migration {
	v := SchemaVersion() + 1
	return migration{version: v, name: fmt.Sprintf("%04d_%s.sql", v, name), sql: body}
}

// legacyDatabase leaves in dir the gateway.db a pre-versioning gateway wrote: schema applied the
// way Open used to apply it, in WAL mode, with user_version never set. fill, when not nil, adds
// rows before the database is closed.
func legacyDatabase(t *testing.T, dir, schema string, fill func(*sql.DB) error) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, dbName), "FULL"))
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		t.Fatalf("apply legacy schema: %v", err)
	}
	if fill != nil {
		if err := fill(db); err != nil {
			_ = db.Close()
			t.Fatalf("fill legacy database: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}
}

// legacyVolume leaves in dir a version-1 database from before versioning holding one job row, and
// returns the row's id.
func legacyVolume(t *testing.T, dir string) string {
	t.Helper()
	return legacyVolumeWith(t, dir, legacySchema)
}

// prePrefixVolume is legacyVolume for a volume created before the prefix column existed.
func prePrefixVolume(t *testing.T, dir string) string {
	t.Helper()
	return legacyVolumeWith(t, dir, prePrefixSchema)
}

// legacyVolumeWith is legacyVolume over another schema from before versioning. Its row names only
// columns every released schema has.
func legacyVolumeWith(t *testing.T, dir, schema string) string {
	t.Helper()
	const id = "00000000-0000-4000-8000-00000000a001"
	now := time.Now().Unix()
	legacyDatabase(t, dir, schema, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO jobs (id, surface, model_id, status, created_at, updated_at,
			expires_at) VALUES (?, 'di', 'prebuilt-layout', ?, ?, ?, ?)`,
			id, string(StatusSucceeded), now, now, now+3600)
		return err
	})
	return id
}

// currentVolume leaves in dir a database at the newest version holding one job row, and returns
// the row's id.
func currentVolume(t *testing.T, dir string) string {
	t.Helper()
	s := openVolume(t, dir)
	const id = "00000000-0000-4000-8000-00000000c001"
	now := time.Now().UTC()
	if err := s.Create(context.Background(), &Job{
		ID: id, Surface: "di", Status: StatusSucceeded,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return id
}

// startingState is a volume a new migration can meet in service.
type startingState struct {
	name   string
	build  func(t *testing.T, dir string) string // leaves a closed volume holding one job
	from   int                                   // the version Open reports migrating from
	stamp  int                                   // user_version on disk before migrating
	legacy bool                                  // whether Open reports a pre-versioning table
}

func startingStates() []startingState {
	return []startingState{
		{"at the newest version", currentVolume, SchemaVersion(), SchemaVersion(), false},
		{"from before versioning", legacyVolume, 1, 0, true},
		{"from before the prefix column", prePrefixVolume, 1, 0, true},
	}
}

// TestFreshVolumeReachesLatest checks an empty volume: every migration runs from nothing, and no
// backup is taken because there is nothing yet to protect.
func TestFreshVolumeReachesLatest(t *testing.T) {
	dir := t.TempDir()
	s := openVolume(t, dir)

	if got := userVersion(t, s.r); got != SchemaVersion() {
		t.Errorf("user_version is %d, want %d", got, SchemaVersion())
	}
	if r := s.Migration; r.From != 0 || r.To != SchemaVersion() || r.Backup != "" || r.Legacy {
		t.Errorf("report %+v, want From=0 To=%d, no backup, not legacy", r, SchemaVersion())
	}
	if b := backups(t, dir); len(b) != 0 {
		t.Errorf("a fresh volume was backed up: %v", b)
	}
	if !hasTable(t, s.r, "jobs") {
		t.Error("a fresh volume has no jobs table")
	}
}

// TestLegacyDatabaseIsVersionOne covers every volume in service today: written before versioning,
// with user_version never set. It must be recognised as version 1 with its rows intact, not
// mistaken for a fresh volume.
func TestLegacyDatabaseIsVersionOne(t *testing.T) {
	dir := t.TempDir()
	id := legacyVolume(t, dir)

	s := openVolume(t, dir)
	// From and To are equal while 0001 is the only migration, but a stamp was written: Legacy is
	// what tells the startup log line so.
	if r := s.Migration; r.From != 1 || r.To != SchemaVersion() || !r.Legacy {
		t.Errorf("report %+v, want From=1 To=%d and legacy", r, SchemaVersion())
	}
	if got := userVersion(t, s.r); got != SchemaVersion() {
		t.Errorf("user_version is %d, want %d", got, SchemaVersion())
	}
	job, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("the legacy row is unreadable after migrating: %v", err)
	}
	if job.Surface != "di" || job.ModelID != "prebuilt-layout" || job.Status != StatusSucceeded {
		t.Errorf("the legacy row changed: %+v", job)
	}

	// While 0001 is the only migration, version 1 is already the target and only the header
	// changes, so there is nothing to back up. Once a later migration exists, the legacy database
	// is backed up as version 1 before it runs.
	var want []string
	if SchemaVersion() > 1 {
		want = append(want, backupPath(dir, 1))
	}
	if got := backups(t, dir); !slices.Equal(got, want) {
		t.Errorf("backups on the volume are %v, want %v", got, want)
	}
	if len(want) == 0 && s.Migration.Backup != "" {
		t.Errorf("report names backup %q, want none", s.Migration.Backup)
	}
}

// volumeState is what a write to the database would change on disk.
type volumeState struct {
	size, mtime int64
	wal         int64 // -1 when there is no WAL file
}

func stateOf(t *testing.T, dir string) volumeState {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, dbName))
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	return volumeState{size: fi.Size(), mtime: fi.ModTime().UnixNano(), wal: walSize(t, dir)}
}

func walSize(t *testing.T, dir string) int64 {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, dbName+"-wal"))
	if os.IsNotExist(err) {
		return -1
	}
	if err != nil {
		t.Fatalf("stat WAL: %v", err)
	}
	return fi.Size()
}

// TestEqualVersionDoesNoWrites guards the normal restart. A database already at this binary's
// version must come up after a single read: no backup, no transaction, nothing written.
func TestEqualVersionDoesNoWrites(t *testing.T) {
	dir := t.TempDir()
	if err := openVolume(t, dir).Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before := stateOf(t, dir)

	s := openVolume(t, dir)
	// Mtime alone could miss a write within the filesystem's timestamp granularity. The WAL cannot:
	// while the store is open, a write sits there as frames until a checkpoint, and nothing at
	// startup writes enough to trigger one before Close.
	if n := walSize(t, dir); n > 0 {
		t.Errorf("the equal-version start wrote %d bytes of WAL", n)
	}
	if r := s.Migration; r.From != r.To || r.To != SchemaVersion() || r.Backup != "" || r.Legacy {
		t.Errorf("report %+v, want From=To=%d, no backup, not legacy", r, SchemaVersion())
	}
	if got := userVersion(t, s.r); got != SchemaVersion() {
		t.Errorf("user_version is %d, want %d", got, SchemaVersion())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if b := backups(t, dir); len(b) != 0 {
		t.Errorf("the equal-version start took a backup: %v", b)
	}
	if after := stateOf(t, dir); after != before {
		t.Errorf("the database changed on disk: before %+v, after %+v", before, after)
	}
}

// TestNewerSchemaIsRefused covers an image rolled back after a migration. The older binary cannot
// know what the newer schema means, so it must refuse to start rather than run against it, and say
// how to recover.
func TestNewerSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := openVolume(t, dir).Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db := rawDB(t, filepath.Join(dir, dbName))
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	s, err := Open(Options{DataDir: dir, AllowNetworkFS: true})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a database newer than this gateway")
	}
	if !strings.Contains(err.Error(), "newer than this gateway") {
		t.Errorf("error %q does not say the schema is newer than this gateway", err)
	}
	if want := backupPath(dir, SchemaVersion()); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the backup to restore, %s", err, want)
	}
	if got := userVersion(t, db); got != 99 {
		t.Errorf("user_version is %d after the refusal, want it left at 99", got)
	}
	if b := backups(t, dir); len(b) != 0 {
		t.Errorf("a refused start took a backup: %v", b)
	}
}

// TestFailedMigrationRollsBack checks that a migration is all or nothing. A statement failing
// partway must take every earlier statement with it, including the version stamp and any column
// added to complete a legacy table, so the gateway refuses to start on a database still exactly at
// its old version, not one stranded between two.
func TestFailedMigrationRollsBack(t *testing.T) {
	// A fresh volume runs 0001 in the same transaction as the failing migration, so it must end
	// with no jobs table at all rather than a half-built schema stamped as nothing.
	fresh := startingState{name: "on a fresh volume",
		build: func(*testing.T, string) string { return "" }}
	for _, st := range append(startingStates(), fresh) {
		t.Run(st.name, func(t *testing.T) {
			dir := t.TempDir()
			id := st.build(t, dir)
			path := filepath.Join(dir, dbName)
			var want []schemaObject
			if id != "" {
				before := rawDB(t, path)
				want = schemaOf(t, before)
				_ = before.Close()
			}

			// Two statements that succeed, then one that cannot, so the rollback has real work.
			bad := nextMigration("broken", "CREATE TABLE extra (x INTEGER);\n"+
				"ALTER TABLE jobs ADD COLUMN doomed INTEGER;\n"+
				"INSERT INTO no_such_table VALUES (1);\n")
			withMigration(t, bad)

			s, err := Open(Options{DataDir: dir, AllowNetworkFS: true})
			if err == nil {
				_ = s.Close()
				t.Fatal("Open succeeded although a migration failed")
			}
			if !strings.Contains(err.Error(), bad.name) {
				t.Errorf("error %q does not name the failing migration %s", err, bad.name)
			}

			db := rawDB(t, path)
			if got := userVersion(t, db); got != st.stamp {
				t.Errorf("user_version is %d, want it left at %d", got, st.stamp)
			}
			if got := schemaOf(t, db); !slices.Equal(got, want) {
				t.Errorf("the failed migration changed the schema:\nbefore %+v\nafter  %+v",
					want, got)
			}
			if id == "" {
				if hasTable(t, db, "jobs") {
					t.Error("a fresh volume kept a jobs table from a migration that failed")
				}
			} else if jobCount(t, db, id) != 1 {
				t.Error("the job row did not survive the failed migration")
			}
			// A transaction left open by the failed Open would still hold the write lock, and with
			// no busy timeout this would fail at once.
			if _, err := db.Exec(`BEGIN IMMEDIATE; ROLLBACK`); err != nil {
				t.Errorf("the failed Open left the database locked: %v", err)
			}
		})
	}
}

// TestBackupTakenBeforeMigrating checks the copy an operator restores after rolling an image back:
// it must be a valid database holding the data and version from before the migration ran. Copies
// from earlier migrations are replaced, not accumulated, and a copy the operator made is left
// alone however it is named.
func TestBackupTakenBeforeMigrating(t *testing.T) {
	for _, st := range startingStates() {
		t.Run(st.name, func(t *testing.T) {
			dir := t.TempDir()
			id := st.build(t, dir)

			// One stale copy shares the new backup's name, which VACUUM INTO refuses to overwrite.
			stale := []string{backupPath(dir, st.from), backupPath(dir, 42),
				backupPath(dir, st.from) + ".partial"}
			for _, p := range stale {
				if err := os.WriteFile(p, []byte("stale"), 0o600); err != nil {
					t.Fatalf("seed stale backup: %v", err)
				}
			}
			operator := filepath.Join(dir, dbName+".v1-before-upgrade.bak")
			if err := os.WriteFile(operator, []byte("operator's own copy"), 0o600); err != nil {
				t.Fatalf("seed operator copy: %v", err)
			}
			next := nextMigration("extra", "CREATE TABLE extra (x INTEGER);\n")
			withMigration(t, next)

			s := openVolume(t, dir)
			bak := backupPath(dir, st.from)
			if r := s.Migration; r.From != st.from || r.To != next.version || r.Backup != bak ||
				r.Legacy != st.legacy {
				t.Errorf("report %+v, want From=%d To=%d Backup=%s Legacy=%v",
					r, st.from, next.version, bak, st.legacy)
			}
			want := []string{bak, operator}
			slices.Sort(want)
			if got := backups(t, dir); !slices.Equal(got, want) {
				t.Errorf("backup files on the volume are %v, want %v", got, want)
			}
			if body, err := os.ReadFile(operator); err != nil || string(body) != "operator's own copy" {
				t.Errorf("the operator's copy changed: %q, %v", body, err)
			}

			old := rawDB(t, bak)
			var check string
			if err := old.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
				t.Fatalf("the backup is not a sound database: %q, %v", check, err)
			}
			if got := userVersion(t, old); got != st.stamp {
				t.Errorf("the backup's user_version is %d, want %d", got, st.stamp)
			}
			if hasTable(t, old, "extra") {
				t.Error("the backup already holds the migration's table; it was taken too late")
			}
			if jobCount(t, old, id) != 1 {
				t.Error("the backup lost the job row")
			}

			if got := userVersion(t, s.r); got != next.version {
				t.Errorf("user_version is %d, want %d", got, next.version)
			}
			if !hasTable(t, s.r, "extra") {
				t.Error("the migration did not run")
			}
			if _, err := s.Get(context.Background(), id); err != nil {
				t.Errorf("the job row is unreadable after migrating: %v", err)
			}
		})
	}
}

// TestSchemaFromScratchEqualsMigrated checks that every migration after 0001 does the same thing
// to a volume in service since before versioning as to a fresh one. A fresh volume runs every
// migration from nothing; the legacy one starts from the frozen version-1 fixture. Both must end
// with the same schema, compared by stored CREATE text. That also catches an in-place edit of
// 0001, which the fixture would no longer match; edits to any other released migration are
// TestReleasedMigrationsAreUnchanged's to catch.
//
// TestPrePrefixLegacyTableIsCompleted makes the same comparison from the pre-prefix fixture, by
// column set for the jobs table, because the ADD COLUMN that completes that table rewrites its
// text.
func TestSchemaFromScratchEqualsMigrated(t *testing.T) {
	fresh := schemaOf(t, openVolume(t, t.TempDir()).r)
	if len(fresh) == 0 {
		t.Fatal("a fresh volume has no schema at all")
	}

	// Before versioning, CREATE TABLE and its indexes ran as separate autocommit statements, so a
	// crash between them could leave an index missing. Completing the legacy table restores it.
	lostIndex := strings.Replace(legacySchema,
		"CREATE INDEX IF NOT EXISTS jobs_expiry  ON jobs(expires_at);\n", "", 1)
	if lostIndex == legacySchema {
		t.Fatal("the lost-index fixture still creates jobs_expiry")
	}
	for _, tt := range []struct{ name, schema string }{
		{"version-1 fixture", legacySchema},
		{"version-1 fixture that lost an index", lostIndex},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			legacyVolumeWith(t, dir, tt.schema)
			migrated := schemaOf(t, openVolume(t, dir).r)
			for i := range max(len(fresh), len(migrated)) {
				var f, m schemaObject
				if i < len(fresh) {
					f = fresh[i]
				}
				if i < len(migrated) {
					m = migrated[i]
				}
				if f != m {
					t.Errorf("schema object %d differs:\nfresh    %+v\nmigrated %+v", i, f, m)
				}
			}
		})
	}
}

// TestReleasedMigrationsAreUnchanged pins every migration this binary carries. A released file has
// already run on volumes in service, so editing it changes nothing there and only makes fresh
// volumes differ from old ones; a schema change must be a new file instead. A file missing from
// the map fails the test too, so adding a migration means adding its hash here.
func TestReleasedMigrationsAreUnchanged(t *testing.T) {
	pinned := map[string]string{
		"0001_initial.sql": "de6eedb8ab8389d825f538658c3382eeeb905bc6cd0a4d60902af11ce10663eb",
	}
	for name, want := range pinned {
		body, err := fs.ReadFile(migrationFS, "migrations/"+name)
		if err != nil {
			t.Errorf("released migration %s is gone: %v", name, err)
			continue
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != want {
			t.Errorf("released migration %s was edited: sha256 %s, want %s; add a new migration "+
				"instead", name, got, want)
		}
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("list embedded migrations: %v", err)
	}
	for _, e := range entries {
		if _, ok := pinned[e.Name()]; !ok {
			body, _ := fs.ReadFile(migrationFS, "migrations/"+e.Name())
			t.Errorf("migration %s is not pinned; add %q: %q to the map", e.Name(), e.Name(),
				fmt.Sprintf("%x", sha256.Sum256(body)))
		}
	}
}

// cutShortCopy writes at path what a copy cut short leaves: a file with pages in it that
// SQLite still reads as a sound, empty database. Being non-empty, it is also what VACUUM INTO
// refuses to write over.
func cutShortCopy(t *testing.T, path string) {
	t.Helper()
	db := rawDB(t, path)
	if _, err := db.Exec(`CREATE TABLE t (x); DROP TABLE t;`); err != nil {
		t.Fatalf("write debris: %v", err)
	}
	var check string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("the debris is not the valid-looking file a cut-short copy is: %q, %v", check, err)
	}
	if len(schemaOf(t, db)) != 0 {
		t.Fatal("the debris is not empty of schema objects")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close debris: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("the debris is empty on disk, which VACUUM INTO would accept anyway: %v", err)
	}
}

// TestInterruptedBackupIsCleared covers a pod killed, or a node losing power, partway through the
// pre-migration copy. Whatever VACUUM INTO had written by then is a valid SQLite file: a copy
// killed 8 MB into 550 MB passes quick_check with no schema objects and user_version 0, so it
// reads as an empty database. Restored as gateway.db it would pass for a fresh volume and every
// job would be gone, so such a file must never carry a backup's name, and the next start must
// clear it away, whatever version it was for.
func TestInterruptedBackupIsCleared(t *testing.T) {
	dir := t.TempDir()
	id := currentVolume(t, dir)
	base := SchemaVersion()
	// One sits on the name this start's copy needs, the other was left by a start at another
	// version.
	cutShortCopy(t, backupPath(dir, base)+".partial")
	cutShortCopy(t, backupPath(dir, 42)+".partial")

	withMigration(t, nextMigration("extra", "CREATE TABLE extra (x INTEGER);\n"))
	s := openVolume(t, dir)
	bak := backupPath(dir, base)
	if s.Migration.Backup != bak {
		t.Errorf("report names backup %q, want %s", s.Migration.Backup, bak)
	}
	if got := backups(t, dir); !slices.Equal(got, []string{bak}) {
		t.Errorf("backup files on the volume are %v, want only %s", got, bak)
	}
	if jobCount(t, rawDB(t, bak), id) != 1 {
		t.Error("the backup is not a full copy of the database")
	}
}

// TestBackupAppearsOnlyWhenComplete checks that a copy reaches the backup's name only through the
// final rename. A copy that cannot be finished, here because a directory sits on its temporary
// name, must fail the start with nothing under gateway.db.v<N>.bak and the database unchanged.
func TestBackupAppearsOnlyWhenComplete(t *testing.T) {
	dir := t.TempDir()
	currentVolume(t, dir)
	base := SchemaVersion()
	blocker := backupPath(dir, base) + ".partial"
	if err := os.MkdirAll(filepath.Join(blocker, "occupied"), 0o750); err != nil {
		t.Fatalf("block the temporary name: %v", err)
	}
	withMigration(t, nextMigration("extra", "CREATE TABLE extra (x INTEGER);\n"))

	s, err := Open(Options{DataDir: dir, AllowNetworkFS: true})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded although the backup could not be written")
	}
	if _, err := os.Stat(backupPath(dir, base)); !os.IsNotExist(err) {
		t.Errorf("something appeared under %s although the copy failed: %v",
			backupPath(dir, base), err)
	}
	db := rawDB(t, filepath.Join(dir, dbName))
	if got := userVersion(t, db); got != base {
		t.Errorf("user_version is %d, want it left at %d", got, base)
	}
	if hasTable(t, db, "extra") {
		t.Error("the migration ran without a backup")
	}
}

// TestMismatchedCopyIsNotInstalled covers a copy taken after another gateway process migrated the
// database this start read. It holds a newer user_version than its name would claim, and a
// rollback restoring it would be misled, so it must be discarded, and the backups already on the
// volume left exactly as they were.
func TestMismatchedCopyIsNotInstalled(t *testing.T) {
	dir := t.TempDir()
	currentVolume(t, dir)
	base := SchemaVersion()
	previous := []string{backupPath(dir, base), backupPath(dir, 42)}
	slices.Sort(previous)
	for _, p := range previous {
		if err := os.WriteFile(p, []byte("previous"), 0o600); err != nil {
			t.Fatalf("seed previous backup: %v", err)
		}
	}
	db := rawDB(t, filepath.Join(dir, dbName))
	// This start read base; another process has since moved the database on.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, base+1)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	_, err = backup(ctx, conn, dir, base, base)
	_ = conn.Close()
	if err == nil || !strings.HasPrefix(err.Error(), "store: ") {
		t.Fatalf("backup of a database that moved on returned %v", err)
	}
	if got := backups(t, dir); !slices.Equal(got, previous) {
		t.Errorf("backup files on the volume are %v, want the previous ones %v", got, previous)
	}
	for _, p := range previous {
		if body, err := os.ReadFile(p); err != nil || string(body) != "previous" {
			t.Errorf("%s changed: %q, %v", p, body, err)
		}
	}
}

// TestVersionChangedBeforeTheLockIsRefused checks that a plan made from a version that has since
// changed is not applied. The migrations it lists have already run, and must not run twice.
func TestVersionChangedBeforeTheLockIsRefused(t *testing.T) {
	dir := t.TempDir()
	currentVolume(t, dir)
	base := SchemaVersion()
	next := nextMigration("extra", "CREATE TABLE extra (x INTEGER);\n")
	withMigration(t, next)

	db := rawDB(t, filepath.Join(dir, dbName))
	// The other gateway won the race: the version moved on after this one read base.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, next.version)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	err = apply(ctx, conn, plan{seen: base, pending: []migration{next}, target: next.version})
	_ = conn.Close()
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("apply on a version that changed under it returned %v", err)
	}
	if !strings.HasPrefix(err.Error(), "store: ") {
		t.Errorf("error %q is not a store error", err)
	}
	if hasTable(t, db, "extra") {
		t.Error("the migration ran a second time")
	}
	if got := userVersion(t, db); got != next.version {
		t.Errorf("user_version is %d, want the winner's %d", got, next.version)
	}
	if _, err := db.Exec(`BEGIN IMMEDIATE; ROLLBACK`); err != nil {
		t.Errorf("the refused apply left the database locked: %v", err)
	}
}

// columnInfo is one PRAGMA table_info row without cid, the column's position.
type columnInfo struct {
	Name, Type string
	NotNull    bool
	Default    sql.NullString
	PK         int
}

// jobsColumnSet returns the jobs table's columns ordered by name, so two tables holding the same
// columns in a different order compare equal.
func jobsColumnSet(t *testing.T, db *sql.DB) []columnInfo {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(jobs)`)
	if err != nil {
		t.Fatalf("read jobs columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []columnInfo
	for rows.Next() {
		var cid int
		var c columnInfo
		if err := rows.Scan(&cid, &c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK); err != nil {
			t.Fatalf("scan jobs column: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read jobs columns: %v", err)
	}
	slices.SortFunc(out, func(a, b columnInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// TestPrePrefixLegacyTableIsCompleted covers a volume created before the prefix column existed.
// Stamping its table version 1 as it stands would be false, and every insert the gateway makes,
// all of which name prefix, would fail. Open must add the column first, inside the migration's
// transaction, which TestFailedMigrationRollsBack checks from this same starting state.
func TestPrePrefixLegacyTableIsCompleted(t *testing.T) {
	if strings.Contains(prePrefixSchema, "prefix") {
		t.Fatal("the pre-prefix fixture still declares the prefix column")
	}
	dir := t.TempDir()
	id := prePrefixVolume(t, dir)

	s := openVolume(t, dir)
	if r := s.Migration; r.From != 1 || r.To != SchemaVersion() || !r.Legacy {
		t.Errorf("report %+v, want From=1 To=%d and legacy", r, SchemaVersion())
	}
	if got := userVersion(t, s.r); got != SchemaVersion() {
		t.Errorf("user_version is %d, want %d", got, SchemaVersion())
	}
	ctx := context.Background()
	job, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("the legacy row is unreadable after migrating: %v", err)
	}
	if job.Prefix != "" || job.Surface != "di" || job.ModelID != "prebuilt-layout" {
		t.Errorf("the legacy row changed: %+v", job)
	}
	now := time.Now().UTC()
	added := &Job{
		ID: "00000000-0000-4000-8000-00000000a002", Surface: "di", Status: StatusNotStarted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Prefix: "/formrecognizer",
	}
	if err := s.Create(ctx, added); err != nil {
		t.Fatalf("create on the completed table: %v", err)
	}
	if got, err := s.Get(ctx, added.ID); err != nil || got.Prefix != added.Prefix {
		t.Errorf("prefix did not round-trip: %+v, %v", got, err)
	}

	// ADD COLUMN appends the column and splices its definition into the stored CREATE TABLE text,
	// so against a fresh volume the jobs table is compared by its set of columns, not by that
	// text. Every other schema object, the three indexes included, must match exactly.
	fresh := openVolume(t, t.TempDir())
	if got, want := jobsColumnSet(t, s.r), jobsColumnSet(t, fresh.r); !slices.Equal(got, want) {
		t.Errorf("jobs columns differ from a fresh volume's:\nfresh    %+v\nmigrated %+v", want, got)
	}
	isJobsTable := func(o schemaObject) bool { return o.Type == "table" && o.Name == "jobs" }
	got := slices.DeleteFunc(schemaOf(t, s.r), isJobsTable)
	want := slices.DeleteFunc(schemaOf(t, fresh.r), isJobsTable)
	if len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("schema differs from a fresh volume's:\nfresh    %+v\nmigrated %+v", want, got)
	}
}

// TestLegacyTableWithoutRequiredColumnIsRefused covers a jobs table no gateway wrote. Every build
// had id, surface, status and the three timestamps, and none of them can be added in the form 0001
// declares them, so Open must refuse, name what is missing, and leave the database as it found it.
func TestLegacyTableWithoutRequiredColumnIsRefused(t *testing.T) {
	dir := t.TempDir()
	legacyDatabase(t, dir, `CREATE TABLE jobs (id TEXT PRIMARY KEY, surface TEXT NOT NULL);`, nil)
	db := rawDB(t, filepath.Join(dir, dbName))
	want := schemaOf(t, db)

	s, err := Open(Options{DataDir: dir, AllowNetworkFS: true})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a jobs table that lacks required columns")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "store: ") {
		t.Errorf("error %q is not a store error", msg)
	}
	for _, col := range []string{"status", "created_at", "updated_at", "expires_at"} {
		if !strings.Contains(msg, col) {
			t.Errorf("error %q does not name the missing column %s", msg, col)
		}
	}
	if strings.Contains(msg, "surface") {
		t.Errorf("error %q names surface, which the table has", msg)
	}
	if got := userVersion(t, db); got != 0 {
		t.Errorf("user_version is %d, want it left at 0", got)
	}
	if got := schemaOf(t, db); !slices.Equal(got, want) {
		t.Errorf("the refused start changed the schema:\nbefore %+v\nafter  %+v", want, got)
	}
}

// TestLoadMigrations checks the guard on the embedded files. A misnamed or misnumbered migration
// would otherwise be skipped or run out of order on every volume, without a word.
func TestLoadMigrations(t *testing.T) {
	sqlFile := &fstest.MapFile{Data: []byte("CREATE TABLE t (x INTEGER);\n")}
	tests := []struct {
		name     string
		files    []string
		versions []int  // when loading succeeds
		wantErr  string // when it must fail, a fragment of the error
	}{
		{"contiguous", []string{"0001_a.sql", "0002_sync-only.sql"}, []int{1, 2}, ""},
		{"gap", []string{"0001_a.sql", "0003_c.sql"}, nil, "0002 is missing"},
		{"not starting at one", []string{"0002_b.sql"}, nil, "0001 is missing"},
		{"duplicate", []string{"0001_a.sql", "0001_b.sql"}, nil, "0001_a.sql and 0001_b.sql"},
		{"version zero", []string{"0000_z.sql", "0001_a.sql"}, nil, "0000_z.sql"},
		{"misnamed", []string{"0001_a.sql", "2_b.sql"}, nil, "2_b.sql"},
		{"upper-case extension", []string{"0001_a.sql", "0002_b.SQL"}, nil, "0002_b.SQL"},
		{"empty", nil, nil, "no migrations"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{"migrations": &fstest.MapFile{Mode: fs.ModeDir | 0o755}}
			for _, f := range tt.files {
				fsys["migrations/"+f] = sqlFile
			}
			got, err := loadMigrations(fsys)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			var versions []int
			for _, m := range got {
				versions = append(versions, m.version)
				if m.sql != string(sqlFile.Data) {
					t.Errorf("%s: body %q, want the file's contents", m.name, m.sql)
				}
			}
			if !slices.Equal(versions, tt.versions) {
				t.Errorf("versions %v, want %v", versions, tt.versions)
			}
		})
	}

	t.Run("embedded", func(t *testing.T) {
		got, err := loadMigrations(migrationFS)
		if err != nil {
			t.Fatalf("the migrations this binary carries do not load: %v", err)
		}
		if got[0].name != "0001_initial.sql" {
			t.Errorf("the first migration is %s, want 0001_initial.sql", got[0].name)
		}
	})
}

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"one per line", "CREATE TABLE a (x);\nCREATE TABLE b (y);\n",
			[]string{"CREATE TABLE a (x);", "CREATE TABLE b (y);"}},
		{"multi-line statement", "CREATE TABLE a (\n  x INTEGER\n);\nCREATE INDEX i ON a(x);\n",
			[]string{"CREATE TABLE a (\n  x INTEGER\n);", "CREATE INDEX i ON a(x);"}},
		{"semicolon inside a line", "INSERT INTO a VALUES ('x;y');\n",
			[]string{"INSERT INTO a VALUES ('x;y');"}},
		{"comment line ending in a semicolon",
			"ALTER TABLE a\n-- was DROP COLUMN x;\n  ADD COLUMN y;\n",
			[]string{"ALTER TABLE a\n-- was DROP COLUMN x;\n  ADD COLUMN y;"}},
		{"comment-only chunks dropped", "-- header\n\nCREATE TABLE a (x);\n\n-- trailer\n",
			[]string{"-- header\n\nCREATE TABLE a (x);"}},
		{"last statement without a semicolon", "CREATE TABLE a (x);\nCREATE TABLE b (y)",
			[]string{"CREATE TABLE a (x);", "CREATE TABLE b (y)"}},
		{"CRLF and trailing blanks", "CREATE TABLE a (x); \t\r\nCREATE TABLE b (y);\r\n",
			[]string{"CREATE TABLE a (x);", "CREATE TABLE b (y);"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitStatements(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	// The version-1 schema is the table and its three indexes.
	if got := splitStatements(legacySchema); len(got) != 4 {
		t.Errorf("the version-1 schema split into %d statements, want 4: %q", len(got), got)
	}
}
