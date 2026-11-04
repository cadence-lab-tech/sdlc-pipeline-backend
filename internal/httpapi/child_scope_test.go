package httpapi

import (
	"net/http"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Граница токена задачи — по привязке, а не по уровню держателя (CLAB-411,
// R-4, AC-15; В-7 эпика CLAB-404). До CLAB-411 при держателе-сопровождающем
// токен задачи заводил ребёнка своей задачи с репозиторием вне её области:
// правило агента пропускало по уровню. Тест падал на прежнем коде (первый
// коммит ветки) и держит границу при держателе любого уровня.
func TestTaskTokenChildScopeByBinding(t *testing.T) {
	f := newAuditFixture(t)
	lead := f.memberToken("user:lead", domain.MemberMaintainer, nil)
	const backend, docs = "cadence-lab-tech/sdlc-pipeline-backend", "cadence-lab-tech/sdlc-pipeline-docs"

	// Своя задача токена: область — backend; держатель — сопровождающий
	// (путь /clab-run) или бот уровня agent.
	own := func(title, holder string) (key, token string) {
		t.Helper()
		code, created := f.json(http.MethodPost, "/v1/projects/SDLC/tasks", f.root, map[string]any{
			"title": title, "repositories": []string{backend}})
		if code != http.StatusCreated {
			t.Fatalf("заведение %s: %d %v", title, code, created)
		}
		key = created["key"].(string)
		if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
			t.Fatalf("старт %s: %d %v", key, code, body)
		}
		if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", holder, map[string]any{}); code != http.StatusOK {
			t.Fatalf("захват %s: %d %v", key, code, body)
		}
		code, issued := f.json(http.MethodPost, "/v1/tasks/"+key+"/task-token", holder, nil)
		if code != http.StatusCreated {
			t.Fatalf("токен задачи %s: %d %v", key, code, issued)
		}
		return key, issued["token"].(string)
	}

	for _, holder := range []struct{ name, token string }{{"maintainer", lead}, {"agent", f.bot}} {
		key, tt := own("Держит "+holder.name, holder.token)

		// Вне области своей задачи — scope_expansion_forbidden, задачи нет.
		code, body := f.json(http.MethodPost, "/v1/projects/SDLC/tasks", tt, map[string]any{
			"title": "Ребёнок вне области", "parent_key": key, "repositories": []string{docs}})
		if code != http.StatusConflict || errCode(body) != "scope_expansion_forbidden" {
			t.Fatalf("держатель %s, ребёнок вне области: %d %v", holder.name, code, body)
		}
		_, card := f.json(http.MethodGet, "/v1/tasks/"+key, f.root, nil)
		if children, _ := card["children"].([]any); len(children) != 0 {
			t.Fatalf("держатель %s: отказ завёл ребёнка: %v", holder.name, children)
		}

		// В области — 201 при держателе любого уровня.
		if code, body := f.json(http.MethodPost, "/v1/projects/SDLC/tasks", tt, map[string]any{
			"title": "Ребёнок в области", "parent_key": key, "repositories": []string{"backend"}}); code != http.StatusCreated {
			t.Fatalf("держатель %s, ребёнок в области: %d %v", holder.name, code, body)
		}
	}
}
