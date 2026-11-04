package httpapi

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// memberToken заводит человека уровня level с ролями специалистов roles
// (nil — любые) и выдаёт ему личный токен проекта SDLC; tokenRoles сужает
// токен.
func (f *auditFixture) memberToken(handle string, level domain.MemberRole, roles []string, tokenRoles ...string) string {
	f.t.Helper()
	ctx := context.Background()
	store := postgres.New(f.pool)
	if _, err := store.ResolveActor(ctx, store.DB(), strings.TrimPrefix(handle, "user:"), domain.PrincipalUser); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.auth.SetMember(ctx, f.admin, "SDLC", handle, level, roles); err != nil {
		f.t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{
		Name: handle, Principal: handle, ProjectKey: "SDLC", RoleKeys: tokenRoles})
	if err != nil {
		f.t.Fatal(err)
	}
	return issued.Token
}

func errOf(body map[string]any) (message, reason string) {
	e, _ := body["error"].(map[string]any)
	message, _ = e["message"].(string)
	if d, ok := e["details"].(map[string]any); ok {
		reason, _ = d["reason"].(string)
	}
	return message, reason
}

func (f *auditFixture) status(key string) string {
	f.t.Helper()
	task, err := postgres.New(f.pool).GetTaskByKey(context.Background(), postgres.New(f.pool).DB(), key)
	if err != nil {
		f.t.Fatal(err)
	}
	return task.StatusKey
}

// Право этапа по REST и MCP (CLAB-379, решение 0072). Заменяет
// характеризацию прежнего поведения (первый коммит ветки): личный токен
// agent больше не запускает задачу из статуса без исполнителя, не снимает
// её и не двигает чужой этап (AC-3, AC-4, AC-6, AC-10); решает апрув и
// чек-лист своего этапа (AC-1, AC-2); предсказание в ответе /transitions
// (AC-13); один отказ на двух транспортах (AC-14, AC-A1); область не
// открывается (AC-15); право читается на момент запроса (AC-A5).
func TestStageRightsREST(t *testing.T) {
	f := newAuditFixture(t)
	dev := f.memberToken("user:dev", domain.MemberAgent, []string{"developer"})
	ana := f.memberToken("user:ana", domain.MemberAgent, []string{"analyst"})
	viewer := f.memberToken("user:watcher", domain.MemberViewer, nil)
	// Участник с [analyst, developer], токен сужен до developer (AC-5).
	narrow := f.memberToken("user:both", domain.MemberAgent, []string{"analyst", "developer"}, "developer")
	post := func(path, token string, body any) (int, map[string]any) {
		t.Helper()
		return f.json(http.MethodPost, path, token, body)
	}
	decide := func(key, tr, gate, token, status string) (int, map[string]any) {
		return post("/v1/tasks/"+key+"/transitions/"+tr+"/gates/"+gate+"/decision", token, map[string]any{"status": status})
	}

	// AC-6: start из new — статуса без исполнителя — agent закрыт при любых
	// ролях, в том числе эпику; сопровождающему — как раньше.
	for _, c := range []struct{ token, key string }{{dev, "SDLC-1"}, {f.bot, "SDLC-1"}, {f.bot, "SDLC-4"}} {
		code, body := post("/v1/tasks/"+c.key+"/transitions/start", c.token, nil)
		if _, reason := errOf(body); code != http.StatusForbidden || reason != workflow.DenyNoStageRole {
			t.Fatalf("start %s agent: %d %v", c.key, code, body)
		}
	}
	if code, body := post("/v1/tasks/SDLC-1/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start сопровождающим: %d %v", code, body)
	}

	// AC-13: предсказание актору; прежние поля на месте.
	_, preview := f.json(http.MethodGet, "/v1/tasks/SDLC-1/transitions", ana, nil)
	byKey := map[string]map[string]any{}
	for _, raw := range preview["transitions"].([]any) {
		p := raw.(map[string]any)
		byKey[p["key"].(string)] = p
		for _, k := range []string{"allowed", "gates", "to_status", "lands_in", "skips"} {
			if _, ok := p[k]; !ok {
				t.Fatalf("пропало поле %s: %v", k, p)
			}
		}
	}
	if byKey["analysis_done"]["actor_may"] != true || byKey["cancel"]["actor_may"] != false ||
		byKey["cancel"]["actor_deny"].(map[string]any)["reason"] != workflow.DenyNoStageRole {
		t.Fatalf("actor_may: %v / %v", byKey["analysis_done"], byKey["cancel"])
	}
	for _, raw := range byKey["analysis_done"]["gates"].([]any) {
		g := raw.(map[string]any)
		decisions, _ := g["actor_decisions"].([]any)
		switch g["gate_key"] {
		case "human_approval":
			if g["actor_may_decide"] != true || !slices.Equal(decisions, []any{"passed", "failed"}) {
				t.Fatalf("апрув в предсказании: %v", g)
			}
		case "peer_review":
			if g["actor_may_decide"] != false || g["actor_deny"].(map[string]any)["reason"] != workflow.DenyMaintainerAction {
				t.Fatalf("перепроверка в предсказании: %v", g)
			}
		}
	}

	// AC-3, AC-5: чужой этап — 403 и статус прежний, в том числе у токена,
	// суженного до developer; свой — до гейтов (409).
	for _, token := range []string{dev, narrow} {
		code, body := post("/v1/tasks/SDLC-1/transitions/analysis_done", token, nil)
		if msg, reason := errOf(body); code != http.StatusForbidden || reason != workflow.DenyStageRole ||
			!strings.Contains(msg, "analyst") || !strings.Contains(msg, "[developer]") {
			t.Fatalf("analysis_done чужого этапа: %d %v", code, body)
		}
	}
	if f.status("SDLC-1") != "analysis" {
		t.Fatalf("задача сдвинулась: %s", f.status("SDLC-1"))
	}
	if code, body := post("/v1/tasks/SDLC-1/transitions/analysis_done", ana, nil); code != http.StatusConflict {
		t.Fatalf("analysis_done своего этапа: %d %v", code, body)
	}
	// AC-4: cancel — сопровождающему, и на своём этапе тоже.
	if code, body := post("/v1/tasks/SDLC-1/transitions/cancel", ana, nil); code != http.StatusForbidden {
		t.Fatalf("cancel аналитиком: %d %v", code, body)
	}
	// AC-9: viewer — 403 на переход и решение, как раньше.
	if code, _ := post("/v1/tasks/SDLC-1/transitions/analysis_done", viewer, nil); code != http.StatusForbidden {
		t.Fatalf("переход viewer: %d", code)
	}
	if code, _ := decide("SDLC-1", "analysis_done", "human_approval", viewer, "passed"); code != http.StatusForbidden {
		t.Fatalf("решение viewer: %d", code)
	}

	// AC-14, AC-A1: тот же отказ по MCP — код и текст; артефакты не сданы.
	restCode, restBody := post("/v1/tasks/SDLC-1/transitions/analysis_done", dev, nil)
	restMsg, _ := errOf(restBody)
	var before int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM artifacts a JOIN tasks t ON t.id = a.task_id WHERE t.key = 'SDLC-1'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	out, isErr := f.mcp(dev, "sdlc_request_transition", map[string]any{
		"task_key": "SDLC-1", "transition": "analysis_done",
		"artifacts": []map[string]any{{"kind": "analysis.brief", "content": "# не должен лечь"}},
	})
	mcpMsg, _ := errOf(out)
	if !isErr || restCode != http.StatusForbidden || out["error"].(map[string]any)["code"] != "forbidden" || mcpMsg != restMsg {
		t.Fatalf("REST и MCP расходятся: %d %q / %v", restCode, restMsg, out)
	}
	var after int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM artifacts a JOIN tasks t ON t.id = a.task_id WHERE t.key = 'SDLC-1'`).Scan(&after); err != nil || after != before {
		t.Fatalf("отказ оставил артефакты: было %d, стало %d (%v)", before, after, err)
	}
	if out, isErr := f.mcp(dev, "sdlc_request_transition", map[string]any{"task_key": "SDLC-2", "transition": "start"}); !isErr {
		t.Fatalf("MCP start agent: %v", out)
	}

	// AC-1, AC-2: апрув своего этапа — да, чужого — нет.
	if code, body := decide("SDLC-1", "analysis_done", "human_approval", dev, "passed"); code != http.StatusForbidden {
		t.Fatalf("апрув разработчиком: %d %v", code, body)
	}
	for _, s := range []string{"failed", "passed"} {
		if code, body := decide("SDLC-1", "analysis_done", "human_approval", ana, s); code != http.StatusOK {
			t.Fatalf("апрув %s аналитиком: %d %v", s, code, body)
		}
	}
	var by string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT p.handle FROM task_events e JOIN tasks t ON t.id = e.task_id JOIN principals p ON p.id = e.actor_id
		WHERE t.key = 'SDLC-1' AND e.type = 'gate.decided' ORDER BY e.created_at DESC, e.id DESC LIMIT 1`).Scan(&by); err != nil || by != "ana" {
		t.Fatalf("gate.decided: %q %v", by, err)
	}
	// AC-15 пересмотрен CLAB-411 (В-3 эпика CLAB-404): сотрудник уровня
	// agent меняет область без права этапа; последний репозиторий области
	// по-прежнему не убирается — 409 движка, а не 403 права.
	if code, _ := f.json(http.MethodPatch, "/v1/tasks/SDLC-1/scope", ana,
		map[string]any{"remove_repositories": []string{"cadence-lab-tech/sdlc-pipeline-backend"}}); code != http.StatusConflict {
		t.Fatalf("область аналитиком: %d", code)
	}

	// AC-A5: предсказание видело свой этап, задача ушла — решение по гейту
	// перехода не из текущего статуса отбито; сопровождающему — как раньше.
	for _, g := range []string{"artifacts", "peer_review", "drift", "repo_scope"} {
		if code, body := decide("SDLC-1", "analysis_done", g, f.root, "waived"); code != http.StatusOK {
			t.Fatalf("waive %s: %d %v", g, code, body)
		}
	}
	if code, body := post("/v1/tasks/SDLC-1/transitions/analysis_done", ana, nil); code != http.StatusOK || f.status("SDLC-1") != "architecture" {
		t.Fatalf("analysis_done аналитиком: %d %v", code, body)
	}
	code, body := decide("SDLC-1", "analysis_done", "human_approval", ana, "passed")
	if _, reason := errOf(body); code != http.StatusForbidden || reason != workflow.DenyNoStageRole {
		t.Fatalf("гейт перехода не из текущего статуса: %d %v", code, body)
	}
	if code, body := decide("SDLC-1", "analysis_done", "human_approval", f.root, "passed"); code != http.StatusOK {
		t.Fatalf("то же сопровождающим: %d %v", code, body)
	}
	if code, body := post("/v1/tasks/SDLC-1/transitions/cancel", f.root, nil); code != http.StatusOK {
		t.Fatalf("cancel сопровождающим: %d %v", code, body)
	}
}
