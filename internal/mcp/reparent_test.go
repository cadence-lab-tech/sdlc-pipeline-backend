package mcp

import (
	"context"
	"strings"
	"testing"
)

// CLAB-196, AC-11: описания тулов называют перенос и правило одного уровня —
// агент выбирает тул по ним, не читая документацию.
func TestReparentToolDescriptions(t *testing.T) {
	cs := connectPrompts(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool.Description
	}
	for name, wants := range map[string][]string{
		"sdlc_update_task":     {"родитель — sdlc_set_task_parent"},
		"sdlc_create_task":     {"Вложенность — один уровень", "parent — только задача без родителя"},
		"sdlc_set_task_parent": {"пусто — задача уходит на верхний уровень", "один уровень", "actor_rights.reparent", "human_action", "clab edit --parent", "PUT /v1/tasks/<ключ>/parent"},
	} {
		for _, want := range wants {
			if !strings.Contains(byName[name], want) {
				t.Errorf("%s: в описании нет %q", name, want)
			}
		}
	}
	if strings.Contains(byName["sdlc_update_task"], "у каждого своя") {
		t.Error("sdlc_update_task: прежнее «у каждого своя» без имени тула")
	}
}
