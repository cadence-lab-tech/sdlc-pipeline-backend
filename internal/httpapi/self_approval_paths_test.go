package httpapi

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// CLAB-380, проверка тестировщика: поля ленты по всем путям входа — токен
// session и токен задачи по REST, переход через MCP. Личный токен и
// заголовок покрыты TestSelfApprovalInTimeline и dev/e2e.sh.
func TestSelfApprovalFieldsAcrossEntryPaths(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	ana := f.memberToken("user:ana", domain.MemberAgent, []string{"analyst"})
	maint := f.memberToken("user:maint", domain.MemberMaintainer, nil)
	arch := f.memberToken("user:arch", domain.MemberAgent, []string{"architect"})
	store := postgres.New(f.pool)

	// Токен вида session участнику ana2 (как после входа через провайдера).
	if _, err := store.ResolveActor(ctx, store.DB(), "ana2", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.SetMember(ctx, f.admin, "SDLC", "user:ana2", domain.MemberAgent, []string{"analyst"}); err != nil {
		t.Fatal(err)
	}
	p, err := store.ResolveActor(ctx, store.DB(), "ana2", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	raw, hash, prefix, err := workflow.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	if _, err := store.CreateToken(ctx, store.DB(), postgres.CreateTokenInput{
		PrincipalID: p.ID, Name: "вход", TokenHash: hash, Prefix: prefix, ExpiresAt: &exp,
		CreatedBy: p.ID, Kind: domain.TokenSession}); err != nil {
		t.Fatal(err)
	}
	const key = "SDLC-1"
	events := func(typ string) []map[string]any {
		t.Helper()
		_, body := f.json(http.MethodGet, "/v1/tasks/"+key+"/timeline?limit=500", f.root, nil)
		var out []map[string]any
		for _, r := range body["events"].([]any) {
			ev := r.(map[string]any)
			if ev["type"] == typ {
				out = append(out, ev["payload"].(map[string]any))
			}
		}
		slices.Reverse(out)
		return out
	}
	decide := func(tr, gate, tok, status string) {
		t.Helper()
		if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/"+tr+"/gates/"+gate+"/decision", tok,
			map[string]any{"status": status}); code != http.StatusOK {
			t.Fatalf("%s/%s: %d %v", tr, gate, code, b)
		}
	}
	if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start %d %v", code, b)
	}

	// session, аренды нет — «чужой».
	decide("analysis_done", "human_approval", raw, "passed")
	ev := events("gate.decided")[0]
	if ev["token_kind"] != "session" || ev["via"] != "stage_role" || ev["level"] != "agent" ||
		ev["self_approval"] != false || len(ev["self_basis"].([]any)) != 0 {
		t.Fatalf("session: %v", ev)
	}

	// Тот же session после аренды ana2 — «свой» по аренде.
	if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", raw, nil); code != http.StatusOK {
		t.Fatalf("claim session: %d %v", code, b)
	}
	decide("analysis_done", "human_approval", raw, "failed")
	ev = events("gate.decided")[1]
	if ev["token_kind"] != "session" || ev["status"] != "failed" || ev["self_approval"] != true ||
		!slices.Equal([]any{"lease"}, ev["self_basis"].([]any)) {
		t.Fatalf("session после аренды: %v", ev)
	}
	decide("analysis_done", "human_approval", ana, "passed") // ana без аренды и артефактов
	if ev = events("gate.decided")[2]; ev["token_kind"] != "api" || ev["self_approval"] != false {
		t.Fatalf("api чужой: %v", ev)
	}
	for _, g := range []string{"artifacts", "peer_review", "drift", "repo_scope"} {
		decide("analysis_done", g, maint, "waived")
	}

	// Переход через MCP: via и stage_role те же, что по REST.
	if out, isErr := f.mcp(ana, "sdlc_request_transition", map[string]any{"task_key": key, "transition": "analysis_done"}); isErr {
		t.Fatalf("MCP переход: %v", out)
	}
	adv := events("task.advanced")
	last := adv[len(adv)-1]
	if last["transition"] != "analysis_done" || last["via"] != "stage_role" || last["stage_role"] != "analyst" {
		t.Fatalf("MCP task.advanced: %v", last)
	}
	if _, ok := last["self_approval"]; ok {
		t.Fatalf("self_approval на переходе: %v", last)
	}

	// Токен задачи по REST: чек-лист passed.
	if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", arch, nil); code != http.StatusOK {
		t.Fatalf("claim arch: %d %v", code, b)
	}
	code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/task-token", arch, nil)
	if code != http.StatusCreated {
		t.Fatalf("task-token: %d %v", code, b)
	}
	taskTok := b["token"].(string)
	decide("architecture_done", "traceability", taskTok, "passed")
	all := events("gate.decided")
	ev = all[len(all)-1]
	if ev["gate"] != "traceability" || ev["token_kind"] != "task" || ev["via"] != "task" ||
		ev["stage_role"] != "architect" || ev["self_approval"] != true ||
		!slices.Equal([]any{"lease"}, ev["self_basis"].([]any)) {
		t.Fatalf("токен задачи: %v", ev)
	}
}
