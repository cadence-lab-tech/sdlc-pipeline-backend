package httpapi

import (
	"net/http"
	"slices"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Самоапрув в ленте (CLAB-380, AC-16…AC-19): решение agent по human_approval и
// переход появляются в GET /v1/tasks/{key}/timeline с полями права и
// признаком self_approval; решение коллеги — без признака; отказ 403 событий
// не пишет, но остаётся в журнале действий.
func TestSelfApprovalInTimeline(t *testing.T) {
	f := newAuditFixture(t)
	ana := f.memberToken("user:ana", domain.MemberAgent, []string{"analyst"})
	ana2 := f.memberToken("user:ana2", domain.MemberAgent, []string{"analyst"})
	dev := f.memberToken("user:dev", domain.MemberAgent, []string{"developer"})
	maint := f.memberToken("user:maint", domain.MemberMaintainer, nil)
	const key = "SDLC-1"

	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	decide := func(token, gate, status string) (int, map[string]any) {
		return f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/analysis_done/gates/"+gate+"/decision",
			token, map[string]any{"status": status})
	}
	events := func(typ string) []map[string]any {
		t.Helper()
		code, body := f.json(http.MethodGet, "/v1/tasks/"+key+"/timeline?limit=500", f.root, nil)
		if code != http.StatusOK {
			t.Fatalf("timeline: %d %v", code, body)
		}
		var out []map[string]any
		for _, raw := range body["events"].([]any) {
			ev := raw.(map[string]any)
			if ev["type"] == typ {
				out = append(out, ev["payload"].(map[string]any))
			}
		}
		// Лента отдаётся от новых к старым; тесту удобнее по порядку записи.
		slices.Reverse(out)
		return out
	}
	basis := func(p map[string]any) []string {
		out := []string{}
		for _, v := range p["self_basis"].([]any) {
			out = append(out, v.(string))
		}
		return out
	}

	// AC-C1: отказ чужой роли — 403, события нет.
	if code, _ := decide(dev, "human_approval", "passed"); code != http.StatusForbidden {
		t.Fatalf("апрув разработчиком: %d", code)
	}
	if n := len(events("gate.decided")); n != 0 {
		t.Fatalf("отказ записал gate.decided: %d", n)
	}

	// Аренда + артефакт ana: оба основания. Коллега ana2 — без них.
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", ana, nil); code != http.StatusOK {
		t.Fatalf("claim: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/artifacts", ana, map[string]any{
		"kind": "analysis.brief", "title": "brief", "content": "# brief"}); code != http.StatusCreated {
		t.Fatalf("артефакт: %d %v", code, body)
	}
	if code, body := decide(ana2, "human_approval", "passed"); code != http.StatusOK {
		t.Fatalf("апрув коллеги: %d %v", code, body)
	}
	if code, body := decide(ana, "human_approval", "passed"); code != http.StatusOK {
		t.Fatalf("апрув ana: %d %v", code, body)
	}
	if code, body := decide(maint, "peer_review", "waived"); code != http.StatusOK {
		t.Fatalf("waive сопровождающим: %d %v", code, body)
	}

	// AC-16, AC-17: первое решение — коллеги, второе — ana, третье — maint.
	got := events("gate.decided")
	if len(got) != 3 {
		t.Fatalf("gate.decided: %d", len(got))
	}
	colleague, self, maintainer := got[0], got[1], got[2]
	if colleague["level"] != "agent" || colleague["via"] != "stage_role" || colleague["token_kind"] != "api" ||
		colleague["stage_role"] != "analyst" || colleague["self_approval"] != false || len(basis(colleague)) != 0 {
		t.Fatalf("коллега: %v", colleague)
	}
	if self["self_approval"] != true || !slices.Equal(basis(self), []string{"lease", "artifact"}) ||
		self["via"] != "stage_role" || self["gate"] != "human_approval" || self["status"] != "passed" {
		t.Fatalf("самоапрув: %v", self)
	}
	if maintainer["via"] != "level" || maintainer["level"] != "maintainer" || maintainer["self_approval"] != false ||
		maintainer["status"] != "waived" {
		t.Fatalf("сопровождающий: %v", maintainer)
	}

	// AC-18: переход agent — via и stage_role, без self_approval.
	for _, g := range []string{"artifacts", "drift", "repo_scope"} {
		if code, body := decide(maint, g, "waived"); code != http.StatusOK {
			t.Fatalf("waive %s: %d %v", g, code, body)
		}
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/analysis_done", ana, nil); code != http.StatusOK {
		t.Fatalf("analysis_done: %d %v", code, body)
	}
	adv := events("task.advanced")
	if len(adv) != 2 {
		t.Fatalf("task.advanced: %v", adv)
	}
	// start из new — статуса без исполнителя: stage_role пуст.
	if adv[0]["via"] != "level" || adv[0]["stage_role"] != "" || adv[0]["transition"] != "start" {
		t.Fatalf("task.advanced start: %v", adv[0])
	}
	last := adv[1]
	if last["via"] != "stage_role" || last["stage_role"] != "analyst" ||
		last["from"] != "analysis" || last["to"] != "architecture" || last["transition"] != "analysis_done" {
		t.Fatalf("task.advanced: %v", last)
	}
	if _, ok := last["self_approval"]; ok {
		t.Fatalf("self_approval на переходе: %v", last)
	}

	// AC-19: журнал действий — принципал, вид токена, ручка, исход; 403 тоже.
	code, audit := f.json(http.MethodGet, "/v1/tasks/"+key+"/audit?limit=200", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("audit: %d %v", code, audit)
	}
	var okDecision, deniedDecision bool
	for _, raw := range audit["entries"].([]any) {
		e := raw.(map[string]any)
		who, _ := e["principal"].(map[string]any)
		tok, _ := e["token"].(map[string]any)
		out, _ := e["outcome"].(map[string]any)
		action, _ := e["action"].(string)
		if (who["handle"] != "ana" && who["handle"] != "dev") || tok["kind"] != "api" {
			continue
		}
		if who["handle"] == "ana" && out["ok"] == true && action == "POST /v1/tasks/:taskKey/transitions/:transitionKey/gates/:gateKey/decision" {
			okDecision = true
		}
		if who["handle"] == "dev" && out["ok"] == false && out["status"] == float64(http.StatusForbidden) {
			deniedDecision = true
		}
	}
	if !okDecision || !deniedDecision {
		t.Fatalf("журнал: решение ana %v, отказ dev %v\n%v", okDecision, deniedDecision, audit)
	}
}
