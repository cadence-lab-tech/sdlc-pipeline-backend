package clabplugin

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// AC-8: вшит весь мод — каждый файл каталога, кроме тестов, Go-файлов и
// типов, которые движок кладёт при разработке; в том числе манифест под
// .claude-plugin/.
func TestEmbeddedMatchesTree(t *testing.T) {
	var onDisk []string
	err := fs.WalkDir(os.DirFS("."), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == "tests" || p == ".claude-plugin/types" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".test.ts") || strings.HasSuffix(p, ".test.tsx") {
			return nil
		}
		onDisk = append(onDisk, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(onDisk)
	files, err := Files("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	var embedded []string
	for _, f := range files {
		embedded = append(embedded, f.Path)
	}
	if strings.Join(embedded, "\n") != strings.Join(onDisk, "\n") {
		t.Fatalf("вшито:\n%s\nна диске:\n%s", strings.Join(embedded, "\n"), strings.Join(onDisk, "\n"))
	}
	for _, want := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.tsx", "types/index.d.ts"} {
		if !strings.Contains("\n"+strings.Join(embedded, "\n")+"\n", "\n"+want+"\n") {
			t.Errorf("не вшит %s", want)
		}
	}
}

// AC-7: версия плагина — версия clab; сборка без версии — семвер ниже любого
// выпуска, своя для каждого содержимого мода.
func TestVersion(t *testing.T) {
	for in, want := range map[string]string{"v0.31.0": "0.31.0", "1.2.3": "1.2.3", "v2.0.0-rc.1": "2.0.0-rc.1"} {
		if got := Version(in); got != want {
			t.Errorf("Version(%q) = %q, want %q", in, got, want)
		}
	}
	dev := regexp.MustCompile(`^0\.0\.0-dev\.[0-9a-f]{8}$`)
	for _, in := range []string{"dev", "", "abc1234", "v1.2"} {
		if got := Version(in); !dev.MatchString(got) {
			t.Errorf("Version(%q) = %q, want 0.0.0-dev.<sha>", in, got)
		}
	}
	if Version("dev") != "0.0.0-dev."+Digest()[:8] {
		t.Error("версия dev не из содержимого мода")
	}

	for _, v := range []string{"v0.31.0", "dev"} {
		files, err := Files(v)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Name       string                    `json:"name"`
			Version    string                    `json:"version"`
			UserConfig map[string]map[string]any `json:"userConfig"`
		}
		for _, f := range files {
			if f.Path == ".claude-plugin/plugin.json" {
				if err := json.Unmarshal(f.Content, &m); err != nil {
					t.Fatal(err)
				}
			}
		}
		if m.Name != Name || m.Version != Version(v) {
			t.Errorf("%s: манифест %s@%s, want %s@%s", v, m.Name, m.Version, Name, Version(v))
		}
		// AC-14: адреса и проекта по умолчанию нет — чужой инстанс не получит
		// наш адрес.
		for _, k := range []string{"host", "project"} {
			if _, ok := m.UserConfig[k]["default"]; ok {
				t.Errorf("userConfig.%s несёт значение по умолчанию", k)
			}
		}
	}
}

// AC-14: мод не знает этапов, ролей и типов нашего цикла и нашего инстанса.
// Типы движка и тесты — вне проверки.
func TestNoCycleVocabulary(t *testing.T) {
	files, err := Files("dev")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`analysis|development|testing|merge|epic|release|CLAB|food-cart`)
	for _, f := range files {
		if strings.HasPrefix(f.Path, "types/") {
			continue
		}
		for i, line := range strings.Split(string(f.Content), "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d: %s", f.Path, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// CLAB-324, AC-17: $ — только в hooks/register.tsx. Движок модов прослеживает
// $ внутри одного файла, поэтому остальные модули мода — чистые функции,
// которые строят модель вида; register.tsx превращает её в дерево.
func TestOnlyRegisterCallsEngine(t *testing.T) {
	files, err := Files("dev")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\$\.`)
	seen := 0
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "hooks/") || f.Path == "hooks/register.tsx" || !strings.HasSuffix(f.Path, ".ts") {
			continue
		}
		seen++
		for i, line := range strings.Split(string(f.Content), "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d: $ вне register.tsx: %s", f.Path, i+1, strings.TrimSpace(line))
			}
		}
	}
	if seen < 5 {
		t.Fatalf("проверено модулей %d: каталог hooks не тот, что ожидался", seen)
	}
}

// AC-9: claude plugin validate --strict и claude plugin test по исходному
// каталогу. В CI claude нет (В-1: его установка — отдельная задача), там
// проверка пропускается; на машине разработчика и тестировщика она идёт с
// пустым CLAUDE_CONFIG_DIR, чтобы не трогать настоящий ~/.claude.
func TestClaudePlugin(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude не найден в PATH: validate и test мода пропущены (в CI claude не ставится)")
	}
	for _, args := range [][]string{{"plugin", "validate", "--strict", "."}, {"plugin", "test", "."}} {
		cmd := exec.Command(claude, args...)
		cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("claude %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}
