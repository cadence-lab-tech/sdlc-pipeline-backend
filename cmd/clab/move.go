package main

// clab move (CLAB-408, эпик CLAB-404): перевод задачи в любой статус её
// цикла мимо переходов и гейтов — как в трекере. Право, статус, причина и
// выключенный этап решает сервер (MoveTask движка); CLI отсекает только
// пустые флаги и узнаёт сервер прежней версии по 404 без кода.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// errMoveOldServer — у сервера нет ручки перевода: он старше CLAB-408.
var errMoveOldServer = errors.New("сервер не знает перевода в статус, обновите сервер (версия сервера — clab version)")

func cmdMove(ctx context.Context, c *Client, args []string) error {
	var to, reason string
	task, rest := popTask(args)
	fs, err := flags("move", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&to, "to", "", "ключ статуса цикла задачи, куда перевести")
		fs.StringVar(&reason, "reason", "", "почему задачу переводят; обязательна")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	to, reason = strings.TrimSpace(to), strings.TrimSpace(reason)
	if to == "" {
		return fmt.Errorf("не указан --to: ключ статуса цикла задачи")
	}
	if reason == "" {
		return fmt.Errorf("не указана --reason: перевод в статус без причины не делается")
	}

	var out json.RawMessage
	if err := c.post(ctx, "/tasks/"+url.PathEscape(task)+"/move",
		map[string]string{"to": to, "reason": reason}, &out); err != nil {
		var apiErr *APIError
		// Plain-text 404 без кода — маршрута нет: сервер прежней версии.
		// «Задачи нет» приходит конвертом с кодом not_found.
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Code == "" {
			return errMoveOldServer
		}
		return withDenialHint(err)
	}
	return emit(out, render(renderMoveResult, out))
}

// runOutcomeLabel — исход прогона словами, а не ключом.
func runOutcomeLabel(outcome string) string {
	switch outcome {
	case "advanced":
		return "пройден"
	case "rejected":
		return "возвращён"
	case "cancelled":
		return "снят"
	case "moved":
		return "переведён"
	}
	return outcome
}

// renderMoveResult — откуда и куда, какая попытка открыта и что закрыто.
func renderMoveResult(raw json.RawMessage) error {
	var out struct {
		Task struct {
			Key       string `json:"key"`
			StatusKey string `json:"status_key"`
		} `json:"task"`
		StageRun struct {
			StatusKey string `json:"status_key"`
			Attempt   int    `json:"attempt"`
		} `json:"stage_run"`
		PreviousRun *struct {
			StatusKey string  `json:"status_key"`
			Attempt   int     `json:"attempt"`
			Outcome   *string `json:"outcome"`
		} `json:"previous_run"`
		ClosedReviews int `json:"closed_reviews"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	from := "—"
	if out.PreviousRun != nil {
		from = out.PreviousRun.StatusKey
	}
	fmt.Printf("%s: %s → %s (перевод, гейты не проверялись), попытка %d\n",
		out.Task.Key, from, out.Task.StatusKey, out.StageRun.Attempt)
	if p := out.PreviousRun; p != nil && p.Outcome != nil {
		line := fmt.Sprintf("Прогон %s попытки %d: %s", p.StatusKey, p.Attempt, runOutcomeLabel(*p.Outcome))
		if out.ClosedReviews > 0 {
			line += fmt.Sprintf(", перепроверок закрыто: %d", out.ClosedReviews)
		}
		fmt.Println(line + "; аренда снята, токен исполнителя погашен")
	}
	return nil
}
