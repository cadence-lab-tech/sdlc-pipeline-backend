package main

import (
	"fmt"
	"strings"
)

// parseDotEnv читает .env установки так же, как его читают скрипты пакета
// (set -a; . ./.env) и compose: KEY=value, необязательный export, строки
// комментариев, значения в одинарных кавычках (буквально; так их пишет
// install.sh, set_kv), в двойных (экранирование \" \\ \$ \` и подстановка
// $VAR) и без кавычек (подстановка $VAR, комментарий после пробела).
// Значение в кавычках может занимать несколько строк. Подстановка берёт уже
// прочитанные ключи, затем окружение процесса — как shell.
//
// Всё, что shell выполнил бы как команду, здесь — ошибка с номером строки:
// .env, который скрипт не смог бы прочитать, не должен молча читаться иначе.
func parseDotEnv(content string, getenv func(string) string) (map[string]string, error) {
	p := &envParser{src: []rune(content), line: 1, getenv: getenv, vars: map[string]string{}}
	for {
		p.skipBlankAndComments()
		if p.eof() {
			return p.vars, nil
		}
		if err := p.assignment(); err != nil {
			return nil, err
		}
	}
}

type envParser struct {
	src    []rune
	pos    int
	line   int
	getenv func(string) string
	vars   map[string]string
}

func (p *envParser) eof() bool  { return p.pos >= len(p.src) }
func (p *envParser) peek() rune { return p.src[p.pos] }
func (p *envParser) next() rune {
	r := p.src[p.pos]
	p.pos++
	if r == '\n' {
		p.line++
	}
	return r
}

func (p *envParser) errorf(format string, a ...any) error {
	return fmt.Errorf("line %d: %s", p.line, fmt.Sprintf(format, a...))
}

// skipBlankAndComments пропускает пробелы, пустые строки и строки-комментарии.
func (p *envParser) skipBlankAndComments() {
	for !p.eof() {
		switch r := p.peek(); {
		case r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == ';':
			p.next()
		case r == '#':
			for !p.eof() && p.peek() != '\n' {
				p.next()
			}
		default:
			return
		}
	}
}

func isNameStart(r rune) bool { return r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') }
func isNameRune(r rune) bool  { return isNameStart(r) || (r >= '0' && r <= '9') }

func (p *envParser) name() string {
	start := p.pos
	if p.eof() || !isNameStart(p.peek()) {
		return ""
	}
	for !p.eof() && isNameRune(p.peek()) {
		p.next()
	}
	return string(p.src[start:p.pos])
}

func (p *envParser) assignment() error {
	key := p.name()
	if key == "export" && !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
			p.next()
		}
		key = p.name()
	}
	if key == "" || p.eof() || p.peek() != '=' {
		return p.errorf("expected KEY=value")
	}
	p.next()
	value, err := p.value()
	if err != nil {
		return err
	}
	// После значения — только пробелы и комментарий до конца строки: иначе
	// shell выполнил бы остаток как команду.
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t' || p.peek() == '\r') {
		p.next()
	}
	if !p.eof() && p.peek() != '\n' && p.peek() != '#' && p.peek() != ';' {
		return p.errorf("unexpected text after the value of %s (quote values with spaces)", key)
	}
	for !p.eof() && p.peek() != '\n' && p.peek() != ';' {
		p.next()
	}
	p.vars[key] = value
	return nil
}

// value читает слово shell до первого пробела вне кавычек.
func (p *envParser) value() (string, error) {
	var b strings.Builder
	for !p.eof() {
		r := p.peek()
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ';':
			return b.String(), nil
		case r == '\'':
			p.next()
			closed := false
			for !p.eof() {
				c := p.next()
				if c == '\'' {
					closed = true
					break
				}
				b.WriteRune(c)
			}
			if !closed {
				return "", p.errorf("unterminated single quote")
			}
		case r == '"':
			p.next()
			closed := false
			for !p.eof() && !closed {
				c := p.next()
				switch c {
				case '"':
					closed = true
				case '\\':
					if p.eof() {
						return "", p.errorf("unterminated double quote")
					}
					n := p.next()
					switch n {
					case '"', '\\', '$', '`':
						b.WriteRune(n)
					case '\n':
					default:
						b.WriteRune('\\')
						b.WriteRune(n)
					}
				case '$':
					if err := p.expand(&b); err != nil {
						return "", err
					}
				case '`':
					return "", p.errorf("command substitution is not supported in .env")
				default:
					b.WriteRune(c)
				}
			}
			if !closed {
				return "", p.errorf("unterminated double quote")
			}
		case r == '\\':
			p.next()
			if !p.eof() {
				n := p.next()
				if n != '\n' {
					b.WriteRune(n)
				}
			}
		case r == '$':
			p.next()
			if err := p.expand(&b); err != nil {
				return "", err
			}
		case r == '`' || r == '(' || r == ')' || r == '|' || r == '&' || r == '<' || r == '>':
			return "", p.errorf("shell syntax %q is not supported in .env", string(r))
		default:
			b.WriteRune(p.next())
		}
	}
	return b.String(), nil
}

// expand подставляет $NAME или ${NAME} — после уже прочитанного «$».
func (p *envParser) expand(b *strings.Builder) error {
	if p.eof() {
		b.WriteRune('$')
		return nil
	}
	if p.peek() == '{' {
		p.next()
		name := p.name()
		if name == "" || p.eof() || p.peek() != '}' {
			return p.errorf("only ${NAME} expansions are supported in .env")
		}
		p.next()
		b.WriteString(p.lookup(name))
		return nil
	}
	if p.peek() == '(' {
		return p.errorf("command substitution is not supported in .env")
	}
	name := p.name()
	if name == "" {
		b.WriteRune('$')
		return nil
	}
	b.WriteString(p.lookup(name))
	return nil
}

func (p *envParser) lookup(name string) string {
	if v, ok := p.vars[name]; ok {
		return v
	}
	if p.getenv != nil {
		return p.getenv(name)
	}
	return ""
}
