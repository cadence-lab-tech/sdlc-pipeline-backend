package main

// Необязательные этапы цикла (решение 0066): флаг --stage у create и edit,
// строка «Этапы» в сводке, назначение перехода с пропуском в gates и claim,
// строка «пропущено» после transition. Какие ключи — необязательные этапы
// цикла задачи, проверяет сервер: CLI знает только форму <статус>=on|off.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const stageUsage = "выбор необязательного этапа <статус>=on|off, например design=off; " +
	"флаг повторяется для разных статусов; не названные этапы не меняются. " +
	"Что статус — необязательный этап цикла задачи, проверяет сервер"

// optionalStageFlag собирает повторяемый --stage. Ошибка формы — до запроса:
// задача не меняется.
type optionalStageFlag struct {
	choice map[string]bool
}

func (f *optionalStageFlag) String() string { return "" }

func (f *optionalStageFlag) Set(value string) error {
	status, state, ok := strings.Cut(value, "=")
	status, state = strings.TrimSpace(status), strings.TrimSpace(state)
	if !ok {
		return fmt.Errorf("%q не по форме <статус>=on|off, например design=off", value)
	}
	if status == "" {
		return fmt.Errorf("%q: не указан статус, форма <статус>=on|off, например design=off", value)
	}
	var on bool
	switch state {
	case "on":
		on = true
	case "off":
	default:
		return fmt.Errorf("%q: значение должно быть on или off, например design=off", value)
	}
	if _, dup := f.choice[status]; dup {
		return fmt.Errorf("статус %s указан дважды: у этапа один выбор, например design=off", status)
	}
	if f.choice == nil {
		f.choice = map[string]bool{}
	}
	f.choice[status] = on
	return nil
}

// patch — тело optional_stages запроса: только переданные ключи.
func (f *optionalStageFlag) patch() map[string]bool {
	out := make(map[string]bool, len(f.choice))
	for k, v := range f.choice {
		out[k] = v
	}
	return out
}

// addOptionalStages кладёт выбор в тело запроса и отвечает, передан ли он.
func addOptionalStages(body map[string]any, passed map[string]bool, stages *optionalStageFlag) bool {
	if !passed["stage"] || len(stages.choice) == 0 {
		return false
	}
	body["optional_stages"] = stages.patch()
	return true
}

// serverKnowsOptionalStages — признак сервера с необязательными этапами:
// задача в его ответе всегда несёт ключ optional_stages, пусть {}. Сервер
// прежней версии неизвестное поле запроса молча выбрасывает, и без этой
// проверки выбор терялся бы.
func serverKnowsOptionalStages(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	_, ok := fields["optional_stages"]
	return ok
}

// optionalStagesNotStored — ошибка «выбор не записан» сервера прежней версии.
// Остальное из запроса сервер применил, это сказано явно.
func optionalStagesNotStored(done string) error {
	return fmt.Errorf("выбор этапов не записан: сервер не знает необязательных этапов (нужен сервер с "+
		"решением 0066, clab version покажет его версию); %s", done)
}

// taskStages — выбор этапов задачи из ответа: ключ необязательного статуса
// → включён ли. Сервер прежней версии поля не отдаёт.
type taskStages struct {
	OptionalStages map[string]bool `json:"optional_stages"`
}

// line — «Этапы: design выкл, docs вкл», ключи по алфавиту: порядок позиций
// в цикле CLI не знает. Пустой набор — строки нет.
func (s taskStages) line() string {
	if len(s.OptionalStages) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s.OptionalStages))
	for k := range s.OptionalStages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		state := "выкл"
		if s.OptionalStages[k] {
			state = "вкл"
		}
		parts = append(parts, k+" "+state)
	}
	return "Этапы: " + strings.Join(parts, ", ")
}

// printStagesLine печатает строку выбора этапов из ответа с задачей; пусто
// или старый сервер — ничего.
func printStagesLine(raw json.RawMessage) error {
	var s taskStages
	if err := decode(raw, &s); err != nil {
		return err
	}
	if line := s.line(); line != "" {
		fmt.Println(line)
	}
	return nil
}

// landing — назначение перехода в ответах gates и claim (0066, п. 15):
// to_status — назначение в цикле, lands_in — где задача встанет на самом
// деле, skips — пропускаемые этапы, unavailable — причина недоступности.
// Сервер прежней версии трёх последних полей не отдаёт.
type landing struct {
	ToStatus    string   `json:"to_status"`
	LandsIn     *string  `json:"lands_in"`
	Skips       []string `json:"skips"`
	Unavailable string   `json:"unavailable"`
}

// text — правая часть строки перехода. Без пропуска и недоступности это
// to_status, как до 0066: вывод байт в байт прежний.
func (l landing) text() string {
	if l.Unavailable != "" {
		return "недоступен: " + unavailableReason(l.Unavailable, l.ToStatus)
	}
	to := l.ToStatus
	if l.LandsIn != nil && *l.LandsIn != "" {
		to = *l.LandsIn
	}
	if len(l.Skips) > 0 {
		to += " (пропуск: " + strings.Join(l.Skips, ", ") + ")"
	}
	return to
}

// unavailableReason — причина недоступности человеческим текстом; незнакомая
// причина — как есть: сервер новее CLI.
func unavailableReason(reason, status string) string {
	if reason == "stage_disabled" {
		return "этап " + status + " выключен у задачи"
	}
	return reason
}

// skippedLine — «пропущено: design» после перехода; пусто — строки нет.
func skippedLine(skips []string) string {
	if len(skips) == 0 {
		return ""
	}
	return "пропущено: " + strings.Join(skips, ", ")
}
