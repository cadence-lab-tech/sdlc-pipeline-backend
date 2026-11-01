package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// CLAB-353: миграция 000037 возвращает `type` задачам, которым его стёрла
// замена свойств. Схема в тесте уже накатана целиком, поэтому SQL миграции
// выполняется повторно поверх испорченных данных — ровно как это сделает
// сервис клиента при апгрейде.
func TestMigrationRestoresErasedTaskType(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	db := store.DB()

	sql, err := migrations.Schema.ReadFile("000037_restore_task_type.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	migrate := func() {
		t.Helper()
		if _, err := db.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("миграция 000037: %v", err)
		}
	}

	mk := func(title, properties string) string {
		t.Helper()
		task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{Title: title},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Порча данных, как её делала замена: свойства переписаны целиком.
		if _, err := db.Exec(ctx, `UPDATE tasks SET properties = $2::jsonb WHERE key = $1`, task.Key, properties); err != nil {
			t.Fatal(err)
		}
		return task.Key
	}
	read := func(key string) (props map[string]any, updated time.Time) {
		t.Helper()
		var raw json.RawMessage
		if err := db.QueryRow(ctx, `SELECT properties, updated_at FROM tasks WHERE key = $1`, key).Scan(&raw, &updated); err != nil {
			t.Fatal(err)
		}
		props = map[string]any{}
		if err := json.Unmarshal(raw, &props); err != nil {
			t.Fatal(err)
		}
		return props, updated
	}

	erased := mk("тип стёрт", `{"multi_repo": true}`)
	empty := mk("тип пустой", `{"type": "", "needs_migration": true}`)
	intact := mk("тип цел", `{"type": "bug", "multi_repo": true}`)

	// Цикл ведёт несколько типов — настоящий тип угадать нельзя, задачи не меняются.
	if _, err := db.Exec(ctx, `UPDATE workflows SET task_types = ARRAY['task','spike'] WHERE id = (SELECT workflow_id FROM tasks WHERE key = $1)`, erased); err != nil {
		t.Fatal(err)
	}
	migrate()
	if p, _ := read(erased); p["type"] != nil {
		t.Fatalf("при неоднозначном цикле тип выдуман: %v", p)
	}

	// Цикл с одним типом — тип однозначен.
	if _, err := db.Exec(ctx, `UPDATE workflows SET task_types = ARRAY['spike'] WHERE id = (SELECT workflow_id FROM tasks WHERE key = $1)`, erased); err != nil {
		t.Fatal(err)
	}
	before, _ := read(erased)
	_, updatedBefore := read(intact)
	migrate()

	if p, _ := read(erased); p["type"] != "spike" || p["multi_repo"] != true || len(p) != 2 {
		t.Fatalf("стёртый тип: %v", p)
	}
	if p, _ := read(empty); p["type"] != "spike" || p["needs_migration"] != true || len(p) != 2 {
		t.Fatalf("пустой тип: %v", p)
	}
	if p, updated := read(intact); p["type"] != "bug" || p["multi_repo"] != true || !updated.Equal(updatedBefore) {
		t.Fatalf("целый тип задет: %v, updated_at %v -> %v", p, updatedBefore, updated)
	}
	_ = before

	// Идемпотентность: второй прогон ничего не меняет, в том числе updated_at.
	_, firstUpdated := read(erased)
	migrate()
	if p, updated := read(erased); p["type"] != "spike" || !updated.Equal(firstUpdated) {
		t.Fatalf("повторный прогон: %v, updated_at %v -> %v", p, firstUpdated, updated)
	}

	// Восстановленная задача читается движком своим типом.
	task, err := store.GetTaskByKey(ctx, db, erased)
	if err != nil {
		t.Fatal(err)
	}
	if task.Type() != "spike" {
		t.Fatalf("Task.Type() = %q", task.Type())
	}
}
