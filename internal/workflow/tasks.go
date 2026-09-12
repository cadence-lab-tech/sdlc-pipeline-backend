package workflow

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

type SubmitArtifactInput struct {
	Kind         string
	Title        string
	Format       string
	Content      []byte
	MediaType    string
	SupersedesID *uuid.UUID
	Meta         json.RawMessage
	Links        []postgres.ArtifactLink
	CodeRefs     []postgres.ArtifactCodeRef
	// Прогон, к которому крепится результат. Пусто — движок определит его
	// сам: прогон перепроверки, если агент держит его, иначе текущий этап.
	StageRunID *uuid.UUID
}

// SubmitArtifact прикрепляет результат работы к попытке этапа.
//
// Привязка именно к stage_run, а не к задаче, — то, на чём стоит гейт
// artifacts_required: после отката задача заходит на статус заново, и
// артефакты прошлой попытки не должны закрывать гейт вместо новой работы.
func (e *Engine) SubmitArtifact(ctx context.Context, taskKey string, actor uuid.UUID, in SubmitArtifactInput) (domain.Artifact, error) {
	var out domain.Artifact

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		run, err := e.workRun(ctx, tx, task, actor, in.StageRunID)
		if err != nil {
			return err
		}
		if run.FinishedAt != nil {
			return apperr.Conflict("этап уже завершён, артефакт прикреплять некуда")
		}
		if err := e.checkContract(ctx, tx, run, in.Kind); err != nil {
			return err
		}
		// Повторная сдача того же kind — это новая версия, а не второй
		// «актуальный» артефакт: иначе гейты и контекст следующего этапа
		// видели бы две версии одного документа и выбирали бы случайную.
		if in.SupersedesID == nil {
			prev, err := e.store.LatestArtifactOfKind(ctx, tx, task.ID, in.Kind)
			if err != nil {
				return err
			}
			in.SupersedesID = prev
		}

		out, err = e.store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task:         task,
			StageRunID:   &run.ID,
			Kind:         in.Kind,
			Title:        in.Title,
			Format:       in.Format,
			Content:      in.Content,
			MediaType:    in.MediaType,
			SupersedesID: in.SupersedesID,
			Status:       domain.ArtifactSubmitted,
			Meta:         in.Meta,
			Links:        in.Links,
			CodeRefs:     in.CodeRefs,
			ProducedBy:   actor,
		})
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE stage_runs SET state = 'submitted'
			WHERE id = $1 AND state IN ('pending', 'claimed', 'in_progress')`, run.ID); err != nil {
			return err
		}

		if err := e.store.AppendEvent(ctx, tx, task.ID, &run.ID, domain.EventArtifactSubmitted, actor,
			map[string]any{
				"artifact_id": out.ID.String(),
				"kind":        out.Kind,
				"title":       out.Title,
				"version":     out.Version,
				"stage_kind":  run.Kind,
			}); err != nil {
			return err
		}

		if run.Kind == domain.StageKindReview {
			// Сдача вердикта и есть завершение работы ревьюера: собственного
			// перехода у него нет.
			return e.completeReview(ctx, tx, task, run, out, actor)
		}
		// Исполнитель сдал новую версию результата — прежний вердикт вынесен
		// о том, чего уже нет, и перепроверку придётся пройти заново.
		return e.resetReview(ctx, tx, run)
	})

	return out, err
}

// workRun определяет, в чей прогон агент сдаёт результат.
//
// Ревьюер и исполнитель работают на одном статусе, но в разных прогонах;
// перепутать их значит закрыть гейт artifacts_required вердиктом ревью или
// наоборот.
func (e *Engine) workRun(ctx context.Context, q postgres.Querier, task domain.Task, actor uuid.UUID, explicit *uuid.UUID) (domain.StageRun, error) {
	if explicit != nil {
		run, err := e.store.GetStageRun(ctx, q, *explicit)
		if err != nil {
			return run, err
		}
		if run.TaskID != task.ID {
			return run, apperr.InvalidInput("прогон принадлежит другой задаче")
		}
		return run, nil
	}

	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return run, err
	}
	reviews, err := e.store.OpenReviewRuns(ctx, q, run.ID)
	if err != nil {
		return run, err
	}
	for _, r := range reviews {
		if r.ClaimedBy != nil && *r.ClaimedBy == actor {
			return r, nil
		}
	}
	return run, nil
}

// checkContract не пускает артефакт, которого нет в output_contract роли.
//
// Это то место, где контракт роли перестаёт быть просто текстом: гейт
// artifacts_required проверяет наличие, а здесь проверяется, что роль вообще
// имеет право сдавать такой kind.
func (e *Engine) checkContract(ctx context.Context, q postgres.Querier, run domain.StageRun, kind string) error {
	if run.RoleVersionID == nil {
		return nil
	}
	rv, err := e.store.GetRoleVersion(ctx, q, *run.RoleVersionID)
	if err != nil {
		return err
	}

	var contract []struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(rv.OutputContract, &contract); err != nil || len(contract) == 0 {
		// Роль без объявленного контракта ничего не ограничивает: описать
		// контракт — решение проекта, а не требование движка.
		return nil
	}
	allowed := make([]string, 0, len(contract))
	for _, c := range contract {
		if c.Kind == kind {
			return nil
		}
		allowed = append(allowed, c.Kind)
	}
	// Описание проблемы сдаётся при откате назад и в контракт роли не входит:
	// это не результат этапа, а объяснение, почему его не будет.
	if isProblemReport(kind) {
		return nil
	}

	return apperr.New(apperr.CodeContractViolation,
		"артефакт %q не входит в контракт роли", kind).
		WithDetails(map[string]any{"allowed_kinds": allowed})
}

type UpdateScopeInput struct {
	// Ссылки на репозитории проекта: owner/name, name или role_hint.
	AddRepositories []string
	// Свойства задачи целиком; nil — не трогать.
	Properties json.RawMessage
	Note       string
}

// UpdateScope меняет область задачи по репозиториям и её свойства.
//
// Включение multi_repo и добавление второго репозитория идут одной
// транзакцией намеренно: инвариант держит отложенный триггер
// enforce_task_repo_scope(), который сработает на коммите и увидит уже
// согласованное состояние. Разбить это на два запроса нельзя — ни один из
// порядков не проходит по отдельности.
func (e *Engine) UpdateScope(ctx context.Context, taskKey string, actor uuid.UUID, in UpdateScopeInput) (domain.Task, error) {
	var out domain.Task

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}

		if in.Properties != nil {
			if err := e.store.UpdateTaskProperties(ctx, tx, task.ID, in.Properties); err != nil {
				return err
			}
			task.Properties = in.Properties
		}

		var run *uuid.UUID
		if r, err := e.store.CurrentStageRun(ctx, tx, task.ID, task.StatusID); err == nil {
			run = &r.ID
		}

		added := []string{}
		for _, ref := range in.AddRepositories {
			repo, err := e.store.FindRepository(ctx, tx, task.ProjectID, ref)
			if err != nil {
				return err
			}
			if err := e.store.AddTaskRepository(ctx, tx, task, repo.ID, run, actor, in.Note); err != nil {
				return err
			}
			added = append(added, repo.Owner+"/"+repo.Name)
			task.Repositories = append(task.Repositories, domain.TaskRepository{
				RepositoryID: repo.ID, Owner: repo.Owner, Name: repo.Name,
			})
		}

		if len(added) > 0 || in.Properties != nil {
			if err := e.store.AppendEvent(ctx, tx, task.ID, run, "task.scope_updated", actor,
				map[string]any{
					"added_repositories": added,
					"multi_repo":         task.MultiRepo(),
					"note":               in.Note,
				}); err != nil {
				return err
			}
		}

		out, err = e.store.GetTaskByKey(ctx, tx, taskKey)
		return err
	})

	return out, err
}

// maxTaskTitleRunes — предел заголовка задачи. Считается рунами, а не
// байтами: русский заголовок в байтах вдвое длиннее, и байтовый предел резал
// бы его посередине слова. Предел живёт в коде, а не в схеме: менять его не
// должно стоить миграции.
const maxTaskTitleRunes = 80

// UpdateTaskInput — правка постановки задачи. nil означает «не трогать»:
// правка описания не должна затирать заголовок.
type UpdateTaskInput struct {
	Title       *string
	Description *string
	Priority    *string
	Labels      *[]string
	// Уровень актора в проекте. Ниже сопровождающего действует правило
	// агента: править можно задачу с его живой арендой и её дочерние —
	// ровно та рамка, в которой он их заводит. Пусто — правило выключено:
	// в режиме header прав нет вовсе.
	Level domain.MemberRole
}

// UpdateTask меняет постановку задачи: заголовок, описание, приоритет, метки.
//
// Тип, статус, родитель, область и исполнитель этой ручкой не меняются — у
// каждого своя операция и свои правила. Особого правила для терминальных
// статусов нет: агента и так ограничивает аренда, которой у закрытой задачи
// не бывает, а сопровождающий вправе поправить опечатку и в закрытой
// постановке.
func (e *Engine) UpdateTask(ctx context.Context, taskKey string, actor uuid.UUID, in UpdateTaskInput) (domain.Task, error) {
	fields := postgres.UpdateTaskFieldsInput{
		Description: in.Description, Priority: in.Priority, Labels: in.Labels,
	}
	if in.Title != nil {
		title := strings.TrimSpace(*in.Title)
		if title == "" {
			return domain.Task{}, apperr.InvalidInput("заголовок задачи не может быть пустым")
		}
		if n := utf8.RuneCountInString(title); n > maxTaskTitleRunes {
			return domain.Task{}, apperr.InvalidInput(
				"заголовок длиннее %d символов (в нём %d): уточнение места — в описании",
				maxTaskTitleRunes, n)
		}
		fields.Title = &title
	}
	if in.Priority != nil && !domain.ValidTaskPriority(*in.Priority) {
		return domain.Task{}, apperr.InvalidInput(
			"приоритет %q не поддерживается: %v", *in.Priority, domain.TaskPriorities)
	}
	if !fields.Any() {
		return domain.Task{}, apperr.InvalidInput(
			"не указано ни одно поле: править можно title, description, priority, labels")
	}

	var out domain.Task
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		if err := e.agentMayEdit(ctx, tx, actor, task, in.Level); err != nil {
			return err
		}
		if err := e.store.UpdateTaskFields(ctx, tx, task.ID, fields); err != nil {
			return err
		}

		// Прежние значения пишутся в ленту: версий у постановки нет, и лента
		// событий — единственное место, где остаётся, что было до правки.
		changed := map[string]any{}
		if fields.Title != nil {
			changed["title"] = map[string]any{"from": task.Title, "to": *fields.Title}
		}
		if fields.Description != nil {
			changed["description"] = map[string]any{
				"from": task.Description, "to": nullIfEmpty(*fields.Description),
			}
		}
		if fields.Priority != nil {
			changed["priority"] = map[string]any{"from": task.Priority, "to": *fields.Priority}
		}
		if fields.Labels != nil {
			changed["labels"] = map[string]any{"from": task.Labels, "to": *fields.Labels}
		}
		fieldNames := make([]string, 0, len(changed))
		for name := range changed {
			fieldNames = append(fieldNames, name)
		}
		sort.Strings(fieldNames)

		var run *uuid.UUID
		if r, err := e.store.CurrentStageRun(ctx, tx, task.ID, task.StatusID); err == nil {
			run = &r.ID
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, run, domain.EventTaskUpdated, actor,
			map[string]any{"fields": fieldNames, "changed": changed}); err != nil {
			return err
		}

		out, err = e.store.GetTaskByKey(ctx, tx, taskKey)
		return err
	})

	return out, err
}

// nullIfEmpty отличает «описания нет» от пустой строки: в ленте у очистки
// описания и у правки на пустую строку обязан быть один и тот же вид, потому
// что в базе это одно и то же.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// agentMayEdit — рамка, в которой агент вправе править постановку.
//
// Она повторяет рамку заведения задач (agentMayCreate): аналитик уточняет
// нарезку эпика, который держит, и не трогает чужие задачи. Сопровождающий и
// выше не ограничены ничем — в том числе на закрытых задачах.
func (e *Engine) agentMayEdit(ctx context.Context, q postgres.Querier, actor uuid.UUID, task domain.Task, level domain.MemberRole) error {
	if level == "" || level.Covers(domain.MemberMaintainer) {
		return nil
	}
	held, err := e.holdsLease(ctx, q, task, actor)
	if err != nil {
		return err
	}
	if held {
		return nil
	}
	if task.ParentKey != nil {
		parent, err := e.store.GetTaskByKey(ctx, q, *task.ParentKey)
		if err != nil {
			return err
		}
		held, err := e.holdsLease(ctx, q, parent, actor)
		if err != nil {
			return err
		}
		if held {
			return nil
		}
	}
	return apperr.Forbidden(
		"задачу %s держите не вы: править постановку может тот, кто забрал её этап "+
			"или этап её родительской задачи", task.Key)
}

// holdsLease отвечает, держит ли актор живую аренду текущего этапа задачи.
// Это и есть право агента на задачу: аренда снимается явно, и протухшая
// аренда права не даёт.
func (e *Engine) holdsLease(ctx context.Context, q postgres.Querier, task domain.Task, actor uuid.UUID) (bool, error) {
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return false, err
	}
	return run.ClaimedBy != nil && *run.ClaimedBy == actor && run.LeaseHeld(time.Now()), nil
}

// CreateTaskInput — заведение задачи движком: то же, что у хранилища, плюс
// связи, назначение и уровень актора.
type CreateTaskInput struct {
	postgres.CreateTaskInput
	// Хэндл принципала-исполнителя: оркестратор адресует нарезку конкретному
	// раннеру или человеку.
	AssigneeHandle string
	// Ключи задач, которые новая задача блокирует: порядок работ оркестратор
	// задаёт сразу при нарезке, а держит его гейт dependencies_closed.
	Blocks []string
	// Уровень актора в проекте. Ниже сопровождающего действует правило агента:
	// заводить можно только детей задачи, которую он держит по аренде, и только
	// внутри её области (решение 0024). Пусто — правило выключено: в режиме
	// header прав нет вовсе.
	Level domain.MemberRole
}

// CreateTask заводит задачу и ставит её на начальный статус цикла её типа.
func (e *Engine) CreateTask(ctx context.Context, projectKey string, actor uuid.UUID, in CreateTaskInput) (domain.Task, error) {
	var out domain.Task

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := e.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		in.ProjectID = project.ID
		in.CreatedBy = actor

		if in.AssigneeHandle != "" {
			assignee, err := e.resolveAssignee(ctx, tx, in.AssigneeHandle)
			if err != nil {
				return err
			}
			in.AssigneeID = &assignee.ID
		}
		if err := e.agentMayCreate(ctx, tx, actor, in); err != nil {
			return err
		}

		out, err = e.store.CreateTask(ctx, tx, in.CreateTaskInput)
		if err != nil {
			return err
		}

		blocked := []string{}
		for _, key := range in.Blocks {
			target, err := e.store.GetTaskByKey(ctx, tx, key)
			if err != nil {
				return err
			}
			if target.ProjectID != project.ID {
				return apperr.InvalidInput("задача %q из другого проекта", key)
			}
			if _, err := e.store.LinkTasks(ctx, tx, out.ID, target.ID,
				domain.TaskLinkBlocks, "", actor); err != nil {
				return err
			}
			blocked = append(blocked, target.Key)
		}

		run, err := e.store.CurrentStageRun(ctx, tx, out.ID, out.StatusID)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"key": out.Key, "title": out.Title, "status": out.StatusKey, "type": out.Type(),
		}
		if out.ParentKey != nil {
			payload["parent"] = *out.ParentKey
		}
		if len(blocked) > 0 {
			payload["blocks"] = blocked
		}
		if in.AssigneeHandle != "" {
			payload["assignee"] = in.AssigneeHandle
		}
		if err := e.store.AppendEvent(ctx, tx, out.ID, &run.ID, domain.EventTaskCreated, actor,
			payload); err != nil {
			return err
		}
		return e.store.AppendEvent(ctx, tx, out.ID, &run.ID, domain.EventStageEntered, actor,
			map[string]any{"status": out.StatusKey, "attempt": run.Attempt})
	})

	return out, err
}

// agentMayCreate — рамка, в которой агент вправе заводить задачи.
//
// Оркестратор нарезает эпик, который держит: он не может завести задачу в
// чужом эпике и не может расширить область — расширение области решает
// человек (решение 0015). Сопровождающий и выше не ограничены ничем.
func (e *Engine) agentMayCreate(ctx context.Context, q postgres.Querier, actor uuid.UUID, in CreateTaskInput) error {
	if in.Level == "" || in.Level.Covers(domain.MemberMaintainer) {
		return nil
	}
	if in.ParentKey == "" {
		return apperr.Forbidden(
			"агент заводит только дочерние задачи той, которую держит: укажите parent")
	}

	parent, err := e.store.GetTaskByKey(ctx, q, in.ParentKey)
	if err != nil {
		return err
	}
	held, err := e.holdsLease(ctx, q, parent, actor)
	if err != nil {
		return err
	}
	if !held {
		return apperr.Forbidden(
			"задачу %s держите не вы: нарезать её на дочерние может только тот, кто её забрал",
			parent.Key)
	}

	if len(in.Repositories) == 0 {
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
	for _, ref := range in.Repositories {
		if !allowed[ref] {
			return apperr.New(apperr.CodeScopeExpansionForbidden,
				"репозиторий %q вне области задачи %s: расширить область — решение человека",
				ref, parent.Key)
		}
	}
	return nil
}

// resolveAssignee находит исполнителя по хэндлу.
//
// Хэндл принимается и с видом принципала (agent:runner-bot), и без него:
// первым его пишут люди и агенты, вторым он хранится. Нового принципала здесь
// не заводят: назначение на несуществующего исполнителя — опечатка, а не
// повод завести учётную запись.
func (e *Engine) resolveAssignee(ctx context.Context, q postgres.Querier, raw string) (domain.Principal, error) {
	_, handle := SplitHandle(raw, domain.PrincipalAgent)
	return e.store.GetPrincipalByHandle(ctx, q, handle)
}

// LinkTasks связывает две задачи. Отношение blocks задаёт порядок работ, и
// его читает гейт dependencies_closed; остальные отношения — контекст для
// агента и человека.
func (e *Engine) LinkTasks(ctx context.Context, fromKey, toKey, rel, note string, actor uuid.UUID, remove bool) ([]postgres.TaskLink, error) {
	if !domain.ValidTaskLinkRel(rel) {
		return nil, apperr.InvalidInput("отношение %q не поддерживается: %v", rel, domain.TaskLinkRels)
	}

	var out []postgres.TaskLink
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		from, err := e.store.GetTaskByKey(ctx, tx, fromKey)
		if err != nil {
			return err
		}
		to, err := e.store.GetTaskByKey(ctx, tx, toKey)
		if err != nil {
			return err
		}
		if from.ID == to.ID {
			return apperr.InvalidInput("задачу нельзя связать с самой собой")
		}
		if from.ProjectID != to.ProjectID {
			return apperr.InvalidInput("задачи из разных проектов")
		}

		if remove {
			if err := e.store.UnlinkTasks(ctx, tx, from.ID, to.ID, rel); err != nil {
				return err
			}
		} else if _, err := e.store.LinkTasks(ctx, tx, from.ID, to.ID, rel, note, actor); err != nil {
			return err
		}

		event := "task.linked"
		if remove {
			event = "task.unlinked"
		}
		if err := e.store.AppendEvent(ctx, tx, from.ID, nil, event, actor,
			map[string]any{"rel": rel, "task": to.Key, "note": note}); err != nil {
			return err
		}
		out, err = e.store.ListTaskLinks(ctx, tx, from.ID)
		return err
	})

	return out, err
}

// Assign меняет исполнителя задачи. Назначенную задачу заберёт только он:
// так оркестратор адресует нарезку конкретному раннеру (решение 0025).
func (e *Engine) Assign(ctx context.Context, taskKey, handle string, actor uuid.UUID) (domain.Task, error) {
	var out domain.Task

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}

		var assignee *uuid.UUID
		if handle != "" {
			p, err := e.resolveAssignee(ctx, tx, handle)
			if err != nil {
				return err
			}
			assignee = &p.ID
		}
		if err := e.store.SetAssignee(ctx, tx, task.ID, assignee); err != nil {
			return err
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, nil, "task.assigned", actor,
			map[string]any{"assignee": handle}); err != nil {
			return err
		}

		out, err = e.store.GetTaskByKey(ctx, tx, taskKey)
		return err
	})

	return out, err
}

// Store открывает доступ к хранилищу для чтения из транспортного слоя.
// Читающие ручки не проходят через движок: им нечего решать.
func (e *Engine) Store() *postgres.Store { return e.store }
