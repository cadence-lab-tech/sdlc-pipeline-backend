// Белый список исполнителя этапа (решение 0049 п. 3–4, CLAB-208).
//
// Токен задачи (tasktoken.go) привязывает работу к своей задаче и прогону.
// Здесь — что этим токеном вообще можно делать: перечень действий, круг
// записи и правила гейтов и переходов. Всё вне перечня — отказ, в том числе
// тулы и ручки, которые появятся позже: транспорты зовут ExecutorMay на
// каждый вызов, а не по месту в обработчиках.
//
// Правила действуют только на токен задачи и только в режиме токенов (Bound):
// личные токены и сессии людей, их уровни участия и правила движка для
// агентов (agentMayCreate, agentMayEdit, UpdateScope) не меняются.
package workflow

import (
	"context"
	"fmt"
	"sort"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// ExecutorRefusal — начало текста каждого отказа токену задачи в действии
// человека: по нему отказ узнают агент, журнал и тесты.
const ExecutorRefusal = "действие человека, не исполнителя этапа"

// PromptAction — имя действия для промпта MCP: у тулов и промптов имена в
// разных пространствах, и в перечне их надо различать.
func PromptAction(name string) string { return "prompt " + name }

// RESTAction — имя действия для маршрута REST: метод и шаблон пути, как его
// отдаёт gin (c.FullPath()).
func RESTAction(method, fullPath string) string { return method + " " + fullPath }

// executorActions — что разрешено токену задачи. Одна строка — один тул MCP,
// промпт или маршрут REST; правило внутри разрешённого (своя задача, круг
// записи, вид гейта и перехода) проверяют сами действия.
var executorActions = map[string]bool{}

func init() {
	for _, a := range []string{
		// Работа по аренде: привязку к своему прогону держит tasktoken.go.
		"sdlc_claim_task", "sdlc_heartbeat", "sdlc_release", "sdlc_submit_artifact",
		"sdlc_link_repo_ref", "sdlc_report_usage", "sdlc_request_transition",
		"sdlc_resolve_drift", "sdlc_check_gates", "sdlc_create_task",
		// Чтение в пределах круга чтения.
		"sdlc_whoami", "sdlc_list_tasks", "sdlc_get_task", "sdlc_get_artifact",
		"sdlc_get_drift", "sdlc_search_context",
		// Чтение правил.
		"sdlc_get_role", "sdlc_list_documents", "sdlc_get_document",
		// Запись в круг своей задачи (RequireWriteCircle, RequireLink).
		"sdlc_update_task", "sdlc_link_tasks", "sdlc_assign_task",
		// Им стартует раннер.
		PromptAction("work_on_task"),

		// REST — те же действия, которыми ходит clab.
		RESTAction("GET", "/v1/auth/me"),
		RESTAction("GET", "/v1/projects"),
		RESTAction("GET", "/v1/projects/:projectKey"),
		RESTAction("GET", "/v1/projects/:projectKey/roles"),
		RESTAction("GET", "/v1/projects/:projectKey/roles/:roleKey"),
		RESTAction("GET", "/v1/projects/:projectKey/documents"),
		RESTAction("GET", "/v1/projects/:projectKey/documents/:documentID"),
		RESTAction("GET", "/v1/projects/:projectKey/documents/:documentID/content"),
		RESTAction("GET", "/v1/projects/:projectKey/tasks"),
		RESTAction("POST", "/v1/projects/:projectKey/tasks"),
		RESTAction("GET", "/v1/projects/:projectKey/queue"),
		RESTAction("GET", "/v1/projects/:projectKey/search"),
		RESTAction("POST", "/v1/projects/:projectKey/search"),
		RESTAction("GET", "/v1/tasks/:taskKey"),
		RESTAction("GET", "/v1/tasks/:taskKey/state"),
		RESTAction("GET", "/v1/tasks/:taskKey/timeline"),
		RESTAction("GET", "/v1/tasks/:taskKey/artifacts"),
		RESTAction("GET", "/v1/tasks/:taskKey/drift"),
		RESTAction("GET", "/v1/tasks/:taskKey/repo-refs"),
		RESTAction("GET", "/v1/tasks/:taskKey/transitions"),
		RESTAction("GET", "/v1/tasks/:taskKey/usage"),
		RESTAction("GET", "/v1/artifacts/:artifactID/content"),
		RESTAction("POST", "/v1/tasks/:taskKey/claim"),
		RESTAction("POST", "/v1/tasks/:taskKey/heartbeat"),
		RESTAction("POST", "/v1/tasks/:taskKey/release"),
		RESTAction("POST", "/v1/tasks/:taskKey/artifacts"),
		RESTAction("POST", "/v1/tasks/:taskKey/repo-refs"),
		RESTAction("POST", "/v1/tasks/:taskKey/usage"),
		RESTAction("POST", "/v1/tasks/:taskKey/runs/:runID/usage"),
		RESTAction("POST", "/v1/drift-signals/:signalID/resolve"),
		RESTAction("POST", "/v1/tasks/:taskKey/transitions/:transitionKey"),
		// Решение по гейту — только passed по чек-листу своего этапа
		// (DecideGate).
		RESTAction("POST", "/v1/tasks/:taskKey/transitions/:transitionKey/gates/:gateKey/decision"),
		RESTAction("PATCH", "/v1/tasks/:taskKey"),
		RESTAction("POST", "/v1/tasks/:taskKey/links"),
		RESTAction("PUT", "/v1/tasks/:taskKey/assignee"),
	} {
		executorActions[a] = true
	}
}

// ExecutorActions — перечень целиком, по порядку: его сверяют с
// зарегистрированными тулами и маршрутами, чтобы опечатка в перечне валила
// тест, а не открывала доступ.
func ExecutorActions() []string {
	out := make([]string, 0, len(executorActions))
	for a := range executorActions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// executorRefused — отказ токену задачи; what уточняет, что именно.
func executorRefused(b *TaskBinding, what string) *apperr.Error {
	return apperr.Forbidden("%s: %s", ExecutorRefusal, what).WithDetails(map[string]any{
		"task_key": b.TaskKey, "stage_run_id": b.StageRunID, "token_kind": domain.TokenTask,
	})
}

// ExecutorMay — разрешено ли действие предъявленному токену. Не токен задачи
// или режим заголовка — всё решают уровни, как раньше. Уровень участия и
// is_admin здесь не смотрятся вовсе: токен задачи администратора ограничен
// так же.
func (a *Auth) ExecutorMay(id Identity, action string) error {
	b := a.Bound(id)
	if b == nil || executorActions[action] {
		return nil
	}
	return executorRefused(b, action)
}

// inWriteCircle — своя задача токена или её прямой ребёнок.
func inWriteCircle(b *TaskBinding, t domain.Task) bool {
	return t.ID == b.TaskID || (t.ParentID != nil && *t.ParentID == b.TaskID)
}

// RequireWriteCircle — правка постановки и назначение токеном задачи: только
// своя задача и её прямые дети, при любом уровне запустившего.
func (a *Auth) RequireWriteCircle(ctx context.Context, id Identity, taskKey, action string) error {
	b := a.Bound(id)
	if b == nil {
		return nil
	}
	task, err := a.store.GetTaskByKey(ctx, a.store.DB(), taskKey)
	if err != nil {
		return err
	}
	if inWriteCircle(b, task) {
		return nil
	}
	return executorRefused(b, fmt.Sprintf(
		"%s задачи %s: токен задачи %s пишет только в свою задачу и её детей", action, task.Key, b.TaskKey))
}

// RequireLink — связь токеном задачи (заведение и снятие), решение человека
// 2026-10-21: обе задачи в круге записи — любая связь; одна — relates_to,
// shares_surface, duplicates и «своя blocks чужую»; «чужая blocks свою»
// меняет порядок работ своей задачи и остаётся человеку. Задача вне круга
// записи должна быть в круге чтения: иначе связь сама открыла бы её чтению.
func (a *Auth) RequireLink(ctx context.Context, id Identity, fromKey, toKey, rel string) error {
	b := a.Bound(id)
	if b == nil {
		return nil
	}
	q := a.store.DB()
	from, err := a.store.GetTaskByKey(ctx, q, fromKey)
	if err != nil {
		return err
	}
	to, err := a.store.GetTaskByKey(ctx, q, toKey)
	if err != nil {
		return err
	}
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
		other := to
		if !fromIn {
			other = from
		}
		return a.RequireReadable(ctx, id, other.ID)
	default:
		return executorRefused(b, link+": с задачей вне круга своей задачи и её детей так связывать нельзя")
	}
}

// executorTransition — переход токеном задачи: вперёд и возврат. Снять
// задачу (cancel) и переназначить (reassign) — решение человека.
func executorTransition(b *TaskBinding, task domain.Task, t domain.Transition) error {
	if task.ID != b.TaskID {
		return executorRefused(b, fmt.Sprintf("переход задачи %s: задача чужая", task.Key))
	}
	switch t.Kind {
	case "advance", "reject":
		return nil
	}
	return executorRefused(b, fmt.Sprintf("переход %q вида %s — решение человека; "+
		"не можете доделать этап — release или problem.report с возвратом", t.Key, t.Kind))
}

// executorGate — решение по гейту токеном задачи: только passed по чек-листу
// перехода из текущего статуса своей задачи, пока идёт прогон токена.
// human_approval проверяется отдельно и раньше — по виду токена, а не
// только принципала.
func executorGate(b *TaskBinding, task domain.Task, run domain.StageRun, t domain.Transition, gate domain.Gate, status string) error {
	what := fmt.Sprintf("решение %s по гейту %q перехода %q", status, gate.Key, t.Key)
	switch {
	case task.ID != b.TaskID:
		return executorRefused(b, what+": задача "+task.Key+" чужая")
	case run.ID != b.StageRunID:
		return executorRefused(b, what+": этап задачи сейчас не у прогона токена")
	case gate.Type != domain.GateChecklist:
		return executorRefused(b, what+": исполнитель отмечает только чек-лист, а это "+gate.Type)
	case status != domain.GateResultPassed:
		return executorRefused(b, what+": исполнитель отмечает чек-лист только пройденным")
	case t.FromStatusID == nil || *t.FromStatusID != task.StatusID:
		return executorRefused(b, what+": переход не из текущего статуса задачи")
	}
	return nil
}
