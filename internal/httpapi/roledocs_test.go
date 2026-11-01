package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Версия роли из документов по REST и MCP (CLAB-343, решение 0068): форма
// ответов по контракту страницы «Пайплайн», права, журнал действий.
// Номера — критерии приёмки анализа CLAB-343.

// tokenFor заводит участника уровня level и выдаёт ему личный токен.
func (f *auditFixture) tokenFor(handle string, level domain.MemberRole) string {
	f.t.Helper()
	ctx := context.Background()
	store := postgres.New(f.pool)
	if _, err := store.ResolveActor(ctx, store.DB(), strings.TrimPrefix(handle, "user:"), domain.PrincipalUser); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.auth.SetMember(ctx, f.admin, "SDLC", handle, level, nil); err != nil {
		f.t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{Name: handle, Principal: handle, ProjectKey: "SDLC"})
	if err != nil {
		f.t.Fatal(err)
	}
	return issued.Token
}

func (f *auditFixture) roleRows() [3]int {
	f.t.Helper()
	var out [3]int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM role_versions), (SELECT count(*) FROM role_version_documents),
		       (SELECT count(*) FROM blobs)`).Scan(&out[0], &out[1], &out[2]); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Ключи версии роли до CLAB-343 — снимок прежнего ответа: все остаются на
// месте (у версии без уровня и подсказки model_level/model_hint нет).
var roleVersionKeysBefore = []string{"changelog", "context_budget", "created_at", "id", "input_kinds",
	"instructions", "output_contract", "repo_scope", "role_id", "tool_policy", "version"}

func checkVersionShape(t *testing.T, where string, v map[string]any, withContent bool) []any {
	t.Helper()
	for _, k := range roleVersionKeysBefore {
		if _, ok := v[k]; !ok && k != "changelog" {
			t.Fatalf("%s: нет прежнего ключа %q (%v)", where, k, keysOf(v))
		}
	}
	docs, ok := v["documents"].([]any)
	if !ok || len(docs) == 0 {
		t.Fatalf("%s: documents — не непустой массив: %v", where, v["documents"])
	}
	for i, raw := range docs {
		d := raw.(map[string]any)
		for _, k := range []string{"name", "kind", "position", "sha256", "size_bytes"} {
			if _, ok := d[k]; !ok {
				t.Fatalf("%s: у документа %d нет %q", where, i, k)
			}
		}
		if _, has := d["content"]; has != withContent {
			t.Fatalf("%s: документ %d, content есть=%v, ждали %v", where, i, has, withContent)
		}
		if int(d["position"].(float64)) != i {
			t.Fatalf("%s: позиции не по порядку: %v", where, docs)
		}
	}
	return docs
}

// AC-4, AC-10, AC-20: форма ответов по ручкам контракта.
func TestRoleDocumentsOverREST(t *testing.T) {
	f := newAuditFixture(t)
	docs := []map[string]any{
		{"name": "main.md", "content": "Ты писатель."},
		{"name": "git.md", "kind": "reference", "content": "Ветки."},
		{"name": "check.md", "kind": "checklist", "content": "- ссылки"},
	}
	code, created := f.json("POST", "/v1/projects/SDLC/roles", f.root, map[string]any{
		"key": "writer", "name": "Техписатель", "version": map[string]any{"documents": docs},
	})
	if code != http.StatusCreated {
		t.Fatalf("POST roles: %d %v", code, created)
	}
	checkVersionShape(t, "POST roles latest_version", created["latest_version"].(map[string]any), true)

	code, v2 := f.json("POST", "/v1/projects/SDLC/roles/writer/versions", f.root, map[string]any{
		"documents": docs[:2], "changelog": "без чек-листа",
	})
	if code != http.StatusCreated || v2["version"].(float64) != 2 {
		t.Fatalf("POST versions: %d %v", code, v2)
	}
	checkVersionShape(t, "POST versions", v2, true)

	code, one := f.json("GET", "/v1/projects/SDLC/roles/writer/versions/1", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("GET версии: %d %v", code, one)
	}
	got := checkVersionShape(t, "GET versions/1", one, true)
	if len(got) != 3 || got[1].(map[string]any)["kind"] != "reference" || got[0].(map[string]any)["kind"] != "instructions" {
		t.Fatalf("документы v1: %v", got)
	}
	if !strings.HasPrefix(one["instructions"].(string), "<!-- document: main.md -->\n") {
		t.Fatalf("склейка v1: %q", one["instructions"])
	}

	code, list := f.json("GET", "/v1/projects/SDLC/roles/writer/versions", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("GET versions: %d %v", code, list)
	}
	for _, raw := range list["versions"].([]any) {
		checkVersionShape(t, "GET versions[]", raw.(map[string]any), false)
	}

	code, role := f.json("GET", "/v1/projects/SDLC/roles/writer", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("GET role: %d %v", code, role)
	}
	for _, k := range []string{"role", "versions", "latest_version", "used_on"} {
		if _, ok := role[k]; !ok {
			t.Fatalf("GET role без %q: %v", k, keysOf(role))
		}
	}
	for _, raw := range role["versions"].([]any) {
		checkVersionShape(t, "GET role versions[]", raw.(map[string]any), false)
	}
	latest := role["latest_version"].(map[string]any)
	checkVersionShape(t, "GET role latest_version", latest, true)
	if latest["version"].(float64) != 2 {
		t.Fatalf("latest_version: %v", latest["version"])
	}

	// Версия сида без строк документов — один main.md из колонки.
	code, seeded := f.json("GET", "/v1/projects/SDLC/roles/analyst/versions/1", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("версия сида: %d %v", code, seeded)
	}
	sd := checkVersionShape(t, "версия сида", seeded, true)
	if len(sd) != 1 || sd[0].(map[string]any)["name"] != "main.md" || sd[0].(map[string]any)["content"] != seeded["instructions"] {
		t.Fatalf("версия сида: %v", sd)
	}

	if code, body := f.json("GET", "/v1/projects/SDLC/roles/writer/versions/9", f.root, nil); code != http.StatusNotFound {
		t.Fatalf("неизвестная версия: %d %v", code, body)
	}
}

// AC-5: отказы — 400 с документом и причиной, базы не меняют.
func TestRoleDocumentsRejectsOverREST(t *testing.T) {
	f := newAuditFixture(t)
	before := f.roleRows()
	for name, body := range map[string]map[string]any{
		"пустой набор":   {"documents": []any{}},
		"ни одного поля": {"changelog": "x"},
		"оба поля":       {"instructions": "x", "documents": []any{map[string]any{"name": "a.md", "content": "x"}}},
		"имя":            {"documents": []any{map[string]any{"name": "A.md", "content": "x"}}},
		"дубль":          {"documents": []any{map[string]any{"name": "a.md", "content": "x"}, map[string]any{"name": "a.md", "content": "y"}}},
		"NUL":            {"documents": []any{map[string]any{"name": "a.md", "content": "a\u0000b"}}},
		"пусто":          {"documents": []any{map[string]any{"name": "a.md", "content": " \n"}}},
	} {
		code, resp := f.json("POST", "/v1/projects/SDLC/roles/developer/versions", f.root, body)
		if code != http.StatusBadRequest || errCode(resp) != "invalid_input" {
			t.Fatalf("%s: %d %v", name, code, resp)
		}
	}
	if after := f.roleRows(); after != before {
		t.Fatalf("отказы изменили базу: %v → %v", before, after)
	}
}

// AC-14, AC-15: права (viewer, agent, токен задачи — 403; viewer читает) и
// журнал без текста документов.
func TestRoleDocumentsPermissionsAndAudit(t *testing.T) {
	f := newAuditFixture(t)
	viewer := f.tokenFor("user:vera", domain.MemberViewer)
	for _, path := range []string{"/v1/tasks/SDLC-1/transitions/start", "/v1/tasks/SDLC-1/claim"} {
		if code, body := f.json("POST", path, f.root, map[string]any{}); code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
	code, issued := f.json("POST", "/v1/tasks/SDLC-1/task-token", f.root, nil)
	if code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %v", code, issued)
	}
	taskToken := issued["token"].(string)

	body := map[string]any{"documents": []any{map[string]any{"name": "main.md", "content": "секретное правило роли"}}}
	before := f.roleRows()
	for name, tok := range map[string]string{"viewer": viewer, "agent": f.bot, "токен задачи": taskToken} {
		if code, resp := f.json("POST", "/v1/projects/SDLC/roles/developer/versions", tok, body); code != http.StatusForbidden {
			t.Fatalf("%s: %d %v", name, code, resp)
		}
	}
	if out, isErr := f.mcp(taskToken, "sdlc_put_role", map[string]any{
		"key": "developer", "documents": []any{map[string]any{"name": "main.md", "content": "x"}},
	}); !isErr {
		t.Fatalf("sdlc_put_role токеном задачи прошёл: %v", out)
	}
	if after := f.roleRows(); after != before {
		t.Fatalf("отказы изменили базу: %v → %v", before, after)
	}
	if code, resp := f.json("GET", "/v1/projects/SDLC/roles/developer/versions/1", viewer, nil); code != http.StatusOK {
		t.Fatalf("GET от viewer: %d %v", code, resp)
	}

	if code, resp := f.json("POST", "/v1/projects/SDLC/roles/developer/versions", f.root, body); code != http.StatusCreated {
		t.Fatalf("POST от администратора: %d %v", code, resp)
	}
	rows := f.rows(`a.action = 'POST /v1/projects/:projectKey/roles/:roleKey/versions' AND a.outcome = 'ok'`)
	if len(rows) != 1 {
		t.Fatalf("записей журнала: %d", len(rows))
	}
	if strings.Contains(rows[0].Params, "секретное правило роли") || !strings.Contains(rows[0].Params, `"sha256"`) {
		t.Fatalf("текст документа в журнале: %s", rows[0].Params)
	}
}

// AC-18: sdlc_put_role с documents и sdlc_get_role без instructions.
func TestRoleDocumentsOverMCP(t *testing.T) {
	f := newAuditFixture(t)
	docs := []any{
		map[string]any{"name": "main.md", "content": "Ты писатель."},
		map[string]any{"name": "git.md", "kind": "reference", "content": "Ветки."},
	}
	out, isErr := f.mcp(f.root, "sdlc_put_role", map[string]any{"key": "writer", "name": "Техписатель", "documents": docs})
	if isErr || out["outcome"].(map[string]any)["outcome"] != "created" {
		t.Fatalf("sdlc_put_role: %v", out)
	}
	if _, has := out["latest_version"].(map[string]any)["instructions"]; has {
		t.Fatalf("sdlc_put_role отдаёт склейку: %v", out["latest_version"])
	}
	out, isErr = f.mcp(f.root, "sdlc_put_role", map[string]any{"key": "writer", "name": "Техписатель", "documents": docs})
	if isErr || out["outcome"].(map[string]any)["outcome"] != "unchanged" {
		t.Fatalf("повтор sdlc_put_role: %v", out)
	}
	out, isErr = f.mcp(f.root, "sdlc_put_role", map[string]any{"key": "writer", "name": "Техписатель",
		"documents": []any{docs[0], map[string]any{"name": "git.md", "kind": "reference", "content": "Ветки и PR."}}})
	if isErr || out["outcome"].(map[string]any)["outcome"] != "new_version" {
		t.Fatalf("правка документа через MCP: %v", out)
	}
	if out, isErr := f.mcp(f.root, "sdlc_put_role", map[string]any{"key": "writer", "instructions": "x", "documents": docs}); !isErr {
		t.Fatalf("оба поля через MCP: %v", out)
	}
	// Прежний вход строкой работает.
	if out, isErr := f.mcp(f.root, "sdlc_put_role", map[string]any{"key": "plain", "instructions": "Одна строка."}); isErr ||
		out["outcome"].(map[string]any)["outcome"] != "created" {
		t.Fatalf("instructions строкой через MCP: %v", out)
	}

	got, isErr := f.mcp(f.root, "sdlc_get_role", map[string]any{"key": "writer"})
	if isErr {
		t.Fatalf("sdlc_get_role: %v", got)
	}
	latest := got["latest_version"].(map[string]any)
	if _, has := latest["instructions"]; has {
		t.Fatalf("sdlc_get_role отдаёт instructions: %v", keysOf(latest))
	}
	gd := latest["documents"].([]any)
	if len(gd) != 2 || gd[1].(map[string]any)["content"] != "Ветки и PR." {
		t.Fatalf("документы sdlc_get_role: %v", gd)
	}
	raw, _ := json.Marshal(latest)
	if !strings.Contains(string(raw), `"input_kinds"`) {
		t.Fatalf("прочие поля версии потеряны: %s", raw)
	}
}
