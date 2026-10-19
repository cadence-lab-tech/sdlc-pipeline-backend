package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

func scrapeValue(t *testing.T, reg *metrics.Registry, series string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		return -1
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Проход аренд и снимок состояния доходят до метрик воркера (CLAB-126,
// AC-11, AC-13, AC-14): тот же путь, что на тике в run().
func TestLeaseSweepAndStateReachMetrics(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{})
	system, err := store.ResolveActor(ctx, store.DB(), systemActor, domain.PrincipalSystem)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE stage_runs sr SET state = 'claimed', claimed_by = $1,
		       lease_expires_at = now() - interval '1 minute'
		FROM tasks t WHERE t.id = sr.task_id AND t.key IN ('SDLC-1', 'SDLC-2')`, system.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO repo_index_runs (repository_id, commit_sha, state)
		SELECT id, 'q-1', 'queued' FROM repositories WHERE name = 'sdlc-pipeline-backend'`); err != nil {
		t.Fatal(err)
	}

	reg := metrics.New("worker", "dev")
	m := metrics.NewWorker(reg, workerJobs...)

	refreshState(ctx, store, log, m)
	if v := scrapeValue(t, reg, `cadence_leases{state="expired"}`); v != 2 {
		t.Fatalf("истёкших аренд до прохода: %v", v)
	}
	if v := scrapeValue(t, reg, `cadence_index_queue_runs{state="queued"}`); v != 1 {
		t.Fatalf("очередь: %v", v)
	}

	sweepLeases(ctx, engine, log, system.ID, m)
	refreshState(ctx, store, log, m)

	for series, want := range map[string]float64{
		`cadence_leases_expired_total`:                       2,
		`cadence_leases{state="expired"}`:                    0,
		`cadence_leases{state="live"}`:                       0,
		`cadence_worker_job_errors_total{job="lease_sweep"}`: 0,
		`cadence_worker_job_errors_total{job="ops_state"}`:   0,
	} {
		if v := scrapeValue(t, reg, series); v != want {
			t.Fatalf("%s = %v, ждали %v", series, v, want)
		}
	}
	for _, job := range []string{"lease_sweep", "ops_state"} {
		if v := scrapeValue(t, reg, `cadence_worker_job_last_success_timestamp_seconds{job="`+job+`"}`); v <= 0 {
			t.Fatalf("%s: нет метки успешного прохода", job)
		}
	}
}
