package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// wf собирает цикл из коротких описаний: "ключ:флаги", где флаги — i (начальный),
// t (терминальный), a (категория active).
func wf(statuses []domain.Status, transitions ...domain.Transition) domain.Workflow {
	return domain.Workflow{Statuses: statuses, Transitions: transitions}
}

func status(key string, initial, terminal bool) domain.Status {
	return domain.Status{ID: uuid.New(), Key: key, Category: "active",
		IsInitial: initial, IsTerminal: terminal}
}

func transition(key string, from *domain.Status, to domain.Status, gates ...domain.Gate) domain.Transition {
	t := domain.Transition{ID: uuid.New(), Key: key, ToStatusID: to.ID, Kind: "advance", Gates: gates}
	if from != nil {
		t.FromStatusID = &from.ID
	}
	return t
}

func allImplemented(string) bool { return true }

func codes(problems []Problem) map[string]string {
	out := map[string]string{}
	for _, p := range problems {
		out[p.Code] = p.Status + p.Transition + p.Gate
	}
	return out
}

func TestValidateAcceptsLinearWorkflow(t *testing.T) {
	newSt := status("new", true, false)
	work := status("work", false, false)
	done := status("done", false, true)
	// на рабочих статусах назначен исполнитель — иначе поедет предупреждение
	newSt.Roles = []domain.StatusRole{{IsPrimary: true}}
	work.Roles = []domain.StatusRole{{IsPrimary: true}}
	done.Roles = []domain.StatusRole{{IsPrimary: true}}

	v := validateWorkflow(wf([]domain.Status{newSt, work, done},
		transition("start", &newSt, work),
		transition("finish", &work, done),
	), allImplemented, false)

	if !v.Valid {
		t.Fatalf("линейный цикл должен быть валиден, проблемы: %v", v.Problems)
	}
	if len(v.Warnings) != 0 {
		t.Fatalf("лишние предупреждения: %v", v.Warnings)
	}
}

func TestValidateCatchesGraphProblems(t *testing.T) {
	newSt := status("new", true, false)
	work := status("work", false, false)
	orphan := status("orphan", false, false)

	v := validateWorkflow(wf([]domain.Status{newSt, work, orphan},
		transition("start", &newSt, work),
	), allImplemented, false)

	if v.Valid {
		t.Fatal("цикл без терминального статуса, с тупиком и сиротой не может быть валиден")
	}
	got := codes(v.Problems)
	for _, want := range []string{"no_terminal_status", "dead_end", "unreachable"} {
		if _, ok := got[want]; !ok {
			t.Errorf("не найдена проблема %q; найдено: %v", want, got)
		}
	}
	if got["unreachable"] != "orphan" {
		t.Errorf("недостижим должен быть orphan, а не %q", got["unreachable"])
	}
}

// Переход «из любого статуса» (отмена) закрывает и тупик, и недостижимость:
// from_status_id = NULL — это ребро из каждого статуса сразу.
func TestValidateCountsAnywhereTransitions(t *testing.T) {
	newSt := status("new", true, false)
	cancelled := status("cancelled", false, true)
	newSt.Roles = []domain.StatusRole{{IsPrimary: true}}
	cancelled.Roles = []domain.StatusRole{{IsPrimary: true}}

	v := validateWorkflow(wf([]domain.Status{newSt, cancelled},
		transition("cancel", nil, cancelled),
	), allImplemented, false)

	if !v.Valid {
		t.Fatalf("переход из любого статуса должен закрывать выход из new: %v", v.Problems)
	}
}

func TestValidateRequiresInitialStatus(t *testing.T) {
	only := status("only", false, true)
	v := validateWorkflow(wf([]domain.Status{only}), allImplemented, false)

	if _, ok := codes(v.Problems)["no_initial_status"]; !ok {
		t.Fatalf("цикл без начального статуса должен ловиться: %v", v.Problems)
	}
}

func TestValidateEmptyWorkflow(t *testing.T) {
	v := validateWorkflow(domain.Workflow{}, allImplemented, false)
	if v.Valid || v.Problems[0].Code != "no_statuses" {
		t.Fatalf("пустой цикл: %v", v.Problems)
	}
}

// Нереализованный тип гейта — предупреждение, пока SDLC_STRICT_GATES выключен,
// и проблема, когда включён: там такой гейт заблокирует переход насовсем.
func TestValidateUnimplementedGateDependsOnStrict(t *testing.T) {
	newSt := status("new", true, false)
	done := status("done", false, true)
	newSt.Roles = []domain.StatusRole{{IsPrimary: true}}
	done.Roles = []domain.StatusRole{{IsPrimary: true}}

	// Тип взят несуществующий: все девять типов из схемы движок исполняет,
	// а проверка тут про поведение, а не про конкретный тип.
	gate := domain.Gate{Key: "ci", Type: "future_gate_type", Blocking: true}
	only := func(string) bool { return false }
	cycle := wf([]domain.Status{newSt, done}, transition("finish", &newSt, done, gate))

	lenient := validateWorkflow(cycle, only, false)
	if !lenient.Valid || len(lenient.Warnings) != 1 {
		t.Fatalf("без strict это предупреждение: valid=%v warnings=%v", lenient.Valid, lenient.Warnings)
	}

	strict := validateWorkflow(cycle, only, true)
	if strict.Valid {
		t.Fatal("при strict нереализованный блокирующий гейт обязан держать активацию")
	}
	if _, ok := codes(strict.Problems)["gate_not_implemented"]; !ok {
		t.Fatalf("не тот код проблемы: %v", strict.Problems)
	}
}

// Неблокирующий гейт не мешает даже в strict: он ничего не держит.
func TestValidateNonBlockingGateStaysWarning(t *testing.T) {
	newSt := status("new", true, false)
	done := status("done", false, true)
	newSt.Roles = []domain.StatusRole{{IsPrimary: true}}
	done.Roles = []domain.StatusRole{{IsPrimary: true}}

	gate := domain.Gate{Key: "hint", Type: "future_gate_type", Blocking: false}
	v := validateWorkflow(wf([]domain.Status{newSt, done},
		transition("finish", &newSt, done, gate)), func(string) bool { return false }, true)

	if !v.Valid || len(v.Warnings) != 1 {
		t.Fatalf("неблокирующий гейт активацию не держит: %v / %v", v.Problems, v.Warnings)
	}
}

func TestValidateWarnsAboutStatusWithoutExecutor(t *testing.T) {
	newSt := status("new", true, false)
	done := status("done", false, true)
	newSt.Roles = []domain.StatusRole{{IsPrimary: true}}
	// у done исполнителя нет

	v := validateWorkflow(wf([]domain.Status{newSt, done},
		transition("finish", &newSt, done)), allImplemented, false)

	if !v.Valid {
		t.Fatalf("отсутствие исполнителя активацию не держит: %v", v.Problems)
	}
	if len(v.Warnings) != 1 || v.Warnings[0].Code != "no_primary_role" {
		t.Fatalf("ожидалось предупреждение про исполнителя: %v", v.Warnings)
	}
}

// Задача, оставшаяся на статусе, которого в новой версии цикла нет, — повод
// предупредить человека: доработает она по старой версии, но колонки для неё
// на доске уже не будет.
func TestCarriedOverWarnings(t *testing.T) {
	next := domain.Workflow{Statuses: []domain.Status{{Key: "analysis"}}}

	if w := carriedOverWarnings([]postgres.OpenTaskOnWorkflow{
		{Key: "SDLC-1", StatusKey: "analysis"},
	}, next); len(w) != 0 {
		t.Fatalf("статус сохранился, предупреждать не о чем: %v", w)
	}

	w := carriedOverWarnings([]postgres.OpenTaskOnWorkflow{
		{Key: "SDLC-2", StatusKey: "review"},
		{Key: "SDLC-3", StatusKey: "review"},
	}, next)
	if len(w) != 1 || w[0].Code != "status_dropped" || w[0].Status != "review" {
		t.Fatalf("ожидалось одно предупреждение про review: %v", w)
	}
	if !strings.Contains(w[0].Message, "SDLC-2, SDLC-3") {
		t.Fatalf("в предупреждении должны быть перечислены задачи: %q", w[0].Message)
	}
}

func TestValidTransitionKind(t *testing.T) {
	for _, kind := range []string{"advance", "reject", "reassign", "cancel"} {
		if !validTransitionKind(kind) {
			t.Errorf("%q — законный вид перехода", kind)
		}
	}
	if validTransitionKind("approve") {
		t.Error("неизвестный вид перехода не должен проходить")
	}
}

// Перепроверку не может делать тот, кто делал работу: иначе гейт agent_review
// превращается в «агент проверяет сам себя». Поймать это можно только на цикле
// целиком — гейт знает ключ роли, но не знает исполнителя статуса.
func TestValidateReviewerRole(t *testing.T) {
	analysis := status("analysis", true, false)
	arch := status("architecture", false, true)
	analysis.Roles = []domain.StatusRole{{IsPrimary: true, RoleKey: "analyst"}}
	arch.Roles = []domain.StatusRole{{IsPrimary: true, RoleKey: "architect"}}

	review := func(config string) domain.Workflow {
		g := domain.Gate{Key: "peer_review", Type: domain.GateAgentReview,
			Blocking: true, Config: json.RawMessage(config)}
		return wf([]domain.Status{analysis, arch}, transition("done", &analysis, arch, g))
	}

	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"чужая роль проверяет", `{"reviewer_role_key":"reviewer"}`, ""},
		{"роль не указана", `{}`, "review_role_missing"},
		{"исполнитель проверяет сам себя", `{"reviewer_role_key":"analyst"}`, "review_role_is_executor"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validateWorkflow(review(tc.config), allImplemented, false)
			if tc.want == "" {
				if !v.Valid {
					t.Fatalf("цикл собран верно: %v", v.Problems)
				}
				return
			}
			if _, ok := codes(v.Problems)[tc.want]; !ok {
				t.Fatalf("не найдена проблема %q: %v", tc.want, v.Problems)
			}
		})
	}
}

// CLAB-217, критерии 1–3: предел попыток — целое ≥ 1 в config, и переход
// ведёт не в терминальный статус. Негодный предел — проблема, а не
// предупреждение: активацию он держит.
func TestValidateAttemptsLimit(t *testing.T) {
	dev := status("development", true, false)
	review := status("review", false, false)
	done := status("done", false, true)

	cycle := func(config string, onTerminal bool) domain.Workflow {
		g := domain.Gate{Key: "attempts", Type: domain.GateAttemptsLimit,
			Blocking: true, Config: json.RawMessage(config)}
		back := transition("reject_to_development", &review, dev, g)
		back.Kind = "reject"
		finish := transition("finish", &review, done)
		if onTerminal {
			finish.Gates = []domain.Gate{g}
		}
		return wf([]domain.Status{dev, review, done},
			transition("dev_done", &dev, review), back, finish)
	}

	cases := []struct {
		name, config string
		terminal     bool
		want         string
	}{
		{"предел 2", `{"max_attempts": 2}`, false, ""},
		{"предел 1", `{"max_attempts":1}`, false, ""},
		{"пустой config", `{}`, false, "attempts_limit_invalid"},
		{"config отсутствует", ``, false, "attempts_limit_invalid"},
		{"ноль", `{"max_attempts": 0}`, false, "attempts_limit_invalid"},
		{"отрицательный", `{"max_attempts": -1}`, false, "attempts_limit_invalid"},
		{"дробный", `{"max_attempts": 2.5}`, false, "attempts_limit_invalid"},
		{"строкой", `{"max_attempts": "3"}`, false, "attempts_limit_invalid"},
		{"null", `{"max_attempts": null}`, false, "attempts_limit_invalid"},
		{"на терминальный статус", `{"max_attempts": 2}`, true, "attempts_limit_terminal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validateWorkflow(cycle(tc.config, tc.terminal), allImplemented, true)
			if tc.want == "" {
				if !v.Valid {
					t.Fatalf("цикл с пределом собран верно: %v", v.Problems)
				}
				return
			}
			if v.Valid {
				t.Fatalf("цикл с негодным пределом прошёл проверку")
			}
			var found *Problem
			for i, p := range v.Problems {
				if p.Code == tc.want {
					found = &v.Problems[i]
				}
			}
			if found == nil {
				t.Fatalf("нет проблемы %q: %v", tc.want, v.Problems)
			}
			if found.Gate != "attempts" || found.Transition == "" {
				t.Fatalf("проблема без ключей перехода и гейта: %+v", found)
			}
			if tc.want == "attempts_limit_invalid" && !strings.Contains(found.Message, "max_attempts") {
				t.Fatalf("причина не называет max_attempts: %q", found.Message)
			}
			for _, w := range v.Warnings {
				if w.Code == tc.want {
					t.Fatalf("негодный предел попал в предупреждения: %+v", w)
				}
			}
		})
	}
}
