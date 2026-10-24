package database

import (
	"context"
	"database/sql"
)

type Group struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type GroupAccessService struct {
	db *sql.DB
}

func NewGroupAccessService(db *sql.DB) *GroupAccessService {
	return &GroupAccessService{db: db}
}

func (s *GroupAccessService) CreateGroup(ctx context.Context, name string) (Group, error) {
	var group Group
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO groups (name) VALUES ($1)
		RETURNING id, name`, name).Scan(&group.ID, &group.Name)
	return group, err
}

func (s *GroupAccessService) AddMember(ctx context.Context, groupID, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO group_members (group_id, user_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, groupID, userID)
	return err
}

func (s *GroupAccessService) SetDatabaseAccess(ctx context.Context, groupID int64, databaseID DatabaseID, level string) error {
	if level != "view" && level != "edit" {
		return ErrInvalidConfig
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO group_database_permissions (group_id, database_id, access_level)
		VALUES ($1, $2, $3)
		ON CONFLICT (group_id, database_id)
		DO UPDATE SET access_level = EXCLUDED.access_level`,
		groupID, databaseID, level)
	return err
}
