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
		{"explain", "POST", "/api/databases/{databaseID}/migrations/explain", auth.ActionEdit, func(h *Handler) http.HandlerFunc { return h.ExplainMigration }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := &recordingAuthorizer{permissionErr: auth.ErrForbidden}
			pools := &failingPools{err: errors.New("must not connect")}
			handler := NewHandler(nil, NewService(nil, pools), authorizer)
			mux := http.NewServeMux()
			mux.HandleFunc(tc.method+" "+tc.path, tc.handler(handler))
			url := strings.ReplaceAll(strings.ReplaceAll(tc.path, "{databaseID}", "42"), "{version}", "1")
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
