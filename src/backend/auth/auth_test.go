package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testTokenProtector struct{}

func (testTokenProtector) Seal(token string) ([]byte, error) { return []byte(token), nil }
func (testTokenProtector) Open(token []byte) (string, error) { return string(token), nil }

func TestCreateUserRejectsInvalidInputBeforeDatabaseAccess(t *testing.T) {
	for _, tc := range []struct{ name, username, password string }{
		{"blank username", " ", "password123"},
		{"long username", strings.Repeat("a", 101), "password123"},
		{"short password", "user", "1234567"},
		{"long password in bytes", "user", strings.Repeat("a", 73)},
		{"unicode byte limit", "user", strings.Repeat("\u754c", 25)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (&Service{}).CreateUser(context.Background(), tc.username, tc.password); !errors.Is(err, ErrInvalidUser) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestSessionCookieFlags(t *testing.T) {
	for _, tc := range []struct {
		name, url, environment string
		secure                 bool
	}{
		{"local", "http://example.test", "", false},
		{"TLS", "https://example.test", "", true},
		{"production", "http://example.test", "production", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("APP_ENV", tc.environment)
			response := httptest.NewRecorder()
			setSessionCookie(response, httptest.NewRequest("GET", tc.url, nil), "test-token", time.Now().Add(time.Hour))
			cookies := response.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatal("missing cookie")
			}
			cookie := cookies[0]
			if cookie.Name != sessionCookieName || !cookie.HttpOnly || cookie.Secure != tc.secure || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode || cookie.Expires.IsZero() {
				t.Fatal("incorrect session cookie attributes")
			}
		})
	}
}

func TestRequireCSRF(t *testing.T) {
	for _, tc := range []struct {
		name, method, body, cookie, header string
		want                               int
	}{
		{"safe GET", "GET", "", "", "", 204},
		{"safe HEAD", "HEAD", "", "", "", 204},
		{"safe OPTIONS", "OPTIONS", "", "", "", 204},
		{"form token", "POST", "_csrf=token", "token", "", 204},
		{"matching header", "POST", "", "token", "token", 204},
		{"mismatched header", "POST", "", "token", "wrong", 403},
		{"missing cookie", "POST", "", "", "token", 403},
		{"missing supplied token", "POST", "", "token", "", 403},
		{"PUT requires token", "PUT", "", "", "", 403},
		{"DELETE requires token", "DELETE", "", "", "", 403},
		{"header takes precedence", "POST", "_csrf=token", "token", "wrong", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "/change", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.cookie != "" {
				request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: tc.cookie})
			}
			request.Header.Set(csrfHeaderName, tc.header)
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(204) })
			response := httptest.NewRecorder()
			(&Handler{}).RequireCSRF(next).ServeHTTP(response, request)
			if response.Code != tc.want || called != (tc.want == 204) {
				t.Fatalf("status=%d called=%v; want status=%d", response.Code, called, tc.want)
			}
		})
	}
}

func TestProtectedHandlersRejectMissingSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(http.Handler) http.Handler
		want int
	}{
		{"page", (&Handler{}).RequireAuth, http.StatusSeeOther},
		{"API", (&Handler{}).RequireAuthAPI, http.StatusUnauthorized},
		{"admin", (&Handler{}).RequireAuthAdmin, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			response := httptest.NewRecorder()
			tc.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(response, httptest.NewRequest("GET", "/protected", nil))
			if response.Code != tc.want || called {
				t.Fatalf("status=%d called=%v", response.Code, called)
			}
			if tc.want == http.StatusSeeOther && response.Header().Get("Location") != "/login" {
				t.Fatal("missing login redirect")
			}
		})
	}
}
