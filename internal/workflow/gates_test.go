package workflow

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

func testEngine(strict bool) *Engine {
	return NewEngine(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{StrictGates: strict})
}

func gate(t string, config string) domain.Gate {
	return domain.Gate{
		ID: uuid.New(), Key: "g", Name: "гейт", Type: t,
		Config: json.RawMessage(config), Blocking: true,
	}
}

func artifact(kind, status string, run uuid.UUID) domain.Artifact {
	return domain.Artifact{ID: uuid.New(), Kind: kind, Status: status, StageRunID: &run}
}

func TestArtifactsRequired(t *testing.T) {
	e := testEngine(false)
	run := uuid.New()

	cases := []struct {
		name   string
		config string
		stage  []domain.Artifact
		task   []domain.Artifact
		want   string
	}{
		{
			name:   "все артефакты сданы в этой попытке",
			config: `{"kinds":["analysis.brief","analysis.acceptance_criteria"]}`,
			stage: []domain.Artifact{
				artifact("analysis.brief", domain.ArtifactSubmitted, run),
				artifact("analysis.acceptance_criteria", domain.ArtifactSubmitted, run),
			},
			want: domain.GateResultPassed,
		},
		{
			name:   "одного не хватает",
			config: `{"kinds":["analysis.brief","analysis.acceptance_criteria"]}`,
			stage:  []domain.Artifact{artifact("analysis.brief", domain.ArtifactSubmitted, run)},
			want:   domain.GateResultFailed,
		},
		{
			// Ради этого случая область по умолчанию — попытка, а не задача:
			// после отката задача заходит на статус заново, и артефакты
			// прошлой попытки не должны закрывать гейт вместо новой работы.
			name:   "артефакты прошлой попытки не считаются",
			config: `{"kinds":["analysis.brief"]}`,
			stage:  nil,
			task:   []domain.Artifact{artifact("analysis.brief", domain.ArtifactSubmitted, uuid.New())},
			want:   domain.GateResultFailed,
		},
		{
			name:   "scope=task разрешает артефакт задачи целиком",
			config: `{"kinds":["analysis.brief"],"scope":"task"}`,
			task:   []domain.Artifact{artifact("analysis.brief", domain.ArtifactSubmitted, uuid.New())},
			want:   domain.GateResultPassed,
		},
		{
			name:   "требуемый статус артефакта не совпал",
			config: `{"kinds":["analysis.brief"],"status":"approved"}`,
			stage:  []domain.Artifact{artifact("analysis.brief", domain.ArtifactSubmitted, run)},
			want:   domain.GateResultFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.evalArtifactsRequired(context.Background(), nil, EvalInput{
				Gate:           gate(domain.GateArtifactsRequired, tc.config),
				StageRun:       domain.StageRun{ID: run},
				StageArtifacts: tc.stage,
				TaskArtifacts:  tc.task,
			})
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if out.Status != tc.want {
				t.Fatalf("статус %q, ожидался %q (%s)", out.Status, tc.want, out.Reason)
			}
		})
	}
}

func TestChecklist(t *testing.T) {
	e := testEngine(false)
	cfg := `{"items":[{"id":"a","text":"первое","required":true},
	                 {"id":"b","text":"второе","required":true},
	                 {"id":"c","text":"необязательное","required":false}]}`

	cases := []struct {
		name     string
		decision *domain.GateResult
		want     string
	}{
		{"решения нет", nil, domain.GateResultPending},
		{"отмечено не всё", &domain.GateResult{Details: json.RawMessage(`{"checked":["a"]}`)}, domain.GateResultPending},
		{"отмечены оба обязательных", &domain.GateResult{Details: json.RawMessage(`{"checked":["a","b"]}`)}, domain.GateResultPassed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.evalChecklist(context.Background(), nil, EvalInput{
				Gate:     gate(domain.GateChecklist, cfg),
				Decision: tc.decision,
			})
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if out.Status != tc.want {
				t.Fatalf("статус %q, ожидался %q", out.Status, tc.want)
			}
		})
	}
}

func TestSeverityAtLeast(t *testing.T) {
	cases := map[string][]string{
		"high":   {"high"},
		"medium": {"medium", "high"},
		"low":    {"low", "medium", "high"},
		"":       {"low", "medium", "high"},
	}
	for min, want := range cases {
		got := severityAtLeast(min)
		if len(got) != len(want) {
			t.Fatalf("min=%q: получили %v, ожидали %v", min, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("min=%q: получили %v, ожидали %v", min, got, want)
			}
		}
	}
}

// Незарегистрированный тип гейта — единственное место, где поведение движка
// зависит от режима: в отладке цикл проходится насквозь, в продакшене нет.
func TestUnimplementedGateDependsOnStrictMode(t *testing.T) {
	g := gate(domain.GateAgentReview, `{}`)

	if out := testEngine(false).unimplemented(g); out.Status != domain.GateResultSkipped || !out.Passed() {
		t.Fatalf("нестрогий режим: получили %q, переход должен пропускаться", out.Status)
	}
	if out := testEngine(true).unimplemented(g); out.Status != domain.GateResultFailed || out.Passed() {
		t.Fatalf("строгий режим: получили %q, переход должен держаться", out.Status)
	}
}

func TestGateOutcomePassed(t *testing.T) {
	passing := []string{domain.GateResultPassed, domain.GateResultWaived, domain.GateResultSkipped}
	blocking := []string{domain.GateResultPending, domain.GateResultFailed}

	for _, s := range passing {
		if !(GateOutcome{Status: s}).Passed() {
			t.Fatalf("%q должен пропускать переход", s)
		}
	}
	for _, s := range blocking {
		if (GateOutcome{Status: s}).Passed() {
			t.Fatalf("%q должен держать переход", s)
		}
	}
}

// Откат — не провал этапа: ревьюер, нашедший проблему, свою работу сделал.
func TestFinishSemantics(t *testing.T) {
	cases := []struct{ kind, state, outcome, event string }{
		{"advance", domain.StagePassed, "advanced", domain.EventTaskAdvanced},
		{"reject", domain.StagePassed, "rejected", domain.EventTaskRejected},
		{"cancel", domain.StageAbandoned, "cancelled", domain.EventTaskCancelled},
		{"reassign", domain.StagePassed, "advanced", domain.EventTaskAdvanced},
	}
	for _, tc := range cases {
		state, outcome, event := finishSemantics(tc.kind)
		if state != tc.state || outcome != tc.outcome || event != tc.event {
			t.Fatalf("%s: получили (%s, %s, %s)", tc.kind, state, outcome, event)
		}
	}
}

func TestValidateDecision(t *testing.T) {
	for _, s := range []string{"passed", "failed", "waived"} {
		if err := ValidateDecision(s); err != nil {
			t.Fatalf("%q должен приниматься: %v", s, err)
		}
	}
	for _, s := range []string{"pending", "skipped", "approved", ""} {
		if err := ValidateDecision(s); err == nil {
			t.Fatalf("%q не должен приниматься", s)
		}
	}
}
