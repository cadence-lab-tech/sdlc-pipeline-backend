package main

// Итог clab setup (CLAB-418, AC-11): по блоку на агента, таблица «агент →
// набор → MCP → как начать», модели уровней найденных агентов, подсказки.
// Оплату («подписка или ключ») clab не угадывает и никуда не пишет (0062):
// только подсказка, как это посмотреть и как назвать при отчёте.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// builtinLevels — таблица уровней, когда привязки с таблицей нет (сервер
// без привязок): алиасы Claude Code из привязки claude.
var builtinLevels = map[string]map[string]string{
	agentClaude: {"low": "haiku", "medium": "sonnet", "high": "opus"},
}

// fillModels заполняет модели уровней агентов: версия настройки агентов
// проекта, иначе таблица привязки по умолчанию, иначе модель агента.
// Любой сбой запроса — тихий откат на таблицу: setup от настройки не
// зависит.
func fillModels(ctx context.Context, c *Client, key string, kit *agentKit, runs []*agentRun, rep *setupReport) {
	want := false
	for _, r := range runs {
		if r.rep.Agent != agentGeneric {
			want = true
		}
	}
	if !want {
		return
	}
	var doc agentDocument
	ctx, cancel := context.WithTimeout(ctx, agentKitTimeout)
	defer cancel()
	var raw json.RawMessage
	if c.get(ctx, "/projects/"+url.PathEscape(key)+"/agent-settings", nil, &raw) == nil {
		var v agentVersion
		if json.Unmarshal(raw, &v) == nil && v.Version > 0 && (len(v.Document) == 0 || json.Unmarshal(v.Document, &doc) == nil) {
			n := v.Version
			rep.AgentSettingsVersion = &n
		}
	}
	var bindings []agentBinding
	if kit != nil {
		bindings = kit.Levels
	}
	for _, r := range runs {
		agent := r.rep.Agent
		if agent == agentGeneric {
			continue
		}
		b := findBinding(bindings, agent)
		if (b == nil || b.Levels == nil) && builtinLevels[agent] != nil {
			b = &agentBinding{Name: agent, Levels: builtinLevels[agent]}
		}
		r.rep.Models = map[string]modelView{}
		for _, level := range []string{"low", "medium", "high"} {
			snapshot := map[string]modelChoice{}
			if rep.AgentSettingsVersion != nil {
				if l, ok := doc.Agents[agent].Levels[level]; ok {
					snapshot[agent] = modelChoice{Model: l.Model, Effort: l.Effort}
				}
			}
			p := pickModel(level, agent, snapshot, b)
			r.rep.Models[level] = modelView{Model: p.Model, Effort: p.Effort, Source: p.Source}
		}
		r.rep.ModelsSource = r.rep.Models["medium"].Source
	}
}

var mcpLabels = map[bool]map[string]string{
	false: {
		mcpAdded: "подключён", mcpUnchanged: "уже подключён", mcpSkipped: "не подключён",
		mcpManual: "не изменён: запись с другим адресом", mcpFailed: "не подключён: ошибка",
	},
	true: {
		mcpAdded: "будет подключён", mcpUnchanged: "уже подключён", mcpSkipped: "не будет подключён",
		mcpCheck:  "будет проверен: добавлен, если не подключён, или заменён, если адрес другой",
		mcpManual: "не будет изменён: запись с другим адресом", mcpFailed: "не подключён: ошибка",
	},
}

func mcpText(a *agentReport, dryRun bool) string {
	if a.MCP == mcpReplaced {
		if dryRun {
			return "будет заменён (был " + a.MCPPrevious + ")"
		}
		return "заменён (был " + a.MCPPrevious + ")"
	}
	if a.MCP == mcpSkipped {
		switch a.Agent {
		case agentGeneric:
			if dryRun {
				return "не будет подключён: ни одного известного агента в PATH или выбран generic — конфигурация MCP напечатана ниже"
			}
			return "не подключён: агент без привязки — конфигурация MCP напечатана ниже"
		default:
			if dryRun {
				return "не будет подключён: " + a.Program + " не найден в PATH"
			}
			return "не подключён: " + a.Program + " не найден в PATH"
		}
	}
	return mcpLabels[dryRun][a.MCP]
}

var fileLabels = map[bool]map[string]string{
	false: {fileCreate: "создан", fileUpdate: "обновлён", fileDelete: "удалён", fileSkipModified: "пропущен"},
	true:  {fileCreate: "создать", fileUpdate: "обновить", fileDelete: "удалить", fileSkipModified: "пропустить"},
}

// kitChanged — были ли у набора изменения.
func kitChanged(a *agentReport) bool {
	for _, f := range a.Files {
		if f.Action != fileUnchanged {
			return true
		}
	}
	return false
}

// kitPlace — где агент ищет набор: коротко, для таблицы.
func kitPlace(a *agentReport) string {
	switch a.Agent {
	case agentClaude:
		if a.Scope == "user" {
			return "~/.claude"
		}
		return ".claude"
	case agentCodex:
		return ".agents/skills, .codex/agents"
	}
	return genericSkillsDir
}

func kitCell(a *agentReport, dryRun bool) string {
	if a.KitVersion == nil {
		return "нет: сервер не раздаёт"
	}
	counts := map[string]int{}
	for _, f := range a.Files {
		counts[f.Action]++
	}
	var parts []string
	for _, act := range []string{fileCreate, fileUpdate, fileDelete, fileSkipModified} {
		if n := counts[act]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", fileLabels[dryRun][act], n))
		}
	}
	if len(parts) == 0 {
		parts = []string{"актуален"}
	}
	return kitPlace(a) + ": " + strings.Join(parts, ", ")
}

func mcpCell(a *agentReport, dryRun bool) string {
	if a.Agent == agentGeneric {
		return a.Name + ": конфигурация напечатана"
	}
	text := mcpText(a, dryRun)
	if i := strings.Index(text, ":"); i > 0 {
		text = text[:i]
	}
	if i := strings.Index(text, " ("); i > 0 && a.MCP != mcpReplaced {
		text = text[:i]
	}
	return a.Name + ": " + text
}

func printAgentBlock(rep *setupReport, a *agentReport, multi bool) {
	if multi {
		fmt.Printf("Агент:   %s\n", a.Agent)
	}
	scope := ""
	if a.Scope != "" {
		scope = " (" + a.Scope + ")"
	}
	fmt.Printf("MCP:     %s → %s%s: %s\n", a.Name, rep.URL, scope, mcpText(a, rep.DryRun))

	if a.KitVersion == nil {
		if a.Agent == agentClaude {
			fmt.Printf("Набор:   сервер %s не раздаёт набор агента — он приходит с сервера начиная с выпуска с clab setup (этот clab — %s). Обновите инстанс и запустите clab setup ещё раз.\n",
				dash(rep.ServerVersion), version)
		} else {
			fmt.Println("Набор:   не поставлен")
		}
	} else {
		fmt.Printf("Набор:   %s → %s\n", *a.KitVersion, a.KitDir)
		labels := fileLabels[rep.DryRun]
		for _, f := range a.Files {
			if f.Action == fileUnchanged {
				continue
			}
			if f.Action == fileSkipModified {
				fmt.Printf("  %-10s %s — изменён после установки; --force перезапишет\n", labels[f.Action], filepath.Join(a.KitDir, filepath.FromSlash(f.Path)))
				continue
			}
			fmt.Printf("  %-10s %s\n", labels[f.Action], f.Path)
		}
		if !kitChanged(a) {
			fmt.Println("  файлы набора актуальны")
		}
	}
	for _, n := range a.Notes {
		fmt.Println("! " + n)
	}
	for _, w := range a.Warnings {
		fmt.Println("! " + w)
	}
	if a.Config != nil {
		raw, _ := json.MarshalIndent(a.Config, "", "  ")
		fmt.Printf("Конфигурация MCP проекта %s для вашего агента (токен — в переменной окружения %s, в конфигурации его нет):\n%s\n", rep.Project, codexTokenEnv, raw)
		fmt.Println("инструкции: clab guide")
	}
}

func printSetup(rep *setupReport) {
	if rep.DryRun {
		fmt.Println("--dry-run: ничего не записано, агенты не вызывались.")
	}
	fmt.Printf("Проект:  %s\n", rep.Project)
	multi := len(rep.Agents) > 1
	for _, a := range rep.Agents {
		printAgentBlock(rep, a, multi)
	}
	for _, n := range rep.Notes {
		fmt.Println("! " + n)
	}
	for _, w := range rep.Warnings {
		fmt.Println("! " + w)
	}
	printSetupMarker(rep.Marker, rep.DryRun)

	changed := false
	allMCPSame, allKit := true, true
	for _, a := range rep.Agents {
		changed = changed || kitChanged(a)
		allMCPSame = allMCPSame && a.MCP == mcpUnchanged
		allKit = allKit && a.KitVersion != nil
	}
	for _, m := range rep.Marker {
		if m.Action == markerCreate || m.Action == markerUpdate {
			changed = true
		}
	}
	if !rep.DryRun && allMCPSame && allKit && !changed {
		fmt.Println("Уже актуально.")
	}

	fmt.Println()
	t := newTable("АГЕНТ", "НАБОР", "MCP", "КАК НАЧАТЬ")
	for _, a := range rep.Agents {
		t.add(a.Agent, kitCell(a, rep.DryRun), mcpCell(a, rep.DryRun), a.Start)
	}
	t.print()

	printModels(rep)
	printTrust(rep)
	printHints(rep)
	fmt.Printf("Инструкция: %s\n", rep.Docs)
}

func printModels(rep *setupReport) {
	var lines []string
	for _, a := range rep.Agents {
		if a.Models == nil {
			continue
		}
		label, models := modelsLabel(rep, a), modelsText(a)
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", a.Agent, label, models))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Println("Модели уровней:")
	for _, l := range lines {
		fmt.Println(l)
	}
}

func modelsLabel(rep *setupReport, a *agentReport) string {
	switch a.ModelsSource {
	case sourceAgentSettings:
		if rep.AgentSettingsVersion != nil {
			return fmt.Sprintf("настройка агентов v%d", *rep.AgentSettingsVersion)
		}
	case sourceAgentDefault:
		if rep.AgentSettingsVersion != nil {
			return fmt.Sprintf("в настройке агентов v%d нет записи для агента", *rep.AgentSettingsVersion)
		}
	}
	return "таблица привязки по умолчанию"
}

func modelsText(a *agentReport) string {
	var parts []string
	for _, level := range []string{"low", "medium", "high"} {
		m := a.Models[level]
		if m.Model == "" {
			if a.Agent == agentCodex {
				return "модель Codex по умолчанию"
			}
			return "модель агента по умолчанию"
		}
		s := level + " " + m.Model
		if m.Effort != "" {
			s += " (усилие " + m.Effort + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func printTrust(rep *setupReport) {
	tr := rep.Trust
	if tr == nil {
		return
	}
	switch {
	case tr.Action == trustUnchanged:
		fmt.Printf("Доверие Codex: %s уже доверен (%s).\n", tr.Path, tr.File)
	case tr.Action == trustConflict:
		fmt.Printf("Доверие Codex: для %s в %s уже есть запись с trust_level = %q — clab её не правит; поменяйте вручную.\n", tr.Path, tr.File, tr.Level)
	case tr.Action == trustAdded && rep.DryRun:
		fmt.Printf("Доверие Codex: --trust добавит в %s (остальное в файле не меняется):\n", tr.File)
		printIndented(tr.Lines)
	case tr.Action == trustAdded:
		fmt.Printf("Доверие Codex: добавлено в %s (остальное в файле не менялось):\n", tr.File)
		printIndented(tr.Lines)
	default:
		fmt.Printf("Доверие Codex: проектный .codex/ (config.toml, агенты) Codex читает только у доверенного проекта; clab setup --trust добавит запись в %s, либо добавьте её сами:\n", tr.File)
		printIndented(tr.Lines)
	}
}

func printIndented(lines []string) {
	for _, l := range lines {
		fmt.Println("    " + l)
	}
}

func printHints(rep *setupReport) {
	var billing bool
	for _, a := range rep.Agents {
		switch a.Agent {
		case agentCodex:
			billing = true
			fmt.Printf("Токен Codex: сервер %s читает его из переменной окружения %s, в файлах и аргументах токена нет. Выставьте её перед запуском Codex: export %s=\"$(clab auth token)\" (в профиле оболочки токен окажется файлом на диске — решайте сами).\n",
				a.Name, codexTokenEnv, codexTokenEnv)
			fmt.Println("Сеть Codex: команды в песочнице ходят в сеть, только если в config.toml включено sandbox_workspace_write.network_access = true (read-only сеть закрывает всегда); clab setup этой настройки не пишет.")
		case agentClaude:
			billing = true
		}
	}
	if billing {
		fmt.Printf("Оплата: подписка у вас или ключ, clab не определяет и нигде не записывает. Посмотреть можно командой claude auth status или codex login status; в отчёте о стоимости назовите её: clab usage <ключ задачи> --billing subscription|api.\n")
	}
}
