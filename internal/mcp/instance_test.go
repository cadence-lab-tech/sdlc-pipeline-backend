package mcp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// Инстансовый адрес MCP (CLAB-483). Номера — критерии приёмки анализа.

// Имена нашей настройки, которых не должно быть в новых текстах продукта
// (AC-10, AC-16): поле project, отказ выбора проекта, описание тула.
var ourSetupNames = []string{"analysis", "development", "testing", "merge", "analytics", "designer",
	"developer", "integrator", "Simple", "Epic", "CLAB"}

// AC-10: каждый тул принимает project необязательным полем с описанием, кроме
// перечня UnscopedTools; каждый промпт — аргумент project, кроме
// UnscopedPrompts. Новый тул без project вне перечня валит тест.
func TestEveryToolTakesProject(t *testing.T) {
	cs := connectPrompts(t)
	ctx := context.Background()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		seen[tool.Name] = true
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		field, has := props[ProjectArg].(map[string]any)
		if UnscopedTools[tool.Name] {
			if has {
				t.Errorf("%s: тул вне проекта с полем project", tool.Name)
			}
			continue
		}
		if !has || field["type"] != "string" || field["description"] == "" {
			t.Errorf("%s: нет поля project со строкой и описанием: %v", tool.Name, props[ProjectArg])
		}
		for _, req := range asStrings(schema["required"]) {
			if req == ProjectArg {
				t.Errorf("%s: project обязателен", tool.Name)
			}
		}
	}
	for name := range UnscopedTools {
		if !seen[name] {
			t.Errorf("тул %s из UnscopedTools не зарегистрирован", name)
		}
	}
	if !seen[listProjectsTool] {
		t.Fatal("sdlc_list_projects не зарегистрирован")
	}
	// AC-4: заведения проектов через MCP нет.
	for name := range seen {
		if strings.Contains(name, "create_project") {
			t.Errorf("тул заведения проекта: %s", name)
		}
	}

	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prompts.Prompts {
		has := false
		for _, arg := range p.Arguments {
			if arg.Name == ProjectArg {
				has = !arg.Required && arg.Description != ""
			}
		}
		if has == UnscopedPrompts[p.Name] {
			t.Errorf("промпт %s: аргумент project %v, в перечне исключений %v", p.Name, has, UnscopedPrompts[p.Name])
		}
	}

	// Тексты выбора проекта — без имён нашей настройки.
	listDesc := ""
	for _, tool := range tools.Tools {
		if tool.Name == listProjectsTool {
			listDesc = tool.Description
		}
	}
	for _, text := range []string{projectArgDescription, missingProject, listDesc} {
		for _, name := range ourSetupNames {
			if strings.Contains(text, name) {
				t.Errorf("в тексте %q имя настройки %q", text, name)
			}
		}
	}
}

func asStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// AC-5 (д): ключ задачи в полях с другим именем — по метке mcp:"task_key".
func TestTaskKeyFields(t *testing.T) {
	names := func(fields [][]int, typ reflect.Type) []string {
		var out []string
		for _, idx := range fields {
			out = append(out, typ.FieldByIndex(idx).Name)
		}
		return out
	}
	for _, c := range []struct {
		got  [][]int
		typ  reflect.Type
		want string
	}{
		{taskKeyFields[taskKeyInput](), reflect.TypeFor[taskKeyInput](), "TaskKey"},
		{taskKeyFields[rankTaskInput](), reflect.TypeFor[rankTaskInput](), "Task"},
		{taskKeyFields[linkTasksInput](), reflect.TypeFor[linkTasksInput](), "From To"},
		{taskKeyFields[createTaskInput](), reflect.TypeFor[createTaskInput](), "Parent"},
		{taskKeyFields[listTasksInput](), reflect.TypeFor[listTasksInput](), "Parent"},
		{taskKeyFields[searchInput](), reflect.TypeFor[searchInput](), ""},
	} {
		if got := strings.Join(names(c.got, c.typ), " "); got != c.want {
			t.Errorf("%s: поля ключа %q, ждали %q", c.typ.Name(), got, c.want)
		}
	}
	if key := taskKeyOf(linkTasksInput{From: "", To: "ACME-2"}, taskKeyFields[linkTasksInput]()); key != "ACME-2" {
		t.Fatalf("первый непустой ключ: %q", key)
	}
}

// AC-5, AC-9: порядок выбора проекта и сверка аргумента на адресе проекта.
func TestSelectProject(t *testing.T) {
	s := &Server{}
	base := context.Background()
	project := func(ctx context.Context) string {
		key, _ := projectKey(ctx)
		return key
	}
	cases := []struct {
		name          string
		ctx           context.Context
		arg, task     string
		want, errCode string
		errHas        string
	}{
		{"адрес проекта без аргумента", WithProject(base, "ACME"), "", "BOLT-1", "ACME", "", ""},
		{"адрес проекта, тот же project", WithProject(base, "ACME"), "ACME", "", "ACME", "", ""},
		{"адрес проекта, другой project", WithProject(base, "ACME"), "BOLT", "", "", "invalid_input", "project"},
		{"инстанс, аргумент сильнее ключа", base, "BOLT", "ACME-1", "BOLT", "", ""},
		{"инстанс, ключ задачи", base, "", "MY-PROJ-12", "MY-PROJ", "", ""},
		{"инстанс, ключ не в формате", base, "", "ACME", "", "invalid_input", "PROJECT-42"},
		{"инстанс без проекта", base, "", "", "", "invalid_input", "sdlc_list_projects"},
	}
	for _, c := range cases {
		ctx, err := s.selectProject(c.ctx, c.arg, c.task)
		if c.errCode != "" {
			if err == nil || string(apperr.As(err).Code) != c.errCode || !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("%s: ошибка %v", c.name, err)
			}
			continue
		}
		if err != nil || project(ctx) != c.want {
			t.Errorf("%s: проект %q, ошибка %v", c.name, project(ctx), err)
		}
	}
}

// R8: import_rules на инстансовом адресе называет проект из аргумента; без
// него — проект этого сервера, как раньше.
func TestImportRulesProjectArg(t *testing.T) {
	cs := connectPrompts(t)
	if text := promptText(t, cs, "import_rules", map[string]string{ProjectArg: "ACME"}); !strings.Contains(text, "в проект ACME") {
		t.Fatalf("project не назван: %q", text[:80])
	}
	if text := promptText(t, cs, "import_rules", nil); !strings.Contains(text, "в проект этого сервера MCP") {
		t.Fatalf("без project: %q", text[:80])
	}
}
