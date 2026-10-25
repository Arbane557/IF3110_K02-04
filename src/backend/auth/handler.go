package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookieName = "lemonmeloncookie"

const (
	csrfCookieName = "csrf_token"
	csrfHeaderName = "X-CSRF-Token"
)

type Handler struct {
	service   *Service
	templates *template.Template
}

func NewHandler(service *Service, templates *template.Template) *Handler {
	return &Handler{service: service, templates: templates}
}

func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.authenticateRequest(w, r); err != nil {
			clearSessionCookie(w, r)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAuthAPI authenticates API requests without redirecting clients to an HTML page.
func (h *Handler) RequireAuthAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.authenticateRequest(w, r); err != nil {
			clearSessionCookie(w, r)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAuthAdmin allows only authenticated platform admins to continue.
func (h *Handler) RequireAuthAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := h.authenticateRequest(w, r)
		if err != nil {
			clearSessionCookie(w, r)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if user.Role != RoleAdmin {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(csrfCookieName)
		if err != nil {
			writeError(w, http.StatusForbidden, "csrf token required")
			return
		}
		provided := r.Header.Get(csrfHeaderName)
		if provided == "" {
			if err := r.ParseForm(); err != nil {
				writeError(w, http.StatusBadRequest, "invalid form body")
				return
			}
			provided = r.FormValue("_csrf")
		}
		if provided == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(provided)) != 1 {
			writeError(w, http.StatusForbidden, "invalid csrf token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderLogin(w, r, http.StatusBadRequest, "Bad request")
		return
	}
	user, passwordHash, err := h.service.FindUserByUsername(r.Context(), r.FormValue("username"))
	if errors.Is(err, sql.ErrNoRows) {
		h.renderLogin(w, r, http.StatusUnauthorized, "Wrong username or password")
		return
	}
	if err != nil {
		h.renderLogin(w, r, http.StatusInternalServerError, "Server error")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(r.FormValue("password"))) != nil {
		h.renderLogin(w, r, http.StatusUnauthorized, "Wrong username or password")
		return
	}
	token, expiresAt, err := h.service.CreateSession(r.Context(), user)
	if err != nil {
		h.renderLogin(w, r, http.StatusInternalServerError, "Server error")
		return
	}
	setSessionCookie(w, r, token, expiresAt)
	setCSRFCookie(w, r, CSRFTokenFromRequest(r), expiresAt)
	destination := "/databases"
	if user.Role == RoleAdmin {
		destination = "/auth"
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if err := h.service.RevokeSession(r.Context(), cookie.Value); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	}
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) Whoami(w http.ResponseWriter, r *http.Request) {
	user, err := h.authenticateRequest(w, r)
	if err != nil {
		clearSessionCookie(w, r)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

type registerUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) RegisterUser(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request registerUserRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	user, err := h.service.CreateUser(r.Context(), request.Username, request.Password)
	switch {
	case errors.Is(err, ErrInvalidUser):
		writeError(w, http.StatusBadRequest, "username or password does not meet requirements")
	case errors.Is(err, ErrUsernameTaken):
		writeError(w, http.StatusConflict, "username already exists")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not create user")
	default:
		writeJSON(w, http.StatusCreated, user)
	}
}

func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.service.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load users")
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (h *Handler) MakeUserAdmin(w http.ResponseWriter, r *http.Request) {
	userID, ok := positivePathID(w, r, "userID")
	if !ok {
		return
	}
	user, err := h.service.MakeUserAdmin(r.Context(), userID)
	if errors.Is(err, ErrUserNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not make user admin")
	} else {
		writeJSON(w, http.StatusOK, user)
	}
}

func (h *Handler) UsersPage(w http.ResponseWriter, r *http.Request) {
	h.renderTemplate(w, "users.html", map[string]string{"CSRFToken": CSRFTokenFromRequest(r)})
}

func (h *Handler) GroupsPage(w http.ResponseWriter, r *http.Request) {
	h.renderTemplate(w, "groups.html", map[string]string{"CSRFToken": CSRFTokenFromRequest(r)})
}

func (h *Handler) GroupPage(w http.ResponseWriter, r *http.Request) {
	h.renderTemplate(w, "group.html", map[string]string{"CSRFToken": CSRFTokenFromRequest(r)})
}

func (h *Handler) ListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.service.ListGroups(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load groups")
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (h *Handler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	group, err := h.service.CreateGroup(r.Context(), request.Name)
	if errors.Is(err, ErrInvalidGroup) {
		writeError(w, http.StatusBadRequest, "group name is required and must be at most 100 characters")
	} else if isUniqueViolation(err) {
		writeError(w, http.StatusConflict, "group name already exists")
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create group")
	} else {
		writeJSON(w, http.StatusCreated, group)
	}
}

func isUniqueViolation(err error) bool {
	var state interface{ SQLState() string }
	return errors.As(err, &state) && state.SQLState() == "23505"
}

func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := positivePathID(w, r, "groupID")
	if !ok {
		return
	}
	details, err := h.service.GroupDetails(r.Context(), groupID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "group not found")
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load group")
	} else {
		writeJSON(w, http.StatusOK, details)
	}
}

func (h *Handler) AddGroupMember(w http.ResponseWriter, r *http.Request) {
	groupID, ok := positivePathID(w, r, "groupID")
	if !ok {
		return
	}
	var request struct {
		UserID int64 `json:"userId"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.UserID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid user ID")
		return
	}
	if err := h.service.AddGroupMember(r.Context(), groupID, request.UserID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not add group member")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) RemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	groupID, ok := positivePathID(w, r, "groupID")
	if !ok {
		return
	}
	userID, ok := positivePathID(w, r, "userID")
	if !ok {
		return
	}
	if err := h.service.RemoveGroupMember(r.Context(), groupID, userID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not remove group member")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) SetGroupDatabaseAccess(w http.ResponseWriter, r *http.Request) {
	groupID, ok := positivePathID(w, r, "groupID")
	if !ok {
		return
	}
	databaseID, ok := positivePathID(w, r, "databaseID")
	if !ok {
		return
	}
	var request struct {
		AccessLevel string `json:"accessLevel"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.AccessLevel == "" {
		if err := h.service.RemoveGroupDatabaseAccess(r.Context(), groupID, databaseID); err != nil {
			writeError(w, http.StatusInternalServerError, "could not remove database access")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.service.SetGroupDatabaseAccess(r.Context(), groupID, databaseID, request.AccessLevel); errors.Is(err, ErrInvalidGroup) {
		writeError(w, http.StatusBadRequest, "access level must be view or edit")
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update database access")
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func positivePathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return 0, false
	}
	return id, true
}

func (h *Handler) AuthPage(w http.ResponseWriter, r *http.Request) {
	h.renderLogin(w, r, http.StatusOK, "")
}

func (h *Handler) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	token, err := ensureCSRFCookie(w, r)
	if err != nil {
		http.Error(w, "could not create csrf token", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	data := map[string]string{"Error": message, "CSRFToken": token}
	if err := h.templates.ExecuteTemplate(w, "login.html", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *Handler) renderTemplate(w http.ResponseWriter, name string, data any) {
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *Handler) authenticateRequest(w http.ResponseWriter, r *http.Request) (User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return User{}, ErrInvalidSession
	}
	user, rotatedToken, expiresAt, err := h.service.AuthenticateSession(r.Context(), cookie.Value)
	if err != nil {
		return User{}, err
	}
	if rotatedToken != "" {
		setSessionCookie(w, r, rotatedToken, expiresAt)
	}
	return user, nil
}

func ensureCSRFCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	if cookie, err := r.Cookie(csrfCookieName); err == nil && cookie.Value != "" {
		return cookie.Value, nil
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	setCSRFCookie(w, r, token, time.Now().Add(sessionAbsoluteTimeout))
	return token, nil
}

// CSRFTokenFromRequest returns the browser-readable token used by page forms and scripts.
func CSRFTokenFromRequest(r *http.Request) string {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func secureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(os.Getenv("APP_ENV"), "production")
}

func setCSRFCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: token, Path: "/", HttpOnly: false,
		SameSite: http.SameSiteLaxMode, Secure: secureCookie(r), Expires: expiresAt,
	})
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookie(r), Expires: expiresAt,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookie(r), MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: "", Path: "/",
		SameSite: http.SameSiteLaxMode, Secure: secureCookie(r), MaxAge: -1,
	})
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}
