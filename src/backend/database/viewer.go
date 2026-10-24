package database

import (
	"html/template"
	"net/http"
)

// Viewer renders database HTML pages.
type Viewer struct {
	templates *template.Template
}

func NewViewer(templates *template.Template) *Viewer {
	return &Viewer{templates: templates}
}

func (v *Viewer) DatabasePage(w http.ResponseWriter, r *http.Request) {
	if err := v.templates.ExecuteTemplate(w, "databases.html", nil); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
