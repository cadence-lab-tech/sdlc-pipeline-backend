package main

// Мостики clab setup в Codex (CLAB-418, решение 0077, п. 9): MCP-сервер
// проекта и, по явному флагу, запись доверия проекту. Форма команд проверена
// на codex 0.167.0: `codex mcp add <имя> --url <адрес> --bearer-token-env-var
// <ПЕРЕМЕННАЯ>` пишет только [mcp_servers.<имя>] в $CODEX_HOME/config.toml
// (по умолчанию ~/.codex), `codex mcp get <имя> --json` отвечает 1, если
// сервера нет. Токен в аргументах и файлах не бывает: Codex читает его из
// переменной окружения при запуске.
//
// Пользовательский config.toml — чужой файл: clab добавляет в него текстом
// только таблицы, которые перечислены здесь, и не пересобирает его целиком,
// чтобы не потерять комментарии и порядок.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// connectCodexMCP подключает сервер проекта в пользовательский конфиг
// Codex. Под тем же именем тот же адрес и та же переменная токена — ничего
// не делает; иначе сервер заменяется (remove, add). Команды идут из домашнего
// каталога: `codex mcp get` видит и проектный слой доверенного каталога, а
// здесь нужен только пользовательский.
//
// secret — токен clab: в тексте ошибки Codex его не будет.
func connectCodexMCP(ctx context.Context, codex, name, mcpURL, secret string) (action, previous string, err error) {
	dir, _ := os.UserHomeDir()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, codex, args...)
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if secret != "" {
				msg = strings.ReplaceAll(msg, secret, "***")
			}
			return out, fmt.Errorf("codex %s: %w: %s", strings.Join(args[:2], " "), err, msg)
		}
		return out, nil
	}
	action = mcpAdded
	// codex mcp get отвечает ненулевым кодом, если сервера нет.
	if out, err := run("mcp", "get", name, "--json"); err == nil {
		var cur struct {
			Transport struct {
				URL               string         `json:"url"`
				BearerTokenEnvVar string         `json:"bearer_token_env_var"`
				HTTPHeaders       map[string]any `json:"http_headers"`
			} `json:"transport"`
		}
		_ = json.Unmarshal(out, &cur)
		t := cur.Transport
		if t.URL == mcpURL && t.BearerTokenEnvVar == codexTokenEnv && len(t.HTTPHeaders) == 0 {
			return mcpUnchanged, "", nil
		}
		if _, err := run("mcp", "remove", name); err != nil {
			return "", "", err
		}
		action, previous = mcpReplaced, t.URL
	}
	if _, err := run("mcp", "add", name, "--url", mcpURL, "--bearer-token-env-var", codexTokenEnv); err != nil {
		return "", "", err
	}
	return action, previous, nil
}

// tomlQuote — строка TOML в двойных кавычках.
func tomlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return `"` + s + `"`
}

// tomlKey — ключ таблицы: голый, если из букв, цифр, - и _, иначе в кавычках.
func tomlKey(s string) string {
	for _, r := range s {
		if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return tomlQuote(s)
		}
	}
	if s == "" {
		return tomlQuote(s)
	}
	return s
}

// appendTOML дописывает таблицу в конец файла, не трогая прежние байты.
// Файл, на который указывает ссылка, правится по ссылке: запись через
// rename заменила бы саму ссылку.
func appendTOML(file string, table string, dirMode, fileMode os.FileMode) error {
	target := file
	if real, err := filepath.EvalSymlinks(file); err == nil {
		target = real
	}
	raw, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	mode := fileMode
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}
	var buf bytes.Buffer
	buf.Write(raw)
	if len(raw) > 0 {
		if !bytes.HasSuffix(raw, []byte("\n")) {
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString(table)
	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		return err
	}
	return writeAtomicMode(target, buf.Bytes(), mode)
}

func readTOML(file string) (map[string]any, error) {
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s не разбирается как TOML: %w", file, err)
	}
	return doc, nil
}

// codexProjectMCP — [mcp_servers.<имя>] в .codex/config.toml корня проекта
// (--scope project): адрес проекта и переменная токена, самого токена нет.
// Codex читает файл только у доверенного проекта. Запись с другим адресом не
// правится: это файл проекта, его ведёт человек. В --dry-run файл не
// пишется.
func codexProjectMCP(root, name, mcpURL string, dryRun bool) (action, previous string, err error) {
	file := filepath.Join(root, ".codex", "config.toml")
	doc, err := readTOML(file)
	if err != nil {
		return "", "", err
	}
	servers, _ := doc["mcp_servers"].(map[string]any)
	if s, ok := servers[name].(map[string]any); ok {
		cur, _ := s["url"].(string)
		env, _ := s["bearer_token_env_var"].(string)
		if cur == mcpURL && env == codexTokenEnv {
			return mcpUnchanged, "", nil
		}
		return mcpManual, cur, nil
	}
	if dryRun {
		return mcpAdded, "", nil
	}
	table := "[mcp_servers." + tomlKey(name) + "]\nurl = " + tomlQuote(mcpURL) + "\nbearer_token_env_var = " + tomlQuote(codexTokenEnv) + "\n"
	if err := appendTOML(file, table, 0o755, 0o644); err != nil {
		return "", "", err
	}
	return mcpAdded, "", nil
}

// Действия с записью доверия.
const (
	trustNotRequested = "not-requested"
	trustAdded        = "added"
	trustUnchanged    = "unchanged"
	trustConflict     = "conflict"
	trustUnreadable   = "unreadable"
)

type trustReport struct {
	Requested bool     `json:"requested"`
	File      string   `json:"file"`
	Path      string   `json:"path"`
	Action    string   `json:"action"`
	Lines     []string `json:"lines"`
	// Level — уровень доверия, который уже записан для этого пути.
	Level string `json:"existing_level,omitempty"`
}

// codexConfigPath — пользовательский конфиг Codex: $CODEX_HOME или ~/.codex.
func codexConfigPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

// trustPath — корень основной копии: linked worktree доверяется по нему
// (проверка 3 решения 0077), одна запись покрывает все копии задачи. Вне git
// и в «голом» репозитории — сам каталог.
func trustPath(ctx context.Context, root string) string {
	p := root
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-common-dir").Output()
	if gc := strings.TrimSpace(string(out)); err == nil && gc != "" {
		if !filepath.IsAbs(gc) {
			gc = filepath.Join(root, gc)
		}
		if gc = filepath.Clean(gc); filepath.Base(gc) == ".git" {
			p = filepath.Dir(gc)
		}
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p
}

// setupTrust — доверие Codex каталогу запуска. Без requested ничего не
// пишется, но строки для ручной записи готовы. Запись — текстом в конец
// пользовательского конфига; чужое значение для этого пути не правится.
func setupTrust(ctx context.Context, root string, requested, dryRun bool) (*trustReport, error) {
	p := trustPath(ctx, root)
	file := codexConfigPath()
	tr := &trustReport{
		Requested: requested, File: file, Path: p, Action: trustNotRequested,
		Lines: []string{"[projects." + tomlQuote(p) + "]", `trust_level = "trusted"`},
	}
	doc, err := readTOML(file)
	if err != nil {
		if requested {
			tr.Action = trustUnreadable
			return tr, fmt.Errorf("--trust: %w; запись доверия не сделана", err)
		}
		return tr, nil
	}
	if projects, ok := doc["projects"].(map[string]any); ok {
		if entry, ok := projects[p].(map[string]any); ok {
			level, _ := entry["trust_level"].(string)
			tr.Level = level
			if level == "trusted" {
				tr.Action = trustUnchanged
			} else {
				tr.Action = trustConflict
			}
			return tr, nil
		}
	}
	if !requested {
		return tr, nil
	}
	tr.Action = trustAdded
	if dryRun {
		return tr, nil
	}
	if err := appendTOML(file, strings.Join(tr.Lines, "\n")+"\n", 0o700, 0o600); err != nil {
		return tr, err
	}
	return tr, nil
}
