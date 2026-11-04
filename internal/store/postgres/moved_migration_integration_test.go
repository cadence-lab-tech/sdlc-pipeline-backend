package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Миграция 000046_stage_runs_outcome_moved (CLAB-408, K-1): исход moved в
// CHECK stage_runs.outcome. Старые строки не трогаются, down снимает moved
// и возвращает прежнее ограничение, неизвестный исход отбивается.

const movedMigration = "000046_stage_runs_outcome_moved"

func execMovedMigration(t *testing.T, pool *pgxpool.Pool, suffix string) {
	t.Helper()
	body, err := migrations.Schema.ReadFile(movedMigration + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("%s%s: %v", movedMigration, suffix, err)
	}
}

func TestMovedOutcomeMigration(t *testing.T) {
	_, pool := testdb.Store(t)
	ctx := context.Background()

	// Прогоны всех прежних исходов и один moved на базе после up.
	if _, err := pool.Exec(ctx, `
		INSERT INTO stage_runs (task_id, status_id, attempt, state, outcome, finished_at)
		SELECT t.id, ws.id, 100 + o.n, 'passed', o.outcome, now()
		FROM tasks t
		JOIN workflow_statuses ws ON ws.workflow_id = t.workflow_id AND ws.key = 'analysis'
		CROSS JOIN (VALUES (1, 'advanced'), (2, 'rejected'), (3, 'cancelled'), (4, 'moved')) AS o(n, outcome)
		WHERE t.key = 'SDLC-1'`); err != nil {
		t.Fatalf("исходы после up: %v", err)
	}
	insert := func(outcome string, attempt int) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO stage_runs (task_id, status_id, attempt, state, outcome, finished_at)
			SELECT t.id, ws.id, $2, 'abandoned', $1, now()
			FROM tasks t JOIN workflow_statuses ws ON ws.workflow_id = t.workflow_id AND ws.key = 'analysis'
			WHERE t.key = 'SDLC-1'`, outcome, attempt)
		return err
	}
	if err := insert("bogus", 200); err == nil {
		t.Fatal("CHECK пропустил неизвестный исход")
	}
	counts := func() map[string]int {
		rows, err := pool.Query(ctx, `SELECT coalesce(outcome, ''), count(*) FROM stage_runs WHERE attempt > 100 GROUP BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				t.Fatal(err)
			}
			out[k] = n
		}
		return out
	}

	// down: moved → NULL, прежние исходы на месте, moved больше не принимается.
	execMovedMigration(t, pool, ".down.sql")
	got := counts()
	if got["advanced"] != 1 || got["rejected"] != 1 || got["cancelled"] != 1 || got["moved"] != 0 || got[""] != 1 {
		t.Fatalf("после down: %v", got)
	}
	if err := insert("moved", 201); err == nil {
		t.Fatal("после down moved принят")
	}

	// up снова: прежние строки не тронуты, moved принимается.
	execMovedMigration(t, pool, ".up.sql")
	if got := counts(); got["advanced"] != 1 || got[""] != 1 {
		t.Fatalf("после повторного up: %v", got)
	}
	if err := insert("moved", 202); err != nil {
		t.Fatalf("moved после up: %v", err)
	}
	if err := insert("bogus", 203); err == nil {
		t.Fatal("CHECK пропустил неизвестный исход после up")
	}
}
