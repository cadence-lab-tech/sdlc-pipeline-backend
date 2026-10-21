package main

// clab setup — связать локальный проект с проектом Cadence Lab одной
// командой (CLAB-232, решение 0050): MCP-сервер проекта у Claude Code и набор
// агента (скилы, саб-агент исполнителя) в .claude/. Набор берётся с
// инстанса — GET /v1/agent-kit, — поэтому его версия всегда совпадает с
// версией сервера, а не CLI.
//
// Повторный запуск безвреден: то, что уже стоит, не трогается. Правку
// человека clab узнаёт по манифесту .claude/clab-kit.json (sha256 каждого
// поставленного файла) и без --force не перезаписывает. Токенов в файлах
// нет: токен уходит только аргументом в claude mcp add, в его локальную
// конфигурацию вне репозитория.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// kitManifest — имя манифеста рядом с набором.
const kitManifest = "clab-kit.json"

// Действия с файлами набора; те же слова — в --json.
const (
	fileCreate       = "create"
	fileUpdate       = "update"
	fileDelete       = "delete"
	fileUnchanged    = "unchanged"
	fileSkipModified = "skip-modified"
)

// Действия с MCP-сервером. check — только в --dry-run: claude там не
// зовётся, и что именно будет (добавить или заменить), решит настоящий
// запуск.
const (
	mcpAdded     = "added"
	mcpUnchanged = "unchanged"
	mcpReplaced  = "replaced"
	mcpSkipped   = "skipped"
	mcpCheck     = "check"
)

type agentKit struct {
	Version string `json:"version"`
	Files   []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		SHA256  string `json:"sha256"`
	} `json:"files"`
}

type kitState struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

type fileAction struct {
	Path   string `json:"path"`
	Action string `json:"action"`

	content []byte
}

type setupReport struct {
	Project       string       `json:"project"`
	URL           string       `json:"url"`
	Name          string       `json:"name"`
	Scope         string       `json:"scope"`
	Agent         string       `json:"agent"`
	DryRun        bool         `json:"dry_run"`
	KitVersion    *string      `json:"kit_version"`
	KitDir        string       `json:"kit_dir,omitempty"`
	ServerVersion string       `json:"server_version,omitempty"`
	MCP           string       `json:"mcp"`
	MCPPrevious   string       `json:"mcp_previous_url,omitempty"`
	Files         []fileAction `json:"files"`
	Docs          string       `json:"docs"`
	// Config — только у --agent other: конфигурация для ручного
	// подключения, с тем же заголовком, что печатает clab mcp config.
	Config map[string]any `json:"config,omitempty"`
}

func cmdSetup(ctx context.Context, c *Client, args []string) error {
	var projectFlag, scope, agent, name string
	var dryRun, force bool
	_, err := flags("setup", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта; иначе SDLC_PROJECT или проект по git remote")
		fs.StringVar(&scope, "scope", "local", "local — набор в .claude/ корня проекта и MCP для этого каталога; user — набор в ~/.claude/ и MCP везде; project — MCP в .mcp.json (только без токена)")
		fs.StringVar(&agent, "agent", "claude", "claude — подключить Claude Code; other — только напечатать конфигурацию MCP для другого агента")
		fs.StringVar(&name, "name", "sdlc", "имя MCP-сервера у агента")
		fs.BoolVar(&dryRun, "dry-run", false, "показать, что будет сделано, ничего не меняя и не вызывая claude")
		fs.BoolVar(&force, "force", false, "перезаписать и файлы набора, изменённые после установки")
	})
	if err != nil {
		return err
	}
	switch scope {
	case "local", "user", "project":
	default:
		return fmt.Errorf("--scope: local, user или project")
	}
	switch agent {
	case "claude", "other":
	default:
		return fmt.Errorf("--agent: claude или other")
	}
	if scope == "project" && c.token != "" {
		return errProjectScopeToken
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	mcpURL, headerName, headerValue := c.mcpEndpoint(key)
	rep := &setupReport{
		Project: key, URL: mcpURL, Name: name, Scope: scope, Agent: agent, DryRun: dryRun,
		Files: []fileAction{}, Docs: c.origin + "/docs?project=" + url.QueryEscape(key),
	}

	if agent == "other" {
		// Другой агент: ни claude, ни файлов — конфигурация и страница.
		rep.MCP = mcpSkipped
		rep.Config = mcpConfig(name, mcpURL, headerName, headerValue)
		if jsonOut {
			return print(rep)
		}
		raw, _ := json.MarshalIndent(rep.Config, "", "  ")
		fmt.Printf("Конфигурация MCP проекта %s для вашего агента:\n%s\n", key, raw)
		fmt.Printf("Инструкция: %s\n", rep.Docs)
		return nil
	}

	kit, serverVersion, err := c.fetchAgentKit(ctx)
	if err != nil {
		return err
	}
	rep.ServerVersion = serverVersion
	var root, dir string
	var plan []fileAction
	var next []byte
	if kit != nil {
		if root, dir, err = kitDir(ctx, scope); err != nil {
			return err
		}
		rep.KitVersion, rep.KitDir = &kit.Version, dir
		if plan, next, err = planKit(dir, kit, force); err != nil {
			return err
		}
		rep.Files = plan
	} else if scope != "user" {
		root, _ = projectRoot(ctx)
	}

	if !dryRun && kit != nil {
		if err := applyKit(dir, plan, next); err != nil {
			return err
		}
	}

	claude, lookErr := exec.LookPath("claude")
	switch {
	case lookErr != nil:
		rep.MCP = mcpSkipped
	case dryRun:
		rep.MCP = mcpCheck
	default:
		header := headerName + ": " + headerValue
		if rep.MCP, rep.MCPPrevious, err = connectMCP(ctx, claude, root, scope, name, mcpURL, header, headerValue); err != nil {
			return err
		}
	}

	if err := emit(rep, func() error { printSetup(rep, dir); return nil }); err != nil {
		return err
	}
	if lookErr != nil && !dryRun {
		raw, _ := json.MarshalIndent(mcpConfig(name, mcpURL, headerName, headerValue), "", "  ")
		return fmt.Errorf("claude не найден в PATH: MCP не подключён. Конфигурация для агента:\n%s", raw)
	}
	return nil
}

// fetchAgentKit — набор с инстанса. Сервер старше ручки (404) — не ошибка:
// набора нет, а версия сервера нужна, чтобы сказать человеку, что обновлять.
func (c *Client) fetchAgentKit(ctx context.Context) (*agentKit, string, error) {
	var kit agentKit
	err := c.get(ctx, "/agent-kit", nil, &kit)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		var info instanceInfo
		if c.get(ctx, "/instance", nil, &info) == nil {
			return nil, info.Version, nil
		}
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	for _, f := range kit.Files {
		if err := kitPathValid(f.Path); err != nil {
			return nil, "", err
		}
		sum := sha256.Sum256([]byte(f.Content))
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, "", fmt.Errorf("набор агента: sha256 файла %s не сходится с содержимым — ничего не записано", f.Path)
		}
	}
	return &kit, kit.Version, nil
}

// kitPathValid — набор пишет только скилы и агентов внутри своего каталога:
// ни абсолютных путей, ни выхода наверх, ни настроек агента с хуками.
func kitPathValid(p string) error {
	if p == "" || path.IsAbs(p) || strings.Contains(p, "\\") || path.Clean(p) != p ||
		strings.HasPrefix(p, "../") || p == ".." ||
		!(strings.HasPrefix(p, "skills/") || strings.HasPrefix(p, "agents/")) {
		return fmt.Errorf("набор агента: недопустимый путь %q — ничего не записано", p)
	}
	return nil
}

// projectRoot — корень локального проекта: верх git-репозитория текущего
// каталога, вне git — сам текущий каталог. В тестах подменяется.
var projectRoot = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root, nil
		}
	}
	return os.Getwd()
}

// kitDir — куда ставится набор: .claude/ корня проекта или каталог
// конфигурации Claude Code пользователя (~/.claude, CLAUDE_CONFIG_DIR).
func kitDir(ctx context.Context, scope string) (root, dir string, err error) {
	if scope == "user" {
		if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
			return "", d, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		return "", filepath.Join(home, ".claude"), nil
	}
	root, err = projectRoot(ctx)
	if err != nil {
		return "", "", err
	}
	return root, filepath.Join(root, ".claude"), nil
}

func fileSum(p string) (string, bool, error) {
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true, nil
}

// planKit сверяет набор с диском и манифестом прошлой установки. Файл,
// который на диске не совпадает ни с набором, ни с тем, что ставили в прошлый
// раз, — правка человека (или его собственный файл с тем же именем): без
// --force он не трогается. next — новый манифест; nil — он не изменился.
func planKit(dir string, kit *agentKit, force bool) ([]fileAction, []byte, error) {
	old := kitState{Files: map[string]string{}}
	if raw, err := os.ReadFile(filepath.Join(dir, kitManifest)); err == nil {
		if err := json.Unmarshal(raw, &old); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", filepath.Join(dir, kitManifest), err)
		}
		if old.Files == nil {
			old.Files = map[string]string{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	plan := []fileAction{}
	nm := kitState{Version: kit.Version, Files: map[string]string{}}
	inKit := map[string]bool{}
	for _, f := range kit.Files {
		inKit[f.Path] = true
		nm.Files[f.Path] = f.SHA256
		disk, exists, err := fileSum(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, nil, err
		}
		a := fileAction{Path: f.Path, content: []byte(f.Content)}
		switch {
		case !exists:
			a.Action = fileCreate
		case disk == f.SHA256:
			a.Action = fileUnchanged
		case old.Files[f.Path] == disk || force:
			a.Action = fileUpdate
		default:
			a.Action = fileSkipModified
		}
		plan = append(plan, a)
	}
	gone := make([]string, 0)
	for p := range old.Files {
		if !inKit[p] {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)
	for _, p := range gone {
		if kitPathValid(p) != nil {
			// Чужая строка в манифесте: удалять по ней ничего нельзя.
			continue
		}
		disk, exists, err := fileSum(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return nil, nil, err
		}
		switch {
		case !exists:
		case disk == old.Files[p] || force:
			plan = append(plan, fileAction{Path: p, Action: fileDelete})
		default:
			// Ушёл из набора, но правлен: остаётся файлом человека и из
			// манифеста выпадает — повторных предупреждений о нём не будет.
			plan = append(plan, fileAction{Path: p, Action: fileSkipModified})
		}
	}

	raw, err := json.MarshalIndent(nm, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	raw = append(raw, '\n')
	if cur, err := os.ReadFile(filepath.Join(dir, kitManifest)); err == nil && bytes.Equal(cur, raw) {
		raw = nil
	}
	return plan, raw, nil
}

func applyKit(dir string, plan []fileAction, next []byte) error {
	for _, a := range plan {
		target := filepath.Join(dir, filepath.FromSlash(a.Path))
		switch a.Action {
		case fileCreate, fileUpdate:
			if err := writeAtomic(target, a.content); err != nil {
				return err
			}
		case fileDelete:
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			// Опустевшие каталоги скила убираются; непустой Remove не тронет.
			for d := filepath.Dir(target); d != dir && strings.HasPrefix(d, dir); d = filepath.Dir(d) {
				if os.Remove(d) != nil {
					break
				}
			}
		}
	}
	if next != nil {
		return writeAtomic(filepath.Join(dir, kitManifest), next)
	}
	return nil
}

// writeAtomic — запись через временный файл рядом: оборванный clab setup не
// оставляет полфайла скила.
func writeAtomic(target string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".clab-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// connectMCP подключает MCP-сервер проекта к Claude Code. Под тем же именем
// уже подключён тот же адрес — ничего не делает; другой адрес в той же
// области — заменяет. Сервер с этим именем в другой области не трогается:
// он может обслуживать другие проекты, а local перекрывает user.
// Команды claude идут из корня проекта: local-область Claude Code
// привязывает к каталогу, из которого её добавили.
//
// secret — значение заголовка: в тексте ошибки claude его не будет.
func connectMCP(ctx context.Context, claude, root, scope, name, mcpURL, header, secret string) (action, previous string, err error) {
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, claude, args...)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if secret != "" {
				msg = strings.ReplaceAll(msg, secret, "***")
			}
			return out, fmt.Errorf("claude %s: %w: %s", strings.Join(args[:2], " "), err, msg)
		}
		return out, nil
	}
	action = mcpAdded
	// claude mcp get отвечает ненулевым кодом, если сервера нет.
	if out, err := run("mcp", "get", name); err == nil {
		curURL, curScope := parseMCPGet(string(out))
		switch {
		case curURL == mcpURL:
			return mcpUnchanged, "", nil
		case curScope == "" || curScope == scope:
			if _, err := run("mcp", "remove", name, "--scope", scope); err != nil {
				return "", "", err
			}
			action, previous = mcpReplaced, curURL
		}
	}
	if _, err := run("mcp", "add", "--transport", "http", "--scope", scope, name, mcpURL, "--header", header); err != nil {
		return "", "", err
	}
	return action, previous, nil
}

// parseMCPGet достаёт адрес и область из вывода claude mcp get: строки
// «URL: …» и «Scope: Local config (…)». Заголовки (с токеном) не читаются.
func parseMCPGet(out string) (mcpURL, scope string) {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "URL":
			if mcpURL == "" {
				mcpURL = v
			}
		case "Scope":
			switch low := strings.ToLower(v); {
			case strings.HasPrefix(low, "local"):
				scope = "local"
			case strings.HasPrefix(low, "user"):
				scope = "user"
			case strings.HasPrefix(low, "project"):
				scope = "project"
			}
		}
	}
	return mcpURL, scope
}

var fileLabels = map[bool]map[string]string{
	false: {fileCreate: "создан", fileUpdate: "обновлён", fileDelete: "удалён", fileSkipModified: "пропущен"},
	true:  {fileCreate: "создать", fileUpdate: "обновить", fileDelete: "удалить", fileSkipModified: "пропустить"},
}

func printSetup(rep *setupReport, dir string) {
	if rep.DryRun {
		fmt.Println("--dry-run: ничего не записано, claude не вызывался.")
	}
	fmt.Printf("Проект:  %s\n", rep.Project)

	mcp := map[string]string{
		mcpAdded:     "подключён",
		mcpUnchanged: "уже подключён",
		mcpReplaced:  "заменён (был " + rep.MCPPrevious + ")",
		mcpSkipped:   "не подключён: claude не найден в PATH",
		mcpCheck:     "будет проверен: добавлен, если не подключён, или заменён, если адрес другой",
	}[rep.MCP]
	if rep.DryRun && rep.MCP == mcpSkipped {
		mcp = "не будет подключён: claude не найден в PATH"
	}
	fmt.Printf("MCP:     %s → %s (%s): %s\n", rep.Name, rep.URL, rep.Scope, mcp)

	changed := false
	if rep.KitVersion == nil {
		fmt.Printf("Набор:   сервер %s не раздаёт набор агента — он приходит с сервера начиная с выпуска с clab setup (этот clab — %s). Обновите инстанс и запустите clab setup ещё раз.\n",
			dash(rep.ServerVersion), version)
	} else {
		fmt.Printf("Набор:   %s → %s\n", *rep.KitVersion, dir)
		labels := fileLabels[rep.DryRun]
		for _, f := range rep.Files {
			if f.Action == fileUnchanged {
				continue
			}
			changed = true
			if f.Action == fileSkipModified {
				fmt.Printf("  %-10s %s — изменён после установки; --force перезапишет\n", labels[f.Action], filepath.Join(dir, filepath.FromSlash(f.Path)))
				continue
			}
			fmt.Printf("  %-10s %s\n", labels[f.Action], f.Path)
		}
		if !changed {
			fmt.Println("  файлы набора актуальны")
		}
	}
	if !rep.DryRun && rep.MCP == mcpUnchanged && rep.KitVersion != nil && !changed {
		fmt.Println("Уже актуально.")
	}
	fmt.Printf("Дальше: в Claude Code — /clab-work %s-<номер>; инструкция — %s\n", rep.Project, rep.Docs)
}
