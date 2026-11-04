package postgres_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Миграция 000045_task_rank (CLAB-399, решение 0075): К-1 и К-2 анализа.

const rankMigration = "000045_task_rank"

func execRankMigration(t *testing.T, pool *pgxpool.Pool, suffix string) {
	t.Helper()
	body, err := migrations.Schema.ReadFile(rankMigration + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("%s%s: %v", rankMigration, suffix, err)
	}
}

// oldBacklogOrder — порядок order=backlog до ранга (CLAB-363), копией:
// по нему миграция обязана заполнить ранги.
const oldBacklogOrder = `
	EXISTS (SELECT 1 FROM task_links bl
	        JOIN tasks blk ON blk.id = bl.from_task_id
	        JOIN workflow_statuses bws ON bws.id = blk.status_id
	        WHERE bl.to_task_id = t.id AND bl.rel = 'blocks' AND bws.category <> 'done'),
	CASE t.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
	t.seq`

// groupKeys — ключи задач группы (parent — ключ родителя, "" — верхний
// уровень проекта SDLC) в порядке order.
func groupKeys(t *testing.T, pool *pgxpool.Pool, parent, order string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT t.key FROM tasks t
		JOIN projects p ON p.id = t.project_id
		LEFT JOIN tasks par ON par.id = t.parent_id
		WHERE p.key = 'SDLC' AND coalesce(par.key, '') = $1
		ORDER BY `+order, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

func TestRankMigrationFillsOldBacklogOrder(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()

	// До миграции: колонки нет.
	execRankMigration(t, pool, ".down.sql")

	// Фикстура: задачи верхнего уровня и дети двух эпиков, приоритеты,
	// блокировки (от незакрытой и от готовой задачи), закрытые задачи.
	if _, err := pool.Exec(ctx, `
		WITH src AS (
		    SELECT t.project_id, t.workflow_id, t.created_by FROM tasks t WHERE t.key = 'SDLC-1'
		)
		INSERT INTO tasks (project_id, key, seq, title, workflow_id, status_id, priority, properties, created_by)
		SELECT src.project_id, 'SDLC-' || g, g, 'задача ' || g, src.workflow_id,
		       (SELECT id FROM workflow_statuses ws WHERE ws.workflow_id = src.workflow_id AND ws.key =
		            CASE WHEN g % 7 = 0 THEN 'done' WHEN g % 11 = 0 THEN 'cancelled'
		                 WHEN g % 5 = 0 THEN 'development' ELSE 'new' END),
		       (ARRAY['low', 'normal', 'high', 'urgent'])[1 + g % 4], '{"type":"task"}', src.created_by
		FROM src, generate_series(10, 69) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (project_id, key, seq, title, workflow_id, status_id, properties, created_by)
		SELECT e.project_id, 'SDLC-9', 9, 'второй эпик', e.workflow_id, e.status_id, e.properties, e.created_by
		FROM tasks e WHERE e.key = 'SDLC-4'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks SET parent_id = (SELECT id FROM tasks WHERE key = 'SDLC-4') WHERE seq BETWEEN 30 AND 44;
		UPDATE tasks SET parent_id = (SELECT id FROM tasks WHERE key = 'SDLC-9') WHERE seq BETWEEN 45 AND 54;
		INSERT INTO task_links (from_task_id, to_task_id, rel, created_by)
		SELECT f.id, x.id, 'blocks', f.created_by
		FROM tasks f, tasks x
		WHERE (f.key = 'SDLC-10' AND x.key IN ('SDLC-11', 'SDLC-15', 'SDLC-31', 'SDLC-46'))
		   OR (f.key = 'SDLC-14' AND x.key IN ('SDLC-12', 'SDLC-33'))`); err != nil {
		t.Fatal(err)
	}

	groups := []string{"", "SDLC-4", "SDLC-9"}
	before := map[string][]string{}
	for _, g := range groups {
		before[g] = groupKeys(t, pool, g, oldBacklogOrder)
		if len(before[g]) < 10 {
			t.Fatalf("группа %q мала: %v", g, before[g])
		}
	}

	execRankMigration(t, pool, ".up.sql")

	// К-1: у каждой задачи непустой ранг, порядок групп прежний, ключи —
	// набор domain.RankSpread для группы.
	var empty int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE rank IS NULL OR rank = ''`).Scan(&empty); err != nil || empty != 0 {
		t.Fatalf("ранг пуст у %d задач: %v", empty, err)
	}
	for _, g := range groups {
		if got := groupKeys(t, pool, g, "t.rank, t.seq"); !slices.Equal(got, before[g]) {
			t.Fatalf("группа %q: после миграции %v, до %v", g, got, before[g])
		}
		rows, err := pool.Query(ctx, `
			SELECT t.rank FROM tasks t JOIN projects p ON p.id = t.project_id
			LEFT JOIN tasks par ON par.id = t.parent_id
			WHERE p.key = 'SDLC' AND coalesce(par.key, '') = $1 ORDER BY t.rank, t.seq`, g)
		if err != nil {
			t.Fatal(err)
		}
		var ranks []string
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			ranks = append(ranks, r)
		}
		rows.Close()
		if want := domain.RankSpread(len(ranks)); !slices.Equal(ranks, want) {
			t.Fatalf("группа %q: ранги %v, RankSpread %v", g, ranks, want)
		}
	}
	// Тот же порядок — через список задач.
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		f := postgres.ListTasksFilter{Order: postgres.TaskListOrderBacklog, ParentKey: g, TopLevel: g == "", Limit: 500}
		tasks, err := store.ListTasks(ctx, store.DB(), project.ID, f)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(tasks))
		for i, tk := range tasks {
			got[i] = tk.Key
			if tk.Rank == "" {
				t.Fatalf("%s без ранга", tk.Key)
			}
		}
		if !slices.Equal(got, before[g]) {
			t.Fatalf("order=backlog группы %q: %v, ждали %v", g, got, before[g])
		}
	}

	// К-2: повторный прогон миграций на заполненной базе ничего не меняет.
	snapshot := groupKeys(t, pool, "", "t.rank, t.seq")
	var sumBefore, sumAfter string
	if err := pool.QueryRow(ctx, `SELECT string_agg(key || '=' || rank, ',' ORDER BY seq) FROM tasks`).Scan(&sumBefore); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, pool, testdb.Logger()); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT string_agg(key || '=' || rank, ',' ORDER BY seq) FROM tasks`).Scan(&sumAfter); err != nil {
		t.Fatal(err)
	}
	if sumAfter != sumBefore || !slices.Equal(groupKeys(t, pool, "", "t.rank, t.seq"), snapshot) {
		t.Fatal("повторный прогон миграций поменял ранги")
	}
}

func TestRankMigrationSchema(t *testing.T) {
	_, pool := testdb.Store(t)
	ctx := context.Background()

	check := func() {
		t.Helper()
		var typ, coll, nullable string
		if err := pool.QueryRow(ctx, `
			SELECT c.data_type, coalesce(c.collation_name, ''), c.is_nullable
			FROM information_schema.columns c
			WHERE c.table_name = 'tasks' AND c.column_name = 'rank'`).Scan(&typ, &coll, &nullable); err != nil {
			t.Fatal(err)
		}
		if typ != "text" || coll != "C" || nullable != "NO" {
			t.Fatalf("колонка rank: %s %s nullable=%s", typ, coll, nullable)
		}
		var def string
		if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'tasks_rank_group'`).Scan(&def); err != nil {
			t.Fatalf("индекс группы: %v", err)
		}
		if want := "(project_id, parent_id, rank, seq)"; !strings.Contains(def, want) {
			t.Fatalf("индекс %s, ждали %s", def, want)
		}
	}
	check()
	// up → down → up на засиженной базе.
	execRankMigration(t, pool, ".down.sql")
	execRankMigration(t, pool, ".up.sql")
	check()

	// Ограничения — негативной проверкой: пустой, NULL, на младшую цифру,
	// не из алфавита.
	for _, bad := range []any{nil, "", "V0", "V-", "Ж"} {
		_, err := pool.Exec(ctx, `
			INSERT INTO tasks (project_id, key, seq, title, workflow_id, status_id, rank)
			SELECT project_id, 'SDLC-900', 900, 'x', workflow_id, status_id, $1 FROM tasks WHERE key = 'SDLC-1'`, bad)
		if err == nil {
			t.Fatalf("ранг %v принят", bad)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (project_id, key, seq, title, workflow_id, status_id, rank)
		SELECT project_id, 'SDLC-900', 900, 'x', workflow_id, status_id, '0V' FROM tasks WHERE key = 'SDLC-1'`); err != nil {
		t.Fatalf("ранг 0V: %v", err)
	}

	// Сравнение в базе совпадает со сравнением байтов в Go (К-14).
	keys := []string{"0V", "1", "A", "Z", "a", "z", "V", "VV", "V1", "a0b", "Zz", "zz"}
	rows, err := pool.Query(ctx, `SELECT k FROM unnest($1::text[]) k ORDER BY k COLLATE "C"`, keys)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		got = append(got, k)
	}
	rows.Close()
	want := slices.Clone(keys)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("порядок базы %v, Go %v", got, want)
	}
	// Колонка сравнивает так же без явного COLLATE.
	var lt bool
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT rank FROM tasks WHERE key = 'SDLC-900') < (SELECT rank FROM tasks WHERE key = 'SDLC-4')`).Scan(&lt); err != nil || !lt {
		t.Fatalf("0V < V в колонке: %v %v", lt, err)
	}
}
