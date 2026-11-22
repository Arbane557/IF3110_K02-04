package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"wbd/backend/observability"
)

type poolAcquireTracer struct{ observer observability.Observer }
type poolAcquireStartKey struct{}

var _ pgxpool.AcquireTracer = poolAcquireTracer{}

func (t poolAcquireTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, poolAcquireStartKey{}, time.Now())
}

func (t poolAcquireTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if started, ok := ctx.Value(poolAcquireStartKey{}).(time.Time); ok {
		t.observer.RecordPoolWait(time.Since(started), data.Err != nil)
	}
}

// ConnConfig accepts a QueryTracer; pgxpool also discovers AcquireTracer on it.
func (t poolAcquireTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (t poolAcquireTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
