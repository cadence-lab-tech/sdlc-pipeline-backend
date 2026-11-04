package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// MCP sdlc_decide_gate (CLAB-410, R-12, AC-14, AC-15, K-4): тот же исход,
// коды и тексты, что у POST …/gates/:g/decision, для сотрудника, бота и
// токена задачи; токену задачи тул закрыт белым списком.
func TestDecideGateMCPParity(t *testing.T) {
	f := newAuditFixture(t)
	emp := f.memberToken("user:emp", domain.MemberAgent, []string{"developer"})
	bot := f.memberToken("agent:ana", domain.MemberAgent, []string{"analyst"})
	viewer := f.memberToken("user:watcher", domain.MemberViewer, nil)
	const key = "SDLC-1"
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/artifacts", f.root, map[string]any{
		"kind": "analysis.brief", "title": "brief", "content": "# brief"}); code != http.StatusCreated {
		t.Fatalf("артефакт: %d %v", code, body)
	}
	rest := func(token, tr, gate string, body map[string]any) (int, map[string]any) {
		return f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/"+tr+"/gates/"+gate+"/decision", token, body)
	}
	mcp := func(token, tr, gate string, body map[string]any) (map[string]any, bool) {
		args := map[string]any{"task_key": key, "transition": tr, "gate": gate}
		for k, v := range body {
			args[k] = v
		}
		return f.mcp(token, "sdlc_decide_gate", args)
	}
	codeOf := map[int]string{http.StatusBadRequest: "invalid_input", http.StatusForbidden: "forbidden",
		http.StatusNotFound: "not_found", http.StatusConflict: "conflict"}

	// Отказы: код и текст одинаковы на двух транспортах.
	wrong := "00000000-0000-0000-0000-000000000000"
	for _, c := range []struct {
		name, token, tr, gate string
		body                  map[string]any
		code                  int
	}{
		{"бот: апрув", bot, "analysis_done", "human_approval", map[string]any{"status": "passed"}, http.StatusForbidden},
		{"сотрудник: waived апрува", emp, "analysis_done", "human_approval", map[string]any{"status": "waived"}, http.StatusForbidden},
		{"сотрудник: перепроверка без предела", emp, "analysis_done", "peer_review", map[string]any{"status": "passed"}, http.StatusForbidden},
		{"viewer", viewer, "analysis_done", "human_approval", map[string]any{"status": "passed"}, http.StatusForbidden},
		{"неизвестный гейт", emp, "analysis_done", "nope", map[string]any{"status": "passed"}, http.StatusNotFound},
		{"неизвестный вердикт", emp, "analysis_done", "human_approval", map[string]any{"status": "maybe"}, http.StatusBadRequest},
		{"устаревший seen", emp, "analysis_done", "human_approval",
			map[string]any{"status": "passed", "seen_artifact_id": wrong}, http.StatusConflict},
	} {
		code, rbody := rest(c.token, c.tr, c.gate, c.body)
		rmsg, rreason := errOf(rbody)
		out, isErr := mcp(c.token, c.tr, c.gate, c.body)
		mmsg, mreason := errOf(out)
		mcode, _ := out["error"].(map[string]any)["code"].(string)
		if code != c.code || !isErr || mcode != codeOf[c.code] || mmsg != rmsg || mreason != rreason {
			t.Fatalf("%s: REST %d %q %q / MCP %v", c.name, code, rmsg, rreason, out)
		}
		if c.code == http.StatusConflict && mreason != workflow.SeenConflict {
			t.Fatalf("%s: причина %q", c.name, mreason)
		}
	}

	// Сотрудник через MCP ставит апрув чужого этапа со сверкой; в ленте —
	// via=level, token_kind=api, принципал сотрудника.
	_, card := f.json(http.MethodGet, "/v1/tasks/"+key+"/artifacts", f.root, nil)
	var latest string
	if arts, ok := card["artifacts"].([]any); ok && len(arts) > 0 {
		latest, _ = arts[len(arts)-1].(map[string]any)["id"].(string)
	}
	if latest == "" {
		t.Fatalf("нет артефакта этапа: %v", card)
	}
	out, isErr := mcp(emp, "analysis_done", "human_approval", map[string]any{
		"status": "passed", "seen_artifact_id": latest, "details": map[string]any{"note": "проверила"}})
	if isErr || out["status"] != "passed" || out["gate_key"] != "human_approval" {
		t.Fatalf("апрув сотрудником через MCP: %v", out)
	}
	var via, tokenKind, handle, note string
	if err := f.pool.QueryRow(t.Context(), `
		SELECT e.payload->>'via', e.payload->>'token_kind', p.handle, e.payload->'details'->>'note'
		FROM task_events e JOIN tasks tk ON tk.id = e.task_id JOIN principals p ON p.id = e.actor_id
		WHERE tk.key = $1 AND e.type = 'gate.decided' ORDER BY e.created_at DESC, e.id DESC LIMIT 1`, key).
		Scan(&via, &tokenKind, &handle, &note); err != nil ||
		via != workflow.ViaLevel || tokenKind != "api" || handle != "emp" || note != "проверила" {
		t.Fatalf("gate.decided: via=%q token_kind=%q handle=%q note=%q %v", via, tokenKind, handle, note, err)
	}

	// Токен задачи: тул вне белого списка — отказ исполнителю на любой
	// вердикт, даже по чек-листу своего этапа; REST-маршрут чек-листа — как
	// раньше.
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-2/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start SDLC-2: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-2/claim", f.bot, map[string]any{}); code != http.StatusOK {
		t.Fatalf("захват: %d %v", code, body)
	}
	code, issued := f.json(http.MethodPost, "/v1/tasks/SDLC-2/task-token", f.bot, nil)
	if code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %v", code, issued)
	}
	tt := issued["token"].(string)
	for _, status := range []string{"passed", "failed", "waived"} {
		out, isErr := f.mcp(tt, "sdlc_decide_gate", map[string]any{
			"task_key": "SDLC-2", "transition": "analysis_done", "gate": "human_approval", "status": status})
		if msg, _ := errOf(out); !isErr || !strings.HasPrefix(msg, workflow.ExecutorRefusal) {
			t.Fatalf("sdlc_decide_gate токеном задачи (%s): %v", status, out)
		}
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-2/transitions/analysis_done/gates/human_approval/decision",
		tt, map[string]any{"status": "passed"}); code != http.StatusForbidden || !strings.Contains(body["error"].(map[string]any)["message"].(string), workflow.ExecutorRefusal) {
		t.Fatalf("апрув токеном задачи по REST: %d %v", code, body)
	}
}
