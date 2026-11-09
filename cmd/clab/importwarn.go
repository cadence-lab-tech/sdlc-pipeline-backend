package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Настройка агентов в связке (CLAB-428, решение 0077, п. 4.10). Сервер
// прежней версии неизвестное поле связки отбрасывает молча, и отличить
// «применил» от «потерял» можно только по отчёту: новый сервер отвечает
// строкой agent_settings, прежний — нет.

// importWarnAgentSettings — предупреждение клиента; текст один для stderr и
// для warnings отчёта.
const importWarnAgentSettings = "сервер не применил настройку агентов — обновите сервер"

// bundleCarriesAgentSettings — несёт ли связка поле agent_settings.
func bundleCarriesAgentSettings(bundle json.RawMessage) bool {
	var probe struct {
		AgentSettings json.RawMessage `json:"agent_settings"`
	}
	if err := json.Unmarshal(bundle, &probe); err != nil {
		return false
	}
	return len(probe.AgentSettings) > 0 && string(probe.AgentSettings) != "null"
}

// warnLostAgentSettings сверяет связку с отчётом одного проекта. Поле было,
// строки нет — предупреждение: в warnings отчёта (его видит и --json), а
// без --json ещё и на stderr. Код выхода не меняется: остальные части связки
// сервер применил.
func warnLostAgentSettings(project string, sent bool, report map[string]any) {
	if !sent {
		return
	}
	if _, ok := report["agent_settings"]; ok {
		return
	}
	warnings, _ := report["warnings"].([]any)
	report["warnings"] = append(warnings, importWarnAgentSettings)
	if !jsonOut {
		fmt.Fprintf(os.Stderr, "Предупреждение: проект %s: %s.\n", project, importWarnAgentSettings)
	}
}
