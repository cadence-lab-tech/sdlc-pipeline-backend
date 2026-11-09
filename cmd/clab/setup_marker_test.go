package main

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-424: clab setup пишет блок привязки в каталог запуска.

func blockFor(c *Client, project string) string {
	return strings.Join(markerBlockLines(project, c.origin), "\n") + "\n"
}

func sum(t *testing.T, path string) [32]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(raw)
}

// AC-11: в пустом каталоге — AGENTS.md из четырёх строк; блок читается
// обратно тем же разбором, что у clab.
func TestSetupMarkerCreatesAgentsMD(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "AGENTS.md")
	if got := readText(t, path); got != blockFor(c, "ACME") {
		t.Fatalf("AGENTS.md:\n%s", got)
	}
	lines := markerBlockLines("ACME", c.origin)
	if len(lines) != 4 || lines[0] != markerBegin || lines[3] != markerEnd ||
		!strings.HasPrefix(lines[2], "Cadence Lab: this directory works with project ACME at "+c.origin) {
		t.Fatalf("блок: %q", lines)
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); err == nil {
		t.Fatal("создан и CLAUDE.md")
	}
	m := mustFind(t, root)
	if m == nil || m.Project != "ACME" || m.Origin != c.origin {
		t.Fatalf("блок не читается: %+v", m)
	}
	if !strings.Contains(out, "создан") || !strings.Contains(out, path) {
		t.Fatalf("вывод:\n%s", out)
	}
}

// AC-11, AC-12: блок в каждый существующий файл, текст вне блока байт в
// байт (CRLF, без завершающего перевода строки); повтор — без изменений;
// другой проект — меняется только блок.
func TestSetupMarkerExistingFiles(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)
	agents := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), "# Agents\r\n\r\nправила\r\n")
	claude := writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), "# Claude\n\nбез перевода строки в конце")

	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	crlf := strings.ReplaceAll(blockFor(c, "ACME"), "\n", "\r\n")
	if got := readText(t, agents); got != "# Agents\r\n\r\nправила\r\n\r\n"+crlf {
		t.Fatalf("AGENTS.md (CRLF):\n%q", got)
	}
	if got := readText(t, claude); got != "# Claude\n\nбез перевода строки в конце\n\n"+blockFor(c, "ACME") {
		t.Fatalf("CLAUDE.md:\n%q", got)
	}

	sa, sc := sum(t, agents), sum(t, claude)
	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil || sum(t, agents) != sa || sum(t, claude) != sc || !strings.Contains(out, "без изменений") {
		t.Fatalf("повтор изменил файлы:\n%s %v", out, err)
	}

	// Текст после блока, другой проект: меняется только блок.
	writeMarkerTestFile(t, claude, readText(t, claude)+"\nхвост после блока\n")
	if _, _, err := runSetup(t, c, "--project", "ZED"); err != nil {
		t.Fatal(err)
	}
	want := "# Claude\n\nбез перевода строки в конце\n\n" + blockFor(c, "ZED") + "\nхвост после блока\n"
	if got := readText(t, claude); got != want {
		t.Fatalf("CLAUDE.md после смены проекта:\n%q\nвместо\n%q", got, want)
	}
	if got := readText(t, agents); got != "# Agents\r\n\r\nправила\r\n\r\n"+strings.ReplaceAll(blockFor(c, "ZED"), "\n", "\r\n") {
		t.Fatalf("AGENTS.md после смены проекта:\n%q", got)
	}
}

// AC-13: --dry-run печатает разницу и ничего не пишет; --marker-file — только
// в названные; неверное имя — ошибка до записи; --no-marker — не пишет.
func TestSetupMarkerFlags(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)
	claude := writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), "# Claude\n")

	out, _, err := runSetup(t, c, "--project", "ACME", "--dry-run")
	if err != nil || sum(t, claude) != sha256.Sum256([]byte("# Claude\n")) ||
		!strings.Contains(out, "обновить") || !strings.Contains(out, "+ <!-- cadence-lab: project=ACME host="+c.origin+" -->") {
		t.Fatalf("--dry-run:\n%s %v", out, err)
	}

	if _, _, err := runSetup(t, c, "--project", "ACME", "--marker-file", "README.md"); err == nil || !strings.Contains(err.Error(), "--marker-file") {
		t.Fatalf("неверное имя: %v", err)
	}
	if len(fc.calls(t)) != 0 || readText(t, claude) != "# Claude\n" {
		t.Fatal("после неверного имени что-то сделано")
	}

	if _, _, err := runSetup(t, c, "--project", "ACME", "--marker-file", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	if readText(t, claude) != "# Claude\n" || readText(t, filepath.Join(root, "AGENTS.md")) != blockFor(c, "ACME") {
		t.Fatal("--marker-file AGENTS.md")
	}
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSetup(t, c, "--project", "ACME", "--no-marker"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err == nil || readText(t, claude) != "# Claude\n" {
		t.Fatal("--no-marker записал блок")
	}
	if _, _, err := runSetup(t, c, "--project", "ACME", "--no-marker", "--marker-file", "AGENTS.md"); err == nil {
		t.Fatal("--no-marker вместе с --marker-file")
	}

	// --json: действия по файлам блока.
	withJSON(t, true)
	out, _, err = runSetup(t, c, "--project", "ACME", "--marker-file", "CLAUDE.md", "--marker-file", "AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Marker []markerAction `json:"marker"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Marker) != 2 ||
		rep.Marker[0].Action != markerUpdate || rep.Marker[1].Action != markerCreate {
		t.Fatalf("--json: %s", out)
	}
}

// AC-14: маркер вне блока и блок без end — файл не меняется, предупреждение
// с путём, код 0, остальная работа setup сделана.
func TestSetupMarkerSkipsHandWritten(t *testing.T) {
	withJSON(t, false)
	for name, body := range map[string]string{
		"маркер руками": "# A\n<!-- cadence-lab: project=OWN -->\n",
		"нет end":       "# A\n<!-- cadence-lab:begin -->\n<!-- cadence-lab: project=OWN -->\n",
		"end без begin": "# A\n<!-- cadence-lab:end -->\n",
		"маркер за end": "<!-- cadence-lab:begin -->\n<!-- cadence-lab:end -->\n<!-- cadence-lab: project=OWN -->\n",
		"код не закрыт": "# A\n```\nпример\n",
		"два блока":     "<!-- cadence-lab:begin -->\n<!-- cadence-lab:end -->\n<!-- cadence-lab:begin -->\n<!-- cadence-lab:end -->\n",
	} {
		root := withRoot(t)
		fc := installClaude(t, "")
		c := (&setupServer{kit: kitV1}).start(t)
		path := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), body)
		out, _, err := runSetup(t, c, "--project", "ACME")
		if err != nil || readText(t, path) != body || !strings.Contains(out, "пропущен") || !strings.Contains(out, path) {
			t.Fatalf("%s:\n%s %v", name, out, err)
		}
		if !fc.called(t, "mcp add ") {
			t.Fatalf("%s: остальная работа не сделана", name)
		}
	}
}

// AC-15: в файле ни токена клиента, ни путей конфига; git не вызывается.
func TestSetupMarkerNoSecretsNoGit(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	fc := installClaude(t, "")
	// Подставной git в PATH: любой вызов попадёт в журнал.
	gitLog := filepath.Join(fc.dir, "git.log")
	writeMarkerTestFile(t, filepath.Join(fc.dir, "git"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+gitLog+"\nexit 1\n")
	if err := os.Chmod(filepath.Join(fc.dir, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := (&setupServer{kit: kitV1}).start(t)
	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	got := readText(t, filepath.Join(root, "AGENTS.md"))
	for _, bad := range []string{setupToken, "config.json", ".config/clab", "claude-", "opus", "sonnet"} {
		if strings.Contains(got, bad) {
			t.Fatalf("в блоке %q:\n%s", bad, got)
		}
	}
	if raw, err := os.ReadFile(gitLog); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "add") || strings.HasPrefix(line, "commit") {
				t.Fatalf("setup позвал git %s", line)
			}
		}
	}
}

// AC-16: AGENTS.md — ссылка на CLAUDE.md: блок один раз в целевом файле,
// ссылка осталась ссылкой, права файла прежние.
func TestSetupMarkerSymlinkAndMode(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)
	claude := writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), "# Claude\n")
	if err := os.Chmod(claude, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(root, "AGENTS.md"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("ссылка перестала быть ссылкой")
	}
	got := readText(t, claude)
	if strings.Count(got, markerBegin) != 1 || got != "# Claude\n\n"+blockFor(c, "ACME") {
		t.Fatalf("CLAUDE.md:\n%s", got)
	}
	if st, _ := os.Stat(claude); st.Mode().Perm() != 0o600 {
		t.Fatalf("права: %v", st.Mode().Perm())
	}
	// Временных файлов рядом не осталось.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".clab-") {
			t.Fatalf("остался %s", e.Name())
		}
	}
}

// UC-8: --agent other тоже пишет блок; в каталоге с маркером setup без
// --project берёт проект из него и ничего не меняет.
func TestSetupMarkerOtherAgentAndRerunFromMarker(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	c := (&setupServer{kit: kitV1}).start(t)
	if _, _, err := runSetup(t, c, "--project", "ACME", "--agent", "other"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "AGENTS.md")
	if readText(t, path) != blockFor(c, "ACME") {
		t.Fatal("--agent other не записал блок")
	}
	c.markerLoad = loadDirMarker
	before := sum(t, path)
	out, _, err := runSetup(t, c, "--agent", "other")
	if err != nil || sum(t, path) != before || !strings.Contains(out, "ACME") {
		t.Fatalf("повтор по маркеру:\n%s %v", out, err)
	}
}
