package main

// Модель уровня для своего агента (решение 0077 пп. 7–8, CLAB-427): под
// строкой «Рекомендованный уровень» clab claim и clab show печатают, какая
// модель стоит на этом уровне у агента, в котором запущен clab. Уровень по-
// прежнему рекомендация (0060): CLI модель не проверяет и не меняет.
//
// Выбор (pickModel) отделён от печати: clab exec и раннер (CLAB-419, 420)
// берут ту же функцию и добавляют свои источники — explicit и model_hint.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Источники модели уровня; имена печатаются как есть (0077 п. 7).
const (
	sourceAgentSettings = "agent_settings"
	sourceBinding       = "binding"
	sourceAgentDefault  = "agent_default"
)

// modelChoice — модель и усилие уровня: запись execution.models.<агент>.
type modelChoice struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// modelPick — результат выбора. Model пуста при источнике agent_default:
// флаг модели агенту не передаётся.
type modelPick struct {
	Agent  string
	Model  string
	Effort string
	Source string
}

// pickModel — шаги 2, 3 и 5 порядка 0077 п. 7: запись версии-снимка прогона
// для своего агента, затем таблица уровней привязки (nil — привязки или
// таблицы нет), иначе модель агента по умолчанию.
func pickModel(level, agent string, snapshot map[string]modelChoice, binding *agentBinding) modelPick {
	if m, ok := snapshot[agent]; ok && m.Model != "" {
		return modelPick{Agent: agent, Model: m.Model, Effort: m.Effort, Source: sourceAgentSettings}
	}
	if binding != nil {
		if model := binding.Levels[level]; model != "" {
			return modelPick{Agent: agent, Model: model, Source: sourceBinding}
		}
	}
	return modelPick{Agent: agent, Source: sourceAgentDefault}
}

// agentEnv — окружение, по которому узнаётся агент; в тестах подменяется.
var agentEnv = os.Getenv

// agentKitTimeout — предел запроса набора: захват ждать его не должен.
var agentKitTimeout = 3 * time.Second

// agentCondition — одно условие признака агента из поля detect привязки.
type agentCondition struct {
	env      string
	prefix   *string
	equals   *string
	nonEmpty bool
}

func (c agentCondition) holds(getenv func(string) string) bool {
	v := getenv(c.env)
	switch {
	case c.prefix != nil:
		return strings.HasPrefix(v, *c.prefix)
	case c.equals != nil:
		return v == *c.equals
	default:
		return c.nonEmpty && v != ""
	}
}

// agentBinding — то, что clab берёт из привязки набора агента: признак,
// таблицу уровней по умолчанию и, для clab exec (CLAB-419), шаблон запуска,
// способ исполнителя, хук и перевод tool_policy. Поле непонятной формы —
// как пустое: разбор терпимый, как у всего набора.
type agentBinding struct {
	Name   string
	Levels map[string]string
	// Run — шаблон команды запуска; nil — у привязки его нет (generic).
	Run []string
	// Executor — subagent или process (0077 п. 3).
	Executor string
	// Hook — хук как записан в наборе; у placement launch сервер кладёт
	// сценарий в command.
	Hook *bindingHook
	// ToolPolicy — привязка переводит tool_policy роли во флаги агента.
	ToolPolicy bool
	// detect пуст, если признака нет или его форма непонятна: такая
	// привязка не узнаёт агента, как привязка без признака.
	detect []agentCondition
}

func (b agentBinding) matches(getenv func(string) string) bool {
	for _, cond := range b.detect {
		if cond.holds(getenv) {
			return true
		}
	}
	return false
}

// builtinBindings — признаки, которые clab знает сам, пока набор с
// привязками недоступен (сервер прежней версии или привязок ещё нет):
// 0077 п. 3, 8. Codex, запущенный из оболочки Claude Code, наследует
// AI_AGENT, поэтому оба признака сразу — неоднозначность, а не claude.
func builtinBindings() []agentBinding {
	claudePrefix, one := "claude-code_", "1"
	return []agentBinding{
		{Name: "claude", detect: []agentCondition{
			{env: "AI_AGENT", prefix: &claudePrefix},
			{env: "CLAUDECODE", equals: &one},
		}},
		{Name: "codex", detect: []agentCondition{{env: "CODEX_THREAD_ID", nonEmpty: true}}},
	}
}

// detectAgent — имена привязок, чей признак совпал с окружением. Агента
// узнали, если имя ровно одно; ноль или несколько — «агент не узнан».
// Признаки берутся у привязок набора, если хоть у одной они понятны, иначе
// встроенные.
func detectAgent(getenv func(string) string, bindings []agentBinding) []string {
	use := bindings
	known := false
	for _, b := range bindings {
		if len(b.detect) > 0 {
			known = true
		}
	}
	if !known {
		use = builtinBindings()
	}
	var names []string
	for _, b := range use {
		if b.matches(getenv) {
			names = append(names, b.Name)
		}
	}
	sort.Strings(names)
	return names
}

// parseBindings читает bindings из ответа GET /v1/agent-kit терпимо:
// массив записей с name или объект по имени; любая непонятная форма — как
// без привязок (форма — CLAB-417, AC-12 его аналитики).
func parseBindings(raw json.RawMessage) []agentBinding {
	type entry struct {
		Name       string            `json:"name"`
		Detect     json.RawMessage   `json:"detect"`
		Levels     map[string]string `json:"levels"`
		Run        json.RawMessage   `json:"run"`
		Executor   json.RawMessage   `json:"executor"`
		Hook       json.RawMessage   `json:"hook"`
		ToolPolicy json.RawMessage   `json:"tool_policy"`
	}
	var entries []entry
	if json.Unmarshal(raw, &entries) != nil {
		var byName map[string]entry
		if json.Unmarshal(raw, &byName) != nil {
			return nil
		}
		for name, e := range byName {
			e.Name = name
			entries = append(entries, e)
		}
	}
	var out []agentBinding
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		b := agentBinding{Name: e.Name, Levels: e.Levels, detect: parseDetect(e.Detect)}
		var run []string
		if json.Unmarshal(e.Run, &run) == nil && len(run) > 0 {
			b.Run = run
		}
		_ = json.Unmarshal(e.Executor, &b.Executor)
		var hook bindingHook
		if json.Unmarshal(e.Hook, &hook) == nil && hook.Placement != "" {
			b.Hook = &hook
		}
		var policy map[string]any
		b.ToolPolicy = json.Unmarshal(e.ToolPolicy, &policy) == nil && len(policy) > 0
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// bindingHook — поле hook привязки: где крепится хук, к какому событию и
// тулу, и сценарий (command) у хука запуска.
type bindingHook struct {
	Placement string `json:"placement"`
	Agent     string `json:"agent"`
	Event     string `json:"event"`
	Matcher   string `json:"matcher"`
	Command   string `json:"command"`
}

// parseDetect — массив условий «или»; хоть одно непонятное — признака нет.
func parseDetect(raw json.RawMessage) []agentCondition {
	var conds []struct {
		Env      string  `json:"env"`
		Prefix   *string `json:"prefix"`
		Equals   *string `json:"equals"`
		NonEmpty bool    `json:"non_empty"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &conds) != nil {
		return nil
	}
	var out []agentCondition
	for _, c := range conds {
		if c.Env == "" || (c.Prefix == nil && c.Equals == nil && !c.NonEmpty) {
			return nil
		}
		out = append(out, agentCondition{env: c.Env, prefix: c.Prefix, equals: c.Equals, nonEmpty: c.NonEmpty})
	}
	return out
}

// fetchBindings — привязки набора инстанса. Любой сбой (ошибка, таймаут,
// страница вместо JSON, ответ без привязок) — nil: захват от набора не
// зависит.
func fetchBindings(ctx context.Context, c *Client) []agentBinding {
	ctx, cancel := context.WithTimeout(ctx, agentKitTimeout)
	defer cancel()
	raw, _, err := c.getContent(ctx, "/agent-kit")
	if err != nil {
		return nil
	}
	var kit struct {
		Bindings json.RawMessage `json:"bindings"`
	}
	if json.Unmarshal(raw, &kit) != nil || len(kit.Bindings) == 0 {
		return nil
	}
	return parseBindings(kit.Bindings)
}

func findBinding(bindings []agentBinding, name string) *agentBinding {
	for i := range bindings {
		if bindings[i].Name == name {
			return &bindings[i]
		}
	}
	return nil
}

// modelLines — строки «Модель уровня …» для сводки claim и show; nil — их
// нет и вывод такой же, как до CLAB-427: нет уровня, сервер прежней версии
// (в ответе нет agent_settings_version), версии нет и взять модель негде.
// Набор агента запрашивается, только когда без него не обойтись.
func modelLines(ctx context.Context, c *Client, e *executionView) []string {
	if e == nil {
		return nil
	}
	level := ptr(e.ModelLevel)
	if level == "" || e.ModelLevelSource == "none" || len(e.AgentSettingsVersion) == 0 {
		return nil
	}
	var version *int
	var n int
	// null разбирается без ошибки и оставляет ноль: проверять его отдельно.
	if string(e.AgentSettingsVersion) != "null" && json.Unmarshal(e.AgentSettingsVersion, &n) == nil {
		version = &n
	}

	var bindings []agentBinding
	names := detectAgent(agentEnv, nil)
	// Запись для узнанного агента уже в версии: набор не нужен.
	if !(len(names) == 1 && version != nil && e.Models[names[0]].Model != "") {
		bindings = fetchBindings(ctx, c)
		names = detectAgent(agentEnv, bindings)
	}

	if len(names) != 1 {
		if version == nil {
			return nil
		}
		return unrecognizedLines(level, *version, e.Models)
	}
	agent := names[0]
	pick := pickModel(level, agent, e.Models, findBinding(bindings, agent))
	switch pick.Source {
	case sourceAgentSettings:
		if version == nil { // записи без версии сервер не отдаёт; на всякий случай
			return nil
		}
		return []string{fmt.Sprintf("Модель уровня: %s%s (%s: агент %s, настройка агентов v%d)",
			pick.Model, effortSuffix(pick.Effort), sourceAgentSettings, agent, *version)}
	case sourceBinding:
		return []string{fmt.Sprintf("Модель уровня: %s (%s: агент %s, таблица привязки по умолчанию)",
			pick.Model, sourceBinding, agent)}
	}
	if version == nil {
		return nil
	}
	return []string{fmt.Sprintf("Модель уровня: не задана, модель агента по умолчанию (%s: агент %s, в настройке агентов v%d нет записи для уровня %s)",
		sourceAgentDefault, agent, *version, level)}
}

func effortSuffix(effort string) string {
	if effort == "" {
		return ""
	}
	return ", усилие " + effort
}

// unrecognizedLines — агент не узнан: модели уровня всех агентов версии, и
// ни одна не названа «своей».
func unrecognizedLines(level string, version int, models map[string]modelChoice) []string {
	var agents []string
	for name, m := range models {
		if m.Model != "" {
			agents = append(agents, name)
		}
	}
	sort.Strings(agents)
	if len(agents) == 0 {
		return []string{fmt.Sprintf("Модель уровня: агент не узнан; в настройке агентов v%d для уровня %s записей нет", version, level)}
	}
	width := 0
	for _, a := range agents {
		width = max(width, len([]rune(a)))
	}
	modelWidth := 0
	for _, a := range agents {
		modelWidth = max(modelWidth, len([]rune(models[a].Model)))
	}
	lines := []string{"Модель уровня: агент не узнан"}
	for _, a := range agents {
		m := models[a]
		line := fmt.Sprintf("  %-*s  %-*s", width, a, modelWidth, m.Model)
		if m.Effort != "" {
			line += "  усилие " + m.Effort
		}
		lines = append(lines, strings.TrimRight(line, " "))
	}
	return append(lines, "  источник: "+sourceAgentSettings+", настройка агентов v"+strconv.Itoa(version))
}

// stateExecution достаёт execution из ответа состояния или захвата; ответ,
// который не разбирается, строк модели не даёт — ошибку покажет рендер.
func stateExecution(raw json.RawMessage) *executionView {
	var s struct {
		Execution *executionView `json:"execution"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return s.Execution
}
