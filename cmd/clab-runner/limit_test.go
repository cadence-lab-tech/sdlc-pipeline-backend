package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// CLAB-215, AC 13: распознавание лимита подписки по таблице выводов.
//
// Текущий формат снят с журнала Claude Code 2.1.296 (~/.claude/projects,
// запись isApiErrorMessage с error=rate_limit, октябрь 2026): «You've hit
// your session limit · resets 8:50pm (Europe/Moscow)». Там же — вариант без
// вида лимита и зоны («You've hit your limit · resets 3pm») и та же строка в
// ошибке саб-агента с хвостом «(error type rate_limit, HTTP 429, …)».
// Обёртка --output-format json (type, is_error, result) — формат Claude
// Code для ошибки в режиме -p. Вид с датой («weekly … resets Oct 30, 3pm»)
// живьём не снят и держится как допущение.
func TestDetectLimit(t *testing.T) {
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 1, 0, 0, 0, moscow)
	inThreeHours := now.Add(3 * time.Hour)
	local := now.In(time.Local)
	nextLocal3pm := time.Date(local.Year(), local.Month(), local.Day(), 15, 0, 0, 0, time.Local)
	if !nextLocal3pm.After(local) {
		nextLocal3pm = nextLocal3pm.AddDate(0, 0, 1)
	}
	result := func(isError bool, text string) string {
		raw, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": isError,
			"duration_ms": 412, "num_turns": 1, "result": text, "session_id": "5c1e…", "total_cost_usd": 0,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0}})
		return string(raw)
	}
	legacy := func(at time.Time) string { return fmt.Sprintf("Claude AI usage limit reached|%d", at.Unix()) }

	for name, tc := range map[string]struct {
		out   string
		limit bool
		reset time.Time // нулевое — время сброса неизвестно
	}{
		"старый формат в JSON":      {out: result(true, legacy(inThreeHours)), limit: true, reset: inThreeHours},
		"старый формат текстом":     {out: legacy(inThreeHours) + "\n", limit: true, reset: inThreeHours},
		"старый формат без времени": {out: result(true, "Claude AI usage limit reached"), limit: true},
		"Claude Code 2.1.296": {out: result(true, "You've hit your session limit · resets 8:50pm (Europe/Moscow)"),
			limit: true, reset: time.Date(2026, 10, 26, 20, 50, 0, 0, moscow)},
		"без вида лимита и зоны": {out: result(true, "You've hit your limit · resets 3pm"),
			limit: true, reset: nextLocal3pm},
		"ошибка саб-агента текстом": {out: "Agent terminated early due to an API error: You've hit your session limit · " +
			"resets 1:30am (Europe/Moscow) (error type rate_limit, HTTP 429, request id req_011Cg)",
			limit: true, reset: time.Date(2026, 10, 26, 1, 30, 0, 0, moscow)},
		"с датой (допущение)": {out: result(true, "You've hit your weekly limit · resets Oct 30, 3pm (Europe/Moscow)"),
			limit: true, reset: time.Date(2026, 10, 30, 15, 0, 0, 0, moscow)},
		"полночь завтра": {out: result(true, "You've hit your session limit · resets 12am (Europe/Moscow)"),
			limit: true, reset: time.Date(2026, 10, 27, 0, 0, 0, 0, moscow)},
		"24 часа": {out: result(true, "You've hit your session limit · resets 20:50 (Europe/Moscow)"),
			limit: true, reset: time.Date(2026, 10, 26, 20, 50, 0, 0, moscow)},
		"неизвестная зона — местная": {out: result(true, "You've hit your limit · resets 3pm (Mars/Olympus)"),
			limit: true, reset: nextLocal3pm},
		"время не разобралось": {out: result(true, "You've hit your session limit · resets soon"), limit: true},
		"время в прошлом":      {out: result(true, legacy(now.Add(-time.Minute))), limit: true},
		"дальше 7 суток":       {out: result(true, legacy(now.Add(8*24*time.Hour))), limit: true},
		"13pm":                 {out: result(true, "You've hit your limit · resets 13pm"), limit: true},

		"is_error без слов о лимите": {out: result(true, "API Error: 500 Internal server error")},
		"успешный ответ о лимитах":   {out: result(false, "You've hit your session limit · resets 8:50pm — так пишет Claude Code")},
		"успешный ответ":             {out: `{"total_cost_usd": 0.5, "model": "claude-sonnet-5"}`},
		"чужой текст":                {out: "Error: limit\n"},
		"пустой вывод":               {out: ""},
		"пробелы":                    {out: " \n\t"},
	} {
		t.Run(name, func(t *testing.T) {
			lim, ok := detectLimit([]byte(tc.out), now)
			if ok != tc.limit {
				t.Fatalf("лимит: %v, ждали %v", ok, tc.limit)
			}
			if !lim.resetAt.Equal(tc.reset) {
				t.Fatalf("сброс %s, ждали %s", lim.resetAt, tc.reset)
			}
		})
	}
}

// Время сброса без даты — ближайшее после now, в том числе через полночь.
func TestParseResetRollsOver(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 12, 31, 23, 0, 0, 0, utc)
	for raw, want := range map[string]time.Time{
		"11:30pm (UTC)":      time.Date(2026, 12, 31, 23, 30, 0, 0, utc),
		"10pm (UTC)":         time.Date(2027, 1, 1, 22, 0, 0, 0, utc),
		"12pm (UTC)":         time.Date(2027, 1, 1, 12, 0, 0, 0, utc),
		"Jan 2, 3pm (UTC)":   time.Date(2027, 1, 2, 15, 0, 0, 0, utc),
		"Jan 2 at 3pm (UTC)": time.Date(2027, 1, 2, 15, 0, 0, 0, utc),
		"Feb 31, 3pm (UTC)":  {},
		"25:00":              {},
		"3":                  {},
		"":                   {},
	} {
		if got := parseReset(raw, now); !got.Equal(want) {
			t.Errorf("%q: %s, ждали %s", raw, got, want)
		}
	}
	if !strings.Contains(limitNote(now, false), "неизвестно") {
		t.Fatal("причина без времени сброса говорит, что оно неизвестно")
	}
}
