package main

import "net/http"

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", app.healthHandler)

	mux.HandleFunc("POST /login", app.login)
	mux.HandleFunc("POST /logout", app.logout)
	mux.HandleFunc("GET /whoami", app.whoami)
	// mux.HandleFunc("GET /databases")

	// mux.HandleFunc("GET /databases/{id}")
	// mux.HandleFunc("GET /databases/{id}/schema")

	// mux.HandleFunc("GET /databases/{id}/migrations")
	// mux.HandleFunc("POST /databases/{id}/migrations")

	// mux.HandlerFunc("GET /users")
}

func (app *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}