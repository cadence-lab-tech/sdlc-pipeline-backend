package main

import (
	"fmt"
	"time"
)

// stageLimitView — блок stage_limit (CLAB-221): предел времени этапа из
// ответа захвата, состояния и предпросмотра переходов. Сервер прежней версии
// его не отдаёт, null — у этапа предела нет; в обоих случаях строки нет.
type stageLimitView struct {
	Gate             string     `json:"gate"`
	Transition       string     `json:"transition"`
	Status           string     `json:"status"`
	MaxDuration      string     `json:"max_duration"`
	Deadline         time.Time  `json:"deadline"`
	ElapsedSeconds   int64      `json:"elapsed_seconds"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	Overdue          bool       `json:"overdue"`
	Waived           bool       `json:"waived"`
	WaivedBy         *string    `json:"waived_by"`
	WaivedAt         *time.Time `json:"waived_at"`
}

// line — строка «Предел этапа»: предел, переход входа и что с ним сейчас.
// У просроченного — чем его снять: это решение человека, и команда нужна
// ему, а не агенту.
func (l *stageLimitView) line(task string) string {
	if l == nil {
		return ""
	}
	head := fmt.Sprintf("%s с входа переходом %s", l.MaxDuration, l.Transition)
	switch {
	case l.Waived:
		who := "человеком"
		if l.WaivedBy != nil && *l.WaivedBy != "" {
			who = *l.WaivedBy
		}
		return head + ", снят " + who + " до конца этапа"
	case l.Overdue:
		over := time.Duration(l.ElapsedSeconds)*time.Second - limitDuration(l)
		return fmt.Sprintf("%s, просрочен на %s (срок %s); снять — человек: clab decide %s --transition %s --gate %s --status waived",
			head, humanDuration(over), when(&l.Deadline), task, l.Transition, l.Gate)
	default:
		return fmt.Sprintf("%s, осталось %s (до %s)",
			head, humanDuration(time.Duration(l.RemainingSeconds)*time.Second), when(&l.Deadline))
	}
}

func limitDuration(l *stageLimitView) time.Duration {
	d, err := time.ParseDuration(l.MaxDuration)
	if err != nil {
		return 0
	}
	return d
}

// humanDuration — длительность без секунд, если она дольше минуты: «3h12m»,
// а не «3h12m7s».
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= time.Minute {
		d = d.Truncate(time.Minute)
	}
	s := d.String()
	if len(s) > 2 && s[len(s)-2:] == "0s" && d >= time.Minute {
		s = s[:len(s)-2]
	}
	return s
}
