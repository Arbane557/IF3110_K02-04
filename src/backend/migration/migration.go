package migration

import (
	"net/http"
)

// Handler connects migration routes to the migration page viewer.
type Handler struct {
	viewer     *Viewer
	service    *Service
	authorizer Authorizer
}

func NewHandler(viewer *Viewer) *Handler {
	return &Handler{viewer: viewer}
}

func NewAPIHandler(service *Service, authorizer Authorizer) *Handler {
	return &Handler{service: service, authorizer: authorizer}
}

func (h *Handler) MigrationPage(w http.ResponseWriter, r *http.Request) {
	h.viewer.MigrationPage(w, r)
}
