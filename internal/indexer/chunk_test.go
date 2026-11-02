package indexer

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunksSplitOnDeclarations(t *testing.T) {
	chunks := Chunks("internal/workflow/engine.go", []byte(goFile), 200)
	if len(chunks) < 2 {
		t.Fatalf("файл на несколько объявлений обязан дать несколько кусков: %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Ord != i {
			t.Fatalf("порядковые номера не подряд: %d у куска %d", c.Ord, i)
		}
		if c.TextSHA == "" {
			t.Fatal("кусок без sha не попадёт в кэш эмбеддингов")
		}
		if c.StartLine < 1 || c.EndLine < c.StartLine {
			t.Fatalf("диапазон строк неверен: %d-%d", c.StartLine, c.EndLine)
		}
	}
}

// Один и тот же текст обязан давать один и тот же sha: на этом держится кэш,
// ради которого повторная индексация не платит за неизменившийся код.
func TestChunkSHAStable(t *testing.T) {
	a := Chunks("a.go", []byte(goFile), 200)
	b := Chunks("a.go", []byte(goFile), 200)
	if len(a) != len(b) {
		t.Fatalf("разное число кусков: %d и %d", len(a), len(b))
	}
	for i := range a {
		if a[i].TextSHA != b[i].TextSHA {
			t.Fatalf("кусок %d: sha разъехались", i)
		}
	}
}

func TestChunksSkipEmpty(t *testing.T) {
	if chunks := Chunks("empty.go", []byte("\n\n  \n"), 100); len(chunks) != 0 {
		t.Fatalf("пустой файл кусков не даёт: %v", chunks)
	}
}

func TestSkipPath(t *testing.T) {
	cases := map[string]bool{
		"internal/workflow/engine.go":   false,
		"node_modules/react/index.js":   true,
		"web/dist/bundle.min.js":        true,
		"package-lock.json":             true,
		"docs/schema.png":               true,
		"frontend/src/api/client.ts":    false,
		"migrations/000001_core.up.sql": false,
	}
	for path, want := range cases {
		got, _ := SkipPath(path, 1000, 512<<10, nil)
		if got != want {
			t.Fatalf("%s: пропуск=%v, ожидали %v", path, got, want)
		}
	}

	if skip, _ := SkipPath("big.go", 2<<20, 512<<10, nil); !skip {
		t.Fatal("файл больше предела индексироваться не должен")
	}
	if skip, _ := SkipPath("internal/gen/api.pb.go", 100, 0, []string{"internal/gen/*"}); !skip {
		t.Fatal("исключение репозитория не сработало")
	}
}

func TestSkipContent(t *testing.T) {
	if skip, _ := SkipContent([]byte{0x00, 0x01, 0x02}); !skip {
		t.Fatal("бинарник в индексе не нужен")
	}
	if skip, _ := SkipContent([]byte("package main\n")); skip {
		t.Fatal("обычный текст пропускать нельзя")
	}
}

// padTo добивает ASCII-префиксом до offset байт, после чего идёт tail: так
// граница в 8000 байт ложится на нужное место символа.
func padTo(offset int, tail string) []byte {
	return append([]byte(strings.Repeat("a", offset)), tail...)
}

// Срез в 8000 байт резал многобайтовый символ, и валидный файл считался
// бинарным (CLAB-361): кириллица (2 байта), иероглиф и € (3), эмодзи (4).
func TestSkipContentKeepsTextWithSplitRuneAtBoundary(t *testing.T) {
	cases := []struct {
		name string
		rune string
		// сколько байт символа остаётся до границы: символ режется на этом
		// месте, 8000-й байт приходится внутрь него.
		before int
	}{
		{"кириллица", "ж", 1},
		{"евро", "€", 1},
		{"евро, режется на два байта", "€", 2},
		{"иероглиф", "世", 1},
		{"эмодзи", "😀", 1},
		{"эмодзи, режется на три байта", "😀", 3},
	}
	for _, c := range cases {
		content := padTo(8000-c.before, c.rune+strings.Repeat("// продолжение\n", 200))
		if utf8.Valid(content[:8000]) {
			t.Fatalf("%s: тест не режет символ на границе", c.name)
		}
		if skip, reason := SkipContent(content); skip {
			t.Errorf("%s: валидный файл отсеян: %s", c.name, reason)
		}
	}
}

// Форма настоящих auth.go и gates.go из CLAB-361: русский текст, символ режется
// на 7999-м байте, определение функции идёт после границы.
func TestSkipContentKeepsRealFileTail(t *testing.T) {
	content := padTo(8000-1, "я\n// Токен задачи гасится вместе с арендой.\nfunc RevokeRunTokens() {}\n")
	if utf8.Valid(content[:8000]) {
		t.Fatal("тест не режет символ на границе")
	}
	if skip, reason := SkipContent(content); skip {
		t.Fatalf("файл отсеян: %s", reason)
	}
}

func TestSkipContentStillRejectsBinaryAndBroken(t *testing.T) {
	long := strings.Repeat("a", 9000)
	cases := []struct {
		name    string
		content []byte
		reason  string
	}{
		{"пустой файл", nil, "пустой файл"},
		{"NUL в голове", append([]byte("ab"), append([]byte{0}, long...)...), "нулевой байт на смещении 2"},
		{"0xff в голове", append([]byte("ab"), append([]byte{0xff}, long...)...), "невалидный UTF-8 на смещении 2"},
		{"NUL после 8000 байт", append([]byte(long), 0, 'x'), "нулевой байт на смещении 9000"},
		{"битая последовательность после 8000 байт", append([]byte(long), 0xff, 'x'), "невалидный UTF-8 на смещении 9000"},
		{"оборванный символ в конце файла", append([]byte(long), 0xd0), "невалидный UTF-8 на смещении 9000"},
		{"суррогат", append([]byte("ab"), 0xed, 0xa0, 0x80), "невалидный UTF-8 на смещении 2"},
	}
	for _, c := range cases {
		skip, reason := SkipContent(c.content)
		if !skip {
			t.Errorf("%s: файл не отсеян", c.name)
			continue
		}
		if reason != c.reason {
			t.Errorf("%s: причина %q, ждали %q", c.name, reason, c.reason)
		}
	}
}

func TestLineDelta(t *testing.T) {
	add, del := lineDelta([]byte("a\nb\nc\n"), []byte("a\nb\nd\ne\n"))
	if add != 2 || del != 1 {
		t.Fatalf("ожидали +2/-1, получили +%d/-%d", add, del)
	}
}
