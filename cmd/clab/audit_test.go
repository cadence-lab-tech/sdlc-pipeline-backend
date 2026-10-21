package main

import (
	"context"
	"strings"
	"testing"
)

const auditPage = `{"entries":[
 {"id":"6a1f0000-0000-4000-8000-000000000001","at":"2026-10-21T12:00:00Z","project_key":"CLAB","task_key":"CLAB-12",
  "principal":{"handle":"runner","kind":"agent"},"token":{"id":"7b000000-0000-4000-8000-000000000001","name":"clab-runner","prefix":"clab_abcd","kind":"task"},
  "channel":"mcp","action":"sdlc_submit_artifact","params":{},"outcome":{"ok":true,"code":null,"status":null},"request_id":"r1"},
 {"id":"6a1f0000-0000-4000-8000-000000000002","at":"2026-10-21T11:00:00Z","project_key":"CLAB","task_key":null,
  "principal":{"handle":"alice","kind":"user"},"token":{"id":null,"name":"ui","prefix":"sess","kind":"session"},
  "channel":"rest","action":"POST /v1/projects/:projectKey/roles/:roleKey/versions","params":{},"outcome":{"ok":false,"code":"forbidden","status":403},"request_id":"r2"}
],"next":"Y3Vyc29y"}`

// clab audit (CLAB-210, критерий 17): задача, проект, инстанс — свои ручки;
// фильтры уходят строкой запроса; таблица несёт кто, токен и вид, канал,
// действие и исход; курсор следующей страницы напечатан.
func TestAuditCommand(t *testing.T) {
	c, rec := newInviteClient(t, 200, auditPage)
	out, _, err := capture(t, func() error {
		return cmdAudit(context.Background(), c, []string{"CLAB-12", "--channel", "mcp", "--outcome", "forbidden"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != "GET" || rec.path != "/v1/tasks/CLAB-12/audit" {
		t.Fatalf("запрос %s %s", rec.method, rec.path)
	}
	for _, want := range []string{"agent:runner", "clab-runner · task", "user:alice", "ui · session",
		"sdlc_submit_artifact", "forbidden 403", "дальше: --cursor Y3Vyc29y"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}

	// --project без значения — текущий проект; ключ из окружения.
	t.Setenv("SDLC_PROJECT", "CLAB")
	if _, _, err := capture(t, func() error {
		return cmdAudit(context.Background(), c, []string{"--project", "--task", "CLAB-12"})
	}); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/v1/projects/CLAB/audit" {
		t.Fatalf("журнал проекта: %s", rec.path)
	}
	out, _, err = capture(t, func() error {
		return cmdAudit(context.Background(), c, []string{"--project", "ACME"})
	})
	if err != nil || rec.path != "/v1/projects/ACME/audit" || !strings.Contains(out, "ЗАДАЧА") {
		t.Fatalf("журнал проекта ACME: %s %v\n%s", rec.path, err, out)
	}

	if _, _, err := capture(t, func() error {
		return cmdAudit(context.Background(), c, []string{"--instance", "--since", "2026-10-01"})
	}); err != nil || rec.path != "/v1/audit" {
		t.Fatalf("журнал инстанса: %s %v", rec.path, err)
	}

	// --json — ответ сервера как есть.
	withJSON(t, true)
	out, _, _ = capture(t, func() error { return cmdAudit(context.Background(), c, []string{"CLAB-12"}) })
	if !strings.Contains(out, `"request_id": "r1"`) {
		t.Fatalf("--json:\n%s", out)
	}
}

func TestBareProjectFlag(t *testing.T) {
	cases := map[string]string{
		"--project":                "--project=",
		"--project --task CLAB-1":  "--project= --task CLAB-1",
		"--project ACME":           "--project ACME",
		"--project=ACME --limit 5": "--project=ACME --limit 5",
	}
	for in, want := range cases {
		if got := strings.Join(bareProject(strings.Fields(in)), " "); got != want {
			t.Fatalf("%q: %q вместо %q", in, got, want)
		}
	}
}
