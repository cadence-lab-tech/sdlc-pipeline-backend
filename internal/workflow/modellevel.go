// Рекомендованный уровень модели (решение 0060): проверка значений, слияние
// настроек этапов задачи, разрешение при захвате.
//
// Движок здесь ничего не знает про конкретные статусы и роли: ключи настроек
// этапов сверяются с версией цикла задачи, а разрешение идёт по данным
// прогона — его статусу и версии роли.

package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// stageSettingFields — поля объекта этапа в stage_settings. Неизвестное поле
// — отказ, а не молчание: опечатка не должна выключать настройку, а профиль
// исполнения (CLAB-64) добавит свои поля явно, строкой здесь.
var stageSettingFields = []string{"model_level"}

// Execution — рекомендация исполнителю этапа в ответе захвата. Место и для
// разрешённого профиля исполнения CLAB-64.
type Execution struct {
	ModelLevel       *string `json:"model_level"`
	ModelLevelSource string  `json:"model_level_source"`
}

// checkModelLevel проверяет уровень на записи: пустая строка — «снять» или
// «не задано», иначе одно из трёх значений.
func checkModelLevel(level, where string) error {
	if level == "" || domain.ValidModelLevel(level) {
		return nil
	}
	return apperr.InvalidInput("%s: уровень модели %q не поддерживается, допустимы %s",
		where, level, strings.Join(domain.ModelLevels, ", ")).
		WithDetails(map[string]any{"allowed": domain.ModelLevels})
}

// stagePatch — разобранный stage_settings из запроса: ключ статуса → поле →
// значение. Пустое значение снимает поле.
type stagePatch map[string]map[string]string

// parseStagePatch разбирает stage_settings запроса и проверяет форму и
// значения. Ключи статусов здесь не сверяются: для этого нужен цикл задачи.
func parseStagePatch(raw json.RawMessage) (stagePatch, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return stagePatch{}, nil
	}
	var stages map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &stages); err != nil || stages == nil {
		return nil, apperr.InvalidInput(
			`stage_settings — объект «ключ статуса → настройки этапа», например {"development": {"model_level": "high"}}`)
	}
	out := stagePatch{}
	for key, body := range stages {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
			return nil, apperr.InvalidInput(
				"stage_settings.%s — объект настроек этапа, например {\"model_level\": \"high\"}", key)
		}
		patch := map[string]string{}
		for name, value := range fields {
			if !knownStageField(name) {
				return nil, apperr.InvalidInput(
					"stage_settings.%s: поле %q не поддерживается, допустимы %s",
					key, name, strings.Join(stageSettingFields, ", ")).
					WithDetails(map[string]any{"allowed_fields": stageSettingFields})
			}
			var v string
			if err := json.Unmarshal(value, &v); err != nil {
				return nil, apperr.InvalidInput("stage_settings.%s.%s — строка", key, name)
			}
			if err := checkModelLevel(v, "stage_settings."+key+"."+name); err != nil {
				return nil, err
			}
			patch[name] = v
		}
		out[key] = patch
	}
	return out, nil
}

func knownStageField(name string) bool {
	for _, f := range stageSettingFields {
		if f == name {
			return true
		}
	}
	return false
}

// checkStageKeys сверяет ключи с версией цикла задачи: допустимы статусы, на
// которых есть роль-исполнитель. Цикл — тот, по которому идёт задача, а не
// активный: задачи доходят по своей версии (решение 0016).
func (e *Engine) checkStageKeys(ctx context.Context, q postgres.Querier, workflowID uuid.UUID, patch stagePatch) error {
	if len(patch) == 0 {
		return nil
	}
	statuses, err := e.store.ListStatuses(ctx, q, workflowID)
	if err != nil {
		return err
	}
	allowed := []string{}
	ok := map[string]bool{}
	for _, st := range statuses {
		for _, r := range st.Roles {
			if r.IsPrimary {
				allowed = append(allowed, st.Key)
				ok[st.Key] = true
				break
			}
		}
	}
	bad := []string{}
	for key := range patch {
		if !ok[key] {
			bad = append(bad, key)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return apperr.InvalidInput(
		"stage_settings: ключи %s не статусы цикла задачи с ролью-исполнителем, допустимы %s",
		strings.Join(bad, ", "), strings.Join(allowed, ", ")).
		WithDetails(map[string]any{"invalid_keys": bad, "allowed_keys": allowed})
}

// mergeStageSettings сливает запрос с прежними настройками: по ключам статуса
// и по полям этапа. Не названное не меняется, пустое значение снимает поле,
// этап без полей уходит из объекта.
func mergeStageSettings(current json.RawMessage, patch stagePatch) (json.RawMessage, error) {
	merged := map[string]map[string]any{}
	if trimmed := bytes.TrimSpace(current); len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &merged); err != nil {
			return nil, apperr.Internal(err, "stage_settings задачи")
		}
	}
	if merged == nil {
		merged = map[string]map[string]any{}
	}
	for key, fields := range patch {
		stage := merged[key]
		if stage == nil {
			stage = map[string]any{}
		}
		for name, v := range fields {
			if v == "" {
				delete(stage, name)
			} else {
				stage[name] = v
			}
		}
		if len(stage) == 0 {
			delete(merged, key)
		} else {
			merged[key] = stage
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, apperr.Internal(err, "stage_settings задачи")
	}
	return out, nil
}

// stageModelLevel — уровень этапа statusKey в настройках задачи; пусто — не
// задан.
func stageModelLevel(settings json.RawMessage, statusKey string) string {
	var stages map[string]struct {
		ModelLevel string `json:"model_level"`
	}
	if len(settings) == 0 || json.Unmarshal(settings, &stages) != nil {
		return ""
	}
	return stages[statusKey].ModelLevel
}

// resolveExecution разрешает уровень прогона: этап задачи на статусе прогона
// → задача → версия роли прогона → none. Перепроверке особый случай не нужен:
// её прогон стоит на статусе родителя, а версия роли у него — ревьюера.
func resolveExecution(task domain.Task, run domain.StageRun, roleLevel *string) Execution {
	if v := stageModelLevel(task.StageSettings, run.StatusKey); v != "" {
		return Execution{ModelLevel: &v, ModelLevelSource: domain.ModelLevelSourceStage}
	}
	if task.ModelLevel != nil && *task.ModelLevel != "" {
		v := *task.ModelLevel
		return Execution{ModelLevel: &v, ModelLevelSource: domain.ModelLevelSourceTask}
	}
	if roleLevel != nil && *roleLevel != "" {
		v := *roleLevel
		return Execution{ModelLevel: &v, ModelLevelSource: domain.ModelLevelSourceRole}
	}
	return Execution{ModelLevelSource: domain.ModelLevelSourceNone}
}

// runExecution — рекомендация прогона: снимок, если он есть, иначе то, что
// получит следующий захват по текущим уровням.
func runExecution(task domain.Task, run domain.StageRun, roleLevel *string) Execution {
	if run.ModelLevelSource != nil {
		return Execution{ModelLevel: run.ModelLevel, ModelLevelSource: *run.ModelLevelSource}
	}
	return resolveExecution(task, run, roleLevel)
}
