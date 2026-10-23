package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookieName = "lemonmeloncookie"

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"isAdmin"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginPageData struct {
	Error string
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func (app *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if _, err := app.checkUserBySession(r.Context(), cookie.Value); err != nil {
			clearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (app *App) loginPage(w http.ResponseWriter, r *http.Request) {
	app.renderLogin(w, http.StatusOK, "")
}

func (app *App) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		app.renderLogin(w, http.StatusBadRequest, "Bad request")
		return
	}

	request := LoginRequest{
		Username: r.FormValue("username"),
		Password: r.FormValue("password"),
	}

	user, passwordHash, err := app.findUserByUsername(r.Context(), request.Username)
	if errors.Is(err, sql.ErrNoRows) {
		app.renderLogin(w, http.StatusUnauthorized, "Wrong username or password")
		return
	}
	if err != nil {
		app.renderLogin(w, http.StatusInternalServerError, "Server error")
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(request.Password)) != nil {
		app.renderLogin(w, http.StatusUnauthorized, "Wrong username or password")
		return
	}

	token, expiresAt, err := app.createNewSession(r.Context(), user)
	if err != nil {
		app.renderLogin(w, http.StatusInternalServerError, "Server error")
		return
	}

	setSessionCookie(w, token, expiresAt)
	http.Redirect(w, r, "/databases", http.StatusSeeOther)
}

func (app *App) logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		if err := app.revokeSession(r.Context(), cookie.Value); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	}

	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (app *App) renderLogin(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	if err := app.Templates.ExecuteTemplate(w, "login.html", LoginPageData{Error: message}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (app *App) whoami(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	user, err := app.checkUserBySession(r.Context(), cookie.Value)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	writeJSON(w, http.StatusOK, user)
}

func (app *App) findUserByUsername(ctx context.Context, username string) (User, string, error) {
	var user User
	var passwordHash string

	err := app.InternalDB.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_admin
		FROM users
		WHERE username = $1
	`, username).Scan(
		&user.ID,
		&user.Username,
		&passwordHash,
		&user.IsAdmin,
	)

	return user, passwordHash, err
}

func (app *App) createNewSession(ctx context.Context, user User) (string, time.Time, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", time.Time{}, err
	}

	token := base64.RawURLEncoding.EncodeToString(randomBytes)
	now := time.Now()
	expiresAt := now.Add(24 * time.Hour)

	_, err := app.InternalDB.ExecContext(ctx, `
		INSERT INTO sessions (
			token_hash,
			user_id,
			idle_expires_at,
			absolute_expires_at
		)
		VALUES ($1, $2, $3, $4)
	`, hashToken(token), user.ID, now.Add(30*time.Minute), expiresAt)

	return token, expiresAt, err
}

func (app *App) checkUserBySession(ctx context.Context, token string) (User, error) {
	var user User
	tokenHash := hashToken(token)

	err := app.InternalDB.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.is_admin
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.idle_expires_at > NOW()
		  AND s.absolute_expires_at > NOW()
	`, tokenHash).Scan(&user.ID, &user.Username, &user.IsAdmin)
	if err != nil {
		return User{}, err
	}

	_, err = app.InternalDB.ExecContext(ctx, `
		UPDATE sessions
		SET last_used_at = NOW(),
			idle_expires_at = LEAST(
				NOW() + INTERVAL '30 minutes',
				absolute_expires_at
			)
		WHERE token_hash = $1
	`, tokenHash)

	return user, err
}

func (app *App) revokeSession(ctx context.Context, token string) error {
	_, err := app.InternalDB.ExecContext(ctx, `
		UPDATE sessions
		SET revoked_at = NOW()
		WHERE token_hash = $1
	`, hashToken(token))

	return err
}

func hashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorResponse{Error: message})
}
