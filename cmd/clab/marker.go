package main

// Строка привязки каталога (решение 0077, п. 6): блок в AGENTS.md или
// CLAUDE.md говорит и ИИ, и clab, с каким проектом и инстансом работают в
// этом каталоге.
//
//	<!-- cadence-lab:begin -->
//	<!-- cadence-lab: project=ACME host=https://sdlc.example.com -->
//	Cadence Lab: this directory works with project ACME at https://sdlc.example.com; …
//	<!-- cadence-lab:end -->
//
// clab читает только строку маркера: прозу файла он не разбирает и в
// сообщения не выводит. Маркер ищется от текущего каталога вверх; с
// CLAB_CONFIG_DIR (исполнитель этапа, решение 0049) не читается вовсе —
// это решает NewClient, не этот файл.

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	markerBegin  = "<!-- cadence-lab:begin -->"
	markerEnd    = "<!-- cadence-lab:end -->"
	markerPrefix = "cadence-lab:"
	// markerReadLimit — сколько байт файла читается в поисках маркера.
	// Блок пишется в конец файла, а AGENTS.md и CLAUDE.md — текст в
	// десятки килобайт; бинарный или огромный файл дальше предела не
	// читается, и маркер за пределом не виден.
	markerReadLimit = 1 << 20
)

// markerFiles — где ищется маркер в каждом каталоге, по порядку.
var markerFiles = []string{"AGENTS.md", "CLAUDE.md"}

// dirMarker — разобранный маркер.
type dirMarker struct {
	// Path — файл, в котором найден маркер, как он назван при поиске.
	Path    string
	Project string
	// Host — ключ хоста (как в конфиге, normalizeHost), Origin — адрес из
	// маркера; оба пусты, если в маркере хоста нет.
	Host   string
	Origin string
}

// markerError — маркер найден, но не принят. Текст называет файл и
// причину, но не содержимое файла.
type markerError struct {
	path string
	msg  string
}

func (e *markerError) Error() string { return "маркер " + e.path + ": " + e.msg }

// markerReadFile читает файл-кандидат; в тестах подменяется, чтобы
// убедиться, что с CLAB_CONFIG_DIR файлы не открываются вовсе.
var markerReadFile = readMarkerFile

var errNotRegular = errors.New("не обычный файл")

func readMarkerFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	return io.ReadAll(io.LimitReader(f, markerReadLimit))
}

// loadDirMarker — маркер для текущего каталога.
func loadDirMarker() (*dirMarker, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, nil
	}
	return findMarker(wd)
}

// findMarker идёт от start вверх до корня файловой системы. В каждом
// каталоге смотрит AGENTS.md, затем CLAUDE.md; файл без маркера
// пропускается. Ближайший каталог с маркером — ответ; разные маркеры в двух
// файлах одного каталога — ошибка с путями обоих. Одинаковые (так пишет
// clab setup, так же выглядит AGENTS.md — ссылка на CLAUDE.md) — не
// конфликт, берётся AGENTS.md.
func findMarker(start string) (*dirMarker, error) {
	dir := filepath.Clean(start)
	for {
		var found []*dirMarker
		for _, name := range markerFiles {
			path := filepath.Join(dir, name)
			raw, err := markerReadFile(path)
			if err != nil {
				// Нет файла, каталог с таким именем, нет прав — не наш файл.
				continue
			}
			m, err := parseMarkerFile(path, raw)
			if err != nil {
				return nil, err
			}
			if m != nil {
				found = append(found, m)
			}
		}
		switch len(found) {
		case 1:
			return found[0], nil
		case 2:
			a, b := found[0], found[1]
			if a.Project == b.Project && a.Origin == b.Origin {
				return a, nil
			}
			return nil, fmt.Errorf("маркеры в %s и %s расходятся: оставьте один или сделайте их одинаковыми", a.Path, b.Path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
		dir = parent
	}
}

type markerLineKind int

const (
	markerLineText markerLineKind = iota
	markerLineBegin
	markerLineEnd
	markerLineMarker
)

// classifyMarkerLine — чем строка файла является для блока. Маркер — строка
// целиком «<!-- cadence-lab: … -->»; begin и end пишутся без пробела после
// двоеточия и маркером не считаются. body — текст маркера между
// двоеточием и «-->».
func classifyMarkerLine(line string) (kind markerLineKind, body string) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "<!--") {
		return markerLineText, ""
	}
	inner := strings.TrimSpace(t[len("<!--"):])
	if !strings.HasPrefix(inner, markerPrefix) {
		return markerLineText, ""
	}
	rest := inner[len(markerPrefix):]
	closed := strings.HasSuffix(rest, "-->")
	rest = strings.TrimSpace(strings.TrimSuffix(rest, "-->"))
	if closed {
		switch rest {
		case "begin":
			return markerLineBegin, ""
		case "end":
			return markerLineEnd, ""
		}
	}
	if !closed {
		// Незакрытый комментарий: маркер обязан быть одной строкой.
		return markerLineMarker, "\x00"
	}
	return markerLineMarker, rest
}

type markerLine struct {
	index int
	kind  markerLineKind
	body  string
}

// scanMarkerLines — строки блока и маркеры файла вне блоков кода (``` и
// ~~~): пример блока в документации маркером не считается. Прочие строки не
// возвращаются.
func scanMarkerLines(lines []string) []markerLine {
	var out []markerLine
	fence := ""
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") {
			fence = "```"
			continue
		}
		if strings.HasPrefix(t, "~~~") {
			fence = "~~~"
			continue
		}
		if kind, body := classifyMarkerLine(line); kind != markerLineText {
			out = append(out, markerLine{index: i, kind: kind, body: body})
		}
	}
	return out
}

// parseMarkerFile — маркер файла: nil, если его нет; ошибка, если маркер
// не принят или их два.
func parseMarkerFile(path string, raw []byte) (*dirMarker, error) {
	var found *dirMarker
	foundLine := 0
	for _, l := range scanMarkerLines(strings.Split(string(raw), "\n")) {
		if l.kind != markerLineMarker {
			continue
		}
		if found != nil {
			return nil, &markerError{path, fmt.Sprintf("два маркера в одном файле (строки %d и %d)", foundLine, l.index+1)}
		}
		m, err := parseMarkerBody(l.body)
		if err != nil {
			return nil, &markerError{path, fmt.Sprintf("строка %d: %v", l.index+1, err)}
		}
		m.Path = path
		found, foundLine = m, l.index+1
	}
	return found, nil
}

var (
	markerKeyRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	markerAttrRe = regexp.MustCompile(`^[a-z]{1,16}$`)
)

// parseMarkerBody разбирает «project=<ключ> host=<адрес>». Значения в
// ошибки не попадают: в неверном значении может оказаться токен.
func parseMarkerBody(body string) (*dirMarker, error) {
	if body == "\x00" {
		return nil, errors.New("маркер не закрыт «-->» на той же строке")
	}
	m := &dirMarker{}
	seen := map[string]bool{}
	for _, field := range strings.Fields(body) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			return nil, errors.New("в маркере есть слово без «=»: допустимы только project=<ключ> и host=<адрес>")
		}
		switch k {
		case "project", "host":
		default:
			if markerAttrRe.MatchString(k) {
				return nil, fmt.Errorf("неизвестный атрибут %q: допустимы только project и host", k)
			}
			return nil, errors.New("неизвестный атрибут: допустимы только project и host")
		}
		if seen[k] {
			return nil, fmt.Errorf("атрибут %s повторяется", k)
		}
		seen[k] = true
		switch k {
		case "project":
			if !markerKeyRe.MatchString(v) {
				return nil, errors.New("ключ проекта не годится: буквы, цифры, «-» и «_», до 64 знаков")
			}
			m.Project = v
		case "host":
			host, origin, err := markerHost(v)
			if err != nil {
				return nil, err
			}
			m.Host, m.Origin = host, origin
		}
	}
	if m.Project == "" {
		return nil, errors.New("в маркере нет project=<ключ>")
	}
	return m, nil
}

// markerHost — адрес инстанса из маркера: http или https, без логина,
// параметров и фрагмента. Маркер лежит в файле репозитория, и такой адрес —
// либо ошибка, либо попытка положить в файл секрет.
func markerHost(v string) (host, origin string, err error) {
	if v == "" {
		return "", "", errors.New("пустой host")
	}
	raw := v
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, perr := url.Parse(raw)
	switch {
	case perr != nil || u.Host == "":
		return "", "", errors.New("host не разбирается как адрес")
	case u.Scheme != "http" && u.Scheme != "https":
		return "", "", errors.New("host: допустимы только http и https")
	case u.User != nil:
		return "", "", errors.New("host с логином или паролем не принимается")
	case u.RawQuery != "" || u.ForceQuery:
		return "", "", errors.New("host с параметрами не принимается")
	case u.Fragment != "" || strings.Contains(v, "#"):
		return "", "", errors.New("host с фрагментом не принимается")
	}
	host, origin, err = normalizeHost(v)
	if err != nil {
		return "", "", errors.New("host не разбирается как адрес")
	}
	return host, origin, nil
}
