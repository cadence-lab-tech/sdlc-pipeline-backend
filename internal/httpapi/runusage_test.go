package httpapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Учёт по прогону в карточке задачи (CLAB-264, решение 0026): у каждого из
// stage_runs в GET /v1/tasks/:key и sdlc_get_task — объект usage по строкам
// stage_run_usage этого прогона. Прогон через REST: одна строка, затем две
// модели и строка без стоимости; прогоны напрямую в базе: перепроверка с
// моделями как пришли, перепроверка с неполной строкой, этап без учёта и
// прогон дочерней задачи.
func TestRunUsageInTaskCard(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	store := postgres.New(f.pool)

	for _, path := range []string{"/v1/tasks/SDLC-1/transitions/start", "/v1/tasks/SDLC-1/claim"} {
		if code, body := f.json("POST", path, f.root, map[string]any{}); code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
	task, err := store.GetTaskByKey(ctx, f.pool, "SDLC-1")
	if err != nil {
		t.Fatal(err)
	}

	card := func() (runs map[string]map[string]any, order []string, usage map[string]any) {
		t.Helper()
		code, body := f.json("GET", "/v1/tasks/SDLC-1", f.root, nil)
		if code != http.StatusOK {
			t.Fatalf("карточка: %d %v", code, body)
		}
		runs = map[string]map[string]any{}
		for _, raw := range body["stage_runs"].([]any) {
			r := raw.(map[string]any)
			runs[r["id"].(string)] = r
			order = append(order, r["id"].(string))
		}
		return runs, order, body["usage"].(map[string]any)
	}
	usageOf := func(runs map[string]map[string]any, id uuid.UUID) map[string]any {
		t.Helper()
		r, ok := runs[id.String()]
		if !ok {
			t.Fatalf("прогона %s нет в stage_runs", id)
		}
		u, ok := r["usage"].(map[string]any)
		if !ok {
			t.Fatalf("у прогона %s нет usage: %v", id, r)
		}
		return u
	}
	// want — ожидаемый usage; числа JSON приходят float64, null — nil.
	check := func(name string, got map[string]any, want map[string]any) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: поля usage %v, ожидали %v", name, got, want)
		}
		for k, w := range want {
			g, has := got[k]
			if !has {
				t.Errorf("%s: нет поля %s в %v", name, k, got)
				continue
			}
			if gf, ok := g.(float64); ok {
				if wf, ok := w.(float64); ok && math.Abs(gf-wf) < 1e-9 {
					continue
				}
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s: %s = %#v, ожидали %#v", name, k, g, w)
			}
		}
	}
	none := map[string]any{
		"reported": false, "reports": float64(0), "models": []any{},
		"input_tokens": nil, "output_tokens": nil, "cache_read_tokens": nil,
		"cache_write_tokens": nil, "cost_usd": nil, "duration_ms": nil,
	}
	with := func(over map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range none {
			out[k] = v
		}
		for k, v := range over {
			out[k] = v
		}
		return out
	}

	// AC-1, AC-4: до отчётов usage есть у каждого прогона, «нет учёта» — не
	// ноль; прежние поля прогона те же, что отдаёт ListStageRuns.
	runs, order, _ := card()
	plain, err := store.ListStageRuns(ctx, f.pool, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != len(order) {
		t.Fatalf("прогонов в карточке %d, в хранилище %d", len(order), len(plain))
	}
	for i, r := range plain {
		raw, _ := json.Marshal(r)
		want := map[string]any{}
		_ = json.Unmarshal(raw, &want)
		got := map[string]any{}
		for k, v := range runs[order[i]] {
			if k != "usage" && k != "role_version" && k != "gate_results" && k != "exit" {
				got[k] = v
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("прежние поля прогона %d изменились:\n%v\n%v", i, got, want)
		}
		check("до отчётов", usageOf(runs, r.ID), none)
	}

	// AC-2, AC-10: строка ложится на прогон, который держит актор, и id в
	// ответе — id того прогона, у которого вырос reports.
	report := func(body map[string]any) uuid.UUID {
		t.Helper()
		code, out := f.json("POST", "/v1/tasks/SDLC-1/usage", f.root, body)
		if code != http.StatusCreated {
			t.Fatalf("usage %v: %d %v", body, code, out)
		}
		id, err := uuid.Parse(out["run_id"].(string))
		if err != nil {
			t.Fatalf("run_id в ответе: %v", out)
		}
		return id
	}
	runA := report(map[string]any{"model": "m1", "input_tokens": 100, "output_tokens": 20, "cost_usd": 0.5})
	runs, _, _ = card()
	check("одна строка", usageOf(runs, runA), with(map[string]any{
		"reported": true, "reports": float64(1), "models": []any{"m1"},
		"input_tokens": float64(100), "output_tokens": float64(20), "cost_usd": 0.5,
	}))

	// AC-3, AC-5: вторая модель, затем повтор первой без стоимости.
	if got := report(map[string]any{"model": "m2", "input_tokens": 50, "cost_usd": 0.25}); got != runA {
		t.Fatalf("вторая строка на другом прогоне: %s", got)
	}
	runs, _, _ = card()
	check("две строки", usageOf(runs, runA), with(map[string]any{
		"reported": true, "reports": float64(2), "models": []any{"m1", "m2"},
		"input_tokens": float64(150), "output_tokens": float64(20), "cost_usd": 0.75,
	}))
	report(map[string]any{"model": "m1", "output_tokens": 5})
	runs, _, _ = card()
	wantA := with(map[string]any{
		"reported": true, "reports": float64(3), "models": []any{"m1", "m2"},
		"input_tokens": float64(150), "output_tokens": float64(25), "cost_usd": 0.75,
	})
	check("строка без стоимости", usageOf(runs, runA), wantA)

	// Прогоны в базе напрямую: перепроверки под runA, этап на другом статусе
	// без учёта, прогон дочерней задачи.
	var statusID, otherStatus, gateID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT status_id FROM stage_runs WHERE id = $1`, runA).Scan(&statusID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT id FROM workflow_statuses WHERE workflow_id = $1 AND id <> $2 ORDER BY position LIMIT 1`,
		task.WorkflowID, statusID).Scan(&otherStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT g.id FROM gates g JOIN workflow_transitions tr ON tr.id = g.transition_id
		WHERE tr.workflow_id = $1 LIMIT 1`, task.WorkflowID).Scan(&gateID); err != nil {
		t.Fatal(err)
	}
	review := func(attempt int) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO stage_runs (task_id, status_id, kind, parent_run_id, gate_id, attempt, state)
			VALUES ($1, $2, 'review', $3, $4, $5, 'passed') RETURNING id`,
			task.ID, statusID, runA, gateID, attempt).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	row := func(run uuid.UUID, model string, cols map[string]any) {
		t.Helper()
		in := postgres.UsageInput{RunID: run, Model: model, ReportedBy: f.admin.Principal.ID}
		for k, v := range cols {
			switch k {
			case "input":
				n := int64(v.(int))
				in.InputTokens = &n
			case "duration":
				n := int64(v.(int))
				in.DurationMS = &n
			case "cost":
				c := v.(float64)
				in.CostUSD = &c
			}
		}
		if _, err := store.RecordUsage(ctx, f.pool, in); err != nil {
			t.Fatal(err)
		}
	}

	// AC-6, AC-11: учёт ревьюера на своём прогоне, модели как пришли.
	runR := review(1)
	row(runR, "opus", map[string]any{"input": 10, "cost": 0.1})
	row(runR, "claude-opus-5-5", map[string]any{"duration": 1000})
	// AC-5: единственная строка без стоимости — cost_usd null при reported.
	runN := review(2)
	row(runN, "m3", map[string]any{"input": 7})
	// AC-4: этап без учёта рядом с отчитавшимися.
	var runS uuid.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO stage_runs (task_id, status_id, attempt, state)
		SELECT $1, $2, coalesce(max(attempt), 0) + 1, 'passed' FROM stage_runs
		WHERE task_id = $1 AND status_id = $2 AND kind = 'stage' RETURNING id`,
		task.ID, otherStatus).Scan(&runS); err != nil {
		t.Fatal(err)
	}
	// AC-12: прогон дочерней задачи (её прогон на начальном статусе) в
	// stage_runs родителя не попадает.
	code, child := f.json("POST", "/v1/projects/SDLC/tasks", f.root, map[string]any{"title": "Ребёнок", "parent_key": "SDLC-1"})
	if code != http.StatusCreated {
		t.Fatalf("ребёнок: %d %v", code, child)
	}
	childTask, err := store.GetTaskByKey(ctx, f.pool, child["key"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var runC uuid.UUID
	if err := f.pool.QueryRow(ctx, `
		SELECT id FROM stage_runs WHERE task_id = $1 AND status_id = $2 AND kind = 'stage'`,
		childTask.ID, childTask.StatusID).Scan(&runC); err != nil {
		t.Fatal(err)
	}
	row(runC, "m1", map[string]any{"cost": 2.0})

	runs, order, taskUsage := card()
	check("родительский прогон после перепроверок", usageOf(runs, runA), wantA)
	check("перепроверка", usageOf(runs, runR), with(map[string]any{
		"reported": true, "reports": float64(2), "models": []any{"opus", "claude-opus-5-5"},
		"input_tokens": float64(10), "cost_usd": 0.1, "duration_ms": float64(1000),
	}))
	check("строка без стоимости и токенов вывода", usageOf(runs, runN), with(map[string]any{
		"reported": true, "reports": float64(1), "models": []any{"m3"}, "input_tokens": float64(7),
	}))
	check("этап без учёта", usageOf(runs, runS), none)
	if _, has := runs[runC.String()]; has {
		t.Errorf("прогон дочерней задачи в stage_runs родителя")
	}
	if runs[runR.String()]["kind"] != "review" || runs[runR.String()]["parent_run_id"] != runA.String() {
		t.Errorf("перепроверка в карточке: %v", runs[runR.String()])
	}

	// AC-8: снимок уровня рядом с учётом у взятого прогона, у вставленного без
	// снимка пары нет, а учёт считается.
	if runs[runA.String()]["model_level_source"] != "none" {
		t.Errorf("снимок у прогона с учётом: %v", runs[runA.String()])
	}
	if _, has := runs[runR.String()]["model_level_source"]; has {
		t.Errorf("снимок у прогона без снимка: %v", runs[runR.String()])
	}

	// AC-10: сумма по своим прогонам равна стоимости задачи; поддерево и
	// счётчики UsageSummary — как прежде, GET /tasks/:key/usage — то же.
	var sum float64
	silent := 0
	for _, id := range order {
		u := runs[id]["usage"].(map[string]any)
		if c, ok := u["cost_usd"].(float64); ok {
			sum += c
		}
		if u["reported"] == false {
			silent++
		}
	}
	if math.Abs(sum-taskUsage["cost_usd"].(float64)) > 1e-9 || math.Abs(sum-0.85) > 1e-9 {
		t.Errorf("сумма по прогонам %v, у задачи %v", sum, taskUsage["cost_usd"])
	}
	if math.Abs(taskUsage["subtree_cost_usd"].(float64)-2.85) > 1e-9 {
		t.Errorf("стоимость поддерева: %v", taskUsage)
	}
	if taskUsage["reported_runs"] != float64(4) || taskUsage["silent_runs"] != float64(silent) {
		t.Errorf("счётчики прогонов: %v, без учёта в карточке %d", taskUsage, silent)
	}
	if code, body := f.json("GET", "/v1/tasks/SDLC-1/usage", f.root, nil); code != http.StatusOK || !reflect.DeepEqual(body, taskUsage) {
		t.Errorf("GET /tasks/:key/usage: %d %v, в карточке %v", code, body, taskUsage)
	}

	// AC-9: учёт всех прогонов — одна функция хранилища; прогонов без строк и
	// чужих задач в ней нет.
	byRun, err := store.RunUsageByTask(ctx, f.pool, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(byRun) != 3 {
		t.Errorf("RunUsageByTask: %d прогонов, ожидали 3: %v", len(byRun), byRun)
	}
	if _, has := byRun[runC]; has {
		t.Errorf("RunUsageByTask взял прогон дочерней задачи")
	}
	if u := byRun[runR]; !u.Reported || u.Reports != 2 || u.OutputTokens != nil {
		t.Errorf("RunUsageByTask перепроверки: %+v", u)
	}
	if u := domain.NoRunUsage(); u.Models == nil {
		t.Errorf("NoRunUsage: models nil")
	}

	// AC-7: sdlc_get_task отдаёт тот же usage по id прогона.
	out, isErr := f.mcp(f.root, "sdlc_get_task", map[string]any{"task_key": "SDLC-1"})
	if isErr {
		t.Fatalf("sdlc_get_task: %v", out)
	}
	mcpRuns := out["stage_runs"].([]any)
	if len(mcpRuns) != len(order) {
		t.Fatalf("sdlc_get_task: прогонов %d, в REST %d", len(mcpRuns), len(order))
	}
	for _, raw := range mcpRuns {
		r := raw.(map[string]any)
		id := r["id"].(string)
		if !reflect.DeepEqual(r["usage"], runs[id]["usage"]) {
			t.Errorf("usage прогона %s: MCP %v, REST %v", id, r["usage"], runs[id]["usage"])
		}
	}
}
