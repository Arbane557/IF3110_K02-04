package migration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"wbd/backend/auth"
	"wbd/backend/database"
)

type Authorizer interface {
	PrincipalFromRequest(r *http.Request) (auth.Principal, error)
	AuthorizeDatabase(ctx context.Context, principal auth.Principal, databaseID int64, action string) error
}

func (h *Handler) ListMigrations(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.authorize(w, r, auth.ActionView)
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

func (h *Handler) GetMigration(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.authorize(w, r, auth.ActionView)
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

func (h *Handler) CreateMigration(w http.ResponseWriter, r *http.Request) {
	id, principal, ok := h.authorize(w, r, auth.ActionEdit)
	if !ok {
		return
	}
	request, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	sqlDown := request.SQLDown
	if sqlDown == "" {
		sqlDown = request.DownSQL
	}
	irreversible := request.Irreversible
	if sqlDown == "" && request.IrreversibleReason == "" {
		irreversible = true
		// Legacy clients that send SQL only are treated as irreversible.
	}
	item, err := h.service.CreateWithDown(r.Context(), id, principal.UserID, request.SQL, sqlDown, irreversible, request.IrreversibleReason)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (h *Handler) UpdateMigration(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.authorize(w, r, auth.ActionEdit)
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

func (h *Handler) ExecuteMigration(w http.ResponseWriter, r *http.Request) {
	h.submitJob(w, r, "up")
}

func (h *Handler) RollbackMigration(w http.ResponseWriter, r *http.Request) {
	h.submitJob(w, r, "rollback")
}

func (h *Handler) ForwardMigration(w http.ResponseWriter, r *http.Request) {
	h.submitJob(w, r, "forward")
}

func (h *Handler) submitJob(w http.ResponseWriter, r *http.Request, direction string) {
	db, principal, ok := h.authorize(w, r, auth.ActionEdit)
	if !ok {
		return
	}
	version, ok := parseVersion(w, r)
	if !ok {
		return
	}
	var opts BatchOptions
	if direction == "up" && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&opts); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, errJobInput)
			return
		} else if err == nil && decoder.Decode(new(any)) != io.EOF {
			writeError(w, errJobInput)
			return
		}
	}
	job, err := h.service.SubmitJob(r.Context(), db, version, principal.UserID, direction, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (h *Handler) Jobs(w http.ResponseWriter, r *http.Request) {
	db, principal, ok := h.authorize(w, r, auth.ActionView)
	if !ok {
		return
	}
	jobs, err := h.service.ListJobs(r.Context(), db)
	if err != nil {
		writeError(w, err)
		return
	}
	for i := range jobs {
		jobs[i].CanApprove, err = h.service.CanApproveJob(r.Context(), jobs[i], principal.UserID)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (h *Handler) Job(w http.ResponseWriter, r *http.Request)        { h.handleJob(w, r, "get") }
func (h *Handler) ApproveJob(w http.ResponseWriter, r *http.Request) { h.handleJob(w, r, "approve") }
func (h *Handler) ResumeJob(w http.ResponseWriter, r *http.Request)  { h.handleJob(w, r, "resume") }
func (h *Handler) CancelJob(w http.ResponseWriter, r *http.Request)  { h.handleJob(w, r, "cancel") }

func (h *Handler) handleJob(w http.ResponseWriter, r *http.Request, op string) {
	action := auth.ActionView
	if op == "resume" || op == "cancel" {
		action = auth.ActionEdit
	}
	db, principal, ok := h.authorize(w, r, action)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("jobID"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, errJobInput)
		return
	}
	var job Job
	status := http.StatusOK
	switch op {
	case "get":
		job, err = h.service.GetJob(r.Context(), db, id)
	case "approve":
		job, err = h.service.ApproveJob(r.Context(), db, id, principal.UserID)
	case "resume", "cancel":
		job, err = h.service.RecoverJob(r.Context(), db, id, principal.UserID, op == "cancel")
		status = http.StatusAccepted
	}
	if err == nil {
		job.CanApprove, err = h.service.CanApproveJob(r.Context(), job, principal.UserID)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, job)
}

func (h *Handler) ListHistory(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.authorize(w, r, auth.ActionView)
	if !ok {
		return
	}
	hideRollback := r.URL.Query().Get("hideRollback") == "true"
	items, err := h.service.ListHistory(r.Context(), id, hideRollback)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) ExplainMigration(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.authorize(w, r, auth.ActionEdit)
	if !ok {
		return
	}
	request, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	plan, err := h.service.Explain(r.Context(), id, request.SQL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"plan": plan})
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, action string) (database.DatabaseID, auth.Principal, bool) {
	value, err := strconv.ParseInt(r.PathValue("databaseID"), 10, 64)
	if err != nil || value <= 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid database ID"})
		return 0, auth.Principal{}, false
	}
	principal, err := h.authorizer.PrincipalFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return 0, auth.Principal{}, false
	}
	if err := h.authorizer.AuthorizeDatabase(r.Context(), principal, value, action); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, auth.ErrUnauthorized) {
			status = http.StatusUnauthorized
		} else if errors.Is(err, auth.ErrForbidden) {
			status = http.StatusForbidden
		}
		writeJSON(w, status, errorResponse{Error: http.StatusText(status)})
		return 0, auth.Principal{}, false
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
	case errors.Is(err, auth.ErrForbidden):
		status, message = http.StatusForbidden, "forbidden"
	case errors.Is(err, errJobInput):
		status, message = http.StatusBadRequest, errJobInput.Error()
	case errors.Is(err, errJobConflict):
		status, message = http.StatusConflict, errJobConflict.Error()
	case errors.Is(err, errJobNotFound):
		status, message = http.StatusNotFound, errJobNotFound.Error()
	case errors.Is(err, ErrInvalidSQL), errors.Is(err, ErrInvalidVersion):
		status, message = http.StatusBadRequest, "invalid migration"
	case errors.Is(err, ErrMigrationNotFound):
		status, message = http.StatusNotFound, "migration not found"
	case errors.Is(err, ErrMigrationLocked):
		status, message = http.StatusConflict, "migration cannot be changed"
	case errors.Is(err, ErrIrreversible):
		status, message = http.StatusConflict, "migration is irreversible"
	case errors.Is(err, ErrNotLatestActive):
		status, message = http.StatusConflict, "only the latest active migration can be rolled back"
	case errors.Is(err, ErrForwardNotNext):
		status, message = http.StatusConflict, "migration is not the next forward migration"
	case errors.Is(err, ErrMigrationFailed):
		status, message = http.StatusUnprocessableEntity, "migration execution failed"
	case errors.Is(err, database.ErrDatabaseUnavailable):
		status, message = http.StatusBadGateway, "target database unavailable"
	}
	writeJSON(w, status, errorResponse{Error: message})
}
