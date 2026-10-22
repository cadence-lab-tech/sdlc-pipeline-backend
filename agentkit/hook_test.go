package agentkit

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// hookCommand достаёт из фронтматтера agents/clab-stage.md тело хука —
// блок `command: |` — ровно тем текстом, что запустит Claude Code.
func hookCommand(t *testing.T) string {
	t.Helper()
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	var content string
	for _, f := range k.Files {
		if f.Path == "agents/clab-stage.md" {
			content = f.Content
		}
	}
	lines := strings.Split(content, "\n")
	var body []string
	indent := -1
	for _, l := range lines {
		depth := len(l) - len(strings.TrimLeft(l, " "))
		if indent < 0 {
			if strings.TrimSpace(l) == "command: |" {
				indent = depth + 2
			}
			continue
		}
		if l == "---" || (strings.TrimSpace(l) != "" && depth < indent) {
			break
		}
		body = append(body, strings.TrimPrefix(l, strings.Repeat(" ", indent)))
	}
	if len(body) == 0 {
		t.Fatal("в agents/clab-stage.md нет блока command: | у хука")
	}
	return strings.Join(body, "\n")
}

// runHook подаёт хуку JSON тула Bash так, как его шлёт Claude Code, и
// возвращает код выхода и stderr.
func runHook(t *testing.T, script, command string) (int, string) {
	t.Helper()
	// Claude Code сериализует как JSON.stringify: & < > не экранируются.
	var in bytes.Buffer
	enc := json.NewEncoder(&in)
	enc.SetEscapeHTML(false)
	err := enc.Encode(map[string]any{
		"session_id":      "s",
		"cwd":             "/home/u/project",
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]string{"command": command, "description": "Run it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = bytes.NewReader(bytes.TrimSuffix(in.Bytes(), []byte("\n")))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), stderr.String()
	}
	t.Fatalf("хук не запустился: %v", err)
	return 0, ""
}

const hookPrefix = "env -u SDLC_TOKEN -u SDLC_API_URL CLAB_CONFIG_DIR=/d "

// Вызовы clab, clab пакета и скриптов пакета без каталога токена задачи
// (CLAB-249, AC-1): хук отклоняет их кодом 2 и называет префикс.
var hookCaught = []string{
	"bin/clab-linux-amd64 version",
	"./bin/clab-darwin-arm64 server status",
	"/opt/cadence/bin/clab-linux-arm64 server backup",
	"$dir/bin/clab-linux-amd64 version",
	"cd /srv/cadence && bin/clab-linux-amd64 version",
	"FOO=1 bin/clab-linux-amd64 version",
	"env bin/clab-linux-amd64 version",
	"./install.sh",
	"/srv/cadence/upgrade.sh v0.14.0",
	"cd x; ./backup.sh",
	"sh ./restore.sh --yes f",
	"bash get-cadence.sh",
	"sh get-cadence.sh --version v0.13.0",
	"sudo ./install.sh",
	"sudo -E bin/clab-linux-amd64 server status",
	"sudo -u root ./upgrade.sh v0.14.0",
	"sudo clab whoami",
	"cd /srv/cadence\n./upgrade.sh v0.14.0",
	"ls\n\tbin/clab-linux-amd64 version",
	"(cd /srv && ./restore.sh --yes f)",
	"x=$(clab whoami)",
	"echo ok | clab whoami",
	"clab whoami",
	"SDLC_PROJECT=X clab show X-1",
	"/opt/homebrew/bin/clab version",
	"command clab version",
	"time ./backup.sh",
	"nohup bin/clab-linux-amd64 server upgrade v0.14.0 &",
}

// Имя скрипта или бинаря как аргумент чужой команды, путь и ветка — не
// вызов (CLAB-249, AC-3).
var hookPassed = []string{
	"cat install.sh",
	"sed -n 1,20p upgrade.sh",
	"grep -n exec backup.sh",
	"shellcheck install.sh upgrade.sh",
	"git diff origin/main -- install.sh",
	"git log --oneline -- restore.sh",
	"ls bin/clab-linux-amd64",
	"chmod +x bin/clab-linux-amd64",
	"file bin/clab-darwin-arm64",
	"cp bin/clab-linux-amd64 /tmp/x",
	"git checkout clab/CLAB-202-install.sh",
	"git push origin clab/CLAB-202-airgap",
	"git -C .worktrees/CLAB-202/self-hosted status",
	"go test ./cmd/clab/...",
	"ls .claude/agents/clab-stage.md",
	"ls /tmp/clab-linux-amd64/",
	"docker logs clab-registry",
	"ci/package.sh --clab dist",
	"sh ci/check-config.sh",
}

func TestHookCatchesPackageClab(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("нет sh — хук не запустить")
	}
	script := hookCommand(t)
	for _, c := range hookCaught {
		code, msg := runHook(t, script, c)
		if code != 2 || !strings.Contains(msg, "clab without the task-token directory") ||
			!strings.Contains(msg, "CLAB_CONFIG_DIR=") {
			t.Errorf("%q без префикса: код %d, %q — ждали отказ 2", c, code, msg)
		}
		// AC-2: с префиксом из промпта тот же вызов проходит.
		if code, msg := runHook(t, script, hookPrefix+c); code != 0 {
			t.Errorf("%q с префиксом: код %d, %q — ждали 0", c, code, msg)
		}
		if code, _ := runHook(t, script, "sudo "+hookPrefix+c); code != 0 && !strings.HasPrefix(c, "sudo") {
			t.Errorf("sudo %q с префиксом: код %d — ждали 0", c, code)
		}
	}
	for _, c := range hookPassed {
		if code, msg := runHook(t, script, c); code != 0 {
			t.Errorf("%q: код %d, %q — ложное срабатывание", c, code, msg)
		}
	}
}

// Известное и оставленное поведение (CLAB-249, AC-4; решение q7): хук — страховка,
// а не разбор shell. sh с флагами считается запуском, даже `sh -n`; `|`
// внутри кавычек считается разделителем команд.
func TestHookKnownFalsePositives(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("нет sh — хук не запустить")
	}
	script := hookCommand(t)
	for _, c := range []string{
		"sh -n install.sh",
		`grep -e 'a\|clab setup' file`,
	} {
		if code, _ := runHook(t, script, c); code != 2 {
			t.Errorf("%q: код %d — поведение изменилось, поправьте комментарий шаблона и тест", c, code)
		}
	}
}
