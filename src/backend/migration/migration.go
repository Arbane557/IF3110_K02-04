package migration

import (
	"net/http"
)

// Handler connects migration routes to the migration page viewer.
type Handler struct {
	viewer *Viewer
}

func NewHandler(viewer *Viewer) *Handler {
	return &Handler{viewer: viewer}
}

func (h *Handler) MigrationPage(w http.ResponseWriter, r *http.Request) {
	h.viewer.MigrationPage(w, r)
}
