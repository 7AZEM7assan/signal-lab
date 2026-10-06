// Package testdb gives integration tests an isolated, freshly migrated PostgreSQL schema.
//
// Tests never touch a developer's data: each call creates a uniquely named schema,
// points the pool's search_path at it, applies the real migrations, and drops the
// schema on cleanup. Set SIGNALLAB_TEST_DATABASE_URL to enable; if it is unset the
// test is skipped, unless SIGNALLAB_REQUIRE_DB=1 (used in CI), which makes it fail.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"signallab/internal/store"
)

func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SIGNALLAB_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("SIGNALLAB_REQUIRE_DB") == "1" {
			t.Fatal("SIGNALLAB_TEST_DATABASE_URL is required (SIGNALLAB_REQUIRE_DB=1)")
		}
		t.Skip("SIGNALLAB_TEST_DATABASE_URL not set; skipping database integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "t_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { // registered first, so it runs after the pool is closed
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
