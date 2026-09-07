package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Symbol — объявление верхнего уровня, найденное в файле.
//
// Извлечение регулярками, а не разбором грамматики: точный символьный граф
// через tree-sitter — фаза 4, и он потребует cgo и по грамматике на язык.
// Здесь задача уже: понять, какие именно объявления затронул коммит, чтобы
// дрейф ловился на уровне символа, а не файла целиком. Промах регулярки
// деградирует до дрейфа уровня файла — это грубее, но не неверно.
type Symbol struct {
	Kind      string
	Name      string
	FQN       string
	StartLine int
	EndLine   int
	Exported  bool
	// BodySHA — sha256 текста объявления. Изменился он — изменился символ;
	// сравнение имён этого не даёт, а хранить два содержимого дорого.
	BodySHA string
}

type declRule struct {
	re *regexp.Regexp
	// group — номер группы с именем; recv — номер группы с получателем
	// метода (0, если метода в языке нет).
	group int
	recv  int
	kind  string
}

var (
	goRules = []declRule{
		{regexp.MustCompile(`^func\s+\(\s*\w*\s*\*?([\w\[\]]+)\s*\)\s+(\w+)`), 2, 1, "method"},
		{regexp.MustCompile(`^func\s+(\w+)`), 1, 0, "func"},
		{regexp.MustCompile(`^type\s+(\w+)`), 1, 0, "type"},
		{regexp.MustCompile(`^(?:var|const)\s+(\w+)\s`), 1, 0, "var"},
	}
	tsRules = []declRule{
		{regexp.MustCompile(`^export\s+(?:default\s+)?(?:abstract\s+)?class\s+(\w+)`), 1, 0, "class"},
		{regexp.MustCompile(`^(?:abstract\s+)?class\s+(\w+)`), 1, 0, "class"},
		{regexp.MustCompile(`^export\s+(?:async\s+)?function\s+\*?(\w+)`), 1, 0, "func"},
		{regexp.MustCompile(`^(?:async\s+)?function\s+\*?(\w+)`), 1, 0, "func"},
		{regexp.MustCompile(`^export\s+(?:type|interface)\s+(\w+)`), 1, 0, "type"},
		{regexp.MustCompile(`^(?:type|interface)\s+(\w+)`), 1, 0, "type"},
		{regexp.MustCompile(`^export\s+(?:const|let|var)\s+(\w+)`), 1, 0, "const"},
		{regexp.MustCompile(`^(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s*)?\(`), 1, 0, "func"},
	}
	pyRules = []declRule{
		{regexp.MustCompile(`^class\s+(\w+)`), 1, 0, "class"},
		{regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)`), 1, 0, "func"},
	}
	sqlRules = []declRule{
		{regexp.MustCompile(`(?i)^create\s+(?:unique\s+)?(?:table|index|view|type|function|trigger|extension)\s+(?:if\s+not\s+exists\s+)?"?(\w+)`), 1, 0, "ddl"},
		{regexp.MustCompile(`(?i)^alter\s+table\s+"?(\w+)`), 1, 0, "ddl"},
	}
	mdRule = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
)

// Symbols разбирает файл на объявления верхнего уровня. Для языка без правил
// возвращает nil — дрейф по такому файлу останется на уровне файла.
func Symbols(filePath string, content []byte) []Symbol {
	lines := strings.Split(string(content), "\n")
	ext := strings.ToLower(path.Ext(filePath))

	switch ext {
	case ".go":
		return byRules(lines, goRules, goPrefix(filePath))
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return byRules(lines, tsRules, modulePrefix(filePath))
	case ".py":
		return byRules(lines, pyRules, modulePrefix(filePath))
	case ".sql":
		return byRules(lines, sqlRules, "")
	case ".md":
		return headings(filePath, lines)
	default:
		return nil
	}
}

// goPrefix — каталог пакета: fqn выглядит как internal/workflow.Engine.Advance
// и совпадает с тем, как на код ссылается артефакт (artifact_code_refs).
func goPrefix(filePath string) string {
	dir := path.Dir(filePath)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// modulePrefix — путь модуля без расширения: у TypeScript и Python единица
// импорта это файл, а не каталог.
func modulePrefix(filePath string) string {
	return strings.TrimSuffix(filePath, path.Ext(filePath))
}

func byRules(lines []string, rules []declRule, prefix string) []Symbol {
	var out []Symbol
	for i, line := range lines {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			// Объявления верхнего уровня начинаются с первой колонки. Вложенные
			// (методы Python, замыкания) остаются внутри своего символа.
			continue
		}
		for _, r := range rules {
			m := r.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[r.group]
			fqn := name
			if r.recv > 0 && m[r.recv] != "" {
				fqn = m[r.recv] + "." + name
			}
			if prefix != "" {
				fqn = prefix + "." + fqn
			}
			out = append(out, Symbol{
				Kind: r.kind, Name: name, FQN: fqn,
				StartLine: i + 1, Exported: exported(name),
			})
			break
		}
	}
	return closeRanges(out, lines)
}

func headings(filePath string, lines []string) []Symbol {
	var out []Symbol
	for i, line := range lines {
		m := mdRule.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, Symbol{
			Kind: "heading", Name: m[2], FQN: filePath + "#" + m[2],
			StartLine: i + 1, Exported: true,
		})
	}
	return closeRanges(out, lines)
}

// closeRanges растягивает каждый символ до начала следующего: где кончается
// объявление, регулярка не знает, а для сравнения содержимого нужен диапазон.
func closeRanges(symbols []Symbol, lines []string) []Symbol {
	for i := range symbols {
		end := len(lines)
		if i+1 < len(symbols) {
			end = symbols[i+1].StartLine - 1
		}
		for end > symbols[i].StartLine && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		symbols[i].EndLine = end
		body := strings.Join(lines[symbols[i].StartLine-1:end], "\n")
		sum := sha256.Sum256([]byte(body))
		symbols[i].BodySHA = hex.EncodeToString(sum[:])
	}
	return symbols
}

func exported(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)[0]
	return unicode.IsUpper(r) || (unicode.IsLetter(r) && r != '_' && !unicode.IsLower(r))
}

// ChangedSymbols — fqn объявлений, различающихся между двумя версиями файла:
// добавленные, удалённые и те, чьё содержимое изменилось.
//
// Это главный вход детектора дрейфа: задача, сославшаяся на конкретный
// символ, получает сигнал high именно отсюда.
func ChangedSymbols(filePath string, before, after []byte) []string {
	oldSyms := map[string]string{}
	for _, s := range Symbols(filePath, before) {
		oldSyms[s.FQN] = s.BodySHA
	}
	newSyms := map[string]string{}
	for _, s := range Symbols(filePath, after) {
		newSyms[s.FQN] = s.BodySHA
	}

	seen := map[string]bool{}
	var changed []string
	for fqn, sha := range newSyms {
		if oldSyms[fqn] != sha && !seen[fqn] {
			seen[fqn] = true
			changed = append(changed, fqn)
		}
	}
	for fqn := range oldSyms {
		if _, ok := newSyms[fqn]; !ok && !seen[fqn] {
			seen[fqn] = true
			changed = append(changed, fqn)
		}
	}
	// Порядок обхода карт случаен, а список уезжает в changed_symbols и в
	// сравнение прогонов: без сортировки одна и та же дельта выглядела бы
	// каждый раз по-новому.
	sort.Strings(changed)
	return changed
}
