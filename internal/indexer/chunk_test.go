package indexer

import "testing"

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
	if !SkipContent([]byte{0x00, 0x01, 0x02}) {
		t.Fatal("бинарник в индексе не нужен")
	}
	if SkipContent([]byte("package main\n")) {
		t.Fatal("обычный текст пропускать нельзя")
	}
}

func TestLineDelta(t *testing.T) {
	add, del := lineDelta([]byte("a\nb\nc\n"), []byte("a\nb\nd\ne\n"))
	if add != 2 || del != 1 {
		t.Fatalf("ожидали +2/-1, получили +%d/-%d", add, del)
	}
}
