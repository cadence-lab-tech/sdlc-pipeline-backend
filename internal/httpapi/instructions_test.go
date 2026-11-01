package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Библиотека инструкций по REST (CLAB-351): права (AC-9) и журнал (AC-10).
func TestInstructionsRESTRightsAndAudit(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	if _, err := f.srv.store.ResolveActor(ctx, f.srv.store.DB(), "vera", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.SetMember(ctx, f.admin, "SDLC", "user:vera", domain.MemberViewer, nil); err != nil {
		t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{Name: "vera", Principal: "user:vera", ProjectKey: "SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	viewer := issued.Token
	const secret = "Текст инструкции, которого в журнале быть не должно.\n"
	create := map[string]any{"key": "git", "name": "Правила git", "content": secret}

	for name, tok := range map[string]string{"viewer": viewer, "agent": f.bot} {
		if rec := f.do("POST", "/v1/projects/SDLC/instructions", tok, create); rec.Code != http.StatusForbidden {
			t.Fatalf("%s заводит инструкцию: %d %s", name, rec.Code, rec.Body)
		}
	}
	rec := f.do("POST", "/v1/projects/SDLC/instructions", f.root, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("maintainer: %d %s", rec.Code, rec.Body)
	}
	var created workflow.InstructionWrite
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec := f.do("POST", "/v1/projects/SDLC/instructions", f.root, create); rec.Code != http.StatusConflict {
		t.Fatalf("занятый ключ: %d %s", rec.Code, rec.Body)
	}
	for name, tok := range map[string]string{"viewer": viewer, "agent": f.bot} {
		for _, path := range []string{"/v1/projects/SDLC/instructions", "/v1/projects/SDLC/instructions/git",
			"/v1/projects/SDLC/instructions/git/versions/1"} {
			if rec := f.do("GET", path, tok, nil); rec.Code != http.StatusOK {
				t.Fatalf("%s читает %s: %d %s", name, path, rec.Code, rec.Body)
			}
		}
		for _, w := range []struct{ method, path string }{
			{"POST", "/v1/projects/SDLC/instructions/git/versions"},
			{"PATCH", "/v1/projects/SDLC/instructions/git"},
			{"POST", "/v1/projects/SDLC/instructions/git/apply"},
		} {
			if rec := f.do(w.method, w.path, tok, map[string]any{"content": "x", "name": "x", "roles": []any{map[string]any{"key": "developer"}}}); rec.Code != http.StatusForbidden {
				t.Fatalf("%s пишет %s %s: %d %s", name, w.method, w.path, rec.Code, rec.Body)
			}
		}
	}

	rows := f.rows(`a.action = 'POST /v1/projects/:projectKey/instructions' AND a.outcome = 'ok'`)
	if len(rows) != 1 {
		t.Fatalf("записей заведения: %d", len(rows))
	}
	p := rows[0].Params
	if strings.Contains(p, "в журнале быть не должно") {
		t.Fatalf("содержимое инструкции в журнале: %s", p)
	}
	for _, want := range []string{`"instruction": "git"`, `"version": 1`, created.Version.SHA256, `"size_bytes"`} {
		if !strings.Contains(p, want) {
			t.Fatalf("в записи заведения нет %s: %s", want, p)
		}
	}

	// «Обновить в ролях»: роли и их новые версии — в журнале.
	if _, err := f.srv.authoring.AddRoleVersion(ctx, "SDLC", "developer", f.admin.Principal.ID, workflow.RoleVersionInput{
		Documents: []workflow.RoleDocumentInput{{Name: "main.md", Content: "Ты разработчик."}, {Name: "git.md", Content: secret}},
	}); err != nil {
		t.Fatal(err)
	}
	rec = f.do("POST", "/v1/projects/SDLC/instructions/git/apply", f.root, map[string]any{"roles": []any{map[string]any{"key": "developer"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	var res workflow.ApplyResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Roles) != 1 || res.Roles[0].Outcome != "created" {
		t.Fatalf("итог apply: %+v", res)
	}
	rows = f.rows(`a.action = 'POST /v1/projects/:projectKey/instructions/:instructionKey/apply' AND a.outcome = 'ok'`)
	if len(rows) != 1 || !strings.Contains(rows[0].Params, `"role_version"`) || !strings.Contains(rows[0].Params, `"developer"`) {
		t.Fatalf("журнал apply: %+v", rows)
	}
	// Роль в ответе списка несёт отставание после новой версии.
	if rec := f.do("POST", "/v1/projects/SDLC/instructions/git/versions", f.root, map[string]any{"content": secret + "v2\n"}); rec.Code != http.StatusCreated {
		t.Fatalf("версия 2: %d %s", rec.Code, rec.Body)
	}
	rec = f.do("GET", "/v1/projects/SDLC/roles", viewer, nil)
	if !strings.Contains(rec.Body.String(), `"instruction_lag":[{"key":"git","pinned_version":1,"latest_version":2}]`) {
		t.Fatalf("отставание в списке ролей: %s", rec.Body)
	}
}
