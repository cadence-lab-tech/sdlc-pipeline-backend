package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Предложения версий ролей по REST (CLAB-381, решение 0072 п. 5). Номера —
// критерии приёмки анализа CLAB-381; права считает workflow.Auth, здесь —
// коды и формы ответов транспорта.
func TestRoleProposalsOverREST(t *testing.T) {
	ctx := context.Background()
	srv, store, call := newTaskTokenServer(t)
	root, err := srv.auth.Authenticate(ctx, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	member := func(raw string, level domain.MemberRole, roles []string) string {
		t.Helper()
		kind, handle := workflow.SplitHandle(raw, domain.PrincipalUser)
		if kind == domain.PrincipalUser {
			if _, err := store.ResolveActor(ctx, store.DB(), handle, kind); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := srv.auth.SetMember(ctx, root, "SDLC", raw, level, roles); err != nil {
			t.Fatal(err)
		}
		issued, err := srv.auth.IssueToken(ctx, root, workflow.IssueTokenInput{
			Name: "personal", Principal: raw, ProjectKey: "SDLC", MemberRole: level,
		})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token
	}
	dev := member("user:dev", domain.MemberAgent, []string{"analyst"})
	maint := member("user:maint", domain.MemberMaintainer, nil)
	viewer := member("user:viewer", domain.MemberViewer, nil)
	base := "/v1/projects/SDLC/roles/analyst"
	body := func(text string) map[string]any {
		return map[string]any{
			"documents": []map[string]any{{"name": "main.md", "content": text}},
			"changelog": "правка " + text,
		}
	}

	// AC-1, AC-2: подача — 201 pending; чужая роль, viewer — 403; нет роли — 404.
	code, created := call("POST", base+"/proposals", dev, body("Ты аналитик v2.\n"))
	if code != http.StatusCreated || created["state"] != "pending" || created["role"] != "analyst" {
		t.Fatalf("подача: %d %v", code, created)
	}
	id, _ := created["id"].(string)
	baseVersion := created["base_version"]
	for _, c := range []struct {
		path, token string
		want        int
	}{
		{"/v1/projects/SDLC/roles/developer/proposals", dev, http.StatusForbidden},
		{base + "/proposals", viewer, http.StatusForbidden},
		{"/v1/projects/SDLC/roles/no-such/proposals", maint, http.StatusNotFound},
	} {
		if code, out := call("POST", c.path, c.token, body("x")); code != c.want {
			t.Errorf("POST %s: %d %v, ждали %d", c.path, code, out, c.want)
		}
	}
	if code, out := call("POST", base+"/proposals", dev, map[string]any{"instructions": "x"}); code != http.StatusBadRequest {
		t.Errorf("без changelog: %d %v", code, out)
	}
	code, list := call("GET", base+"/proposals?state=pending", viewer, nil)
	items, _ := list["proposals"].([]any)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("список: %d %v", code, list)
	}
	code, one := call("GET", base+"/proposals/"+id, viewer, nil)
	docs, _ := one["documents"].([]any)
	if code != http.StatusOK || len(docs) != 1 || docs[0].(map[string]any)["content"] != "Ты аналитик v2.\n" {
		t.Fatalf("чтение: %d %v", code, one)
	}
	if code, _ := call("GET", base+"/proposals/not-a-uuid", viewer, nil); code != http.StatusBadRequest {
		t.Errorf("кривой id: %d", code)
	}

	// AC-5: прямая версия, импорт, роль — по-прежнему maintainer; agent
	// получает подсказку про clab role propose, viewer — обычный отказ.
	code, out := call("POST", base+"/versions", dev, map[string]any{"instructions": "x"})
	if code != http.StatusForbidden || !strings.Contains(errorText(out), "clab role propose") {
		t.Fatalf("прямая версия agent: %d %v", code, out)
	}
	code, out = call("POST", base+"/versions", viewer, map[string]any{"instructions": "x"})
	if code != http.StatusForbidden || strings.Contains(errorText(out), "propose") {
		t.Fatalf("прямая версия viewer: %d %v", code, out)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/projects/SDLC/import"}, {"POST", "/v1/projects/SDLC/roles"},
		{"DELETE", base}, {"PATCH", base},
	} {
		if code, out := call(c.method, c.path, dev, map[string]any{}); code != http.StatusForbidden {
			t.Errorf("%s %s агентом: %d %v", c.method, c.path, code, out)
		}
	}

	// AC-8: утверждает maintainer, agent-автор — 403.
	if code, out := call("POST", base+"/proposals/"+id+"/approve", dev, nil); code != http.StatusForbidden {
		t.Fatalf("утверждает автор-agent: %d %v", code, out)
	}
	stale := call2(t, call, "POST", base+"/proposals", dev, body("второе"))
	code, approved := call("POST", base+"/proposals/"+id+"/approve", maint, nil)
	if code != http.StatusOK || approved["state"] != "approved" || approved["result_version"] != baseVersion.(float64)+1 {
		t.Fatalf("утверждение: %d %v", code, approved)
	}
	if code, _ := call("POST", base+"/proposals/"+id+"/approve", maint, nil); code != http.StatusConflict {
		t.Errorf("повторное утверждение: %d", code)
	}
	// AC-7: устаревшая база — 409 stale_role_base с номерами.
	code, out = call("POST", base+"/proposals/"+stale+"/approve", maint, nil)
	details, _ := out["error"].(map[string]any)["details"].(map[string]any)
	if code != http.StatusConflict || errCode(out) != "stale_role_base" ||
		details["base_version"] != baseVersion || details["latest_version"] != baseVersion.(float64)+1 {
		t.Fatalf("устаревшая база: %d %v", code, out)
	}
	// AC-10: отклонение без заметки — 400, с заметкой — 200.
	if code, out := call("POST", base+"/proposals/"+stale+"/reject", maint, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("отклонение без заметки: %d %v", code, out)
	}
	code, rejected := call("POST", base+"/proposals/"+stale+"/reject", maint, map[string]any{"note": "от новой базы"})
	if code != http.StatusOK || rejected["state"] != "rejected" || rejected["decision_note"] != "от новой базы" {
		t.Fatalf("отклонение: %d %v", code, rejected)
	}
	// Отзыв автором — пустым телом.
	third := call2(t, call, "POST", base+"/proposals", dev, body("третье"))
	if code, out := call("POST", base+"/proposals/"+third+"/withdraw", viewer, nil); code != http.StatusForbidden {
		t.Fatalf("отзыв чужим viewer: %d %v", code, out)
	}
	code, withdrawn := call("POST", base+"/proposals/"+third+"/withdraw", dev, nil)
	if code != http.StatusOK || withdrawn["state"] != "withdrawn" {
		t.Fatalf("отзыв автором: %d %v", code, withdrawn)
	}
	// Предложение чужой роли по пути другой роли — 404.
	if code, _ := call("GET", "/v1/projects/SDLC/roles/architect/proposals/"+third, viewer, nil); code != http.StatusNotFound {
		t.Errorf("чужая роль в пути: %d", code)
	}
}

func errorText(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
}

// call2 — подача, которой нужен только id.
func call2(t *testing.T, call restCall, method, path, token string, body any) string {
	t.Helper()
	code, out := call(method, path, token, body)
	if code != http.StatusCreated {
		t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out["id"].(string)
}
