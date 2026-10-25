package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Карточка задачи и лента этапа (CLAB-283, AC-1…AC-3, AC-5, AC-11): у прогона
// в stage_runs — номер версии роли, вердикты гейтов из gate_results и выход
// (переход, целевой статус, комментарий). Прогон идёт по настоящему движку:
// старт с комментарием, заблокированный переход (движок пишет вердикты),
// решение человека по апруву, отмена; откат — на второй задаче.

// cardRuns разбирает stage_runs карточки в порядке ответа.
func cardRuns(t *testing.T, f *auditFixture, key string) []map[string]any {
	t.Helper()
	code, body := f.json("GET", "/v1/tasks/"+key, f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("карточка %s: %d %v", key, code, body)
	}
	var out []map[string]any
	for _, raw := range body["stage_runs"].([]any) {
		out = append(out, raw.(map[string]any))
	}
	return out
}

func runOf(t *testing.T, runs []map[string]any, status, kind string) map[string]any {
	t.Helper()
	for _, r := range runs {
		if r["status_key"] == status && r["kind"] == kind {
			return r
		}
	}
	t.Fatalf("в карточке нет прогона %s/%s: %v", status, kind, runs)
	return nil
}

func TestCardRunRoleVersionGatesAndExit(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	// Старт с комментарием и захват: прогон new закончен переходом start.
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/start", f.root, map[string]any{"comment": "поехали"}); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/claim", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("claim: %d %v", code, body)
	}
	// Переход без артефактов блокируется, и движок записывает вердикты гейтов.
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/analysis_done", f.root, nil); code != http.StatusConflict {
		t.Fatalf("analysis_done без артефактов: %d %v", code, body)
	}
	// Решение человека по апруву: хэндл решившего и время.
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/analysis_done/gates/human_approval/decision", f.root,
		map[string]any{"status": "passed", "details": map[string]any{"note": "ок"}}); code != http.StatusOK {
		t.Fatalf("апрув: %d %v", code, body)
	}

	runs := cardRuns(t, f, "SDLC-1")
	analysis := runOf(t, runs, "analysis", "stage")

	// AC-1: номер совпадает с role_versions.version по role_version_id.
	var version int
	if err := f.pool.QueryRow(ctx, `
		SELECT version FROM role_versions WHERE id = $1`, analysis["role_version_id"]).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if analysis["role_version"] != float64(version) {
		t.Errorf("role_version = %v, в базе %d", analysis["role_version"], version)
	}
	// Прогон без роли — null, а не ошибка.
	initial := runOf(t, runs, "new", "stage")
	if v, has := initial["role_version"]; !has || v != nil {
		t.Errorf("role_version у прогона без роли: %v (есть=%v), ждали null", v, has)
	}

	// AC-2: вердикты — ключ, имя, тип гейта, ключ перехода, статус, details.
	gates, _ := analysis["gate_results"].([]any)
	byKey := map[string]map[string]any{}
	for _, raw := range gates {
		g := raw.(map[string]any)
		byKey[g["gate_key"].(string)] = g
	}
	art, ok := byKey["artifacts"]
	if !ok {
		t.Fatalf("нет вердикта гейта artifacts: %v", gates)
	}
	if art["gate_type"] != "artifacts_required" || art["transition_key"] != "analysis_done" ||
		art["status"] != "failed" || art["gate_name"] == "" || art["decided_by_handle"] != nil {
		t.Errorf("вердикт движка: %v", art)
	}
	if _, isObj := art["details"].(map[string]any); !isObj {
		t.Errorf("details не объект: %v", art["details"])
	}
	appr, ok := byKey["human_approval"]
	if !ok {
		t.Fatalf("нет вердикта human_approval: %v", gates)
	}
	if appr["status"] != "passed" || appr["decided_by_handle"] != "root" || appr["decided_at"] == nil ||
		appr["gate_type"] != "human_approval" || appr["transition_key"] != "analysis_done" {
		t.Errorf("решение человека: %v", appr)
	}
	if d, _ := appr["details"].(map[string]any); d["note"] != "ок" {
		t.Errorf("details решения: %v", appr["details"])
	}
	// Прогон без gate_results — пустой список, не null.
	if g, ok := initial["gate_results"].([]any); !ok || len(g) != 0 {
		t.Errorf("gate_results прогона без вердиктов: %#v", initial["gate_results"])
	}

	// AC-3: выход закончившегося прогона; идущий — null.
	exit, _ := initial["exit"].(map[string]any)
	if exit == nil || exit["kind"] != "advanced" || exit["transition"] != "start" ||
		exit["to_status"] != "analysis" || exit["comment"] != "поехали" || exit["by_handle"] != "root" || exit["at"] == nil {
		t.Errorf("выход прогона new: %v", initial["exit"])
	}
	if v, has := analysis["exit"]; !has || v != nil {
		t.Errorf("выход идущего прогона: %v (есть=%v)", v, has)
	}

	// Отмена без комментария: выход есть, comment — null.
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/cancel", f.root, nil); code != http.StatusOK {
		t.Fatalf("cancel: %d %v", code, body)
	}
	analysis = runOf(t, cardRuns(t, f, "SDLC-1"), "analysis", "stage")
	exit, _ = analysis["exit"].(map[string]any)
	if exit == nil || exit["kind"] != "cancelled" || exit["transition"] != "cancel" ||
		exit["to_status"] != "cancelled" || exit["comment"] != nil {
		t.Errorf("выход отменённого прогона: %v", analysis["exit"])
	}
	// Вердикты не потерялись и после закрытия прогона.
	if g, _ := analysis["gate_results"].([]any); len(g) != len(gates) {
		t.Errorf("вердиктов после отмены %d, было %d", len(g), len(gates))
	}
}

// AC-3 (откат), AC-5, AC-11: возврат с причиной; прежние поля прогона и
// карточки не меняются, секретов в новых полях нет, MCP отдаёт то же самое.
func TestCardRunRejectExitAndCompat(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	// SDLC-2 ставится на статус development напрямую (путь до него идёт через
	// апрувы и перепроверки, здесь они не предмет проверки).
	var taskID, devStatus uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM tasks WHERE key = 'SDLC-2'`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT s.id FROM workflow_statuses s JOIN tasks t ON t.workflow_id = s.workflow_id
		WHERE t.id = $1 AND s.key = 'development'`, taskID).Scan(&devStatus); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tasks SET status_id = $2 WHERE id = $1`, taskID, devStatus); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO stage_runs (task_id, status_id, kind, role_id, role_version_id, attempt, state)
		SELECT $1, $2, 'stage', ro.id, rv.id, 1, 'pending'
		FROM status_roles sro JOIN roles ro ON ro.id = sro.role_id
		JOIN role_versions rv ON rv.role_id = ro.id
		WHERE sro.status_id = $2 AND sro.is_primary ORDER BY rv.version DESC LIMIT 1`, taskID, devStatus); err != nil {
		t.Fatal(err)
	}

	if code, body := f.json("POST", "/v1/tasks/SDLC-2/claim", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("claim: %d %v", code, body)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-2/artifacts", f.root, map[string]any{
		"kind": "problem.report", "title": "Проблема", "content": "ADR не сходится с кодом",
	}); code != http.StatusCreated {
		t.Fatalf("problem.report: %d %v", code, body)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-2/transitions/reject_to_architecture", f.root,
		map[string]any{"comment": "ADR не сходится"}); code != http.StatusOK {
		t.Fatalf("откат: %d %v", code, body)
	}

	runs := cardRuns(t, f, "SDLC-2")
	dev := runOf(t, runs, "development", "stage")
	arc := runOf(t, runs, "architecture", "stage")
	exit, _ := dev["exit"].(map[string]any)
	if exit == nil || exit["kind"] != "rejected" || exit["transition"] != "reject_to_architecture" ||
		exit["to_status"] != "architecture" || exit["comment"] != "ADR не сходится" {
		t.Errorf("выход прогона development: %v", dev["exit"])
	}
	// Откат: у нового прогона rejected_from_run_id — прогон, откативший задачу.
	if arc["rejected_from_run_id"] != dev["id"] {
		t.Errorf("rejected_from_run_id = %v, ожидали %v", arc["rejected_from_run_id"], dev["id"])
	}
	if v, has := arc["exit"]; !has || v != nil {
		t.Errorf("выход нового прогона: %v", v)
	}
	if arc["role_version"] == nil {
		t.Errorf("у нового прогона с ролью нет role_version: %v", arc)
	}

	// AC-5: карточка отличается от прежнего ответа (ListStageRuns + usage)
	// только добавленными ключами; значения прежних полей те же.
	plain, err := postgres.New(f.pool).ListStageRuns(ctx, f.pool, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != len(runs) {
		t.Fatalf("прогонов в карточке %d, в хранилище %d", len(runs), len(plain))
	}
	for i, r := range plain {
		raw, _ := json.Marshal(r)
		want := map[string]any{}
		_ = json.Unmarshal(raw, &want)
		got := map[string]any{}
		for k, v := range runs[i] {
			if k != "usage" && k != "role_version" && k != "gate_results" && k != "exit" {
				got[k] = v
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("прежние поля прогона %d изменились:\n%v\n%v", i, got, want)
		}
		for _, k := range []string{"usage", "role_version", "gate_results", "exit"} {
			if _, has := runs[i][k]; !has {
				t.Errorf("в прогоне %d нет ключа %s", i, k)
			}
		}
	}
	// AC-11: в новых полях нет токенов, хэшей и секретов.
	raw, _ := json.Marshal(map[string]any{
		"role_version": dev["role_version"], "gate_results": dev["gate_results"], "exit": dev["exit"],
	})
	for _, bad := range []string{"token", "hash", "secret"} {
		if strings.Contains(strings.ToLower(string(raw)), bad) {
			t.Errorf("в новых полях встретилось %q: %s", bad, raw)
		}
	}

	// MCP sdlc_get_task отдаёт те же прогоны, что REST.
	out, isErr := f.mcp(f.root, "sdlc_get_task", map[string]any{"task_key": "SDLC-2"})
	if isErr {
		t.Fatalf("sdlc_get_task: %v", out)
	}
	mcpRuns, _ := out["stage_runs"].([]any)
	if len(mcpRuns) != len(runs) {
		t.Fatalf("sdlc_get_task: прогонов %d, в REST %d", len(mcpRuns), len(runs))
	}
	for i, m := range mcpRuns {
		r := m.(map[string]any)
		for _, k := range []string{"role_version", "gate_results", "exit"} {
			if !reflect.DeepEqual(r[k], runs[i][k]) {
				t.Errorf("прогон %v, поле %s: MCP %v, REST %v", r["id"], k, r[k], runs[i][k])
			}
		}
	}
}

// AC-6…AC-8: лента этапа. Статус development с вложенной перепроверкой и
// больше 500 heartbeat'ов, три события другого статуса; предел после
// фильтра, незнакомый ключ — пустая лента, без параметра — как раньше.
func TestTimelineStatusFilter(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	for _, path := range []string{"/v1/tasks/SDLC-1/transitions/start", "/v1/tasks/SDLC-1/claim"} {
		if code, body := f.json("POST", path, f.root, map[string]any{}); code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
	var taskID, runAnalysis, statusDev, gateID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM tasks WHERE key = 'SDLC-1'`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT sr.id FROM stage_runs sr JOIN workflow_statuses s ON s.id = sr.status_id
		WHERE sr.task_id = $1 AND s.key = 'analysis'`, taskID).Scan(&runAnalysis); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT s.id FROM workflow_statuses s JOIN tasks t ON t.workflow_id = s.workflow_id
		WHERE t.id = $1 AND s.key = 'development'`, taskID).Scan(&statusDev); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT g.id FROM gates g JOIN workflow_transitions tr ON tr.id = g.transition_id
		JOIN tasks t ON t.workflow_id = tr.workflow_id WHERE t.id = $1 LIMIT 1`, taskID).Scan(&gateID); err != nil {
		t.Fatal(err)
	}
	// Прогон development с перепроверкой под ним; heartbeat'ы — на нём, чтобы
	// они вытеснили из последних 500 события более раннего статуса.
	var runDev, runReview uuid.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO stage_runs (task_id, status_id, kind, attempt, state)
		VALUES ($1, $2, 'stage', 1, 'in_progress') RETURNING id`, taskID, statusDev).Scan(&runDev); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO stage_runs (task_id, status_id, kind, parent_run_id, gate_id, attempt, state)
		VALUES ($1, $2, 'review', $3, $4, 1, 'passed') RETURNING id`, taskID, statusDev, runDev, gateID).Scan(&runReview); err != nil {
		t.Fatal(err)
	}
	// События analysis старше всех heartbeat'ов.
	if _, err := f.pool.Exec(ctx, `
		UPDATE task_events SET created_at = now() - interval '1 day' WHERE task_id = $1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO task_events (task_id, stage_run_id, type, payload, created_at)
		SELECT $1, $2, 'lease.extended', '{}', now() - interval '1 hour' + n * interval '1 second'
		FROM generate_series(1, 520) n`, taskID, runDev); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO task_events (task_id, stage_run_id, type, payload, created_at)
		VALUES ($1, $2, 'artifact.submitted', '{}', now() - interval '30 minutes'),
		       ($1, $3, 'gate.review_started', '{}', now() - interval '29 minutes')`,
		taskID, runDev, runReview); err != nil {
		t.Fatal(err)
	}

	events := func(query string) []map[string]any {
		t.Helper()
		code, body := f.json("GET", "/v1/tasks/SDLC-1/timeline"+query, f.root, nil)
		if code != http.StatusOK {
			t.Fatalf("timeline%s: %d %v", query, code, body)
		}
		list, ok := body["events"].([]any)
		if !ok {
			t.Fatalf("timeline%s: events не список: %v", query, body)
		}
		var out []map[string]any
		for _, e := range list {
			out = append(out, e.(map[string]any))
		}
		return out
	}

	// AC-7: без параметра — прежнее поведение (предел 200 по умолчанию, свежие
	// сверху), событий analysis в нём нет — их вытеснили heartbeat'ы.
	all := events("")
	if len(all) != 200 {
		t.Errorf("без параметра: %d событий, ожидали 200", len(all))
	}
	for _, e := range all {
		if e["stage_run_id"] == runAnalysis.String() {
			t.Fatalf("события analysis не должны попадать в последние 200: %v", e)
		}
	}
	// AC-7: предел после фильтра — события analysis приходят все.
	ana := events("?status=analysis&limit=500")
	if len(ana) == 0 {
		t.Fatal("status=analysis: пусто, а события прогона есть")
	}
	for _, e := range ana {
		if e["stage_run_id"] != runAnalysis.String() {
			t.Errorf("в status=analysis чужое событие: %v", e)
		}
	}
	types := map[string]bool{}
	for _, e := range ana {
		types[e["type"].(string)] = true
	}
	if !types["lease.claimed"] && !types["stage.entered"] {
		t.Errorf("в status=analysis нет событий входа/аренды: %v", types)
	}

	// AC-6: development — события этапа и вложенной перепроверки; предел
	// применяется после фильтра и по-прежнему ограничен 500.
	dev := events("?status=development&limit=500")
	if len(dev) != 500 {
		t.Errorf("status=development&limit=500: %d событий", len(dev))
	}
	devSmall := events("?status=development&limit=3")
	if len(devSmall) != 3 {
		t.Fatalf("status=development&limit=3: %d событий", len(devSmall))
	}
	// Свежие сверху: самое свежее событие development — перепроверки, затем
	// сдача артефакта.
	if devSmall[0]["stage_run_id"] != runReview.String() || devSmall[0]["type"] != "gate.review_started" {
		t.Errorf("вложенная перепроверка не в ленте этапа: %v", devSmall[0])
	}
	if devSmall[1]["type"] != "artifact.submitted" {
		t.Errorf("второе событие: %v", devSmall[1])
	}
	for _, e := range dev {
		if e["stage_run_id"] == runAnalysis.String() {
			t.Fatalf("события analysis попали в status=development: %v", e)
		}
	}

	// AC-8: незнакомый статус — пустой список, код 200.
	for _, q := range []string{"?status=nope", "?status=nope&limit=10", "?status=ANALYSIS"} {
		if got := events(q); len(got) != 0 {
			t.Errorf("%s: %d событий, ожидали 0", q, len(got))
		}
	}
	if rec := f.do("GET", "/v1/tasks/SDLC-1/timeline?status=nope", f.root, nil); !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Errorf("тело для незнакомого статуса: %s", rec.Body)
	}
	// Доступ — как у ленты без параметра: без токена отказ.
	if rec := f.do("GET", "/v1/tasks/SDLC-1/timeline?status=analysis", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("без токена: %d", rec.Code)
	}
	if rec := f.do("GET", "/v1/tasks/SDLC-1/timeline?status=analysis", f.bot, nil); rec.Code != http.StatusOK {
		t.Errorf("участник проекта (агент): %d", rec.Code)
	}
}
