package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Необязательные этапы в связке (CLAB-314, решение 0066, п. 3).

// AC-12: цикл без необязательных этапов экспортируется байт в байт как
// прежде — ни одного нового ключа.
func TestExportWorkflowWithoutOptionalIsUnchanged(t *testing.T) {
	newID, doneID := uuid.New(), uuid.New()
	wf := domain.Workflow{Name: "Цикл", Statuses: []domain.Status{
		{ID: newID, Key: "new", Name: "New", Category: "todo", IsInitial: true, EnabledByDefault: true},
		{ID: doneID, Key: "done", Name: "Done", Category: "done", IsTerminal: true, EnabledByDefault: true},
	}, Transitions: []domain.Transition{
		{Key: "finish", Name: "Finish", FromStatusID: &newID, ToStatusID: doneID, Kind: "advance"},
	}}
	raw, err := json.Marshal(exportWorkflow(wf))
	if err != nil {
		t.Fatal(err)
	}
	// Та же форма, что отдавал сервер до CLAB-314.
	const golden = `{"name":"Цикл","statuses":[` +
		`{"key":"new","name":"New","category":"todo","is_initial":true},` +
		`{"key":"done","name":"Done","category":"done","is_terminal":true}],` +
		`"transitions":[{"key":"finish","name":"Finish","from":"new","to":"done","kind":"advance"}]}`
	if string(raw) != golden {
		t.Fatalf("экспорт изменился:\n%s\nожидали\n%s", raw, golden)
	}
}

// AC-12: is_optional — только у необязательного, enabled_by_default — только
// когда он false.
func TestExportWorkflowOptionalFields(t *testing.T) {
	wf := domain.Workflow{Name: "Цикл", Statuses: []domain.Status{
		{ID: uuid.New(), Key: "new", Category: "todo", IsInitial: true, EnabledByDefault: true},
		{ID: uuid.New(), Key: "design", Category: "active", IsOptional: true, EnabledByDefault: false},
		{ID: uuid.New(), Key: "docs", Category: "active", IsOptional: true, EnabledByDefault: true},
	}}
	raw, _ := json.Marshal(exportWorkflow(wf).Statuses)
	var got []map[string]any
	_ = json.Unmarshal(raw, &got)

	if _, ok := got[0]["is_optional"]; ok {
		t.Fatalf("у обязательного is_optional не пишется: %s", raw)
	}
	if _, ok := got[0]["enabled_by_default"]; ok {
		t.Fatalf("у обязательного enabled_by_default не пишется: %s", raw)
	}
	if got[1]["is_optional"] != true || got[1]["enabled_by_default"] != false {
		t.Fatalf("design — необязательный, выключен по умолчанию: %s", raw)
	}
	if _, ok := got[2]["enabled_by_default"]; ok || got[2]["is_optional"] != true {
		t.Fatalf("docs — необязательный, включён по умолчанию, без enabled_by_default: %s", raw)
	}
}

// AC-13, п. 4-5: нет enabled_by_default — «включён», а не нулевое bool.
func TestBundleStatusEnabledByDefault(t *testing.T) {
	var st BundleStatus
	if err := json.Unmarshal([]byte(`{"key":"design","is_optional":true}`), &st); err != nil {
		t.Fatal(err)
	}
	if !st.IsOptional || !st.enabledByDefault() {
		t.Fatalf("is_optional без enabled_by_default — включён: %+v", st)
	}
	var old BundleStatus
	_ = json.Unmarshal([]byte(`{"key":"design"}`), &old)
	if old.IsOptional || !old.enabledByDefault() {
		t.Fatalf("связка без полей — обязательный и включённый: %+v", old)
	}
}

// AC-13, п. 3: различие только в is_optional или только в
// enabled_by_default — другой цикл; отсутствие поля и true — одно и то же.
func TestSameWorkflowOptionalFields(t *testing.T) {
	base := func() BundleWorkflow {
		return BundleWorkflow{Name: "Цикл", Statuses: []BundleStatus{
			{Key: "new", Category: "todo", IsInitial: true},
			{Key: "design", Category: "active", IsOptional: true},
		}}
	}
	on, off := true, false

	same := base()
	same.Statuses[1].EnabledByDefault = &on
	if !sameWorkflow(base(), same) {
		t.Error("enabled_by_default: true и отсутствие поля — одно и то же")
	}

	diffOptional := base()
	diffOptional.Statuses[1].IsOptional = false
	if sameWorkflow(base(), diffOptional) {
		t.Error("различие только в is_optional — другой цикл")
	}

	diffDefault := base()
	diffDefault.Statuses[1].EnabledByDefault = &off
	if sameWorkflow(base(), diffDefault) {
		t.Error("различие только в enabled_by_default — другой цикл")
	}
}

// Круг «экспорт → разбор → сравнение» без потерь отметок.
func TestExportWorkflowOptionalRoundTrip(t *testing.T) {
	wf := domain.Workflow{Name: "Цикл", Statuses: []domain.Status{
		{ID: uuid.New(), Key: "design", Category: "active", IsOptional: true, EnabledByDefault: false},
	}}
	exported := exportWorkflow(wf)
	raw, _ := json.Marshal(exported)
	if !strings.Contains(string(raw), `"is_optional":true,"enabled_by_default":false`) {
		t.Fatalf("форма полей в связке: %s", raw)
	}
	var back BundleWorkflow
	_ = json.Unmarshal(raw, &back)
	if !sameWorkflow(*exported, back) {
		t.Fatalf("после разбора цикл тот же: %s", raw)
	}
}
