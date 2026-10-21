package agentkit

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"testing"
	"unicode"
)

// Состав набора (CLAB-232, критерий 1 и решение человека о clab-rules):
// пять скилов работы с циклом, справка и саб-агент исполнителя.
var wantFiles = []string{
	"agents/clab-stage.md",
	"skills/clab-backlog/SKILL.md",
	"skills/clab-guide/SKILL.md",
	"skills/clab-rules/SKILL.md",
	"skills/clab-run/SKILL.md",
	"skills/clab-status/SKILL.md",
	"skills/clab-work/SKILL.md",
}

func TestKitComposition(t *testing.T) {
	k, err := Load("v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if k.Version != "v9.9.9" {
		t.Fatalf("версия набора: %q", k.Version)
	}
	var got []string
	for _, f := range k.Files {
		got = append(got, f.Path)
	}
	if strings.Join(got, "\n") != strings.Join(wantFiles, "\n") {
		t.Fatalf("файлы набора:\n%s\nвместо\n%s", strings.Join(got, "\n"), strings.Join(wantFiles, "\n"))
	}
	if len(k.Skills) != 6 || len(k.Agents) != 1 || k.Agents[0].Name != "clab-stage" {
		t.Fatalf("скилы %v, агенты %v", k.Skills, k.Agents)
	}
}

// Критерий 1: у скила name совпадает с каталогом, description не пуст; у
// агента — name, description и tools. Сумма — sha256 содержимого.
func TestKitFrontmatter(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range k.Files {
		sum := sha256.Sum256([]byte(f.Content))
		if f.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: sha256 не сходится", f.Path)
		}
		meta := Frontmatter(f.Content)
		switch {
		case strings.HasPrefix(f.Path, "skills/"):
			dir := path.Base(path.Dir(f.Path))
			if meta["name"] != dir {
				t.Errorf("%s: name %q, а каталог %q", f.Path, meta["name"], dir)
			}
			if strings.TrimSpace(meta["description"]) == "" {
				t.Errorf("%s: пустой description", f.Path)
			}
		case strings.HasPrefix(f.Path, "agents/"):
			if meta["name"] != strings.TrimSuffix(path.Base(f.Path), ".md") ||
				meta["description"] == "" || meta["tools"] == "" {
				t.Errorf("%s: фронтматтер агента %v", f.Path, meta)
			}
		default:
			t.Errorf("%s: файл вне skills/ и agents/", f.Path)
		}
	}
	for _, s := range k.Skills {
		if s.Name == "" || s.Description == "" {
			t.Errorf("скил без имени или описания: %+v", s)
		}
	}
}

// Критерий 2: набор не знает нашего инстанса, наших путей и ключей переходов
// конкретного цикла — их агент берёт из clab claim / show / gates. Язык —
// английский: кириллицы в наборе нет.
func TestKitIsGeneric(t *testing.T) {
	forbidden := []string{
		"CLAB-", "sdlc.food-cart.ru", "stepankaziatko", "sdlc-pipeline", "stack.sh",
		"analysis_done", "dev_done", "testing_passed", "merge_ready",
	}
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range k.Files {
		for _, s := range forbidden {
			if strings.Contains(f.Content, s) {
				t.Errorf("%s содержит %q", f.Path, s)
			}
		}
		for i, line := range strings.Split(f.Content, "\n") {
			for _, r := range line {
				if unicode.Is(unicode.Cyrillic, r) {
					t.Errorf("%s:%d: кириллица — набор на английском", f.Path, i+1)
					break
				}
			}
		}
		for _, s := range []string{"clab claim", "clab gates"} {
			if strings.HasSuffix(f.Path, "clab-work/SKILL.md") && !strings.Contains(f.Content, s) {
				t.Errorf("clab-work не отсылает к %q за переходами и гейтами", s)
			}
		}
	}
}

func TestFrontmatter(t *testing.T) {
	doc := "---\nname: x\ndescription: \"a: b\"\nhooks:\n  PreToolUse:\n    - matcher: Bash\ntools: Bash, Read\n---\nbody: no\n"
	m := Frontmatter(doc)
	if m["name"] != "x" || m["description"] != "a: b" || m["tools"] != "Bash, Read" || m["body"] != "" || m["matcher"] != "" {
		t.Fatalf("разбор: %v", m)
	}
	if len(Frontmatter("no frontmatter\nname: x\n")) != 0 || len(Frontmatter("---\nname: x\n")) != 0 {
		t.Fatal("без фронтматтера поля не читаются")
	}
}
