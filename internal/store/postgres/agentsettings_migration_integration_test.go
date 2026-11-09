package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Миграция 000047_agent_settings (CLAB-425, AC-1, AC-2, AC-21): база с
// данными на 000046 после 000047 читается целиком, у прогонов снимок NULL;
// down снимает таблицу и колонку без потерь остального; индексы держат «не
// больше одного черновика и одной активной» в обход сервиса; удаление
// проекта уносит его версии и не трогает чужие.

const agentSettingsMigration = "000047_agent_settings"

func execAgentSettingsMigration(t *testing.T, pool *pgxpool.Pool, suffix string) {
	t.Helper()
	body, err := migrations.Schema.ReadFile(agentSettingsMigration + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("%s%s: %v", agentSettingsMigration, suffix, err)
	}
}

func TestAgentSettingsMigration(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()
	scalar := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	// База «как на 000046»: снимаем 000047 и кладём данные прежнего релиза —
	// закрытый и идущий прогоны и строку учёта. Откат не теряет остального:
	// проекты, задачи, прогоны и артефакты демо-данных на месте.
	kept := []string{"projects", "tasks", "stage_runs", "artifacts"}
	seeded := map[string]int{}
	for _, table := range kept {
		seeded[table] = scalar(`SELECT count(*) FROM ` + table)
	}
	execAgentSettingsMigration(t, pool, ".down.sql")
	for _, table := range kept {
		if got := scalar(`SELECT count(*) FROM ` + table); got != seeded[table] {
			t.Fatalf("откат 000047 потерял строки %s: %d, было %d", table, got, seeded[table])
		}
	}
	if n := scalar(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'stage_runs' AND column_name = 'agent_settings_version_id'`); n != 0 {
		t.Fatalf("после down колонка осталась")
	}
	if n := scalar(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'agent_settings_versions'`); n != 0 {
		t.Fatalf("после down таблица осталась")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO stage_runs (task_id, status_id, attempt, state, outcome, finished_at)
		SELECT t.id, ws.id, 300, 'passed', 'advanced', now()
		FROM tasks t JOIN workflow_statuses ws ON ws.workflow_id = t.workflow_id AND ws.key = 'analysis'
		WHERE t.key = 'SDLC-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO stage_runs (task_id, status_id, attempt, state, model_level, model_level_source)
		SELECT t.id, ws.id, 301, 'claimed', 'high', 'task'
		FROM tasks t JOIN workflow_statuses ws ON ws.workflow_id = t.workflow_id AND ws.key = 'analysis'
		WHERE t.key = 'SDLC-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO stage_run_usage (run_id, model, input_tokens, cost_usd)
		SELECT id, 'claude-opus-5-5', 10, 0.5 FROM stage_runs WHERE attempt = 301`); err != nil {
		t.Fatal(err)
	}
	runs := scalar(`SELECT count(*) FROM stage_runs`)
	usage := scalar(`SELECT count(*) FROM stage_run_usage`)

	// up: старые строки на месте и читаются, снимок у всех прогонов NULL.
	execAgentSettingsMigration(t, pool, ".up.sql")
	if got := scalar(`SELECT count(*) FROM stage_runs`); got != runs {
		t.Fatalf("прогонов %d, было %d", got, runs)
	}
	if got := scalar(`SELECT count(*) FROM stage_run_usage`); got != usage {
		t.Fatalf("строк учёта %d, было %d", got, usage)
	}
	if n := scalar(`SELECT count(*) FROM stage_runs WHERE agent_settings_version_id IS NOT NULL`); n != 0 {
		t.Fatalf("у %d прогонов снимок не NULL", n)
	}
	if n := scalar(`SELECT count(*) FROM stage_runs WHERE attempt IN (300, 301) AND model_level_source IS NOT DISTINCT FROM CASE attempt WHEN 301 THEN 'task' END`); n != 2 {
		t.Fatalf("прежние колонки прогонов изменились: %d", n)
	}

	project := func(key string) string {
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE key = $1`, key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	sdlc := project("SDLC")
	insert := func(projectID string, version int, state string) error {
		_, err := pool.Exec(ctx, `INSERT INTO agent_settings_versions (project_id, version, state) VALUES ($1, $2, $3)`,
			projectID, version, state)
		return err
	}

	// AC-2: второй draft и второй active падают, retired — сколько угодно,
	// пара (проект, номер) уникальна, у другого проекта номера свои.
	for _, step := range []struct {
		version int
		state   string
		fail    bool
	}{
		{1, "retired", false}, {2, "retired", false}, {3, "active", false}, {4, "active", true},
		{4, "draft", false}, {5, "draft", true}, {3, "retired", true}, {0, "retired", true}, {6, "bogus", true},
	} {
		err := insert(sdlc, step.version, step.state)
		if (err != nil) != step.fail {
			t.Fatalf("версия %d %s: %v, ожидали отказ %v", step.version, step.state, err, step.fail)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_settings_versions (project_id, version, state, document) VALUES ($1, 9, 'retired', '[]')`, sdlc); err == nil {
		t.Fatal("документ-массив принят")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (key, name, owner_team_id) SELECT 'OTHER', 'Other', owner_team_id FROM projects WHERE key = 'SDLC'`); err != nil {
		t.Fatal(err)
	}
	other := project("OTHER")
	for _, v := range []struct {
		n     int
		state string
	}{{3, "active"}, {4, "draft"}} {
		if err := insert(other, v.n, v.state); err != nil {
			t.Fatalf("другой проект, версия %d: %v", v.n, err)
		}
	}

	// Снимок ссылается на версию; удаление проекта уносит задачи, прогоны и
	// версии без нарушения ключей, версии другого проекта на месте (AC-21).
	if _, err := pool.Exec(ctx, `
		UPDATE stage_runs SET agent_settings_version_id =
		  (SELECT id FROM agent_settings_versions WHERE project_id = $1 AND state = 'active')
		WHERE attempt = 301`, sdlc); err != nil {
		t.Fatal(err)
	}
	if err := store.InTx(ctx, func(tx pgx.Tx) error {
		id, err := uuid.Parse(sdlc)
		if err != nil {
			return err
		}
		return store.DeleteProject(ctx, tx, id)
	}); err != nil {
		t.Fatalf("удаление проекта: %v", err)
	}
	if n := scalar(`SELECT count(*) FROM agent_settings_versions WHERE project_id = $1`, sdlc); n != 0 {
		t.Fatalf("версии удалённого проекта остались: %d", n)
	}
	if n := scalar(`SELECT count(*) FROM agent_settings_versions WHERE project_id = $1`, other); n != 2 {
		t.Fatalf("версии другого проекта: %d", n)
	}

	// Повторный down → up на базе с версиями: откат снимает всё, накат снова чист.
	execAgentSettingsMigration(t, pool, ".down.sql")
	execAgentSettingsMigration(t, pool, ".up.sql")
	if n := scalar(`SELECT count(*) FROM agent_settings_versions`); n != 0 {
		t.Fatalf("после down → up версий %d", n)
	}
}
