package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ссылка на основной трекер в CLI (CLAB-95): форма флага проверяется до
// запроса, тело несёт ровно переданное, вывод показывает ссылки и молча
// пропускает записи чужой формы.

const createdTask = `{"key":"CLAB-7","title":"T","status_key":"backlog","properties":{"type":"task"},` +
	`"external_ref":{"jira":{"key":"PROJ-12","url":"https://jira.example.com/browse/PROJ-12"}}}`

func TestCreateCarriesExternalRefs(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusCreated, createdTask)
	if _, _, err := capture(t, func() error {
		return cmdCreate(context.Background(), c, []string{"--project", "CLAB", "--title", "T",
			"--external-ref", "jira:PROJ-12=https://jira.example.com/browse/PROJ-12",
			"--external-ref", "youtrack:YT-7=https://yt.example.com/issue/YT-7"})
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"title": "T", "external_ref": map[string]any{
		"jira":     map[string]any{"key": "PROJ-12", "url": "https://jira.example.com/browse/PROJ-12"},
		"youtrack": map[string]any{"key": "YT-7", "url": "https://yt.example.com/issue/YT-7"},
	}}
	if rec.method != "POST" || !equalJSON(rec.body, want) {
		t.Fatalf("%s тело %v, ждали %v", rec.method, rec.body, want)
	}

	// Без флага поля в теле нет: ссылки ставит только тот, кто их передал.
	c, rec = newInviteClient(t, http.StatusCreated, createdTask)
	if _, _, err := capture(t, func() error {
		return cmdCreate(context.Background(), c, []string{"--project", "CLAB", "--title", "T"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.body["external_ref"]; ok {
		t.Fatalf("external_ref без флага: %v", rec.body)
	}
}

func TestEditExternalRefReplacesAndClears(t *testing.T) {
	withJSON(t, false)
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"--external-ref", "jira:PROJ-13=https://jira.example.com/browse/PROJ-13"},
			map[string]any{"external_ref": map[string]any{
				"jira": map[string]any{"key": "PROJ-13", "url": "https://jira.example.com/browse/PROJ-13"}}}},
		{[]string{"--external-ref", ""}, map[string]any{"external_ref": map[string]any{}}},
		{[]string{"--title", "X"}, map[string]any{"title": "X"}},
	}
	for _, tc := range cases {
		c, rec := newInviteClient(t, http.StatusOK, editedTask)
		if _, _, err := capture(t, func() error {
			return cmdEdit(context.Background(), c, append([]string{"CLAB-58"}, tc.args...))
		}); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if !equalJSON(rec.body, tc.want) {
			t.Fatalf("%v: тело %v, ждали %v", tc.args, rec.body, tc.want)
		}
	}
}

// Ошибка формы — до запроса, с примером, и команда завершается ошибкой.
func TestExternalRefFlagRefusesBadForm(t *testing.T) {
	withJSON(t, false)
	for _, value := range []string{
		"PROJ-12", "jira:PROJ-12", "jira:=https://jira.example.com/P-1",
		":PROJ-12=https://jira.example.com/P-1", "jira:PROJ-12=", "PROJ-12=https://jira.example.com/P-1",
	} {
		for _, run := range []func(*Client) error{
			func(c *Client) error {
				return cmdCreate(context.Background(), c, []string{"--project", "CLAB", "--title", "T", "--external-ref", value})
			},
			func(c *Client) error {
				return cmdEdit(context.Background(), c, []string{"CLAB-58", "--external-ref", value})
			},
		} {
			c, rec := newInviteClient(t, http.StatusOK, editedTask)
			_, stderr, err := capture(t, func() error { return run(c) })
			if err == nil || rec.calls != 0 {
				t.Fatalf("%q: ошибка %v, запросов %d", value, err, rec.calls)
			}
			if !strings.Contains(stderr, "система:КЛЮЧ=URL") || !strings.Contains(stderr, "jira:PROJ-12=https://") {
				t.Fatalf("%q: нет примера формы:\n%s", value, stderr)
			}
		}
	}

	// Одна система дважды и пустое значение вперемешку со ссылками — тоже до запроса.
	for _, args := range [][]string{
		{"--external-ref", "jira:A-1=https://a.example.com", "--external-ref", "jira:A-2=https://a.example.com"},
		{"--external-ref", "jira:A-1=https://a.example.com", "--external-ref", ""},
	} {
		c, rec := newInviteClient(t, http.StatusOK, editedTask)
		if _, _, err := capture(t, func() error {
			return cmdEdit(context.Background(), c, append([]string{"CLAB-58"}, args...))
		}); err == nil || rec.calls != 0 {
			t.Fatalf("%v: ошибка %v, запросов %d", args, err, rec.calls)
		}
	}
}

func showReply(ref string) string {
	return `{"stage_run_id":"c64e3b39-0000","kind":"stage","task":{"key":"CLAB-7","title":"T",` +
		`"status":"development","status_name":"Development","attempt":1,"priority":"normal","type":"task",` +
		`"external_ref":` + ref + `},"context":{"repositories":[]}}`
}

func TestShowPrintsTrackerLines(t *testing.T) {
	withJSON(t, false)
	ref := `{"youtrack":{"key":"YT-7","url":"https://yt.example.com/issue/YT-7"},` +
		`"jira":{"key":"PROJ-12","url":"https://jira.example.com/browse/PROJ-12"},"legacy_epic_key":"SDLC-E1"}`
	c, _ := newInviteClient(t, http.StatusOK, showReply(ref))
	stdout, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-7"}) })
	if err != nil {
		t.Fatal(err)
	}
	jira := strings.Index(stdout, "jira PROJ-12 https://jira.example.com/browse/PROJ-12")
	yt := strings.Index(stdout, "youtrack YT-7 https://yt.example.com/issue/YT-7")
	if jira < 0 || yt < 0 || jira > yt || strings.Count(stdout, "Трекер:") != 2 {
		t.Fatalf("строки трекера:\n%s", stdout)
	}
	if strings.Contains(stdout, "legacy") || strings.Contains(stdout, "SDLC-E1") {
		t.Fatalf("запись чужой формы напечатана:\n%s", stdout)
	}

	for _, empty := range []string{`{}`, `{"legacy_epic_key":"SDLC-E1"}`, `null`} {
		c, _ := newInviteClient(t, http.StatusOK, showReply(empty))
		stdout, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-7"}) })
		if err != nil {
			t.Fatalf("%s: %v", empty, err)
		}
		if strings.Contains(stdout, "Трекер:") || !strings.Contains(stdout, "CLAB-7") {
			t.Fatalf("%s: вывод\n%s", empty, stdout)
		}
	}
}

func TestTasksFilterByExternal(t *testing.T) {
	const reply = `{"tasks":[{"key":"CLAB-7","title":"T","status_key":"development","priority":"normal",` +
		`"properties":{"type":"task"},"external_ref":{"jira":{"key":"PROJ-12","url":"https://jira.example.com/browse/PROJ-12"}}}]}`
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	c := &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}

	withJSON(t, false)
	stdout, _, err := capture(t, func() error {
		return cmdTasks(context.Background(), c, []string{"--project", "CLAB", "--external", "jira:PROJ-12"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if query != "external=jira%3APROJ-12" || !strings.Contains(stdout, "CLAB-7") {
		t.Fatalf("запрос %q, вывод\n%s", query, stdout)
	}

	withJSON(t, true)
	stdout, _, err = capture(t, func() error {
		return cmdTasks(context.Background(), c, []string{"--project", "CLAB", "--external", "PROJ-12"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := decode([]byte(stdout), &got); err != nil {
		t.Fatalf("не JSON: %s", stdout)
	}
	_ = decode([]byte(reply), &want)
	if query != "external=PROJ-12" || !equalJSON(got, want) {
		t.Fatalf("--json: запрос %q, %v", query, got)
	}
}

func TestExternalFlagsInHelp(t *testing.T) {
	want := map[string]string{"create": "--external-ref", "edit": "--external-ref", "tasks": "--external"}
	for _, c := range commands() {
		if flag, ok := want[c.name]; ok {
			if !strings.Contains(c.usage, flag) {
				t.Fatalf("%s: нет %s в строке использования: %s", c.name, flag, c.usage)
			}
			delete(want, c.name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("команд нет в списке: %v", want)
	}
}
