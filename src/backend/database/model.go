// Package database contains the PostgreSQL target-database registry,
// connection manager, and read-only viewer. It deliberately does not import
// the application's auth or migration packages; those integrations happen
// through the interfaces declared here and in handler.go.
package database

import (
	"errors"
	"time"
)

type DatabaseID int64

type DatabaseConfig struct {
	ID           DatabaseID `json:"id"`
	Name         string     `json:"name"`
	Host         string     `json:"host"`
	Port         uint16     `json:"port"`
	DatabaseName string     `json:"databaseName"`
	Username     string     `json:"username"`
	SSLMode      string     `json:"sslMode"`
	Password     string     `json:"-"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// Password is accepted on create/update and used only in memory to open a
// target pool. It is never serialized or returned by this package.

type DatabaseSummary struct {
	ID   DatabaseID `json:"id"`
	Name string     `json:"name"`
}

type Schema struct {
	Name string `json:"name"`
}

type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Type   string `json:"type"`
}

type Column struct {
	Name     string  `json:"name"`
	DataType string  `json:"dataType"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default,omitempty"`
	Position int     `json:"position"`
}

type Constraint struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Definition string `json:"definition"`
}

type Index struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

type TableDetails struct {
	Schema      string       `json:"schema"`
	Name        string       `json:"name"`
	Columns     []Column     `json:"columns"`
	Constraints []Constraint `json:"constraints"`
	Indexes     []Index      `json:"indexes"`
}

type Page struct {
	Number int `json:"number"`
	Size   int `json:"size"`
}

type RowsResult struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Page    Page             `json:"page"`
}

var (
	ErrNotFound            = errors.New("database resource not found")
	ErrInvalidConfig       = errors.New("invalid database configuration")
	ErrInvalidIdentifier   = errors.New("invalid schema or table identifier")
	ErrDatabaseUnavailable = errors.New("target database is unavailable")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrForbidden           = errors.New("forbidden")
)
