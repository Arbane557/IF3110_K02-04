package migration

import (
	"html/template"
	"net/http"
)

// Handler serves migration pages and migration API requests.
type Handler struct {
	templates  *template.Template
	service    *Service
	authorizer Authorizer
}

func NewHandler(templates *template.Template, service *Service, authorizer Authorizer) *Handler {
	return &Handler{templates: templates, service: service, authorizer: authorizer}
}

func (h *Handler) MigrationPage(w http.ResponseWriter, r *http.Request) {
	if err := h.templates.ExecuteTemplate(w, "migrations.html", nil); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
