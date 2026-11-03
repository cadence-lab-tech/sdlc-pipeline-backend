package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// CLAB-393 (AC-1…AC-7, AC-10, AC-11): вердикт прогона в карточке несёт ту же
// пометку самоапрува, что gate.decided того же решения, на всех путях входа —
// токен api, session и токен задачи; sdlc_get_task отдаёт то же, что REST.
func TestVerdictCarriesSelfApprovalAcrossEntryPaths(t *testing.T) {
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
	decide := func(tr, gate, tok, status string) {
		t.Helper()
		if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/"+tr+"/gates/"+gate+"/decision", tok,
			map[string]any{"status": status}); code != http.StatusOK {
			t.Fatalf("%s/%s: %d %v", tr, gate, code, b)
		}
	}
	// Последнее gate.decided гейта (лента — от новых к старым).
	decided := func(gate string) map[string]any {
		t.Helper()
		_, body := f.json(http.MethodGet, "/v1/tasks/"+key+"/timeline?limit=500", f.root, nil)
		for _, r := range body["events"].([]any) {
			ev := r.(map[string]any)
			if ev["type"] == "gate.decided" && ev["payload"].(map[string]any)["gate"] == gate {
				return ev["payload"].(map[string]any)
			}
		}
		t.Fatalf("в ленте нет gate.decided по %s", gate)
		return nil
	}
	// Запись вердикта карточки по ключам перехода и гейта.
	verdict := func(tr, gate string) map[string]any {
		t.Helper()
		var found map[string]any
		for _, run := range cardRuns(t, f, key) {
			for _, g := range run["gate_results"].([]any) {
				v := g.(map[string]any)
				if v["gate_key"] == gate && v["transition_key"] == tr {
					found = v
				}
			}
		}
		if found == nil {
			t.Fatalf("в карточке нет вердикта %s/%s: %v", tr, gate, cardRuns(t, f, key))
		}
		return found
	}
	// Вердикт совпадает с gate.decided: признак и основания.
	same := func(name, tr, gate string, self bool, basis []string) {
		t.Helper()
		v, ev := verdict(tr, gate), decided(gate)
		if v["self_approval"] != self || ev["self_approval"] != self {
			t.Fatalf("%s: вердикт %v, событие %v, ждали %v", name, v["self_approval"], ev["self_approval"], self)
		}
		got, ok := v["self_basis"].([]any)
		if !ok || got == nil {
			t.Fatalf("%s: self_basis не массив: %#v", name, v["self_basis"])
		}
		if len(got) != len(basis) || !reflect.DeepEqual(got, ev["self_basis"]) {
			t.Fatalf("%s: self_basis вердикта %v, события %v, ждали %v", name, got, ev["self_basis"], basis)
		}
		for i, b := range basis {
			if got[i] != b {
				t.Fatalf("%s: self_basis %v, ждали %v", name, got, basis)
			}
		}
	}

	if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start %d %v", code, b)
	}

	// AC-2, AC-7: session без аренды и артефактов — чужое решение, [].
	decide("analysis_done", "human_approval", raw, "passed")
	same("session чужой", "analysis_done", "human_approval", false, []string{})

	// AC-1, AC-7: тот же session после аренды — по аренде.
	if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", raw, nil); code != http.StatusOK {
		t.Fatalf("claim session: %d %v", code, b)
	}
	decide("analysis_done", "human_approval", raw, "failed")
	same("session с арендой", "analysis_done", "human_approval", true, []string{"lease"})
	if v := verdict("analysis_done", "human_approval"); v["status"] != "failed" {
		t.Fatalf("статус вердикта: %v", v["status"])
	}

	// Сдача артефакта снимает апрув человека (решение 0042): вердикта по
	// human_approval больше нет, и пометки на нём тоже.
	if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/artifacts", raw, map[string]any{
		"kind": "analysis.brief", "title": "после решения", "content": "# x"}); code != http.StatusCreated {
		t.Fatalf("артефакт: %d %v", code, b)
	}
	for _, run := range cardRuns(t, f, key) {
		for _, g := range run["gate_results"].([]any) {
			if g.(map[string]any)["gate_key"] == "human_approval" {
				t.Fatalf("апрув пережил новую сдачу: %v", g)
			}
		}
	}

	// AC-3: новое решение после сдачи — оба основания в фиксированном порядке.
	decide("analysis_done", "human_approval", raw, "passed")
	same("аренда и артефакт", "analysis_done", "human_approval", true, []string{"lease", "artifact"})

	// AC-7: личный токен (api) без аренды и артефактов — чужое решение.
	decide("analysis_done", "human_approval", ana, "passed")
	same("api чужой", "analysis_done", "human_approval", false, []string{})

	// AC-6 (в): гейт другого типа — решение записано в ленту с признаком, но на
	// вердикте ключей нет (в JSON, не null).
	for _, g := range []string{"artifacts", "peer_review", "drift", "repo_scope"} {
		decide("analysis_done", g, maint, "waived")
	}
	for _, g := range []string{"artifacts", "peer_review", "drift", "repo_scope"} {
		v := verdict("analysis_done", g)
		for _, k := range []string{"self_approval", "self_basis"} {
			if _, ok := v[k]; ok {
				t.Fatalf("гейт %s: на вердикте есть %s: %v", g, k, v)
			}
		}
		if v["decided_by_handle"] != "maint" {
			t.Fatalf("гейт %s: решил %v", g, v["decided_by_handle"])
		}
	}

	// Переход, затем токен задачи (AC-4): чек-лист passed.
	if out, isErr := f.mcp(ana, "sdlc_request_transition", map[string]any{"task_key": key, "transition": "analysis_done"}); isErr {
		t.Fatalf("MCP переход: %v", out)
	}
	if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", arch, nil); code != http.StatusOK {
		t.Fatalf("claim arch: %d %v", code, b)
	}
	code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/task-token", arch, nil)
	if code != http.StatusCreated {
		t.Fatalf("task-token: %d %v", code, b)
	}
	decide("architecture_done", "traceability", b["token"].(string), "passed")
	same("токен задачи", "architecture_done", "traceability", true, []string{"lease"})
	if ev := decided("traceability"); ev["token_kind"] != "task" {
		t.Fatalf("вид токена: %v", ev["token_kind"])
	}

	// AC-5: сдача артефакта тем же принципалом в том же прогоне после решения
	// (чек-лист новую сдачу переживает) пометку не меняет: пересчёт дал бы
	// ["lease", "artifact"], а в gate.decided записано ["lease"].
	if code, b := f.json(http.MethodPost, "/v1/tasks/"+key+"/artifacts", arch, map[string]any{
		"kind": "arch.adr", "title": "после чек-листа", "content": "# y"}); code != http.StatusCreated {
		t.Fatalf("артефакт после решения: %d %v", code, b)
	}
	same("после сдачи артефакта", "architecture_done", "traceability", true, []string{"lease"})

	// AC-11: прежние ключи вердикта на месте.
	v := verdict("architecture_done", "traceability")
	for _, k := range []string{"gate_key", "gate_name", "gate_type", "transition_key", "status", "details",
		"decided_by_handle", "decided_at"} {
		if _, ok := v[k]; !ok {
			t.Errorf("пропал ключ %s: %v", k, v)
		}
	}

	// AC-10: sdlc_get_task отдаёт те же вердикты, что REST.
	out, isErr := f.mcp(f.root, "sdlc_get_task", map[string]any{"task_key": key})
	if isErr {
		t.Fatalf("sdlc_get_task: %v", out)
	}
	runs := cardRuns(t, f, key)
	mcpRuns, _ := out["stage_runs"].([]any)
	if len(mcpRuns) != len(runs) {
		t.Fatalf("прогонов в MCP %d, в REST %d", len(mcpRuns), len(runs))
	}
	withMark := 0
	for i, m := range mcpRuns {
		r := m.(map[string]any)
		if !reflect.DeepEqual(r["gate_results"], runs[i]["gate_results"]) {
			t.Errorf("прогон %v: gate_results MCP %v, REST %v", r["id"], r["gate_results"], runs[i]["gate_results"])
		}
		for _, g := range r["gate_results"].([]any) {
			if _, ok := g.(map[string]any)["self_approval"]; ok {
				withMark++
			}
		}
	}
	if withMark != 2 {
		t.Errorf("вердиктов с пометкой в MCP: %d, ждали 2 (human_approval и traceability)", withMark)
	}
}
