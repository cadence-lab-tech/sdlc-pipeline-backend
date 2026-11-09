package main

// clab setup — связать локальный проект с проектом Cadence Lab одной
// командой (CLAB-232, решение 0050; агенты по привязкам — CLAB-418, решение
// 0077): каждому найденному в PATH агенту (claude, codex; иначе generic)
// ставится набор — скилы и агент исполнителя — туда, где агент его найдёт, и
// подключается MCP-сервер проекта способом его привязки. Набор берётся с
// инстанса — GET /v1/agent-kit, — поэтому его версия всегда совпадает с
// версией сервера, а не CLI.
//
// Повторный запуск безвреден: то, что уже стоит, не трогается. Правку
// человека clab узнаёт по манифесту рядом с набором (sha256 каждого
// поставленного файла) и без --force не перезаписывает. Токенов в файлах
// нет: у Claude Code токен уходит аргументом в claude mcp add (в его
// локальную конфигурацию вне репозитория), у Codex — вообще не уходит:
// сервер читает его из переменной окружения SDLC_TOKEN.
//
// Файл агента чужой: clab не пишет своих ключей в .claude/settings*.json и
// в config.toml Codex. Исключения — штатная запись MCP-сервера и, по
// явному --trust, запись доверия проекту.

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
	"slices"
	"sort"
	"strings"
)

// kitManifest — имя манифеста рядом с набором Claude.
const kitManifest = "clab-kit.json"

// Действия с файлами набора; те же слова — в --json.
const (
	fileCreate       = "create"
	fileUpdate       = "update"
	fileDelete       = "delete"
	fileUnchanged    = "unchanged"
	fileSkipModified = "skip-modified"
)

// Действия с MCP-сервером. check — только в --dry-run: агент там не
// зовётся, и что именно будет (добавить или заменить), решит настоящий
// запуск. manual — запись есть и ведёт на другой адрес, а чужой файл
// настроек clab не правит. failed — подключение не удалось.
const (
	mcpAdded     = "added"
	mcpUnchanged = "unchanged"
	mcpReplaced  = "replaced"
	mcpSkipped   = "skipped"
	mcpCheck     = "check"
	mcpManual    = "manual"
	mcpFailed    = "failed"
)

// kitEntry — файл набора с содержимым и суммой.
type kitEntry struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

// kitLayout — где привязка кладёт набор: пути от корня проекта.
type kitLayout struct {
	Manifest  string `json:"manifest"`
	SkillsDir string `json:"skills_dir"`
	AgentsDir string `json:"agents_dir"`
}

// kitBinding — из привязки набору нужны место и готовые файлы; признак и
// таблицу уровней читает parseBindings.
type kitBinding struct {
	Name  string     `json:"name"`
	Kit   *kitLayout `json:"kit"`
	Files []kitEntry `json:"files"`
}

type agentKit struct {
	Version string     `json:"version"`
	Files   []kitEntry `json:"files"`
	Core    *struct {
		Skills []kitEntry `json:"skills"`
	} `json:"core"`
	RawBindings json.RawMessage `json:"bindings"`

	// Bindings — привязки с местом и файлами; пусто у сервера до
	// CLAB-417. Levels — привязки для таблицы моделей.
	Bindings []kitBinding   `json:"-"`
	Levels   []agentBinding `json:"-"`
	// Skipped — имена привязок агентов, которых clab не знает: не проверяются
	// и не используются, setup предупреждает.
	Skipped []string `json:"-"`
}

func (k *agentKit) binding(name string) *kitBinding {
	if k == nil {
		return nil
	}
	for i := range k.Bindings {
		if k.Bindings[i].Name == name {
			return &k.Bindings[i]
		}
	}
	return nil
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
	// Marker — блок привязки в AGENTS.md / CLAUDE.md каталога запуска
	// (решение 0077, п. 6); пустой список — --no-marker.
	Marker []markerAction `json:"marker"`
	Docs   string         `json:"docs"`
	// Config — у generic: конфигурация для ручного подключения, с тем же
	// заголовком, что печатает clab mcp config, но токен — ${SDLC_TOKEN}.
	Config map[string]any `json:"config,omitempty"`

	// Поля выше — первый из агентов, как до выбора агента по привязкам;
	// Agents — все поставленные.
	Agents []*agentReport `json:"agents"`
	// AgentSettingsVersion — активная версия настройки агентов проекта;
	// null — настройки нет и модели взяты из таблицы привязки.
	AgentSettingsVersion *int         `json:"agent_settings_version"`
	Trust                *trustReport `json:"trust,omitempty"`
	Notes                []string     `json:"notes,omitempty"`
	Warnings             []string     `json:"warnings,omitempty"`
}

func cmdSetup(ctx context.Context, c *Client, args []string) error {
	var projectFlag, scope, agent, name string
	var dryRun, force, plugin, noMarker, trust bool
	var markerFiles repeated
	fs, err := flags("setup", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта; иначе SDLC_PROJECT или проект по git remote")
		fs.StringVar(&scope, "scope", "", "у claude: local (по умолчанию) — набор в .claude/ корня проекта и MCP для этого каталога; user — набор в ~/.claude/ и MCP везде; project — MCP в .mcp.json (только без токена). У codex: user (по умолчанию) — MCP в ~/.codex/config.toml; project — MCP в .codex/config.toml корня проекта (без токена; Codex читает его у доверенного проекта)")
		fs.StringVar(&agent, "agent", "auto", "auto — каждый найденный в PATH агент (claude, codex), а если ни одного — generic; claude, codex — только он; generic (прежнее other) — ядро набора в .agents/skills и печать конфигурации MCP для любого другого агента")
		fs.StringVar(&name, "name", "", "имя MCP-сервера у агента; по умолчанию sdlc у claude и sdlc-<ключ проекта> у codex (--scope project у codex — sdlc)")
		fs.BoolVar(&dryRun, "dry-run", false, "показать, что будет сделано, ничего не меняя и не вызывая агентов")
		fs.BoolVar(&force, "force", false, "перезаписать и файлы набора, изменённые после установки")
		fs.BoolVar(&trust, "trust", false, "codex: добавить в ~/.codex/config.toml (или $CODEX_HOME) запись доверия [projects.\"<корень проекта>\"] trust_level = \"trusted\" — только для каталога запуска; без флага clab её только подсказывает")
		fs.Var(&markerFiles, "marker-file", "AGENTS.md или CLAUDE.md — писать блок привязки только в него; флаг можно повторять (по умолчанию — в каждый существующий, нет ни одного — создаётся AGENTS.md)")
		fs.BoolVar(&noMarker, "no-marker", false, "не писать блок привязки каталога к проекту в AGENTS.md / CLAUDE.md")
		fs.BoolVar(&plugin, "plugin", false, "только плагин Claude clab-plugin: вшит в clab, ставится в Claude в области user; без входа, проекта и сети, сочетается только с --dry-run")
	})
	if err != nil {
		return err
	}
	if plugin {
		if err := pluginOnly(fs); err != nil {
			return err
		}
		return cmdSetupPlugin(ctx, dryRun)
	}
	switch scope {
	case "", "local", "user", "project":
	default:
		return fmt.Errorf("--scope: local, user или project")
	}
	if agent, err = normalizeAgent(agent); err != nil {
		return err
	}
	if noMarker && len(markerFiles) > 0 {
		return fmt.Errorf("--no-marker и --marker-file вместе не задаются")
	}
	selected := selectAgents(agent)
	if scope == "project" && c.token != "" && hasAgent(selected, agentClaude) {
		return errProjectScopeToken
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	mcpURL, headerName, headerValue := c.mcpEndpoint(key)
	rep := &setupReport{
		Project: key, URL: mcpURL, DryRun: dryRun, Agents: []*agentReport{},
		Files: []fileAction{}, Marker: []markerAction{}, Docs: c.origin + "/docs?project=" + url.QueryEscape(key),
	}
	// Блок привязки — в каталог запуска, не в корень git (решение 0077,
	// п. 6). План — до любой записи: неверное имя файла и неверный --agent
	// ничего не меняют.
	if !noMarker {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if rep.Marker, err = planSetupMarker(wd, markerFiles, key, c.origin); err != nil {
			return err
		}
	}

	kit, serverVersion, err := c.fetchAgentKit(ctx)
	if err != nil {
		return err
	}
	rep.ServerVersion = serverVersion
	if kit != nil {
		for _, name := range kit.Skipped {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("привязка агента %q пропущена: этот clab (%s) такого агента не знает — обновите clab; остальные агенты настроены как обычно", name, version))
		}
	}
	if kit != nil && len(kit.Bindings) == 0 && len(kit.Skipped) == 0 && (hasAgent(selected, agentClaude) || hasAgent(selected, agentCodex)) {
		rep.Notes = append(rep.Notes, fmt.Sprintf("привязки агентов недоступны: сервер %s их не раздаёт (они приходят с выпуска с привязками; этот clab — %s). Набор Claude ставится из прежнего списка файлов; для Codex обновите инстанс.",
			dash(serverVersion), version))
	}

	// План всех агентов — до любой записи: ошибка пути или суммы не
	// оставляет половину набора.
	runs := make([]*agentRun, 0, len(selected))
	for _, sel := range selected {
		r := &agentRun{path: sel.path, rep: &agentReport{
			Agent: sel.name, Program: agentPrograms[sel.name], Found: sel.path != "",
			Scope: agentScope(sel.name, scope), Name: agentMCPName(sel.name, scope, name, key),
			Files: []fileAction{}, Start: agentStart(sel.name, key),
		}}
		if err := r.planKit(ctx, kit, serverVersion, force); err != nil {
			return err
		}
		runs = append(runs, r)
	}

	if !dryRun {
		for _, r := range runs {
			if r.plan.hasKit {
				if err := applyKit(r.plan.dir, r.plan.manifest, r.rep.Files, r.plan.next); err != nil {
					return err
				}
			}
		}
		if err := applySetupMarker(rep.Marker); err != nil {
			return err
		}
	}

	var errs []error
	for _, r := range runs {
		if err := r.connect(ctx, c, dryRun, mcpURL, headerName, headerValue); err != nil {
			errs = append(errs, err)
		}
	}

	var codex *agentRun
	for _, r := range runs {
		if r.rep.Agent == agentCodex {
			codex = r
		}
	}
	switch {
	case trust && codex == nil:
		rep.Warnings = append(rep.Warnings, "--trust относится к Codex, а среди агентов его нет: ничего не записано")
	case codex != nil && codex.failure == nil:
		root := codex.plan.dir
		if root == "" {
			root, _ = projectRoot(ctx)
		}
		tr, err := setupTrust(ctx, root, trust, dryRun)
		if err != nil {
			errs = append(errs, err)
		}
		rep.Trust = tr
	}

	fillModels(ctx, c, key, kit, runs, rep)
	for _, r := range runs {
		rep.Agents = append(rep.Agents, r.rep)
	}
	first := runs[0].rep
	rep.Agent, rep.Name, rep.Scope = first.Agent, first.Name, first.Scope
	if len(runs) > 1 {
		names := make([]string, 0, len(runs))
		for _, r := range runs {
			names = append(names, r.rep.Agent)
		}
		rep.Agent = strings.Join(names, ",")
	}
	rep.KitVersion, rep.KitDir, rep.Files = first.KitVersion, first.KitDir, first.Files
	rep.MCP, rep.MCPPrevious, rep.Config = first.MCP, first.MCPPrevious, first.Config
	codexOnlyClaudeMD(rep, runs)

	if err := emit(rep, func() error { printSetup(rep); return nil }); err != nil {
		return err
	}
	return errors.Join(errs...)
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
		if err := checkSum(f); err != nil {
			return nil, "", err
		}
	}
	if kit.Core != nil {
		for _, f := range kit.Core.Skills {
			if err := kitPathValid(f.Path); err != nil {
				return nil, "", err
			}
			if err := checkSum(f); err != nil {
				return nil, "", err
			}
		}
	}
	// Привязки терпимы к форме: непонятные считаются отсутствующими, как у
	// сервера прежней версии; а путь наружу или чужой каталог — отказ.
	if len(kit.RawBindings) > 0 {
		if json.Unmarshal(kit.RawBindings, &kit.Bindings) != nil {
			kit.Bindings = nil
		}
		kit.Levels = parseBindings(kit.RawBindings)
	}
	// Привязка агента, которого clab не знает, пропускается с предупреждением:
	// старый clab продолжает ставить Claude, когда сервер добавил нового агента.
	// Для известных (claude, codex, generic) проверка путей строгая, как в AC-8.
	known := kit.Bindings[:0]
	for _, b := range kit.Bindings {
		if !slices.Contains([]string{agentClaude, agentCodex, agentGeneric}, b.Name) {
			kit.Skipped = append(kit.Skipped, b.Name)
			continue
		}
		known = append(known, b)
	}
	kit.Bindings = known
	for _, b := range kit.Bindings {
		if b.Kit == nil {
			continue
		}
		if err := checkBindingKit(b); err != nil {
			return nil, "", err
		}
		for _, f := range b.Files {
			if err := checkSum(f); err != nil {
				return nil, "", err
			}
		}
	}
	return &kit, kit.Version, nil
}

func checkSum(f kitEntry) error {
	sum := sha256.Sum256([]byte(f.Content))
	if hex.EncodeToString(sum[:]) != f.SHA256 {
		return fmt.Errorf("набор агента: sha256 файла %s не сходится с содержимым — ничего не записано", f.Path)
	}
	return nil
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

// kitDir — куда ставится набор Claude: .claude/ корня проекта или каталог
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

// kitOwner описывает, чем набор владеет в каталоге: owns решает, чей путь
// из манифеста — наш и его можно удалить, если он ушёл из набора;
// keepForeign сохраняет в новом манифесте чужие записи (общий манифест
// Codex и generic); confine не пускает записи и удаления через ссылки
// наружу корня.
type kitOwner struct {
	owns        func(string) bool
	keepForeign bool
	confine     bool
}

// claudeOwner — набор Claude: пути skills/ и agents/ от .claude/.
var claudeOwner = kitOwner{owns: func(p string) bool { return kitPathValid(p) == nil }}

// planKit — набор Claude (пути от каталога .claude/).
func planKit(dir string, kit *agentKit, force bool) ([]fileAction, []byte, error) {
	return planKitFiles(dir, kitManifest, kit.Version, kit.Files, claudeOwner, force)
}

// planKitFiles сверяет набор с диском и манифестом прошлой установки. Файл,
// который на диске не совпадает ни с набором, ни с тем, что ставили в прошлый
// раз, — правка человека (или его собственный файл с тем же именем): без
// --force он не трогается. next — новый манифест; nil — он не изменился.
func planKitFiles(dir, manifest, kitVersion string, files []kitEntry, o kitOwner, force bool) ([]fileAction, []byte, error) {
	manifestPath := filepath.Join(dir, filepath.FromSlash(manifest))
	old := kitState{Files: map[string]string{}}
	if raw, err := os.ReadFile(manifestPath); err == nil {
		if err := json.Unmarshal(raw, &old); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		if old.Files == nil {
			old.Files = map[string]string{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	plan := []fileAction{}
	nm := kitState{Version: kitVersion, Files: map[string]string{}}
	inKit := map[string]bool{}
	for _, f := range files {
		if o.confine {
			if err := confined(dir, f.Path); err != nil {
				return nil, nil, err
			}
		}
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
	for p, sum := range old.Files {
		if inKit[p] {
			continue
		}
		if !o.owns(p) {
			if o.keepForeign {
				nm.Files[p] = sum
			}
			continue
		}
		gone = append(gone, p)
	}
	sort.Strings(gone)
	for _, p := range gone {
		if o.confine && confined(dir, p) != nil {
			// Запись манифеста ведёт через ссылку наружу: удалять по ней
			// ничего нельзя.
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
	if cur, err := os.ReadFile(manifestPath); err == nil && bytes.Equal(cur, raw) {
		raw = nil
	}
	return plan, raw, nil
}

func applyKit(dir, manifest string, plan []fileAction, next []byte) error {
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
		return writeAtomic(filepath.Join(dir, filepath.FromSlash(manifest)), next)
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
