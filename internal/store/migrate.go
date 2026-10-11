package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Schema changes reach a volume only through numbered migrations.
//
// The schema used to be a single CREATE TABLE IF NOT EXISTS, which does nothing when the table is
// already there. A changed schema therefore never reached a volume in service, and queries against
// a new column would fail there at runtime instead of the gateway failing at startup.
//
// Each file in migrations/ is named NNNN_name.sql, numbered from 0001 without gaps, and the highest
// number is the schema version this binary expects. The version a database is actually at lives in
// PRAGMA user_version. A file holds plain DDL, every statement ending in ";" at the end of a line;
// a "--" comment line never ends one. A released file is never edited: each change is a new file,
// and TestReleasedMigrationsAreUnchanged pins every released file by hash.
//
// Every pending file runs inside one transaction, which rules two things out:
//   - VACUUM cannot run inside a transaction, so a migration must not contain one.
//   - PRAGMA foreign_keys=OFF does nothing inside a transaction, and the writer runs with
//     foreign keys on. A table rebuild (create the new table, copy, drop, rename) therefore cannot
//     switch them off; the schema has no foreign keys today, and one added later must account for
//     that.

// MigrationReport describes what Open did to the schema, for the startup log line.
type MigrationReport struct {
	// From and To are the schema versions before and after. They are equal when no migration file
	// ran, which can still mean a write: see Legacy.
	From, To int
	// Legacy reports a table from before versioning, completed to version 1 where it fell short
	// and stamped as such. From is then 1 although user_version was 0.
	Legacy bool
	Backup string        // path of the VACUUM INTO copy, "" when none was taken
	Took   time.Duration // the whole check, backup included
}

// The whole directory is embedded, not just *.sql, so that a misnamed file such as 0002_x.SQL
// reaches the name check in loadMigrations and fails startup rather than being silently left out.
//
//go:embed migrations
var migrationFS embed.FS

// migration is one numbered schema change.
type migration struct {
	version int
	name    string // the file name, which is what an operator can find and read
	sql     string
}

// migrations holds every schema change this binary carries, in version order. A set that does not
// load is reported by Open through migrationsErr, so the gateway refuses to start with a message
// naming the bad file instead of panicking during package initialisation.
var migrations, migrationsErr = loadMigrations(migrationFS)

// SchemaVersion is the highest migration this binary carries.
func SchemaVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

// migrate implements spec §4 steps 1–5 on the writer connection.
//
//  1. A database already at SchemaVersion costs one read and is not written: that is every
//     ordinary restart.
//  2. user_version 0 with a jobs table is a volume from before versioning, which is version 1
//     once its table holds every column 0001 declares (see completeLegacyTable). user_version 0
//     without one is a fresh volume, migrated from nothing and not backed up.
//  3. A version newer than this binary's is refused, because only the newer image knows what it
//     means. Rolling an image back after a migration leaves exactly that behind.
//  4. Otherwise the database is copied with VACUUM INTO, replacing any older copy and installed
//     only once complete, and every pending migration plus the new user_version is applied in one
//     BEGIN IMMEDIATE transaction, after checking under that lock that the version is still the
//     one read. A failure rolls all of it back, and a pod killed partway never commits, so either
//     way the next start finds the old version intact and tries again.
//  5. It returns a report for the startup log line.
//
// All of it runs on one dedicated connection: the transaction is state of a single SQLite
// connection, and the pool would otherwise be free to run each statement on a different one. On
// error the caller must close w, which also rolls back anything a failed ROLLBACK left open.
func migrate(w *sql.DB, dataDir string) (MigrationReport, error) {
	if migrationsErr != nil {
		return MigrationReport{}, migrationsErr
	}
	start := time.Now()
	ctx := context.Background()
	target := SchemaVersion()

	conn, err := w.Conn(ctx)
	if err != nil {
		return MigrationReport{}, fmt.Errorf("store: open migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var seen int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&seen); err != nil {
		return MigrationReport{}, fmt.Errorf("store: read schema version: %w", err)
	}
	from := seen
	legacy := false
	switch {
	case from == target:
		return MigrationReport{From: from, To: target, Took: time.Since(start)}, nil
	case from > target:
		return MigrationReport{}, fmt.Errorf("store: database schema v%d is newer than this "+
			"gateway (v%d); deploy the newer image, or restore %s",
			from, target, backupPath(dataDir, target))
	case from < 0:
		return MigrationReport{}, fmt.Errorf("store: database schema version %d is invalid", from)
	case from == 0:
		if legacy, err = tableExists(ctx, conn, "jobs"); err != nil {
			return MigrationReport{}, err
		}
		if legacy {
			// Before versioning, Open applied its build's schema and never set user_version: what
			// is now migration 0001, or 0001 less a column apply adds back. Either way this volume
			// is at version 1, not empty.
			from = 1
		}
	}

	var pending []migration
	for _, m := range migrations {
		if m.version > from {
			pending = append(pending, m)
		}
	}
	report := MigrationReport{From: from, To: target, Legacy: legacy}
	// The backup protects data a migration is about to change. A fresh volume holds none yet, and
	// bringing a pre-versioning database to version 1 touches no row: it stamps the header and at
	// most adds a column with a default, which leaves every earlier image able to run on it.
	if from > 0 && len(pending) > 0 {
		if report.Backup, err = backup(ctx, conn, dataDir, from); err != nil {
			return MigrationReport{}, err
		}
	}
	p := plan{seen: seen, legacy: legacy, pending: pending, target: target}
	if err := apply(ctx, conn, p); err != nil {
		return MigrationReport{}, err
	}
	report.Took = time.Since(start)
	return report, nil
}

// tableExists reports whether the database holds the named table.
func tableExists(ctx context.Context, conn *sql.Conn, name string) (bool, error) {
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: inspect schema: %w", err)
	}
	return n > 0, nil
}

// backupPath names the copy of a version-v database taken before migrating it. After an image
// rollback, the image that runs version v is the one that can use it.
func backupPath(dataDir string, v int) string {
	return filepath.Join(dataDir, fmt.Sprintf("%s.v%d.bak", dbName, v))
}

// backup copies the database to backupPath(dataDir, from) with VACUUM INTO, which writes one
// consistent snapshot, WAL content included, as a standalone file.
//
// The copy is written under a ".partial" name and renamed into place only once it is complete and
// fsynced. VACUUM INTO builds its output incrementally, and what it has written when the pod is
// killed or the node loses power is still a valid SQLite file: a copy cut short early has no pages
// at all, passes quick_check, and reads as an empty database. Under the backup's own name, an
// operator restoring it would get a gateway that takes it for a fresh volume and silently drops
// every job. A rename is atomic, so gateway.db.v<N>.bak is either complete or absent.
//
// Only the newest copy is kept. Older ones hold jobs long since expired, and VACUUM INTO refuses to
// overwrite a file, which a leftover partial copy or a retry at the same version would otherwise
// hit.
func backup(ctx context.Context, conn *sql.Conn, dataDir string, from int) (_ string, err error) {
	if err := removeBackups(dataDir); err != nil {
		return "", err
	}
	bak := backupPath(dataDir, from)
	partial := bak + ".partial"
	defer func() {
		if err != nil {
			_ = os.Remove(partial)
		}
	}()
	// The path is bound rather than quoted into the SQL, so no DATA_DIR can break out of it.
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, partial); err != nil {
		return "", fmt.Errorf("store: back up database to %s: %w", partial, err)
	}
	// VACUUM INTO does not fsync its output. The content must be on disk before the rename makes it
	// the backup, and the migration's COMMIT that follows is itself durable under synchronous=FULL.
	if err := syncFile(partial); err != nil {
		return "", err
	}
	if err := os.Rename(partial, bak); err != nil {
		return "", fmt.Errorf("store: install backup %s: %w", bak, err)
	}
	if err := syncDir(dataDir); err != nil {
		return "", err
	}
	return bak, nil
}

// backupFile matches exactly the names backup writes: gateway.db.v<N>.bak, and the same with
// ".partial" while a copy is in progress or after one was cut short. Anything else in DATA_DIR,
// such as a copy an operator saved as gateway.db.v1-before-upgrade.bak, is theirs and is left
// alone.
var backupFile = regexp.MustCompile(`^` + regexp.QuoteMeta(dbName) + `\.v[0-9]+\.bak(\.partial)?$`)

// removeBackups deletes every earlier pre-migration copy in dataDir, complete or partial.
func removeBackups(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("store: list old backups: %w", err)
	}
	for _, e := range entries {
		// Names are matched one by one rather than by globbing the joined path, which a DATA_DIR
		// containing glob metacharacters would make match nothing at all.
		name := e.Name()
		if e.IsDir() || !backupFile.MatchString(name) {
			continue
		}
		err := os.Remove(filepath.Join(dataDir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: remove old backup %s: %w", name, err)
		}
	}
	return nil
}

// syncFile fsyncs a file that was written without one, such as the output of VACUUM INTO.
func syncFile(name string) error {
	// Opened for writing because some platforms refuse to flush through a read-only descriptor.
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("store: open %s for sync: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: sync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", name, err)
	}
	return nil
}

// plan is what migrate decided from the version it read before taking the write lock.
type plan struct {
	seen    int  // user_version as migrate read it
	legacy  bool // the database predates versioning
	pending []migration
	target  int
}

// apply runs the pending migrations and stamps the target version, in one transaction. A legacy
// database has its jobs table completed to version 1 first, in the same transaction, so a failure
// anywhere leaves that table as it was found too.
//
// BEGIN IMMEDIATE takes the write lock before the first statement runs, so a competing writer is
// waited out, up to busy_timeout, before anything changes. A deferred transaction would take the
// lock at its first write instead, and in WAL mode that upgrade can fail at once with SQLITE_BUSY,
// busy_timeout or not.
//
// The plan was made from a version read before the lock was taken. Two gateways can start on one
// volume, as a RollingUpdate briefly runs them, and the one that waited for the lock would then
// apply migrations the other has just committed. So the version is read again under the lock, and
// any change stops this start; the restart that follows finds the schema current.
func apply(ctx context.Context, conn *sql.Conn, p plan) (err error) {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if _, rbErr := conn.ExecContext(ctx, `ROLLBACK`); rbErr != nil {
			err = fmt.Errorf("%w (the rollback failed too: %w)", err, rbErr)
		}
	}()
	var now int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&now); err != nil {
		return fmt.Errorf("store: re-read schema version: %w", err)
	}
	if now != p.seen {
		return fmt.Errorf("store: schema version changed from v%d to v%d while this gateway was "+
			"starting; another gateway process is migrating this volume", p.seen, now)
	}
	if p.legacy {
		if err := completeLegacyTable(ctx, conn); err != nil {
			return err
		}
	}
	for _, m := range p.pending {
		for i, stmt := range splitStatements(m.sql) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("store: migration %s, statement %d: %w", m.name, i+1, err)
			}
		}
	}
	// PRAGMA takes no bound parameters; target is an int, so formatting it in is safe.
	stamp := fmt.Sprintf(`PRAGMA user_version = %d`, p.target)
	if _, err := conn.ExecContext(ctx, stamp); err != nil {
		return fmt.Errorf("store: set schema version %d: %w", p.target, err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("store: commit migration: %w", err)
	}
	return nil
}

// completeLegacyTable brings a jobs table from before versioning to what migration 0001 declares,
// columns and indexes, so that stamping it version 1 is true.
//
// Builds before commit ef625fe created the table without prefix, and CREATE TABLE IF NOT EXISTS
// never added it afterwards, so on such a volume every insert, which names prefix, fails. Each
// column the schema gained that way carries a DEFAULT, which is what lets ADD COLUMN append it to
// a table already holding rows. A missing column with no default (id, surface, status and the
// three timestamps, there since the first schema) cannot be added in the form 0001 declares it,
// and no gateway build ever lacked one, so such a table is refused rather than guessed at.
//
// The indexes have been identical in every released schema, but pre-versioning gateways created
// them as separate autocommit statements after the table and re-ran them at every start to make up
// for a crash in between. 0001 is IF NOT EXISTS throughout, so re-running it here keeps that
// repair for a volume whose last pre-versioning start was cut short.
func completeLegacyTable(ctx context.Context, conn *sql.Conn) error {
	want, err := v1Columns(ctx)
	if err != nil {
		return err
	}
	have, err := jobsColumns(ctx, conn)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(have))
	for _, c := range have {
		present[strings.ToLower(c.name)] = true // SQLite column names are case-insensitive
	}
	var add []column
	var missing []string
	for _, c := range want {
		switch {
		case present[strings.ToLower(c.name)]:
		case c.dflt.Valid:
			add = append(add, c)
		default:
			missing = append(missing, c.name)
		}
	}
	if len(missing) > 0 {
		noun := "column"
		if len(missing) > 1 {
			noun = "columns"
		}
		return fmt.Errorf("store: jobs table from before versioning lacks %s %s, which %s declares "+
			"without a default, so it cannot be brought to schema v1",
			noun, strings.Join(missing, ", "), migrations[0].name)
	}
	for _, c := range add {
		if _, err := conn.ExecContext(ctx, c.addStatement()); err != nil {
			return fmt.Errorf("store: add column %s to the legacy jobs table: %w", c.name, err)
		}
	}
	v1 := migrations[0] // loadMigrations guarantees the first is version 1
	for i, stmt := range splitStatements(v1.sql) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: re-run %s on the legacy jobs table, statement %d: %w",
				v1.name, i+1, err)
		}
	}
	return nil
}

// column is one jobs column as PRAGMA table_info describes it.
type column struct {
	name    string
	decl    string // declared type, "" when there is none
	notNull bool
	dflt    sql.NullString // the DEFAULT expression as written; NULL when there is none
}

// addStatement is the ALTER TABLE that adds c to the jobs table as declared. The name, type and
// default come from migration 0001, which this binary carries, so they are formatted in as
// written. For prefix that is
//
//	ALTER TABLE jobs ADD COLUMN prefix TEXT NOT NULL DEFAULT ''
func (c column) addStatement() string {
	stmt := "ALTER TABLE jobs ADD COLUMN " + c.name
	if c.decl != "" {
		stmt += " " + c.decl
	}
	if c.notNull {
		stmt += " NOT NULL"
	}
	return stmt + " DEFAULT " + c.dflt.String
}

// queryer is the part of *sql.DB and *sql.Conn that jobsColumns uses.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// jobsColumns lists the jobs table's columns in declaration order.
func jobsColumns(ctx context.Context, q queryer) ([]column, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(jobs)`)
	if err != nil {
		return nil, fmt.Errorf("store: read jobs columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []column
	for rows.Next() {
		var cid, pk int
		var c column
		if err := rows.Scan(&cid, &c.name, &c.decl, &c.notNull, &c.dflt, &pk); err != nil {
			return nil, fmt.Errorf("store: read jobs columns: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read jobs columns: %w", err)
	}
	return out, nil
}

// v1Columns returns the jobs columns migration 0001 creates, as SQLite itself reads them, by
// applying 0001 to an empty in-memory database. Deriving them keeps 0001 the one place the
// version-1 schema is written down.
func v1Columns(ctx context.Context) ([]column, error) {
	v1 := migrations[0] // loadMigrations guarantees the first is version 1
	scratch, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("store: open scratch database: %w", err)
	}
	defer func() { _ = scratch.Close() }()
	// Every connection to :memory: is a database of its own, so all of this uses one.
	conn, err := scratch.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: open scratch database: %w", err)
	}
	defer func() { _ = conn.Close() }()
	for _, stmt := range splitStatements(v1.sql) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("store: apply %s to a scratch database: %w", v1.name, err)
		}
	}
	return jobsColumns(ctx, conn)
}

// splitStatements cuts a migration file into statements, ending one at each line whose last
// character is ";".
//
// That small grammar is enough because migrations are plain DDL without triggers, so no statement
// has an inner line ending in ";". A "--" comment line never ends a statement, and a piece holding
// nothing but comments and blank lines is dropped.
func splitStatements(src string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); !onlyComments(s) {
			out = append(out, s)
		}
		cur.Reset()
	}
	for _, line := range strings.SplitAfter(src, "\n") {
		cur.WriteString(line)
		if l := strings.TrimSpace(line); strings.HasSuffix(l, ";") && !strings.HasPrefix(l, "--") {
			flush()
		}
	}
	flush()
	return out
}

// onlyComments reports whether s holds nothing but "--" comment lines and blank lines.
func onlyComments(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "--") {
			return false
		}
	}
	return true
}

// migrationFile is the naming contract: a four-digit version, an underscore, a name, ".sql".
var migrationFile = regexp.MustCompile(`^([0-9]{4})_([A-Za-z0-9_-]+)\.sql$`)

// loadMigrations reads every migration under migrations/ in fsys, in version order.
//
// A misnamed, duplicated or missing number is an error, never something to skip: skipping would
// give every volume a schema other than the one this binary was tested against, without a word.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	byVersion := make(map[int]migration, len(entries))
	for _, e := range entries {
		parts := migrationFile.FindStringSubmatch(e.Name())
		if parts == nil || e.IsDir() {
			return nil, fmt.Errorf("store: migration %s is not named NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration %s: %w", e.Name(), err)
		}
		if v == 0 {
			return nil, fmt.Errorf("store: migration %s: versions start at 0001", e.Name())
		}
		if prev, dup := byVersion[v]; dup {
			return nil, fmt.Errorf("store: migrations %s and %s share version %04d",
				prev.name, e.Name(), v)
		}
		body, err := fs.ReadFile(fsys, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		byVersion[v] = migration{version: v, name: e.Name(), sql: string(body)}
	}
	if len(byVersion) == 0 {
		return nil, errors.New("store: no migrations embedded")
	}
	// n distinct versions, none below 1, are free of gaps exactly when they are 1..n.
	out := make([]migration, 0, len(byVersion))
	for v := 1; v <= len(byVersion); v++ {
		m, ok := byVersion[v]
		if !ok {
			return nil, fmt.Errorf("store: migration %04d is missing; versions must run from 0001 "+
				"without gaps", v)
		}
		out = append(out, m)
	}
	return out, nil
}
