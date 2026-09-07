package indexer

import (
	"slices"
	"testing"
)

const goFile = `package workflow

import "context"

// Engine ведёт задачу по циклу.
type Engine struct {
	store *Store
}

func NewEngine(store *Store) *Engine {
	return &Engine{store: store}
}

func (e *Engine) Advance(ctx context.Context) error {
	return nil
}
`

func TestSymbolsGo(t *testing.T) {
	syms := Symbols("internal/workflow/engine.go", []byte(goFile))

	var fqns []string
	for _, s := range syms {
		fqns = append(fqns, s.FQN)
	}
	want := []string{
		"internal/workflow.Engine",
		"internal/workflow.NewEngine",
		"internal/workflow.Engine.Advance",
	}
	for _, w := range want {
		if !slices.Contains(fqns, w) {
			t.Fatalf("не найден символ %q, есть: %v", w, fqns)
		}
	}
}

// Изменение тела одной функции обязано давать ровно один изменившийся символ:
// на этом стоит уровень high у детектора дрейфа. Если сюда попадёт весь файл,
// каждая задача с ссылкой на любой символ файла получит high.
func TestChangedSymbolsIsolatesOneFunction(t *testing.T) {
	after := []byte(`package workflow

import "context"

// Engine ведёт задачу по циклу.
type Engine struct {
	store *Store
}

func NewEngine(store *Store) *Engine {
	return &Engine{store: store}
}

func (e *Engine) Advance(ctx context.Context) error {
	return e.store.Save(ctx)
}
`)

	changed := ChangedSymbols("internal/workflow/engine.go", []byte(goFile), after)
	if len(changed) != 1 || changed[0] != "internal/workflow.Engine.Advance" {
		t.Fatalf("ожидали один изменившийся символ Advance, получили %v", changed)
	}
}

func TestChangedSymbolsCatchesRemoval(t *testing.T) {
	after := []byte(`package workflow

type Engine struct {
	store *Store
}
`)
	changed := ChangedSymbols("internal/workflow/engine.go", []byte(goFile), after)
	if !slices.Contains(changed, "internal/workflow.NewEngine") {
		t.Fatalf("удалённая функция обязана попасть в дельту: %v", changed)
	}
}

// Для языка без правил дрейф деградирует до уровня файла — это ожидаемое
// поведение, а не пропущенный случай.
func TestSymbolsUnknownLanguage(t *testing.T) {
	if syms := Symbols("config/values.ini", []byte("[main]\nkey=value\n")); len(syms) != 0 {
		t.Fatalf("для неизвестного языка символов быть не должно: %v", syms)
	}
}

func TestSymbolsSQLAndTypeScript(t *testing.T) {
	sql := Symbols("migrations/000001_core.up.sql",
		[]byte("CREATE TABLE tasks (\n id uuid\n);\n\nALTER TABLE tasks ADD COLUMN x int;\n"))
	if len(sql) != 2 || sql[0].FQN != "tasks" {
		t.Fatalf("SQL разобран неверно: %+v", sql)
	}

	ts := Symbols("src/api/client.ts",
		[]byte("export type ErrorCode = 'a' | 'b'\n\nexport async function fetchTask(key: string) {\n  return key\n}\n"))
	if len(ts) != 2 || ts[1].FQN != "src/api/client.fetchTask" {
		t.Fatalf("TypeScript разобран неверно: %+v", ts)
	}
}
