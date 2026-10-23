package main

// Рекомендованный уровень модели (решение 0060): флаги записи у create, edit
// и role put, строки сводки claim и show. Значения уровней и ключи статусов
// проверяет сервер — CLI правило не дублирует и про конкретные статусы не
// знает. Сам он проверяет только форму --model-level-for.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	modelLevelUsage = "рекомендованный уровень модели задачи: low, medium, high; \"\" снимает"
	stageLevelUsage = "уровень модели этапа <статус>=<уровень>, например development=high; " +
		"флаг повторяется для разных статусов; <статус>= снимает уровень этапа"
	roleLevelUsage = "уровень модели версии роли по умолчанию: low, medium, high. Без флага у новой " +
		"версии уровня нет, как у прочих полей put; сервер без уровней (до решения 0060) поле теряет молча"
)

// stageLevelFlag собирает повторяемый --model-level-for в stage_settings.
// Ошибка формы — до запроса: задача не меняется.
type stageLevelFlag struct {
	levels map[string]string
}

func (f *stageLevelFlag) String() string { return "" }

func (f *stageLevelFlag) Set(value string) error {
	status, level, ok := strings.Cut(value, "=")
	status, level = strings.TrimSpace(status), strings.TrimSpace(level)
	if !ok {
		return fmt.Errorf("%q не по форме <статус>=<уровень>, например development=high; "+
			"<статус>= снимает уровень этапа", value)
	}
	if status == "" {
		return fmt.Errorf("%q: не указан статус, форма <статус>=<уровень>", value)
	}
	if _, dup := f.levels[status]; dup {
		return fmt.Errorf("статус %s указан дважды: у этапа один уровень", status)
	}
	if f.levels == nil {
		f.levels = map[string]string{}
	}
	f.levels[status] = level
	return nil
}

// stageSettings — тело stage_settings запроса: пустой уровень уходит "" и
// снимает уровень этапа на сервере.
func (f *stageLevelFlag) stageSettings() map[string]any {
	out := map[string]any{}
	for status, level := range f.levels {
		out[status] = map[string]string{"model_level": level}
	}
	return out
}

// addModelLevels кладёт в тело запроса то, что передано флагами, и
// отвечает, передано ли хоть что-то.
func addModelLevels(body map[string]any, passed map[string]bool, level string, stages *stageLevelFlag) bool {
	set := false
	if passed["model-level"] {
		body["model_level"] = strings.TrimSpace(level)
		set = true
	}
	if passed["model-level-for"] && len(stages.levels) > 0 {
		body["stage_settings"] = stages.stageSettings()
		set = true
	}
	return set
}

// serverKnowsLevels — признак сервера с уровнями модели: задача в его ответе
// всегда несёт ключ model_level, пусть null. Сервер прежней версии неизвестное
// поле запроса молча выбрасывает, и без этой проверки уровень терялся бы.
func serverKnowsLevels(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	_, ok := fields["model_level"]
	return ok
}

// levelsNotStored — ошибка «уровень не записан» сервера прежней версии.
// Остальное из запроса сервер применил, это сказано явно.
func levelsNotStored(done string) error {
	return fmt.Errorf("уровень модели не записан: сервер не знает уровней (нужен сервер с решением 0060, "+
		"clab version покажет его версию); %s", done)
}

// taskLevels — уровни задачи из ответа: задача и этапы по ключу статуса.
type taskLevels struct {
	ModelLevel    *string         `json:"model_level"`
	StageSettings json.RawMessage `json:"stage_settings"`
}

// stages — заданные уровни этапов «статус=уровень» по порядку ключей.
func (l taskLevels) stages() []string {
	var settings map[string]struct {
		ModelLevel string `json:"model_level"`
	}
	if len(l.StageSettings) == 0 || json.Unmarshal(l.StageSettings, &settings) != nil {
		return nil
	}
	keys := make([]string, 0, len(settings))
	for k, s := range settings {
		if s.ModelLevel != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+settings[k].ModelLevel)
	}
	return out
}

// editLine — строка правки: «Уровень: medium   Этапы: development=high».
func (l taskLevels) editLine() string {
	return fmt.Sprintf("Уровень: %s   Этапы: %s", dash(ptr(l.ModelLevel)), joinOr(l.stages(), "—"))
}

// summaryLine — строка сводки claim и show: текущие уровни задачи, а не
// снимок прогона — они могут разойтись, и CLI ничего не пересчитывает.
func (l taskLevels) summaryLine() string {
	task, stages := ptr(l.ModelLevel), l.stages()
	if task == "" && len(stages) == 0 {
		return "Уровни задачи: не заданы"
	}
	return fmt.Sprintf("Уровни задачи: задача %s; этапы %s", dash(task), joinOr(stages, "—"))
}

// executionView — рекомендация прогона из ответа захвата и состояния.
// Сервер прежней версии поля не отдаёт: тогда строк уровня нет вовсе.
type executionView struct {
	ModelLevel       *string `json:"model_level"`
	ModelLevelSource string  `json:"model_level_source"`
}

// recommendedLine — «Рекомендованный уровень: high (этап задачи)». На эту
// строку опираются скилы исполнителя этапа: формулировку не менять.
func (e executionView) recommendedLine() string {
	level := ptr(e.ModelLevel)
	if level == "" || e.ModelLevelSource == "none" {
		return "Рекомендованный уровень: не задан"
	}
	return fmt.Sprintf("Рекомендованный уровень: %s (%s)", level, levelSource(e.ModelLevelSource))
}

// levelSource — подпись источника; незнакомый — как есть: сервер новее CLI.
func levelSource(source string) string {
	switch source {
	case "stage":
		return "этап задачи"
	case "task":
		return "задача"
	case "role":
		return "роль"
	}
	return dash(source)
}
