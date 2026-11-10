package launch

// Разбор вывода агента — свойство привязки (поле output набора, CLAB-420):
// claude-json — JSON Claude Code с --output-format json, codex-jsonl —
// события codex exec --json. Привязка без разбора (generic, своя команда
// неизвестного агента) отчёта запускающего не даёт: о стоимости
// отчитывается агент сам, по стартовой инструкции без usage=runner.
//
// Счётчики токенов запускающий берёт из вывода, а не из самоотчёта агента:
// агент, который называет числа сам, занижает их в десятки раз (этапы
// CLAB-419: 2800/500 сообщено при ~294 тыс. входа с кэшем по выводу).
//
// Один разбор на раннер и clab exec (CLAB-470): копия в каждом package
// main разошлась бы на первой правке. Кто отчитался, вызывающий называет
// сам — значение details.reported_by.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// Форматы вывода — значения поля output привязки.
const (
	OutputClaudeJSON = "claude-json"
	OutputCodexJSONL = "codex-jsonl"
)

// LauncherReports — отчитается ли о стоимости запускающий (раннер, clab
// exec). Решается до запуска: от этого зависит текст инструкции
// (usage=runner). У claude-json модель всегда в выводе, у codex-jsonl её в
// выводе нет, и запускающий может назвать только ту, что передал сам в
// {model}.
func LauncherReports(format, model string) bool {
	switch format {
	case OutputClaudeJSON:
		return true
	case OutputCodexJSONL:
		return model != ""
	}
	return false
}

// formatFlags — слова команды, которые включают формат вывода у программы:
// без них вывод не в формате, и обещать отчёт по выводу нельзя. Ключ —
// значение output привязки, а не имя агента.
var formatFlags = map[string][][]string{
	OutputClaudeJSON: {{"--output-format", "json"}, {"--output-format=json"}},
	OutputCodexJSONL: {{"--json"}, {"--experimental-json"}},
}

// Parses — умеет ли общий разбор этот формат вывода.
func Parses(format string) bool {
	_, ok := formatFlags[format]
	return ok
}

// EmitsFormat — несёт ли команда флаг, включающий формат вывода. Проверка
// консервативная: формат без известного флага или команда без него — нет.
// Своя команда запускающего (не шаблон привязки) без флага даёт вывод,
// который не разобрать, и о стоимости тогда отчитывается агент сам.
func EmitsFormat(format string, argv []string) bool {
	for _, seq := range formatFlags[format] {
		for i := range argv {
			if i+len(seq) <= len(argv) && slices.Equal(argv[i:i+len(seq)], seq) {
				return true
			}
		}
	}
	return false
}

// FormatFlagHint — флаг формата для подсказки в предупреждении.
func FormatFlagHint(format string) string {
	if seqs := formatFlags[format]; len(seqs) > 0 {
		return strings.Join(seqs[0], " ")
	}
	return ""
}

// ParseOutput — отчёт о стоимости по выводу агента в формате привязки;
// nil — «стоимость неизвестна», а не ноль (решение 0026). model —
// переданная агенту в {model} (нужна codex-jsonl), reportedBy — значение
// details.reported_by.
func ParseOutput(format string, raw []byte, model, reportedBy string) map[string]any {
	switch format {
	case OutputClaudeJSON:
		return ParseClaudeJSON(raw, reportedBy)
	case OutputCodexJSONL:
		return ParseCodexJSONL(raw, model, reportedBy)
	}
	return nil
}

// ParseCodexJSONL суммирует usage событий turn.completed codex exec --json.
//
// Формат (Codex 0.167.0, codex-rs/exec/src/exec_events.rs, Usage):
// input_tokens, cached_input_tokens, cache_write_input_tokens (serde
// default, у прежних версий поля нет), output_tokens,
// reasoning_output_tokens. input_tokens — весь вход ответа Responses API,
// чтение и запись кэша — его части (codex-rs/codex-api/src/sse/responses.rs:
// input_tokens 100 = cached_tokens 40 + cache_write_tokens 60). Учёт сервера
// раскладывает вход на непересекающиеся корзины, как у Claude Code: входные —
// input_tokens без чтения и записи кэша, чтение — cache_read_tokens, запись —
// cache_write_tokens; сумма трёх равна input_tokens Codex, ни потери, ни
// двойного счёта. Без поля записи разбор прежний: cache_write_tokens нет.
// reasoning_output_tokens — часть output_tokens, отдельно не складывается.
//
// Модели и денег в выводе нет: модель — переданная в {model}, сумму по
// прайсу версии посчитает сервер (CLAB-426). Ни одного разобранного хода —
// nil: молчание честнее нуля.
func ParseCodexJSONL(raw []byte, model, reportedBy string) map[string]any {
	if model == "" {
		return nil
	}
	var input, cached, written, output int64
	turns, writes := 0, false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 16<<20) // строки событий с выводом команд бывают длинными
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Usage *struct {
				InputTokens           *int64 `json:"input_tokens"`
				CachedInputTokens     *int64 `json:"cached_input_tokens"`
				CacheWriteInputTokens *int64 `json:"cache_write_input_tokens"`
				OutputTokens          *int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type != "turn.completed" || ev.Usage == nil {
			continue
		}
		in, c, w, o := deref(ev.Usage.InputTokens), deref(ev.Usage.CachedInputTokens),
			deref(ev.Usage.CacheWriteInputTokens), deref(ev.Usage.OutputTokens)
		input += max(in-c-w, 0)
		cached += c
		written += w
		output += o
		writes = writes || ev.Usage.CacheWriteInputTokens != nil
		turns++
	}
	if turns == 0 {
		return nil
	}
	report := map[string]any{
		"model":             model,
		"input_tokens":      input,
		"cache_read_tokens": cached,
		"output_tokens":     output,
		"details":           map[string]any{"reported_by": reportedBy},
	}
	if writes {
		report["cache_write_tokens"] = written
	}
	return report
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// ParseClaudeJSON достаёт стоимость из вывода Claude Code с
// --output-format json.
//
// Чужой агент отдаст что-то другое, и это не ошибка: неразобранный вывод
// означает «стоимость неизвестна», а не «ноль» (решение 0026).
func ParseClaudeJSON(out []byte, reportedBy string) map[string]any {
	var result struct {
		TotalCostUSD *float64 `json:"total_cost_usd"`
		DurationMS   *int64   `json:"duration_ms"`
		Model        string   `json:"model"`
		Usage        struct {
			InputTokens              *int64 `json:"input_tokens"`
			OutputTokens             *int64 `json:"output_tokens"`
			CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		ModelUsage map[string]struct {
			InputTokens  *int64   `json:"inputTokens"`
			OutputTokens *int64   `json:"outputTokens"`
			CostUSD      *float64 `json:"costUSD"`
		} `json:"modelUsage"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil
	}

	model := result.Model
	if model == "" {
		// Разрез по моделям — map, и порядок обхода у неё случайный: та же
		// выдача агента записывалась бы то одной моделью, то другой.
		// Основная модель запуска — та, что стоила больше всего; при
		// равенстве — та, что больше написала, затем по имени.
		// Несообщённое число — ниже любого сообщённого, нуля тоже.
		bestCost, bestOut := float64(-1), int64(-1)
		for name, u := range result.ModelUsage {
			cost, out := float64(-1), int64(-1)
			if u.CostUSD != nil {
				cost = *u.CostUSD
			}
			if u.OutputTokens != nil {
				out = *u.OutputTokens
			}
			better := model == "" || cost > bestCost ||
				(cost == bestCost && (out > bestOut || (out == bestOut && name < model)))
			if better {
				model, bestCost, bestOut = name, cost, out
			}
		}
	}
	if model == "" {
		// Без модели отчёт не с чем сравнивать, а сравнение моделей — весь
		// смысл учёта; такой вывод честнее не записывать.
		return nil
	}

	report := map[string]any{"model": model}
	if result.TotalCostUSD != nil {
		report["cost_usd"] = *result.TotalCostUSD
	}
	if result.DurationMS != nil {
		report["duration_ms"] = *result.DurationMS
	}
	if v := result.Usage.InputTokens; v != nil {
		report["input_tokens"] = *v
	}
	if v := result.Usage.OutputTokens; v != nil {
		report["output_tokens"] = *v
	}
	if v := result.Usage.CacheReadInputTokens; v != nil {
		report["cache_read_tokens"] = *v
	}
	if v := result.Usage.CacheCreationInputTokens; v != nil {
		report["cache_write_tokens"] = *v
	}
	report["details"] = map[string]any{"reported_by": reportedBy}
	return report
}
