package migration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"wbd/backend/database"
)

type Authorizer interface {
	PrincipalFromRequest(r *http.Request) (database.Principal, error)
	AuthorizeDatabase(ctx context.Context, principal database.Principal, databaseID database.DatabaseID, action string) error
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/databases/{databaseID}/migrations", h.list)
	mux.HandleFunc("POST /api/databases/{databaseID}/migrations", h.create)
	mux.HandleFunc("GET /api/databases/{databaseID}/migrations/{version}", h.get)
	mux.HandleFunc("PUT /api/databases/{databaseID}/migrations/{version}", h.update)
	mux.HandleFunc("POST /api/databases/{databaseID}/migrations/{version}/execute", h.execute)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.authorize(w, r, ActionView)
	if !ok {
		return
	}
	items, err := h.service.List(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.authorize(w, r, ActionView)
	if !ok {
		return
	}
	version, ok := parseVersion(w, r)
	if !ok {
		return
	}
	item, err := h.service.Get(r.Context(), id, version)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	id, principal, ok := h.authorize(w, r, ActionEdit)
	if !ok {
		return
	}
	request, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	item, err := h.service.Create(r.Context(), id, principal.UserID, request.SQL)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.authorize(w, r, ActionEdit)
	if !ok {
		return
	}
	version, ok := parseVersion(w, r)
	if !ok {
		return
	}
	request, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	item, err := h.service.Update(r.Context(), id, version, request.SQL)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) execute(w http.ResponseWriter, r *http.Request) {
	id, principal, ok := h.authorize(w, r, ActionEdit)
	if !ok {
		return
	}
	version, ok := parseVersion(w, r)
	if !ok {
		return
	}
	item, err := h.service.Execute(r.Context(), id, version, principal.UserID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, action string) (database.DatabaseID, database.Principal, bool) {
	value, err := strconv.ParseInt(r.PathValue("databaseID"), 10, 64)
	if err != nil || value <= 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid database ID"})
		return 0, database.Principal{}, false
	}
	principal, err := h.authorizer.PrincipalFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return 0, database.Principal{}, false
	}
	if err := h.authorizer.AuthorizeDatabase(r.Context(), principal, database.DatabaseID(value), action); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, database.ErrUnauthorized) {
			status = http.StatusUnauthorized
		} else if errors.Is(err, database.ErrForbidden) {
			status = http.StatusForbidden
		}
		writeJSON(w, status, errorResponse{Error: http.StatusText(status)})
		return 0, database.Principal{}, false
	}
	return database.DatabaseID(value), principal, true
}

func parseVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	value, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || value <= 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid migration version"})
		return 0, false
	}
	return value, true
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (CreateRequest, bool) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var request CreateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
		return CreateRequest{}, false
	}
	return request, true
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal server error"
	switch {
	case errors.Is(err, ErrInvalidSQL), errors.Is(err, ErrInvalidVersion):
		status, message = http.StatusBadRequest, "invalid migration"
	case errors.Is(err, ErrMigrationNotFound):
		status, message = http.StatusNotFound, "migration not found"
	case errors.Is(err, ErrMigrationLocked):
		status, message = http.StatusConflict, "migration cannot be changed"
	case errors.Is(err, ErrMigrationFailed):
		status, message = http.StatusUnprocessableEntity, "migration execution failed"
	case errors.Is(err, database.ErrDatabaseUnavailable):
		status, message = http.StatusBadGateway, "target database unavailable"
	}
	writeJSON(w, status, errorResponse{Error: message})
}
