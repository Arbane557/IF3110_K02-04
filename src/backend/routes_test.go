package main

import (
	"net/http/httptest"
	"testing"

	"wbd/backend/auth"
	"wbd/backend/database"
	"wbd/backend/migration"
)

func TestRoutesRejectUnauthenticatedRequests(t *testing.T) {
	app := &App{Auth: auth.NewHandler(nil, nil), Database: database.NewHandler(nil, nil, nil, nil), Migration: migration.NewHandler(nil, nil, nil)}
	routes := app.routes()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/auth/users", 401}, {"POST", "/api/auth/register", 401},
		{"PUT", "/api/auth/users/1/admin", 401}, {"GET", "/api/auth/groups", 401},
		{"POST", "/api/auth/groups", 401}, {"GET", "/api/auth/groups/1", 401},
		{"POST", "/api/auth/groups/1/members", 401}, {"DELETE", "/api/auth/groups/1/members/1", 401},
		{"PUT", "/api/auth/groups/1/databases/1", 401},
		{"GET", "/api/databases", 401}, {"POST", "/api/databases", 401},
		{"GET", "/api/databases/1", 401}, {"PUT", "/api/databases/1", 401}, {"DELETE", "/api/databases/1", 401},
		{"POST", "/api/databases/1/test-connection", 401}, {"GET", "/api/databases/1/schemas", 401},
		{"GET", "/api/databases/1/schemas/public/tables", 401},
		{"GET", "/api/databases/1/schemas/public/tables/items", 401},
		{"GET", "/api/databases/1/schemas/public/tables/items/rows", 401},
		{"GET", "/api/databases/1/migrations", 401}, {"POST", "/api/databases/1/migrations", 401},
		{"GET", "/api/databases/1/migrations/1", 401}, {"PUT", "/api/databases/1/migrations/1", 401},
		{"POST", "/api/databases/1/migrations/explain", 401}, {"POST", "/api/databases/1/migrations/1/execute", 401},
		{"GET", "/whoami", 401}, {"GET", "/databases", 303}, {"GET", "/migrations", 303},
		{"GET", "/auth", 401}, {"GET", "/groups", 401}, {"GET", "/groups/1", 401},
		{"POST", "/login", 403}, {"POST", "/logout", 403},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}
