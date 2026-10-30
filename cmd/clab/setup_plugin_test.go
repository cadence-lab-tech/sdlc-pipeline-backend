package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/clabplugin"
)

// CLAB-329: clab setup --plugin. claude — подставной скрипт, который пишет
// свои аргументы в журнал и помнит подключённый маркетплейс и установленную
// версию плагина в файлах состояния — как настоящий claude plugin … --json.
// Клиент API — nil: режим «только плагин» к серверу не ходит вовсе (AC-6).

type pluginClaude struct{ dir, log, market, plugin, enabled string }

func installPluginClaude(t *testing.T) *pluginClaude {
	t.Helper()
	d := t.TempDir()
	pc := &pluginClaude{
		dir: d, log: filepath.Join(d, "log"), market: filepath.Join(d, "market"),
		plugin: filepath.Join(d, "plugin"), enabled: filepath.Join(d, "enabled"),
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + pc.log + `
version_of() {
  sed -n 's/.*"version": "\(.*\)".*/\1/p' "$(cat ` + pc.market + `)/clab-plugin/.claude-plugin/plugin.json"
}
case "$1 $2 $3" in
"plugin marketplace list")
  if [ -f ` + pc.market + ` ]; then
    printf '[{"name":"cadence-lab","source":"directory","path":"%s","installLocation":"%s"}]\n' "$(cat ` + pc.market + `)" "$(cat ` + pc.market + `)"
  else
    echo '[]'
  fi;;
"plugin marketplace add") printf '%s' "$4" > ` + pc.market + `;;
"plugin marketplace remove") rm -f ` + pc.market + `;;
"plugin marketplace update") ;;
"plugin list --json")
  if [ -f ` + pc.plugin + ` ]; then
    printf '[{"id":"clab-plugin@cadence-lab","version":"%s","scope":"user","enabled":%s}]\n' "$(cat ` + pc.plugin + `)" "$(cat ` + pc.enabled + ` 2>/dev/null || echo true)"
  else
    echo '[]'
  fi;;
"plugin install clab-plugin@cadence-lab"|"plugin update clab-plugin@cadence-lab") version_of > ` + pc.plugin + `;;
*) echo "unexpected: $*" >&2; exit 2;;
esac
`
	if err := os.WriteFile(filepath.Join(d, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d+":/usr/bin:/bin")
	return pc
}

func (pc *pluginClaude) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(pc.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func (pc *pluginClaude) called(t *testing.T, prefix string) bool {
	for _, c := range pc.calls(t) {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (pc *pluginClaude) reset(t *testing.T) {
	t.Helper()
	if err := os.Remove(pc.log); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// withPluginEnv — каталог данных clab во временном каталоге, пустой конфиг
// clab (входа нет) и версия сборки.
func withPluginEnv(t *testing.T, v string) string {
	t.Helper()
	data := t.TempDir()
	t.Setenv("CLAB_DATA_DIR", data)
	t.Setenv("CLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("SDLC_API_URL", "")
	t.Setenv("SDLC_TOKEN", "")
	withVersion(t, v)
	return filepath.Join(data, "plugin-marketplace")
}

func runPlugin(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return capture(t, func() error { return cmdSetup(context.Background(), nil, append([]string{"--plugin"}, args...)) })
}

func treeSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// AC-1, AC-7: установка — каталог в данных clab, маркетплейс, плагин в
// области user той же версии, что clab; вывод — путь, версия, новая сессия.
func TestSetupPluginInstall(t *testing.T) {
	dir := withPluginEnv(t, "v0.42.0")
	t.Setenv("SDLC_TOKEN", "sdlc_personal-secret-77")
	pc := installPluginClaude(t)

	out, _, err := runPlugin(t)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.called(t, "plugin marketplace add "+dir) || !pc.called(t, "plugin install clab-plugin@cadence-lab --scope user") {
		t.Fatalf("вызовы claude: %v", pc.calls(t))
	}
	if got, _ := os.ReadFile(pc.plugin); string(got) != "0.42.0\n" {
		t.Errorf("установлена версия %q, want 0.42.0", got)
	}
	for _, want := range []string{dir, "clab-plugin@cadence-lab 0.42.0", "новую сессию Claude", "/clab-plugin", "clab auth login"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "clab-plugin", ".claude-plugin", "plugin.json"))
	if err != nil || !strings.Contains(string(manifest), `"version": "0.42.0"`) {
		t.Errorf("plugin.json: %v\n%s", err, manifest)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "marketplace.json")); err != nil {
		t.Error(err)
	}
	// Токенов в каталоге нет (AC-7).
	for p := range treeSums(t, dir) {
		raw, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if strings.Contains(string(raw), "sdlc_personal-secret-77") {
			t.Errorf("%s содержит токен", p)
		}
	}
	// Ровно вшитые файлы и манифест маркетплейса.
	files, _ := clabplugin.Files("v0.42.0")
	if got := len(treeSums(t, dir)); got != len(files)+1 {
		t.Errorf("файлов в каталоге %d, want %d", got, len(files)+1)
	}
}

// AC-2: повтор той же версии — «без изменений», файлы не тронуты, claude не
// ставит и не обновляет.
func TestSetupPluginIdempotent(t *testing.T) {
	dir := withPluginEnv(t, "v0.42.0")
	pc := installPluginClaude(t)
	if _, _, err := runPlugin(t); err != nil {
		t.Fatal(err)
	}
	before := treeSums(t, dir)
	stat, _ := os.Stat(filepath.Join(dir, "clab-plugin", "hooks", "register.tsx"))
	pc.reset(t)

	out, _, err := runPlugin(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Без изменений") {
		t.Errorf("нет «Без изменений»:\n%s", out)
	}
	after := treeSums(t, dir)
	if len(after) != len(before) {
		t.Fatalf("набор файлов изменился: %v → %v", before, after)
	}
	for p, s := range before {
		if after[p] != s {
			t.Errorf("%s изменился", p)
		}
	}
	if again, _ := os.Stat(filepath.Join(dir, "clab-plugin", "hooks", "register.tsx")); !again.ModTime().Equal(stat.ModTime()) {
		t.Error("файл переписан, хотя не менялся")
	}
	for _, c := range pc.calls(t) {
		if strings.Contains(c, "install") || strings.Contains(c, "update") || strings.Contains(c, "marketplace add") {
			t.Errorf("лишний вызов claude: %s", c)
		}
	}
}

// AC-3: другая версия clab — каталог ровно новый, marketplace update и update,
// просьба перезапустить; плагин, удалённый из Claude, ставится заново.
func TestSetupPluginUpgrade(t *testing.T) {
	dir := withPluginEnv(t, "v0.42.0")
	pc := installPluginClaude(t)
	if _, _, err := runPlugin(t); err != nil {
		t.Fatal(err)
	}
	// Лишний файл и правка руками в каталоге-маркетплейсе не сохраняются.
	if err := os.WriteFile(filepath.Join(dir, "clab-plugin", "hooks", "extra.ts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clab-plugin", "types", "index.d.ts"), []byte("hand edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc.reset(t)

	withVersion(t, "v0.43.0")
	out, _, err := runPlugin(t)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.called(t, "plugin marketplace update cadence-lab") || !pc.called(t, "plugin update clab-plugin@cadence-lab --scope user") {
		t.Errorf("вызовы claude: %v", pc.calls(t))
	}
	if pc.called(t, "plugin install") || pc.called(t, "plugin marketplace add") {
		t.Errorf("лишние вызовы: %v", pc.calls(t))
	}
	if !strings.Contains(out, "обновлён с 0.42.0 до 0.43.0") || !strings.Contains(out, "Перезапустите Claude") {
		t.Errorf("вывод:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "clab-plugin", "hooks", "extra.ts")); !errors.Is(err, os.ErrNotExist) {
		t.Error("лишний файл остался")
	}
	files, _ := pluginFiles("v0.43.0")
	sums := treeSums(t, dir)
	if len(sums) != len(files) {
		t.Errorf("файлов %d, want %d", len(sums), len(files))
	}
	for _, f := range files {
		s := sha256.Sum256(f.Content)
		if sums[f.Path] != hex.EncodeToString(s[:]) {
			t.Errorf("%s не совпадает с вшитым", f.Path)
		}
	}
	if got, _ := os.ReadFile(pc.plugin); string(got) != "0.43.0\n" {
		t.Errorf("версия в Claude %q", got)
	}

	// Плагин сняли в Claude, каталог остался — ставится заново.
	if err := os.Remove(pc.plugin); err != nil {
		t.Fatal(err)
	}
	pc.reset(t)
	if _, _, err := runPlugin(t); err != nil {
		t.Fatal(err)
	}
	if !pc.called(t, "plugin install clab-plugin@cadence-lab --scope user") {
		t.Errorf("не переустановлен: %v", pc.calls(t))
	}
}

// Сборка без версии: семвер dev с хешем мода, тот же путь установки.
func TestSetupPluginDevVersion(t *testing.T) {
	dir := withPluginEnv(t, "dev")
	installPluginClaude(t)
	out, _, err := runPlugin(t)
	if err != nil {
		t.Fatal(err)
	}
	want := "0.0.0-dev." + clabplugin.Digest()[:8]
	manifest, _ := os.ReadFile(filepath.Join(dir, "clab-plugin", ".claude-plugin", "plugin.json"))
	if !strings.Contains(string(manifest), `"version": "`+want+`"`) || !strings.Contains(out, want) {
		t.Errorf("версия dev %s:\n%s\n%s", want, manifest, out)
	}
}

// AC-4: --dry-run — ни каталога, ни вызовов claude; путь, версия и команды;
// код 0 и без claude.
func TestSetupPluginDryRun(t *testing.T) {
	for _, withClaude := range []bool{true, false} {
		dir := withPluginEnv(t, "v0.42.0")
		var pc *pluginClaude
		if withClaude {
			pc = installPluginClaude(t)
		} else {
			t.Setenv("PATH", t.TempDir())
		}
		out, _, err := runPlugin(t, "--dry-run")
		if err != nil {
			t.Fatalf("claude=%v: %v", withClaude, err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("claude=%v: каталог создан", withClaude)
		}
		if pc != nil && len(pc.calls(t)) != 0 {
			t.Errorf("claude вызывался: %v", pc.calls(t))
		}
		for _, want := range []string{"--dry-run", dir, "0.42.0", "claude plugin marketplace add " + dir, "claude plugin install clab-plugin@cadence-lab --scope user"} {
			if !strings.Contains(out, want) {
				t.Errorf("claude=%v: нет %q:\n%s", withClaude, want, out)
			}
		}
	}
}

// AC-5: claude нет — каталог записан, две команды с настоящим путём, код
// ненулевой.
func TestSetupPluginNoClaude(t *testing.T) {
	dir := withPluginEnv(t, "v0.42.0")
	t.Setenv("PATH", t.TempDir())
	out, _, err := runPlugin(t)
	if err == nil || !strings.Contains(err.Error(), "claude не найден") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "clab-plugin", ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("каталог не записан: %v", err)
	}
	for _, want := range []string{"claude plugin marketplace add " + dir, "claude plugin install clab-plugin@cadence-lab --scope user"} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "установлен в области") {
		t.Errorf("вывод говорит об установке:\n%s", out)
	}
}

// AC-6: --plugin не сочетается с флагами MCP и набора; отказ до записей и
// вызовов. Без --plugin плагин не ставится.
func TestSetupPluginFlags(t *testing.T) {
	for _, extra := range [][]string{{"--project", "ACME"}, {"--scope", "user"}, {"--agent", "other"}, {"--name", "x"}, {"--force"}} {
		dir := withPluginEnv(t, "v0.42.0")
		pc := installPluginClaude(t)
		_, _, err := runPlugin(t, extra...)
		if !errors.Is(err, errPluginFlags) {
			t.Errorf("%v: err = %v", extra, err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%v: каталог создан", extra)
		}
		if len(pc.calls(t)) != 0 {
			t.Errorf("%v: claude вызывался: %v", extra, pc.calls(t))
		}
	}

	// clab setup без --plugin: MCP и набор как раньше, плагина нет.
	dir := withPluginEnv(t, "v0.42.0")
	withRoot(t)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitJSON("v1.0.0", kitFile{"skills/a/SKILL.md", "a"}), version: "v1.0.0"}).start(t)
	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("clab setup без --plugin создал каталог плагина")
	}
	for _, call := range fc.calls(t) {
		if strings.HasPrefix(call, "plugin") {
			t.Errorf("clab setup без --plugin вызвал claude %s", call)
		}
	}
}

// Маркетплейс с тем же именем смотрит в другой каталог (сменили
// CLAB_DATA_DIR) — его место занимает нынешний.
func TestSetupPluginMovedMarketplace(t *testing.T) {
	dir := withPluginEnv(t, "v0.42.0")
	pc := installPluginClaude(t)
	if err := os.WriteFile(pc.market, []byte("/old/place"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runPlugin(t); err != nil {
		t.Fatal(err)
	}
	if !pc.called(t, "plugin marketplace remove cadence-lab") || !pc.called(t, "plugin marketplace add "+dir) {
		t.Errorf("вызовы: %v", pc.calls(t))
	}
}

// Плагин выключен человеком — clab его не включает, а подсказывает.
func TestSetupPluginDisabled(t *testing.T) {
	withPluginEnv(t, "v0.42.0")
	pc := installPluginClaude(t)
	if _, _, err := runPlugin(t); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pc.enabled, []byte("false"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc.reset(t)
	out, _, err := runPlugin(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "claude plugin enable clab-plugin@cadence-lab") {
		t.Errorf("нет подсказки:\n%s", out)
	}
	for _, c := range pc.calls(t) {
		if strings.Contains(c, "enable") || strings.Contains(c, "install") {
			t.Errorf("лишний вызов: %s", c)
		}
	}
}

// --json — отчёт целиком.
func TestSetupPluginJSON(t *testing.T) {
	dir := withPluginEnv(t, "v0.42.0")
	installPluginClaude(t)
	withJSON(t, true)
	out, _, err := runPlugin(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"plugin": "clab-plugin@cadence-lab"`, `"version": "0.42.0"`, `"dir": "` + dir + `"`, `"marketplace": "added"`, `"install": "installed"`, `"restart": true`} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %s:\n%s", want, out)
		}
	}
}

func TestClabDataDir(t *testing.T) {
	t.Setenv("CLAB_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if d, _ := clabDataDir(); d != filepath.Join("/xdg", "clab") {
		t.Errorf("XDG: %s", d)
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/ann")
	if d, _ := clabDataDir(); d != filepath.Join("/home/ann", ".local", "share", "clab") {
		t.Errorf("HOME: %s", d)
	}
	t.Setenv("CLAB_DATA_DIR", "/data/clab")
	if d, _ := clabDataDir(); d != "/data/clab" {
		t.Errorf("CLAB_DATA_DIR: %s", d)
	}
	if got := shellArg("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Errorf("shellArg: %s", got)
	}
}
