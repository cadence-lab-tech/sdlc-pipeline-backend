// Право на работу с задачей: постановка, заведение, назначение, связи и
// область (CLAB-411, эпик CLAB-404).
//
// Класс актора считает одна функция (Actor.Class) по уже собранному
// Auth.Actor: привязке токена задачи, уровню участия и виду принципала.
// Право по классу — одна чистая функция TaskRight: её зовут правила движка
// (UpdateTask, CreateTask, UpdateScope, Assign, LinkTasks) и прогноз
// actor_rights карточки, поэтому прогноз и исход запроса не расходятся.
// Транспорты уровни и виды не сравнивают — передают актора.
//
// Новых видов токена нет: сотрудник — человек (principals.kind = user)
// уровня agent, бот — агент-принципал того же уровня с api-токеном; бот
// работает, как до CLAB-411 (рамка аренды), а сотрудник — с любой задачей
// проекта, как в трекере.
package workflow

import (
	"context"
	"fmt"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// ActorClass — класс актора для права на работу с задачей.
type ActorClass string

const (
	// Уровень не задан: внутренние пути движка и его тесты. Правил нет, как
	// до CLAB-411 (в режиме header уровень — admin, это ClassMaintainer).
	ClassUnchecked ActorClass = "unchecked"
	// Токен задачи (решение 0049): рамка по привязке, при любом уровне
	// держателя аренды.
	ClassTaskToken ActorClass = "task_token"
	// Сопровождающий и выше, любой вид принципала: без ограничений.
	ClassMaintainer ActorClass = "maintainer"
	// Человек уровня agent (личный токен или сессия): любая задача проекта.
	ClassEmployee ActorClass = "employee"
	// Агент-принципал уровня agent и актор без вида: рамка аренды.
	ClassBot ActorClass = "bot"
	// Ниже agent (viewer, будущий уровень CLAB-302): только чтение.
	ClassOutsider ActorClass = "outsider"
)

// Class — класс актора. Порядок проверок: токен задачи раньше уровня,
// уровень сопровождающего раньше вида принципала. Вид, отличный от user
// (agent, system, не задан), — бот: ограничения прежние.
func (a Actor) Class() ActorClass {
	switch {
	case a.Bound != nil:
		return ClassTaskToken
	case a.Level == "":
		return ClassUnchecked
	case a.Level.Covers(domain.MemberMaintainer):
		return ClassMaintainer
	case !a.Level.Covers(domain.MemberAgent):
		return ClassOutsider
	case a.Principal.Kind == domain.PrincipalUser:
		return ClassEmployee
	}
	return ClassBot
}

// TaskAct — действие с задачей.
type TaskAct string

const (
	// Правка постановки (UpdateTask без выбора этапов).
	ActEdit TaskAct = "edit"
	// Выбор необязательных этапов (UpdateTask с optional_stages).
	ActStageChoice TaskAct = "stage_choice"
	// Заведение задачи без parent; в карточке его нет — оно не про задачу.
	ActCreate TaskAct = "create"
	// Заведение ребёнка этой задачи.
	ActCreateChild TaskAct = "create_child"
	ActAssign      TaskAct = "assign"
	// Связь от этой задачи; Other — второй конец, nil — прогноз карточки.
	ActLink TaskAct = "link"
	// Расширение и сужение области по репозиториям.
	ActScope TaskAct = "scope"
	// Свойства без репозиториев (UpdateScope только с properties).
	ActProperties TaskAct = "properties"
)

// Коды причины отказа права на задачу в details.reason, рядом с кодами права
// этапа (stage_authority.go).
const (
	// Бот не держит аренду этапа задачи (или её родителя).
	DenyLease = "lease"
	// Действие человека — сотрудника или сопровождающего, не агента.
	DenyHumanAction = "human_action"
)

// TaskFacts — факты задачи для права. Аренды нужны только боту: движок
// читает их, когда класс актора — ClassBot (taskFacts).
type TaskFacts struct {
	Task domain.Task
	// Актор держит живую аренду текущего этапа Task.
	Held bool
	// Актор держит живую аренду текущего этапа родителя Task.
	ParentHeld bool
	// ActLink: второй конец связи и отношение; nil — прогноз карточки.
	Other *domain.Task
	Rel   string
	// ActScope: сужение, а не расширение, — только для текста отказа.
	Narrow bool
}

// TaskRight — вправе ли актор выполнить act над задачей: nil — да, иначе
// ошибка домена того же кода и текста, что вернул бы запрос.
func TaskRight(a Actor, act TaskAct, f TaskFacts) error {
	var err error
	switch a.Class() {
	case ClassTaskToken:
		err = taskTokenRight(a.Bound, act, f)
	case ClassOutsider:
		have := string(a.Level)
		return apperr.Forbidden("нужен уровень %s, а есть %s", domain.MemberAgent, have).
			WithDetails(map[string]any{"reason": DenyLevel})
	case ClassBot:
		err = botRight(act, f)
	}
	if err != nil {
		return err
	}
	// Выбор этапов закрытой задачи не меняется ни у кого (решение 0066,
	// п. 9): тот же отказ, что у PATCH, — после права на правку.
	if act == ActStageChoice && f.Task.ClosedAt != nil {
		return closedStageChoice(f.Task)
	}
	return nil
}

// taskTokenRight — рамка токена задачи (решение 0049): круг записи своей
// задачи и её детей, заведение — только детей своей задачи, область и
// выбор этапов своей задачи — решение человека. Тексты те же, что у
// RequireWriteCircle, RequireOwnTaskKey, RequireLink и ownStageChoice.
func taskTokenRight(b *TaskBinding, act TaskAct, f TaskFacts) error {
	switch act {
	case ActEdit:
		return writeCircle(b, f.Task, "правка постановки")
	case ActStageChoice:
		if err := ownStageChoice(b, f.Task); err != nil {
			return err
		}
		return writeCircle(b, f.Task, "правка постановки")
	case ActAssign:
		return writeCircle(b, f.Task, "назначение")
	case ActCreate:
		return notOwn(b, "задача не указана — заводить можно только дочерние к своей (parent)")
	case ActCreateChild:
		if f.Task.ID != b.TaskID {
			return notOwn(b, fmt.Sprintf("задача %s чужая", f.Task.Key))
		}
		return nil
	case ActLink:
		if f.Other == nil {
			if inWriteCircle(b, f.Task) {
				return nil
			}
			return executorRefused(b, fmt.Sprintf(
				"связь от задачи %s: задача вне круга своей задачи %s и её детей", f.Task.Key, b.TaskKey))
		}
		return linkCircle(b, f.Task, *f.Other, f.Rel)
	case ActScope, ActProperties:
		return executorRefused(b, "область задачи и её свойства — решение человека")
	}
	return executorRefused(b, string(act))
}

// botRight — бот и актор без вида: рамка аренды, как до CLAB-411.
// Назначение, связи и свойства — без ограничений (решение человека по OQ-1
// CLAB-411: поведение бота не меняется).
func botRight(act TaskAct, f TaskFacts) error {
	switch act {
	case ActEdit, ActStageChoice:
		if f.Held || f.ParentHeld {
			return nil
		}
		return apperr.Forbidden(
			"задачу %s держите не вы: править постановку может тот, кто забрал её этап "+
				"или этап её родительской задачи", f.Task.Key).
			WithDetails(map[string]any{"reason": DenyLease})
	case ActCreate:
		return apperr.Forbidden(
			"агент заводит только дочерние задачи той, которую держит: укажите parent").
			WithDetails(map[string]any{"reason": DenyLease})
	case ActCreateChild:
		if f.Held {
			return nil
		}
		return apperr.Forbidden(
			"задачу %s держите не вы: нарезать её на дочерние может только тот, кто её забрал",
			f.Task.Key).WithDetails(map[string]any{"reason": DenyLease})
	case ActScope:
		if f.Narrow {
			return apperr.Forbidden(
				"сузить область задачи может человек — сопровождающий или сотрудник проекта уровня agent: " +
					"убрать репозиторий из области — решение человека, не агента").
				WithDetails(map[string]any{"reason": DenyHumanAction})
		}
		return apperr.Forbidden(
			"расширить область задачи может человек — сопровождающий или сотрудник проекта уровня agent: " +
				"добавить репозиторий в область — решение человека, не агента; " +
				"опишите нужный репозиторий и причину в артефакте этапа и в отчёте").
			WithDetails(map[string]any{"reason": DenyHumanAction})
	}
	return nil
}

// closedStageChoice — отказ выбора этапов закрытой задачи (решение 0066,
// п. 9): один текст у PATCH и у прогноза карточки.
func closedStageChoice(task domain.Task) error {
	return apperr.Conflict("задача %s закрыта: выбор этапов у неё не меняется", task.Key)
}

// writeCircle — запись токеном задачи только в свою задачу и её детей.
func writeCircle(b *TaskBinding, task domain.Task, action string) error {
	if inWriteCircle(b, task) {
		return nil
	}
	return executorRefused(b, fmt.Sprintf(
		"%s задачи %s: токен задачи %s пишет только в свою задачу и её детей", action, task.Key, b.TaskKey))
}

// linkCircle — связь токеном задачи по паре задач (решение человека
// 2026-10-21, RequireLink): обе в круге записи — любая связь; одна —
// relates_to, shares_surface, duplicates и «своя blocks чужую»; «чужая
// blocks свою» и связь двух задач вне круга — человеку. Чтение второго
// конца RequireLink проверяет отдельно; в движке оно следует из проверки
// «один проект».
func linkCircle(b *TaskBinding, from, to domain.Task, rel string) error {
	fromIn, toIn := inWriteCircle(b, from), inWriteCircle(b, to)
	link := fmt.Sprintf("связь %s %s %s", from.Key, rel, to.Key)
	switch {
	case fromIn && toIn:
		return nil
	case !fromIn && !toIn:
		return executorRefused(b, link+": обе задачи вне круга своей задачи "+b.TaskKey+" и её детей")
	case rel == domain.TaskLinkBlocks && !fromIn:
		return executorRefused(b, link+": чужая задача блокирует свою — порядок работ решает человек")
	case rel == domain.TaskLinkBlocks, rel == domain.TaskLinkRelatesTo,
		rel == domain.TaskLinkSharesSurface, rel == domain.TaskLinkDuplicates:
		return nil
	}
	return executorRefused(b, link+": с задачей вне круга своей задачи и её детей так связывать нельзя")
}

// childInScope — репозитории ребёнка в области родителя: рамка заведения
// бота и токена задачи (решение 0024, R-4 CLAB-411). Сотрудник и
// сопровождающий заводят ребёнка и вне области родителя.
func childInScope(a Actor, parent domain.Task, repos []string) error {
	switch a.Class() {
	case ClassBot, ClassTaskToken:
	default:
		return nil
	}
	allowed := map[string]bool{}
	for _, r := range parent.Repositories {
		allowed[r.Owner+"/"+r.Name] = true
		allowed[r.Name] = true
		if r.RoleHint != nil {
			allowed[*r.RoleHint] = true
		}
	}
	for _, ref := range repos {
		if !allowed[ref] {
			return apperr.New(apperr.CodeScopeExpansionForbidden,
				"репозиторий %q вне области задачи %s: расширить область — решение человека",
				ref, parent.Key)
		}
	}
	return nil
}

// taskFacts читает аренды, которые нужны праву: только для бота — у
// остальных классов право от аренды не зависит.
func (e *Engine) taskFacts(ctx context.Context, q postgres.Querier, a Actor, task domain.Task) (TaskFacts, error) {
	f := TaskFacts{Task: task}
	if a.Class() != ClassBot {
		return f, nil
	}
	var err error
	if f.Held, err = e.holdsLease(ctx, q, task, a.Principal.ID); err != nil {
		return f, err
	}
	if task.ParentKey != nil && !f.Held {
		parent, err := e.store.GetTaskByKey(ctx, q, *task.ParentKey)
		if err != nil {
			return f, err
		}
		if f.ParentHeld, err = e.holdsLease(ctx, q, parent, a.Principal.ID); err != nil {
			return f, err
		}
	}
	return f, nil
}

// mayTask — право актора на act над задачей с фактами из базы.
func (e *Engine) mayTask(ctx context.Context, q postgres.Querier, a Actor, act TaskAct, task domain.Task) error {
	f, err := e.taskFacts(ctx, q, a, task)
	if err != nil {
		return err
	}
	return TaskRight(a, act, f)
}

// TaskRights — право актора на задачу для карточки (actor_rights): булевы
// поля и у каждого false — отказ тем же видом, что у переходов. Поле move
// добавит перевод в статус (CLAB-408).
type TaskRights struct {
	Edit        bool                    `json:"edit"`
	StageChoice bool                    `json:"stage_choice"`
	CreateChild bool                    `json:"create_child"`
	Assign      bool                    `json:"assign"`
	Link        bool                    `json:"link"`
	Scope       bool                    `json:"scope"`
	ActorDeny   map[string]*ActorDenial `json:"actor_deny,omitempty"`
}

// TaskRightsQ — actor_rights задачи на q: та же TaskRight, что у ручек, на
// тех же фактах. Свойства без репозиториев в карточке не отдельно: их право
// не уже права на область ни у одного класса, кроме бота.
func (e *Engine) TaskRightsQ(ctx context.Context, q postgres.Querier, task domain.Task, a Actor) (TaskRights, error) {
	f, err := e.taskFacts(ctx, q, a, task)
	if err != nil {
		return TaskRights{}, err
	}
	out := TaskRights{}
	for _, r := range []struct {
		act TaskAct
		dst *bool
	}{
		{ActEdit, &out.Edit}, {ActStageChoice, &out.StageChoice}, {ActCreateChild, &out.CreateChild},
		{ActAssign, &out.Assign}, {ActLink, &out.Link}, {ActScope, &out.Scope},
	} {
		err := TaskRight(a, r.act, f)
		*r.dst = err == nil
		if err != nil {
			if out.ActorDeny == nil {
				out.ActorDeny = map[string]*ActorDenial{}
			}
			out.ActorDeny[string(r.act)] = denialOf(err)
		}
	}
	return out, nil
}

// TaskRights — actor_rights задачи по ключу на пуле.
func (e *Engine) TaskRights(ctx context.Context, taskKey string, a Actor) (TaskRights, error) {
	q := e.store.DB()
	task, err := e.store.GetTaskByKey(ctx, q, taskKey)
	if err != nil {
		return TaskRights{}, err
	}
	return e.TaskRightsQ(ctx, q, task, a)
}

// TaskWork — что актор может делать с задачами проекта, для clab whoami и
// sdlc_whoami: класс и строка для человека. Клиент печатает её как есть.
type TaskWork struct {
	Class   ActorClass `json:"class"`
	Summary string     `json:"summary"`
}

// TaskWorkOf — сводка права по классу актора.
func TaskWorkOf(a Actor) TaskWork {
	c := a.Class()
	w := TaskWork{Class: c}
	switch c {
	case ClassUnchecked, ClassMaintainer:
		w.Summary = "любая задача проекта: постановка и выбор этапов, заведение, назначение, связи, область"
	case ClassEmployee:
		w.Summary = "любая задача проекта без аренды: постановка и выбор этапов, заведение (и без родителя), " +
			"назначение, связи, область"
	case ClassBot:
		w.Summary = "постановка и дочерние задачи — там, где держите этап по аренде, в области родителя; " +
			"назначение и связи; область — решение человека"
	case ClassTaskToken:
		w.Summary = "своя задача " + a.Bound.TaskKey + " и её дети: постановка, назначение, связи; " +
			"дети — в области своей задачи; выбор этапов своей задачи и область — решение человека"
	default:
		w.Summary = "только чтение: работа с задачами — с уровня agent"
	}
	return w
}
