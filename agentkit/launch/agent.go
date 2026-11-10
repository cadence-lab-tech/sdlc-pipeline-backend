package launch

// Общее у clab exec (CLAB-419) и раннера (CLAB-420) сверх подстановок:
// имена привязок, программа команды, выбор модели уровня (решение 0077
// п. 7) и перевод tool_policy роли. Шаги, которые есть только у одного из
// них (--model clab exec, SDLC_RUNNER_MODEL_LEVELS и model_hint раннера),
// каждый добавляет сам вокруг PickModel.

import (
	"path/filepath"
	"strings"
)

// Имена привязок, которые clab и раннер знают по имени программы.
const (
	AgentClaude  = "claude"
	AgentCodex   = "codex"
	AgentGeneric = "generic"
)

// Program — имя программы первого слова команды: без пути, без .exe, в
// нижнем регистре. «/opt/bin/codex» → codex, «Codex.exe» → codex, «env» →
// env (обёртка над агентом — своя программа, а не агент).
func Program(word string) string {
	word = strings.ReplaceAll(word, `\`, "/")
	return strings.TrimSuffix(strings.ToLower(filepath.Base(word)), ".exe")
}

// Источники модели уровня; имена печатаются как есть (0077 п. 7).
const (
	SourceExplicit      = "explicit"
	SourceAgentSettings = "agent_settings"
	SourceBinding       = "binding"
	SourceAgentDefault  = "agent_default"
)

// ModelChoice — модель и усилие уровня: запись execution.models.<агент>.
type ModelChoice struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// Pick — результат выбора. Model пуста при источнике agent_default: флаг
// модели агенту не передаётся.
type Pick struct {
	Agent  string
	Model  string
	Effort string
	Source string
}

// PickModel — шаги 2, 3 и 5 порядка 0077 п. 7: запись версии-снимка прогона
// для своего агента, затем таблица уровней привязки (nil — привязки или
// таблицы нет), иначе модель агента по умолчанию.
func PickModel(level, agent string, snapshot map[string]ModelChoice, levels map[string]string) Pick {
	if m, ok := snapshot[agent]; ok && m.Model != "" {
		return Pick{Agent: agent, Model: m.Model, Effort: m.Effort, Source: SourceAgentSettings}
	}
	if model := levels[level]; model != "" {
		return Pick{Agent: agent, Model: model, Source: SourceBinding}
	}
	return Pick{Agent: agent, Source: SourceAgentDefault}
}

// PolicyWarnings — что из tool_policy роли не дойдёт до агента (решение
// 0077 п. 3, CLAB-420): привязка не переводит политику вовсе, или
// переводит, а в команде нет подстановки для её части. Своя команда с
// подстановкой политики — перевод администратора, и для неё части
// проверяются так же. Пустая политика — предупреждений нет. Молча не
// теряется ни allow, ни deny: запуск без ограничения по именам — решение
// администратора, и он видит его в выводе.
func PolicyWarnings(agent string, translates bool, template, allow, deny []string) []string {
	if len(allow)+len(deny) == 0 {
		return nil
	}
	if !translates && !Uses(template, SlotAllowedTools) && !Uses(template, SlotDisallowedTools) {
		return []string{"tool_policy роли не переведён для привязки " + agent + ": " + policyText(allow, deny) +
			"; агент запущен без ограничений по тулам"}
	}
	var out []string
	if len(allow) > 0 && !Uses(template, SlotAllowedTools) {
		out = append(out, "tool_policy роли: allow не передан привязке "+agent+" — в команде нет "+SlotAllowedTools+
			" (allow="+strings.Join(allow, ",")+"); агент запущен без этого ограничения")
	}
	if len(deny) > 0 && !Uses(template, SlotDisallowedTools) {
		out = append(out, "tool_policy роли: deny не передан привязке "+agent+" — в команде нет "+SlotDisallowedTools+
			" (deny="+strings.Join(deny, ",")+"); запрещённые роли тулы агенту доступны")
	}
	return out
}

func policyText(allow, deny []string) string {
	dash := func(list []string) string {
		if len(list) == 0 {
			return "—"
		}
		return strings.Join(list, ",")
	}
	return "allow=" + dash(allow) + ", deny=" + dash(deny)
}
