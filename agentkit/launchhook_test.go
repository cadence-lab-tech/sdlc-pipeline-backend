package agentkit

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"
)

// CLAB-419, AC-9: хук, который clab exec крепит к запуску Codex, — тот же
// сценарий, что во фронтматтере clab-stage привязки claude, и он проходит
// весь корпус hook_test.go.

func launchHook(t *testing.T, k Kit, name string) bindingHook {
	t.Helper()
	var h bindingHook
	if err := json.Unmarshal(binding(t, k, name).Hook, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

type bindingHook struct {
	Placement string `json:"placement"`
	Agent     string `json:"agent"`
	Event     string `json:"event"`
	Matcher   string `json:"matcher"`
	Command   string `json:"command"`
}

func TestCodexHookIsStageHook(t *testing.T) {
	k := loadKit(t)
	h := launchHook(t, k, "codex")
	if h.Placement != "launch" || h.Agent != "clab-stage" || h.Event != "PreToolUse" || h.Matcher != "Bash" {
		t.Fatalf("хук codex: %+v", h)
	}
	if h.Command == "" || h.Command != hookCommand(t) {
		t.Fatalf("сценарий хука codex не равен хуку clab-stage:\n%s", h.Command)
	}
	if c := launchHook(t, k, "claude"); c.Command != "" {
		t.Errorf("у хука во фронтматтере command в наборе не нужен: %q", c.Command)
	}
	if !launch.Uses(binding(t, k, "codex").Run, launch.SlotHookScript) {
		t.Error("шаблон run codex не крепит хук: нет {hook_script}")
	}
}

func TestCodexHookCorpus(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("нет sh — хук не запустить")
	}
	script := launchHook(t, loadKit(t), "codex").Command
	for _, cmd := range hookCaught {
		if code, _ := runHook(t, script, cmd); code != 2 {
			t.Errorf("codex: %q без префикса — код %d, ждали 2", cmd, code)
		}
		if code, _ := runHook(t, script, hookPrefix+cmd); code != 0 {
			t.Errorf("codex: %q с префиксом — код %d, ждали 0", cmd, code)
		}
	}
	for _, cmd := range hookPassed {
		if code, _ := runHook(t, script, cmd); code != 0 {
			t.Errorf("codex: %q — код %d, ждали 0", cmd, code)
		}
	}
}

// Хук при запуске без сценария у агента, сценарий в самом файле привязки и
// {hook_script} без хука при запуске — ошибки сборки: сценарий собирается
// из фронтматтера, одной копией.
func TestLaunchHookNeedsAgentScript(t *testing.T) {
	codex := string(kitFS(t)["bindings/codex.json"].Data)
	cases := map[string]string{
		"чужой агент":     strings.Replace(codex, `"agent": "clab-stage"`, `"agent": "nobody"`, 1),
		"command в файле": strings.Replace(codex, `"agent": "clab-stage",`, `"agent": "clab-stage", "command": "exit 0",`, 1),
		"слот без агента": strings.Replace(codex, `"agent": "clab-stage",`, ``, 1),
	}
	for name, changed := range cases {
		if changed == codex {
			t.Fatalf("%s: замена не сработала", name)
		}
		m := kitFS(t)
		m["bindings/codex.json"] = &fstest.MapFile{Data: []byte(changed)}
		if _, err := build(m); err == nil {
			t.Errorf("%s: набор собрался", name)
		}
	}
}

// AC-11: ядро описывает оба способа запуска исполнителя и шаги процесса,
// описания clab-run и clab-stage нейтральны.
func TestCoreDescribesProcessExecutor(t *testing.T) {
	k := loadKit(t)
	core := filesByPath(append(append([]File{}, k.Core.Skills...), k.Core.Agents...))
	run := strings.Join(strings.Fields(core["skills/clab-run/SKILL.md"].Content), " ")
	for _, s := range []string{
		"`executor: subagent`", "`executor: process`", "### Executor as a process", "`clab exec <key> --dir",
		"--note-file <run directory>/note.md", "--dry-run", "<run directory>/report.md", "Exit code 125",
	} {
		if !strings.Contains(run, s) {
			t.Errorf("ядро clab-run: нет %q", s)
		}
	}
	work := strings.Join(strings.Fields(core["skills/clab-work/SKILL.md"].Content), " ")
	for _, s := range []string{"started by `clab exec`", "report.md"} {
		if !strings.Contains(work, s) {
			t.Errorf("ядро clab-work: нет %q", s)
		}
	}
	if d := Frontmatter(core["skills/clab-run/SKILL.md"].Content)["description"]; strings.Contains(d, "sub-agent") {
		t.Errorf("description clab-run: %q", d)
	}
	if d := Frontmatter(core["agents/clab-stage.md"].Content)["description"]; strings.Contains(d, "/clab-run") {
		t.Errorf("description clab-stage: %q", d)
	}
	codex := filesByPath(binding(t, k, "codex").Files)[".agents/skills/clab-run/SKILL.md"].Content
	if !strings.Contains(codex, "## Notes for Codex") || !strings.Contains(codex, "`executor: process`") {
		t.Error("у codex нет заметок clab-run")
	}
}
