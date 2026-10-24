package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookieName = "lemonmeloncookie"

type Handler struct {
	service *Service
	viewer  *Viewer
}

func NewHandler(service *Service, viewer *Viewer) *Handler {
	return &Handler{service: service, viewer: viewer}
}

func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if _, err := h.service.UserBySession(r.Context(), cookie.Value); err != nil {
			clearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAuthAdmin allows only authenticated platform admins to continue.
func (h *Handler) RequireAuthAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		user, err := h.service.UserBySession(r.Context(), cookie.Value)
		if err != nil {
			clearSessionCookie(w)
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

func (h *Handler) UserFromRequest(r *http.Request) (User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return User{}, err
	}
	return h.service.UserBySession(r.Context(), cookie.Value)
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderLogin(w, http.StatusBadRequest, "Bad request")
		return
	}
	user, passwordHash, err := h.service.FindUserByUsername(r.Context(), r.FormValue("username"))
	if errors.Is(err, sql.ErrNoRows) {
		h.renderLogin(w, http.StatusUnauthorized, "Wrong username or password")
		return
	}
	if err != nil {
		h.renderLogin(w, http.StatusInternalServerError, "Server error")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(r.FormValue("password"))) != nil {
		h.renderLogin(w, http.StatusUnauthorized, "Wrong username or password")
		return
	}
	token, expiresAt, err := h.service.CreateSession(r.Context(), user)
	if err != nil {
		h.renderLogin(w, http.StatusInternalServerError, "Server error")
		return
	}
	setSessionCookie(w, token, expiresAt)
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
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) Whoami(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	user, err := h.service.UserBySession(r.Context(), cookie.Value)
	if err != nil {
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

func (h *Handler) UsersPage(w http.ResponseWriter, r *http.Request) {
	if err := h.viewer.RenderUsers(w); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *Handler) AuthPage(w http.ResponseWriter, r *http.Request) {
	h.renderLogin(w, http.StatusOK, "")
}

func (h *Handler) renderLogin(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	if err := h.viewer.RenderLogin(w, LoginPageData{Error: message}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expiresAt,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
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
