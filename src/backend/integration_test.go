//go:build integration

package main

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wbd/backend/auth"
	"wbd/backend/database"
	"wbd/backend/internal/testutil"
	"wbd/backend/migration"
)

func TestLoginAdminAndCSRFRoutesIntegration(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	ctx := context.Background()
	protector, err := database.NewAESGCMProtector([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	accounts := auth.NewService(fixture.SQL, protector)
	admin, err := accounts.CreateUser(ctx, "admin", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.MakeUserAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	templates, err := template.ParseGlob("templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	repository := database.NewPostgresRegistryRepository(fixture.SQL)
	connections := database.NewConnectionManager(repository, protector)
	t.Cleanup(connections.Close)
	access := auth.NewAccessService(fixture.SQL)
	app := &App{
		Auth:      auth.NewHandler(accounts, templates),
		Database:  database.NewHandler(database.NewRegistryService(repository, connections, protector), database.NewCatalogService(connections), access, templates),
		Migration: migration.NewHandler(templates, migration.NewService(fixture.SQL, connections), access),
	}
	routes := app.routes()
	loginPage := httptest.NewRecorder()
	routes.ServeHTTP(loginPage, httptest.NewRequest("GET", "/login", nil))
	if loginPage.Code != 200 {
		t.Fatalf("login page status=%d", loginPage.Code)
	}
	var csrf *http.Cookie
	for _, cookie := range loginPage.Result().Cookies() {
		if cookie.Name == "csrf_token" {
			csrf = cookie
		}
	}
	if csrf == nil {
		t.Fatal("login page did not issue CSRF cookie")
	}
	login := func(password string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/login", strings.NewReader("username=admin&password="+password))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrf)
		req.Header.Set("X-CSRF-Token", csrf.Value)
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req)
		return response
	}
	if response := login("incorrect"); response.Code != 401 {
		t.Fatalf("wrong password status=%d", response.Code)
	}
	var count int
	if err := fixture.SQL.QueryRow("SELECT count(*) FROM sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("wrong password created a session")
	}
	loggedIn := login("password123")
	if loggedIn.Code != 303 || loggedIn.Header().Get("Location") != "/auth" {
		t.Fatalf("admin login status=%d", loggedIn.Code)
	}
	var session *http.Cookie
	for _, cookie := range loggedIn.Result().Cookies() {
		if cookie.Name == "lemonmeloncookie" {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("login did not create session cookie")
	}
	request := func(method, path, body string, withCSRF bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(session)
		req.Header.Set("Content-Type", "application/json")
		if withCSRF {
			req.AddCookie(csrf)
			req.Header.Set("X-CSRF-Token", csrf.Value)
		}
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req)
		return response
	}
	whoami := request("GET", "/whoami", "", false)
	var identity auth.User
	if err := json.Unmarshal(whoami.Body.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	if whoami.Code != 200 || identity.ID != admin.ID || identity.Role != auth.RoleAdmin {
		t.Fatalf("identity=%+v status=%d", identity, whoami.Code)
	}
	body := `{"username":"new-user","password":"password456"}`
	if response := request("POST", "/api/auth/register", body, false); response.Code != 403 {
		t.Fatalf("missing CSRF status=%d", response.Code)
	}
	if err := fixture.SQL.QueryRow("SELECT count(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("CSRF-denied request created a user")
	}
	created := request("POST", "/api/auth/register", body, true)
	if created.Code != 201 || strings.Contains(created.Body.String(), "password456") {
		t.Fatalf("register status=%d", created.Code)
	}
	user, _, err := accounts.FindUserByUsername(ctx, "new-user")
	if err != nil || user.Role != auth.RoleUser {
		t.Fatalf("persisted user=%+v error=%v", user, err)
	}
	if response := request("POST", "/logout", "", true); response.Code != 303 {
		t.Fatalf("logout status=%d", response.Code)
	}
	if response := request("GET", "/whoami", "", false); response.Code != 401 {
		t.Fatalf("revoked session still accepted: %d", response.Code)
	}
}
