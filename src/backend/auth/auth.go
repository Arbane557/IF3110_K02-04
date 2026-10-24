package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidUser  = errors.New("invalid user")
	ErrUsernameTaken = errors.New("username already exists")
	ErrInvalidGroup = errors.New("invalid group")
)

// User is the authenticated account exposed to the application layer.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     Role   `json:"role"`
}

type Role string

const (
	RoleAdmin Role = "ADMIN"
	RoleUser  Role = "USER"
)

type Group struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type GroupDatabase struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	AccessLevel string `json:"accessLevel"`
}

type GroupDetails struct {
	Group     Group           `json:"group"`
	Members   []User          `json:"members"`
	Users     []User          `json:"users"`
	Databases []GroupDatabase `json:"databases"`
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
	var isAdmin bool
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, is_admin
		FROM users
		WHERE username = $1
	`, username).Scan(&user.ID, &user.Username, &passwordHash, &isAdmin)
	user.Role = roleForAdminFlag(isAdmin)
	return user, passwordHash, err
}

func (s *Service) CreateUser(ctx context.Context, username, password string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" || utf8.RuneCountInString(username) > 100 ||
		utf8.RuneCountInString(password) < 8 || len(password) > 72 {
		return User{}, ErrInvalidUser
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}

	var user User
	var isAdmin bool
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO users (username, password_hash)
		VALUES ($1, $2)
		ON CONFLICT (username) DO NOTHING
		RETURNING id, username, is_admin
	`, username, string(passwordHash)).Scan(&user.ID, &user.Username, &isAdmin)
	user.Role = roleForAdminFlag(isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUsernameTaken
	}
	return user, err
}

func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM groups ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Service) CreateGroup(ctx context.Context, name string) (Group, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return Group{}, ErrInvalidGroup
	}
	var group Group
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO groups (name) VALUES ($1)
		RETURNING id, name`, name).Scan(&group.ID, &group.Name)
	return group, err
}

func (s *Service) GroupDetails(ctx context.Context, groupID int64) (GroupDetails, error) {
	var details GroupDetails
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM groups WHERE id = $1`, groupID).
		Scan(&details.Group.ID, &details.Group.Name)
	if err != nil {
		return GroupDetails{}, err
	}
	details.Members, err = s.groupUsers(ctx, `
		SELECT u.id, u.username, u.is_admin
		FROM users u JOIN group_members gm ON gm.user_id = u.id
		WHERE gm.group_id = $1 ORDER BY u.username`, groupID)
	if err != nil {
		return GroupDetails{}, err
	}
	details.Users, err = s.groupUsers(ctx, `SELECT id, username, is_admin FROM users ORDER BY username`)
	if err != nil {
		return GroupDetails{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.name, COALESCE(gp.access_level, '')
		FROM managed_databases d
		LEFT JOIN group_database_permissions gp
		  ON gp.database_id = d.id AND gp.group_id = $1
		ORDER BY d.name, d.id`, groupID)
	if err != nil {
		return GroupDetails{}, err
	}
	defer rows.Close()
	details.Databases = make([]GroupDatabase, 0)
	for rows.Next() {
		var database GroupDatabase
		if err := rows.Scan(&database.ID, &database.Name, &database.AccessLevel); err != nil {
			return GroupDetails{}, err
		}
		details.Databases = append(details.Databases, database)
	}
	return details, rows.Err()
}

func (s *Service) groupUsers(ctx context.Context, query string, args ...any) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var user User
		var isAdmin bool
		if err := rows.Scan(&user.ID, &user.Username, &isAdmin); err != nil {
			return nil, err
		}
		user.Role = roleForAdminFlag(isAdmin)
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Service) AddGroupMember(ctx context.Context, groupID, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO group_members (group_id, user_id)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, groupID, userID)
	return err
}

func (s *Service) RemoveGroupMember(ctx context.Context, groupID, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID)
	return err
}

func (s *Service) SetGroupDatabaseAccess(ctx context.Context, groupID, databaseID int64, level string) error {
	if level != "view" && level != "edit" {
		return ErrInvalidGroup
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO group_database_permissions (group_id, database_id, access_level)
		VALUES ($1, $2, $3)
		ON CONFLICT (group_id, database_id)
		DO UPDATE SET access_level = EXCLUDED.access_level`, groupID, databaseID, level)
	return err
}

func (s *Service) RemoveGroupDatabaseAccess(ctx context.Context, groupID, databaseID int64) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM group_database_permissions WHERE group_id = $1 AND database_id = $2`, groupID, databaseID)
	return err
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
	var isAdmin bool
	tokenHash := hashToken(token)
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.is_admin
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.idle_expires_at > NOW()
		  AND s.absolute_expires_at > NOW()
	`, tokenHash).Scan(&user.ID, &user.Username, &isAdmin)
	if err != nil {
		return User{}, err
	}
	user.Role = roleForAdminFlag(isAdmin)

	_, err = s.db.ExecContext(ctx, `
		UPDATE sessions
		SET last_used_at = NOW(),
		    idle_expires_at = LEAST(NOW() + INTERVAL '30 minutes', absolute_expires_at)
		WHERE token_hash = $1
	`, tokenHash)
	return user, err
}

func roleForAdminFlag(isAdmin bool) Role {
	if isAdmin {
		return RoleAdmin
	}
	return RoleUser
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
