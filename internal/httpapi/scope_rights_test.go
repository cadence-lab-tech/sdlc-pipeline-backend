package httpapi

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Расширение области — решение человека (CLAB-391, решения 0015 и 0072
// п. 6). Заменяет характеризацию прежнего поведения (первый коммит ветки),
// где agent добавлял репозиторий и по REST, и тулом MCP. Номера — критерии
// приёмки анализа CLAB-391; токен задачи (AC-6) держат executor_test.go
// обоих транспортов. CLAB-411 (В-3 эпика CLAB-404, AC-18): человек уровня
// agent — сотрудник — область меняет; отказ держится для бота (agent:bot
// фикстуры с api-токеном).
func TestScopeExpansionRights(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	dev := f.bot
	employee := f.memberToken("user:dev", domain.MemberAgent, nil)
	lead := f.memberToken("user:lead", domain.MemberMaintainer, nil)
	boss := f.memberToken("user:boss", domain.MemberAdmin, nil)
	watcher := f.memberToken("user:watcher", domain.MemberViewer, nil)
	// AC-9: уровень — максимум по путям участия (решение 0027).
	mixed := f.memberToken("user:mixed", domain.MemberAgent, nil)
	twice := f.memberToken("user:twice", domain.MemberAgent, nil)
	for _, team := range []struct {
		key, member string
		level       domain.MemberRole
	}{{"platform", "user:mixed", domain.MemberMaintainer}, {"qa", "user:twice", domain.MemberAgent}} {
		if _, err := f.auth.CreateTeam(ctx, f.admin, workflow.CreateTeamInput{Key: team.key, Name: team.key}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.auth.SetTeamMember(ctx, f.admin, team.key, team.member, domain.TeamRoleMember); err != nil {
			t.Fatal(err)
		}
		if _, err := f.auth.SetProjectTeam(ctx, f.admin, "SDLC", team.key, team.level, nil); err != nil {
			t.Fatal(err)
		}
	}
	const docs = "cadence-lab-tech/sdlc-pipeline-docs"

	repos := func(key string) []string {
		t.Helper()
		_, body := f.json(http.MethodGet, "/v1/tasks/"+key, f.root, nil)
		task := body["task"].(map[string]any)
		var out []string
		for _, raw := range task["repositories"].([]any) {
			r := raw.(map[string]any)
			out = append(out, r["owner"].(string)+"/"+r["name"].(string))
		}
		return out
	}
	multiRepo := func(key string) any {
		t.Helper()
		_, body := f.json(http.MethodGet, "/v1/tasks/"+key, f.root, nil)
		return body["task"].(map[string]any)["properties"].(map[string]any)["multi_repo"]
	}
	// scopeEvents — события task.scope_updated из ленты задачи.
	scopeEvents := func(key string) []map[string]any {
		t.Helper()
		_, body := f.json(http.MethodGet, "/v1/tasks/"+key+"/timeline", f.root, nil)
		var out []map[string]any
		for _, raw := range body["events"].([]any) {
			if e := raw.(map[string]any); e["type"] == "task.scope_updated" {
				out = append(out, e)
			}
		}
		return out
	}
	patch := func(token, key string, body map[string]any) (int, map[string]any) {
		t.Helper()
		return f.json(http.MethodPatch, "/v1/tasks/"+key+"/scope", token, body)
	}
	addDocs := map[string]any{"add_repositories": []string{docs}, "note": "нужны решения"}

	// AC-1: бот — 403 с понятным текстом; область и лента прежние.
	code, body := patch(dev, "SDLC-3", addDocs)
	refusal, _ := errOf(body)
	if e, _ := body["error"].(map[string]any); code != http.StatusForbidden || e["code"] != "forbidden" ||
		!strings.Contains(refusal, "решение человека") || !strings.Contains(refusal, "сопровождающ") ||
		!strings.Contains(refusal, "сотрудник") || strings.HasPrefix(refusal, workflow.ExecutorRefusal) {
		t.Fatalf("REST бот: %d %v", code, body)
	}
	if got := repos("SDLC-3"); len(got) != 2 || slices.Contains(got, docs) {
		t.Fatalf("отказ изменил область: %v", got)
	}
	if n := len(scopeEvents("SDLC-3")); n != 0 {
		t.Fatalf("отказ оставил в ленте %d событий task.scope_updated", n)
	}

	// AC-10: отказ — в журнале действий.
	rows := f.rows("p.handle = 'bot' AND a.outcome = 'forbidden'")
	if len(rows) != 1 || rows[0].Channel != "rest" || rows[0].Action != "PATCH /v1/tasks/:taskKey/scope" ||
		s(rows[0].Task) != "SDLC-3" || rows[0].Status == nil || *rows[0].Status != http.StatusForbidden {
		t.Fatalf("журнал отказа: %+v", rows)
	}

	// AC-2: запрос отбивается целиком — multi_repo из него не включается.
	if code, body := patch(dev, "SDLC-1", map[string]any{
		"add_repositories": []string{docs}, "properties": map[string]any{"multi_repo": true}}); code != http.StatusForbidden {
		t.Fatalf("REST agent с multi_repo: %d %v", code, body)
	}
	if multiRepo("SDLC-1") != false || len(repos("SDLC-1")) != 1 {
		t.Fatalf("отказ изменил SDLC-1: multi_repo %v, область %v", multiRepo("SDLC-1"), repos("SDLC-1"))
	}

	// AC-4: тул MCP передаёт уровень — тот же отказ, что в REST.
	out, isErr := f.mcp(dev, "sdlc_update_task_scope", map[string]any{
		"task_key": "SDLC-3", "add": []string{docs}, "reason": "нужны решения"})
	if e, _ := out["error"].(map[string]any); !isErr || e["code"] != "forbidden" || e["message"] != refusal {
		t.Fatalf("MCP agent: %v", out)
	}
	if got := repos("SDLC-3"); slices.Contains(got, docs) {
		t.Fatalf("MCP agent изменил область: %v", got)
	}

	// AC-7: остальное как прежде — сужение боту не открыто, свойства без
	// репозиториев открыты, viewer отбит маршрутом.
	code, body = patch(dev, "SDLC-3", map[string]any{"remove_repositories": []string{"cadence-lab-tech/sdlc-pipeline-frontend"}})
	if msg, _ := errOf(body); code != http.StatusForbidden || !strings.Contains(msg, "сузить область") {
		t.Fatalf("сужение agent: %d %v", code, body)
	}
	if code, body := patch(dev, "SDLC-2", map[string]any{"properties": map[string]any{"multi_repo": true}}); code != http.StatusOK {
		t.Fatalf("свойства agent: %d %v", code, body)
	}
	if code, body := patch(watcher, "SDLC-3", addDocs); code != http.StatusForbidden {
		t.Fatalf("viewer: %d %v", code, body)
	}

	// AC-3: сопровождающий и администратор проекта расширяют, в ленте —
	// событие с репозиторием и автором.
	if code, body := patch(lead, "SDLC-3", addDocs); code != http.StatusOK {
		t.Fatalf("REST maintainer: %d %v", code, body)
	}
	events := scopeEvents("SDLC-3")
	if got := repos("SDLC-3"); len(events) != 1 || !slices.Contains(got, docs) {
		t.Fatalf("maintainer: область %v, событий %d", got, len(events))
	}
	if p := events[0]["payload"].(map[string]any); events[0]["actor_handle"] != "lead" ||
		!slices.Equal(p["added_repositories"].([]any), []any{docs}) {
		t.Fatalf("событие расширения: %v", events[0])
	}
	if code, body := patch(boss, "SDLC-1", map[string]any{
		"add_repositories": []string{docs}, "properties": map[string]any{"multi_repo": true}}); code != http.StatusOK {
		t.Fatalf("REST admin: %d %v", code, body)
	}
	if multiRepo("SDLC-1") != true || !slices.Contains(repos("SDLC-1"), docs) {
		t.Fatalf("admin: multi_repo %v, область %v", multiRepo("SDLC-1"), repos("SDLC-1"))
	}

	// AC-4: тул MCP от сопровождающего — успех.
	if out, isErr := f.mcp(lead, "sdlc_update_task_scope", map[string]any{
		"task_key": "SDLC-2", "add": []string{docs}, "reason": "нужны решения"}); isErr {
		t.Fatalf("MCP maintainer: %v", out)
	}
	if got := repos("SDLC-2"); !slices.Contains(got, docs) {
		t.Fatalf("MCP maintainer: область %v", got)
	}

	// AC-7, AC-9: сопровождающий сужает; человек agent по двум путям —
	// сотрудник, расширяет (CLAB-411); agent с maintainer по команде —
	// расширяет.
	if code, body := patch(lead, "SDLC-3", map[string]any{"remove_repositories": []string{docs}}); code != http.StatusOK {
		t.Fatalf("сужение maintainer: %d %v", code, body)
	}
	if code, body := patch(twice, "SDLC-3", addDocs); code != http.StatusOK {
		t.Fatalf("agent по двум путям: %d %v", code, body)
	}
	if code, body := patch(lead, "SDLC-3", map[string]any{"remove_repositories": []string{docs}}); code != http.StatusOK {
		t.Fatalf("сужение maintainer: %d %v", code, body)
	}
	if code, body := patch(mixed, "SDLC-3", addDocs); code != http.StatusOK {
		t.Fatalf("maintainer через команду: %d %v", code, body)
	}
	if got := repos("SDLC-3"); !slices.Contains(got, docs) {
		t.Fatalf("maintainer через команду: область %v", got)
	}

	// CLAB-411, AC-18: сотрудник без аренды сужает и расширяет область по
	// REST и тулом MCP; в ленте — актор и причина; последний репозиторий не
	// убирается (409).
	before := len(scopeEvents("SDLC-3"))
	if code, body := patch(employee, "SDLC-3", map[string]any{
		"remove_repositories": []string{docs}, "note": "решения не нужны"}); code != http.StatusOK {
		t.Fatalf("сужение сотрудником: %d %v", code, body)
	}
	if out, isErr := f.mcp(employee, "sdlc_update_task_scope", map[string]any{
		"task_key": "SDLC-3", "add": []string{docs}, "reason": "нужны решения"}); isErr {
		t.Fatalf("MCP сотрудник: %v", out)
	}
	events = scopeEvents("SDLC-3")
	if got := repos("SDLC-3"); len(events) != before+2 || !slices.Contains(got, docs) {
		t.Fatalf("сотрудник: область %v, событий %d (было %d)", got, len(events), before)
	}
	byEmployee := 0
	for _, e := range events {
		if e["actor_handle"] != "dev" {
			continue
		}
		byEmployee++
		if p := e["payload"].(map[string]any); p["note"] == "" {
			t.Fatalf("событие сотрудника без причины: %v", e)
		}
	}
	if byEmployee != 2 {
		t.Fatalf("событий сотрудника %d, ждали 2: %v", byEmployee, events)
	}
	if code, body := patch(employee, "SDLC-2", map[string]any{
		"remove_repositories": repos("SDLC-2")}); code != http.StatusConflict {
		t.Fatalf("сужение до пустой области: %d %v", code, body)
	}
}
