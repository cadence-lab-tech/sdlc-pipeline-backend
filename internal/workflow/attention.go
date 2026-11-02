package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Причины внимания (CLAB-363, решение 0070): почему задача требует взгляда
// человека. Правило одно для строки списка задач, интерфейса «Обзор»,
// плагина и уведомлений (CLAB-190): клиент не выводит свою версию. Каждая
// причина — факт движка (гейт, прогон, аренда, связь), а не порог «на глаз»
// и не ключ статуса, роли или типа задачи.

// Коды причин. Набор расширяемый: новая причина — запись в attentionRules
// и строка в таблице решения 0070.
const (
	AttentionAwaitsHuman    = "awaits_human"
	AttentionGateRejected   = "gate_rejected"
	AttentionLeaseExpired   = "lease_expired"
	AttentionOverStageLimit = "over_stage_limit"
	AttentionBlocked        = "blocked"
	AttentionReturned       = "returned"
	AttentionUnclaimed      = "unclaimed"
)

// Attention — одна причина: код, с какого момента она действует (nil — без
// момента) и поля причины по таблице решения. JSON плоский: {code, since,
// …поля}. Поля — карта, а не структура: новая причина приносит свои поля, не
// меняя тип, а клиент без знания кода читает code и since.
type Attention struct {
	Code   string
	Since  *time.Time
	Fields map[string]any
}

func (a Attention) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(a.Fields)+2)
	for k, v := range a.Fields {
		m[k] = v
	}
	m["code"] = a.Code
	m["since"] = a.Since
	return json.Marshal(m)
}

// AttentionGate — гейт причины: ключи перехода и гейта. Подписи клиент берёт
// из версии цикла задачи.
type AttentionGate struct {
	Transition string `json:"transition"`
	Gate       string `json:"gate"`
}

// RejectedGate — отклонённый гейт: ключи и причина отклонения из
// details.reason решения (null, если её не написали).
type RejectedGate struct {
	Transition string    `json:"transition"`
	Gate       string    `json:"gate"`
	Reason     *string   `json:"reason"`
	DecidedAt  time.Time `json:"-"`
}

// AttentionFacts — всё, что правилу нужно знать о нетерминальной задаче.
// Собирает их движок пакетом на страницу (TaskOverviews); правило — чистая
// функция, её тест базы не поднимает.
type AttentionFacts struct {
	Now time.Time
	// Открытая попытка этапа на текущем статусе; nil — её нет.
	Stage *domain.StageRun
	// Прогон, в котором сейчас можно работать: открытая перепроверка, иначе
	// Stage.
	Current *domain.StageRun
	// Человеческие гейты переходов вперёд, которые ждут решения (то же, что
	// у /approvals, без учёта прав смотрящего).
	HumanGates []AttentionGate
	// Гейты попытки этапа, отклонённые решающим.
	Rejected []RejectedGate
	// Последнее событие аренды Current, если он ждёт исполнителя.
	LeaseEvent *postgres.LeaseEvent
	// Предел этапа Stage; nil — предела нет.
	Limit *StageLimit
	// Незакрытые блокирующие.
	BlockedBy []string
}

// attentionRule — запись реестра: код и функция по фактам. Место в срезе —
// порядок важности: первая причина в ответе — главная.
type attentionRule struct {
	Code   string
	Detect func(AttentionFacts) (Attention, bool)
}

// attentionRules — реестр причин в порядке важности. Добавить причину —
// одна запись здесь (код, место, функция, тест) и строка в решении 0070;
// обработчик и параметры списка коды берут отсюда (AttentionCodes).
var attentionRules = []attentionRule{
	{AttentionAwaitsHuman, detectAwaitsHuman},
	{AttentionGateRejected, detectGateRejected},
	{AttentionLeaseExpired, detectLeaseExpired},
	{AttentionOverStageLimit, detectOverStageLimit},
	{AttentionBlocked, detectBlocked},
	{AttentionReturned, detectReturned},
	{AttentionUnclaimed, detectUnclaimed},
}

// AttentionCodes — известные коды в порядке важности.
func AttentionCodes() []string {
	out := make([]string, len(attentionRules))
	for i, r := range attentionRules {
		out[i] = r.Code
	}
	return out
}

// EvalAttention — причины задачи по фактам, по одной на код, в порядке
// важности. Пусто — [] (не nil): в ответе массив, а не null.
func EvalAttention(f AttentionFacts) []Attention {
	out := []Attention{}
	for _, r := range attentionRules {
		if a, ok := r.Detect(f); ok {
			a.Code = r.Code
			out = append(out, a)
		}
	}
	return out
}

// Человеческий гейт перехода вперёд ждёт решения, остальные блокирующие
// прошли; с момента входа в прогон этапа.
func detectAwaitsHuman(f AttentionFacts) (Attention, bool) {
	if len(f.HumanGates) == 0 {
		return Attention{}, false
	}
	var since *time.Time
	if f.Stage != nil {
		since = &f.Stage.EnteredAt
	}
	return Attention{Since: since, Fields: map[string]any{"gates": f.HumanGates}}, true
}

// Гейт попытки этапа отклонён решающим (человек отклонил апрув или
// чек-лист, перепроверка вынесла fail) и ждёт исполнителя; с первого
// отклонения.
func detectGateRejected(f AttentionFacts) (Attention, bool) {
	if len(f.Rejected) == 0 {
		return Attention{}, false
	}
	since := f.Rejected[0].DecidedAt
	for _, g := range f.Rejected[1:] {
		if g.DecidedAt.Before(since) {
			since = g.DecidedAt
		}
	}
	return Attention{Since: &since, Fields: map[string]any{"gates": f.Rejected}}, true
}

// Аренда текущего прогона протухла: прогон ещё числится забранным, а срок
// вышел (воркер не прошёл), либо воркер уже вернул его в pending и последнее
// событие аренды — lease.expired, а не новый захват или явный отпуск.
func detectLeaseExpired(f AttentionFacts) (Attention, bool) {
	r := f.Current
	if r == nil {
		return Attention{}, false
	}
	if (r.State == domain.StageClaimed || r.State == domain.StageInProgress) &&
		r.LeaseExpires != nil && !r.LeaseExpires.After(f.Now) {
		at := *r.LeaseExpires
		return Attention{Since: &at, Fields: map[string]any{"holder": r.ClaimedHandle, "run_kind": r.Kind}}, true
	}
	if r.State == domain.StagePending && f.LeaseEvent != nil && f.LeaseEvent.Type == domain.EventLeaseExpired {
		at := f.LeaseEvent.At
		return Attention{Since: &at, Fields: map[string]any{"holder": f.LeaseEvent.Holder, "run_kind": r.Kind}}, true
	}
	return Attention{}, false
}

// Срок этапа вышел, а человек предел не снял; с момента срока. Считается
// по часам, отметки воркера не ждёт.
func detectOverStageLimit(f AttentionFacts) (Attention, bool) {
	if !f.Limit.Holds() {
		return Attention{}, false
	}
	at := f.Limit.Deadline
	return Attention{Since: &at, Fields: map[string]any{"max_duration": f.Limit.MaxDuration, "gate": f.Limit.Gate}}, true
}

// Есть незакрытые блокирующие; ключи — в blocked_by строки.
func detectBlocked(f AttentionFacts) (Attention, bool) {
	return Attention{}, len(f.BlockedBy) > 0
}

// Открытая попытка этапа — не первая: задача вернулась сюда возвратом или
// вошла в статус повторно.
func detectReturned(f AttentionFacts) (Attention, bool) {
	r := f.Stage
	if r == nil || (r.Attempt <= 1 && r.RejectedFromRunID == nil) {
		return Attention{}, false
	}
	at := r.EnteredAt
	return Attention{Since: &at, Fields: map[string]any{"attempt": r.Attempt}}, true
}

// У текущего прогона есть роль, а живой аренды нет: этап ждёт исполнителя.
// От смотрящего не зависит — может ли забрать он, говорит current_run.claimable.
func detectUnclaimed(f AttentionFacts) (Attention, bool) {
	r := f.Current
	if r == nil || r.RoleID == nil || r.LeaseHeld(f.Now) {
		return Attention{}, false
	}
	at := r.EnteredAt
	return Attention{Since: &at}, true
}

// AttentionFilter — фильтр списка задач по причинам. Поля без set не
// действуют. Все заданные условия складываются по «и».
type AttentionFilter struct {
	has, hasSet bool
	anyOf       []string
	noneOf      []string
	except      []string
	exceptSet   bool
	within      []string
	withinSet   bool
}

// Active — задан ли хоть один параметр.
func (f AttentionFilter) Active() bool {
	return f.hasSet || len(f.anyOf) > 0 || len(f.noneOf) > 0 || f.exceptSet || f.withinSet
}

// Match — подходит ли задача с кодами codes.
func (f AttentionFilter) Match(codes []string) bool {
	if f.hasSet && (len(codes) > 0) != f.has {
		return false
	}
	if len(f.anyOf) > 0 && !slices.ContainsFunc(codes, func(c string) bool { return slices.Contains(f.anyOf, c) }) {
		return false
	}
	if slices.ContainsFunc(codes, func(c string) bool { return slices.Contains(f.noneOf, c) }) {
		return false
	}
	// Код, которого нет в списке, — причина, в том числе будущий код,
	// незнакомый клиенту: задача с ним не теряется.
	if f.exceptSet && !slices.ContainsFunc(codes, func(c string) bool { return !slices.Contains(f.except, c) }) {
		return false
	}
	if f.withinSet && slices.ContainsFunc(codes, func(c string) bool { return !slices.Contains(f.within, c) }) {
		return false
	}
	return true
}

// Codes — коды причин списком.
func Codes(as []Attention) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Code
	}
	return out
}

// ParseAttentionFilter разбирает параметры attention, attention_not,
// attention_except и attention_within. get — значение параметра запроса
// (пусто — не задан). Незнакомый код — ошибка с перечнем известных: клиент
// узнаёт об опечатке, а не получает молча пустую выборку.
func ParseAttentionFilter(get func(string) string) (AttentionFilter, error) {
	var f AttentionFilter
	known := AttentionCodes()
	codes := func(name, raw string) ([]string, error) {
		var out []string
		for _, c := range strings.Split(raw, ",") {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if !slices.Contains(known, c) {
				return nil, fmt.Errorf("%s: неизвестный код причины %q, известны: %s", name, c, strings.Join(known, ", "))
			}
			out = append(out, c)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s: нужен хотя бы один код причины, известны: %s", name, strings.Join(known, ", "))
		}
		return out, nil
	}
	var err error
	if raw := strings.TrimSpace(get("attention")); raw != "" {
		if raw == "true" || raw == "false" {
			f.has, f.hasSet = raw == "true", true
		} else if f.anyOf, err = codes("attention", raw); err != nil {
			return f, fmt.Errorf("%w (или true, false)", err)
		}
	}
	if raw := strings.TrimSpace(get("attention_not")); raw != "" {
		if f.noneOf, err = codes("attention_not", raw); err != nil {
			return f, err
		}
	}
	if raw := strings.TrimSpace(get("attention_except")); raw != "" {
		if f.except, err = codes("attention_except", raw); err != nil {
			return f, err
		}
		f.exceptSet = true
	}
	if raw := strings.TrimSpace(get("attention_within")); raw != "" {
		if f.within, err = codes("attention_within", raw); err != nil {
			return f, err
		}
		f.withinSet = true
	}
	return f, nil
}
