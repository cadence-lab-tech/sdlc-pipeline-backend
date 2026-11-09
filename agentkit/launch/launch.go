// Package launch — запуск агента по шаблону run привязки набора (решение
// 0077, п. 3): подстановки в слова команды, снятие флага с пустым значением
// и сценарий хука из фронтматтера агента. Общий для clab exec (CLAB-419) и
// раннера (CLAB-420): одна функция, а не две копии trimDanglingFlag.
//
// Пакет не встраивает набор: его импортирует CLI, а тексты набора отдаёт
// сервер (GET /v1/agent-kit).
package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
)

// Подстановки шаблона run. Значение каждой — одно слово команды или его
// часть; пустое значение снимает слово (и флаг перед ним, см. Expand).
const (
	SlotModel           = "{model}"
	SlotEffort          = "{effort}"
	SlotMCPConfig       = "{mcp_config}"
	SlotMCPURL          = "{mcp_url}"
	SlotConfigDir       = "{config_dir}"
	SlotRunDir          = "{run_dir}"
	SlotAllowedTools    = "{allowed_tools}"
	SlotDisallowedTools = "{disallowed_tools}"
	SlotTask            = "{task}"
	SlotRole            = "{role}"
	SlotWorkdir         = "{workdir}"
	// SlotHookScript — сценарий хука строкой в кавычках (базовая строка
	// TOML, она же строка JSON): шаблон вставляет её в значение настройки
	// агента как есть, например command={hook_script}.
	SlotHookScript = "{hook_script}"
	SlotPrompt     = "{prompt}"
)

// Slots — все подстановки, которые знает запуск; набор проверяет по ним
// шаблоны привязок.
var Slots = []string{
	SlotModel, SlotEffort, SlotMCPConfig, SlotMCPURL, SlotConfigDir, SlotRunDir,
	SlotAllowedTools, SlotDisallowedTools, SlotTask, SlotRole, SlotWorkdir,
	SlotHookScript, SlotPrompt,
}

// positional — подстановки-аргументы, а не значения флага: пустая снимает
// только своё слово.
var positional = []string{SlotPrompt}

var slotShape = regexp.MustCompile(`\{[a-z_]+\}`)

// SlotsIn — известные подстановки, встреченные в словах шаблона.
func SlotsIn(words []string) []string {
	var out []string
	for _, w := range words {
		for _, s := range slotShape.FindAllString(w, -1) {
			if slices.Contains(Slots, s) && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// Uses — есть ли подстановка в словах шаблона.
func Uses(words []string, slot string) bool {
	return slices.Contains(SlotsIn(words), slot)
}

// Expand подставляет значения в слова шаблона. Слово с известной
// подстановкой, значение которой пусто, снимается целиком, а если это
// значение флага — то вместе с флагом перед ним: «--model {model}»,
// «-m {model}», «-c model_reasoning_effort="{effort}"». Значение флага —
// слово, которое не начинается с «-», а флаг — слово с «-» и без «=».
// Подстановки-аргументы ({prompt}) флаг перед собой не снимают.
// Подставленные значения повторно не разбираются: фигурные скобки в
// инструкции остаются текстом.
func Expand(words []string, values map[string]string) []string {
	pairs := make([]string, 0, 2*len(Slots))
	for _, s := range Slots {
		pairs = append(pairs, s, values[s])
	}
	replacer := strings.NewReplacer(pairs...)
	out := make([]string, 0, len(words))
	for i, w := range words {
		if !emptySlot(w, values) {
			out = append(out, replacer.Replace(w))
			continue
		}
		if slices.Contains(positional, w) || strings.HasPrefix(w, "-") || i == 0 {
			continue
		}
		if prev := words[i-1]; isFlag(prev) && len(out) > 0 && out[len(out)-1] == prev {
			out = out[:len(out)-1]
		}
	}
	return out
}

func emptySlot(word string, values map[string]string) bool {
	for _, s := range slotShape.FindAllString(word, -1) {
		if slices.Contains(Slots, s) && values[s] == "" {
			return true
		}
	}
	return false
}

func isFlag(w string) bool {
	return len(w) > 1 && strings.HasPrefix(w, "-") && !strings.Contains(w, "=")
}

// Quoted — строка в двойных кавычках с экранированием: годится и как
// базовая строка TOML, и как строка JSON. Значение {hook_script}.
func Quoted(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// ErrNoHook — во фронтматтере нет блока «command: |» у хука.
var ErrNoHook = errors.New("во фронтматтере нет блока command: | у хука")

// HookCommand достаёт из фронтматтера файла агента тело хука — блок
// «command: |» — ровно тем текстом, что запустит агент: без отступа блока,
// строки через перевод строки, без завершающего.
func HookCommand(content string) (string, error) {
	var body []string
	indent := -1
	for _, l := range strings.Split(content, "\n") {
		depth := len(l) - len(strings.TrimLeft(l, " "))
		if indent < 0 {
			if strings.TrimSpace(l) == "command: |" {
				indent = depth + 2
			}
			continue
		}
		if l == "---" || (strings.TrimSpace(l) != "" && depth < indent) {
			break
		}
		body = append(body, strings.TrimPrefix(l, strings.Repeat(" ", indent)))
	}
	if len(body) == 0 {
		return "", ErrNoHook
	}
	return strings.Join(body, "\n"), nil
}
