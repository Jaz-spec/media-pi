// Package state owns all persistent state in SQLite.
// The daemon is the only writer; the TUI opens read-only.
package state

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps *sql.DB with project-specific helpers.
type DB struct {
	*sql.DB
	path     string
	readOnly bool
}

// Open opens (and if needed creates) the SQLite file at path. WAL mode is
// enabled on the writer side so readers don't block writers. The read-only
// flag opens with mode=ro and no migrations are run (caller beware: schema
// must already exist).
func Open(path string, readOnly bool) (*DB, error) {
	var dsn string
	if readOnly {
		// mode=ro + immutable=0 + _pragma=busy_timeout lets us read while
		// the writer has a WAL transaction open.
		dsn = fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path)
	} else {
		// journal_mode=WAL, foreign_keys=ON, busy_timeout=5s.
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	}
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection for WAL-mode writers avoids "database is locked" under
	// mixed read/write load. Readers can be pooled normally.
	if !readOnly {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	return &DB{DB: sqlDB, path: path, readOnly: readOnly}, nil
}

// Migrate runs any unapplied migrations in lexical filename order. Idempotent.
// Each migration file is one SQL statement block; we track applied filenames
// in a tiny schema_migrations table.
func (db *DB) Migrate(ctx context.Context) error {
	if db.readOnly {
		return fmt.Errorf("cannot migrate a read-only DB")
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename    TEXT PRIMARY KEY,
			applied_at  INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM schema_migrations WHERE filename = ?`, name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := db.execTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
				return fmt.Errorf("apply %s: %w", name, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations(filename) VALUES (?)`, name,
			); err != nil {
				return fmt.Errorf("record %s: %w", name, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) execTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
