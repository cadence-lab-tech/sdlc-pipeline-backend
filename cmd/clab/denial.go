package main

import (
	"encoding/json"
	"errors"
	"strings"
)

// Отказы по праву этапа (CLAB-382, решение 0072): сервер отвечает 403 с
// кодом причины в details.reason, а токену задачи — без него, с
// details.token_kind = task. Право считает движок; clab печатает текст
// сервера и, по полученному коду, строку «к кому идти». Своего права он не
// придумывает: нет кода — нет и строки.

// denialHints — что делать по коду причины details.reason.
var denialHints = map[string]string{
	"stage_role": "этап адресован роли, которой у вас нет: действие за участником с ролью этапа " +
		"или за сопровождающим проекта; проверить своё право заранее — clab gates",
	"no_stage_role": "у статуса нет исполнителя, переход глобальный (например cancel) или идёт не из текущего статуса: " +
		"решение сопровождающего проекта",
	"maintainer_action": "на своём этапе это действие за сопровождающим проекта (maintainer или admin)",
	"level":             "уровня участия в проекте не хватает: его выдаёт сопровождающий проекта",
	// Право на задачу (CLAB-411): бот работает в рамке аренды, область —
	// решение человека.
	"lease": "агент работает с задачей, этап которой держит по аренде, и с её детьми: " +
		"забрать этап — clab claim, иначе действие за человеком проекта",
	"human_action": "это решение человека — сотрудника или сопровождающего проекта: " +
		"опишите нужное и причину в артефакте этапа и в отчёте",
}

// taskTokenHint — отказ токену задачи: апрув и правила цикла ему закрыты
// (решение 0049), обходить его нечем и не нужно.
const taskTokenHint = "решение человека: токен задачи не закрывает такие гейты и не меняет правила цикла; " +
	"запишите несделанное в сданный артефакт и остановитесь"

// withDenialHint дописывает к отказу 403 строку «что делать». Только к
// отказам по праву: другие ошибки, успех и --json (скрипты читают ответ
// сервера как есть) не меняются.
func withDenialHint(err error) error {
	var apiErr *APIError
	if err == nil || jsonOut || !errors.As(err, &apiErr) || apiErr.Code != "forbidden" {
		return err
	}
	var details struct {
		Reason    string `json:"reason"`
		TokenKind string `json:"token_kind"`
	}
	if len(apiErr.Details) > 0 {
		_ = json.Unmarshal(apiErr.Details, &details)
	}
	hint := denialHints[details.Reason]
	if hint == "" && details.Reason == "" && details.TokenKind == "task" {
		hint = taskTokenHint
	}
	if hint == "" || strings.Contains(apiErr.Message, hint) {
		return err
	}
	return &APIError{Code: apiErr.Code, Message: apiErr.Message + "\n" + hint,
		Details: apiErr.Details, Status: apiErr.Status}
}
