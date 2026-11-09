//go:build integration

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"wbd/backend/internal/testutil"
)

func TestAuthenticateSessionRotatesWithGracePeriod(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	db := fixture.SQL
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash) VALUES (1, 'rotation-test', 'unused')`); err != nil {
		t.Fatal(err)
	}
	service := NewService(db, testTokenProtector{})
	initialToken, expiresAt, err := service.CreateSession(context.Background(), User{ID: 1})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, rotated, _, err := service.AuthenticateSession(context.Background(), initialToken); err != nil || rotated != "" {
		t.Fatalf("unexpected early rotation: rotated=%v error=%v", rotated != "", err)
	}
	if _, err := db.Exec(`UPDATE sessions SET rotated_at = NOW() - INTERVAL '16 minutes'`); err != nil {
		t.Fatalf("age session rotation: %v", err)
	}

	_, currentToken, actualExpiry, err := service.AuthenticateSession(context.Background(), initialToken)
	if err != nil || currentToken == "" || currentToken == initialToken {
		t.Fatalf("rotation did not produce a distinct nonempty token: %v", err)
	}
	if !actualExpiry.Equal(expiresAt) {
		t.Fatalf("absolute expiry changed from %v to %v", expiresAt, actualExpiry)
	}
	if _, returnedToken, graceExpiry, err := service.AuthenticateSession(context.Background(), initialToken); err != nil || returnedToken != currentToken || !graceExpiry.Equal(expiresAt) {
		t.Fatalf("old token did not recover current token and absolute expiry during grace: %v", err)
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

func TestUserPasswordStorageIntegration(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	service := NewService(fixture.SQL, testTokenProtector{})
	ctx := context.Background()
	user, err := service.CreateUser(ctx, " alice ", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "alice" || user.Role != RoleUser {
		t.Fatalf("unexpected user: %+v", user)
	}
	_, hash, err := service.FindUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "password123" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("password123")) != nil {
		t.Fatal("stored password is not a valid bcrypt hash")
	}
	if _, err := service.CreateUser(ctx, "alice", "anotherpass"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate error=%v", err)
	}
}

func TestSessionExpiryAndRevocationIntegration(t *testing.T) {
	for _, tc := range []struct{ name, update string }{
		{"idle expired", `UPDATE sessions SET idle_expires_at = NOW() - INTERVAL '1 second'`},
		{"absolute expired", `UPDATE sessions SET absolute_expires_at = NOW() - INTERVAL '1 second'`},
		{"revoked", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := testutil.NewDatabase(t, true)
			service := NewService(fixture.SQL, testTokenProtector{})
			ctx := context.Background()
			user, err := service.CreateUser(ctx, "session-user", "password123")
			if err != nil {
				t.Fatal(err)
			}
			token, _, err := service.CreateSession(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			if tc.update == "" {
				err = service.RevokeSession(ctx, token)
			} else {
				_, err = fixture.SQL.Exec(tc.update)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := service.AuthenticateSession(ctx, token); !errors.Is(err, ErrInvalidSession) {
				t.Fatalf("authenticate error=%v; want invalid session", err)
			}
			request := httptest.NewRequest("GET", "/", nil)
			request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
			if _, err := NewAccessService(fixture.SQL).PrincipalFromRequest(request); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("principal error=%v", err)
			}
		})
	}
}

func TestDatabasePermissionsAndRevocationIntegration(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	ctx := context.Background()
	service := NewService(fixture.SQL, testTokenProtector{})
	user, err := service.CreateUser(ctx, "member", "password123")
	if err != nil {
		t.Fatal(err)
	}
	var databaseID int64
	err = fixture.SQL.QueryRow(`INSERT INTO managed_databases (name, host, port, database_name, username, password_ciphertext) VALUES ('target', 'localhost', 5432, 'target', 'app', '\x00') RETURNING id`).Scan(&databaseID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.CreateGroup(ctx, "viewers")
	if err != nil {
		t.Fatal(err)
	}
	edit, err := service.CreateGroup(ctx, "editors")
	if err != nil {
		t.Fatal(err)
	}
	access := NewAccessService(fixture.SQL)
	principal := Principal{UserID: user.ID}
	if err := access.AuthorizeDatabase(ctx, principal, databaseID, ActionView); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no grant error=%v", err)
	}
	for _, group := range []Group{view, edit} {
		if err := service.AddGroupMember(ctx, group.ID, user.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.SetGroupDatabaseAccess(ctx, view.ID, databaseID, "view"); err != nil {
		t.Fatal(err)
	}
	if err := access.AuthorizeDatabase(ctx, principal, databaseID, ActionView); err != nil {
		t.Fatal(err)
	}
	if err := access.AuthorizeDatabase(ctx, principal, databaseID, ActionEdit); !errors.Is(err, ErrForbidden) {
		t.Fatalf("view user edit error=%v", err)
	}
	if err := service.SetGroupDatabaseAccess(ctx, edit.ID, databaseID, "edit"); err != nil {
		t.Fatal(err)
	}
	if err := access.AuthorizeDatabase(ctx, principal, databaseID, ActionEdit); err != nil {
		t.Fatal(err)
	}
	if err := access.AuthorizeDatabase(ctx, principal, databaseID+1, ActionView); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other database error=%v", err)
	}
	token, _, err := service.CreateSession(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetGroupDatabaseAccess(ctx, edit.ID, databaseID, "view"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.AuthenticateSession(ctx, token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("downgrade did not revoke session: %v", err)
	}
	if err := access.AuthorizeDatabase(ctx, principal, databaseID, ActionEdit); !errors.Is(err, ErrForbidden) {
		t.Fatalf("edit survived downgrade: %v", err)
	}
}
