package migration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"wbd/backend/database"

	"github.com/jackc/pgx/v5/pgxpool"
)

type integrationPool struct {
	pool *pgxpool.Pool
}

func (p integrationPool) Pool(context.Context, database.DatabaseID) (*pgxpool.Pool, error) {
	return p.pool, nil
}

type integrationAuthorizer struct {
	edit bool
}

func (a integrationAuthorizer) PrincipalFromRequest(*http.Request) (database.Principal, error) {
	return database.Principal{UserID: 7}, nil
}

func (a integrationAuthorizer) AuthorizeDatabase(context.Context, database.Principal, database.DatabaseID, string) error {
	if !a.edit {
		return database.ErrForbidden
	}
	return nil
}

func openIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create integration pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping integration database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrationLifecycleAndHistory(t *testing.T) {
	pool := openIntegrationPool(t)
	service := NewService(integrationPool{pool: pool})
	ctx := context.Background()
	name := "migration_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := ensureTable(ctx, pool); err != nil {
		t.Fatalf("create migration history: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE platform_migrations`); err != nil {
		t.Fatalf("reset migration history: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+name)
	})

	first, err := service.Create(ctx, 1, 7, `CREATE TABLE `+name+` (id INTEGER PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	if first.Version != 1 || first.Status != "draft" {
		t.Fatalf("unexpected first migration: %+v", first)
	}
	if _, err := service.Execute(ctx, 1, first.Version, 7); err != nil {
		t.Fatalf("execute successful migration: %v", err)
	}

	failed, err := service.Create(ctx, 1, 7, `SELECT * FROM table_that_does_not_exist`)
	if err != nil {
		t.Fatalf("create failing migration: %v", err)
	}
	if _, err := service.Execute(ctx, 1, failed.Version, 7); !errors.Is(err, ErrMigrationFailed) {
		t.Fatalf("expected migration failure, got %v", err)
	}

	history, err := service.List(ctx, 1)
	if err != nil {
		t.Fatalf("list migration history: %v", err)
	}
	if len(history) != 2 || history[0].Version != 1 || history[0].Status != "success" ||
		history[1].Version != 2 || history[1].Status != "failed" {
		t.Fatalf("unexpected migration history: %+v", history)
	}
	var tableExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&tableExists); err != nil {
		t.Fatalf("check committed table: %v", err)
	}
	if !tableExists {
		t.Fatal("successful migration was not committed")
	}
}

func TestMigrationAccessLevels(t *testing.T) {
	pool := openIntegrationPool(t)
	service := NewService(integrationPool{pool: pool})
	sqlText := fmt.Sprintf("CREATE TABLE migration_access_%d (id INTEGER)", time.Now().UnixNano())

	viewOnly := NewHandlerWithAPI(nil, service, integrationAuthorizer{edit: false})
	mux := http.NewServeMux()
	viewOnly.RegisterRoutes(mux)
	request := httptest.NewRequest(http.MethodPost, "/api/databases/1/migrations", strings.NewReader(fmt.Sprintf(`{"sql":%q}`, sqlText)))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("View Only create status = %d, want %d", response.Code, http.StatusForbidden)
	}

	viewEdit := NewHandlerWithAPI(nil, service, integrationAuthorizer{edit: true})
	mux = http.NewServeMux()
	viewEdit.RegisterRoutes(mux)
	request = httptest.NewRequest(http.MethodPost, "/api/databases/1/migrations", strings.NewReader(fmt.Sprintf(`{"sql":%q}`, sqlText)))
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("View + Edit create status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body.String())
	}
}
