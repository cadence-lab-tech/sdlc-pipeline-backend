package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Снимок состояния для метрик воркера (CLAB-126, AC-11, AC-13, AC-12 —
// головы веток) — на настоящей базе: правила «истекла», «в очереди» и
// «не читается» живут в SQL, и мок проверял бы собственный текст.
func TestOpsState(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	repo := func(name string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT id FROM repositories WHERE name = $1`, name).Scan(&id); err != nil {
			t.Fatalf("репозиторий %s: %v", name, err)
		}
		return id
	}

	st, err := store.OpsState(ctx, store.DB())
	if err != nil {
		t.Fatal(err)
	}
	if st.QueuedRuns != 0 || st.RunningRuns != 0 || st.OldestQueued != 0 ||
		st.LiveLeases != 0 || st.ExpiredLeases != 0 || st.HeadFailures != 0 {
		t.Fatalf("на демо-базе без прогонов и аренд: %+v", st)
	}

	backend, frontend, docs := repo("sdlc-pipeline-backend"), repo("sdlc-pipeline-frontend"), repo("sdlc-pipeline-docs")
	// Очередь: два в queued (старший — десять минут, в том числе отложенный
	// на повтор), один в running, один завершённый — не в счёт.
	exec(`INSERT INTO repo_index_runs (repository_id, commit_sha, state, created_at, next_attempt_at) VALUES
		($1, 'q-old', 'queued', now() - interval '10 minutes', now() + interval '5 minutes'),
		($1, 'q-new', 'queued', now() - interval '1 minute', now()),
		($2, 'r-1', 'running', now() - interval '2 minutes', now()),
		($2, 'c-1', 'completed', now() - interval '1 hour', now())`, backend, frontend)
	// Аренды: SDLC-1 держится, SDLC-2 истекла и ещё не возвращена, SDLC-3
	// сдал артефакты (submitted) с прошлым сроком — не аренда.
	exec(`UPDATE stage_runs sr SET state = 'claimed', lease_expires_at = now() + interval '10 minutes'
	      FROM tasks t WHERE t.id = sr.task_id AND t.key = 'SDLC-1'`)
	exec(`UPDATE stage_runs sr SET state = 'in_progress', lease_expires_at = now() - interval '1 minute'
	      FROM tasks t WHERE t.id = sr.task_id AND t.key = 'SDLC-2'`)
	exec(`UPDATE stage_runs sr SET state = 'submitted', lease_expires_at = now() - interval '1 minute'
	      FROM tasks t WHERE t.id = sr.task_id AND t.key = 'SDLC-3'`)
	// Головы: серия 2 на отслеживаемой ветке — в счёт; одиночный провал,
	// неотслеживаемая ветка и репозиторий с выключенной индексацией — нет.
	exec(`INSERT INTO repository_head_failures (repository_id, branch, failures, error) VALUES
		($1, 'main', 2, 'boom'), ($2, 'main', 1, 'blip'), ($1, 'gone', 5, 'deleted'), ($3, 'main', 3, 'boom')`,
		backend, frontend, docs)
	exec(`UPDATE repositories SET index_enabled = false WHERE id = $1`, docs)

	st, err = store.OpsState(ctx, store.DB())
	if err != nil {
		t.Fatal(err)
	}
	if st.QueuedRuns != 2 || st.RunningRuns != 1 {
		t.Fatalf("очередь: %+v", st)
	}
	if st.OldestQueued < 9*time.Minute || st.OldestQueued > 11*time.Minute {
		t.Fatalf("возраст очереди %s, ждали ~10m", st.OldestQueued)
	}
	if st.LiveLeases != 1 || st.ExpiredLeases != 1 {
		t.Fatalf("аренды: %+v", st)
	}
	if st.HeadFailures != 1 {
		t.Fatalf("нечитаемые головы: %d, ждали 1", st.HeadFailures)
	}

	// Воркер вернул истёкшую — в снимке её больше нет.
	expired, err := store.ExpireLeases(ctx, store.DB(), 100)
	if err != nil || len(expired) != 1 {
		t.Fatalf("возврат аренд: %d, %v", len(expired), err)
	}
	st, err = store.OpsState(ctx, store.DB())
	if err != nil {
		t.Fatal(err)
	}
	if st.LiveLeases != 1 || st.ExpiredLeases != 0 {
		t.Fatalf("после возврата: %+v", st)
	}
}
