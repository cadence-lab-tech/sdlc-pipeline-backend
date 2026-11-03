package httpapi

import (
	"net/http"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// CLAB-391, характеризация до правки: участник уровня agent расширяет
// область задачи и по REST, и тулом MCP. Следующий коммит переворачивает
// ожидания — расширение области решает человек уровня maintainer.
func TestScopeExpansionRights(t *testing.T) {
	f := newAuditFixture(t)
	dev := f.memberToken("user:dev", domain.MemberAgent, nil)
	const docs = "cadence-lab-tech/sdlc-pipeline-docs"

	if code, body := f.json(http.MethodPatch, "/v1/tasks/SDLC-3/scope", dev,
		map[string]any{"add_repositories": []string{docs}, "note": "нужны решения"}); code != http.StatusOK {
		t.Fatalf("REST agent: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPatch, "/v1/tasks/SDLC-1/scope", dev, map[string]any{
		"add_repositories": []string{docs}, "properties": map[string]any{"multi_repo": true}}); code != http.StatusOK {
		t.Fatalf("REST agent с multi_repo: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPatch, "/v1/tasks/SDLC-2/scope", dev,
		map[string]any{"properties": map[string]any{"multi_repo": true}}); code != http.StatusOK {
		t.Fatalf("REST agent свойства: %d %v", code, body)
	}
	if out, isErr := f.mcp(dev, "sdlc_update_task_scope", map[string]any{
		"task_key": "SDLC-2", "add": []string{docs}, "reason": "нужны решения"}); isErr {
		t.Fatalf("MCP agent: %v", out)
	}
}
