package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CLAB-382, решение 0072 п. 9: clab печатает право актора, которое отдал
// сервер, и не считает своего.

// rightsTransitions — ответ /transitions участника agent на development:
// переход вперёд ему доступен, cancel — нет; гейт-чеклист и апрув он решает,
// предел попыток — нет.
const rightsTransitions = `{"transitions":[
 {"key":"dev_done","name":"Dev done","to_status":"testing","allowed":false,"actor_may":true,
  "gates":[
   {"gate_key":"artifacts","type":"required_artifacts","blocking":true,"status":"passed"},
   {"gate_key":"selfhosted","type":"checklist","blocking":true,"status":"pending",
    "actor_may_decide":true,"actor_decisions":["passed"]},
   {"gate_key":"approval","type":"human_approval","blocking":true,"status":"pending",
    "actor_may_decide":true,"actor_decisions":["passed","failed"]},
   {"gate_key":"limit","type":"attempts_limit","blocking":true,"status":"pending",
    "actor_may_decide":false,"actor_deny":{"code":"forbidden","reason":"maintainer_action","message":"решение по гейту limit — решение сопровождающего"}}
  ]},
 {"key":"cancel","name":"Cancel","to_status":"cancelled","allowed":true,"actor_may":false,
  "actor_deny":{"code":"forbidden","reason":"maintainer_action","message":"переход cancel — решение сопровождающего"},
  "gates":[]}
]}`

func rightsServer(t *testing.T, state, transitions string, calls *[]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls = append(*calls, r.Method+" "+r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions"):
			_, _ = w.Write([]byte(transitions))
		case strings.HasSuffix(r.URL.Path, "/state"):
			_, _ = w.Write([]byte(state))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}
}

func TestGatesPrintActorRights(t *testing.T) {
	withJSON(t, false)
	c := rightsServer(t, "", rightsTransitions, nil)
	out, _, err := capture(t, func() error { return cmdGates(context.Background(), c, []string{"CLAB-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"право: вправе",
		"решить: passed\n",
		"решить: passed, failed",
		"решить: нельзя — решение по гейту limit — решение сопровождающего",
		"право: не вправе: переход cancel — решение сопровождающего",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе gates нет %q:\n%s", want, out)
		}
	}
	// У гейта без полей права (artifacts) строки «решить» нет.
	if strings.Count(out, "решить:") != 3 {
		t.Errorf("строк «решить» не три:\n%s", out)
	}

	// --transition: право только этого перехода.
	out, _, err = capture(t, func() error {
		return cmdGates(context.Background(), c, []string{"CLAB-1", "--transition", "cancel"})
	})
	if err != nil || strings.Contains(out, "dev_done") || !strings.Contains(out, "не вправе") {
		t.Fatalf("gates --transition cancel: %v\n%s", err, out)
	}
	_, _, err = capture(t, func() error {
		return cmdGates(context.Background(), c, []string{"CLAB-1", "--transition", "nope"})
	})
	if err == nil || !strings.Contains(err.Error(), "нет из текущего статуса") {
		t.Fatalf("переход, которого нет: %v", err)
	}
}

func TestGatesJSONKeepsServerAnswer(t *testing.T) {
	withJSON(t, true)
	c := rightsServer(t, "", rightsTransitions, nil)
	out, _, err := capture(t, func() error { return cmdGates(context.Background(), c, []string{"CLAB-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil || len(arr) != 2 {
		t.Fatalf("форма --json: %v\n%s", err, out)
	}
	if _, ok := arr[0]["actor_may"]; !ok {
		t.Errorf("в --json нет actor_may: %v", arr[0])
	}
	gates := arr[0]["gates"].([]any)
	if _, ok := gates[2].(map[string]any)["actor_decisions"]; !ok {
		t.Errorf("в --json нет actor_decisions: %v", gates[2])
	}
	if strings.Contains(out, "право:") {
		t.Errorf("--json напечатал строки права:\n%s", out)
	}
}

// Сервер прежней версии полей права не отдаёт: строк нет, ошибки нет.
func TestGatesOldServerNoRights(t *testing.T) {
	withJSON(t, false)
	old := `{"transitions":[{"key":"dev_done","name":"Dev","to_status":"testing","allowed":true,` +
		`"gates":[{"gate_key":"artifacts","type":"required_artifacts","blocking":true,"status":"passed"}]}]}`
	c := rightsServer(t, `{"stage_run_id":"r1","task":{"key":"CLAB-1"},"context":{},"gates_ahead":[{"transition":"dev_done","gates":[{"key":"artifacts","blocking":true}]}]}`, old, nil)
	out, _, err := capture(t, func() error { return cmdGates(context.Background(), c, []string{"CLAB-1"}) })
	if err != nil || strings.Contains(out, "право") || strings.Contains(out, "решить") || !strings.Contains(out, "dev_done") {
		t.Fatalf("gates со старым сервером: %v\n%s", err, out)
	}
	out, _, err = capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-1"}) })
	if err != nil || strings.Contains(out, "право") || !strings.Contains(out, "dev_done → ") {
		t.Fatalf("show со старым сервером: %v\n%s", err, out)
	}
}

func TestShowPrintsActorRightsAndClaimDoesNot(t *testing.T) {
	withJSON(t, false)
	state := claimReply(t, func(m map[string]any) {
		m["gates_ahead"] = []map[string]any{
			{"transition": "dev_done", "gates": []map[string]any{{"key": "approval", "blocking": true}}},
			{"transition": "cancel", "gates": []map[string]any{}},
		}
	})
	var calls []string
	c := rightsServer(t, state, rightsTransitions, &calls)
	out, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  dev_done → ", "      право: вправе", "      гейт approval — решить: passed, failed",
		"  cancel → ", "      право: не вправе: переход cancel — решение сопровождающего",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в show нет %q:\n%s", want, out)
		}
	}
	if len(calls) != 2 {
		t.Errorf("show сделал запросы %v", calls)
	}

	// Сводка claim — прежняя: строк права нет.
	out, _, err = capture(t, func() error { return renderClaim(json.RawMessage(state), "", "") })
	if err != nil || strings.Contains(out, "право:") || strings.Contains(out, "решить:") {
		t.Fatalf("claim напечатал право: %v\n%s", err, out)
	}

	// show --json — один запрос, ответ как есть.
	withJSON(t, true)
	calls = nil
	_, _, err = capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
	if err != nil || len(calls) != 1 {
		t.Fatalf("show --json: %v, запросы %v", err, calls)
	}
}

// Нет ответа /transitions — show всё равно печатает сводку.
func TestShowSurvivesRightsFailure(t *testing.T) {
	withJSON(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transitions") {
			http.Error(w, `{"error":{"code":"forbidden","message":"нет"}}`, http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(claimReply(t, nil)))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}
	out, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
	if err != nil || !strings.Contains(out, "CLAB-146") {
		t.Fatalf("show: %v\n%s", err, out)
	}
}

func forbiddenBody(reason, tokenKind string) string {
	d := `{"stage_role":"developer"`
	if reason != "" {
		d += `,"reason":"` + reason + `"`
	}
	if tokenKind != "" {
		d += `,"token_kind":"` + tokenKind + `"`
	}
	return `{"error":{"code":"forbidden","message":"отказ сервера","details":` + d + `}}}`
}

// Тексты отказов transition и decide: текст сервера плюс строка «что
// делать» по коду причины; не по коду — строки нет.
func TestDenialHints(t *testing.T) {
	withJSON(t, false)
	cases := []struct {
		name, body, want string
	}{
		{"stage_role", forbiddenBody("stage_role", ""), "роли, которой у вас нет"},
		{"no_stage_role", forbiddenBody("no_stage_role", ""), "нет исполнителя, переход глобальный"},
		{"maintainer_action", forbiddenBody("maintainer_action", ""), "за сопровождающим проекта"},
		{"level", forbiddenBody("level", ""), "уровня участия в проекте не хватает"},
		{"token", forbiddenBody("", "task"), "решение человека"},
	}
	for _, tc := range cases {
		c, _ := newInviteClient(t, http.StatusForbidden, tc.body)
		run := map[string]func() error{
			"transition": func() error {
				return cmdTransition(context.Background(), c, []string{"CLAB-1", "--to", "dev_done"})
			},
			"decide": func() error {
				return cmdDecide(context.Background(), c, []string{"CLAB-1", "--transition", "dev_done", "--gate", "g", "--status", "waived"})
			},
		}
		for cmd, fn := range run {
			_, _, err := capture(t, fn)
			if err == nil || !strings.Contains(err.Error(), "отказ сервера") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s %s: %v", cmd, tc.name, err)
			}
			var api *APIError
			if !errors.As(err, &api) || api.Code != "forbidden" || api.Status != http.StatusForbidden {
				t.Errorf("%s %s: код ошибки потерян: %v", cmd, tc.name, err)
			}
		}
	}

	// Без кода причины и без токена задачи — только текст сервера.
	c, _ := newInviteClient(t, http.StatusForbidden, `{"error":{"code":"forbidden","message":"отказ сервера"}}`)
	_, _, err := capture(t, func() error {
		return cmdTransition(context.Background(), c, []string{"CLAB-1", "--to", "dev_done"})
	})
	if err == nil || strings.Contains(err.Error(), "\n") {
		t.Errorf("подсказка без кода причины: %v", err)
	}

	// --json: ответ сервера как есть.
	withJSON(t, true)
	c, _ = newInviteClient(t, http.StatusForbidden, forbiddenBody("level", ""))
	_, _, err = capture(t, func() error {
		return cmdTransition(context.Background(), c, []string{"CLAB-1", "--to", "dev_done"})
	})
	if err == nil || strings.Contains(err.Error(), "уровня участия") {
		t.Errorf("--json получил подсказку: %v", err)
	}
}

// Успешный ответ не меняется: подсказка только к 403.
func TestDecideSuccessUnchanged(t *testing.T) {
	withJSON(t, false)
	c, _ := newInviteClient(t, http.StatusOK,
		`{"gate_key":"selfhosted","type":"checklist","blocking":true,"status":"passed"}`)
	out, _, err := capture(t, func() error {
		return cmdDecide(context.Background(), c, []string{"CLAB-1", "--transition", "dev_done", "--gate", "selfhosted"})
	})
	want := "решение записано:\n    [passed] selfhosted (checklist)\n"
	if err != nil || out != want {
		t.Fatalf("decide: %v\n%q", err, out)
	}
}
