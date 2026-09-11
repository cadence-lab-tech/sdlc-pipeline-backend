package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// inviteServer — сервер, который запоминает последний запрос и отвечает
// заданным статусом и телом.
type inviteServer struct {
	calls  int
	method string
	path   string
	body   map[string]any
}

func newInviteClient(t *testing.T, status int, reply string) (*Client, *inviteServer) {
	t.Helper()
	rec := &inviteServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.calls++
		rec.method, rec.path, rec.body = r.Method, r.URL.EscapedPath(), nil
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.body); err != nil {
				t.Errorf("тело не JSON: %s", raw)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}, rec
}

// capture перехватывает stdout и stderr на время fn.
func capture(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	err = fn()
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	return string(o), string(e), err
}

func withJSON(t *testing.T, on bool) {
	t.Helper()
	prev := jsonOut
	jsonOut = on
	t.Cleanup(func() { jsonOut = prev })
}

const issuedTeam = `{"invite":{"id":"4f1c0c1e-0000-4000-8000-000000000001","target_kind":"team","team_key":"acme","target_name":"Acme","team_role":"member","role_keys":null,"status":"pending","created_by_handle":"alice","created_at":"2026-09-12T10:00:00Z","expires_at":"2026-09-19T10:00:00Z"},"token":"clab_inv_secret"}`

const issuedProject = `{"invite":{"id":"4f1c0c1e-0000-4000-8000-000000000002","target_kind":"project","project_key":"CLAB","target_name":"Cadence Lab","level":"agent","role_keys":["developer"],"status":"pending","created_by_handle":"alice","created_at":"2026-09-12T10:00:00Z","expires_at":"2026-09-19T10:00:00Z"},"token":"clab_inv_secret"}`

func TestInviteLink(t *testing.T) {
	cases := map[[2]string]string{
		{"https://sdlc.food-cart.ru", "clab_inv_x"}: "https://sdlc.food-cart.ru/invite/clab_inv_x",
		{"https://example.com/clab", "clab_inv_x"}:  "https://example.com/clab/invite/clab_inv_x",
		{"http://localhost:5173/", "clab_inv_x"}:    "http://localhost:5173/invite/clab_inv_x",
	}
	for in, want := range cases {
		if got := inviteLink(in[0], in[1]); got != want {
			t.Errorf("%v: %q вместо %q", in, got, want)
		}
	}
}

// Подписи статусов — те же слова, что в интерфейсе; незнакомый статус от
// сервера новее CLI показывается как есть, а не пустой строкой.
func TestInviteStatusLabels(t *testing.T) {
	cases := map[string]string{
		"pending": "выдан", "accepted": "принят", "expired": "протух", "revoked": "отозван",
		"suspended": "suspended", "": "—",
	}
	for in, want := range cases {
		if got := inviteStatus(in); got != want {
			t.Errorf("%q: %q вместо %q", in, got, want)
		}
	}
}

// Выдача в команду: без --role — member, без --days поля days нет вовсе,
// «--days 0» уходит на сервер как 0 — ошибку срока даёт он.
func TestInviteCreateTeamBody(t *testing.T) {
	withJSON(t, false)
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"--team", "acme"}, map[string]any{"role": "member"}},
		{[]string{"--team", "acme", "--role", "lead"}, map[string]any{"role": "lead"}},
		{[]string{"--team", "acme", "--days", "3"}, map[string]any{"role": "member", "days": float64(3)}},
		{[]string{"--team", "acme", "--days", "0"}, map[string]any{"role": "member", "days": float64(0)}},
	}
	for _, tc := range cases {
		c, rec := newInviteClient(t, http.StatusCreated, issuedTeam)
		stdout, stderr, err := capture(t, func() error {
			return cmdInvite(context.Background(), c, append([]string{"create"}, tc.args...))
		})
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if rec.method != "POST" || rec.path != "/v1/teams/acme/invites" {
			t.Fatalf("%v: %s %s", tc.args, rec.method, rec.path)
		}
		if !equalJSON(rec.body, tc.want) {
			t.Fatalf("%v: тело %v, ждали %v", tc.args, rec.body, tc.want)
		}
		if !strings.Contains(stdout, "Ссылка: https://ui.example/invite/clab_inv_secret\n") {
			t.Fatalf("нет ссылки в stdout:\n%s", stdout)
		}
		if !strings.Contains(stderr, "один раз") || strings.Contains(stderr, "clab_inv_") {
			t.Fatalf("предупреждение в stderr:\n%s", stderr)
		}
	}
}

// Выдача в проект: role_keys уходит только при непустом --roles; без флагов
// цели — проект из SDLC_PROJECT.
func TestInviteCreateProjectBody(t *testing.T) {
	withJSON(t, false)
	t.Setenv("SDLC_PROJECT", "CLAB")
	cases := []struct {
		args []string
		path string
		want map[string]any
	}{
		{[]string{"--project", "ACME", "--level", "agent", "--roles", "developer,test"}, "/v1/projects/ACME/invites",
			map[string]any{"level": "agent", "role_keys": []any{"developer", "test"}}},
		{[]string{"--level", "agent"}, "/v1/projects/CLAB/invites", map[string]any{"level": "agent"}},
		{[]string{"--level", "viewer", "--roles", "", "--days", "30"}, "/v1/projects/CLAB/invites",
			map[string]any{"level": "viewer", "days": float64(30)}},
	}
	for _, tc := range cases {
		c, rec := newInviteClient(t, http.StatusCreated, issuedProject)
		stdout, _, err := capture(t, func() error {
			return cmdInvite(context.Background(), c, append([]string{"create"}, tc.args...))
		})
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if rec.path != tc.path || !equalJSON(rec.body, tc.want) {
			t.Fatalf("%v: %s %v, ждали %s %v", tc.args, rec.path, rec.body, tc.path, tc.want)
		}
		if !strings.Contains(stdout, "developer") || !strings.Contains(stdout, "project CLAB — Cadence Lab") {
			t.Fatalf("сводка:\n%s", stdout)
		}
	}
}

// --json у выдачи — ответ сервера как есть плюс поле url с той же ссылкой.
func TestInviteCreateJSONAddsURL(t *testing.T) {
	withJSON(t, true)
	c, _ := newInviteClient(t, http.StatusCreated, issuedTeam)
	stdout, _, err := capture(t, func() error {
		return cmdInvite(context.Background(), c, []string{"create", "--team", "acme"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("не JSON: %s", stdout)
	}
	var server map[string]any
	_ = json.Unmarshal([]byte(issuedTeam), &server)
	server["url"] = "https://ui.example/invite/clab_inv_secret"
	if !equalJSON(got, server) {
		t.Fatalf("JSON %v, ждали %v", got, server)
	}
}

// Ошибки формы команды ловятся до запроса: сервер не вызывается.
func TestInviteCreateFlagConflicts(t *testing.T) {
	t.Setenv("SDLC_PROJECT", "CLAB")
	cases := [][]string{
		{"--team", "acme", "--project", "CLAB", "--level", "agent"},
		{"--team", "acme", "--level", "agent"},
		{"--team", "acme", "--roles", "developer"},
		{"--project", "CLAB", "--role", "lead", "--level", "agent"},
		{"--role", "member"},
		{"--project", "CLAB"},
		{},
	}
	for _, args := range cases {
		c, rec := newInviteClient(t, http.StatusCreated, issuedTeam)
		_, stderr, err := capture(t, func() error {
			return cmdInvite(context.Background(), c, append([]string{"create"}, args...))
		})
		if err == nil || rec.calls != 0 {
			t.Fatalf("%v: ошибка %v, запросов %d", args, err, rec.calls)
		}
		if !strings.Contains(stderr, "clab invite create") {
			t.Fatalf("%v: нет usage в stderr:\n%s", args, stderr)
		}
	}
}

func TestInviteListConflictAndPaths(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusOK, `{"invites":[]}`)
	if _, _, err := capture(t, func() error {
		return cmdInvite(context.Background(), c, []string{"list", "--team", "acme", "--project", "CLAB"})
	}); err == nil || rec.calls != 0 {
		t.Fatalf("оба флага цели: %v, запросов %d", err, rec.calls)
	}

	stdout, _, err := capture(t, func() error {
		return cmdInvite(context.Background(), c, []string{"list", "--team", "acme"})
	})
	if err != nil || rec.method != "GET" || rec.path != "/v1/teams/acme/invites" || strings.TrimSpace(stdout) != "пусто" {
		t.Fatalf("список команды: %v %s %s %q", err, rec.method, rec.path, stdout)
	}

	t.Setenv("SDLC_PROJECT", "CLAB")
	if _, _, err := capture(t, func() error { return cmdInvite(context.Background(), c, nil) }); err != nil || rec.path != "/v1/projects/CLAB/invites" {
		t.Fatalf("список проекта из контекста: %v %s", err, rec.path)
	}
}

const inviteList = `{"invites":[
 {"id":"4f1c0c1e-0000-4000-8000-000000000005","target_kind":"project","project_key":"CLAB","target_name":"Cadence Lab","level":"agent","role_keys":null,"status":"suspended","created_by_handle":"alice","created_at":"2026-09-12T10:00:00Z","expires_at":"2026-09-19T10:00:00Z"},
 {"id":"4f1c0c1e-0000-4000-8000-000000000004","target_kind":"project","project_key":"CLAB","target_name":"Cadence Lab","level":"maintainer","role_keys":["developer","test"],"status":"revoked","created_by_handle":"alice","created_at":"2026-09-11T10:00:00Z","expires_at":"2026-09-18T10:00:00Z","revoked_by_handle":"bob","revoked_at":"2026-09-11T11:00:00Z"},
 {"id":"4f1c0c1e-0000-4000-8000-000000000003","target_kind":"project","project_key":"CLAB","target_name":"Cadence Lab","level":"viewer","role_keys":null,"status":"accepted","created_by_handle":"alice","created_at":"2026-09-10T10:00:00Z","expires_at":"2026-09-17T10:00:00Z","accepted_by_handle":"carol","accepted_at":"2026-09-10T12:00:00Z"},
 {"id":"4f1c0c1e-0000-4000-8000-000000000002","target_kind":"project","project_key":"CLAB","target_name":"Cadence Lab","level":"agent","role_keys":null,"status":"expired","created_by_handle":"alice","created_at":"2026-09-01T10:00:00Z","expires_at":"2026-09-02T10:00:00Z"},
 {"id":"4f1c0c1e-0000-4000-8000-000000000001","target_kind":"project","project_key":"CLAB","target_name":"Cadence Lab","level":"agent","role_keys":null,"status":"pending","created_by_handle":"alice","created_at":"2026-08-31T10:00:00Z","expires_at":"2026-09-30T10:00:00Z"}
]}`

// Таблица: полный ID, подписи статусов, «любые» у приглашения без ролей,
// незнакомый статус как есть, порядок сервера.
func TestRenderInvites(t *testing.T) {
	stdout, _, err := capture(t, func() error { return renderInvites(json.RawMessage(inviteList)) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("таблица:\n%s", stdout)
	}
	for _, h := range []string{"СТАТУС", "РОЛЬ", "РОЛИ", "ВЫДАЛ", "ИСТЕКАЕТ", "ПРИНЯЛ"} {
		if !strings.Contains(lines[0], h) {
			t.Fatalf("нет колонки %s: %s", h, lines[0])
		}
	}
	want := []struct{ id, status, extra string }{
		{"4f1c0c1e-0000-4000-8000-000000000005", "suspended", "любые"},
		{"4f1c0c1e-0000-4000-8000-000000000004", "отозван", "отозвал bob"},
		{"4f1c0c1e-0000-4000-8000-000000000003", "принят", "carol"},
		{"4f1c0c1e-0000-4000-8000-000000000002", "протух", "любые"},
		{"4f1c0c1e-0000-4000-8000-000000000001", "выдан", "любые"},
	}
	for i, w := range want {
		line := lines[i+1]
		if !strings.HasPrefix(line, w.id) || !strings.Contains(line, w.status) || !strings.Contains(line, w.extra) {
			t.Fatalf("строка %d: %q, ждали %v", i+1, line, w)
		}
	}
	if !strings.Contains(lines[2], "developer, test") {
		t.Fatalf("роли: %q", lines[2])
	}
}

// Отзыв: DELETE по идентификатору, сводка со статусом; ошибка сервера
// возвращается как есть — код и текст без обёрток.
func TestInviteRevoke(t *testing.T) {
	withJSON(t, false)
	id := "4f1c0c1e-0000-4000-8000-000000000001"
	c, rec := newInviteClient(t, http.StatusOK, `{"id":"`+id+`","target_kind":"team","team_key":"acme","target_name":"Acme","team_role":"member","role_keys":null,"status":"revoked","created_by_handle":"alice","expires_at":"2026-09-19T10:00:00Z","revoked_by_handle":"alice","revoked_at":"2026-09-12T11:00:00Z"}`)
	stdout, _, err := capture(t, func() error { return cmdInvite(context.Background(), c, []string{"revoke", id}) })
	if err != nil || rec.method != "DELETE" || rec.path != "/v1/invites/"+id {
		t.Fatalf("отзыв: %v %s %s", err, rec.method, rec.path)
	}
	if !strings.Contains(stdout, "отозван") || !strings.Contains(stdout, "alice") {
		t.Fatalf("сводка:\n%s", stdout)
	}

	if _, _, err := capture(t, func() error { return cmdInvite(context.Background(), c, []string{"revoke"}) }); err == nil || rec.calls != 1 {
		t.Fatalf("revoke без id: %v, запросов %d", err, rec.calls)
	}

	c, _ = newInviteClient(t, http.StatusConflict, `{"error":{"code":"conflict","message":"приглашение уже принято"}}`)
	_, _, err = capture(t, func() error { return cmdInvite(context.Background(), c, []string{"revoke", id}) })
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "conflict" || apiErr.Message != "приглашение уже принято" || apiErr.Status != http.StatusConflict {
		t.Fatalf("ошибка сервера: %#v", err)
	}
}

func TestInviteUnknownSubcommandAndCatalog(t *testing.T) {
	err := cmdInvite(context.Background(), &Client{}, []string{"foo"})
	if err == nil || !strings.Contains(err.Error(), "create, list, revoke") {
		t.Fatalf("неизвестная подкоманда: %v", err)
	}
	for _, cmd := range commands() {
		if cmd.name == "invite" {
			for _, sub := range []string{"invite create", "invite list", "invite revoke"} {
				if !strings.Contains(cmd.usage, sub) {
					t.Fatalf("в usage нет %q", sub)
				}
			}
			return
		}
	}
	t.Fatal("invite нет в каталоге команд")
}

func equalJSON(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
