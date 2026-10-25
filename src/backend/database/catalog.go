package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const maxPageSize = 100

type CatalogService struct {
	pools TargetPoolProvider
}

func NewCatalogService(pools TargetPoolProvider) *CatalogService {
	return &CatalogService{pools: pools}
}

func (s *CatalogService) ListSchemas(ctx context.Context, id DatabaseID) ([]Schema, error) {
	pool, err := s.pools.Pool(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('information_schema', 'pg_catalog')
		  AND schema_name NOT LIKE 'pg_toast%'
		ORDER BY schema_name`)
	if err != nil {
		return nil, ErrDatabaseUnavailable
	}
	defer rows.Close()
	result := make([]Schema, 0)
	for rows.Next() {
		var schema Schema
		if err := rows.Scan(&schema.Name); err != nil {
			return nil, err
		}
		result = append(result, schema)
	}
	return result, rows.Err()
}

func (s *CatalogService) ListTables(ctx context.Context, id DatabaseID, schema string) ([]Table, error) {
	pool, err := s.pools.Pool(ctx, id)
	if err != nil {
		return nil, err
	}
	if !schemaExists(ctx, pool, schema) {
		return nil, ErrInvalidIdentifier
	}
	rows, err := pool.Query(ctx, `
		SELECT table_schema, table_name, table_type
		FROM information_schema.tables
		WHERE table_schema = $1
		ORDER BY table_name`, schema)
	if err != nil {
		return nil, ErrDatabaseUnavailable
	}
	defer rows.Close()
	result := make([]Table, 0)
	for rows.Next() {
		var table Table
		if err := rows.Scan(&table.Schema, &table.Name, &table.Type); err != nil {
			return nil, err
		}
		result = append(result, table)
	}
	return result, rows.Err()
}

func (s *CatalogService) GetTableDetails(ctx context.Context, id DatabaseID, schema, table string) (TableDetails, error) {
	pool, err := s.pools.Pool(ctx, id)
	if err != nil {
		return TableDetails{}, err
	}
	if !tableExists(ctx, pool, schema, table) {
		return TableDetails{}, ErrInvalidIdentifier
	}
	columns, err := getColumns(ctx, pool, schema, table)
	if err != nil {
		return TableDetails{}, err
	}
	constraints, err := getConstraints(ctx, pool, schema, table)
	if err != nil {
		return TableDetails{}, err
	}
	indexes, err := getIndexes(ctx, pool, schema, table)
	if err != nil {
		return TableDetails{}, err
	}
	return TableDetails{Schema: schema, Name: table, Columns: columns, Constraints: constraints, Indexes: indexes}, nil
}

func (s *CatalogService) GetRows(ctx context.Context, id DatabaseID, schema, table string, page Page) (RowsResult, error) {
	pool, err := s.pools.Pool(ctx, id)
	if err != nil {
		return RowsResult{}, err
	}
	if !tableExists(ctx, pool, schema, table) {
		return RowsResult{}, ErrInvalidIdentifier
	}
	page = normalizePage(page)
	query := fmt.Sprintf("SELECT * FROM %s LIMIT $1 OFFSET $2", pgx.Identifier{schema, table}.Sanitize())
	rows, err := pool.Query(ctx, query, page.Size, (page.Number-1)*page.Size)
	if err != nil {
		return RowsResult{}, ErrDatabaseUnavailable
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	columns := make([]string, len(fields))
	for i, field := range fields {
		columns[i] = field.Name
	}
	result := RowsResult{Columns: columns, Rows: make([]map[string]any, 0), Page: page}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return RowsResult{}, err
		}
		item := make(map[string]any, len(columns))
		for i, column := range columns {
			item[column] = values[i]
		}
		result.Rows = append(result.Rows, item)
	}
	if err := rows.Err(); err != nil {
		return RowsResult{}, err
	}
	return result, nil
}

func schemaExists(ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, schema string) bool {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil {
		return false
	}
	return exists
}

func tableExists(ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, schema, table string) bool {
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		)`, schema, table).Scan(&exists)
	return err == nil && exists
}

func getColumns(ctx context.Context, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, schema, table string) ([]Column, error) {
	rows, err := pool.Query(ctx, `
		SELECT column_name, data_type, is_nullable, column_default, ordinal_position
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, ErrDatabaseUnavailable
	}
	defer rows.Close()
	result := make([]Column, 0)
	for rows.Next() {
		var column Column
		var nullable string
		if err := rows.Scan(&column.Name, &column.DataType, &nullable, &column.Default, &column.Position); err != nil {
			return nil, err
		}
		column.Nullable = nullable == "YES"
		result = append(result, column)
	}
	return result, rows.Err()
}

func getConstraints(ctx context.Context, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, schema, table string) ([]Constraint, error) {
	rows, err := pool.Query(ctx, `
		SELECT con.conname,
		       CASE con.contype WHEN 'p' THEN 'PRIMARY KEY' WHEN 'f' THEN 'FOREIGN KEY'
		       WHEN 'u' THEN 'UNIQUE' WHEN 'c' THEN 'CHECK' ELSE con.contype::text END,
		       pg_get_constraintdef(con.oid)
		FROM pg_constraint con JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = rel.relnamespace
		WHERE ns.nspname = $1 AND rel.relname = $2 ORDER BY con.conname`, schema, table)
	if err != nil {
		return nil, ErrDatabaseUnavailable
	}
	defer rows.Close()
	result := make([]Constraint, 0)
	for rows.Next() {
		var value Constraint
		if err := rows.Scan(&value.Name, &value.Type, &value.Definition); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func getIndexes(ctx context.Context, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, schema, table string) ([]Index, error) {
	rows, err := pool.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = $1 AND tablename = $2 ORDER BY indexname`, schema, table)
	if err != nil {
		return nil, ErrDatabaseUnavailable
	}
	defer rows.Close()
	result := make([]Index, 0)
	for rows.Next() {
		var value Index
		if err := rows.Scan(&value.Name, &value.Definition); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func normalizePage(page Page) Page {
	if page.Number < 1 {
		page.Number = 1
	}
	if page.Size < 1 {
		page.Size = 50
	}
	if page.Size > maxPageSize {
		page.Size = maxPageSize
	}
	return page
}
