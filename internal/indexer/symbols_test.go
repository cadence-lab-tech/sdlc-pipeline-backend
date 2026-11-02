package indexer

import (
	"os"
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

func symbolNames(syms []Symbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Name)
	}
	return out
}

// AC-15 (CLAB-369): объявление внутри многострочного литерала — текст, а не
// символ; после литерала объявления снова символы, а кавычки внутри обычных
// строк, рун и комментариев литерала не открывают.
func TestSymbolsSkipMultilineLiterals(t *testing.T) {
	bt := "`"
	cases := []struct {
		name string
		path string
		src  string
		want []string
	}{
		{
			name: "go raw-строка с начала строки",
			path: "x/a_test.go",
			src: "package x\n\nconst fixture = " + bt + "package y\n" +
				"func (s *Store) RevokeRunTokens(ctx context.Context) error {\n\treturn nil\n}\n" +
				"type Fake struct{}\n" + bt + "\n\nfunc TestA(t *testing.T) {}\n",
			want: []string{"fixture", "TestA"},
		},
		{
			name: "go raw-строка, открытая в середине выражения",
			path: "x/a.go",
			src:  "package x\n\nvar q = \"a\" + " + bt + "\nfunc Hidden() {}\n" + bt + " + \"b\"\n\nfunc Real() {}\n",
			want: []string{"q", "Real"},
		},
		{
			name: "go обратная кавычка в строке, руне и комментарии",
			path: "x/a.go",
			src: "package x\n\nvar s = \"a`b\"\nvar r = '`'\n// пример: `foo\n/* и тут ` */\n" +
				"func One() {}\n\nfunc Two() {}\n",
			want: []string{"s", "r", "One", "Two"},
		},
		{
			name: "go блочный комментарий на несколько строк",
			path: "x/a.go",
			src:  "package x\n\n/*\n строка с ` внутри\n*/\nfunc One() {}\n",
			want: []string{"One"},
		},
		{
			name: "go экранированная кавычка в строке",
			path: "x/a.go",
			src:  "package x\n\nvar s = \"\\\"`\"\nfunc One() {}\n",
			want: []string{"s", "One"},
		},
		{
			name: "ts шаблонная строка",
			path: "src/a.test.ts",
			src: "const page = " + bt + "\nexport function Hidden() {}\nclass Ghost {}\n" + bt + "\n\n" +
				"export function Real() {}\n",
			// const без стрелочной функции правилами TS не берётся.
			want: []string{"Real"},
		},
		{
			name: "ts подстановка с вложенным шаблоном",
			path: "src/a.ts",
			src: "const x = " + bt + "a ${cond ? " + bt + "b" + bt + " : {k: 1}.k} c\nfunction Hidden() {}\n" + bt + "\n" +
				"export const y = '" + bt + "'\n// " + bt + "\nfunction Real() {}\n",
			want: []string{"y", "Real"},
		},
		{
			name: "python тройные кавычки с префиксами",
			path: "pkg/a.py",
			src: "X = r\"\"\"\nclass Hidden:\n    pass\n\"\"\"\nY = B'''\ndef hidden():\n'''\n" +
				"Z = f\"\"\"one line\"\"\"\nW = U'''\ndef hidden2(): pass\n'''\n" +
				"s = \"'''\"  # ''' в обычной строке\n# \"\"\" в комментарии\nclass Real:\n    \"\"\"Документация.\n\ndef notTop():\n\"\"\"\n\ndef real():\n    pass\n",
			want: []string{"Real", "real"},
		},
		{
			name: "markdown ограждённые блоки",
			path: "docs/a.md",
			src: "# Заголовок\n\n```bash\n# комментарий оболочки\n```\n\n## Второй\n\n" +
				"   ~~~~\n# внутри тильд\n~~~\n# всё ещё внутри: ограда короче\n~~~~~\n\n## Третий\n" +
				"````\n```\n# внутри: закрытие короче\n````\n### Четвёртый\n" +
				"``` не ограда `с кавычкой`\n# Пятый\n",
			want: []string{"Заголовок", "Второй", "Третий", "Четвёртый", "Пятый"},
		},
		{
			name: "незакрытый литерал go: остаток разбирается как прежде",
			path: "x/a.go",
			src:  "package x\n\nfunc One() {}\n\nvar broken = " + bt + "\nfunc Two() {}\n\nfunc Three() {}\n",
			want: []string{"One", "broken", "Two", "Three"},
		},
		{
			name: "незакрытый блок markdown: остаток разбирается как прежде",
			path: "docs/a.md",
			src:  "# Один\n```\n# в закрытом\n```\n\n```go\n# Два\n\n# Три\n",
			want: []string{"Один", "Два", "Три"},
		},
		{
			name: "незакрытая строка python",
			path: "a.py",
			src:  "def one():\n    pass\n\nx = \"\"\"\ndef two():\n    pass\n",
			want: []string{"one", "two"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := symbolNames(Symbols(c.path, []byte(c.src))); !slices.Equal(got, c.want) {
				t.Fatalf("символы %v, ждали %v", got, c.want)
			}
		})
	}
}

// Прежнее извлечение (версия 0) литералов не знает: им тесты доводки
// строят индекс «как до CLAB-369».
func TestLegacyExtractionKeepsFalseSymbols(t *testing.T) {
	src := []byte("package x\n\nconst f = `\nfunc Hidden() {}\n`\n")
	if got := symbolNames(extractSymbols("x/a.go", src, false)); !slices.Equal(got, []string{"f", "Hidden"}) {
		t.Fatalf("прежнее извлечение: %v", got)
	}
}

// AC-15: фикстуры поиска searchAuthDecl и searchAttachmentsDecl лежат в
// raw-строках — извлечение файла с ними даёт только его настоящие
// объявления (на проде отсюда брались internal/indexer.tokenHash и
// internal/indexer.Store.RevokeRunTokens).
func TestSymbolsOfSearchFixtureFile(t *testing.T) {
	content, err := os.ReadFile("search_code_test.go")
	if err != nil {
		t.Fatal(err)
	}
	syms := Symbols("internal/indexer/search_code_test.go", content)
	for _, s := range syms {
		switch s.FQN {
		case "internal/indexer.tokenHash", "internal/indexer.Store.RevokeRunTokens",
			"internal/indexer.Store.ListAttachments", "internal/indexer.Store.IssueTaskToken",
			"internal/indexer.Engine.releaseLease", "internal/indexer.task", "internal/indexer.mixed",
			"internal/indexer.PutBlob", "internal/indexer.Page":
			t.Fatalf("символ из строки-фикстуры: %s (строка %d)", s.FQN, s.StartLine)
		}
	}
	names := symbolNames(syms)
	for _, want := range []string{"searchPadding", "searchAuthDecl", "searchAttachmentsDecl",
		"indexSearchFixture", "searchCode", "TestCodeSearchPutsDefinitionFirst"} {
		if !slices.Contains(names, want) {
			t.Fatalf("нет настоящего объявления %s: %v", want, names)
		}
	}
}

// AC-15, R19: две версии файла, различающиеся только строкой внутри
// литерала, не дают в дельте ни одного символа из литерала — ни прежнего
// ложного, ни нового. Остаётся объявление, в тело которого литерал входит:
// его текст изменился по-настоящему, и задача, сославшаяся на него, обязана
// получить сигнал (BodySHA).
func TestChangedSymbolsInsideLiteral(t *testing.T) {
	before := []byte("package x\n\nfunc A() {}\n\nconst f = `\nfunc Hidden() {}\n`\n\nfunc B() {}\n")
	after := []byte("package x\n\nfunc A() {}\n\nconst f = `\nfunc Hidden2() {}\n`\n\nfunc B() {}\n")
	if got := ChangedSymbols("x/a.go", before, after); !slices.Equal(got, []string{"x.f"}) {
		t.Fatalf("дельта %v, ждали только x.f", got)
	}
	// Тот же литерал при прежнем извлечении дал бы ложные Hidden и Hidden2.
	same := []byte("package x\n\nconst f = `\nfunc Hidden() {}\n`\n")
	if got := ChangedSymbols("x/a.go", same, same); len(got) != 0 {
		t.Fatalf("одинаковые версии дали дельту %v", got)
	}
}
