package database

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type integrationPoolProvider struct{ pool *pgxpool.Pool }

func (p integrationPoolProvider) Pool(context.Context, DatabaseID) (*pgxpool.Pool, error) {
	return p.pool, nil
}

func openDatabaseIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open integration pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping integration database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func prepareRegistryTable(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS managed_databases`); err != nil {
		t.Fatalf("drop registry table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE managed_databases (
			id BIGSERIAL PRIMARY KEY, name VARCHAR(100) NOT NULL UNIQUE,
			host TEXT NOT NULL, port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
			database_name TEXT NOT NULL, username TEXT NOT NULL, ssl_mode TEXT NOT NULL,
			password_ciphertext BYTEA NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		t.Fatalf("create registry table: %v", err)
	}
}

func TestRegistryRepositoryAndConnectionManagerIntegration(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL is not set")
	}
	internalDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open internal database: %v", err)
	}
	t.Cleanup(func() { _ = internalDB.Close() })
	prepareRegistryTable(t, internalDB)

	config := configFromDSN(t, dsn)
	config.Name = "integration-target"
	repository := NewPostgresRegistryRepository(internalDB)
	protector, err := NewAESGCMProtector([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil { t.Fatalf("create protector: %v", err) }
	manager := NewConnectionManager(repository, protector)
	service := NewRegistryService(repository, manager, protector)
	created, err := service.Create(context.Background(), config)
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	stored, err := repository.FindByID(context.Background(), created.ID)
	password, decryptErr := protector.Open(stored.PasswordCiphertext)
	if err != nil || decryptErr != nil || stored.Config.Name != config.Name || password != config.Password {
		t.Fatalf("stored registry is invalid: %+v, %v", stored.Config, err)
	}
	pool, err := manager.Pool(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("open target pool: %v", err)
	}
	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query target pool: %d, %v", one, err)
	}
	manager.Invalidate(created.ID)
}

func TestCatalogIntegration(t *testing.T) {
	pool := openDatabaseIntegrationPool(t)
	ctx := context.Background()
	statements := []string{
		`DROP SCHEMA IF EXISTS database_integration CASCADE`,
		`CREATE SCHEMA database_integration`,
		`CREATE TABLE database_integration.users (id BIGINT PRIMARY KEY, email TEXT NOT NULL UNIQUE, score INTEGER NOT NULL CHECK (score >= 0))`,
		`CREATE TABLE database_integration.orders (id BIGINT PRIMARY KEY, user_id BIGINT NOT NULL REFERENCES database_integration.users(id), amount INTEGER NOT NULL)`,
		`CREATE INDEX orders_amount_idx ON database_integration.orders(amount)`,
		`INSERT INTO database_integration.users VALUES (1, 'a@example.test', 1), (2, 'b@example.test', 2)`,
		`INSERT INTO database_integration.orders VALUES (1, 1, 10), (2, 2, 20)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create catalog fixture: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS database_integration CASCADE`) })

	catalog := NewCatalogService(integrationPoolProvider{pool})
	schemas, err := catalog.ListSchemas(ctx, 1)
	if err != nil || !hasSchema(schemas, "database_integration") {
		t.Fatalf("schemas = %+v, %v", schemas, err)
	}
	tables, err := catalog.ListTables(ctx, 1, "database_integration")
	if err != nil || !hasTable(tables, "users") || !hasTable(tables, "orders") {
		t.Fatalf("tables = %+v, %v", tables, err)
	}
	details, err := catalog.GetTableDetails(ctx, 1, "database_integration", "orders")
	if err != nil || len(details.Columns) != 3 || !hasConstraint(details.Constraints, "FOREIGN KEY") || !hasIndex(details.Indexes, "orders_amount_idx") {
		t.Fatalf("details = %+v, %v", details, err)
	}
	rows, err := catalog.GetRows(ctx, 1, "database_integration", "users", Page{Number: 2, Size: 1})
	if err != nil || len(rows.Rows) != 1 || rows.Page.Number != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if _, err := catalog.ListTables(ctx, 1, "does_not_exist"); err != ErrInvalidIdentifier {
		t.Fatalf("invalid schema error = %v", err)
	}
}

func configFromDSN(t *testing.T, dsn string) DatabaseConfig {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse integration DSN: %v", err)
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		t.Fatalf("parse integration port: %v", err)
	}
	password, _ := u.User.Password()
	return DatabaseConfig{Name: "target", Host: u.Hostname(), Port: uint16(port), DatabaseName: strings.TrimPrefix(u.Path, "/"), Username: u.User.Username(), Password: password, SSLMode: u.Query().Get("sslmode")}
}

func hasSchema(values []Schema, name string) bool {
	for _, value := range values {
		if value.Name == name {
			return true
		}
	}
	return false
}
func hasTable(values []Table, name string) bool {
	for _, value := range values {
		if value.Name == name {
			return true
		}
	}
	return false
}
func hasConstraint(values []Constraint, kind string) bool {
	for _, value := range values {
		if value.Type == kind {
			return true
		}
	}
	return false
}
func hasIndex(values []Index, name string) bool {
	for _, value := range values {
		if value.Name == name {
			return true
		}
	}
	return false
}
