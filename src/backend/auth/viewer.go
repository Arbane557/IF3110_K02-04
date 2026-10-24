package auth

import (
	"html/template"
	"net/http"
)

type LoginPageData struct {
	Error string
}

// Viewer renders authentication HTML pages.
type Viewer struct {
	templates *template.Template
}

func NewViewer(templates *template.Template) *Viewer {
	return &Viewer{templates: templates}
}

func (v *Viewer) RenderLogin(w http.ResponseWriter, data LoginPageData) error {
	return v.templates.ExecuteTemplate(w, "login.html", data)
}

func (v *Viewer) RenderUsers(w http.ResponseWriter) error {
	return v.templates.ExecuteTemplate(w, "users.html", nil)
}
