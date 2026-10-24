package database

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

type Handler struct {
	registry    *RegistryService
	catalog     *CatalogService
	currentRole func(*http.Request) (string, error)
}

func NewHandler(registry *RegistryService, catalog *CatalogService, currentRole func(*http.Request) (string, error)) *Handler {
	return &Handler{registry: registry, catalog: catalog, currentRole: currentRole}
}

func (h *Handler) ListDatabases(w http.ResponseWriter, r *http.Request) {
	role, err := h.currentRole(r)
	if err != nil {
		writeServiceError(w, ErrUnauthorized)
		return
	}
	items, err := h.registry.ListByRole(r.Context(), role)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) CreateDatabase(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	config, ok := decodeConfig(w, r)
	if !ok {
		return
	}
	created, err := h.registry.Create(r.Context(), config)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) GetDatabase(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, ok := pathDatabaseID(w, r)
	if !ok {
		return
	}
	config, err := h.registry.Get(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (h *Handler) UpdateDatabase(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, ok := pathDatabaseID(w, r)
	if !ok {
		return
	}
	config, ok := decodeConfig(w, r)
	if !ok {
		return
	}
	config.ID = id
	updated, err := h.registry.Update(r.Context(), config)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) DeleteDatabase(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, ok := pathDatabaseID(w, r)
	if !ok {
		return
	}
	if err := h.registry.Delete(r.Context(), id); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) TestConnection(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, ok := pathDatabaseID(w, r)
	if !ok {
		return
	}
	if err := h.registry.TestConnection(r.Context(), id); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) ListSchemas(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireView(w, r)
	if !ok {
		return
	}
	result, err := h.catalog.ListSchemas(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) ListTables(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireView(w, r)
	if !ok {
		return
	}
	result, err := h.catalog.ListTables(r.Context(), id, r.PathValue("schema"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) GetTableDetails(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireView(w, r)
	if !ok {
		return
	}
	result, err := h.catalog.GetTableDetails(r.Context(), id, r.PathValue("schema"), r.PathValue("table"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) GetRows(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireView(w, r)
	if !ok {
		return
	}
	page, err := pageFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid pagination"})
		return
	}
	result, err := h.catalog.GetRows(r.Context(), id, r.PathValue("schema"), r.PathValue("table"), page)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	role, err := h.currentRole(r)
	if err != nil {
		writeServiceError(w, ErrUnauthorized)
		return false
	}
	if role != RoleAdmin {
		writeServiceError(w, ErrForbidden)
		return false
	}
	return true
}

func (h *Handler) requireView(w http.ResponseWriter, r *http.Request) (DatabaseID, bool) {
	id, ok := pathDatabaseID(w, r)
	if !ok {
		return 0, false
	}
	role, err := h.currentRole(r)
	if err != nil {
		writeServiceError(w, ErrUnauthorized)
		return 0, false
	}
	config, err := h.registry.Get(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return 0, false
	}
	if !roleCanAccess(role, config.AccessRoles) {
		writeServiceError(w, ErrForbidden)
		return 0, false
	}
	return id, true
}

func roleCanAccess(role string, accessRoles []string) bool {
	for _, allowedRole := range accessRoles {
		if allowedRole == role {
			return true
		}
	}
	return false
}

func decodeConfig(w http.ResponseWriter, r *http.Request) (DatabaseConfig, bool) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	var request databaseConfigRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
		return DatabaseConfig{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
		return DatabaseConfig{}, false
	}
	return DatabaseConfig{
		Name: request.Name, Host: request.Host, Port: request.Port,
		DatabaseName: request.DatabaseName, Username: request.Username,
		SSLMode: request.SSLMode, AccessRoles: request.AccessRoles, Password: request.Password,
	}, true
}

func pathDatabaseID(w http.ResponseWriter, r *http.Request) (DatabaseID, bool) {
	id, err := strconv.ParseInt(r.PathValue("databaseID"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid database ID"})
		return 0, false
	}
	return DatabaseID(id), true
}

func pageFromRequest(r *http.Request) (Page, error) {
	page := Page{Number: 1, Size: 50}
	var err error
	if value := r.URL.Query().Get("page"); value != "" {
		page.Number, err = strconv.Atoi(value)
		if err != nil || page.Number < 1 {
			return Page{}, errors.New("invalid page")
		}
	}
	if value := r.URL.Query().Get("pageSize"); value != "" {
		page.Size, err = strconv.Atoi(value)
		if err != nil || page.Size < 1 {
			return Page{}, errors.New("invalid page size")
		}
	}
	return page, nil
}

type errorResponse struct {
	Error string `json:"error"`
}

type databaseConfigRequest struct {
	Name         string `json:"name"`
	Host         string `json:"host"`
	Port         uint16 `json:"port"`
	DatabaseName string `json:"databaseName"`
	Username     string   `json:"username"`
	SSLMode      string   `json:"sslMode"`
	AccessRoles  []string `json:"accessRoles"`
	Password     string   `json:"password"`
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "not found"})
	case errors.Is(err, ErrInvalidConfig), errors.Is(err, ErrInvalidIdentifier):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request"})
	case errors.Is(err, ErrDatabaseUnavailable):
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "target database unavailable"})
	default:
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
	}
}


func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
