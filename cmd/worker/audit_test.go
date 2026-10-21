package main

import (
	"context"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Уборка журнала по сроку (CLAB-210, критерий 19): записи старше срока
// уходят, свежие остаются, проход виден в метриках задания audit_gc.
func TestPurgeAuditByRetention(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit_log (created_at, channel, action, outcome)
		SELECT now() - make_interval(days => d), 'rest', 'POST /v1/teams', 'ok'
		FROM generate_series(1, 40) AS d`); err != nil {
		t.Fatal(err)
	}

	reg := metrics.New("worker", "dev")
	m := metrics.NewWorker(reg, workerJobs...)
	purgeAudit(ctx, store, testdb.Logger(), 30, m)

	var left, oldest int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), coalesce(max(extract(day FROM now() - created_at))::int, 0) FROM audit_log`).Scan(&left, &oldest); err != nil {
		t.Fatal(err)
	}
	if left != 29 || oldest >= 30 {
		t.Fatalf("осталось %d записей, самая старая %d дн.", left, oldest)
	}
	if v := scrapeValue(t, reg, `cadence_worker_job_last_success_timestamp_seconds{job="audit_gc"}`); v <= 0 {
		t.Fatalf("задание audit_gc не видно в метриках: %v", v)
	}
}
