package httpapi

import (
	"net/http"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// CLAB-390: кэш projectOf в запросе и снимок чтения не открывают карточку
// тому, кому проект закрыт: чужой проект, несуществующая задача и запрос без
// токена отвечают как раньше (режим token).
func TestCardRightsUnchangedBySnapshot(t *testing.T) {
	f := newAuditFixture(t)
	viewer := f.memberToken("user:watcher", domain.MemberViewer, nil)
	if rec := f.do(http.MethodPost, "/v1/projects", f.root, map[string]any{"key": "ACME", "name": "Acme"}); rec.Code != http.StatusCreated {
		t.Fatalf("проект: %d %s", rec.Code, rec.Body)
	}
	bundle := map[string]any{
		"version": 1,
		"roles":   []any{map[string]any{"key": "builder", "name": "Сборщик", "instructions": "собирать"}},
		"workflows": []any{map[string]any{
			"name": "Свой", "task_types": []string{"task"},
			"statuses": []any{
				map[string]any{"key": "inbox", "name": "Входящие", "category": "backlog", "is_initial": true},
				map[string]any{"key": "build", "name": "Сборка", "category": "active", "role": "builder"},
				map[string]any{"key": "shipped", "name": "Отгружено", "category": "done", "is_terminal": true},
			},
			"transitions": []any{
				map[string]any{"key": "go", "name": "go", "from": "inbox", "to": "build", "kind": "advance"},
				map[string]any{"key": "ship", "name": "ship", "from": "build", "to": "shipped", "kind": "advance"},
			},
		}},
	}
	code, report := f.json(http.MethodPost, "/v1/projects/ACME/import", f.root, bundle)
	if code >= 300 {
		t.Fatalf("связка: %d %v", code, report)
	}
	entry := report["workflows"].([]any)[0].(map[string]any)
	if code, body := f.json(http.MethodPost, "/v1/workflows/"+entry["id"].(string)+"/activate", f.root, nil); code >= 300 {
		t.Fatalf("активация: %d %v", code, body)
	}
	code, created := f.json(http.MethodPost, "/v1/projects/ACME/tasks", f.root, map[string]any{"title": "Чужая"})
	foreign, _ := created["key"].(string)
	if code >= 300 || foreign == "" {
		t.Fatalf("задача чужого проекта: %d %v", code, created)
	}

	cases := []struct {
		name, key, token string
		want             int
	}{
		{"viewer: своя задача", "SDLC-1", viewer, http.StatusOK},
		{"viewer: задача чужого проекта", foreign, viewer, http.StatusForbidden},
		{"viewer: нет такой задачи", "SDLC-9999", viewer, http.StatusNotFound},
		{"без токена", "SDLC-1", "", http.StatusUnauthorized},
		{"сопровождающий: чужой проект", foreign, f.root, http.StatusOK},
		{"agent без участия в ACME", foreign, f.bot, http.StatusForbidden},
	}
	for _, c := range cases {
		if got, body := f.json(http.MethodGet, "/v1/tasks/"+c.key, c.token, nil); got != c.want {
			t.Errorf("%s: код %d, ждали %d (%v)", c.name, got, c.want, body)
		}
	}
}
