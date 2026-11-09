//go:build integration

// Package testutil provides isolated PostgreSQL fixtures for integration tests.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

type Database struct {
	SQL    *sql.DB
	Pool   *pgxpool.Pool
	Schema string
}

// NewDatabase creates a private schema. All connections use that schema only,
// preventing unqualified queries from falling back to application/public tables.
// With platform=true, fixtures load the actual production init SQL.
func NewDatabase(t *testing.T, platform bool) *Database {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Fatal("integration tests require INTEGRATION_DATABASE_URL pointing to a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect integration database (check URL and PostgreSQL availability)")
	}
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	schema := "wbd_test_" + hex.EncodeToString(suffix[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("cleanup schema: %v", err)
		}
		if err := admin.Close(cleanup); err != nil {
			t.Errorf("close fixture connection: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse integration database configuration")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("create fixture pool")
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("ping fixture pool")
	}
	db := stdlib.OpenDB(*config.ConnConfig)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fixture database: %v", err)
		}
	})
	fixture := &Database{SQL: db, Pool: pool, Schema: schema}
	if platform {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("locate production init SQL")
		}
		paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "..", "..", "db", "init", "*.sql"))
		if err != nil || len(paths) == 0 {
			t.Fatal("production init SQL not found")
		}
		for _, path := range paths {
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read init SQL: %v", err)
			}
			if _, err := pool.Exec(ctx, string(contents)); err != nil {
				t.Fatalf("apply %s: %v", filepath.Base(path), err)
			}
		}
	}
	return fixture
}
