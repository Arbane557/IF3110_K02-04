package migration

import "time"

type Migration struct {
	Version      int64      `json:"version"`
	SQL          string     `json:"sql"`
	CreatedBy    int64      `json:"createdBy"`
	ExecutedBy   *int64     `json:"executedBy,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	ExecutedAt   *time.Time `json:"executedAt,omitempty"`
	Status       string     `json:"status"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
}

type CreateRequest struct {
	SQL string `json:"sql"`
}

type UpdateRequest struct {
	SQL string `json:"sql"`
}
