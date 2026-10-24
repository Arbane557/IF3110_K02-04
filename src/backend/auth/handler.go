package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
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
	http.Redirect(w, r, "/databases", http.StatusSeeOther)
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
