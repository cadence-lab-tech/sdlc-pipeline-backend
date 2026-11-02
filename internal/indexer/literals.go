package indexer

import (
	"regexp"
	"strings"
)

// Многострочные литералы и ограждённые блоки (CLAB-369).
//
// Правила объявлений смотрят на строку с первой колонки и состояния между
// строками не знают: строка `func (s *Store) RevokeRunTokens(` внутри
// raw-строки Go-теста или `# комментарий` внутри блока ```bash в markdown
// становились символами, резали файл на куски и поднимали кусок фикстуры
// выше настоящего определения в поиске. Здесь — ровно столько лексики, чтобы
// знать, какие строки начинаются внутри многострочного литерала: обычные
// строки, руны и комментарии разбираются только затем, чтобы обратная или
// тройная кавычка в них литерал не открывала.
//
// Разбор эвристический, и промах обязан деградировать до лишнего символа, а
// не до потери настоящих: литерал или блок, не закрытый до конца файла, —
// ошибка разбора, и остаток файла после его начала разбирается как прежде
// (hiddenLines).
//
// Блочные комментарии /* … */ разбираются, но строк не прячут: объявления в
// них остаются символами, как были (вне объёма CLAB-369).

// lineLexer проходит файл построчно и помнит состояние между строками.
type lineLexer interface {
	// scan разбирает строку и сообщает, закончилась ли она внутри
	// многострочного литерала (inside) и открыт ли этот литерал на ней же
	// (opened).
	scan(line string) (inside, opened bool)
}

// hiddenLines отмечает строки, которые начинаются внутри многострочного
// литерала: правила объявлений их не смотрят.
func hiddenLines(lines []string, lex lineLexer) []bool {
	hidden := make([]bool, len(lines))
	inside, open := false, -1
	for i, line := range lines {
		hidden[i] = inside
		var opened bool
		inside, opened = lex.scan(line)
		if opened {
			open = i
		}
	}
	if inside {
		// Литерал не закрылся до конца файла: верить разбору с этого места
		// нельзя, и остаток файла разбирается прежним способом — лишний
		// символ лучше потерянного.
		for j := open + 1; j < len(lines); j++ {
			hidden[j] = false
		}
	}
	return hidden
}

// skipQuoted пропускает однострочный литерал в кавычках с экранированием
// обратной косой и возвращает позицию за ним (или конец строки, если
// литерал не закрыт на ней).
func skipQuoted(line string, i int) int {
	q := line[i]
	for j := i + 1; j < len(line); j++ {
		switch line[j] {
		case '\\':
			j++
		case q:
			return j + 1
		}
	}
	return len(line)
}

// skipBlockComment ищет конец блочного комментария с позиции i: позиция за
// ним и true, либо false, если комментарий продолжается на следующей строке.
func skipBlockComment(line string, i int) (int, bool) {
	j := strings.Index(line[i:], "*/")
	if j < 0 {
		return len(line), false
	}
	return i + j + 2, true
}

// goLexer — raw-строки Go `…`; "…", '…' и комментарии их не открывают.
type goLexer struct {
	raw   bool
	block bool
}

func (l *goLexer) scan(line string) (bool, bool) {
	opened := false
	for i := 0; i < len(line); {
		switch {
		case l.raw:
			j := strings.IndexByte(line[i:], '`')
			if j < 0 {
				return true, opened
			}
			l.raw, opened = false, false
			i += j + 1
		case l.block:
			var closed bool
			if i, closed = skipBlockComment(line, i); closed {
				l.block = false
			}
		default:
			switch c := line[i]; {
			case c == '/' && strings.HasPrefix(line[i:], "//"):
				return false, false
			case c == '/' && strings.HasPrefix(line[i:], "/*"):
				l.block = true
				i += 2
			case c == '"' || c == '\'':
				i = skipQuoted(line, i)
			case c == '`':
				l.raw, opened = true, true
				i++
			default:
				i++
			}
		}
	}
	return l.raw, opened && l.raw
}

// tsLexer — шаблонные строки TS/JS `…` с подстановками ${…}, внутри которых
// снова код (и снова шаблоны). Стек: -1 — шаблон, n ≥ 0 — подстановка с
// глубиной фигурных скобок n.
type tsLexer struct {
	stack []int
	block bool
}

func (l *tsLexer) top() int { return l.stack[len(l.stack)-1] }

func (l *tsLexer) scan(line string) (bool, bool) {
	opened := false
	for i := 0; i < len(line); {
		if l.block {
			var closed bool
			if i, closed = skipBlockComment(line, i); closed {
				l.block = false
			}
			continue
		}
		c := line[i]
		if len(l.stack) > 0 && l.top() < 0 {
			switch {
			case c == '\\':
				i += 2
			case c == '`':
				l.stack = l.stack[:len(l.stack)-1]
				if len(l.stack) == 0 {
					opened = false
				}
				i++
			case c == '$' && strings.HasPrefix(line[i:], "${"):
				l.stack = append(l.stack, 0)
				i += 2
			default:
				i++
			}
			continue
		}
		switch {
		case c == '/' && strings.HasPrefix(line[i:], "//"):
			i = len(line)
		case c == '/' && strings.HasPrefix(line[i:], "/*"):
			l.block = true
			i += 2
		case c == '"' || c == '\'':
			i = skipQuoted(line, i)
		case c == '`':
			if len(l.stack) == 0 {
				opened = true
			}
			l.stack = append(l.stack, -1)
			i++
		case c == '{' && len(l.stack) > 0:
			l.stack[len(l.stack)-1]++
			i++
		case c == '}' && len(l.stack) > 0:
			if l.top() == 0 {
				l.stack = l.stack[:len(l.stack)-1]
			} else {
				l.stack[len(l.stack)-1]--
			}
			i++
		default:
			i++
		}
	}
	inside := len(l.stack) > 0
	return inside, opened && inside
}

// pyLexer — строки Python в тройных кавычках, три " или три ' (префиксы r,
// b, f, u — буквы перед кавычкой, разбору они не мешают); '…', "…" и
// комментарии # их не открывают.
type pyLexer struct {
	quote string
}

func (l *pyLexer) scan(line string) (bool, bool) {
	opened := false
	for i := 0; i < len(line); {
		if l.quote != "" {
			end := -1
			for j := i; j < len(line); j++ {
				if line[j] == '\\' {
					j++
					continue
				}
				if strings.HasPrefix(line[j:], l.quote) {
					end = j + len(l.quote)
					break
				}
			}
			if end < 0 {
				return true, opened
			}
			l.quote, opened = "", false
			i = end
			continue
		}
		switch c := line[i]; c {
		case '#':
			return false, false
		case '"', '\'':
			if triple := strings.Repeat(string(c), 3); strings.HasPrefix(line[i:], triple) {
				l.quote, opened = triple, true
				i += 3
			} else {
				i = skipQuoted(line, i)
			}
		default:
			i++
		}
	}
	return l.quote != "", opened && l.quote != ""
}

// fenceOpen — открывающая ограда блока кода markdown (CommonMark): до трёх
// пробелов отступа, не меньше трёх ` или ~, дальше строка информации.
var fenceOpen = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")

// fenceLexer — ограждённые блоки кода markdown. Закрывает блок ограда того
// же символа и не короче открывающей, без текста после неё.
type fenceLexer struct {
	char byte
	n    int
}

func (l *fenceLexer) scan(line string) (bool, bool) {
	if l.n == 0 {
		m := fenceOpen.FindStringSubmatch(line)
		if m == nil || (m[1][0] == '`' && strings.Contains(m[2], "`")) {
			// У ограды из ` в строке информации обратной кавычки не бывает:
			// это строка с кодом в тексте, а не блок.
			return false, false
		}
		l.char, l.n = m[1][0], len(m[1])
		return true, true
	}
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) <= 3 {
		run := 0
		for run < len(t) && t[run] == l.char {
			run++
		}
		if run >= l.n && strings.TrimSpace(t[run:]) == "" {
			l.n = 0
			return false, false
		}
	}
	return true, false
}
