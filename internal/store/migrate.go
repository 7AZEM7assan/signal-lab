package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
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

	versions, err := embeddedVersions()
	if err != nil {
		return nil, err
	}

	var applied []string
	for _, version := range versions {
		name := version + ".sql"
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

// undefinedTable is PostgreSQL's SQLSTATE for a missing relation.
const undefinedTable = "42P01"

// embeddedVersions lists every embedded migration version in filename order.
func embeddedVersions() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			versions = append(versions, strings.TrimSuffix(e.Name(), ".sql"))
		}
	}
	sort.Strings(versions)
	return versions, nil
}

// PendingMigrations lists embedded migrations that schema_migrations does not
// record, in filename order. A missing schema_migrations table means nothing
// has been applied, so every version is pending rather than an error: an
// un-migrated database is the case this is here to report.
//
// One indexed query over a table with one row per migration, cheap enough for
// a readiness probe.
func PendingMigrations(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	want, err := embeddedVersions()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations WHERE version = ANY($1)`, want)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			return want, nil
		}
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool, len(want))
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("migrate: scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}

	var pending []string
	for _, v := range want {
		if !applied[v] {
			pending = append(pending, v)
		}
	}
	return pending, nil
}
