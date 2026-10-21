// Package agentkit — набор агента, который инстанс раздаёт пользователям:
// скилы и определение саб-агента для Claude Code (CLAB-232, решение 0050).
//
// Набор лежит исходниками рядом с кодом и встраивается в бинарник api, как
// migrations: его версия — версия сборки сервера, и клиент self-hosted
// получает ровно тот набор, что соответствует его инстансу. Отдаёт его
// открытая ручка GET /v1/agent-kit, ставит — clab setup.
//
// Пути файлов — относительно каталога конфигурации агента (.claude/ проекта
// или ~/.claude/): skills/<имя>/SKILL.md, agents/<имя>.md. Тексты — на
// английском и без привязки к конкретному инстансу, проекту и циклу: это
// проверяет kit_test.go.
package agentkit

import (
	"bufio"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

//go:embed skills agents
var source embed.FS

// File — файл набора с содержимым и его sha256: по сумме clab setup узнаёт,
// правил ли файл человек после установки.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

// Entry — скил или саб-агент набора, как его видит человек на странице
// /docs: имя и описание из фронтматтера.
type Entry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Kit — ответ GET /v1/agent-kit. Объект, а не массив: новые сведения
// (например, для clab auth login) ложатся новыми полями, не ломая clab
// прошлой версии.
type Kit struct {
	Version string  `json:"version"`
	Files   []File  `json:"files"`
	Skills  []Entry `json:"skills"`
	Agents  []Entry `json:"agents"`
}

var (
	loadOnce sync.Once
	loaded   Kit
	loadErr  error
)

// Load собирает набор из встроенных файлов один раз на процесс: содержимое
// не меняется до следующей сборки. Версию подставляет вызывающий — набор её
// не знает.
func Load(version string) (Kit, error) {
	loadOnce.Do(func() { loaded, loadErr = build(source) })
	if loadErr != nil {
		return Kit{}, loadErr
	}
	k := loaded
	k.Version = version
	return k, nil
}

func build(fsys fs.FS) (Kit, error) {
	k := Kit{Files: []File{}, Skills: []Entry{}, Agents: []Entry{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		k.Files = append(k.Files, File{Path: p, Content: string(raw), SHA256: hex.EncodeToString(sum[:])})

		meta := Frontmatter(string(raw))
		switch {
		case path.Base(p) == "SKILL.md" && strings.HasPrefix(p, "skills/"):
			k.Skills = append(k.Skills, Entry{Name: meta["name"], Description: meta["description"]})
		case strings.HasPrefix(p, "agents/") && path.Ext(p) == ".md":
			k.Agents = append(k.Agents, Entry{Name: meta["name"], Description: meta["description"]})
		}
		return nil
	})
	if err != nil {
		return Kit{}, fmt.Errorf("agentkit: %w", err)
	}
	sort.Slice(k.Files, func(i, j int) bool { return k.Files[i].Path < k.Files[j].Path })
	sort.Slice(k.Skills, func(i, j int) bool { return k.Skills[i].Name < k.Skills[j].Name })
	sort.Slice(k.Agents, func(i, j int) bool { return k.Agents[i].Name < k.Agents[j].Name })
	return k, nil
}

// Frontmatter читает верхнеуровневые поля «ключ: значение» между первыми
// строками «---». Вложенные блоки (hooks у агента) пропускаются: набору
// нужны только имя, описание и перечень тулов, а YAML-библиотека ради трёх
// строк в зависимости не идёт.
func Frontmatter(content string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return out
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			return out
		}
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		out[strings.TrimSpace(key)] = value
	}
	// Фронтматтер не закрыт — его нет.
	return map[string]string{}
}
