package agentkit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CLAB-405 (решение 0076, эпик CLAB-404, AC-24.7): набор знает «уровни —
// людям, роли — агентам». Сотрудник (человек уровня agent) решает гейты
// любого этапа и переводит задачу в статус clab move, оркестратору хватает
// уровня agent, бот остаётся при роли этапа, токен задачи — при своей рамке.
// Удалённый абзац роняет тест, прежние формулировки права этапа — тоже.
func TestKitLevelsForPeople(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	flat := map[string]string{}
	for _, f := range k.Files {
		flat[f.Path] = strings.Join(strings.Fields(f.Content), " ")
	}
	want := map[string][]string{
		"skills/clab-guide/SKILL.md": {
			"Levels are for people, roles are for agents", "**An employee**", "**A bot**",
			"any transition of the cycle", "decides the gates of any stage",
			"clab move <key> --to <status> --reason", "`sdlc_move_task`", "`sdlc_decide_gate`",
			"`stage_disabled`", "server v0.25.0 or newer", "`actor_rights`",
			"**Merging is work, not a status.**", "no moves to a status",
			"at any level of the human who issued the token",
		},
		"skills/clab-run/SKILL.md": {
			"**Which level the orchestrator needs.** Level `agent` is enough",
			"`integrator`", "decides the gates of any stage",
			"never a substitute for an approval", "moving the task to a status",
		},
		"skills/clab-status/SKILL.md": {
			"## Moving a task to a status", `clab move ABC-12 --to <status> --reason "…"`,
			"`actor_rights.move`", "Gates are not checked on a move", "`task.moved`",
			"`stage_disabled`", "clab edit ABC-12 --stage <status>=on", "v0.25.0 or newer",
			"`sdlc_move_task`", "`sdlc_decide_gate`", "decides the gates of any stage",
		},
		"skills/clab-work/SKILL.md": {
			"at level `agent` the human is an employee", "moving a task to a status",
			"at any level of the human who holds the lease",
			`clab move <key> --to <status> --reason "…"`, "A stage executor never moves a task",
			"an employee at level `agent` on any stage",
		},
	}
	for path, needles := range want {
		body, ok := flat[path]
		if !ok {
			t.Fatalf("в наборе нет %s", path)
		}
		for _, s := range needles {
			if !strings.Contains(body, s) {
				t.Errorf("%s: нет %q", path, s)
			}
		}
	}
	// Формулировки 0072 о людях, которые после 0076 неверны.
	outdated := []string{
		"whose roles include the role of the task's current status, or a maintainer",
		"with the role of the status, or a maintainer",
		"the member who works the stage's role",
		"whose roles include the role of the task's status, or a maintainer",
		"works with level `maintainer`", "needs `maintainer` for start",
	}
	for path, body := range flat {
		for _, s := range outdated {
			if strings.Contains(body, s) {
				t.Errorf("%s: устаревшая формулировка %q", path, s)
			}
		}
	}
}

// CLAB-405 (AC-24.7): каждый тул sdlc_* и каждая команда clab, которые
// называет набор, существуют — тулы зарегистрированы в internal/mcp, команды
// стоят в таблице cmd/clab/main.go. Набор не обещает агенту того, чего нет.
func TestKitNamesExist(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]bool{}
	files, err := filepath.Glob("../internal/mcp/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("исходники MCP не найдены: %v", err)
	}
	reg := regexp.MustCompile(`add\(s, "(sdlc_[a-z_]+)"`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reg.FindAllStringSubmatch(string(b), -1) {
			tools[m[1]] = true
		}
	}
	main, err := os.ReadFile("../cmd/clab/main.go")
	if err != nil {
		t.Fatal(err)
	}
	commands := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\t\t\{"([a-z-]+)", "`).FindAllStringSubmatch(string(main), -1) {
		commands[m[1]] = true
	}
	if !commands["move"] || !commands["decide"] || !tools["sdlc_move_task"] || !tools["sdlc_decide_gate"] {
		t.Fatalf("разбор исходников не нашёл move и decide: команд %d, тулов %d", len(commands), len(tools))
	}
	toolRef := regexp.MustCompile("`(sdlc_[a-z_]+)`")
	cmdRef := regexp.MustCompile("`clab ([a-z][a-z-]*)")
	for _, f := range k.Files {
		for _, m := range toolRef.FindAllStringSubmatch(f.Content, -1) {
			if !tools[m[1]] {
				t.Errorf("%s: тула %s нет в internal/mcp", f.Path, m[1])
			}
		}
		for _, m := range cmdRef.FindAllStringSubmatch(f.Content, -1) {
			if !commands[m[1]] {
				t.Errorf("%s: команды clab %s нет в cmd/clab", f.Path, m[1])
			}
		}
	}
}
