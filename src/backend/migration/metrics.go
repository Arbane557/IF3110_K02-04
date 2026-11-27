package migration

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Scrape durable job state in the API process only. Worker-local counters are
// exported separately; no user, database, job ID or SQL becomes a metric label.
type JobCollector struct {
	db                  *sql.DB
	jobs, scrapeSuccess *prometheus.Desc
}

func NewJobCollector(db *sql.DB) *JobCollector {
	return &JobCollector{
		db:            db,
		jobs:          prometheus.NewDesc("migration_jobs", "Current persisted jobs by status across all managed databases.", []string{"status"}, nil),
		scrapeSuccess: prometheus.NewDesc("migration_job_metrics_scrape_success", "Whether persisted job state was read successfully.", nil, nil),
	}
}

func (c *JobCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.jobs
	ch <- c.scrapeSuccess
}

func (c *JobCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT status,count(*) FROM migration_jobs GROUP BY status`)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.scrapeSuccess, prometheus.GaugeValue, 0)
		return
	}
	defer rows.Close()
	counts := map[string]float64{
		"pending_review": 0, "pending": 0, "running": 0, "success": 0,
		"failed": 0, "partial": 0, "partially_reverted": 0, "uncertain": 0, "other": 0,
	}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			ch <- prometheus.MustNewConstMetric(c.scrapeSuccess, prometheus.GaugeValue, 0)
			return
		}
		if _, known := counts[status]; !known {
			status = "other"
		}
		counts[status] += float64(count)
	}
	if rows.Err() != nil {
		ch <- prometheus.MustNewConstMetric(c.scrapeSuccess, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.scrapeSuccess, prometheus.GaugeValue, 1)
	for status, count := range counts {
		ch <- prometheus.MustNewConstMetric(c.jobs, prometheus.GaugeValue, count, status)
	}
}

func (s *Service) beginLockWait(ctx context.Context, job Job) (time.Time, error) {
	var started time.Time
	err := s.internalDB.QueryRowContext(ctx, `UPDATE migration_jobs
        SET lock_wait_started_at=COALESCE(lock_wait_started_at,clock_timestamp())
        WHERE id=$1 AND generation=$2 AND attempts=$3 AND status=$4
        RETURNING lock_wait_started_at`, job.ID, job.generation, job.attempts, job.Status).Scan(&started)
	return started, err
}

func (s *Service) finishLockWait(ctx context.Context, job Job, started time.Time) error {
	_, err := s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET lock_wait_started_at=NULL
        WHERE id=$1 AND generation=$2 AND attempts=$3 AND status=$4`, job.ID, job.generation, job.attempts, job.Status)
	if err == nil && s.observer != nil {
		s.observer.RecordLockWait(time.Since(started))
	}
	return err
}
