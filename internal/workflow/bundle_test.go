package workflow

import (
	"encoding/json"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

func TestParseRepository(t *testing.T) {
	ok := map[string][2]string{
		"acme/api":                               {"acme", "api"},
		"https://github.com/acme/api":            {"acme", "api"},
		"https://github.com/acme/api.git":        {"acme", "api"},
		"git@github.com:acme/api.git":            {"acme", "api"},
		"github.com/acme/api/":                   {"acme", "api"},
		"  stepaKaziatko/sdlc-pipeline-backend ": {"stepaKaziatko", "sdlc-pipeline-backend"},
	}
	for raw, want := range ok {
		owner, name, err := ParseRepository(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if owner != want[0] || name != want[1] {
			t.Errorf("%q: получили %s/%s", raw, owner, name)
		}
	}
	for _, raw := range []string{"", "api", "a/b/c", "/api", "acme/"} {
		if _, _, err := ParseRepository(raw); err == nil {
			t.Errorf("%q должен отбиваться", raw)
		}
	}
}

// Импорт роли с той же инструкцией не должен заводить версию: пробелы и
// порядок ключей в JSON — не изменение контракта.
func TestSameVersion(t *testing.T) {
	hint := "sonnet"
	have := domain.RoleVersion{
		Instructions:   "Ты ревьюер.",
		InputKinds:     []string{"dev.change_summary"},
		OutputContract: json.RawMessage(`[{"kind":"review.verdict","required":true}]`),
		ToolPolicy:     json.RawMessage(`{}`),
		ContextBudget:  json.RawMessage(`{"rag_top_k":12}`),
		ModelHint:      &hint,
		RepoScope:      []string{},
	}
	same := RoleVersionInput{
		Instructions:   "Ты ревьюер.",
		InputKinds:     []string{"dev.change_summary"},
		OutputContract: json.RawMessage(`[ {"required": true, "kind": "review.verdict"} ]`),
		ContextBudget:  json.RawMessage(`{"rag_top_k": 12}`),
		ModelHint:      &hint,
	}
	if !sameVersion(have, same) {
		t.Fatal("нормализованно равные версии сочтены разными")
	}

	changed := same
	changed.Instructions = "Ты ревьюер. Проверяй тесты."
	if sameVersion(have, changed) {
		t.Fatal("другая инструкция сочтена той же версией")
	}

	contract := same
	contract.OutputContract = json.RawMessage(`[{"kind":"review.verdict","required":false}]`)
	if sameVersion(have, contract) {
		t.Fatal("другой контракт сочтён той же версией")
	}

	noHint := same
	noHint.ModelHint = nil
	if sameVersion(have, noHint) {
		t.Fatal("снятая подсказка модели — изменение")
	}
}

func TestExportWorkflowKeepsAnyStatusTransition(t *testing.T) {
	wf := domain.Workflow{Name: "Цикл"}
	// Переход без from_status — «из любого статуса»; экспорт обязан
	// оставить from пустым, а не подставить первый попавшийся статус.
	wf.Statuses = []domain.Status{{Key: "new"}, {Key: "cancelled"}}
	wf.Transitions = []domain.Transition{{Key: "cancel", ToStatusID: wf.Statuses[1].ID, Kind: "cancel"}}
	out := exportWorkflow(wf)
	if out.Transitions[0].From != "" {
		t.Fatalf("from должен быть пустым, а не %q", out.Transitions[0].From)
	}
}

// Повторный импорт того же цикла — unchanged (CLAB-108): сравнение
// нормализовано по пробелам конфига, порядку типов задач и умолчанию
// blocking; содержательное отличие — новый черновик.
func TestSameWorkflow(t *testing.T) {
	tr := func() bool { return true }
	base := BundleWorkflow{
		Name: "Starter", TaskTypes: []string{"task", "bug"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "New", Category: "todo", IsInitial: true},
			{Key: "analysis", Name: "Analysis", Category: "in_progress", Role: "analyst", Reviewers: []string{"reviewer"}},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "Start", From: "new", To: "analysis", Kind: "advance"},
			{Key: "done", Name: "Done", From: "analysis", To: "new", Kind: "advance", Gates: []BundleGate{
				{Key: "ok", Name: "OK", Type: "human_approval", Config: json.RawMessage(`{"min_approvals": 1}`)},
			}},
		},
	}
	clone := func(mutate func(w *BundleWorkflow)) BundleWorkflow {
		raw, _ := json.Marshal(base)
		var w BundleWorkflow
		_ = json.Unmarshal(raw, &w)
		mutate(&w)
		return w
	}
	_ = tr

	same := []BundleWorkflow{
		clone(func(*BundleWorkflow) {}),
		clone(func(w *BundleWorkflow) { w.TaskTypes = []string{"bug", "task"} }),
		clone(func(w *BundleWorkflow) { w.Transitions[1].Gates[0].Config = json.RawMessage(`{ "min_approvals":1 }`) }),
		clone(func(w *BundleWorkflow) { b := true; w.Transitions[1].Gates[0].Blocking = &b }),
	}
	for i, w := range same {
		if !sameWorkflow(base, w) {
			t.Errorf("вариант %d обязан совпадать", i)
		}
	}
	different := []BundleWorkflow{
		clone(func(w *BundleWorkflow) { w.Name = "Starter v2" }),
		clone(func(w *BundleWorkflow) { w.TaskTypes = []string{"task"} }),
		clone(func(w *BundleWorkflow) { w.Statuses[1].Role = "developer" }),
		clone(func(w *BundleWorkflow) { w.Transitions[1].Gates[0].Config = json.RawMessage(`{"min_approvals":2}`) }),
		clone(func(w *BundleWorkflow) { b := false; w.Transitions[1].Gates[0].Blocking = &b }),
		clone(func(w *BundleWorkflow) { w.Transitions[1].Gates = nil }),
		clone(func(w *BundleWorkflow) { w.Statuses = w.Statuses[:1] }),
	}
	for i, w := range different {
		if sameWorkflow(base, w) {
			t.Errorf("вариант %d обязан отличаться", i)
		}
	}
}
