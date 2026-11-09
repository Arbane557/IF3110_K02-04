//go:build integration

package database

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"wbd/backend/internal/testutil"
)

type integrationPoolProvider struct{ pool *pgxpool.Pool }

func (p integrationPoolProvider) Pool(context.Context, DatabaseID) (*pgxpool.Pool, error) {
	return p.pool, nil
}

func TestRegistryRepositoryAndConnectionManagerIntegration(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	internalDB := fixture.SQL
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")

	config := configFromDSN(t, dsn)
	config.Name = "integration-target"
	repository := NewPostgresRegistryRepository(internalDB)
	protector, err := NewAESGCMProtector([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("create protector: %v", err)
	}
	manager := NewConnectionManager(repository, protector)
	t.Cleanup(manager.Close)
	service := NewRegistryService(repository, manager, protector)
	created, err := service.Create(context.Background(), config)
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	stored, err := repository.FindByID(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	reused, err := manager.Pool(context.Background(), created.ID)
	if err != nil || reused != pool {
		t.Fatal("connection pool was not reused")
	}
	config.ID = created.ID
	config.Name = "renamed-target"
	config.Password = ""
	updated, err := service.Update(context.Background(), config)
	if err != nil || updated.Name != config.Name {
		t.Fatalf("update registry: %v", err)
	}
	reopened, err := manager.Pool(context.Background(), created.ID)
	if err != nil || reopened == pool {
		t.Fatalf("update did not replace cached pool: %v", err)
	}
	if err := reopened.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, err := service.List(context.Background())
	if err != nil || len(items) != 1 || items[0].Name != config.Name {
		t.Fatalf("registry list=%+v error=%v", items, err)
	}
	if err := service.Delete(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FindByID(context.Background(), created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted record error=%v", err)
	}
	if _, err := manager.Pool(context.Background(), created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted pool error=%v", err)
	}
}

func TestCatalogIntegration(t *testing.T) {
	fixture := testutil.NewDatabase(t, false)
	pool := fixture.Pool
	ctx := context.Background()
	statements := []string{

		`CREATE TABLE users (id BIGINT PRIMARY KEY, email TEXT NOT NULL UNIQUE, score INTEGER NOT NULL DEFAULT 0 CHECK (score >= 0))`,
		`CREATE TABLE orders (id BIGINT PRIMARY KEY, user_id BIGINT NOT NULL REFERENCES users(id), amount INTEGER NOT NULL)`,
		`CREATE INDEX orders_amount_idx ON orders(amount)`,
		`INSERT INTO users VALUES (1, 'a@example.test', 1), (2, 'b@example.test', 2)`,
		`INSERT INTO orders VALUES (1, 1, 10), (2, 2, 20)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create catalog fixture: %v", err)
		}
	}

	catalog := NewCatalogService(integrationPoolProvider{pool})
	schemas, err := catalog.ListSchemas(ctx, 1)
	if err != nil || !hasSchema(schemas, fixture.Schema) {
		t.Fatalf("schemas = %+v, %v", schemas, err)
	}
	tables, err := catalog.ListTables(ctx, 1, fixture.Schema)
	if err != nil || !hasTable(tables, "users") || !hasTable(tables, "orders") {
		t.Fatalf("tables = %+v, %v", tables, err)
	}
	details, err := catalog.GetTableDetails(ctx, 1, fixture.Schema, "orders")
	if err != nil || len(details.Columns) != 3 || !hasConstraint(details.Constraints, "FOREIGN KEY") || !hasIndex(details.Indexes, "orders_amount_idx") {
		t.Fatalf("details = %+v, %v", details, err)
	}
	rows, err := catalog.GetRows(ctx, 1, fixture.Schema, "users", Page{Number: 2, Size: 1})
	if err != nil || len(rows.Rows) != 1 || rows.Page.Number != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if _, err := catalog.ListTables(ctx, 1, "does_not_exist"); err != ErrInvalidIdentifier {
		t.Fatalf("invalid schema error = %v", err)
	}
	users, err := catalog.GetTableDetails(ctx, 1, fixture.Schema, "users")
	if err != nil {
		t.Fatal(err)
	}
	if len(users.Columns) != 3 || users.Columns[0].Name != "id" || users.Columns[0].DataType != "bigint" || users.Columns[0].Nullable || users.Columns[2].Default == nil || *users.Columns[2].Default != "0" || !hasConstraint(users.Constraints, "PRIMARY KEY") || !hasConstraint(users.Constraints, "UNIQUE") || !hasConstraint(users.Constraints, "CHECK") {
		t.Fatalf("incorrect column/constraint metadata: %+v", users)
	}
	all, err := catalog.GetRows(ctx, 1, fixture.Schema, "users", Page{Number: 1, Size: 50})
	if err != nil || len(all.Rows) != 2 || len(all.Columns) != 3 {
		t.Fatalf("rows=%+v error=%v", all, err)
	}
	seen := map[string]bool{}
	for _, row := range all.Rows {
		email, ok := row["email"].(string)
		if !ok {
			t.Fatal("email is not text")
		}
		seen[email] = true
	}
	if !seen["a@example.test"] || !seen["b@example.test"] {
		t.Fatalf("incorrect row values: %+v", all.Rows)
	}
	beyond, err := catalog.GetRows(ctx, 1, fixture.Schema, "users", Page{Number: 3, Size: 1})
	if err != nil || len(beyond.Rows) != 0 {
		t.Fatalf("offset past end returned rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users SELECT n, n::text || '@example.test', n FROM generate_series(3, 120) n`); err != nil {
		t.Fatal(err)
	}
	capped, err := catalog.GetRows(ctx, 1, fixture.Schema, "users", Page{Number: 1, Size: 1000})
	if err != nil || capped.Page.Size != 100 || len(capped.Rows) != 100 {
		t.Fatalf("page cap not enforced: size=%d rows=%d error=%v", capped.Page.Size, len(capped.Rows), err)
	}
	if _, err := catalog.GetRows(ctx, 1, fixture.Schema, "users; DROP TABLE users", Page{}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("invalid identifier error=%v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE "odd""table" ("odd column" INTEGER); INSERT INTO "odd""table" VALUES (42)`); err != nil {
		t.Fatal(err)
	}
	odd, err := catalog.GetRows(ctx, 1, fixture.Schema, "odd\"table", Page{})
	if err != nil || len(odd.Rows) != 1 || odd.Rows[0]["odd column"] != int32(42) {
		t.Fatalf("quoted identifier rows=%+v error=%v", odd, err)
	}
}

func configFromDSN(t *testing.T, dsn string) DatabaseConfig {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid integration DSN (expected a PostgreSQL TCP URL)")
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
