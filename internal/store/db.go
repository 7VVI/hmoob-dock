// Package store owns the SQLite persistence of the control端.
//
// The write pool keeps a single connection so that writes are serialized and
// never collide with each other. Queries run on a separate read-only pool
// whose connections are short-lived, so a slow query cannot block the writer.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const (
	// busyTimeoutMs bounds how long a statement waits for a lock held by
	// another connection before it fails.
	busyTimeoutMs = 5000
	// readPoolSize is the number of connections in the read-only pool.
	readPoolSize = 4
	// readPoolIdleTime is how long an idle read connection is kept around.
	readPoolIdleTime = 30 * time.Second
	// readPoolLifetime caps the lifetime of a read connection.
	readPoolLifetime = 5 * time.Minute
	// The two connection kinds understood by dsn.
	writeMode = false
	readMode  = true
)

// Open opens the database at path, applies every pending migration and
// returns the write pool. A failure to open or to migrate is returned to the
// caller, so a broken database stops the control端 instead of starting with
// an unknown schema.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, writeMode))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// A single connection serializes the writes and removes any chance of
	// a write transaction deadlocking against another one.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect %s: %w", path, err)
	}
	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: read the embedded migrations: %w", err)
	}
	if err := migrate(db, migrations); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenReader opens a read-only pool over the database at path. The
// connections reject writes, which keeps accidental mutations out of the
// query path.
func OpenReader(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, readMode))
	if err != nil {
		return nil, fmt.Errorf("store: open read pool for %s: %w", path, err)
	}
	db.SetMaxOpenConns(readPoolSize)
	db.SetMaxIdleConns(readPoolSize)
	db.SetConnMaxIdleTime(readPoolIdleTime)
	db.SetConnMaxLifetime(readPoolLifetime)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect read pool for %s: %w", path, err)
	}
	return db, nil
}

// dsn builds the driver connection string for the given database path.
func dsn(path string, readOnly bool) string {
	params := []string{
		fmt.Sprintf("_pragma=busy_timeout(%d)", busyTimeoutMs),
		"_pragma=foreign_keys(1)",
	}
	if readOnly {
		// The journal mode is persisted in the database file, so the read
		// pool only needs to refuse writes.
		params = append(params, "_query_only=1")
	} else {
		params = append(params,
			"_pragma=journal_mode(WAL)",
			"_pragma=synchronous(NORMAL)",
		)
	}
	return filepath.ToSlash(path) + "?" + strings.Join(params, "&")
}

// migration is a single numbered schema change.
type migration struct {
	version int
	name    string
	body    string
}

// migrate applies the migrations of fsys that are not recorded yet, in
// version order. The root of fsys holds the numbered SQL files. Each
// migration runs in its own transaction, so a failing migration leaves
// neither a half applied schema nor a version record.
func migrate(db *sql.DB, fsys fs.FS) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	applied, err := appliedVersions(db)
	if err != nil {
		return err
	}
	migrations, err := listMigrations(fsys)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return err
		}
	}
	return nil
}

// appliedVersions reads the versions that were applied before.
func appliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	return applied, nil
}

// listMigrations collects the numbered SQL files of fsys in version order.
func listMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{
			version: version,
			name:    entry.Name(),
			body:    string(body),
		})
	}
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	return migrations, nil
}

// migrationVersion reads the version number from a migration file name such
// as 001_core.sql.
func migrationVersion(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	digits := base
	if index := strings.IndexAny(base, "_-."); index >= 0 {
		digits = base[:index]
	}
	version, err := strconv.Atoi(digits)
	if err != nil {
		return 0, fmt.Errorf("store: migration %s has no version number: %w", name, err)
	}
	if version <= 0 {
		return 0, fmt.Errorf("store: migration %s has an invalid version number", name)
	}
	return version, nil
}

// applyMigration runs a single migration inside a transaction.
func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: start migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(m.body); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", m.name, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, m.version); err != nil {
		return fmt.Errorf("store: record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", m.name, err)
	}
	return nil
}
