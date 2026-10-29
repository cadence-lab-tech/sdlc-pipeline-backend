// Выбор необязательных этапов на задаче и проход мимо выключенных (решение
// 0066, п. 7-15): разбор и сверка выбора, разрешение полного набора и расчёт
// того, где встанет задача по переходу.
//
// Движок здесь не знает ни одного ключа статуса: необязательность — отметка
// статуса версии цикла (is_optional), обход — единственный исходящий advance
// из самого статуса, выбор — данные задачи. Расчёт назначения — одна функция
// (planLanding) на Preview, gatesAhead и ExecuteBound: предпросмотр и
// переход не расходятся.

package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// UnavailableStageDisabled — почему переход недоступен: он ведёт возвратом
// или переназначением в этап, выключенный у задачи (п. 13, 15). Перечислимое
// значение поля unavailable; других пока нет.
const UnavailableStageDisabled = "stage_disabled"

// optionalPatch — выбор из запроса: ключ статуса → включён ли.
type optionalPatch map[string]bool

// parseOptionalStages разбирает optional_stages запроса: объект, значения —
// только bool. Ключи здесь не сверяются: для этого нужна версия цикла
// задачи. null и пусто — пустой выбор.
func parseOptionalStages(raw json.RawMessage) (optionalPatch, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return optionalPatch{}, nil
	}
	var stages map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &stages); err != nil || stages == nil {
		return nil, apperr.InvalidInput(
			`optional_stages — объект «ключ необязательного этапа → true | false», например {"design": false}`)
	}
	out := optionalPatch{}
	for key, body := range stages {
		var on bool
		if err := json.Unmarshal(body, &on); err != nil || string(bytes.TrimSpace(body)) == "null" {
			return nil, apperr.InvalidInput("optional_stages.%s — true или false", key)
		}
		out[key] = on
	}
	return out, nil
}

// optionalOf — необязательные статусы версии по position.
func optionalOf(statuses []domain.Status) []domain.Status {
	var out []domain.Status
	for _, st := range byPosition(statuses) {
		if st.IsOptional {
			out = append(out, st)
		}
	}
	return out
}

// checkOptionalKeys сверяет ключи выбора с необязательными статусами версии
// цикла задачи (п. 9): чужой, обязательный или несуществующий ключ — отказ с
// перечнем допустимых, а не молчание: опечатка не должна включать этап.
func checkOptionalKeys(optional []domain.Status, patch optionalPatch) error {
	if len(patch) == 0 {
		return nil
	}
	if len(optional) == 0 {
		return apperr.InvalidInput("optional_stages: у цикла задачи нет необязательных этапов").
			WithDetails(map[string]any{"allowed": []string{}})
	}
	allowed := make([]string, 0, len(optional))
	ok := map[string]bool{}
	for _, st := range optional {
		allowed = append(allowed, st.Key)
		ok[st.Key] = true
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
		"optional_stages: %s — не необязательные этапы цикла задачи, допустимы %s",
		strings.Join(bad, ", "), strings.Join(allowed, ", ")).
		WithDetails(map[string]any{"invalid_keys": bad, "allowed": allowed})
}

// storedChoice читает выбор задачи. Значение не bool (данные испорчены
// вручную) в набор не попадает — для него действует умолчание статуса.
func storedChoice(raw json.RawMessage) map[string]bool {
	out := map[string]bool{}
	var stages map[string]any
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &stages) != nil {
		return out
	}
	for k, v := range stages {
		if on, ok := v.(bool); ok {
			out[k] = on
		}
	}
	return out
}

// resolveChoice — полный набор по необязательным статусам версии: значение
// из запроса, иначе прежнее, иначе умолчание статуса. Ключи вне набора
// отпадают.
func resolveChoice(optional []domain.Status, current map[string]bool, patch optionalPatch) map[string]bool {
	out := make(map[string]bool, len(optional))
	for _, st := range optional {
		switch v, ok := patch[st.Key]; {
		case ok:
			out[st.Key] = v
		default:
			if prev, had := current[st.Key]; had {
				out[st.Key] = prev
			} else {
				out[st.Key] = st.EnabledByDefault
			}
		}
	}
	return out
}

func sameChoice(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func choiceJSON(choice map[string]bool) (json.RawMessage, error) {
	if choice == nil {
		choice = map[string]bool{}
	}
	out, err := json.Marshal(choice)
	if err != nil {
		return nil, apperr.Internal(err, "optional_stages задачи")
	}
	return out, nil
}

// stageEnabled — включён ли статус у задачи: обязательный всегда, ключа нет
// в наборе — умолчание статуса (п. 7).
func stageEnabled(st domain.Status, choice map[string]bool) bool {
	if !st.IsOptional {
		return true
	}
	if on, ok := choice[st.Key]; ok {
		return on
	}
	return st.EnabledByDefault
}

// cycleGraph — статусы и переходы версии цикла задачи: всё, что нужно
// расчёту назначения. Читается одним походом на предпросмотр или переход.
type cycleGraph struct {
	statuses    []domain.Status
	transitions []domain.Transition
	byID        map[uuid.UUID]domain.Status
}

func (e *Engine) cycleGraph(ctx context.Context, q postgres.Querier, workflowID uuid.UUID) (cycleGraph, error) {
	statuses, err := e.store.ListStatuses(ctx, q, workflowID)
	if err != nil {
		return cycleGraph{}, err
	}
	transitions, err := e.store.ListTransitions(ctx, q, workflowID)
	if err != nil {
		return cycleGraph{}, err
	}
	g := cycleGraph{statuses: statuses, transitions: transitions, byID: map[uuid.UUID]domain.Status{}}
	for _, st := range statuses {
		g.byID[st.ID] = st
	}
	return g, nil
}

// available — переходы из статуса, в том числе «из любого».
func (g cycleGraph) available(statusID uuid.UUID) []domain.Transition {
	out := []domain.Transition{}
	for _, t := range g.transitions {
		if t.FromStatusID == nil || *t.FromStatusID == statusID {
			out = append(out, t)
		}
	}
	return out
}

// bypassOf — обходной переход статуса: единственный исходящий advance из
// него самого (п. 4). Переходы «из любого статуса» обходными не считаются.
func (g cycleGraph) bypassOf(st domain.Status) (domain.Transition, error) {
	var found []domain.Transition
	for _, t := range g.transitions {
		if t.Kind == "advance" && t.FromStatusID != nil && *t.FromStatusID == st.ID {
			found = append(found, t)
		}
	}
	if len(found) != 1 {
		// Активированная версия так не бывает (валидатор держит
		// optional_no_bypass и optional_bypass_ambiguous): это испорченные
		// данные, и гадать, куда идти, движок не должен.
		return domain.Transition{}, apperr.Internal(
			fmt.Errorf("у статуса %q исходящих advance: %d", st.Key, len(found)),
			"обход выключенного этапа %q не определён", st.Key)
	}
	return found[0], nil
}

// landing — где встанет задача по переходу при её выборе этапов.
type landing struct {
	// Статус, где встанет задача; у недоступного перехода не задан.
	To domain.Status
	// Переход, которым задача войдёт в To: запрошенный или последний
	// обходной цепочки. По нему считается stage_time_limit (0061, п. 9).
	Via domain.Transition
	// Пропущенные статусы и их обходные переходы по порядку цепочки.
	Skipped []domain.Status
	Bypass  []domain.Transition
	// Причина недоступности (UnavailableStageDisabled) и выключенный этап.
	Unavailable string
	Disabled    string
}

func (l landing) skipKeys() []string {
	out := make([]string, 0, len(l.Skipped))
	for _, st := range l.Skipped {
		out = append(out, st.Key)
	}
	return out
}

// landsIn — lands_in представления: у недоступного перехода null.
func (l landing) landsIn() *string {
	if l.Unavailable != "" {
		return nil
	}
	key := l.To.Key
	return &key
}

// planLanding считает назначение перехода по выбору задачи (п. 12-13).
//
// Включённый или обязательный целевой статус — задача встаёт туда, как
// всегда. Выключенный: переход вида advance идёт по цепочке обходов до
// первого включённого или обязательного статуса, иной вид недоступен.
// Цепочка не длиннее числа статусов версии: кольцо в испорченных данных —
// внутренняя ошибка, а не бесконечный цикл.
func planLanding(g cycleGraph, choice map[string]bool, t domain.Transition) (landing, error) {
	target, ok := g.byID[t.ToStatusID]
	if !ok {
		return landing{}, apperr.Internal(fmt.Errorf("статус %s", t.ToStatusID),
			"переход %q ведёт в статус вне версии цикла", t.Key)
	}
	out := landing{To: target, Via: t, Skipped: []domain.Status{}, Bypass: []domain.Transition{}}
	if stageEnabled(target, choice) {
		return out, nil
	}
	if t.Kind != "advance" {
		out.Unavailable, out.Disabled = UnavailableStageDisabled, target.Key
		return out, nil
	}
	cur := target
	for !stageEnabled(cur, choice) {
		if len(out.Skipped) >= len(g.statuses) {
			return landing{}, apperr.Internal(fmt.Errorf("цепочка от %q длиннее версии", target.Key),
				"цепочка обходов от этапа %q замкнулась", target.Key)
		}
		bypass, err := g.bypassOf(cur)
		if err != nil {
			return landing{}, err
		}
		next, ok := g.byID[bypass.ToStatusID]
		if !ok {
			return landing{}, apperr.Internal(fmt.Errorf("статус %s", bypass.ToStatusID),
				"обходной переход %q ведёт в статус вне версии цикла", bypass.Key)
		}
		out.Skipped = append(out.Skipped, cur)
		out.Bypass = append(out.Bypass, bypass)
		out.Via = bypass
		cur = next
	}
	out.To = cur
	return out, nil
}

// stageDisabled — отказ перехода в выключенный этап (п. 13).
func stageDisabled(l landing) error {
	return apperr.New(apperr.CodeTransitionNotAllowed,
		"этап %s выключен у задачи: вернуть задачу туда — включить этап (optional_stages) и запросить переход снова",
		l.Disabled).WithDetails(map[string]any{
		"status": l.Disabled, "unavailable": l.Unavailable,
	})
}

// ownStageChoice — выбор этапов своей задачи токеном задачи (п. 11): иначе
// исполнитель мог бы выключить следующий этап проверки своей же работы.
// Проверяется раньше разбора: у токена на своей задаче — 403, а не 400.
func ownStageChoice(b *TaskBinding, task domain.Task) error {
	if b == nil || task.ID != b.TaskID {
		return nil
	}
	return executorRefused(b, "выбор этапов своей задачи — решение человека")
}
