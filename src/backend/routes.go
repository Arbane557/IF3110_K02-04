package main

import (
	"encoding/json"
	"net/http"
)

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /health", app.healthHandler)
	mux.HandleFunc("GET /login", app.Auth.AuthPage)
	mux.HandleFunc("POST /login", app.Auth.Login)
	mux.HandleFunc("POST /logout", app.Auth.Logout)
	mux.HandleFunc("GET /whoami", app.Auth.Whoami)
	mux.Handle("GET /auth", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.UsersPage)))
	mux.Handle(
		"POST /api/auth/register",
		app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.RegisterUser)),
	)
	mux.Handle(
		"GET /databases",
		app.Auth.RequireAuth(http.HandlerFunc(app.DatabasePage.DatabasePage)),
	)
	mux.Handle("GET /migrations", app.Auth.RequireAuth(http.HandlerFunc(app.Migration.MigrationPage)))
	app.registerDatabaseRoutes(mux)

	mux.Handle("GET /static/", http.StripPrefix(
		"/static/",
		http.FileServer(http.Dir("static")),
	))

	return mux
}

func (app *App) registerDatabaseRoutes(mux *http.ServeMux) {
	if app.DatabaseAPI == nil {
		return
	}
	mux.HandleFunc("GET /api/databases", app.DatabaseAPI.ListDatabases)
	mux.HandleFunc("POST /api/databases", app.DatabaseAPI.CreateDatabase)
	mux.HandleFunc("GET /api/databases/{databaseID}", app.DatabaseAPI.GetDatabase)
	mux.HandleFunc("PUT /api/databases/{databaseID}", app.DatabaseAPI.UpdateDatabase)
	mux.HandleFunc("DELETE /api/databases/{databaseID}", app.DatabaseAPI.DeleteDatabase)
	mux.HandleFunc("POST /api/databases/{databaseID}/test-connection", app.DatabaseAPI.TestConnection)
	mux.HandleFunc("GET /api/databases/{databaseID}/schemas", app.DatabaseAPI.ListSchemas)
	mux.HandleFunc("GET /api/databases/{databaseID}/schemas/{schema}/tables", app.DatabaseAPI.ListTables)
	mux.HandleFunc("GET /api/databases/{databaseID}/schemas/{schema}/tables/{table}", app.DatabaseAPI.GetTableDetails)
	mux.HandleFunc("GET /api/databases/{databaseID}/schemas/{schema}/tables/{table}/rows", app.DatabaseAPI.GetRows)
}

func (app *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

