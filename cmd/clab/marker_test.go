package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-424: строка привязки каталога (решение 0077, п. 6). Здесь — разбор и
// поиск маркера на временном дереве; выбор проекта и хоста клиентом —
// marker_client_test.go, clab auth — auth_marker_test.go, запись блока —
// setup_marker_test.go.

// markerTestRoots — корни временных деревьев теста: поиск маркера идёт до
// корня файловой системы, и файл выше дерева (маркер в / верстака, в
// $TMPDIR или в рабочей директории, AC-19) не должен менять результат.
var markerTestRoots = map[string]bool{}

// inMarkerTestTree — путь внутри одного из деревьев теста.
func inMarkerTestTree(path string) bool {
	for root := range markerTestRoots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// tree — временное дерево каталогов с реальным путём (на macOS TempDir
// лежит за ссылкой /var → /private/var, а os.Getwd отдаёт реальный).
// Файлы маркера вне деревьев теста для поиска не существуют.
func markerTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	markerTestRoots[root] = true
	prev := markerReadFile
	markerReadFile = func(path string) ([]byte, error) {
		if !inMarkerTestTree(path) {
			return nil, os.ErrNotExist
		}
		return prev(path)
	}
	t.Cleanup(func() {
		markerReadFile = prev
		delete(markerTestRoots, root)
	})
	return root
}

func writeMarkerTestFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func markerText(project, host string) string {
	attrs := "project=" + project
	if host != "" {
		attrs += " host=" + host
	}
	return "# Проект\n\nПроза файла.\n\n<!-- cadence-lab:begin -->\n<!-- cadence-lab: " + attrs + " -->\n" +
		"Cadence Lab: this directory works with project " + project + ".\n<!-- cadence-lab:end -->\n"
}

func mustFind(t *testing.T, start string) *dirMarker {
	t.Helper()
	m, err := findMarker(start)
	if err != nil {
		t.Fatalf("findMarker(%s): %v", start, err)
	}
	return m
}

// AC-1, AC-2: маркер в корне находится из .worktrees/X/backend, где свой
// CLAUDE.md без маркера.
func TestMarkerFoundFromNestedWorktree(t *testing.T) {
	root := markerTree(t)
	writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), markerText("CLAB", "https://sdlc.example.com"))
	deep := filepath.Join(root, ".worktrees", "CLAB-12", "backend")
	writeMarkerTestFile(t, filepath.Join(deep, "CLAUDE.md"), "# backend\n\nсвой CLAUDE.md без маркера\n")

	m := mustFind(t, deep)
	if m == nil || m.Project != "CLAB" || m.Host != "sdlc.example.com" || m.Origin != "https://sdlc.example.com" ||
		m.Path != filepath.Join(root, "CLAUDE.md") {
		t.Fatalf("маркер: %+v", m)
	}
}

// AC-2: ближний сильнее дальнего; AGENTS.md раньше CLAUDE.md; одинаковые
// маркеры — не конфликт; симлинк — один файл.
func TestMarkerNearestAndOrder(t *testing.T) {
	root := markerTree(t)
	writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), markerText("CLAB", ""))
	client := filepath.Join(root, "client")
	writeMarkerTestFile(t, filepath.Join(client, "AGENTS.md"), markerText("ACME", ""))
	if m := mustFind(t, filepath.Join(client, "service")); m == nil || m.Project != "ACME" || m.Path != filepath.Join(client, "AGENTS.md") {
		t.Fatalf("ближний: %+v", m)
	}

	// Одинаковые в обоих файлах — берётся AGENTS.md.
	writeMarkerTestFile(t, filepath.Join(client, "CLAUDE.md"), "другая проза\n"+markerText("ACME", ""))
	if m := mustFind(t, client); m == nil || m.Path != filepath.Join(client, "AGENTS.md") {
		t.Fatalf("одинаковые: %+v", m)
	}

	// AGENTS.md — ссылка на CLAUDE.md.
	link := filepath.Join(root, "linked")
	writeMarkerTestFile(t, filepath.Join(link, "CLAUDE.md"), markerText("LNK", ""))
	if err := os.Symlink("CLAUDE.md", filepath.Join(link, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if m := mustFind(t, link); m == nil || m.Project != "LNK" {
		t.Fatalf("симлинк: %+v", m)
	}

	// Без маркеров вниз по дереву — ничего; файл-каталог с именем AGENTS.md
	// пропускается.
	empty := filepath.Join(markerTree(t), "a", "b")
	if err := os.MkdirAll(filepath.Join(empty, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if m := mustFind(t, empty); m != nil {
		t.Fatalf("маркер из ниоткуда: %+v", m)
	}
}

// AC-2, AC-8: разные маркеры в одном каталоге — ошибка с путями обоих.
func TestMarkerConflictInOneDir(t *testing.T) {
	root := markerTree(t)
	a := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("ONE", ""))
	c := writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), markerText("TWO", ""))
	_, err := findMarker(root)
	if err == nil || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), c) {
		t.Fatalf("конфликт: %v", err)
	}
}

// AC-8: маркер не принят — ошибка называет файл и причину, но не значение и
// не прозу файла.
func TestMarkerRejected(t *testing.T) {
	const secret = "sdlc_tok_very-secret-42"
	cases := map[string]string{
		"токен атрибутом":   "<!-- cadence-lab: project=ACME token=" + secret + " -->",
		"слово без =":       "<!-- cadence-lab: project=ACME " + secret + " -->",
		"логин в адресе":    "<!-- cadence-lab: project=ACME host=https://u:" + secret + "@a.example -->",
		"параметры":         "<!-- cadence-lab: project=ACME host=https://a.example/?token=" + secret + " -->",
		"фрагмент":          "<!-- cadence-lab: project=ACME host=https://a.example/#" + secret + " -->",
		"схема":             "<!-- cadence-lab: project=ACME host=ftp://a.example -->",
		"нет project":       "<!-- cadence-lab: host=https://a.example -->",
		"пустой маркер":     "<!-- cadence-lab: -->",
		"кривой ключ":       "<!-- cadence-lab: project=" + secret + "/x -->",
		"повтор":            "<!-- cadence-lab: project=A project=B -->",
		"незакрытый":        "<!-- cadence-lab: project=ACME",
		"два маркера":       "<!-- cadence-lab: project=A -->\nтекст\n<!-- cadence-lab: project=B -->",
		"секрет в названии": "<!-- cadence-lab: project=ACME " + secret + "=1 -->",
	}
	const prose = "Прозаическая строка про пароль hunter2"
	for name, line := range cases {
		root := markerTree(t)
		path := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), prose+"\n"+line+"\n")
		_, err := findMarker(root)
		if err == nil {
			t.Errorf("%s: маркер принят", name)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, path) {
			t.Errorf("%s: ошибка без пути: %s", name, msg)
		}
		if strings.Contains(msg, secret) || strings.Contains(msg, "hunter2") {
			t.Errorf("%s: в ошибке содержимое файла: %s", name, msg)
		}
	}
}

// AC-9: читается только строка маркера. Проза с похожими словами, маркер в
// блоке кода (пример в документации) и строки begin/end маркером не
// считаются; бинарный и огромный файл не роняют поиск.
func TestMarkerReadsOnlyMarkerLine(t *testing.T) {
	root := markerTree(t)
	writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), strings.Join([]string{
		"project=EVIL host=https://evil.example",
		"Текст: cadence-lab: project=EVIL",
		"```markdown",
		"<!-- cadence-lab: project=<KEY> host=<URL> -->",
		"```",
		"~~~",
		"<!-- cadence-lab: project=FENCE -->",
		"~~~",
		"  <!-- cadence-lab: project=GOOD host=good.example -->  ",
		"<!-- cadence-lab:begin -->",
		"<!-- cadence-lab:end -->",
	}, "\r\n"))
	m := mustFind(t, root)
	if m == nil || m.Project != "GOOD" || m.Origin != "https://good.example" {
		t.Fatalf("маркер: %+v", m)
	}

	// Бинарный мусор без маркера — пропускается.
	bin := markerTree(t)
	writeMarkerTestFile(t, filepath.Join(bin, "AGENTS.md"), string(bytes.Repeat([]byte{0, 0xff, '\n', 0x7f}, 1<<16)))
	if m := mustFind(t, bin); m != nil {
		t.Fatalf("маркер в бинарном файле: %+v", m)
	}

	// Огромный файл: читается не дальше предела.
	big := markerTree(t)
	pad := strings.Repeat("x", 1023) + "\n"
	head := markerText("HEAD", "") + strings.Repeat(pad, 3*1024)
	writeMarkerTestFile(t, filepath.Join(big, "AGENTS.md"), head)
	if m := mustFind(t, big); m == nil || m.Project != "HEAD" {
		t.Fatalf("маркер в начале большого файла: %+v", m)
	}
	far := markerTree(t)
	writeMarkerTestFile(t, filepath.Join(far, "AGENTS.md"), strings.Repeat(pad, markerReadLimit/len(pad)+16)+markerText("FAR", ""))
	if m := mustFind(t, far); m != nil {
		t.Fatalf("маркер за пределом %d байт прочитан: %+v", markerReadLimit, m)
	}
	if markerReadLimit != 1<<20 {
		t.Fatalf("предел чтения %d, ожидался 1 МиБ", markerReadLimit)
	}
}

// Ключ проекта и адрес — в форме, которой их принимает clab.
func TestMarkerBodyForms(t *testing.T) {
	for body, want := range map[string][3]string{
		"project=ACME":                             {"ACME", "", ""},
		"project=acme_2-x host=sdlc.example.com":   {"acme_2-x", "sdlc.example.com", "https://sdlc.example.com"},
		"host=http://127.0.0.1:8080/ project=K":    {"K", "127.0.0.1:8080", "http://127.0.0.1:8080"},
		"project=K host=https://a.example/clab/v1": {"K", "a.example", "https://a.example/clab"},
	} {
		m, err := parseMarkerBody(body)
		if err != nil || m.Project != want[0] || m.Host != want[1] || m.Origin != want[2] {
			t.Errorf("%q: %+v %v", body, m, err)
		}
	}
}
