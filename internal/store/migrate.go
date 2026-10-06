package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// advisoryLockKey serialises concurrent migrators (e.g. two replicas starting together).
const advisoryLockKey = 0x5167_6e61_6c4c_6162 // "SignalLab"-ish; any constant works

// Migrate applies every embedded migration that has not been applied yet, in
// filename order. Each file runs in its own transaction together with its
// schema_migrations row, so a failed migration leaves no partial state.
// It returns the versions it applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(advisoryLockKey)); err != nil {
		return nil, fmt.Errorf("migrate: advisory lock: %w", err)
	}
	// Unlock with a fresh context so a cancelled ctx cannot leave the lock held on a pooled connection.
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(advisoryLockKey)) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var applied []string
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			return applied, fmt.Errorf("migrate: check %s: %w", version, err)
		}
		if exists {
			continue
		}
		sqlText, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return applied, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, fmt.Errorf("migrate: begin %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(sqlText)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("migrate: apply %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("migrate: record %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, fmt.Errorf("migrate: commit %s: %w", version, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}
