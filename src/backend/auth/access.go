package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden   = errors.New("forbidden")
)

type Principal struct {
	UserID          int64
	IsPlatformAdmin bool
}

const (
	ActionView = "database:view"
	ActionEdit = "database:edit"
)

type AccessService struct{ db *sql.DB }

func NewAccessService(db *sql.DB) *AccessService { return &AccessService{db: db} }

func (s *AccessService) PrincipalFromRequest(r *http.Request) (Principal, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	var principal Principal
	hash := sha256.Sum256([]byte(cookie.Value))
	err = s.db.QueryRowContext(r.Context(), `
		SELECT u.id, u.is_admin
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL
		  AND s.idle_expires_at > NOW() AND s.absolute_expires_at > NOW()
	`, hex.EncodeToString(hash[:])).Scan(&principal.UserID, &principal.IsPlatformAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	return principal, nil
}

func (s *AccessService) AuthorizeDatabase(ctx context.Context, principal Principal, databaseID int64, action string) error {
	if principal.UserID <= 0 {
		return ErrUnauthorized
	}
	if principal.IsPlatformAdmin {
		return nil
	}
	requiredLevel := "view"
	if action == ActionEdit {
		requiredLevel = "edit"
	}
	var allowed bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM group_members gm
			JOIN group_database_permissions gp ON gp.group_id = gm.group_id
			WHERE gm.user_id = $1 AND gp.database_id = $2
			  AND ($3 = 'view' OR gp.access_level = 'edit')
		)
	`, principal.UserID, databaseID, requiredLevel).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}
