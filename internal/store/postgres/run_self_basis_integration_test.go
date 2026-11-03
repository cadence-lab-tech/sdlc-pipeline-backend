package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// CLAB-380: основания самоапрува читаются из ленты. Аренда идёт по
// частичному индексу task_events_stage_claimed, сдача артефакта — по ленте
// задачи (task_id, created_at), сжатой входом в этап. Тест наполняет ленту
// задачи тысячами событий, а соседние задачи — сотнями тысяч (в проде 440 тысяч,
// миграция 000042; lease.extended раз в пять минут на долгой
// аренде), проверяет ответ и план: seqscan по task_events не берётся ни в
// одном из случаев, включая худший — нет ни аренды, ни артефакта, и
// EXISTS обходит всё окно. План и буферы — в логе теста (-v).
func TestRunSelfBasisReadsFeedWithoutSeqScan(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()

	var taskID, other, statusID, roleID, rvID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT t.id, s.id, ro.id, rv.id,
		       (SELECT id FROM tasks WHERE key = 'SDLC-2')
		FROM tasks t
		JOIN workflow_statuses s ON s.workflow_id = t.workflow_id AND s.key = 'analysis'
		JOIN roles ro ON ro.project_id = t.project_id AND ro.key = 'analyst'
		JOIN role_versions rv ON rv.role_id = ro.id
		WHERE t.key = 'SDLC-1' ORDER BY rv.version DESC LIMIT 1`).Scan(&taskID, &statusID, &roleID, &rvID, &other); err != nil {
		t.Fatal(err)
	}
	var runID uuid.UUID
	var entered time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, attempt, state)
		VALUES ($1, $2, $3, $4, 1, 'claimed') RETURNING id, entered_at`,
		taskID, statusID, roleID, rvID).Scan(&runID, &entered); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
			INSERT INTO principals (kind, handle, display_name) VALUES ('user', $1, $1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	leaser, submitter, both, stranger := mk("leaser"), mk("submitter"), mk("both"), mk("stranger")

	ev := func(task uuid.UUID, run *uuid.UUID, typ string, actor *uuid.UUID, n int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_events (task_id, stage_run_id, type, actor_id, payload, created_at)
			SELECT $1, $2, $3, $4, '{}', $5::timestamptz + (g || ' seconds')::interval
			FROM generate_series(1, $6) g`, task, run, typ, actor, entered, n); err != nil {
			t.Fatal(err)
		}
	}
	// Долгая аренда другого держателя и шум соседней задачи.
	ev(taskID, &runID, "lease.extended", &stranger, 8000)
	ev(other, nil, "lease.extended", &stranger, 400000)
	ev(taskID, &runID, "stage.claimed", &leaser, 1)
	ev(taskID, &runID, "artifact.submitted", &submitter, 1)
	ev(taskID, &runID, "stage.claimed", &both, 1)
	ev(taskID, &runID, "artifact.submitted", &both, 1)
	// Аренда и артефакт посторонней попытки не засчитываются.
	otherRun := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO stage_runs (id, task_id, status_id, role_id, role_version_id, attempt, state)
		VALUES ($1, $2, $3, $4, $5, 2, 'claimed')`, otherRun, taskID, statusID, roleID, rvID); err != nil {
		t.Fatal(err)
	}
	ev(taskID, &otherRun, "artifact.submitted", &stranger, 1)
	if _, err := pool.Exec(ctx, `ANALYZE task_events`); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name            string
		actor           uuid.UUID
		lease, artifact bool
	}{
		{"аренда", leaser, true, false},
		{"артефакт", submitter, false, true},
		{"оба", both, true, true},
		{"никто", stranger, false, false},
	} {
		lease, artifact, err := store.RunSelfBasis(ctx, pool, taskID, runID, c.actor, entered)
		if err != nil || lease != c.lease || artifact != c.artifact {
			t.Fatalf("%s: аренда %v, артефакт %v (%v); ждали %v и %v", c.name, lease, artifact, err, c.lease, c.artifact)
		}

		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+strings.NewReplacer(
			"$1", "'"+taskID.String()+"'", "$2", "'"+runID.String()+"'",
			"$3", "'"+c.actor.String()+"'", "$4", "'"+entered.Format(time.RFC3339Nano)+"'",
		).Replace(postgres.RunSelfBasisSQL))
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		text := strings.Join(plan, "\n")
		t.Logf("%s:\n%s", c.name, text)
		if strings.Contains(text, "Seq Scan on task_events") {
			t.Fatalf("%s: проход всей таблицы task_events:\n%s", c.name, text)
		}
	}
}
