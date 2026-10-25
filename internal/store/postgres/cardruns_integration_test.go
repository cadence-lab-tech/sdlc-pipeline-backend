package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// queryCounter считает обращения к базе.
type queryCounter struct {
	postgres.Querier
	n int
}

func (c *queryCounter) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.n++
	return c.Querier.Exec(ctx, sql, args...)
}

func (c *queryCounter) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	return c.Querier.Query(ctx, sql, args...)
}

func (c *queryCounter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	return c.Querier.QueryRow(ctx, sql, args...)
}

// CLAB-283, AC-4: номер версии роли, вердикты гейтов и выходы читаются по
// одному запросу на задачу — число обращений CardStageRuns не растёт с
// числом прогонов. Те же прогоны содержательно проверены в httpapi.
func TestCardStageRunsQueriesDoNotGrowWithRuns(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()

	var taskID, statusID, roleID, rvID, gateID, transitionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT t.id, s.id, ro.id, rv.id
		FROM tasks t
		JOIN workflow_statuses s ON s.workflow_id = t.workflow_id AND s.key = 'analysis'
		JOIN roles ro ON ro.project_id = t.project_id AND ro.key = 'analyst'
		JOIN role_versions rv ON rv.role_id = ro.id
		WHERE t.key = 'SDLC-1' ORDER BY rv.version DESC LIMIT 1`).Scan(&taskID, &statusID, &roleID, &rvID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT g.id, g.transition_id FROM gates g
		JOIN workflow_transitions tr ON tr.id = g.transition_id
		JOIN tasks t ON t.workflow_id = tr.workflow_id
		WHERE t.key = 'SDLC-1' LIMIT 1`).Scan(&gateID, &transitionID); err != nil {
		t.Fatal(err)
	}

	addRuns := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			var runID uuid.UUID
			if err := pool.QueryRow(ctx, `
				INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, attempt, state)
				VALUES ($1, $2, $3, $4, (SELECT coalesce(max(attempt), 0) + 1 FROM stage_runs WHERE task_id = $1 AND status_id = $2),
				        'passed') RETURNING id`,
				taskID, statusID, roleID, rvID).Scan(&runID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO gate_results (stage_run_id, gate_id, transition_id, status)
				VALUES ($1, $2, $3, 'passed')`, runID, gateID, transitionID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO task_events (task_id, stage_run_id, type, payload)
				VALUES ($1, $2, 'task.advanced', '{"transition":"analysis_done","to":"development","comment":"ок"}')`,
				taskID, runID); err != nil {
				t.Fatal(err)
			}
		}
	}
	measure := func() (queries, runs int) {
		t.Helper()
		c := &queryCounter{Querier: pool}
		out, err := store.CardStageRuns(ctx, c, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return c.n, len(out)
	}

	addRuns(1)
	q1, r1 := measure()
	addRuns(6)
	q7, r7 := measure()
	if r7 <= r1 {
		t.Fatalf("прогонов не прибавилось: %d и %d", r1, r7)
	}
	if q1 != q7 {
		t.Errorf("обращений к базе: %d на %d прогонов и %d на %d", q1, r1, q7, r7)
	}

	// Содержимое: у каждого добавленного прогона номер версии, один вердикт
	// и выход с комментарием.
	c := &queryCounter{Querier: pool}
	out, err := store.CardStageRuns(ctx, c, taskID)
	if err != nil {
		t.Fatal(err)
	}
	withExit := 0
	for _, r := range out {
		if r.Exit == nil {
			continue
		}
		withExit++
		if r.RoleVersion == nil || len(r.GateResults) != 1 || r.Exit.Comment == nil || *r.Exit.Comment != "ок" ||
			r.Exit.Transition != "analysis_done" || r.Exit.ToStatus != "development" || r.Exit.Kind != "advanced" {
			t.Errorf("прогон %s: версия %v, вердиктов %d, выход %+v", r.ID, r.RoleVersion, len(r.GateResults), r.Exit)
		}
	}
	if withExit != 7 {
		t.Errorf("прогонов с выходом %d, ожидали 7", withExit)
	}
}
