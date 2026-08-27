// Редактирование ролей и цикла.
//
// Два правила, из которых следует всё остальное в этом файле:
//
//  1. Версия роли иммутабельна. Правка инструкции — новая версия; stage_runs
//     пинят ту, по которой работали, поэтому история этапов не переписывается.
//  2. Активный цикл иммутабелен. Правка цикла — новая версия черновиком,
//     которую потом активируют целиком. Иначе задача, стоящая сейчас на
//     статусе, теряет из-под себя переход или гейт посреди этапа.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

type Authoring struct {
	store *postgres.Store
	log   *slog.Logger
	// Движок нужен ради одного: знать, какие типы гейтов он умеет исполнять.
	// Цикл с гейтом нереализованного типа — законный черновик, но человек
	// должен узнать об этом при активации, а не когда задача встанет.
	engine *Engine
}

func NewAuthoring(store *postgres.Store, log *slog.Logger, engine *Engine) *Authoring {
	return &Authoring{store: store, log: log, engine: engine}
}

// ── роли ────────────────────────────────────────────────────────────────────

type RoleVersionInput struct {
	Instructions   string
	InputKinds     []string
	OutputContract json.RawMessage
	ToolPolicy     json.RawMessage
	ModelHint      *string
	RepoScope      []string
	ContextBudget  json.RawMessage
	Changelog      *string
}

type CreateRoleInput struct {
	Key     string
	Name    string
	Summary *string
	// Первая версия инструкции. Роль без версии бесполезна — на статус её
	// поставить можно, но агенту нечего будет отдать.
	Version *RoleVersionInput
}

type RoleWithVersions struct {
	Role     domain.Role          `json:"role"`
	Versions []domain.RoleVersion `json:"versions,omitempty"`
	Latest   *domain.RoleVersion  `json:"latest_version,omitempty"`
	UsedOn   []postgres.RoleUsage `json:"used_on"`
}

func (a *Authoring) CreateRole(ctx context.Context, projectKey string, actor uuid.UUID, in CreateRoleInput) (RoleWithVersions, error) {
	var out RoleWithVersions

	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		role, err := a.store.CreateRole(ctx, tx, postgres.CreateRoleInput{
			ProjectID: project.ID, Key: in.Key, Name: in.Name,
			Summary: in.Summary, CreatedBy: actor,
		})
		if err != nil {
			return err
		}
		out.Role = role

		if in.Version == nil {
			return nil
		}
		rv, err := a.addVersion(ctx, tx, role.ID, actor, *in.Version)
		if err != nil {
			return err
		}
		out.Latest = &rv
		out.Role.LatestVersion = &rv.Version
		return nil
	})

	return out, err
}

// AddRoleVersion — единственный способ изменить поведение роли.
func (a *Authoring) AddRoleVersion(ctx context.Context, projectKey, roleKey string, actor uuid.UUID, in RoleVersionInput) (domain.RoleVersion, error) {
	var out domain.RoleVersion

	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		role, err := a.role(ctx, tx, projectKey, roleKey)
		if err != nil {
			return err
		}
		out, err = a.addVersion(ctx, tx, role.ID, actor, in)
		return err
	})

	return out, err
}

func (a *Authoring) addVersion(ctx context.Context, tx pgx.Tx, roleID uuid.UUID, actor uuid.UUID, in RoleVersionInput) (domain.RoleVersion, error) {
	if strings.TrimSpace(in.Instructions) == "" {
		return domain.RoleVersion{}, apperr.InvalidInput("инструкция роли не может быть пустой")
	}
	if err := validateOutputContract(in.OutputContract); err != nil {
		return domain.RoleVersion{}, err
	}
	return a.store.CreateRoleVersion(ctx, tx, postgres.CreateRoleVersionInput{
		RoleID:         roleID,
		Instructions:   in.Instructions,
		InputKinds:     in.InputKinds,
		OutputContract: in.OutputContract,
		ToolPolicy:     in.ToolPolicy,
		ModelHint:      in.ModelHint,
		RepoScope:      in.RepoScope,
		ContextBudget:  in.ContextBudget,
		Changelog:      in.Changelog,
		CreatedBy:      actor,
	})
}

// validateOutputContract проверяет форму контракта выхода роли.
//
// На эти же kind'ы ссылается гейт artifacts_required. Опечатка здесь означает
// гейт, который не закроется ничем, — ловить её лучше при сохранении роли.
func validateOutputContract(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var items []struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return apperr.InvalidInput(
			"output_contract — список артефактов вида [{\"kind\":\"analysis.brief\",...}]")
	}
	for i, it := range items {
		if strings.TrimSpace(it.Kind) == "" {
			return apperr.InvalidInput("в output_contract[%d] не указан kind", i)
		}
	}
	return nil
}

func (a *Authoring) GetRole(ctx context.Context, projectKey, roleKey string) (RoleWithVersions, error) {
	var out RoleWithVersions
	q := a.store.DB()

	role, err := a.role(ctx, q, projectKey, roleKey)
	if err != nil {
		return out, err
	}
	out.Role = role
	if out.Versions, err = a.store.ListRoleVersions(ctx, q, role.ID); err != nil {
		return out, err
	}
	if len(out.Versions) > 0 {
		out.Latest = &out.Versions[0]
	}
	out.UsedOn, err = a.store.RoleUsage(ctx, q, role.ID)
	return out, err
}

func (a *Authoring) UpdateRole(ctx context.Context, projectKey, roleKey string, name, summary *string) (domain.Role, error) {
	q := a.store.DB()
	role, err := a.role(ctx, q, projectKey, roleKey)
	if err != nil {
		return role, err
	}
	if err := a.store.UpdateRole(ctx, q, role.ID, name, summary); err != nil {
		return role, err
	}
	return a.store.GetRoleByKey(ctx, q, role.ProjectID, roleKey)
}

// ArchiveRole убирает роль из выбора. Роль, стоящую на статусе активного
// цикла, архивировать нельзя: статус остался бы без исполнителя, и задачи
// встали бы на нём молча.
func (a *Authoring) ArchiveRole(ctx context.Context, projectKey, roleKey string, archived bool) (domain.Role, error) {
	q := a.store.DB()
	role, err := a.role(ctx, q, projectKey, roleKey)
	if err != nil {
		return role, err
	}

	if archived {
		usage, err := a.store.RoleUsage(ctx, q, role.ID)
		if err != nil {
			return role, err
		}
		active := []postgres.RoleUsage{}
		for _, u := range usage {
			if u.WorkflowState == domain.WorkflowActive {
				active = append(active, u)
			}
		}
		if len(active) > 0 {
			return role, apperr.Conflict(
				"роль %q стоит на статусах активного цикла; сначала уберите её новой версией цикла",
				roleKey).WithDetails(map[string]any{"used_on": active})
		}
	}

	if err := a.store.ArchiveRole(ctx, q, role.ID, archived); err != nil {
		return role, err
	}
	return a.store.GetRoleByKey(ctx, q, role.ProjectID, roleKey)
}

func (a *Authoring) ListRoles(ctx context.Context, projectKey string, includeArchived bool) ([]domain.Role, error) {
	q := a.store.DB()
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	return a.store.ListRoles(ctx, q, project.ID, includeArchived)
}

func (a *Authoring) role(ctx context.Context, q postgres.Querier, projectKey, roleKey string) (domain.Role, error) {
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return domain.Role{}, err
	}
	return a.store.GetRoleByKey(ctx, q, project.ID, roleKey)
}

// ── версии цикла ────────────────────────────────────────────────────────────

// CreateDraft заводит черновик цикла, обычно копией уже работающего:
// с нуля цикл собирают один раз, дальше меняют один гейт из двадцати.
func (a *Authoring) CreateDraft(ctx context.Context, projectKey string, actor uuid.UUID, name, from string) (domain.Workflow, error) {
	var out domain.Workflow

	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}

		var src *domain.Workflow
		switch {
		case from == "empty":
			// Цикл с нуля: законный старт для проекта, у которого его ещё нет.
		case from == "" || from == "active":
			wf, err := a.store.GetActiveWorkflow(ctx, tx, project.ID)
			if err != nil {
				// Копировать нечего только если цикла в проекте вообще нет —
				// и это не ошибка, если источник не потребовали явно.
				if from == "active" {
					return err
				}
				break
			}
			src = &wf
		default:
			id, err := uuid.Parse(from)
			if err != nil {
				return apperr.InvalidInput("from — \"active\" или идентификатор версии цикла")
			}
			wf, err := a.store.WorkflowHead(ctx, tx, id)
			if err != nil {
				return err
			}
			if wf.ProjectID != project.ID {
				return apperr.InvalidInput("версия цикла принадлежит другому проекту")
			}
			src = &wf
		}

		if name == "" {
			if src != nil {
				name = src.Name
			} else {
				name = "Цикл"
			}
		}

		out, err = a.store.CreateWorkflow(ctx, tx, project.ID, name, actor)
		if err != nil {
			return err
		}
		if src != nil {
			if err := a.store.CopyWorkflowContent(ctx, tx, src.ID, out.ID); err != nil {
				return err
			}
		}
		out, err = a.store.GetWorkflow(ctx, tx, out.ID)
		return err
	})

	return out, err
}

func (a *Authoring) ListWorkflows(ctx context.Context, projectKey string) ([]domain.Workflow, error) {
	q := a.store.DB()
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	return a.store.ListWorkflows(ctx, q, project.ID)
}

func (a *Authoring) GetWorkflow(ctx context.Context, id uuid.UUID) (domain.Workflow, error) {
	return a.store.GetWorkflow(ctx, a.store.DB(), id)
}

// DeleteWorkflow сносит черновик. Активную и уже отработавшую версию удалять
// нельзя: на их статусы ссылаются задачи и пройденные этапы.
func (a *Authoring) DeleteWorkflow(ctx context.Context, id uuid.UUID) error {
	return a.store.InTx(ctx, func(tx pgx.Tx) error {
		wf, err := a.draft(ctx, tx, id)
		if err != nil {
			return err
		}
		return a.store.DeleteWorkflow(ctx, tx, wf.ID)
	})
}

// draft читает версию цикла и требует, чтобы она была черновиком.
// Через эту проверку проходит каждая правка: активный цикл не редактируется.
func (a *Authoring) draft(ctx context.Context, q postgres.Querier, id uuid.UUID) (domain.Workflow, error) {
	wf, err := a.store.WorkflowHead(ctx, q, id)
	if err != nil {
		return wf, err
	}
	if wf.State != domain.WorkflowDraft {
		return wf, apperr.Conflict(
			"версия цикла %q (v%d) в состоянии %q и не редактируется; "+
				"правка цикла — новый черновик копией этой версии",
			wf.Name, wf.Version, wf.State)
	}
	return wf, nil
}

// edit — общая обёртка правки черновика: проверка состояния и работа под одной
// транзакцией, чтобы порядок «сначала проверили, потом изменили» не разъезжался.
func (a *Authoring) edit(ctx context.Context, id uuid.UUID, fn func(tx pgx.Tx, wf domain.Workflow) error) error {
	return a.store.InTx(ctx, func(tx pgx.Tx) error {
		wf, err := a.draft(ctx, tx, id)
		if err != nil {
			return err
		}
		return fn(tx, wf)
	})
}

// ── правка черновика ────────────────────────────────────────────────────────

func (a *Authoring) CreateStatus(ctx context.Context, id uuid.UUID, in postgres.StatusInput) (domain.Status, error) {
	var out domain.Status
	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		var err error
		out, err = a.store.CreateStatus(ctx, tx, wf.ID, in)
		return err
	})
	return out, err
}

func (a *Authoring) UpdateStatus(ctx context.Context, id uuid.UUID, key string, in postgres.StatusInput) (domain.Status, error) {
	var out domain.Status
	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		var err error
		out, err = a.store.UpdateStatus(ctx, tx, wf.ID, key, in)
		return err
	})
	return out, err
}

func (a *Authoring) DeleteStatus(ctx context.Context, id uuid.UUID, key string) error {
	return a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		return a.store.DeleteStatus(ctx, tx, wf.ID, key)
	})
}

func (a *Authoring) ReorderStatuses(ctx context.Context, id uuid.UUID, keys []string) ([]domain.Status, error) {
	var out []domain.Status
	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		if err := a.store.ReorderStatuses(ctx, tx, wf.ID, keys); err != nil {
			return err
		}
		var err error
		out, err = a.store.ListStatuses(ctx, tx, wf.ID)
		return err
	})
	return out, err
}

// SetStatusRoles задаёт исполнителя статуса и проверяющих для agent_review.
func (a *Authoring) SetStatusRoles(ctx context.Context, id uuid.UUID, statusKey string, roles []postgres.StatusRoleInput) (domain.Status, error) {
	var out domain.Status

	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		primary := 0
		for _, r := range roles {
			if r.IsPrimary {
				primary++
			}
		}
		if primary > 1 {
			return apperr.InvalidInput("на статусе может быть только одна основная роль")
		}

		st, err := a.store.GetStatusByKey(ctx, tx, wf.ID, statusKey)
		if err != nil {
			return err
		}
		if err := a.store.SetStatusRoles(ctx, tx, wf.ProjectID, st.ID, roles); err != nil {
			return err
		}
		out, err = a.store.GetStatusByKey(ctx, tx, wf.ID, statusKey)
		if err != nil {
			return err
		}
		out.Roles, err = a.store.StatusRoles(ctx, tx, out.ID)
		return err
	})

	return out, err
}

func (a *Authoring) CreateTransition(ctx context.Context, id uuid.UUID, in postgres.TransitionInput) (domain.Transition, error) {
	var out domain.Transition
	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		if in.ToStatus == "" {
			return apperr.InvalidInput("у перехода не указан статус назначения")
		}
		if !validTransitionKind(in.Kind) {
			return apperr.InvalidInput(
				"вид перехода — advance, reject, reassign или cancel, а не %q", in.Kind)
		}
		var err error
		out, err = a.store.CreateTransition(ctx, tx, wf.ID, in)
		return err
	})
	return out, err
}

func (a *Authoring) UpdateTransition(ctx context.Context, id uuid.UUID, key string, in postgres.TransitionInput) (domain.Transition, error) {
	var out domain.Transition
	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		if in.Kind != "" && !validTransitionKind(in.Kind) {
			return apperr.InvalidInput(
				"вид перехода — advance, reject, reassign или cancel, а не %q", in.Kind)
		}
		var err error
		out, err = a.store.UpdateTransition(ctx, tx, wf.ID, key, in)
		return err
	})
	return out, err
}

func (a *Authoring) DeleteTransition(ctx context.Context, id uuid.UUID, key string) error {
	return a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		return a.store.DeleteTransition(ctx, tx, wf.ID, key)
	})
}

func validTransitionKind(kind string) bool {
	switch kind {
	case "advance", "reject", "reassign", "cancel":
		return true
	}
	return false
}

func (a *Authoring) CreateGate(ctx context.Context, id uuid.UUID, transitionKey string, in postgres.GateInput) (domain.Gate, error) {
	var out domain.Gate
	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		t, err := a.store.GetTransitionByKey(ctx, tx, wf.ID, transitionKey)
		if err != nil {
			return err
		}
		out, err = a.store.CreateGate(ctx, tx, t.ID, in)
		return err
	})
	return out, err
}

func (a *Authoring) UpdateGate(ctx context.Context, id uuid.UUID, transitionKey, gateKey string, in postgres.GateInput) (domain.Gate, error) {
	var out domain.Gate
	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		t, err := a.store.GetTransitionByKey(ctx, tx, wf.ID, transitionKey)
		if err != nil {
			return err
		}
		out, err = a.store.UpdateGate(ctx, tx, t.ID, gateKey, in)
		return err
	})
	return out, err
}

func (a *Authoring) DeleteGate(ctx context.Context, id uuid.UUID, transitionKey, gateKey string) error {
	return a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		t, err := a.store.GetTransitionByKey(ctx, tx, wf.ID, transitionKey)
		if err != nil {
			return err
		}
		return a.store.DeleteGate(ctx, tx, t.ID, gateKey)
	})
}

// ── проверка и активация ────────────────────────────────────────────────────

func (a *Authoring) Validate(ctx context.Context, id uuid.UUID) (Validation, error) {
	wf, err := a.store.GetWorkflow(ctx, a.store.DB(), id)
	if err != nil {
		return Validation{}, err
	}
	return validateWorkflow(wf, a.engine.implemented, a.engine.strict), nil
}

// ActivationResult — что стало с циклом проекта после активации.
type ActivationResult struct {
	Workflow domain.Workflow `json:"workflow"`
	// Версия, ушедшая на покой.
	RetiredID *uuid.UUID `json:"retired_workflow_id,omitempty"`
	// Задачи, которые доходят по прежней версии: задача идёт по тому циклу, на
	// котором начата, и активация её не двигает.
	OnPreviousVersion []postgres.OpenTaskOnWorkflow `json:"tasks_on_previous_version"`
	Warnings          []Problem                     `json:"warnings"`
}

// Activate делает черновик действующим циклом проекта.
//
// Задачи, начатые на прежней версии, остаются на ней и доходят по её правилам
// (см. Engine.Execute). Это не забывчивость: переписать под задачей набор
// гейтов посреди этапа — значит обнулить уже вынесенные по ним решения.
func (a *Authoring) Activate(ctx context.Context, id uuid.UUID, actor uuid.UUID) (ActivationResult, error) {
	var res ActivationResult

	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		wf, err := a.draft(ctx, tx, id)
		if err != nil {
			return err
		}
		full, err := a.store.GetWorkflow(ctx, tx, wf.ID)
		if err != nil {
			return err
		}

		v := validateWorkflow(full, a.engine.implemented, a.engine.strict)
		if !v.Valid {
			return apperr.InvalidInput("цикл нельзя активировать: %d проблем(ы)", len(v.Problems)).
				WithDetails(map[string]any{"problems": v.Problems, "warnings": v.Warnings})
		}
		res.Warnings = v.Warnings

		retired, err := a.store.RetireActiveWorkflow(ctx, tx, wf.ProjectID)
		if err != nil {
			return err
		}
		res.RetiredID = retired

		if err := a.store.SetWorkflowState(ctx, tx, wf.ID, domain.WorkflowActive); err != nil {
			return err
		}

		if retired != nil {
			carried, err := a.store.OpenTasksOnWorkflow(ctx, tx, *retired)
			if err != nil {
				return err
			}
			res.OnPreviousVersion = carried
			res.Warnings = append(res.Warnings, carriedOverWarnings(carried, full)...)
		}

		res.Workflow, err = a.store.GetWorkflow(ctx, tx, wf.ID)
		return err
	})

	if err == nil {
		a.log.Info("активирована версия цикла",
			"workflow", res.Workflow.ID, "name", res.Workflow.Name,
			"version", res.Workflow.Version, "actor", actor)
	}
	return res, err
}

// carriedOverWarnings предупреждает про задачи, статуса которых в новой версии
// нет: они доработают по старой, но на доске новой версии им нет колонки.
func carriedOverWarnings(tasks []postgres.OpenTaskOnWorkflow, wf domain.Workflow) []Problem {
	known := map[string]bool{}
	for _, st := range wf.Statuses {
		known[st.Key] = true
	}

	byStatus := map[string][]string{}
	for _, t := range tasks {
		if !known[t.StatusKey] {
			byStatus[t.StatusKey] = append(byStatus[t.StatusKey], t.Key)
		}
	}

	out := []Problem{}
	for status, keys := range byStatus {
		out = append(out, Problem{
			Code:   "status_dropped",
			Status: status,
			Message: fmt.Sprintf(
				"в новой версии нет статуса %q, а на нём стоят задачи прежней версии: %s",
				status, strings.Join(keys, ", ")),
		})
	}
	return out
}
