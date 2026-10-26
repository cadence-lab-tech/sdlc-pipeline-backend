package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// CLAB-218, критерий 2: предел перепроверок необязателен, но заданный —
// целое ≥ 1 тем же разбором, что у attempts_limit. Негодный — проблема,
// а не предупреждение: активацию он держит.
func TestValidateReviewLimit(t *testing.T) {
	analysis := status("analysis", true, false)
	arch := status("architecture", false, true)
	analysis.Roles = []domain.StatusRole{{IsPrimary: true, RoleKey: "analyst"}}
	arch.Roles = []domain.StatusRole{{IsPrimary: true, RoleKey: "architect"}}

	cycle := func(config string) domain.Workflow {
		g := domain.Gate{Key: "peer_review", Type: domain.GateAgentReview,
			Blocking: true, Config: json.RawMessage(config)}
		return wf([]domain.Status{analysis, arch}, transition("done", &analysis, arch, g))
	}

	cases := []struct {
		name, config string
		invalid      bool
	}{
		{"без предела", `{"reviewer_role_key":"reviewer"}`, false},
		{"предел 2", `{"reviewer_role_key":"reviewer","max_attempts":2}`, false},
		{"предел 1", `{"reviewer_role_key":"reviewer","max_attempts":1}`, false},
		{"ноль", `{"reviewer_role_key":"reviewer","max_attempts":0}`, true},
		{"отрицательный", `{"reviewer_role_key":"reviewer","max_attempts":-1}`, true},
		{"дробный", `{"reviewer_role_key":"reviewer","max_attempts":2.5}`, true},
		{"строкой", `{"reviewer_role_key":"reviewer","max_attempts":"3"}`, true},
		{"null", `{"reviewer_role_key":"reviewer","max_attempts":null}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validateWorkflow(cycle(tc.config), allImplemented, true)
			var found *Problem
			for i, p := range v.Problems {
				if p.Code == "review_limit_invalid" {
					found = &v.Problems[i]
				}
			}
			if !tc.invalid {
				if !v.Valid || found != nil {
					t.Fatalf("цикл собран верно: %v", v.Problems)
				}
				return
			}
			if v.Valid || found == nil {
				t.Fatalf("негодный предел прошёл проверку: %+v", v)
			}
			if found.Gate != "peer_review" || found.Transition != "done" ||
				!strings.Contains(found.Message, "max_attempts") ||
				!strings.Contains(found.Message, `"peer_review"`) || !strings.Contains(found.Message, `"done"`) {
				t.Fatalf("проблема не называет гейт, переход и причину: %+v", found)
			}
			for _, w := range v.Warnings {
				if w.Code == "review_limit_invalid" {
					t.Fatalf("негодный предел попал в предупреждения: %+v", w)
				}
			}
		})
	}
}

// Критерии 8, 10, 11: agent_review решает только человек лишь при заданном
// пределе; без предела и с негодным пределом — как раньше.
func TestHumanOnlyByGate(t *testing.T) {
	gate := func(typ, config string) domain.Gate {
		return domain.Gate{Type: typ, Config: json.RawMessage(config)}
	}
	cases := []struct {
		name      string
		g         domain.Gate
		human     bool
		waiveOnly bool
	}{
		{"апрув", gate(domain.GateHumanApproval, `{"min_approvals":1}`), true, false},
		{"предел попыток", gate(domain.GateAttemptsLimit, `{"max_attempts":2}`), true, true},
		{"перепроверка с пределом", gate(domain.GateAgentReview, `{"reviewer_role_key":"reviewer","max_attempts":2}`), true, true},
		{"перепроверка без предела", gate(domain.GateAgentReview, `{"reviewer_role_key":"reviewer"}`), false, false},
		{"перепроверка с негодным пределом", gate(domain.GateAgentReview, `{"reviewer_role_key":"reviewer","max_attempts":2.5}`), false, false},
		{"чек-лист", gate(domain.GateChecklist, `{}`), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule, ok := humanOnly(tc.g)
			if ok != tc.human || rule.waiveOnly != tc.waiveOnly {
				t.Fatalf("humanOnly = %+v %v, ждали %v %v", rule, ok, tc.human, tc.waiveOnly)
			}
		})
	}
}
