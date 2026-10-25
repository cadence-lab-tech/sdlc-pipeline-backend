package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Предел времени этапа через транспорты (CLAB-221, критерии 12, 14, 17, 23):
// блок stage_limit в карточке, предпросмотре переходов, состоянии и MCP;
// отказ захвата агентом — 409 stage_time_exceeded с details в REST и тот же
// код в MCP; человек захват получает и снимает предел решением waived.
func TestStageLimitOverTransports(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO gates (transition_id, key, name, type, config)
		SELECT tr.id, 'time', 'Предел этапа', 'stage_time_limit', '{"max_duration": "1h"}'
		FROM workflow_transitions tr JOIN tasks t ON t.workflow_id = tr.workflow_id
		WHERE t.key = 'SDLC-1' AND tr.key = 'start'`); err != nil {
		t.Fatal(err)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}

	limitOf := func(body map[string]any) map[string]any {
		l, _ := body["stage_limit"].(map[string]any)
		return l
	}
	code, card := f.json("GET", "/v1/tasks/SDLC-1", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("карточка: %d %v", code, card)
	}
	l := limitOf(card)
	for _, k := range []string{"gate", "transition", "status", "max_duration", "entered_at", "deadline",
		"remaining_seconds", "overdue", "overdue_at", "waived"} {
		if _, ok := l[k]; !ok {
			t.Fatalf("в stage_limit карточки нет %q: %v", k, l)
		}
	}
	if l["gate"] != "time" || l["transition"] != "start" || l["status"] != "analysis" ||
		l["max_duration"] != "1h" || l["overdue"] != false || l["remaining_seconds"].(float64) <= 0 {
		t.Fatalf("stage_limit карточки: %v", l)
	}
	if _, ok := card["transitions"].([]any); !ok {
		t.Fatalf("transitions карточки сменили форму: %T", card["transitions"])
	}
	code, preview := f.json("GET", "/v1/tasks/SDLC-1/transitions", f.root, nil)
	if _, ok := preview["transitions"].([]any); code != http.StatusOK || !ok || limitOf(preview)["gate"] != "time" {
		t.Fatalf("предпросмотр переходов: %d %v", code, preview)
	}

	// Срок вышел: часы этапа — entered_at прогона.
	if _, err := f.pool.Exec(ctx, `
		UPDATE stage_runs sr SET entered_at = entered_at - interval '2 hours'
		FROM tasks t WHERE t.id = sr.task_id AND t.key = 'SDLC-1' AND sr.finished_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	code, refused := f.json("POST", "/v1/tasks/SDLC-1/claim", f.bot, map[string]any{})
	e, _ := refused["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "stage_time_exceeded" ||
		!strings.Contains(e["message"].(string), "SDLC-1") || d["gate"] != "time" ||
		d["transition"] != "start" || d["max_duration"] != "1h" || d["stage_run_id"] == nil || d["deadline"] == nil {
		t.Fatalf("захват агентом после срока: %d %v", code, refused)
	}
	out, isErr := f.mcp(f.bot, "sdlc_claim_task", map[string]any{"task_key": "SDLC-1"})
	if me, _ := out["error"].(map[string]any); !isErr || me["code"] != "stage_time_exceeded" {
		t.Fatalf("sdlc_claim_task после срока: %v", out)
	}
	for _, tool := range []string{"sdlc_get_task", "sdlc_check_gates"} {
		out, isErr := f.mcp(f.bot, tool, map[string]any{"task_key": "SDLC-1"})
		if isErr || limitOf(out)["overdue"] != true {
			t.Fatalf("%s: stage_limit %v", tool, out["stage_limit"])
		}
	}

	// Человек под своей сессией захват получает и видит просрочку.
	code, claimed := f.json("POST", "/v1/tasks/SDLC-1/claim", f.root, map[string]any{})
	if code != http.StatusOK || limitOf(claimed)["overdue"] != true {
		t.Fatalf("захват человеком: %d %v", code, claimed["stage_limit"])
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/release", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("release: %d %v", code, body)
	}
	if code, body := f.json("GET", "/v1/tasks/SDLC-1/state", f.root, nil); code != http.StatusOK || limitOf(body)["overdue"] != true {
		t.Fatalf("состояние: %d %v", code, body["stage_limit"])
	}

	// Снимает человек — waived на переходе входа; после этого агент забирает.
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/start/gates/time/decision", f.root,
		map[string]any{"status": "waived"}); code != http.StatusOK {
		t.Fatalf("снятие предела: %d %v", code, body)
	}
	code, claimed = f.json("POST", "/v1/tasks/SDLC-1/claim", f.bot, map[string]any{})
	if l := limitOf(claimed); code != http.StatusOK || l["waived"] != true || l["waived_by"] == nil {
		t.Fatalf("захват агентом после снятия: %d %v", code, claimed)
	}
}
