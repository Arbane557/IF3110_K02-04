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
)

type failingPools struct {
	calls int
	err   error
}

func (p *failingPools) Pool(context.Context, database.DatabaseID) (*pgxpool.Pool, error) {
	p.calls++
	return nil, p.err
}

func TestMigrationServiceRejectsInvalidInputBeforeConnecting(t *testing.T) {
	pools := &failingPools{err: errors.New("must not connect")}
	service := NewService(nil, pools)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func() error
		want error
	}{
		{"create blank SQL", func() error { _, err := service.Create(ctx, 1, 1, " \n\t"); return err }, ErrInvalidSQL},
		{"update invalid version", func() error { _, err := service.Update(ctx, 1, 0, "SELECT 1"); return err }, ErrInvalidVersion},
		{"update blank SQL", func() error { _, err := service.Update(ctx, 1, 1, ""); return err }, ErrInvalidSQL},
		{"get negative version", func() error { _, err := service.Get(ctx, 1, -1); return err }, ErrInvalidVersion},
		{"execute invalid version", func() error { _, err := service.Execute(ctx, 1, 0, 1); return err }, ErrInvalidVersion},
		{"execute invalid actor", func() error { _, err := service.Execute(ctx, 1, 1, 0); return err }, ErrInvalidVersion},
		{"rollback invalid version", func() error { _, err := service.Rollback(ctx, 1, 0, 1); return err }, ErrInvalidVersion},
		{"forward invalid version", func() error { _, err := service.Forward(ctx, 1, 0, 1); return err }, ErrInvalidVersion},
		{"explain blank SQL", func() error { _, err := service.Explain(ctx, 1, ""); return err }, ErrInvalidSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
	if pools.calls != 0 {
		t.Fatalf("invalid input opened %d connections", pools.calls)
	}
}

type recordingAuthorizer struct {
	principalErr, permissionErr error
	calls                       int
	action                      string
	id                          int64
}

func (a *recordingAuthorizer) PrincipalFromRequest(*http.Request) (auth.Principal, error) {
	return auth.Principal{UserID: 7}, a.principalErr
}
func (a *recordingAuthorizer) AuthorizeDatabase(_ context.Context, _ auth.Principal, id int64, action string) error {
	a.calls++
	a.id = id
	a.action = action
	return a.permissionErr
}

func TestMigrationHandlersCheckResourceAndAction(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, action string
		handler                    func(*Handler) http.HandlerFunc
	}{
		{"list", "GET", "/api/databases/{databaseID}/migrations", auth.ActionView, func(h *Handler) http.HandlerFunc { return h.ListMigrations }},
		{"detail", "GET", "/api/databases/{databaseID}/migrations/{version}", auth.ActionView, func(h *Handler) http.HandlerFunc { return h.GetMigration }},
		{"create", "POST", "/api/databases/{databaseID}/migrations", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.CreateMigration }},
		{"update", "PUT", "/api/databases/{databaseID}/migrations/{version}", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.UpdateMigration }},
		{"execute", "POST", "/api/databases/{databaseID}/migrations/{version}/execute", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.ExecuteMigration }},
		{"rollback", "POST", "/api/databases/{databaseID}/migrations/{version}/rollback", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.RollbackMigration }},
		{"forward", "POST", "/api/databases/{databaseID}/migrations/{version}/forward", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.ForwardMigration }},
		{"history", "GET", "/api/databases/{databaseID}/migrations/history", auth.ActionView, func(h *Handler) http.HandlerFunc { return h.ListHistory }},
		{"jobs", "GET", "/api/databases/{databaseID}/jobs", auth.ActionView, func(h *Handler) http.HandlerFunc { return h.Jobs }},
		{"job", "GET", "/api/databases/{databaseID}/jobs/{jobID}", auth.ActionView, func(h *Handler) http.HandlerFunc { return h.Job }},
		{"approve", "POST", "/api/databases/{databaseID}/jobs/{jobID}/approve", auth.ActionView, func(h *Handler) http.HandlerFunc { return h.ApproveJob }},
		{"resume", "POST", "/api/databases/{databaseID}/jobs/{jobID}/resume", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.ResumeJob }},
		{"cancel", "POST", "/api/databases/{databaseID}/jobs/{jobID}/cancel", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.CancelJob }},
		{"explain", "POST", "/api/databases/{databaseID}/migrations/explain", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.ExplainMigration }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := &recordingAuthorizer{permissionErr: auth.ErrForbidden}
			pools := &failingPools{err: errors.New("must not connect")}
			handler := NewHandler(nil, NewService(nil, pools), authorizer)
			mux := http.NewServeMux()
			mux.HandleFunc(tc.method+" "+tc.path, tc.handler(handler))
			url := strings.NewReplacer("{databaseID}", "42", "{version}", "1", "{jobID}", "9").Replace(tc.path)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(tc.method, url, strings.NewReader(`{"sql":"SELECT 1"}`)))
			if response.Code != 403 || authorizer.calls != 1 || authorizer.id != 42 || authorizer.action != tc.action || pools.calls != 0 {
				t.Fatalf("status=%d permission calls=%d id=%d action=%s DB calls=%d", response.Code, authorizer.calls, authorizer.id, authorizer.action, pools.calls)
			}
		})
	}
}

func TestMigrationErrorsAreMappedToJSON(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{ErrInvalidSQL, 400}, {ErrInvalidVersion, 400}, {ErrMigrationNotFound, 404}, {ErrMigrationLocked, 409}, {ErrMigrationFailed, 422}, {database.ErrDatabaseUnavailable, 502}, {errors.New("private diagnostic"), 500},
		{ErrIrreversible, 409}, {ErrNotLatestActive, 409}, {ErrForwardNotNext, 409},
		{errJobInput, 400}, {errJobNotFound, 404}, {errJobConflict, 409}, {auth.ErrForbidden, 403},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			response := httptest.NewRecorder()
			writeError(response, tc.err)
			var body errorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.want || body.Error == "" || response.Header().Get("Content-Type") != "application/json" || strings.Contains(body.Error, "private diagnostic") {
				t.Fatalf("status=%d body=%+v", response.Code, body)
			}
		})
	}
}

func TestJobSQLValidation(t *testing.T) {
	batch := BatchOptions{Mode: "batch", Table: "public.items", Size: 100}
	up := "UPDATE public.items SET value=value+1 WHERE id = ANY($1::bigint[])"
	down := "UPDATE public.items SET value=value-1 WHERE id = ANY($1::bigint[])"
	for _, tc := range []struct {
		name, query, down string
		opts              BatchOptions
		invalid           bool
	}{
		{"normal script", "CREATE TABLE items(id bigint); INSERT INTO items VALUES(1)", "", BatchOptions{Mode: "normal"}, false},
		{"quoted semicolon", "SELECT 'a;b'; SELECT $$c;d$$", "", BatchOptions{Mode: "normal"}, false},
		{"empty", " ", "", BatchOptions{Mode: "normal"}, true},
		{"comments only", "/* no SQL */ -- ignored", "", BatchOptions{Mode: "normal"}, true},
		{"transaction control", "UPDATE items SET value=1; COMMIT", "", BatchOptions{Mode: "normal"}, true},
		{"unterminated quote", "SELECT 'secret", "", BatchOptions{Mode: "normal"}, true},
		{"batch", up, down, batch, false},
		{"batch without compensation", up, "", batch, false},
		{"batch missing scope", "UPDATE public.items SET value=1", down, batch, true},
		{"down missing scope", up, "UPDATE public.items SET value=0", batch, true},
		{"multiple batch statements", up+"; SELECT 1", down, batch, true},
		{"invalid table", up, down, BatchOptions{"batch", "items;DROP TABLE users", 100}, true},
		{"zero size", up, down, BatchOptions{"batch", "public.items", 0}, true},
		{"oversized batch", up, down, BatchOptions{"batch", "public.items", 10001}, true},
		{"unknown mode", up, down, BatchOptions{"unknown", "public.items", 100}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateJobSQL(tc.query, tc.down, "up", tc.opts)
			if tc.invalid && !errors.Is(err, errJobInput) || !tc.invalid && err != nil {
				t.Fatalf("error=%v invalid=%v", err, tc.invalid)
			}
		})
	}
}

func TestSQLScriptSplittingAndLogRedaction(t *testing.T) {
	query := "/* outer /* nested */ comment */ SELECT 'private;value'; SELECT $$private;body$$; SELECT \"semi;column\" FROM items -- private comment\n;"
	statements, err := splitMigrationSQL(query)
	if err != nil || len(statements) != 3 {
		t.Fatalf("statements=%d error=%v", len(statements), err)
	}
	if !strings.Contains(statements[0].sql, "'private;value'") || !strings.Contains(statements[1].sql, "$$private;body$$") {
		t.Fatal("execution SQL lost its original literals")
	}
	logged := sqlForLog(query)
	if strings.Contains(logged, "private") || !strings.Contains(logged, `"semi;column"`) {
		t.Fatal("SQL log exposed a literal/comment or lost its identifier")
	}
	for _, query := range []string{"SELECT 'unfinished", "SELECT $$unfinished", "SELECT 1 /* unfinished"} {
		if _, err := splitMigrationSQL(query); !errors.Is(err, errJobInput) {
			t.Fatalf("malformed SQL error=%v", err)
		}
		if sqlForLog(query) != "[SQL withheld]" {
			t.Fatal("malformed SQL was logged")
		}
	}
}

func TestExecuteHandlerRejectsInvalidJobOptionsBeforeStoreAccess(t *testing.T) {
	for _, body := range []string{`{`, `{"unexpected":true}`, `{} {}`, `{"batchSize":"large"}`} {
		t.Run(body, func(t *testing.T) {
			h := NewHandler(nil, NewService(nil, nil), &recordingAuthorizer{})
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/databases/{databaseID}/migrations/{version}/execute", h.ExecuteMigration)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest("POST", "/api/databases/1/migrations/1/execute", strings.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
