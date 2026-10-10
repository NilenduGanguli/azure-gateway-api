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
// and TestSchemaFromScratchEqualsMigrated fails otherwise.

// MigrationReport describes what Open did to the schema, for the startup log line.
type MigrationReport struct {
	From, To int           // schema versions before and after; equal when nothing ran
	Backup   string        // path of the VACUUM INTO copy, "" when none was taken
	Took     time.Duration // the whole check, backup included
}

//go:embed migrations/*.sql
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
//  2. user_version 0 with a jobs table is a volume from before versioning, which is version 1.
//     user_version 0 without one is a fresh volume, migrated from nothing and not backed up.
//  3. A version newer than this binary's is refused, because only the newer image knows what it
//     means. Rolling an image back after a migration leaves exactly that behind.
//  4. Otherwise the database is copied with VACUUM INTO, replacing any older copy, and every
//     pending migration plus the new user_version is applied in one BEGIN IMMEDIATE transaction.
//     A failure rolls all of it back, and a pod killed partway never commits, so either way the
//     next start finds the old version intact and tries again.
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

	var from int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&from); err != nil {
		return MigrationReport{}, fmt.Errorf("store: read schema version: %w", err)
	}
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
		legacy, err := tableExists(ctx, conn, "jobs")
		if err != nil {
			return MigrationReport{}, err
		}
		if legacy {
			// Before versioning, Open applied what is now migration 0001 and never set
			// user_version, so this volume is at version 1, not empty.
			from = 1
		}
	}

	var pending []migration
	for _, m := range migrations {
		if m.version > from {
			pending = append(pending, m)
		}
	}
	report := MigrationReport{From: from, To: target}
	// The backup protects data a migration is about to change. A fresh volume holds none yet, and
	// stamping a pre-versioning database as version 1 rewrites only the header.
	if from > 0 && len(pending) > 0 {
		if report.Backup, err = backup(ctx, conn, dataDir, from); err != nil {
			return MigrationReport{}, err
		}
	}
	if err := apply(ctx, conn, pending, target); err != nil {
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
// Only the newest copy is kept. Older ones hold jobs long since expired, and VACUUM INTO refuses to
// overwrite a file, which a retry at the same version after a failed migration would otherwise hit.
func backup(ctx context.Context, conn *sql.Conn, dataDir string, from int) (_ string, err error) {
	if err := removeBackups(dataDir); err != nil {
		return "", err
	}
	bak := backupPath(dataDir, from)
	defer func() {
		if err != nil {
			// A partial copy must not be left where an operator would take it for a good one.
			_ = os.Remove(bak)
		}
	}()
	// The path is bound rather than quoted into the SQL, so no DATA_DIR can break out of it.
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, bak); err != nil {
		return "", fmt.Errorf("store: back up database to %s: %w", bak, err)
	}
	// VACUUM INTO does not fsync its output. The migration's COMMIT is durable under
	// synchronous=FULL, so without this a power loss just after it could leave the only way back to
	// the old schema incomplete.
	if err := syncFile(bak); err != nil {
		return "", err
	}
	if err := syncDir(dataDir); err != nil {
		return "", err
	}
	return bak, nil
}

// removeBackups deletes every earlier pre-migration copy in dataDir, that is gateway.db.v*.bak.
func removeBackups(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("store: list old backups: %w", err)
	}
	for _, e := range entries {
		// Names are matched one by one rather than by globbing the joined path, which a DATA_DIR
		// containing glob metacharacters would make match nothing at all.
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, dbName+".v") || !strings.HasSuffix(name, ".bak") {
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

// apply runs the pending migrations and stamps the target version, in one transaction.
//
// BEGIN IMMEDIATE takes the write lock before the first statement runs, so a competing writer is
// waited out, up to busy_timeout, before anything changes. A deferred transaction would take the
// lock at its first write instead, and in WAL mode that upgrade can fail at once with SQLITE_BUSY,
// busy_timeout or not.
func apply(ctx context.Context, conn *sql.Conn, pending []migration, target int) (err error) {
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
	for _, m := range pending {
		for i, stmt := range splitStatements(m.sql) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("store: migration %s, statement %d: %w", m.name, i+1, err)
			}
		}
	}
	// PRAGMA takes no bound parameters; target is an int, so formatting it in is safe.
	stamp := fmt.Sprintf(`PRAGMA user_version = %d`, target)
	if _, err := conn.ExecContext(ctx, stamp); err != nil {
		return fmt.Errorf("store: set schema version %d: %w", target, err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("store: commit migration: %w", err)
	}
	return nil
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
