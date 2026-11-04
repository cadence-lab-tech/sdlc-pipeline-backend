package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Строка списка задач для «Обзора» (CLAB-363, решение 0070): текущий
// прогон, блокирующие, последнее движение, стоимость, причины внимания,
// фильтры по причинам и порядок бэклога. Номера — критерии AC-A анализа
// CLAB-363. Факты создаёт движок (переходы, захват, сдача, решения, проход
// воркера); в базу напрямую пишется только то, что движок делает по часам
// (срок аренды и вход в этап), и статус «готово» блокирующей задачи.

type ovFixture struct {
	*auditFixture
	ctx context.Context
}

func newOvFixture(t *testing.T) *ovFixture {
	return &ovFixture{auditFixture: newAuditFixture(t), ctx: context.Background()}
}

func (f *ovFixture) ok(method, path, token string, body any) map[string]any {
	f.t.Helper()
	code, out := f.json(method, path, token, body)
	if code != http.StatusOK && code != http.StatusCreated {
		f.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out
}

func (f *ovFixture) create(title string, extra map[string]any) string {
	f.t.Helper()
	body := map[string]any{"title": title}
	for k, v := range extra {
		body[k] = v
	}
	return f.ok("POST", "/v1/projects/SDLC/tasks", f.root, body)["key"].(string)
}

func (f *ovFixture) move(key, transition, token string) {
	f.t.Helper()
	f.ok("POST", "/v1/tasks/"+key+"/transitions/"+transition, token, map[string]any{})
}

func (f *ovFixture) submit(key, token, kind, content string) {
	f.t.Helper()
	body := map[string]any{"kind": kind, "title": kind, "content": content}
	if strings.HasPrefix(content, "{") {
		body["format"] = "json"
	}
	f.ok("POST", "/v1/tasks/"+key+"/artifacts", token, body)
}

func (f *ovFixture) decide(key, transition, gate string, body map[string]any) {
	f.t.Helper()
	f.ok("POST", "/v1/tasks/"+key+"/transitions/"+transition+"/gates/"+gate+"/decision", f.root, body)
}

// list — строки выборки и total под токеном token.
func (f *ovFixture) list(token, query string) ([]map[string]any, int) {
	f.t.Helper()
	rec := f.do("GET", "/v1/projects/SDLC/tasks?"+query, token, nil)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("список %q: %d %s", query, rec.Code, rec.Body)
	}
	var out struct {
		Tasks []map[string]any `json:"tasks"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Tasks, out.Total
}

func (f *ovFixture) item(token, key string) map[string]any {
	f.t.Helper()
	items, _ := f.list(token, "limit=500&q="+url.QueryEscape(key))
	for _, it := range items {
		if it["key"] == key {
			return it
		}
	}
	f.t.Fatalf("строки %s нет в списке", key)
	return nil
}

func taskKeysOf(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it["key"].(string))
	}
	return out
}

func attentionCodes(item map[string]any) []string {
	out := []string{}
	for _, raw := range item["attention"].([]any) {
		out = append(out, raw.(map[string]any)["code"].(string))
	}
	return out
}

func attentionOf(item map[string]any, code string) map[string]any {
	for _, raw := range item["attention"].([]any) {
		if a := raw.(map[string]any); a["code"] == code {
			return a
		}
	}
	return nil
}

func (f *ovFixture) setStatus(key, status string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE tasks t SET status_id = ws.id FROM workflow_statuses ws
		WHERE t.key = $1 AND ws.workflow_id = t.workflow_id AND ws.key = $2`, key, status); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ovFixture) expireWorker() {
	f.t.Helper()
	store := postgres.New(f.pool)
	system, err := store.ResolveActor(f.ctx, store.DB(), "test-worker", domain.PrincipalSystem)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.srv.engine.ExpireLeases(f.ctx, system.ID, 100); err != nil {
		f.t.Fatal(err)
	}
}

// AC-A1, AC-A2, AC-A21: форма строки и совместимость.
func TestOverviewShapeAndTerminal(t *testing.T) {
	f := newOvFixture(t)
	live := f.create("Живая", nil)
	gone := f.create("Снятая", nil)
	f.ok("POST", "/v1/tasks/"+live+"/links", f.root, map[string]any{"task": gone, "rel": "blocks"})
	f.move(gone, "cancel", f.root)

	rec := f.do("GET", "/v1/projects/SDLC/tasks", f.root, nil)
	var raw struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	order := []string{`"awaiting_approval"`, `"blocked_by"`, `"last_moved_at"`, `"current_run"`, `"attention"`, `"cost"`}
	for _, item := range raw.Tasks {
		// Прежняя структура читается как раньше: поля задачи первыми.
		var old domain.TaskListItem
		if err := json.Unmarshal(item, &old); err != nil || old.Key == "" {
			t.Fatalf("прежняя структура: %v %s", err, item)
		}
		prev := strings.Index(string(item), `"status_category"`)
		for _, k := range order {
			at := strings.Index(string(item), k)
			if at < 0 || at < prev {
				t.Fatalf("поле %s не на месте: %s", k, item)
			}
			prev = at
		}
		var m map[string]any
		_ = json.Unmarshal(item, &m)
		if m["last_moved_at"] == nil {
			t.Fatalf("last_moved_at null: %s", item)
		}
		if _, ok := m["blocked_by"].([]any); !ok {
			t.Fatalf("blocked_by не массив: %s", item)
		}
	}

	// Терминальная: ни причин, ни прогона, ни блокирующих, движение есть.
	g := f.item(f.root, gone)
	if len(g["attention"].([]any)) != 0 || g["current_run"] != nil || len(g["blocked_by"].([]any)) != 0 ||
		g["last_moved_at"] == nil || g["cost"] != nil {
		t.Fatalf("терминальная задача: %v", g)
	}
	// Без новых параметров — прежние значения прежних полей.
	if g["status_key"] != "cancelled" || g["awaiting_approval"] != false {
		t.Fatalf("прежние поля: %v", g)
	}
}

// AC-A3, AC-A5, AC-A14, AC-A28: текущий прогон сверяется с /queue и
// карточкой; статус без роли; назначенная другому; viewer и токен задачи.
func TestOverviewCurrentRun(t *testing.T) {
	f := newOvFixture(t)
	held := f.create("Забранная", nil)
	free := f.create("Свободная", nil)
	assigned := f.create("Назначенная", nil)
	idle := f.create("Не начата", nil)
	for _, k := range []string{held, free, assigned} {
		f.move(k, "start", f.root)
	}
	f.ok("POST", "/v1/tasks/"+held+"/claim", f.bot, map[string]any{})
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE tasks SET assignee_id = (SELECT id FROM principals WHERE kind = 'agent' ORDER BY created_at LIMIT 1)
		WHERE key = $1`, assigned); err != nil {
		t.Fatal(err)
	}

	cr := f.item(f.root, held)["current_run"].(map[string]any)
	if cr["kind"] != "stage" || cr["state"] != "claimed" || cr["status_key"] != "analysis" || cr["role_key"] != "analyst" ||
		cr["role_name"] == nil || cr["lease_alive"] != true || cr["claimable"] != false || cr["attempt"] != float64(1) {
		t.Fatalf("забранный прогон: %v", cr)
	}
	if _, ok := cr["review_attempt"]; ok {
		t.Fatalf("review_attempt у этапа: %v", cr)
	}
	// Сверка с очередью и карточкой на том же снимке.
	queue := f.ok("GET", "/v1/projects/SDLC/queue", f.root, nil)["queue"].([]any)
	var qrow map[string]any
	for _, raw := range queue {
		if r := raw.(map[string]any); r["task"].(map[string]any)["key"] == held {
			qrow = r
		}
	}
	run := qrow["stage_run"].(map[string]any)
	for _, k := range []string{"id", "state", "attempt", "claimed_by_handle", "claimed_at", "heartbeat_at", "lease_expires_at", "entered_at"} {
		if !reflect.DeepEqual(run[k], cr[k]) {
			t.Errorf("%s: очередь %v, список %v", k, run[k], cr[k])
		}
	}
	if qrow["role_key"] != cr["role_key"] || qrow["role_name"] != cr["role_name"] || qrow["claimable"] != cr["claimable"] {
		t.Errorf("роль и claimable: очередь %v, список %v", qrow, cr)
	}
	card := f.ok("GET", "/v1/tasks/"+held, f.root, nil)
	for _, raw := range card["stage_runs"].([]any) {
		if r := raw.(map[string]any); r["id"] == cr["id"] && (r["lease_expires_at"] != cr["lease_expires_at"] || r["state"] != cr["state"]) {
			t.Errorf("карточка %v, список %v", r, cr)
		}
	}
	if codes := attentionCodes(f.item(f.root, held)); len(codes) != 0 {
		t.Fatalf("живая аренда без причин, а тут %v", codes)
	}

	// Свободная: забрать можно, причина — не забран.
	fr := f.item(f.root, free)
	if fr["current_run"].(map[string]any)["claimable"] != true || !slices.Equal(attentionCodes(fr), []string{workflow.AttentionUnclaimed}) {
		t.Fatalf("свободная: %v", fr)
	}
	// Назначенная другому: не claimable для смотрящего, причина та же.
	as := f.item(f.root, assigned)
	if as["current_run"].(map[string]any)["claimable"] != false || !slices.Equal(attentionCodes(as), []string{workflow.AttentionUnclaimed}) {
		t.Fatalf("назначенная другому: %v", as)
	}
	// Статус без роли: прогон есть, роли и причин нет.
	id := f.item(f.root, idle)
	icr := id["current_run"].(map[string]any)
	if icr["role_key"] != nil || icr["role_name"] != nil || icr["claimable"] != false || icr["entered_at"] == nil ||
		len(attentionCodes(id)) != 0 {
		t.Fatalf("статус без роли: %v", id)
	}

	// AC-A28: viewer и токен задачи видят те же поля; уровень влияет только
	// на claimable.
	viewer := f.tokenFor("user:vera", domain.MemberViewer)
	task := f.ok("POST", "/v1/tasks/"+held+"/task-token", f.bot, nil)["token"].(string)
	for name, tok := range map[string]string{"viewer": viewer, "токен задачи": task} {
		for _, k := range []string{held, free, assigned, idle} {
			got, want := f.item(tok, k), f.item(f.root, k)
			for _, field := range []string{"attention", "blocked_by", "last_moved_at", "cost", "awaiting_approval"} {
				if !reflect.DeepEqual(got[field], want[field]) {
					t.Errorf("%s, %s, %s: %v против %v", name, k, field, got[field], want[field])
				}
			}
			g, w := got["current_run"].(map[string]any), want["current_run"].(map[string]any)
			delete(g, "claimable")
			delete(w, "claimable")
			delete(g, "stage_limit")
			delete(w, "stage_limit")
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s, %s: current_run %v против %v", name, k, g, w)
			}
		}
	}
}

// AC-A11: протухшая аренда до и после прохода воркера, новый захват и
// явный отпуск.
func TestOverviewLeaseExpired(t *testing.T) {
	f := newOvFixture(t)
	key := f.create("Аренда", nil)
	f.move(key, "start", f.root)
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.bot, map[string]any{})
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE stage_runs sr SET lease_expires_at = now() - interval '1 minute'
		FROM tasks t WHERE t.id = sr.task_id AND t.key = $1 AND sr.finished_at IS NULL`, key); err != nil {
		t.Fatal(err)
	}
	it := f.item(f.root, key)
	a := attentionOf(it, workflow.AttentionLeaseExpired)
	if a == nil || a["holder"] == nil || a["run_kind"] != "stage" || a["since"] == nil ||
		!slices.Equal(attentionCodes(it), []string{workflow.AttentionLeaseExpired, workflow.AttentionUnclaimed}) {
		t.Fatalf("до прохода воркера: %v", it["attention"])
	}
	holder := a["holder"]

	f.expireWorker()
	it = f.item(f.root, key)
	if a := attentionOf(it, workflow.AttentionLeaseExpired); a == nil || a["holder"] != holder ||
		it["current_run"].(map[string]any)["state"] != "pending" {
		t.Fatalf("после прохода воркера: %v", it)
	}

	f.ok("POST", "/v1/tasks/"+key+"/claim", f.bot, map[string]any{})
	if codes := attentionCodes(f.item(f.root, key)); len(codes) != 0 {
		t.Fatalf("после нового захвата: %v", codes)
	}
	f.ok("POST", "/v1/tasks/"+key+"/release", f.bot, map[string]any{})
	if codes := attentionCodes(f.item(f.root, key)); !slices.Equal(codes, []string{workflow.AttentionUnclaimed}) {
		t.Fatalf("после явного отпуска: %v", codes)
	}
}

// AC-A4, AC-A9, AC-A10, AC-A13: перепроверка, отклонение и пересдача,
// ожидание человека со сверкой с /approvals, возврат.
func TestOverviewReviewApprovalAndReturn(t *testing.T) {
	f := newOvFixture(t)
	key := f.create("Цикл", nil)
	f.move(key, "start", f.root)
	viewer := f.tokenFor("user:vera", domain.MemberViewer)

	// approvalsSet — гейты /approvals этой задачи; attentionSet — gates
	// причины awaits_human под токеном token.
	approvalsSet := func(token string) []string {
		t.Helper()
		var out []string
		for _, raw := range f.ok("GET", "/v1/projects/SDLC/approvals", token, nil)["approvals"].([]any) {
			a := raw.(map[string]any)
			if a["task"].(map[string]any)["key"] == key {
				out = append(out, a["transition"].(map[string]any)["key"].(string)+"/"+a["gate"].(map[string]any)["gate_key"].(string))
			}
		}
		sort.Strings(out)
		return out
	}
	attentionSet := func(token string) []string {
		t.Helper()
		var out []string
		if a := attentionOf(f.item(token, key), workflow.AttentionAwaitsHuman); a != nil {
			for _, raw := range a["gates"].([]any) {
				g := raw.(map[string]any)
				out = append(out, g["transition"].(string)+"/"+g["gate"].(string))
			}
		}
		sort.Strings(out)
		return out
	}
	sameAsApprovals := func(when string) {
		t.Helper()
		if a, b := approvalsSet(f.root), attentionSet(f.root); !slices.Equal(a, b) {
			t.Fatalf("%s: /approvals %v, awaits_human %v", when, a, b)
		}
		// Наблюдателю /approvals пуст, причина та же.
		if b, v := attentionSet(f.root), attentionSet(viewer); !slices.Equal(b, v) || len(approvalsSet(viewer)) != 0 {
			t.Fatalf("%s: причина под viewer %v, у maintainer %v", when, v, b)
		}
	}

	// Область задачи не объявлена — гейт области снят, чтобы ждал только
	// человек.
	f.decide(key, "analysis_done", "repo_scope", map[string]any{"status": "waived"})
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.bot, map[string]any{})
	f.submit(key, f.bot, "analysis.brief", "# бриф")
	f.submit(key, f.bot, "analysis.acceptance_criteria", "# критерии")
	sameAsApprovals("апрув при непрошедшей перепроверке")
	if code, _ := f.json("POST", "/v1/tasks/"+key+"/transitions/analysis_done", f.bot, map[string]any{}); code == http.StatusOK {
		t.Fatal("переход прошёл без перепроверки")
	}
	// Этап в gating: текущий прогон — перепроверка.
	it := f.item(f.root, key)
	cr := it["current_run"].(map[string]any)
	if cr["kind"] != "review" || cr["role_key"] != "reviewer" || cr["attempt"] != float64(1) || cr["review_attempt"] != float64(1) ||
		!slices.Equal(attentionCodes(it), []string{workflow.AttentionUnclaimed}) {
		t.Fatalf("перепроверка: %v %v", cr, it["attention"])
	}

	// Перепроверка вынесла fail — отклонение до новой сдачи.
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.root, map[string]any{})
	f.submit(key, f.root, "review.verdict", `{"verdict":"fail","findings":[]}`)
	it = f.item(f.root, key)
	rej := attentionOf(it, workflow.AttentionGateRejected)
	if rej == nil || it["current_run"].(map[string]any)["kind"] != "stage" {
		t.Fatalf("после fail: %v", it)
	}
	g := rej["gates"].([]any)[0].(map[string]any)
	if g["transition"] != "analysis_done" || g["gate"] != "peer_review" || g["reason"] != nil {
		t.Fatalf("отклонённый гейт: %v", g)
	}
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.bot, map[string]any{})
	f.submit(key, f.bot, "analysis.acceptance_criteria", "# критерии v2")
	if a := attentionOf(f.item(f.root, key), workflow.AttentionGateRejected); a != nil {
		t.Fatalf("новая сдача не сняла отклонение: %v", a)
	}

	// Вердикт вычислителя без решающего (неудачная попытка перехода) —
	// не отклонение.
	if code, _ := f.json("POST", "/v1/tasks/"+key+"/transitions/analysis_done", f.bot, map[string]any{}); code == http.StatusOK {
		t.Fatal("переход прошёл без второй перепроверки")
	}
	if a := attentionOf(f.item(f.root, key), workflow.AttentionGateRejected); a != nil {
		t.Fatalf("вердикт вычислителя засчитан отклонением: %v", a)
	}
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.root, map[string]any{})
	f.submit(key, f.root, "review.verdict", `{"verdict":"pass","findings":[]}`)
	sameAsApprovals("апрув ждёт")
	it = f.item(f.root, key)
	if attentionOf(it, workflow.AttentionAwaitsHuman) == nil || it["awaiting_approval"] != true {
		t.Fatalf("ждёт апрува: %v", it)
	}

	// Человек отклонил апрув с причиной.
	f.decide(key, "analysis_done", "human_approval", map[string]any{"status": "failed", "details": map[string]any{"reason": "нет AC-3"}})
	it = f.item(f.root, key)
	rej = attentionOf(it, workflow.AttentionGateRejected)
	if rej == nil || rej["gates"].([]any)[0].(map[string]any)["reason"] != "нет AC-3" || attentionOf(it, workflow.AttentionAwaitsHuman) != nil {
		t.Fatalf("отклонённый апрув: %v", it["attention"])
	}
	sameAsApprovals("апрув отклонён")

	// Апрув, архитектура: чек-лист при живой аренде не ждёт человека.
	f.decide(key, "analysis_done", "human_approval", map[string]any{"status": "passed"})
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.bot, map[string]any{})
	f.move(key, "analysis_done", f.bot)
	f.decide(key, "architecture_done", "repo_scope", map[string]any{"status": "waived"})
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.root, map[string]any{})
	f.submit(key, f.root, "arch.adr", "# adr")
	f.submit(key, f.root, "arch.impact_map", "# impact")
	sameAsApprovals("чек-лист при живой аренде")
	if got := attentionSet(f.root); !slices.Equal(got, []string{"architecture_done/human_approval"}) {
		t.Fatalf("при живой аренде ждёт только апрув: %v", got)
	}
	f.ok("POST", "/v1/tasks/"+key+"/release", f.root, map[string]any{})
	sameAsApprovals("чек-лист без аренды")
	if got := attentionSet(f.root); !slices.Equal(got, []string{"architecture_done/human_approval", "architecture_done/traceability"}) {
		t.Fatalf("без аренды ждут апрув и чек-лист: %v", got)
	}

	// Возврат аналитику: попытка 2.
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.root, map[string]any{})
	f.submit(key, f.root, "problem.report", "# проблема")
	f.move(key, "reject_to_analysis", f.root)
	it = f.item(f.root, key)
	ret := attentionOf(it, workflow.AttentionReturned)
	if ret == nil || ret["attempt"] != float64(2) || !slices.Equal(attentionCodes(it), []string{workflow.AttentionReturned, workflow.AttentionUnclaimed}) {
		t.Fatalf("возврат: %v", it["attention"])
	}
	// Возврат с живой арендой — в «В работе».
	f.ok("POST", "/v1/tasks/"+key+"/claim", f.bot, map[string]any{})
	if items, _ := f.list(f.root, "in_work=true&attention_within=returned"); !slices.Contains(taskKeysOf(items), key) {
		t.Fatalf("возврат с арендой не в «В работе»: %v", taskKeysOf(items))
	}
}

// AC-A6, AC-A12: предел этапа по часам, снятие человеком.
func TestOverviewStageLimit(t *testing.T) {
	f := newOvFixture(t)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO gates (transition_id, key, name, type, config)
		SELECT tr.id, 'time', 'Предел этапа', 'stage_time_limit', '{"max_duration": "1h"}'
		FROM workflow_transitions tr JOIN tasks t ON t.workflow_id = tr.workflow_id
		WHERE t.key = 'SDLC-1' AND tr.key = 'start'`); err != nil {
		t.Fatal(err)
	}
	fresh, late := f.create("В пределе", nil), f.create("Просрочена", nil)
	f.move(fresh, "start", f.root)
	f.move(late, "start", f.root)
	f.ok("POST", "/v1/tasks/"+late+"/claim", f.bot, map[string]any{})
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE stage_runs sr SET entered_at = entered_at - interval '2 hours'
		FROM tasks t WHERE t.id = sr.task_id AND t.key = $1 AND sr.finished_at IS NULL`, late); err != nil {
		t.Fatal(err)
	}
	fr := f.item(f.root, fresh)
	if l := fr["current_run"].(map[string]any)["stage_limit"].(map[string]any); l["overdue"] != false || l["max_duration"] != "1h" ||
		attentionOf(fr, workflow.AttentionOverStageLimit) != nil {
		t.Fatalf("в пределе: %v", fr)
	}
	it := f.item(f.root, late)
	a := attentionOf(it, workflow.AttentionOverStageLimit)
	if a == nil || a["max_duration"] != "1h" || a["gate"] != "time" || a["since"] == nil {
		t.Fatalf("просрочка без прохода воркера: %v", it["attention"])
	}
	// Блок предела — как в карточке.
	card := f.ok("GET", "/v1/tasks/"+late, f.root, nil)["stage_limit"].(map[string]any)
	got := it["current_run"].(map[string]any)["stage_limit"].(map[string]any)
	for _, k := range []string{"stage_run_id", "gate", "transition", "status", "max_duration", "entered_at", "deadline", "overdue", "waived"} {
		if !reflect.DeepEqual(card[k], got[k]) {
			t.Errorf("stage_limit.%s: карточка %v, список %v", k, card[k], got[k])
		}
	}
	f.decide(late, "start", "time", map[string]any{"status": "waived"})
	it = f.item(f.root, late)
	if attentionOf(it, workflow.AttentionOverStageLimit) != nil || it["current_run"].(map[string]any)["stage_limit"].(map[string]any)["waived"] != true {
		t.Fatalf("после снятия: %v", it)
	}
}

// AC-A7, AC-A15: незакрытые блокирующие; закрытая уходит, отменённая
// остаётся; карточка родителя не меняется.
func TestOverviewBlockedBy(t *testing.T) {
	f := newOvFixture(t)
	target := f.create("Ждёт", nil)
	open, closed, gone := f.create("Идёт", nil), f.create("Готова", nil), f.create("Отменена", nil)
	for _, k := range []string{gone, closed, open} {
		f.ok("POST", "/v1/tasks/"+k+"/links", f.root, map[string]any{"task": target, "rel": "blocks"})
	}
	f.setStatus(closed, "done")
	f.move(gone, "cancel", f.root)
	it := f.item(f.root, target)
	want := []any{open, gone} // по номеру: open заведена раньше gone
	if !reflect.DeepEqual(it["blocked_by"], want) || !slices.Contains(attentionCodes(it), workflow.AttentionBlocked) {
		t.Fatalf("blocked_by: %v, причины %v", it["blocked_by"], it["attention"])
	}
	if a := attentionOf(it, workflow.AttentionBlocked); a["since"] != nil {
		t.Fatalf("blocked.since: %v", a)
	}
	f.setStatus(open, "done")
	f.ok("POST", "/v1/tasks/"+gone+"/links", f.root, map[string]any{"task": open, "rel": "relates_to"})
	if it := f.item(f.root, target); !reflect.DeepEqual(it["blocked_by"], []any{gone}) {
		t.Fatalf("после закрытия: %v", it["blocked_by"])
	}
}

// AC-A8: last_moved_at по каждому типу события; поддерево.
func TestOverviewLastMoved(t *testing.T) {
	f := newOvFixture(t)
	parent := f.create("Родитель", nil)
	child := f.create("Ребёнок", map[string]any{"parent_key": parent})
	moved := func(key string) (time.Time, *time.Time) {
		t.Helper()
		it := f.item(f.root, key)
		at, err := time.Parse(time.RFC3339Nano, it["last_moved_at"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var sub *time.Time
		if raw, ok := it["subtree_last_moved_at"].(string); ok {
			s, _ := time.Parse(time.RFC3339Nano, raw)
			sub = &s
		}
		return at, sub
	}
	base, sub := moved(parent)
	if sub == nil {
		t.Fatal("у задачи с детьми нет subtree_last_moved_at")
	}
	if _, s := moved(child); s != nil {
		t.Fatal("у задачи без детей есть subtree_last_moved_at")
	}
	// Правка заголовка — не движение.
	f.ok("PATCH", "/v1/tasks/"+parent, f.root, map[string]any{"title": "Родитель, новое имя"})
	if at, _ := moved(parent); !at.Equal(base) {
		t.Fatalf("правка заголовка сдвинула движение: %v → %v", base, at)
	}

	var parentID string
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM tasks WHERE key = $1`, parent).Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	step := 0
	event := func(typ string) time.Time {
		t.Helper()
		step++
		at := time.Now().Add(time.Duration(step) * time.Hour).UTC().Truncate(time.Microsecond)
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO task_events (task_id, type, payload, created_at) VALUES ($1, $2, '{}', $3)`,
			parentID, typ, at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	last := base
	for _, typ := range domain.MovementEvents {
		at := event(typ)
		if got, _ := moved(parent); !got.Equal(at) {
			t.Errorf("%s: last_moved_at %v, ждали %v", typ, got, at)
		}
		last = at
	}
	for _, typ := range []string{domain.EventLeaseExtended, domain.EventUsageReported, domain.EventTaskUpdated,
		domain.EventAttachmentAdded, domain.EventRepoRefLinked, domain.EventDriftDetected, domain.EventDriftResolved,
		domain.EventGateFailed} {
		event(typ)
		if got, _ := moved(parent); !got.Equal(last) {
			t.Errorf("%s сдвинул движение: %v, ждали %v", typ, got, last)
		}
	}
	// Ребёнок двинулся позже — поддерево родителя показывает его время.
	f.move(child, "start", f.root)
	own, sub := moved(parent)
	childAt, _ := moved(child)
	wantSub := last
	if childAt.After(last) {
		wantSub = childAt
	}
	if !own.Equal(last) || sub == nil || !sub.Equal(wantSub) {
		t.Fatalf("поддерево: своё %v, поддерево %v, ребёнок %v", own, sub, childAt)
	}
	// Событие ребёнка позже всех событий родителя — поддерево берёт его.
	var childID string
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM tasks WHERE key = $1`, child).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	later := last.Add(time.Hour)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO task_events (task_id, type, payload, created_at) VALUES ($1, $2, '{}', $3)`,
		childID, domain.EventStageClaimed, later); err != nil {
		t.Fatal(err)
	}
	if own, sub := moved(parent); !own.Equal(last) || sub == nil || !sub.Equal(later) {
		t.Fatalf("поддерево после события ребёнка: своё %v, поддерево %v, ждали %v", own, sub, later)
	}
}

// AC-A23: стоимость совпадает с /usage; нет учёта — null.
func TestOverviewCost(t *testing.T) {
	f := newOvFixture(t)
	parent := f.create("Эпик со счётом", nil)
	kid := f.create("Без учёта", map[string]any{"parent_key": parent})
	paid := f.create("С учётом", map[string]any{"parent_key": parent})
	f.move(parent, "start", f.root)
	f.ok("POST", "/v1/tasks/"+parent+"/claim", f.root, map[string]any{})
	f.move(paid, "start", f.root)
	f.ok("POST", "/v1/tasks/"+paid+"/claim", f.root, map[string]any{})
	for _, body := range []map[string]any{
		{"model": "claude-opus-5-5", "input_tokens": 10, "output_tokens": 5, "cost_usd": 1.25},
		{"model": "claude-sonnet-5", "input_tokens": 3, "cost_usd": 0.5, "billing": "subscription"},
		{"model": "claude-sonnet-5", "input_tokens": 3},
	} {
		f.ok("POST", "/v1/tasks/"+paid+"/usage", f.root, body)
	}
	f.ok("POST", "/v1/tasks/"+parent+"/usage", f.root, map[string]any{"model": "claude-opus-5-5", "cost_usd": 2, "billing": "api"})

	for _, k := range []string{parent, paid} {
		it := f.item(f.root, k)
		usage := f.ok("GET", "/v1/tasks/"+k+"/usage", f.root, nil)
		if u, ok := usage["usage"].(map[string]any); ok {
			usage = u
		}
		cost := it["cost"].(map[string]any)
		for _, field := range []string{"cost_usd", "api_cost_usd", "subscription_cost_usd"} {
			if cost[field] != usage[field] {
				t.Errorf("%s.%s: список %v, /usage %v", k, field, cost[field], usage[field])
			}
		}
		if k == parent && it["subtree_cost_usd"] != usage["subtree_cost_usd"] {
			t.Errorf("subtree_cost_usd: список %v, /usage %v", it["subtree_cost_usd"], usage["subtree_cost_usd"])
		}
	}
	if c := f.item(f.root, paid)["cost"].(map[string]any); c["reports"] != float64(3) {
		t.Errorf("reports: %v", c)
	}
	if it := f.item(f.root, kid); it["cost"] != nil {
		t.Errorf("без учёта — null, а не %v", it["cost"])
	}
	if _, ok := f.item(f.root, paid)["subtree_cost_usd"]; ok {
		t.Error("у задачи без детей есть subtree_cost_usd")
	}
}

// AC-A17…AC-A19, AC-A18: фильтры по причинам, зоны B, листание, потолок.
func TestOverviewZonesAndFilters(t *testing.T) {
	f := newOvFixture(t)
	mk := func(title string) string {
		k := f.create(title, nil)
		f.move(k, "start", f.root)
		return k
	}
	working := mk("Работает")
	f.ok("POST", "/v1/tasks/"+working+"/claim", f.bot, map[string]any{})
	queued := mk("В очереди")
	assigned := mk("Назначена другому")
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE tasks SET assignee_id = (SELECT id FROM principals WHERE kind = 'agent' ORDER BY created_at LIMIT 1)
		WHERE key = $1`, assigned); err != nil {
		t.Fatal(err)
	}
	stale := mk("Протухла")
	f.ok("POST", "/v1/tasks/"+stale+"/claim", f.bot, map[string]any{})
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE stage_runs sr SET lease_expires_at = now() - interval '1 minute'
		FROM tasks t WHERE t.id = sr.task_id AND t.key = $1 AND sr.finished_at IS NULL`, stale); err != nil {
		t.Fatal(err)
	}
	blocker := f.create("Блокирует", nil)
	blocked := mk("Заблокирована")
	f.ok("POST", "/v1/tasks/"+blocker+"/links", f.root, map[string]any{"task": blocked, "rel": "blocks"})
	f.ok("POST", "/v1/tasks/"+blocked+"/claim", f.bot, map[string]any{})
	waiting := mk("Ждёт апрува")
	for _, gate := range []string{"artifacts", "peer_review", "drift", "repo_scope"} {
		f.decide(waiting, "analysis_done", gate, map[string]any{"status": "waived"})
	}

	keysFor := func(q string) []string {
		items, total := f.list(f.root, q+"&limit=500")
		if total != len(items) {
			t.Fatalf("%s: total %d при %d строках", q, total, len(items))
		}
		return taskKeysOf(items)
	}
	inWork := keysFor("in_work=true")
	zones := map[string][]string{
		"ждут решения": keysFor("in_work=true&attention=awaits_human"),
		"застряло":     keysFor("in_work=true&attention_not=awaits_human&attention_except=awaits_human,returned,unclaimed"),
		"очередь":      keysFor("in_work=true&attention_within=returned,unclaimed&attention=unclaimed"),
		"в работе":     keysFor("in_work=true&attention_within=returned"),
	}
	seen := map[string]string{}
	for zone, keys := range zones {
		for _, k := range keys {
			if prev, dup := seen[k]; dup {
				t.Errorf("%s и в «%s», и в «%s»", k, prev, zone)
			}
			seen[k] = zone
		}
	}
	for _, k := range inWork {
		if _, ok := seen[k]; !ok {
			t.Errorf("%s в работе, но ни в одной зоне", k)
		}
	}
	if len(seen) != len(inWork) {
		t.Errorf("зоны покрывают %d задач, в работе %d", len(seen), len(inWork))
	}
	for k, zone := range map[string]string{working: "в работе", queued: "очередь", assigned: "очередь",
		stale: "застряло", blocked: "застряло", waiting: "ждут решения"} {
		if seen[k] != zone {
			t.Errorf("%s: зона «%s», ждали «%s»", k, seen[k], zone)
		}
	}

	// Листание по отфильтрованной выборке.
	all := keysFor("attention=true")
	var paged []string
	for off := 0; off < len(all)+2; off += 2 {
		items, total := f.list(f.root, fmt.Sprintf("attention=true&limit=2&offset=%d", off))
		if total != len(all) {
			t.Fatalf("total %d, выборка %d", total, len(all))
		}
		paged = append(paged, taskKeysOf(items)...)
	}
	if !slices.Equal(paged, all) {
		t.Fatalf("страницы %v, выборка %v", paged, all)
	}
	// Без причин подходят и терминальные: они причин не имеют.
	f.move(f.create("Снята", nil), "cancel", f.root)
	if none := keysFor("attention=false"); !slices.ContainsFunc(none, func(k string) bool { return f.item(f.root, k)["status_key"] == "cancelled" }) {
		t.Fatalf("attention=false без терминальных: %v", none)
	}

	// Незнакомый код — 400 с перечнем.
	for _, bad := range []string{"attention=nope", "attention_not=x", "attention_except=", "attention_within=awaits_human,zzz"} {
		rec := f.do("GET", "/v1/projects/SDLC/tasks?"+bad, f.root, nil)
		if bad == "attention_except=" {
			if rec.Code != http.StatusOK {
				t.Errorf("%s: пустой параметр — как отсутствующий, а не %d", bad, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unclaimed") {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	// Потолок выборки режима фильтров.
	saved := workflow.AttentionScanLimit
	workflow.AttentionScanLimit = 2
	t.Cleanup(func() { workflow.AttentionScanLimit = saved })
	if rec := f.do("GET", "/v1/projects/SDLC/tasks?attention=true", f.root, nil); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "in_work") {
		t.Fatalf("потолок: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("GET", "/v1/projects/SDLC/tasks?limit=500", f.root, nil); rec.Code != http.StatusOK {
		t.Fatalf("без фильтров потолка нет: %d", rec.Code)
	}
}

// AC-A20 (CLAB-363) после ранга (CLAB-399, решение 0075, К-18, К-19):
// order=backlog — ручной порядок; приоритет и блокировка задачу сами не
// двигают, листание и фильтры бэклога — прежние.
func TestOverviewBacklogOrder(t *testing.T) {
	f := newOvFixture(t)
	mk := func(title, priority string) string {
		return f.create("Бэклог "+title, map[string]any{"priority": priority})
	}
	low, normal, high, urgent := mk("низкий", "low"), mk("обычный", "normal"), mk("высокий", "high"), mk("срочный", "urgent")
	blocked := mk("срочный заблокирован", "urgent")
	active := f.create("Активный блокирующий", nil)
	f.ok("POST", "/v1/tasks/"+active+"/links", f.root, map[string]any{"task": blocked, "rel": "blocks"})

	// Заведение ставит в конец: порядок — порядок заведения, а не приоритет.
	want := []string{low, normal, high, urgent, blocked}
	q := "q=" + url.QueryEscape("Бэклог") + "&order=backlog"
	items, total := f.list(f.root, q+"&limit=100")
	if got := taskKeysOf(items); !slices.Equal(got, want) || total != len(want) {
		t.Fatalf("порядок бэклога %v, ждали %v", got, want)
	}
	// Приоритет не двигает.
	f.ok("PATCH", "/v1/tasks/"+high, f.root, map[string]any{"priority": "low"})
	f.ok("PATCH", "/v1/tasks/"+low, f.root, map[string]any{"priority": "urgent"})
	if items, _ := f.list(f.root, q+"&limit=100"); !slices.Equal(taskKeysOf(items), want) {
		t.Fatalf("приоритет сдвинул задачу: %v", taskKeysOf(items))
	}
	// Заблокированную поставили наверх — она там и остаётся, blocked_by есть.
	if code, out := f.rank(blocked, f.root, map[string]any{"before": low}); code != http.StatusOK {
		t.Fatalf("перемещение: %d %v", code, out)
	}
	want = []string{blocked, low, normal, high, urgent}
	items, _ = f.list(f.root, q+"&limit=100")
	if got := taskKeysOf(items); !slices.Equal(got, want) {
		t.Fatalf("заблокированная наверху: %v", got)
	}
	if bb := items[0]["blocked_by"].([]any); len(bb) != 1 || bb[0] != active {
		t.Fatalf("blocked_by: %v", items[0])
	}
	var paged []string
	for off := 0; off < len(want); off += 2 {
		items, _ := f.list(f.root, fmt.Sprintf("%s&limit=2&offset=%d", q, off))
		paged = append(paged, taskKeysOf(items)...)
	}
	if !slices.Equal(paged, want) {
		t.Fatalf("листание %v, ждали %v", paged, want)
	}
	// К-19: запрос блока «Приоритетные из бэклога» отдаёт новый порядок.
	if items, _ := f.list(f.root, q+"&terminal=false&in_work=false&top_level=true&limit=3"); !slices.Equal(taskKeysOf(items), want[:3]) {
		t.Fatalf("бэклог с фильтрами: %v", taskKeysOf(items))
	}
	// order=updated и пустой порядок — прежние; неизвестный — 400 с обоими.
	if items, _ := f.list(f.root, "q="+url.QueryEscape("Бэклог")+"&limit=100"); slices.Equal(taskKeysOf(items), want) {
		t.Fatal("порядок по умолчанию стал порядком бэклога")
	}
	if items, _ := f.list(f.root, "q="+url.QueryEscape("Бэклог")+"&order=updated&limit=1"); taskKeysOf(items)[0] != low {
		t.Fatalf("order=updated: %v", taskKeysOf(items))
	}
	rec := f.do("GET", "/v1/projects/SDLC/tasks?order=random", f.root, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "updated") || !strings.Contains(rec.Body.String(), "backlog") {
		t.Fatalf("неизвестный порядок: %d %s", rec.Code, rec.Body)
	}
}

// AC-A22: проект со своим циклом (свои ключи статусов и роли) и задача на
// прежней версии цикла получают поля по тем же правилам.
func TestOverviewOtherCycleAndOldVersion(t *testing.T) {
	f := newOvFixture(t)
	old := f.create("На прежней версии", nil)
	f.move(old, "start", f.root)
	f.activateSkipCycle()
	fresh := f.create("На новой версии", nil)
	f.move(fresh, "start", f.root)
	for _, k := range []string{old, fresh} {
		it := f.item(f.root, k)
		if it["current_run"] == nil || !slices.Equal(attentionCodes(it), []string{workflow.AttentionUnclaimed}) {
			t.Fatalf("%s: %v", k, it)
		}
	}

	f.ok("POST", "/v1/projects", f.root, map[string]any{"key": "ACME", "name": "Acme"})
	bundle := map[string]any{
		"version": 1,
		"roles":   []any{map[string]any{"key": "builder", "name": "Сборщик", "instructions": "собирать"}},
		"workflows": []any{map[string]any{
			"name": "Свой", "task_types": []string{"task"},
			"statuses": []any{
				map[string]any{"key": "inbox", "name": "Входящие", "category": "backlog", "is_initial": true},
				map[string]any{"key": "build", "name": "Сборка", "category": "active", "role": "builder"},
				map[string]any{"key": "shipped", "name": "Отгружено", "category": "done", "is_terminal": true},
			},
			"transitions": []any{
				map[string]any{"key": "go", "name": "go", "from": "inbox", "to": "build", "kind": "advance"},
				map[string]any{"key": "ship", "name": "ship", "from": "build", "to": "shipped", "kind": "advance"},
			},
		}},
	}
	report := f.ok("POST", "/v1/projects/ACME/import", f.root, bundle)
	entry := report["workflows"].([]any)[0].(map[string]any)
	f.ok("POST", "/v1/workflows/"+entry["id"].(string)+"/activate", f.root, nil)
	a := f.ok("POST", "/v1/projects/ACME/tasks", f.root, map[string]any{"title": "Чужой цикл"})["key"].(string)
	b := f.ok("POST", "/v1/projects/ACME/tasks", f.root, map[string]any{"title": "Ждёт чужую"})["key"].(string)
	f.ok("POST", "/v1/tasks/"+a+"/links", f.root, map[string]any{"task": b, "rel": "blocks"})
	f.move(a, "go", f.root)
	f.move(b, "go", f.root)
	rec := f.do("GET", "/v1/projects/ACME/tasks?in_work=true", f.root, nil)
	var out struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	got := map[string][]string{}
	for _, it := range out.Tasks {
		got[it["key"].(string)] = attentionCodes(it)
		if cr := it["current_run"].(map[string]any); cr["role_key"] != "builder" || cr["status_key"] != "build" {
			t.Fatalf("чужой цикл: %v", cr)
		}
	}
	if !slices.Equal(got[a], []string{workflow.AttentionUnclaimed}) ||
		!slices.Equal(got[b], []string{workflow.AttentionBlocked, workflow.AttentionUnclaimed}) {
		t.Fatalf("причины в чужом цикле: %v", got)
	}
}

// AC-A24: число запросов списка не растёт с размером страницы (задачи без
// вычисления гейтов).
func TestOverviewQueryCount(t *testing.T) {
	f := newOvFixture(t)
	var prev string
	for i := 0; i < 50; i++ {
		extra := map[string]any{}
		if i%7 == 3 && prev != "" {
			extra["parent_key"] = prev
		}
		k := f.create(fmt.Sprintf("Счёт %02d", i), extra)
		if i%5 == 0 && prev != "" {
			f.ok("POST", "/v1/tasks/"+prev+"/links", f.root, map[string]any{"task": k, "rel": "blocks"})
		}
		if i%9 == 0 {
			f.setStatus(k, "done")
		}
		prev = k
	}
	store := postgres.New(f.pool)
	project, err := store.GetProjectByKey(f.ctx, f.pool, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	count := func(limit int) int {
		t.Helper()
		q := &countingQuerier{Querier: f.pool}
		page, err := f.srv.engine.ListTaskOverviews(f.ctx, q, project.ID,
			postgres.ListTasksFilter{Query: "Счёт", Limit: limit}, workflow.AttentionFilter{}, f.admin.Principal.ID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != limit {
			t.Fatalf("страница %d строк, ждали %d", len(page.Items), limit)
		}
		return q.n
	}
	if small, big := count(5), count(50); small != big {
		t.Fatalf("запросов на 5 строк %d, на 50 — %d", small, big)
	}
}

// countingQuerier считает обращения к базе: Query, QueryRow и Exec.
type countingQuerier struct {
	postgres.Querier
	n int
}

func (c *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	return c.Querier.Query(ctx, sql, args...)
}

func (c *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	return c.Querier.QueryRow(ctx, sql, args...)
}

func (c *countingQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.n++
	return c.Querier.Exec(ctx, sql, args...)
}
