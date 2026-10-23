package main

import "net/http"

func (app *App) databasesPage(w http.ResponseWriter, r *http.Request) {
	if err := app.Templates.ExecuteTemplate(w, "databases.html", nil); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
