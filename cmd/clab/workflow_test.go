package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Команды чтения показывают таблицу, а не JSON ответа (решение 0032,
// CLAB-105): версии цикла — таблицей с полным идентификатором, чтобы его
// можно было скопировать в activate.
func TestWorkflowListTable(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, 200, `{"workflows":[
	  {"id":"14a2d654-3469-4981-a62b-2c4d8559746f","name":"Starter","version":1,"state":"active","task_types":["task"]},
	  {"id":"590ae03b-31cf-4b79-ab7d-19bbe3b145c3","name":"Starter epics","version":2,"state":"draft","task_types":["epic"]}]}`)
	stdout, _, err := capture(t, func() error {
		return cmdWorkflow(context.Background(), c, []string{"list", "--project", "PLAT"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/v1/projects/PLAT/workflows" {
		t.Fatalf("запрос: %s", rec.path)
	}
	for _, want := range []string{"14a2d654-3469-4981-a62b-2c4d8559746f", "Starter epics", "draft", "epic", "ВЕРСИЯ"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("в таблице нет %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, `"workflows"`) {
		t.Fatalf("без --json напечатан JSON:\n%s", stdout)
	}
}

func TestWorkflowActivateSummary(t *testing.T) {
	withJSON(t, false)
	c, _ := newInviteClient(t, 200, `{"workflow":{"id":"x","name":"Starter","version":2,"state":"active","task_types":["task"]},
	  "retired_workflow_ids":["y"],"tasks_on_previous_version_total":3,"tasks_moved_total":1,"warnings":[{"message":"гейт drift_clear не реализован"}]}`)
	stdout, _, err := capture(t, func() error {
		return cmdWorkflow(context.Background(), c, []string{"activate", "x"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"активирован: Starter, версия 2", "прежней версии: 3", "переехало на новую: 1", "! гейт drift_clear"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("в сводке нет %q:\n%s", want, stdout)
		}
	}
}

// Заблокированный переход печатает гейты тем же списком, что clab gates, и
// завершается ошибкой без JSON деталей; с --json детали остаются.
func TestTransitionBlockedPrintsGates(t *testing.T) {
	reply := `{"error":{"code":"gates_not_satisfied","message":"переход \"analysis_done\" заблокирован гейтами",
	  "details":{"gates":[{"gate_key":"human_approval","type":"human_approval","blocking":true,"status":"pending","reason":"апрувов 0 из 1"}]}}}`

	withJSON(t, false)
	c, _ := newInviteClient(t, 409, reply)
	stdout, _, err := capture(t, func() error {
		return cmdTransition(context.Background(), c, []string{"SDLC-1", "--to", "analysis_done"})
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "gates_not_satisfied" {
		t.Fatalf("ошибка: %v", err)
	}
	if len(apiErr.Details) != 0 {
		t.Fatalf("детали остались в ошибке и уйдут в stderr JSON'ом: %s", apiErr.Details)
	}
	if !strings.Contains(stdout, "[pending] human_approval") || !strings.Contains(stdout, "апрувов 0 из 1") {
		t.Fatalf("гейты не напечатаны:\n%s", stdout)
	}

	withJSON(t, true)
	c, _ = newInviteClient(t, 409, reply)
	stdout, _, err = capture(t, func() error {
		return cmdTransition(context.Background(), c, []string{"SDLC-1", "--to", "analysis_done"})
	})
	if !errors.As(err, &apiErr) || len(apiErr.Details) == 0 || stdout != "" {
		t.Fatalf("с --json детали обязаны остаться: %v, stdout %q", err, stdout)
	}
}

func TestDecidePrintsGate(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, 200, `{"gate_key":"human_approval","gate_name":"Апрув","type":"human_approval","blocking":true,"status":"passed"}`)
	stdout, _, err := capture(t, func() error {
		return cmdDecide(context.Background(), c, []string{"SDLC-1", "--transition", "analysis_done", "--gate", "human_approval"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/v1/tasks/SDLC-1/transitions/analysis_done/gates/human_approval/decision" {
		t.Fatalf("запрос: %s", rec.path)
	}
	if !strings.Contains(stdout, "[passed] human_approval") {
		t.Fatalf("решение не напечатано:\n%s", stdout)
	}
}

func TestTeamCreatePrintsSummary(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, 201, `{"key":"acme","name":"Acme","kind":"organization","can_create_projects":true}`)
	stdout, _, err := capture(t, func() error {
		return cmdTeam(context.Background(), c, []string{"create", "acme", "--name", "Acme"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != "POST" || rec.path != "/v1/teams" {
		t.Fatalf("запрос: %s %s", rec.method, rec.path)
	}
	if !strings.Contains(stdout, "команда acme «Acme» создана") || !strings.Contains(stdout, "да") {
		t.Fatalf("сводка:\n%s", stdout)
	}
}
