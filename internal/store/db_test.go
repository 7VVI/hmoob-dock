package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAppliesCoreMigration(t *testing.T) {
	db := openTestDB(t)

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_migrations WHERE version = 1`).Scan(&version); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema_migrations version = %d, want 1", version)
	}

	if _, err := db.Exec(
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result) VALUES ('evt-1', 1700000000000, 'login', 'admin', 'success')`,
	); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
}

func TestOpenPersistsDataAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result) VALUES ('evt-1', 1700000000000, 'login', 'admin', 'success')`,
	); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	var count int
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE id = 'evt-1'`).Scan(&count); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit events = %d, want 1", count)
	}
}

func TestOpenDoesNotReapplyAppliedMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	wanted := countMigrations(t)

	for attempt := 1; attempt <= 3; attempt++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open attempt %d: %v", attempt, err)
		}
		var applied int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
			t.Fatalf("count schema_migrations: %v", err)
		}
		if applied != wanted {
			t.Fatalf("applied migrations = %d, want %d", applied, wanted)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close attempt %d: %v", attempt, err)
		}
	}
}

// countMigrations returns the number of the migration files of the package.
func countMigrations(t *testing.T) int {
	t.Helper()

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	return len(entries)
}

func TestOpenFailsWhenMigrationCannotBeApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE audit_events (id TEXT PRIMARY KEY, unexpected TEXT)`); err != nil {
		t.Fatalf("create conflicting table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	db, err := Open(path)
	if err == nil {
		db.Close()
		t.Fatal("Open succeeded even though the migration failed")
	}

	inspect, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw database: %v", err)
	}
	defer inspect.Close()
	var applied int
	if err := inspect.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if applied != 0 {
		t.Fatalf("failed migration recorded as applied (%d rows)", applied)
	}
}

func TestMigrationRollsBackOnFailure(t *testing.T) {
	db := openTestDB(t)

	fsys := fstest.MapFS{
		"900_broken.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE staged (id TEXT PRIMARY KEY);\n" +
				"INSERT INTO missing_table (id) VALUES ('x');\n",
		)},
	}
	if err := migrate(db, fsys); err == nil {
		t.Fatal("migrate succeeded despite a broken migration")
	}

	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'staged'`).Scan(&name)
	if err == nil {
		t.Fatal("partial migration was committed")
	}
	if err != sql.ErrNoRows {
		t.Fatalf("unexpected error while inspecting the schema: %v", err)
	}

	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 900`).Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if applied != 0 {
		t.Fatalf("failed migration recorded as applied (%d rows)", applied)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.Exec(`CREATE TABLE parent (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := db.Exec(
		`CREATE TABLE child (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL REFERENCES parent(id))`,
	); err != nil {
		t.Fatalf("create child: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO child (id, parent_id) VALUES ('child-1', 'missing')`); err == nil {
		t.Fatal("foreign key violation accepted")
	}
}

func TestConnectionPragmas(t *testing.T) {
	db := openTestDB(t)

	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}

	var busyTimeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout <= 0 {
		t.Fatalf("busy_timeout = %d, want a positive value", busyTimeout)
	}
}

func TestOpenReaderIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result) VALUES ('evt-1', 1700000000000, 'login', 'admin', 'success')`,
	); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}

	reader, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()

	var count int
	if err := reader.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatalf("read through the read pool: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit events = %d, want 1", count)
	}

	if _, err := reader.Exec(
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result) VALUES ('evt-2', 1700000000000, 'login', 'admin', 'success')`,
	); err == nil {
		t.Fatal("the read pool accepted a write")
	}
}

func TestOpenFailsForUnusablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-directory", "control.db")
	if _, err := Open(path); err == nil {
		t.Fatal("Open succeeded for a database inside a missing directory")
	}
}

// TestMigrateRefusesTwoMigrationsWithTheSameVersion proves that two
// migrations with the same version number are an error and not a migration
// that is silently skipped.
func TestMigrateRefusesTwoMigrationsWithTheSameVersion(t *testing.T) {
	db := openTestDB(t)

	fsys := fstest.MapFS{
		"901_first.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE first_table (id TEXT PRIMARY KEY);\n")},
		"901_second.sql": &fstest.MapFile{Data: []byte("CREATE TABLE second_table (id TEXT PRIMARY KEY);\n")},
	}
	err := migrate(db, fsys)
	if err == nil {
		t.Fatal("migrate accepted two migrations with the same version number")
	}
	if !strings.Contains(err.Error(), "901") {
		t.Fatalf("the error = %q, want the version number", err)
	}

	// The refusal happens before any migration runs, so the schema of the
	// database does not hold the table of the first migration.
	var name string
	query := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'first_table'`)
	scanErr := query.Scan(&name)
	if scanErr == nil {
		t.Fatal("a migration ran even though the version numbers were not unique")
	}
	if scanErr != sql.ErrNoRows {
		t.Fatalf("unexpected error while inspecting the schema: %v", scanErr)
	}
}
