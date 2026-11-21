package migration

import "time"

type Migration struct {
	Version      int64      `json:"version"`
	SQL          string     `json:"sql"`
	SQLDown      string     `json:"sqlDown,omitempty"`
	Reversible   bool       `json:"reversible"`
	IrreversibleReason string `json:"irreversibleReason,omitempty"`
	Active       bool       `json:"active"`
	CreatedBy    int64      `json:"createdBy"`
	ExecutedBy   *int64     `json:"executedBy,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	ExecutedAt   *time.Time `json:"executedAt,omitempty"`
	Status       string     `json:"status"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
}

type CreateRequest struct {
	SQL string `json:"sql"`
	SQLDown string `json:"sqlDown"`
	DownSQL string `json:"downSql,omitempty"`
	Irreversible bool `json:"irreversible"`
	IrreversibleReason string `json:"irreversibleReason"`
}

type UpdateRequest struct {
	SQL string `json:"sql"`
}

type HistoryEntry struct {
	ExecutionID           int64      `json:"executionId"`
	Version               int64      `json:"version"`
	UserID                int64      `json:"userId"`
	Direction             string     `json:"direction"`
	Status                string     `json:"status"`
	SQL                   string     `json:"sql"`
	StartedAt             time.Time  `json:"startedAt"`
	ExecutedAt            *time.Time `json:"executedAt,omitempty"`
	ErrorMessage          string     `json:"errorMessage,omitempty"`
	CancelledExecutionID  *int64     `json:"cancelledExecutionId,omitempty"`
}
