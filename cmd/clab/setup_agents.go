package main

// Агенты clab setup и clab mcp install (CLAB-418, решение 0077, п. 8–9):
// выбор по PATH, версии, место набора по привязке и подключение MCP тем
// способом, который называет привязка. Одна функция подключения на агента:
// ею пользуются и setup, и mcp install.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Имена привязок, которые знает clab.
const (
	agentClaude  = "claude"
	agentCodex   = "codex"
	agentGeneric = "generic"
)

// agentPrograms — программа привязки в PATH; у generic её нет.
var agentPrograms = map[string]string{agentClaude: "claude", agentCodex: "codex"}

// codexTokenEnv — переменная, из которой Codex берёт токен MCP-сервера
// (поле mcp.bearer_token_env_var привязки codex): токена в файлах нет.
const codexTokenEnv = "SDLC_TOKEN"

// Общее ядро для агента без своего каталога (вопрос 1 аналитики CLAB-418):
// скилы в .agents/skills, манифест рядом. Файлов generic на сервере нет.
const (
	genericSkillsDir = ".agents/skills"
	genericManifest  = ".agents/clab-kit.json"
)

// Каталоги и манифесты, куда набор вправе писать: список живёт в clab, а не
// приходит с сервером — привязка с другим путём отвергается целиком.
var (
	allowedKitDirs   = []string{".claude/skills", ".claude/agents", ".agents/skills", ".codex/agents"}
	allowedManifests = []string{".claude/clab-kit.json", ".agents/clab-kit.json"}
)

// Минимальные версии агентов (решение 0077, «Проверено руками»): старее —
// предупреждение, установка идёт.
var minAgentVersions = map[string][3]int{agentClaude: {2, 1, 296}, agentCodex: {0, 167, 0}}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

type selectedAgent struct{ name, path string }

// normalizeAgent — значение --agent: auto, claude, codex, generic; прежнее
// other — псевдоним generic.
func normalizeAgent(a string) (string, error) {
	switch a {
	case "auto", agentClaude, agentCodex, agentGeneric:
		return a, nil
	case "other":
		return agentGeneric, nil
	}
	return "", fmt.Errorf("--agent: auto, claude, codex, generic (other — то же, что generic), а не %q", a)
}

func lookProgram(name string) string {
	p, err := exec.LookPath(agentPrograms[name])
	if err != nil {
		return ""
	}
	return p
}

// selectAgents — кого настраивать. auto: каждый найденный в PATH из claude и
// codex, а если ни одного — generic. Явный claude или codex остаётся в
// списке и без программы: набор ставится, MCP — нет.
func selectAgents(agent string) []selectedAgent {
	switch agent {
	case "auto":
		var out []selectedAgent
		for _, n := range []string{agentClaude, agentCodex} {
			if p := lookProgram(n); p != "" {
				out = append(out, selectedAgent{n, p})
			}
		}
		if len(out) == 0 {
			out = []selectedAgent{{agentGeneric, ""}}
		}
		return out
	case agentGeneric:
		return []selectedAgent{{agentGeneric, ""}}
	}
	return []selectedAgent{{agent, lookProgram(agent)}}
}

func hasAgent(list []selectedAgent, name string) bool {
	for _, a := range list {
		if a.name == name {
			return true
		}
	}
	return false
}

// agentScope — область агента. local — понятие Claude; Codex знает user
// (~/.codex/config.toml) и project (.codex/config.toml корня проекта).
func agentScope(agent, scope string) string {
	switch agent {
	case agentClaude:
		if scope == "" {
			return "local"
		}
		return scope
	case agentCodex:
		if scope == "project" {
			return "project"
		}
		return "user"
	}
	return ""
}

// agentMCPName — имя сервера. У Codex в пользовательском конфиге оно
// несёт ключ проекта: адрес проектный, а конфиг один на машину (0077 п. 9).
func agentMCPName(agent, scope, flagName, key string) string {
	switch {
	case flagName != "":
		return flagName
	case agent == agentCodex && agentScope(agent, scope) == "user":
		return "sdlc-" + strings.ToLower(key)
	}
	return "sdlc"
}

func agentStart(agent, key string) string {
	switch agent {
	case agentClaude:
		return "/clab-work " + key + "-<номер>"
	case agentCodex:
		return "$clab-work " + key + "-<номер>"
	}
	return "clab guide"
}

// rootPathValid — файл набора от корня проекта: чистый относительный путь
// внутри одного из каталогов dirs и не файл настроек агента.
func rootPathValid(p string, dirs []string) error {
	bad := fmt.Errorf("набор агента: недопустимый путь %q — ничего не записано", p)
	if p == "" || path.IsAbs(p) || strings.Contains(p, "\\") || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return bad
	}
	base := path.Base(p)
	if base == "config.toml" || (strings.HasPrefix(base, "settings") && strings.HasSuffix(base, ".json")) {
		return bad
	}
	for _, d := range dirs {
		if strings.HasPrefix(p, d+"/") {
			return nil
		}
	}
	return bad
}

// checkBindingKit проверяет привязку с набором: каталоги и манифест — из
// списка clab, каждый файл — внутри этих каталогов.
func checkBindingKit(b kitBinding) error {
	bad := func(p string) error {
		return fmt.Errorf("набор агента: недопустимый путь %q — ничего не записано", p)
	}
	if !slices.Contains(allowedManifests, b.Kit.Manifest) {
		return bad(b.Kit.Manifest)
	}
	for _, d := range []string{b.Kit.SkillsDir, b.Kit.AgentsDir} {
		if !slices.Contains(allowedKitDirs, d) {
			return bad(d)
		}
	}
	dirs := []string{b.Kit.SkillsDir, b.Kit.AgentsDir}
	for _, f := range b.Files {
		if err := rootPathValid(f.Path, dirs); err != nil {
			return err
		}
	}
	return nil
}

func ownedUnder(dirs ...string) func(string) bool {
	return func(p string) bool { return rootPathValid(p, dirs) == nil }
}

// confined — путь внутри корня и после раскрытия ссылок: запись по нему не
// выйдет наружу. Раскрывается ближайший существующий предок.
func confined(root, rel string) error {
	bad := fmt.Errorf("набор агента: недопустимый путь %q — ничего не записано", rel)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	cur := filepath.Join(root, filepath.FromSlash(rel))
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return bad
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
		return bad
	}
	return nil
}

// agentReport — один агент в итоге setup.
type agentReport struct {
	Agent       string               `json:"agent"`
	Program     string               `json:"program,omitempty"`
	Found       bool                 `json:"found"`
	Version     string               `json:"version,omitempty"`
	Scope       string               `json:"scope,omitempty"`
	Name        string               `json:"name"`
	KitVersion  *string              `json:"kit_version"`
	KitDir      string               `json:"kit_dir,omitempty"`
	KitManifest string               `json:"kit_manifest,omitempty"`
	Files       []fileAction         `json:"files"`
	MCP         string               `json:"mcp"`
	MCPPrevious string               `json:"mcp_previous_url,omitempty"`
	Config      map[string]any       `json:"config,omitempty"`
	Start       string               `json:"start"`
	Models      map[string]modelView `json:"models,omitempty"`
	// ModelsSource — откуда модели: agent_settings, binding или
	// agent_default (имена источников 0077, п. 7).
	ModelsSource string   `json:"models_source,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

// modelView — модель уровня агента в итоге; пустая модель — модель агента
// по умолчанию.
type modelView struct {
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
	Source string `json:"source"`
}

type kitPlan struct {
	hasKit   bool
	dir      string
	manifest string
	next     []byte
}

// agentRun — состояние одного агента между планом и подключением.
type agentRun struct {
	rep  *agentReport
	path string
	root string
	plan kitPlan
	// failure — набор для агента недоступен у этого сервера: код ненулевой,
	// агент не тронут.
	failure error
}

// planKit строит план набора агента — без записи.
func (r *agentRun) planKit(ctx context.Context, kit *agentKit, serverVersion string, force bool) error {
	rep := r.rep
	switch rep.Agent {
	case agentClaude:
		if kit != nil {
			root, dir, err := kitDir(ctx, rep.Scope)
			if err != nil {
				return err
			}
			plan, next, err := planKit(dir, kit, force)
			if err != nil {
				return err
			}
			r.root = root
			rep.KitVersion, rep.KitDir, rep.KitManifest, rep.Files = &kit.Version, dir, filepath.Join(dir, kitManifest), plan
			r.plan = kitPlan{true, dir, kitManifest, next}
		} else if rep.Scope != "user" {
			r.root, _ = projectRoot(ctx)
		}
	case agentCodex:
		b := kit.binding(agentCodex)
		switch {
		case kit == nil:
			r.failure = fmt.Errorf("codex: сервер %s не раздаёт набор агента — он приходит с сервера начиная с выпуска с clab setup (этот clab — %s). Обновите инстанс и запустите clab setup ещё раз", dash(serverVersion), version)
		case b == nil || b.Kit == nil:
			r.failure = fmt.Errorf("codex: сервер %s не раздаёт привязку codex — она приходит с выпуска с привязками агентов (этот clab — %s). Обновите инстанс и запустите clab setup ещё раз", dash(serverVersion), version)
		}
		if r.failure != nil {
			rep.Notes = append(rep.Notes, "набор Codex не поставлен: "+r.failure.Error())
			return nil
		}
		root, err := projectRoot(ctx)
		if err != nil {
			return err
		}
		o := kitOwner{owns: ownedUnder(b.Kit.SkillsDir, b.Kit.AgentsDir), keepForeign: true, confine: true}
		plan, next, err := planKitFiles(root, b.Kit.Manifest, kit.Version, b.Files, o, force)
		if err != nil {
			return err
		}
		r.root = root
		rep.KitVersion, rep.KitDir, rep.KitManifest, rep.Files = &kit.Version, root, filepath.Join(root, filepath.FromSlash(b.Kit.Manifest)), plan
		r.plan = kitPlan{true, root, b.Kit.Manifest, next}
	case agentGeneric:
		if kit == nil || kit.Core == nil || len(kit.Core.Skills) == 0 {
			rep.Notes = append(rep.Notes, fmt.Sprintf("ядро набора недоступно: сервер %s не раздаёт его (этот clab — %s); обновите инстанс. Конфигурация MCP напечатана.", dash(serverVersion), version))
			return nil
		}
		root, err := projectRoot(ctx)
		if err != nil {
			return err
		}
		files := make([]kitEntry, 0, len(kit.Core.Skills))
		for _, s := range kit.Core.Skills {
			files = append(files, kitEntry{Path: genericSkillsDir + "/" + strings.TrimPrefix(s.Path, "skills/"), Content: s.Content, SHA256: s.SHA256})
		}
		o := kitOwner{owns: ownedUnder(genericSkillsDir), keepForeign: true, confine: true}
		plan, next, err := planKitFiles(root, genericManifest, kit.Version, files, o, force)
		if err != nil {
			return err
		}
		r.root = root
		rep.KitVersion, rep.KitDir, rep.KitManifest, rep.Files = &kit.Version, root, filepath.Join(root, filepath.FromSlash(genericManifest)), plan
		r.plan = kitPlan{true, root, genericManifest, next}
	}
	return nil
}

// checkVersion спрашивает у программы версию и предупреждает, если она
// старее проверенной. Не отказывает: версия — совет, а не условие.
func (r *agentRun) checkVersion(ctx context.Context) {
	rep := r.rep
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, r.path, "--version").Output()
	m := versionRe.FindStringSubmatch(string(out))
	if m == nil {
		rep.Warnings = append(rep.Warnings, "версия "+rep.Program+" не определена (вывод --version не разобран); минимальная проверенная — "+minVersionText(rep.Agent))
		return
	}
	rep.Version = m[0]
	have := [3]int{}
	for i := range have {
		have[i], _ = strconv.Atoi(m[i+1])
	}
	if want := minAgentVersions[rep.Agent]; slices.Compare(have[:], want[:]) < 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s %s старше минимальной проверенной %s: набор ставится, но подключение может не заработать — обновите %s",
			rep.Program, m[0], minVersionText(rep.Agent), rep.Program))
	}
}

func minVersionText(agent string) string {
	v := minAgentVersions[agent]
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// genericConfig — конфигурация MCP для печати (агент без привязки, claude или
// codex не найден в PATH): вместо токена — ${SDLC_TOKEN}, ни в вывод, ни в
// файл он не попадает. Единственное место, где токен идёт вне выбранного
// агента, — аргумент --header у claude mcp add (рекомендация 2).
func genericConfig(c *Client, name, mcpURL, headerName, headerValue string) map[string]any {
	if c.token != "" {
		headerValue = "Bearer ${" + codexTokenEnv + "}"
	}
	return mcpConfig(name, mcpURL, headerName, headerValue)
}

// connect — версия агента и подключение MCP его способом. Ошибка не
// останавливает остальных агентов: setup печатает итог и возвращает её.
func (r *agentRun) connect(ctx context.Context, c *Client, dryRun bool, mcpURL, headerName, headerValue string) error {
	rep := r.rep
	if r.failure != nil {
		rep.MCP = mcpSkipped
		return r.failure
	}
	if rep.Agent == agentGeneric {
		rep.MCP = mcpSkipped
		rep.Config = genericConfig(c, rep.Name, mcpURL, headerName, headerValue)
		return nil
	}
	if r.path != "" && !dryRun {
		r.checkVersion(ctx)
	}
	var err error
	switch rep.Agent {
	case agentClaude:
		switch {
		case r.path == "":
			rep.MCP = mcpSkipped
			if !dryRun {
				raw, _ := json.MarshalIndent(genericConfig(c, rep.Name, mcpURL, headerName, headerValue), "", "  ")
				return fmt.Errorf("claude не найден в PATH: MCP не подключён. Конфигурация для агента:\n%s", raw)
			}
		case dryRun:
			rep.MCP = mcpCheck
		default:
			header := headerName + ": " + headerValue
			rep.MCP, rep.MCPPrevious, err = connectMCP(ctx, r.path, r.root, rep.Scope, rep.Name, mcpURL, header, headerValue)
		}
	case agentCodex:
		switch {
		case rep.Scope == "project":
			rep.MCP, rep.MCPPrevious, err = codexProjectMCP(r.root, rep.Name, mcpURL, dryRun)
		case r.path == "":
			rep.MCP = mcpSkipped
			if !dryRun {
				return fmt.Errorf("codex не найден в PATH: MCP не подключён. Подключить вручную:\n  %s", codexAddCommand(rep.Name, mcpURL))
			}
		case dryRun:
			rep.MCP = mcpCheck
		default:
			rep.MCP, rep.MCPPrevious, err = connectCodexMCP(ctx, r.path, rep.Name, mcpURL, c.token)
		}
	}
	if err != nil {
		rep.MCP = mcpFailed
	}
	return err
}

// codexAddCommand — команда ручного подключения; токена в ней нет.
func codexAddCommand(name, mcpURL string) string {
	return "codex mcp add " + name + " --url " + mcpURL + " --bearer-token-env-var " + codexTokenEnv
}

// codexOnlyClaudeMD: Codex читает AGENTS.md, а блок привязки (и инструкции
// проекта) могут лежать только в CLAUDE.md.
func codexOnlyClaudeMD(rep *setupReport, runs []*agentRun) {
	var codex *agentRun
	for _, r := range runs {
		if r.rep.Agent == agentCodex {
			codex = r
		}
	}
	wd, err := os.Getwd()
	if codex == nil || err != nil {
		return
	}
	if _, err := os.Lstat(filepath.Join(wd, "AGENTS.md")); err == nil {
		return
	}
	for _, m := range rep.Marker {
		if filepath.Base(m.Path) == "AGENTS.md" {
			return
		}
	}
	if _, err := os.Lstat(filepath.Join(wd, "CLAUDE.md")); err == nil {
		codex.rep.Notes = append(codex.rep.Notes, "в каталоге есть CLAUDE.md, а AGENTS.md нет: Codex читает только AGENTS.md — блок привязки туда не попал; clab setup --marker-file AGENTS.md добавит его")
	}
}
