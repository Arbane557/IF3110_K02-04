package main

import "net/http"

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /health", app.healthHandler)
	mux.HandleFunc("GET /login", app.loginPage)
	mux.HandleFunc("POST /login", app.login)
	mux.HandleFunc("POST /logout", app.logout)
	mux.HandleFunc("GET /whoami", app.whoami)
	mux.Handle(
		"GET /databases",
		app.requireAuth(http.HandlerFunc(app.databasesPage)),
	)
	mux.Handle("GET /static/", http.StripPrefix(
		"/static/",
		http.FileServer(http.Dir("static")),
	))

	return mux
}

func (app *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}
