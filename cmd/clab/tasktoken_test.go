package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// CLAB-212: clab token task выпускает токен задачи на живую аренду и
// печатает его одной строкой; clab whoami показывает привязку токена.

const taskTokenReply = `{"token":"sdlc_tasktokensecret","record":{"id":"7d0a6c0e-0000-4000-8000-000000000001","token_kind":"task","task_key":"CLAB-12","stage_run_id":"5ca900b3-57f1-4df7-bd43-e69b594d53d4","role_keys":["developer"],"name":"task:CLAB-12/development#1"},"task_key":"CLAB-12","stage_run_id":"5ca900b3-57f1-4df7-bd43-e69b594d53d4","role":"developer","project_key":"CLAB","lease_expires_at":"2026-10-21T16:00:00Z"}`

func TestTokenTaskPrintsTokenOnly(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusCreated, taskTokenReply)
	out, _, err := capture(t, func() error {
		return cmdToken(context.Background(), c, []string{"task", "CLAB-12"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/tasks/CLAB-12/task-token" {
		t.Fatalf("запрос %s %s", rec.method, rec.path)
	}
	if out != "sdlc_tasktokensecret\n" {
		t.Fatalf("вывод не одна строка с токеном: %q", out)
	}

	withJSON(t, true)
	c, _ = newInviteClient(t, http.StatusCreated, taskTokenReply)
	out, _, err = capture(t, func() error {
		return cmdToken(context.Background(), c, []string{"task", "CLAB-12"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--json не JSON: %q", out)
	}
	record, _ := v["record"].(map[string]any)
	if v["token"] != "sdlc_tasktokensecret" || record["token_kind"] != "task" || v["role"] != "developer" {
		t.Fatalf("--json: %s", out)
	}
}

// Отказ сервера (токеном задачи токен не выпускается) доходит кодом forbidden.
func TestTokenTaskForbidden(t *testing.T) {
	withJSON(t, false)
	c, _ := newInviteClient(t, http.StatusForbidden,
		`{"error":{"code":"forbidden","message":"токеном задачи токены не выпускаются"}}`)
	_, _, err := capture(t, func() error {
		return cmdToken(context.Background(), c, []string{"task", "CLAB-12"})
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "forbidden" {
		t.Fatalf("отказ: %v", err)
	}
	if _, _, err := capture(t, func() error {
		return cmdToken(context.Background(), c, []string{"task"})
	}); err == nil || !strings.Contains(err.Error(), "clab token task") {
		t.Fatalf("без ключа: %v", err)
	}
}

func TestWhoamiShowsTaskToken(t *testing.T) {
	outsideRepo(t)
	me := `{"principal":{"kind":"agent","handle":"pool"},"authenticated":true,"memberships":[],` +
		`"token":{"token_kind":"task","task_key":"CLAB-12","role_keys":["developer"]}}`
	routes := map[string]string{"/v1/auth/me": me, "/v1/instance": instanceReply}

	withJSON(t, false)
	c, _ := whoamiServer(t, routes)
	out, _, err := capture(t, func() error { return cmdWhoami(context.Background(), c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "задачи CLAB-12, роль developer") {
		t.Fatalf("привязки токена нет в выводе:\n%s", out)
	}

	withJSON(t, true)
	c, _ = whoamiServer(t, routes)
	out, _, err = capture(t, func() error { return cmdWhoami(context.Background(), c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if tt, _ := v["task_token"].(map[string]any); tt["task_key"] != "CLAB-12" {
		t.Fatalf("--json без task_token: %s", out)
	}

	// Личный токен — без строки «Токен:».
	withJSON(t, false)
	c, _ = whoamiServer(t, map[string]string{"/v1/auth/me": meReply, "/v1/instance": instanceReply})
	out, _, _ = capture(t, func() error { return cmdWhoami(context.Background(), c, nil) })
	if strings.Contains(out, "Токен:") {
		t.Fatalf("личный токен показан как токен задачи:\n%s", out)
	}
}
