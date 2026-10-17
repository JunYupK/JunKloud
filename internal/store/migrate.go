package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads migrations/NNN_name.sql in version order.
func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, convErr := strconv.Atoi(prefix)
		if !ok || convErr != nil || v < 1 {
			return nil, fmt.Errorf("store: migration %q must be named NNN_name.sql", e.Name())
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migration versions must be contiguous from 1, found %q at position %d", m.name, i+1)
		}
	}
	return ms, nil
}

// migrate brings the schema to the newest version in one immediate
// transaction, so two processes starting together cannot both apply it. A
// database whose recorded version is newer than this program knows is
// refused untouched.
func migrate(ctx context.Context, db *sql.DB) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL)`); err != nil {
		return classify(fmt.Errorf("store: create migration table: %w", err))
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return classify(fmt.Errorf("store: read schema version: %w", err))
	}
	if current > len(ms) {
		return fmt.Errorf("%w: database is at version %d, this program knows %d", ErrSchemaTooNew, current, len(ms))
	}
	for _, m := range ms[current:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("store: apply %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, encodeTime(time.Now())); err != nil {
			return fmt.Errorf("store: record %s: %w", m.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration: %w", err)
	}
	return nil
}
