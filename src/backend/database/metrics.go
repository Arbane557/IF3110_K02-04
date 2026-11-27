package database

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"wbd/backend/observability"
)

type poolAcquireTracer struct{ observer observability.Observer }
type poolAcquireStartKey struct{}

var _ pgxpool.AcquireTracer = poolAcquireTracer{}
var _ pgx.QueryTracer = poolAcquireTracer{}

func (t poolAcquireTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, poolAcquireStartKey{}, time.Now())
}

func (t poolAcquireTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if started, ok := ctx.Value(poolAcquireStartKey{}).(time.Time); ok {
		t.observer.RecordPoolWait(time.Since(started), data.Err != nil)
	}
}

type readQueryContextKey struct{}
type queryTraceKey struct{}

type readQueryContext struct {
	database DatabaseID
	actor    int64
	query    string
}

// WithReadOnlyQuery marks a target query for logging. Pass a safe SQL label
// when the statement contains user-supplied SQL instead of catalog SQL.
func WithReadOnlyQuery(ctx context.Context, database DatabaseID, actor int64, safeSQL string) context.Context {
	return context.WithValue(ctx, readQueryContextKey{}, readQueryContext{database, actor, safeSQL})
}

func catalogQueryContext(ctx context.Context, id DatabaseID, safeSQL string) context.Context {
	info, _ := ctx.Value(readQueryContextKey{}).(readQueryContext)
	return WithReadOnlyQuery(ctx, id, info.actor, safeSQL)
}

type queryTrace struct {
	started time.Time
	fields  observability.Fields
	span    observability.Span
}

func (t poolAcquireTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	info, ok := ctx.Value(readQueryContextKey{}).(readQueryContext)
	if !ok {
		return ctx
	}
	query := strings.TrimSpace(data.SQL)
	if info.query != "" && query != "begin read only" && query != "rollback" {
		query = info.query
	}
	parameters := make([]any, len(data.Args))
	for i, arg := range data.Args {
		// Catalog integers are LIMIT and OFFSET. Identifiers supplied as
		// string parameters and any other values stay out of telemetry.
		parameters[i] = "[redacted]"
		if value, ok := arg.(int); ok {
			parameters[i] = value
		}
	}
	fields := observability.Fields{
		"database": int64(info.database), "actor": info.actor, "query": query,
		"parameters": parameters, "status": "running",
	}
	started := time.Now()
	ctx, span := t.observer.StartTrace(ctx, "database.query", fields)
	t.observer.Log(ctx, slog.LevelInfo, "database.query.started", fields)
	return context.WithValue(ctx, queryTraceKey{}, queryTrace{started, fields, span})
}

func (t poolAcquireTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	state, ok := ctx.Value(queryTraceKey{}).(queryTrace)
	if !ok {
		return
	}
	fields := maps.Clone(state.fields)
	fields["duration_ms"] = time.Since(state.started).Milliseconds()
	fields["status"] = "success"
	level := slog.LevelInfo
	var safeErr error
	if data.Err != nil {
		level = slog.LevelError
		fields["status"] = "failed"
		// PostgreSQL errors can echo SQL literals, values, and connection data.
		message := "database query failed"
		var pgErr *pgconn.PgError
		switch {
		case errors.As(data.Err, &pgErr):
			fields["sqlstate"] = pgErr.Code
			message += " (SQLSTATE " + pgErr.Code + ")"
		case errors.Is(data.Err, context.Canceled):
			message = "database query canceled"
		case errors.Is(data.Err, context.DeadlineExceeded):
			message = "database query deadline exceeded"
		}
		fields["error"] = message
		safeErr = errors.New(message)
	} else {
		fields["rows"] = data.CommandTag.RowsAffected()
	}
	t.observer.Log(ctx, level, "database.query.finished", fields)
	if state.span != nil {
		state.span.AddEvent("database.query.result", fields)
		state.span.End(safeErr)
	}
}
