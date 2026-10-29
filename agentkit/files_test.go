package agentkit

import (
	"strings"
	"testing"
)

// CLAB-298 (AC-32.2): скилы clab-work и clab-guide рассказывают про файлы:
// где вложения, как скачать в свой каталог и открыть своим инструментом, как
// приложить и сдать файл, что содержимое идёт мимо MCP и что долгую
// загрузку ведёт обёртка heartbeat. Формы команд — те же, что называют
// описания тулов MCP (CLAB-297), менять их можно только вместе с ними.
func TestKitDescribesFiles(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	flat := map[string]string{}
	for _, f := range k.Files {
		// Без переносов строк: фразу рвёт перенос абзаца.
		flat[f.Path] = strings.Join(strings.Fields(f.Content), " ")
	}
	common := []string{
		"content does not go through MCP",
		"-o <file>",
		"`-o -`",
		"clab attach <key>",
		"--supersedes",
		"clab submit",
		"--file shot.png",
		"format: binary",
		"clab heartbeat <key> --with-credentials -- clab attach <key> big.bin",
		"clab show",
		"clab claim",
		"attachments",
		"sha256",
		"`clab transition --artifact` takes text only",
		"the server does not support attachments",
	}
	for _, path := range []string{"skills/clab-work/SKILL.md", "skills/clab-guide/SKILL.md"} {
		body := flat[path]
		for _, s := range common {
			if !strings.Contains(body, s) {
				t.Errorf("%s: нет %q", path, s)
			}
		}
	}
	if !strings.Contains(flat["skills/clab-work/SKILL.md"], "clab artifact <id> -o <your directory>/") {
		t.Error("clab-work: нет команды скачивания в свой каталог")
	}
	if !strings.Contains(flat["skills/clab-guide/SKILL.md"], "clab artifact <id> -o <directory>/") {
		t.Error("clab-guide: нет команды скачивания в каталог")
	}
	// Саб-агент исполнителя ради файлов не менялся: префикс токена задачи и
	// правила хука те же.
	if strings.Contains(flat["agents/clab-stage.md"], "clab attach") {
		t.Error("clab-stage менять ради файлов не нужно")
	}
}
