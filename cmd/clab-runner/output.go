package main

import (
	"bufio"
	"bytes"
	"encoding/json"
)

// Разбор вывода агента — свойство привязки (поле output набора, CLAB-420):
// claude-json — JSON Claude Code с --output-format json, codex-jsonl —
// события codex exec --json. Привязка без разбора (generic, своя команда
// неизвестного агента) отчёта раннера не даёт: о стоимости отчитывается
// агент сам, по стартовой инструкции без usage=runner.
//
// Счётчики токенов раннер берёт из вывода, а не из самоотчёта агента:
// агент, который называет числа сам, занижает их в десятки раз (этапы
// CLAB-419: 2800/500 сообщено при ~294 тыс. входа с кэшем по выводу).

// runnerReports — отчитается ли о стоимости раннер. Решается до запуска:
// от этого зависит текст инструкции (usage=runner). У claude-json модель
// всегда в выводе, у codex-jsonl её в выводе нет, и раннер может назвать
// только ту, что передал сам в {model}.
func runnerReports(out, model string) bool {
	switch out {
	case outputClaudeJSON:
		return true
	case outputCodexJSONL:
		return model != ""
	}
	return false
}

// parseOutput — отчёт о стоимости по выводу агента в формате привязки;
// nil — «стоимость неизвестна», а не ноль (решение 0026).
func parseOutput(out string, raw []byte, model string) map[string]any {
	switch out {
	case outputClaudeJSON:
		return parseUsage(raw)
	case outputCodexJSONL:
		return parseCodexUsage(raw, model)
	}
	return nil
}

// parseCodexUsage суммирует usage событий turn.completed codex exec --json.
// input_tokens Codex включает кэшированные: входные — разность, кэш —
// cache_read_tokens. Модели и денег в выводе нет: модель — переданная в
// {model}, сумму по прайсу версии посчитает сервер (CLAB-426). Ни одного
// разобранного хода — nil: молчание честнее нуля.
func parseCodexUsage(raw []byte, model string) map[string]any {
	if model == "" {
		return nil
	}
	var input, cached, output int64
	turns := 0
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
				InputTokens       *int64 `json:"input_tokens"`
				CachedInputTokens *int64 `json:"cached_input_tokens"`
				OutputTokens      *int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type != "turn.completed" || ev.Usage == nil {
			continue
		}
		in, c, o := deref(ev.Usage.InputTokens), deref(ev.Usage.CachedInputTokens), deref(ev.Usage.OutputTokens)
		input += max(in-c, 0)
		cached += c
		output += o
		turns++
	}
	if turns == 0 {
		return nil
	}
	return map[string]any{
		"model":             model,
		"input_tokens":      input,
		"cache_read_tokens": cached,
		"output_tokens":     output,
		"details":           map[string]any{"reported_by": "clab-runner"},
	}
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// parseUsage достаёт стоимость из вывода агента.
//
// Формат — тот, что отдаёт Claude Code с --output-format json. Чужой агент
// отдаст что-то другое, и это не ошибка: неразобранный вывод означает
// «стоимость неизвестна», а не «ноль» (решение 0026).
func parseUsage(out []byte) map[string]any {
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
	report["details"] = map[string]any{"reported_by": "clab-runner"}
	return report
}
