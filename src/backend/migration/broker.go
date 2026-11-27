package migration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel/propagation"
)

const migrationQueue = "wbd.migration.jobs"

// RunWorker runs the outbox dispatcher and independent consumers. Only the
// worker process needs broker credentials. PostgreSQL is the source of truth.
func (s *Service) RunWorker(ctx context.Context, url string, parallelism int) {
	var workers sync.WaitGroup
	workers.Add(parallelism + 1)
	go func() {
		defer workers.Done()
		reconnectBroker(ctx, url, "publisher", s.publishJobs)
	}()
	for i := 0; i < parallelism; i++ {
		go func() {
			defer workers.Done()
			reconnectBroker(ctx, url, "consumer", s.consumeJobs)
		}()
	}
	workers.Wait()
}

func reconnectBroker(ctx context.Context, url, role string, run func(context.Context, *amqp.Channel) error) {
	for ctx.Err() == nil {
		err := withBroker(ctx, url, run)
		if ctx.Err() != nil {
			return
		}
		// Connection errors can include the broker address or credentials.
		if err != nil {
			slog.Warn("migration broker disconnected", "role", role)
		}
		if !waitWorker(ctx, 2*time.Second) {
			return
		}
	}
}

func withBroker(ctx context.Context, url string, run func(context.Context, *amqp.Channel) error) error {
	conn, err := amqp.Dial(url)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	_, err = ch.QueueDeclare(migrationQueue, true, false, false, false, nil)
	if err != nil {
		return err
	}
	return run(ctx, ch)
}

func waitWorker(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Service) publishJobs(ctx context.Context, ch *amqp.Channel) error {
	if err := ch.Confirm(false); err != nil {
		return err
	}
	returned := ch.NotifyReturn(make(chan amqp.Return, 1))
	for ctx.Err() == nil {
		published, err := s.publishNext(ctx, ch, returned)
		if err != nil {
			return err
		}
		if !published && !waitWorker(ctx, 300*time.Millisecond) {
			break
		}
	}
	return ctx.Err()
}

func (s *Service) publishNext(ctx context.Context, ch *amqp.Channel, returned <-chan amqp.Return) (bool, error) {
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs
        WHERE status='pending' AND published_at IS NULL AND available_at<=now()
          AND NOT EXISTS (SELECT 1 FROM migration_jobs b
            WHERE b.database_id=migration_jobs.database_id AND b.id<>migration_jobs.id
              AND (b.blocks_database OR (NOT migration_jobs.blocks_database AND b.id<migration_jobs.id AND b.status='pending')))
        ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if errors.Is(err, errJobNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	traceCtx := propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": job.traceparent, "tracestate": job.tracestate})
	traceCtx, span := s.startTrace(traceCtx, "migration.queue.publish", job.fields())
	err = publishMessage(traceCtx, ch, returned, job)
	endTrace(span, err)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE migration_jobs SET published_at=now() WHERE id=$1", job.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func publishMessage(ctx context.Context, ch *amqp.Channel, returned <-chan amqp.Return, job Job) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	headers := traceHeaders(ctx)
	id := strconv.FormatInt(job.ID, 10)
	confirm, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", migrationQueue, true, false, amqp.Publishing{
		ContentType: "text/plain", DeliveryMode: amqp.Persistent, MessageId: id, Body: []byte(id),
		Headers: amqp.Table{"traceparent": headers["traceparent"], "tracestate": headers["tracestate"]},
	})
	if err != nil {
		return err
	}
	ack, err := confirm.WaitContext(ctx)
	if err != nil {
		return err
	}
	select {
	case <-returned:
		return errors.New("migration message was unroutable")
	default:
	}
	if !ack {
		return errors.New("migration message publish rejected")
	}
	return nil
}

func (s *Service) consumeJobs(ctx context.Context, ch *amqp.Channel) error {
	if err := ch.Qos(1, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(migrationQueue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("migration deliveries closed")
			}
			id, err := strconv.ParseInt(string(delivery.Body), 10, 64)
			if err != nil || id <= 0 {
				if err = delivery.Reject(false); err != nil {
					return err
				}
				continue
			}
			headers := propagation.MapCarrier{}
			for _, key := range []string{"traceparent", "tracestate"} {
				if value, ok := delivery.Headers[key].(string); ok {
					headers[key] = value
				}
			}
			deliveryCtx := propagation.TraceContext{}.Extract(ctx, headers)
			if err = s.processJob(deliveryCtx, id); err != nil {
				// Closing the channel sends the unacknowledged message back
				// to the queue. Reconnect backoff prevents a tight loop.
				return fmt.Errorf("process job %d: %w", id, err)
			}
			if err = delivery.Ack(false); err != nil {
				return err
			}
		}
	}
}
