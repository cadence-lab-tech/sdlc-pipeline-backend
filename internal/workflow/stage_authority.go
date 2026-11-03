// Право этапа (решение 0072, п. 1–2, 8–9; CLAB-379).
//
// Участник уровня agent — человек-специалист: на этапе своей роли он
// утверждает работу (human_approval, чек-лист) и двигает задачу вперёд или
// возвратом; чужие этапы, статусы без исполнителя, снятие задачи, пределы и
// waived остаются сопровождающему. Правило одно на переход, решение по
// гейту, предпросмотр (actor_may*) и «Ждут меня»: транспорты его не
// повторяют, а передают актора движку.
//
// StageAuthority — чистая функция над уровнем, набором ролей, ролью статуса
// и действием: таблица проверяется без базы, а предсказание и исход
// запроса не расходятся, потому что их считает она одна. Факты (статус
// задачи, основная роль, откуда переход) движок читает под блокировкой
// задачи. Токена задачи правило не касается: его рамка — executor.go.
package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Основание права: по уровню (maintainer и выше), по роли этапа (agent) или
// рамкой токена задачи. Его же пишет в события лента (CLAB-380).
const (
	ViaLevel     = "level"
	ViaStageRole = "stage_role"
	ViaTask      = "task"
)

// Коды причины отказа в details.reason: по ним клиенты различают отказы, не
// разбирая текст. Контракт CLAB-382 и CLAB-383.
const (
	// Этап адресован роли, которой у актора нет.
	DenyStageRole = "stage_role"
	// У статуса нет исполнителя, переход глобальный или идёт не из текущего
	// статуса задачи.
	DenyNoStageRole = "no_stage_role"
	// Действие на своём этапе, но за сопровождающим.
	DenyMaintainerAction = "maintainer_action"
	// Уровня участия не хватает вовсе (viewer, не участник).
	DenyLevel = "level"
)

// Actor — кто действует, как его видит движок: принципал, действующий
// уровень в проекте задачи, набор ролей специалистов и привязка токена
// задачи. Собирает его Auth.Actor один раз на запрос; уровень и роли — про
// актора, а не про задачу, и блокировки задачи не требуют.
type Actor struct {
	Principal domain.Principal
	Level     domain.MemberRole
	// Действующий набор ролей (Auth.AllowedRoles): nil — любые, [] — ни одной.
	Roles []string
	// Токен задачи: правила 0049, право этапа не применяется.
	Bound *TaskBinding
}

// UncheckedActor — актор без права этапа: уровень администратора, рамка
// токена задачи сохраняется. Для внутренних путей и тестов движка
// (Execute, ExecuteBound, DecideGate, DecideGateSeen); транспорты собирают
// актора Auth.Actor — это держит TestTransportsUseStageAuthority.
func UncheckedActor(id uuid.UUID, bound *TaskBinding) Actor {
	return Actor{Principal: domain.Principal{ID: id}, Level: domain.MemberAdmin, Bound: bound}
}

// StageFacts — факты задачи для права: текущий статус и его основная роль
// в версии цикла задачи ("" — исполнителя у статуса нет).
type StageFacts struct {
	StatusKey string
	StageRole string
}

// StageAction — что актор делает: переход или решение по гейту перехода.
type StageAction struct {
	Transition string
	// Вид перехода: advance, reject, reassign, cancel.
	Kind string
	// Переход без исходного статуса («из любого»).
	Global bool
	// Переход идёт из текущего статуса задачи.
	FromCurrent bool
	// Решение по гейту: ключ, тип и вердикт; Gate == "" — сам переход.
	Gate     string
	GateType string
	Verdict  string
}

// StageVerdict — ответ права: можно ли, на каком основании, а если нельзя —
// код причины и текст. StageRole — основная роль статуса (CLAB-380 пишет её
// в события).
type StageVerdict struct {
	Allowed   bool   `json:"allowed"`
	Via       string `json:"via,omitempty"`
	StageRole string `json:"stage_role,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Err — отказ как ошибка домена: 403 с кодом причины в details. nil —
// разрешено.
func (v StageVerdict) Err() error {
	if v.Allowed {
		return nil
	}
	return apperr.Forbidden("%s", v.Message).WithDetails(map[string]any{
		"reason": v.Reason, "stage_role": v.StageRole,
	})
}

// StageAuthority решает, вправе ли актор уровня level с ролями roles
// выполнить act на задаче с фактами facts. Уровни сравниваются через Covers:
// новый уровень ниже agent (CLAB-302) получит «ничего», а не права agent.
func StageAuthority(level domain.MemberRole, roles []string, facts StageFacts, act StageAction) StageVerdict {
	v := StageVerdict{StageRole: facts.StageRole}
	switch {
	case level.Covers(domain.MemberMaintainer):
		v.Allowed, v.Via = true, ViaLevel
		return v
	case !level.Covers(domain.MemberAgent):
		v.Reason = DenyLevel
		have := string(level)
		if have == "" {
			have = "нет участия"
		}
		v.Message = fmt.Sprintf("%s — нужен уровень %s, а есть %s", act.what(), domain.MemberAgent, have)
		return v
	}

	switch {
	case act.Global:
		v.Reason = DenyNoStageRole
		v.Message = fmt.Sprintf("%s: переход %q из любого статуса — решение сопровождающего", act.what(), act.Transition)
		return v
	case !act.FromCurrent:
		v.Reason = DenyNoStageRole
		v.Message = fmt.Sprintf("%s: переход %q идёт не из текущего статуса %q — решение сопровождающего",
			act.what(), act.Transition, facts.StatusKey)
		return v
	case facts.StageRole == "":
		v.Reason = DenyNoStageRole
		v.Message = fmt.Sprintf("%s: статус %q без исполнителя — решение сопровождающего", act.what(), facts.StatusKey)
		return v
	case !hasRole(roles, facts.StageRole):
		v.Reason = DenyStageRole
		v.Message = fmt.Sprintf("%s: этап %q адресован роли %s, у вас роли [%s]",
			act.what(), facts.StatusKey, facts.StageRole, strings.Join(roles, ", "))
		return v
	case !act.stageWork():
		v.Reason = DenyMaintainerAction
		v.Message = fmt.Sprintf("%s — решение сопровождающего", act.what())
		return v
	}
	v.Allowed, v.Via = true, ViaStageRole
	return v
}

// stageWork — действие из перечня п. 2 решения 0072: утверждение работы
// этапа (human_approval passed/failed, чек-лист passed) и переход вперёд
// или возвратом. Пределы, waived, провал чек-листа, прочие гейты, снятие и
// переназначение задачи — нет.
func (a StageAction) stageWork() bool {
	if a.Gate == "" {
		return a.Kind == "advance" || a.Kind == "reject"
	}
	switch a.GateType {
	case domain.GateHumanApproval:
		return a.Verdict == domain.GateResultPassed || a.Verdict == domain.GateResultFailed
	case domain.GateChecklist:
		return a.Verdict == domain.GateResultPassed
	}
	return false
}

func (a StageAction) what() string {
	if a.Gate == "" {
		return fmt.Sprintf("переход %q вида %s", a.Transition, a.Kind)
	}
	return fmt.Sprintf("решение %s по гейту %q (%s) перехода %q", a.Verdict, a.Gate, a.GateType, a.Transition)
}

// hasRole: nil — любые роли, [] — ни одной.
func hasRole(roles []string, key string) bool {
	if roles == nil {
		return true
	}
	for _, r := range roles {
		if r == key {
			return true
		}
	}
	return false
}

// stageRole — ключ основной роли статуса в версии цикла; "" — исполнителя нет.
func (g cycleGraph) stageRole(statusID uuid.UUID) string {
	for _, r := range g.byID[statusID].Roles {
		if r.IsPrimary {
			return r.RoleKey
		}
	}
	return ""
}

// stageRoleOf — основная роль статуса одним запросом, когда граф цикла не
// нужен (решение по гейту).
func (e *Engine) stageRoleOf(ctx context.Context, q postgres.Querier, statusID uuid.UUID) (string, error) {
	roles, err := e.store.StatusRoles(ctx, q, statusID)
	if err != nil {
		return "", err
	}
	for _, r := range roles {
		if r.IsPrimary {
			return r.RoleKey, nil
		}
	}
	return "", nil
}

// transitionAction — действие перехода t для задачи task.
func transitionAction(task domain.Task, t domain.Transition) StageAction {
	return StageAction{
		Transition: t.Key, Kind: t.Kind, Global: t.FromStatusID == nil,
		FromCurrent: t.FromStatusID != nil && *t.FromStatusID == task.StatusID,
	}
}

// mayTransition — право актора на переход: токен задачи — его рамка (0049),
// остальные — право этапа. Одна функция для ExecuteAs, предпроверки MCP и
// actor_may предпросмотра.
func mayTransition(actor Actor, task domain.Task, t domain.Transition, stageRole string) (StageVerdict, error) {
	if actor.Bound != nil {
		if err := executorTransition(actor.Bound, task, t); err != nil {
			return StageVerdict{StageRole: stageRole}, err
		}
		return StageVerdict{Allowed: true, Via: ViaTask, StageRole: stageRole}, nil
	}
	v := StageAuthority(actor.Level, actor.Roles, StageFacts{StatusKey: task.StatusKey, StageRole: stageRole},
		transitionAction(task, t))
	return v, v.Err()
}

// mayDecide — может ли актор вынести вердикт status по гейту gate перехода t
// на прогоне run. who — принципал решающего (вид важен для гейтов, которые
// решает только человек). Порядок проверок прежний для токена задачи и
// сопровождающего; право этапа встаёт между рамкой токена задачи и
// проверкой вида принципала. Одна функция для DecideGateAs, actor_may_decide
// предпросмотра и can_decide «Ждут меня».
func mayDecide(actor Actor, who domain.Principal, task domain.Task, run domain.StageRun, t domain.Transition,
	gate domain.Gate, stageRole, status string) (StageVerdict, error) {
	rule, onlyHuman := humanOnly(gate)
	v := StageVerdict{StageRole: stageRole}

	// Токен задачи апрув не ставит, даже если аренду держит человек
	// (/clab-run): проверка по виду токена, а не только принципала.
	if actor.Bound != nil && onlyHuman {
		return v, executorRefused(actor.Bound, fmt.Sprintf("гейт %q закрывает человек", gate.Key))
	}
	if actor.Bound != nil {
		if err := executorGate(actor.Bound, task, run, t, gate, status); err != nil {
			return v, err
		}
		v.Allowed, v.Via = true, ViaTask
	} else {
		act := transitionAction(task, t)
		act.Gate, act.GateType, act.Verdict = gate.Key, gate.Type, status
		v = StageAuthority(actor.Level, actor.Roles, StageFacts{StatusKey: task.StatusKey, StageRole: stageRole}, act)
		if err := v.Err(); err != nil {
			return v, err
		}
	}

	// Апрув человека ставит человек. Агент, представившийся человеком,
	// в режиме токенов не пройдёт аутентификацию; в режиме заголовка
	// это защита от случайного самоапрува агентом, а не от злого умысла.
	if onlyHuman {
		if who.Kind != domain.PrincipalUser {
			v.Allowed = false
			return v, apperr.Forbidden("гейт %q закрывает человек, а не %s %q",
				gate.Key, who.Kind, who.Handle)
		}
		if rule.waiveOnly && status != domain.GateResultWaived {
			v.Allowed = false
			return v, apperr.InvalidInput(
				"по гейту %q вердикт выносит движок; человек может только снять его: "+
					"status waived, а не %q", gate.Key, status)
		}
	}
	if err := enteredVia(task, run, t, gate); err != nil {
		v.Allowed = false
		return v, err
	}
	return v, nil
}
