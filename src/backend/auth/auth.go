package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"time"
)

// User is the authenticated account exposed to the application layer.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"isAdmin"`
}

// Service handles account lookup and server-side session persistence.
type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

func (s *Service) FindUserByUsername(ctx context.Context, username string) (User, string, error) {
	var user User
	var passwordHash string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_admin
		FROM users
		WHERE username = $1
	`, username).Scan(&user.ID, &user.Username, &passwordHash, &user.IsAdmin)
	return user, passwordHash, err
}

func (s *Service) CreateSession(ctx context.Context, user User) (string, time.Time, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", time.Time{}, err
	}

	token := base64.RawURLEncoding.EncodeToString(randomBytes)
	now := time.Now()
	expiresAt := now.Add(24 * time.Hour)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, idle_expires_at, absolute_expires_at)
		VALUES ($1, $2, $3, $4)
	`, hashToken(token), user.ID, now.Add(30*time.Minute), expiresAt)
	return token, expiresAt, err
}

func (s *Service) UserBySession(ctx context.Context, token string) (User, error) {
	var user User
	tokenHash := hashToken(token)
	err := s.db.QueryRowContext(ctx, `
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

	_, err = s.db.ExecContext(ctx, `
		UPDATE sessions
		SET last_used_at = NOW(),
		    idle_expires_at = LEAST(NOW() + INTERVAL '30 minutes', absolute_expires_at)
		WHERE token_hash = $1
	`, tokenHash)
	return user, err
}

func (s *Service) RevokeSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = NOW() WHERE token_hash = $1
	`, hashToken(token))
	return err
}

func hashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
