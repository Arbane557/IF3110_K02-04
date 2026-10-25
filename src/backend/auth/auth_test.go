package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestRequireCSRFRejectsMissingAndMismatchedTokens(t *testing.T) {
	for _, test := range []struct {
		name   string
		cookie string
		header string
	}{
		{name: "missing token"},
		{name: "mismatched token", cookie: "cookie-token", header: "other-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			nextCalled := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true })
			handler := (&Handler{}).RequireCSRF(next)
			request := httptest.NewRequest(http.MethodPost, "/api/change", nil)
			if test.cookie != "" {
				request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: test.cookie})
			}
			if test.header != "" {
				request.Header.Set(csrfHeaderName, test.header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || nextCalled {
				t.Fatalf("status=%d nextCalled=%v; want forbidden without calling next", response.Code, nextCalled)
			}
		})
	}
}

func TestRequireCSRFAcceptsMatchingTokens(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := (&Handler{}).RequireCSRF(next)
	request := httptest.NewRequest(http.MethodPost, "/api/change", nil)
	request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "valid-token"})
	request.Header.Set(csrfHeaderName, "valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d; want %d", response.Code, http.StatusNoContent)
	}
}

func TestAuthenticateSessionRotatesWithGracePeriod(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open integration database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TEMP TABLE users (id BIGINT PRIMARY KEY, username TEXT NOT NULL, is_admin BOOLEAN NOT NULL)`,
		`CREATE TEMP TABLE sessions (
			id BIGSERIAL PRIMARY KEY, token_hash CHAR(64) NOT NULL UNIQUE,
			previous_token_hash CHAR(64), previous_token_expires_at TIMESTAMPTZ,
			user_id BIGINT NOT NULL REFERENCES users(id), idle_expires_at TIMESTAMPTZ NOT NULL,
			absolute_expires_at TIMESTAMPTZ NOT NULL, last_used_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			rotated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), revoked_at TIMESTAMPTZ
		)`,
		`INSERT INTO users (id, username, is_admin) VALUES (1, 'rotation-test', FALSE)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare session fixture: %v", err)
		}
	}

	service := NewService(db)
	initialToken, expiresAt, err := service.CreateSession(context.Background(), User{ID: 1})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, rotated, _, err := service.AuthenticateSession(context.Background(), initialToken); err != nil || rotated != "" {
		t.Fatalf("unexpected early rotation: token=%q err=%v", rotated, err)
	}
	if _, err := db.Exec(`UPDATE sessions SET rotated_at = NOW() - INTERVAL '16 minutes'`); err != nil {
		t.Fatalf("age session rotation: %v", err)
	}

	_, currentToken, actualExpiry, err := service.AuthenticateSession(context.Background(), initialToken)
	if err != nil || currentToken == "" || currentToken == initialToken {
		t.Fatalf("rotation token=%q err=%v; want a new token", currentToken, err)
	}
	if !actualExpiry.Equal(expiresAt) {
		t.Fatalf("absolute expiry changed from %v to %v", expiresAt, actualExpiry)
	}
	if _, _, _, err := service.AuthenticateSession(context.Background(), initialToken); err != nil {
		t.Fatalf("old token rejected during grace period: %v", err)
	}
	if _, err := db.Exec(`UPDATE sessions SET previous_token_expires_at = NOW() - INTERVAL '1 second'`); err != nil {
		t.Fatalf("expire previous token: %v", err)
	}
	if _, _, _, err := service.AuthenticateSession(context.Background(), initialToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("old token error=%v; want expired session", err)
	}
	if _, _, _, err := service.AuthenticateSession(context.Background(), currentToken); err != nil {
		t.Fatalf("current token rejected: %v", err)
	}
}
