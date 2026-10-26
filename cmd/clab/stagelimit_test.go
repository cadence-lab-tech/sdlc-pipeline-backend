package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// CLAB-221, критерий 24: clab show и clab gates печатают строку предела —
// сколько осталось, «просрочен на …» с командой снятия или «снят <кто>»; без
// блока (нет предела или старый сервер) строки нет, а --json gates отдаёт
// переходы в прежней форме.
func TestStageLimitLine(t *testing.T) {
	deadline := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		view stageLimitView
		want []string
	}{
		{"осталось", stageLimitView{Gate: "time", Transition: "qa_failed", MaxDuration: "4h",
			Deadline: deadline, RemainingSeconds: 3*3600 + 12*60 + 7},
			[]string{"4h с входа переходом qa_failed", "осталось 3h12m"}},
		{"просрочен", stageLimitView{Gate: "time", Transition: "qa_failed", MaxDuration: "4h",
			Deadline: deadline, ElapsedSeconds: 5*3600 + 5*60, Overdue: true},
			[]string{"просрочен на 1h5m", "clab decide CLAB-1 --transition qa_failed --gate time --status waived"}},
		{"снят", stageLimitView{Gate: "time", Transition: "qa_failed", MaxDuration: "4h",
			Overdue: true, Waived: true, WaivedBy: ptrTo("lead")},
			[]string{"снят lead"}},
	}
	for _, tc := range cases {
		line := tc.view.line("CLAB-1")
		for _, w := range tc.want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: %q без %q", tc.name, line, w)
			}
		}
	}
	if (*stageLimitView)(nil).line("CLAB-1") != "" {
		t.Error("у пустого предела есть строка")
	}
}

func ptrTo(s string) *string { return &s }

func TestShowAndGatesPrintStageLimit(t *testing.T) {
	limit := `{"gate":"time","transition":"qa_failed","status":"development","max_duration":"4h",` +
		`"deadline":"2026-10-26T14:00:00Z","elapsed_seconds":18300,"remaining_seconds":0,"overdue":true,"waived":false}`
	state := `{"stage_run_id":"r1","kind":"stage","task":{"key":"CLAB-1","title":"Задача","status":"development"},` +
		`"context":{},"stage_limit":` + limit + `}`
	c, _ := newInviteClient(t, http.StatusOK, state)
	out, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-1"}) })
	if err != nil || !strings.Contains(out, "Предел этапа:") || !strings.Contains(out, "просрочен на 1h5m") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	// Старый сервер блока не отдаёт — строки нет.
	c, _ = newInviteClient(t, http.StatusOK, `{"stage_run_id":"r1","task":{"key":"CLAB-1"},"context":{}}`)
	out, _, _ = capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-1"}) })
	if strings.Contains(out, "Предел этапа") {
		t.Fatalf("show без блока напечатал предел:\n%s", out)
	}

	gates := `{"transitions":[{"key":"dev_done","name":"Dev","to_status":"testing","allowed":true,"gates":[]}],"stage_limit":` + limit + `}`
	c, _ = newInviteClient(t, http.StatusOK, gates)
	out, _, err = capture(t, func() error { return cmdGates(context.Background(), c, []string{"CLAB-1"}) })
	if err != nil || !strings.Contains(out, "Предел этапа: 4h с входа переходом qa_failed") || !strings.Contains(out, "dev_done") {
		t.Fatalf("gates: %v\n%s", err, out)
	}
	withJSON(t, true)
	out, _, err = capture(t, func() error { return cmdGates(context.Background(), c, []string{"CLAB-1"}) })
	var arr []map[string]any
	if err != nil || json.Unmarshal([]byte(out), &arr) != nil || len(arr) != 1 {
		t.Fatalf("gates --json сменил форму: %v\n%s", err, out)
	}
}
