package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// clab role propose|proposals|approve|reject|withdraw против настоящего
// сервера (CLAB-381, решение 0072 п. 5; AC-11, AC-5 анализа CLAB-381).
func TestRoleProposalsCLI(t *testing.T) {
	withJSON(t, false)
	ctx := context.Background()
	w := newLeaseWorld(t, time.Minute)
	auth := workflow.NewAuth(w.store, slog.New(slog.DiscardHandler), workflow.AuthToken)
	root, err := auth.Authenticate(ctx, leaseRootToken)
	if err != nil {
		t.Fatal(err)
	}
	member := func(handle string, level domain.MemberRole, roles []string) *Client {
		t.Helper()
		if _, err := w.store.ResolveActor(ctx, w.store.DB(), handle, domain.PrincipalUser); err != nil {
			t.Fatal(err)
		}
		if _, err := auth.SetMember(ctx, root, "SDLC", "user:"+handle, level, roles); err != nil {
			t.Fatal(err)
		}
		issued, err := auth.IssueToken(ctx, root, workflow.IssueTokenInput{
			Name: "personal", Principal: "user:" + handle, ProjectKey: "SDLC", MemberRole: level,
		})
		if err != nil {
			t.Fatal(err)
		}
		return w.client(issued.Token)
	}
	dev := member("dev", domain.MemberAgent, []string{"analyst"})
	maint := member("maint", domain.MemberMaintainer, nil)
	const gitV1 = "Ветки clab/<ключ>-<slug>.\n"
	w.call("POST", "/v1/projects/SDLC/instructions", leaseRootToken,
		map[string]any{"key": "git", "name": "git", "content": gitV1}, http.StatusCreated)

	run := func(c *Client, args ...string) (map[string]any, error) {
		t.Helper()
		out, _, err := capture(t, func() error { return cmdRole(ctx, c, args) })
		if err != nil {
			return nil, err
		}
		body := map[string]any{}
		if err := json.Unmarshal([]byte(out), &body); err != nil {
			t.Fatalf("%v: вывод не JSON: %s", args, out)
		}
		return body, nil
	}
	mustRun := func(c *Client, args ...string) map[string]any {
		t.Helper()
		out, err := run(c, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}

	// Каталог роли: закреплённый документ git.md уходит ссылкой {key} с
	// содержимым, сервер находит версию по sha.
	dir := t.TempDir()
	for name, body := range map[string]string{
		"role.json": `{"key": "analyst", "name": "Аналитик", "model_level": "low",
			"documents": [{"name": "main.md"}, {"name": "git.md", "instruction": "git"}]}`,
		"main.md": "Ты аналитик из каталога.\n",
		"git.md":  gitV1,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(dev, "propose", "--project", "SDLC", "--dir", dir); err == nil || !strings.Contains(err.Error(), "--changelog") {
		t.Fatalf("без changelog: %v", err)
	}
	p := mustRun(dev, "propose", "--project", "SDLC", "--dir", dir, "--changelog", "git из библиотеки")
	id, _ := p["id"].(string)
	docs, _ := p["documents"].([]any)
	if p["state"] != "pending" || p["model_level"] != "low" || len(docs) != 2 {
		t.Fatalf("propose --dir: %v", p)
	}
	if pin, _ := docs[1].(map[string]any)["instruction"].(map[string]any); pin["key"] != "git" || pin["version"] != float64(1) {
		t.Fatalf("git.md не закреплён: %v", docs[1])
	}

	list := mustRun(dev, "proposals", "analyst", "--project", "SDLC", "--state", "pending")
	if items, _ := list["proposals"].([]any); len(items) != 1 {
		t.Fatalf("proposals: %v", list)
	}
	if one := mustRun(dev, "proposals", "analyst", id, "--project", "SDLC"); one["id"] != id {
		t.Fatalf("proposals <id>: %v", one)
	}

	// AC-5: role put у agent — отказ с подсказкой про propose.
	file := filepath.Join(t.TempDir(), "main.md")
	if err := os.WriteFile(file, []byte("Ты аналитик.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(dev, "put", "--project", "SDLC", "--key", "analyst", "--name", "Аналитик", "--file", file); err == nil ||
		!strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "clab role propose") {
		t.Fatalf("role put агентом: %v", err)
	}

	if _, err := run(dev, "approve", id, "--project", "SDLC"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("approve агентом: %v", err)
	}
	approved := mustRun(maint, "approve", id, "--project", "SDLC")
	if approved["state"] != "approved" || approved["result_version"] == nil {
		t.Fatalf("approve: %v", approved)
	}

	second := mustRun(dev, "propose", "analyst", "--project", "SDLC", "--file", file, "--changelog", "второе")
	sid := second["id"].(string)
	if _, err := run(maint, "reject", sid, "--project", "SDLC"); err == nil || !strings.Contains(err.Error(), "--note") {
		t.Fatalf("reject без --note: %v", err)
	}
	if rejected := mustRun(maint, "reject", sid, "--project", "SDLC", "--role", "analyst", "--note", "мало"); rejected["state"] != "rejected" {
		t.Fatalf("reject: %v", rejected)
	}
	third := mustRun(dev, "propose", "analyst", "--project", "SDLC", "--file", file, "--changelog", "третье")
	if withdrawn := mustRun(dev, "withdraw", third["id"].(string), "--project", "SDLC"); withdrawn["state"] != "withdrawn" {
		t.Fatalf("withdraw: %v", withdrawn)
	}
	if _, err := run(maint, "approve", "00000000-0000-4000-8000-000000000000", "--project", "SDLC"); err == nil ||
		!strings.Contains(err.Error(), "не найдено") {
		t.Fatalf("approve несуществующего: %v", err)
	}
}
