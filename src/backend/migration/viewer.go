package migration

import (
	"html/template"
	"net/http"
)

// PageViewer renders the migration page scaffold.
type Viewer struct {
	templates *template.Template
}

func NewViewer(templates *template.Template) *Viewer {
	return &Viewer{templates: templates}
}

func (v *Viewer) MigrationPage(w http.ResponseWriter, r *http.Request) {
	if err := v.templates.ExecuteTemplate(w, "migrations.html", nil); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
