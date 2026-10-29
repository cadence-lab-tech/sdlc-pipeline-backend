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

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
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
	// Устарел (решение 0060), живёт ради clab и раннера прежних версий.
	ModelHint *string
	// Уровень модели роли по умолчанию; nil или "" — не задан.
	ModelLevel    *string
	RepoScope     []string
	ContextBudget json.RawMessage
	Changelog     *string
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
	level, err := roleModelLevel(in.ModelLevel)
	if err != nil {
		return domain.RoleVersion{}, err
	}
	return a.store.CreateRoleVersion(ctx, tx, postgres.CreateRoleVersionInput{
		RoleID:         roleID,
		Instructions:   in.Instructions,
		InputKinds:     in.InputKinds,
		OutputContract: in.OutputContract,
		ToolPolicy:     in.ToolPolicy,
		ModelHint:      in.ModelHint,
		ModelLevel:     level,
		RepoScope:      in.RepoScope,
		ContextBudget:  in.ContextBudget,
		Changelog:      in.Changelog,
		CreatedBy:      actor,
	})
}

// roleModelLevel проверяет уровень версии роли; пустая строка — «не задан».
func roleModelLevel(level *string) (*string, error) {
	if level == nil || *level == "" {
		return nil, nil
	}
	if err := checkModelLevel(*level, "model_level роли"); err != nil {
		return nil, err
	}
	return level, nil
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

// DraftInput — из чего собрать черновик цикла.
type DraftInput struct {
	Name string
	// "active" (по умолчанию) — копия работающего цикла тех же типов задач,
	// идентификатор версии — копия конкретной, "empty" — пустой черновик.
	From string
	// Типы задач, которые поведёт цикл. Пусто — обычные задачи.
	TaskTypes []string
}

// CreateDraft заводит черновик цикла, обычно копией уже работающего:
// с нуля цикл собирают один раз, дальше меняют один гейт из двадцати.
func (a *Authoring) CreateDraft(ctx context.Context, projectKey string, actor uuid.UUID, in DraftInput) (domain.Workflow, error) {
	var out domain.Workflow
	name, from := in.Name, in.From
	taskTypes := in.TaskTypes
	if len(taskTypes) == 0 {
		taskTypes = []string{domain.DefaultTaskType}
	}

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
			// Копируется цикл того же типа задач: черновик цикла эпиков не
			// должен начинаться с копии цикла разработки.
			wf, err := a.store.GetActiveWorkflowForType(ctx, tx, project.ID, taskTypes[0])
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

		out, err = a.store.CreateWorkflow(ctx, tx, project.ID, name, taskTypes, actor)
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

// UpdateDraft меняет имя черновика и типы задач, которые он поведёт.
// Активную версию не тронуть: по ней прямо сейчас идут задачи своего типа.
func (a *Authoring) UpdateDraft(ctx context.Context, id uuid.UUID, name string, taskTypes []string) (domain.Workflow, error) {
	var out domain.Workflow

	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		wf, err := a.draft(ctx, tx, id)
		if err != nil {
			return err
		}
		if name != "" && name != wf.Name {
			if err := a.store.RenameWorkflow(ctx, tx, wf.ID, name); err != nil {
				return err
			}
		}
		if len(taskTypes) > 0 {
			if err := a.store.SetWorkflowTaskTypes(ctx, tx, wf.ID, taskTypes); err != nil {
				return err
			}
		}
		out, err = a.store.GetWorkflow(ctx, tx, wf.ID)
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
		// Новый статус по умолчанию обязательный и включённый.
		if err := resolveOptional(false, true, &in); err != nil {
			return err
		}
		var err error
		out, err = a.store.CreateStatus(ctx, tx, wf.ID, in)
		return err
	})
	return out, err
}

func (a *Authoring) UpdateStatus(ctx context.Context, id uuid.UUID, key string, in postgres.StatusInput) (domain.Status, error) {
	var out domain.Status
	err := a.edit(ctx, id, func(tx pgx.Tx, wf domain.Workflow) error {
		cur, err := a.store.GetStatusByKey(ctx, tx, wf.ID, key)
		if err != nil {
			return err
		}
		if err := resolveOptional(cur.IsOptional, cur.EnabledByDefault, &in); err != nil {
			return err
		}
		out, err = a.store.UpdateStatus(ctx, tx, wf.ID, key, in)
		return err
	})
	return out, err
}

// resolveOptional сводит отметку необязательного этапа и его умолчание к
// итоговой паре (решение 0066, п. 2). Поля, которых нет во вводе, берутся из
// статуса до правки.
//
// Умолчание «выключен» бывает только у необязательного статуса: такой ввод
// отбивается здесь, до записи, — CHECK в базе лишь страховка, и его текст
// автору цикла ничего не объяснил бы. Снятие отметки возвращает умолчание
// во «включён» само: у обязательного статуса умолчания нет, и требовать
// ради этого второго поля в запросе незачем.
func resolveOptional(isOptional, enabledByDefault bool, in *postgres.StatusInput) error {
	if in.IsOptional != nil {
		isOptional = *in.IsOptional
	}
	if isOptional {
		return nil
	}
	if in.EnabledByDefault != nil && !*in.EnabledByDefault {
		return apperr.InvalidInput(
			"enabled_by_default: false бывает только у необязательного статуса (is_optional: true)")
	}
	if !enabledByDefault {
		on := true
		in.EnabledByDefault = &on
	}
	return nil
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
	// Версия, ушедшая на покой. Оставлено ради прежних клиентов; версий может
	// быть несколько, если новый цикл ведёт несколько типов задач.
	RetiredID *uuid.UUID  `json:"retired_workflow_id,omitempty"`
	Retired   []uuid.UUID `json:"retired_workflow_ids,omitempty"`
	// Задачи, которые доходят по прежней версии: начатая задача идёт по тому
	// циклу, на котором начата, и активация её не двигает.
	// Список обрезан до activationReportLimit, полное число — в Total.
	OnPreviousVersion      []postgres.OpenTaskOnWorkflow `json:"tasks_on_previous_version"`
	OnPreviousVersionTotal int                           `json:"tasks_on_previous_version_total"`
	// Задачи, переехавшие на новую версию: стояли в бэклоге, никто их не
	// забирал — им нужны актуальные правила (решение 0036). Список обрезан
	// до activationReportLimit, полное число — в MovedTotal.
	Moved      []postgres.MovedTask `json:"tasks_moved"`
	MovedTotal int                  `json:"tasks_moved_total"`
	Warnings   []Problem            `json:"warnings"`
}

// activationReportLimit — сколько задач перечисляется в отчёте активации.
// Бэклог проекта может быть в тысячи задач; человеку нужен счётчик и первые
// ключи, а не список на весь экран.
const activationReportLimit = 50

// Activate делает черновик действующим циклом проекта.
//
// Задачи, начатые на прежней версии, остаются на ней и доходят по её правилам
// (см. Engine.Execute). Это не забывчивость: переписать под задачей набор
// гейтов посреди этапа — значит обнулить уже вынесенные по ним решения.
// Задачи, которые ещё не начинались, — другое дело: они переезжают на новую
// версию, потому что терять им нечего, а работать они должны по актуальным
// правилам (решение 0036).
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

		// Замок проекта: создание задачи берёт ключ под тем же замком, так
		// что задача, заводимая в момент активации, либо переедет вместе с
		// бэклогом, либо сразу ляжет на новую версию.
		if err := a.store.LockProject(ctx, tx, wf.ProjectID); err != nil {
			return err
		}

		// На покой уходят циклы тех же типов задач: активация цикла эпиков не
		// должна оставить обычные задачи без цикла (решение 0024).
		retired, err := a.store.RetireActiveWorkflows(ctx, tx, wf.ProjectID, wf.TaskTypes)
		if err != nil {
			return err
		}
		res.Retired = retired
		if len(retired) > 0 {
			res.RetiredID = &retired[0]
		}

		if err := a.store.SetWorkflowState(ctx, tx, wf.ID, domain.WorkflowActive); err != nil {
			return err
		}

		// Типы задач цикла проект узнаёт из самой активации: иначе цикл для
		// эпиков активен, а завести эпик нельзя — тип «не объявлен в проекте».
		if err := a.store.DeclareTaskTypes(ctx, tx, wf.ProjectID, wf.TaskTypes); err != nil {
			return err
		}

		// Бэклог переезжает со всех прежних версий цикла, а не только со
		// снятой сейчас: задача могла остаться на более ранней версии
		// (решение 0036). Список только что снятых идёт первым — по нему
		// же считаются задачи, которые доходят по прежней версии.
		previous, err := a.store.RetiredWorkflows(ctx, tx, wf.ProjectID, wf.TaskTypes)
		if err != nil {
			return err
		}
		sources := append([]uuid.UUID{}, retired...)
		seen := map[uuid.UUID]bool{}
		for _, id := range retired {
			seen[id] = true
		}
		for _, id := range previous {
			if !seen[id] {
				sources = append(sources, id)
			}
		}

		res.Moved = []postgres.MovedTask{}
		for _, id := range sources {
			moved, err := a.store.MoveWaitingTasks(ctx, tx, id, full, actor)
			if err != nil {
				return err
			}
			res.Moved = append(res.Moved, moved...)

			carried, err := a.store.OpenTasksOnWorkflow(ctx, tx, id)
			if err != nil {
				return err
			}
			res.OnPreviousVersion = append(res.OnPreviousVersion, carried...)
			res.Warnings = append(res.Warnings, carriedOverWarnings(carried, full)...)
		}

		// Тип задачи, оставшийся без цикла, — не ошибка активации, но по нему
		// нельзя завести задачу: проект узнает об этом здесь, а не в момент,
		// когда кто-то попробует.
		orphans, err := a.typesWithoutWorkflow(ctx, tx, wf.ProjectID)
		if err != nil {
			return err
		}
		for _, taskType := range orphans {
			res.Warnings = append(res.Warnings, Problem{
				Code: "task_type_without_workflow",
				Message: fmt.Sprintf(
					"тип задачи %q объявлен в проекте, но активного цикла для него нет", taskType)})
		}

		res.MovedTotal = len(res.Moved)
		if len(res.Moved) > activationReportLimit {
			res.Moved = res.Moved[:activationReportLimit]
		}
		res.OnPreviousVersionTotal = len(res.OnPreviousVersion)
		if len(res.OnPreviousVersion) > activationReportLimit {
			res.OnPreviousVersion = res.OnPreviousVersion[:activationReportLimit]
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

// typesWithoutWorkflow — типы задач, объявленные проектом, которые не ведёт
// ни один действующий цикл.
func (a *Authoring) typesWithoutWorkflow(ctx context.Context, q postgres.Querier, projectID uuid.UUID) ([]string, error) {
	declared, err := a.store.TaskTypes(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	active, err := a.store.ListActiveWorkflows(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	for _, wf := range active {
		for _, t := range wf.TaskTypes {
			covered[t] = true
		}
	}

	out := []string{}
	for _, t := range declared {
		if !covered[t] {
			out = append(out, t)
		}
	}
	return out, nil
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
