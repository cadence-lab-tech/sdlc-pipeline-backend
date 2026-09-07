package workflow

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
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

// CreateTask заводит задачу и ставит её на начальный статус активного цикла.
func (e *Engine) CreateTask(ctx context.Context, projectKey string, actor uuid.UUID, in postgres.CreateTaskInput) (domain.Task, error) {
	var out domain.Task

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := e.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		in.ProjectID = project.ID
		in.CreatedBy = actor

		out, err = e.store.CreateTask(ctx, tx, in)
		if err != nil {
			return err
		}

		run, err := e.store.CurrentStageRun(ctx, tx, out.ID, out.StatusID)
		if err != nil {
			return err
		}
		if err := e.store.AppendEvent(ctx, tx, out.ID, &run.ID, domain.EventTaskCreated, actor,
			map[string]any{"key": out.Key, "title": out.Title, "status": out.StatusKey}); err != nil {
			return err
		}
		return e.store.AppendEvent(ctx, tx, out.ID, &run.ID, domain.EventStageEntered, actor,
			map[string]any{"status": out.StatusKey, "attempt": run.Attempt})
	})

	return out, err
}

// Store открывает доступ к хранилищу для чтения из транспортного слоя.
// Читающие ручки не проходят через движок: им нечего решать.
func (e *Engine) Store() *postgres.Store { return e.store }
