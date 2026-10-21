package main

import "net/http"

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", app.healthHandler)
}

func (app *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}