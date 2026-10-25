package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Распознавание исчерпанного лимита подписки агента (CLAB-215).
//
// Это знание о Claude Code, а не о раннере: когда появятся адаптеры агентов
// (CLAB-118), функция переедет в адаптер. Чужая SDLC_RUNNER_COMMAND без
// такого вывода раннер на паузу не ставит. Живёт отдельно от parseUsage:
// стоимость и лимит читают один вывод, но отвечают на разные вопросы.

// limitWindow — дальше этого время сброса не верится: скорее разбор
// ошибся, чем подписка встала на неделю, и раннер берёт свой срок.
const limitWindow = 7 * 24 * time.Hour

// agentLimit — лимит подписки, найденный в выводе агента.
type agentLimit struct {
	// Когда лимит сбросится; нулевое — время не названо, не разобралось
	// или лежит вне окна, и паузу раннер берёт из SDLC_RUNNER_LIMIT_PAUSE.
	resetAt time.Time
}

var (
	// Прежний формат Claude Code: «Claude AI usage limit reached|<unix>».
	legacyLimitRe = regexp.MustCompile(`Claude AI usage limit reached(?:\|(\d{9,12}))?`)
	// Текущий формат (Claude Code 2.1.x): «You've hit your session limit ·
	// resets 8:50pm (Europe/Moscow)», бывает без вида лимита и без зоны.
	limitRe = regexp.MustCompile(`(?i)you(?:'|’)?ve hit your (?:[\w-]+ ){0,2}limit\b(?:\s*[·•∙|-]\s*resets\s+([^\n"]*))?`)
	// Время сброса: «3pm», «8:50pm», «20:50», с датой «Oct 30, 3pm» или
	// «Oct 30 at 3pm» и зоной в скобках.
	resetRe = regexp.MustCompile(`(?i)^(?:([a-z]{3,9})\.?\s+(\d{1,2})(?:,|\s+at)?\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)
)

// detectLimit ищет признак лимита в выводе агента при любом коде выхода.
//
// JSON Claude Code (--output-format json) — признак только в ошибке
// (is_error) и только в её тексте: успешный ответ, где агент рассказывает
// о лимитах, паузы не значит. Вывод не JSON — признак ищется в тексте
// целиком: так печатает Claude Code без --output-format.
func detectLimit(out []byte, now time.Time) (agentLimit, bool) {
	text, ok := limitText(out)
	if !ok {
		return agentLimit{}, false
	}
	if m := legacyLimitRe.FindStringSubmatch(text); m != nil {
		var lim agentLimit
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			lim.resetAt = inWindow(time.Unix(sec, 0), now)
		}
		return lim, true
	}
	if m := limitRe.FindStringSubmatch(text); m != nil {
		return agentLimit{resetAt: inWindow(parseReset(strings.TrimSpace(m[1]), now), now)}, true
	}
	return agentLimit{}, false
}

// limitText — текст, в котором ищется признак: у JSON-ответа Claude Code
// поле result ошибки, у прочего вывода — он сам.
func limitText(out []byte) (string, bool) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return "", false
	}
	if trimmed[0] == '{' {
		var res struct {
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
		}
		if json.Unmarshal(trimmed, &res) == nil {
			return res.Result, res.IsError
		}
	}
	return string(trimmed), true
}

// parseReset разбирает время сброса из текста Claude Code. Зона — из скобок,
// а неизвестная зона или её отсутствие — местная: Claude Code печатает
// время машины, на которой он и раннер работают. Без даты — ближайшее
// такое время после now.
func parseReset(raw string, now time.Time) time.Time {
	m := resetRe.FindStringSubmatch(raw)
	if m == nil || raw == "" {
		return time.Time{}
	}
	hour, _ := strconv.Atoi(m[3])
	minute := 0
	if m[4] != "" {
		minute, _ = strconv.Atoi(m[4])
	}
	switch ampm := strings.ToLower(m[5]); {
	case ampm == "" && m[4] == "":
		return time.Time{} // «resets 3» — не время
	case ampm == "" && hour > 23, ampm != "" && (hour < 1 || hour > 12), minute > 59:
		return time.Time{}
	case ampm == "am" && hour == 12:
		hour = 0
	case ampm == "pm" && hour != 12:
		hour += 12
	}

	loc := time.Local
	if tz := strings.TrimSpace(m[6]); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	local := now.In(loc)

	if m[1] == "" {
		at := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		if !at.After(local) {
			at = at.AddDate(0, 0, 1)
		}
		return at
	}
	month, err := time.Parse("Jan", strings.ToUpper(m[1][:1])+strings.ToLower(m[1][1:3]))
	if err != nil {
		return time.Time{}
	}
	day, _ := strconv.Atoi(m[2])
	at := time.Date(local.Year(), month.Month(), day, hour, minute, 0, 0, loc)
	if at.Day() != day {
		return time.Time{} // «Feb 31»
	}
	if at.Before(local.AddDate(0, -6, 0)) {
		at = at.AddDate(1, 0, 0) // «Jan 2» в конце декабря — следующий год
	}
	return at
}

// inWindow оставляет время сброса, если оно впереди и не дальше окна.
func inWindow(at, now time.Time) time.Time {
	if at.IsZero() || !at.After(now) || at.Sub(now) > limitWindow {
		return time.Time{}
	}
	return at
}
