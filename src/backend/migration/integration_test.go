//go:build integration

package migration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"wbd/backend/auth"
	"wbd/backend/database"
	"wbd/backend/internal/testutil"
)

type integrationPool struct{ pool *pgxpool.Pool }

func (p integrationPool) Pool(context.Context, database.DatabaseID) (*pgxpool.Pool, error) {
	return p.pool, nil
}

func TestMigrationLifecycleAndHistoryIntegration(t *testing.T) {
	service, target := asyncFixture(t)
	ctx := context.Background()
	empty, err := service.List(ctx, 1)
	if err != nil || len(empty) != 0 {
		t.Fatalf("fresh history=%+v error=%v", empty, err)
	}
	first, err := service.Create(ctx, 1, 1, `CREATE TABLE committed (id INTEGER PRIMARY KEY)`)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.Status != "draft" || first.CreatedBy != 1 || first.CreatedAt.IsZero() {
		t.Fatalf("draft=%+v", first)
	}
	job, err := service.SubmitJob(ctx, 1, first.Version, 1, "up", BatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(ctx, 1, first.Version, "SELECT 1"); !errors.Is(err, ErrMigrationLocked) {
		t.Fatalf("update queued migration error=%v", err)
	}
	if err := service.processJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	requireJobStatus(t, service, job.ID, "success")
	executed, err := service.Get(ctx, 1, first.Version)
	if err != nil || executed.Status != "success" || !executed.Active || executed.ExecutedBy == nil || *executed.ExecutedBy != 1 || executed.ExecutedAt == nil {
		t.Fatalf("execution=%+v", executed)
	}
	// A successful return alone does not establish that DDL was committed.
	var count int
	if err := target.Pool.QueryRow(ctx, `SELECT count(*) FROM committed`).Scan(&count); err != nil {
		t.Fatal("successful DDL was not committed")
	}
	if _, err := service.Update(ctx, 1, first.Version, "SELECT 1"); !errors.Is(err, ErrMigrationLocked) {
		t.Fatalf("update executed migration error=%v", err)
	}
	// Duplicate delivery must not execute already committed SQL again.
	if err := service.processJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	failed, err := service.Create(ctx, 1, 1, `CREATE TABLE rolled_back (id INTEGER); INSERT INTO committed VALUES (1); SELECT * FROM missing_relation`)
	if err != nil {
		t.Fatal(err)
	}
	failedJob, err := service.SubmitJob(ctx, 1, failed.Version, 1, "up", BatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.processJob(ctx, failedJob.ID); err != nil {
		t.Fatal(err)
	}
	requireJobStatus(t, service, failedJob.ID, "failed")
	var exists bool
	if err := target.Pool.QueryRow(ctx, `SELECT to_regclass('rolled_back') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("failed migration left its DDL behind")
	}
	if err := target.Pool.QueryRow(ctx, `SELECT count(*) FROM committed`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration left inserted data behind")
	}
	history, err := service.ListHistory(ctx, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Version != 2 || history[0].Status != "failed" || history[0].ErrorMessage == "" || history[0].ExecutedAt == nil || history[0].UserID != 1 || history[0].SQL != failed.SQL || history[1].Version != 1 || history[1].Status != "success" || history[1].SQL != first.SQL {
		t.Fatalf("persisted history=%+v", history)
	}
	if _, err := service.Get(ctx, 1, 999); !errors.Is(err, ErrMigrationNotFound) {
		t.Fatalf("missing migration error=%v", err)
	}
}

func TestMigrationDraftUpdateIntegration(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	_, err := fixture.SQL.Exec(`
        INSERT INTO users(id,username,password_hash) VALUES (7,'creator','test'),(8,'executor','test');
        INSERT INTO managed_databases(id,name,host,port,database_name,username,password_ciphertext)
        VALUES(1,'target','localhost',5432,'test','test',decode('00','hex'))`)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(fixture.SQL, integrationPool{fixture.Pool})
	ctx := context.Background()
	draft, err := service.Create(ctx, 1, 7, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(ctx, 1, draft.Version, "SELECT 2")
	if err != nil {
		t.Fatalf("update draft with NULL execution metadata: %v", err)
	}
	persisted, err := service.Get(ctx, 1, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SQL != "SELECT 2" || persisted.SQL != "SELECT 2" || persisted.Status != "draft" || persisted.ExecutedBy != nil || persisted.ErrorMessage != "" {
		t.Fatalf("updated draft=%+v persisted=%+v", updated, persisted)
	}
}

func TestMigrationExplainDoesNotExecuteIntegration(t *testing.T) {
	fixture := testutil.NewDatabase(t, false)
	ctx := context.Background()
	if _, err := fixture.Pool.Exec(ctx, `CREATE TABLE items (id INTEGER); INSERT INTO items VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	service := NewService(fixture.SQL, integrationPool{fixture.Pool})
	plan, err := service.Explain(ctx, 1, "DELETE FROM items")
	if err != nil || !strings.Contains(plan, "Delete on items") {
		t.Fatalf("plan=%q error=%v", plan, err)
	}
	var count int
	if err := fixture.Pool.QueryRow(ctx, "SELECT count(*) FROM items").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("EXPLAIN executed the DELETE")
	}
	if _, err := service.Explain(ctx, 1, "not valid SQL"); err == nil {
		t.Fatal("invalid SQL accepted")
	}
}

func TestMigrationAccessLevelsIntegration(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	ctx := context.Background()
	protector, err := database.NewAESGCMProtector([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	accounts := auth.NewService(fixture.SQL, protector)
	user, err := accounts.CreateUser(ctx, "member", "password123")
	if err != nil {
		t.Fatal(err)
	}
	group, err := accounts.CreateGroup(ctx, "team", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.AddGroupMember(ctx, group.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.SQL.Exec(`INSERT INTO managed_databases (id,name,host,port,database_name,username,password_ciphertext) VALUES (1,'target','localhost',5432,'target','app','\x00')`); err != nil {
		t.Fatal(err)
	}
	token, _, err := accounts.CreateSession(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(fixture.SQL, integrationPool{fixture.Pool})
	handler := NewHandler(nil, service, auth.NewAccessService(fixture.SQL))
	authHandler := auth.NewHandler(accounts, nil)
	mux := http.NewServeMux()
	mux.Handle("POST /api/databases/{databaseID}/migrations", authHandler.RequireAuthAPI(authHandler.RequireCSRF(http.HandlerFunc(handler.CreateMigration))))
	mux.Handle("GET /api/databases/{databaseID}/migrations", authHandler.RequireAuthAPI(http.HandlerFunc(handler.ListMigrations)))
	mux.Handle("GET /api/databases/{databaseID}/migrations/history", authHandler.RequireAuthAPI(http.HandlerFunc(handler.ListHistory)))
	mux.Handle("GET /api/databases/{databaseID}/jobs", authHandler.RequireAuthAPI(http.HandlerFunc(handler.Jobs)))
	for path, action := range map[string]http.HandlerFunc{
		"/api/databases/{databaseID}/migrations/{version}/execute":  handler.ExecuteMigration,
		"/api/databases/{databaseID}/migrations/{version}/rollback": handler.RollbackMigration,
		"/api/databases/{databaseID}/migrations/{version}/forward":  handler.ForwardMigration,
		"/api/databases/{databaseID}/jobs/{jobID}/resume":           handler.ResumeJob,
		"/api/databases/{databaseID}/jobs/{jobID}/cancel":           handler.CancelJob,
	} {
		mux.Handle("POST "+path, authHandler.RequireAuthAPI(authHandler.RequireCSRF(action)))
	}
	request := func(method, url string, authenticated, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(`{"sql":"SELECT 1"}`))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.AddCookie(&http.Cookie{Name: "lemonmeloncookie", Value: token})
		}
		if csrf {
			req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "test-csrf"})
			req.Header.Set("X-CSRF-Token", "test-csrf")
		}
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		return response
	}
	for _, tc := range []struct {
		name                string
		authenticated, csrf bool
		want                int
	}{
		{"anonymous", false, true, 401}, {"missing CSRF", true, false, 403}, {"no grant", true, true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := request("POST", "/api/databases/1/migrations", tc.authenticated, tc.csrf); got.Code != tc.want {
				t.Fatalf("status=%d want=%d", got.Code, tc.want)
			}
		})
	}
	if err := accounts.SetGroupDatabaseAccess(ctx, group.ID, 1, "view"); err != nil {
		t.Fatal(err)
	}
	if got := request("GET", "/api/databases/1/migrations", true, false); got.Code != 200 {
		t.Fatalf("view history status=%d", got.Code)
	}
	if got := request("POST", "/api/databases/1/migrations", true, true); got.Code != 403 {
		t.Fatalf("view create status=%d", got.Code)
	}
	for _, path := range []string{"/api/databases/1/migrations/history", "/api/databases/1/jobs"} {
		if got := request("GET", path, true, false); got.Code != 200 {
			t.Fatalf("viewer read %s: status=%d", path, got.Code)
		}
	}
	for _, path := range []string{
		"/api/databases/1/migrations/1/execute", "/api/databases/1/migrations/1/rollback",
		"/api/databases/1/migrations/1/forward", "/api/databases/1/jobs/1/resume", "/api/databases/1/jobs/1/cancel",
	} {
		if got := request("POST", path, true, true); got.Code != 403 {
			t.Fatalf("viewer mutation %s: status=%d", path, got.Code)
		}
	}
	jobs, err := service.ListJobs(ctx, 1)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("denied requests changed jobs=%+v error=%v", jobs, err)
	}
	before, err := service.List(ctx, 1)
	if err != nil || len(before) != 0 {
		t.Fatal("denied requests changed history")
	}
	if err := accounts.SetGroupDatabaseAccess(ctx, group.ID, 1, "edit"); err != nil {
		t.Fatal(err)
	}
	if got := request("POST", "/api/databases/2/migrations", true, true); got.Code != 403 {
		t.Fatalf("other database status=%d", got.Code)
	}
	response := request("POST", "/api/databases/1/migrations", true, true)
	if response.Code != 201 {
		t.Fatalf("edit create status=%d body=%s", response.Code, response.Body.String())
	}
	var created Migration
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	actual, err := service.Get(ctx, 1, created.Version)
	if err != nil || actual.SQL != "SELECT 1" || actual.CreatedBy != user.ID || actual.Status != "draft" {
		t.Fatalf("persisted migration=%+v error=%v", actual, err)
	}
}
