package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
)

// databaseRecord is intentionally private so encrypted credentials cannot
// accidentally leave the database module in an HTTP response.
type databaseRecord struct {
	Config             DatabaseConfig
	PasswordCiphertext []byte
}

type RegistryRepository interface {
	Create(ctx context.Context, record databaseRecord) (databaseRecord, error)
	List(ctx context.Context) ([]DatabaseSummary, error)
	ListByRole(ctx context.Context, role string) ([]DatabaseSummary, error)
	FindByID(ctx context.Context, id DatabaseID) (databaseRecord, error)
	Update(ctx context.Context, record databaseRecord) (databaseRecord, error)
	Delete(ctx context.Context, id DatabaseID) error
}

type PostgresRegistryRepository struct {
	db *sql.DB
}

func NewPostgresRegistryRepository(db *sql.DB) *PostgresRegistryRepository {
	return &PostgresRegistryRepository{db: db}
}

func (r *PostgresRegistryRepository) Create(ctx context.Context, record databaseRecord) (databaseRecord, error) {
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO managed_databases
			(name, host, port, database_name, username, ssl_mode, access_roles, password_ciphertext)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at`,
		record.Config.Name, record.Config.Host, record.Config.Port,
		record.Config.DatabaseName, record.Config.Username, record.Config.SSLMode,
		record.Config.AccessRoles, record.PasswordCiphertext,
	).Scan(&record.Config.ID, &record.Config.CreatedAt, &record.Config.UpdatedAt)
	if err != nil {
		return databaseRecord{}, err
	}
	return record, nil
}

func (r *PostgresRegistryRepository) List(ctx context.Context) ([]DatabaseSummary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, name, access_roles FROM managed_databases ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]DatabaseSummary, 0)
	for rows.Next() {
		var item DatabaseSummary
		rolesScanner := pgtype.NewMap().SQLScanner(&item.AccessRoles)
		if err := rows.Scan(&item.ID, &item.Name, rolesScanner); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *PostgresRegistryRepository) ListByRole(ctx context.Context, role string) ([]DatabaseSummary, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, access_roles
		FROM managed_databases
		WHERE $1 = ANY(access_roles)
		ORDER BY name, id`, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]DatabaseSummary, 0)
	for rows.Next() {
		var item DatabaseSummary
		rolesScanner := pgtype.NewMap().SQLScanner(&item.AccessRoles)
		if err := rows.Scan(&item.ID, &item.Name, rolesScanner); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *PostgresRegistryRepository) FindByID(ctx context.Context, id DatabaseID) (databaseRecord, error) {
	var record databaseRecord
	rolesScanner := pgtype.NewMap().SQLScanner(&record.Config.AccessRoles)
	err := r.db.QueryRowContext(ctx, `
		SELECT id, name, host, port, database_name, username, ssl_mode, access_roles,
		       password_ciphertext, created_at, updated_at
		FROM managed_databases WHERE id = $1`, id,
	).Scan(
		&record.Config.ID, &record.Config.Name, &record.Config.Host, &record.Config.Port,
		&record.Config.DatabaseName, &record.Config.Username, &record.Config.SSLMode, rolesScanner,
		&record.PasswordCiphertext, &record.Config.CreatedAt, &record.Config.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return databaseRecord{}, ErrNotFound
	}
	return record, err
}

func (r *PostgresRegistryRepository) Update(ctx context.Context, record databaseRecord) (databaseRecord, error) {
	err := r.db.QueryRowContext(ctx, `
		UPDATE managed_databases
		SET name = $2, host = $3, port = $4, database_name = $5,
		    username = $6, ssl_mode = $7, access_roles = $8, password_ciphertext = $9,
		    updated_at = NOW()
		WHERE id = $1
		RETURNING created_at, updated_at`,
		record.Config.ID, record.Config.Name, record.Config.Host, record.Config.Port,
		record.Config.DatabaseName, record.Config.Username, record.Config.SSLMode,
		record.Config.AccessRoles, record.PasswordCiphertext,
	).Scan(&record.Config.CreatedAt, &record.Config.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return databaseRecord{}, ErrNotFound
	}
	if err != nil {
		return databaseRecord{}, err
	}
	return record, nil
}

func (r *PostgresRegistryRepository) Delete(ctx context.Context, id DatabaseID) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM managed_databases WHERE id = $1`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
