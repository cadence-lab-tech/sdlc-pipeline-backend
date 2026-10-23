// Аренда задачи агентом: захват, продление, отпускание, и сборка того
// контекста, ради которого агент вообще приходит — инструкция роли,
// результаты предыдущих этапов и требования гейтов впереди.
//
// Всё это живёт в движке, а не в транспорте: MCP-тул и REST-ручка обязаны
// отдавать агенту одно и то же (решение 0011).
package workflow

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/trust"
)

// UsageReportExpected едет в ответе на захват: по нему агент понимает, что
// в конце этапа его ждут с отчётом о стоимости.
const UsageReportExpected = "expected"

// Lease — аренда: до какого момента задача закреплена за агентом и как часто
// он обязан подтверждать, что жив.
type Lease struct {
	StageRunID               uuid.UUID `json:"stage_run_id"`
	ExpiresAt                time.Time `json:"expires_at"`
	HeartbeatIntervalSeconds int       `json:"heartbeat_interval_seconds"`
}

// RoleView — инструкция роли в том виде, в каком её получает агент.
// Приезжает в ответе на захват: в локальном репозитории синхронизировать
// нечего, правка роли в UI действует со следующего захвата.
type RoleView struct {
	Key            string          `json:"key"`
	Name           string          `json:"name"`
	Version        int             `json:"version"`
	Instructions   string          `json:"instructions"`
	InputKinds     []string        `json:"input_kinds"`
	OutputContract json.RawMessage `json:"output_contract"`
	ToolPolicy     json.RawMessage `json:"tool_policy"`
	// Уровень модели версии роли по умолчанию (решение 0060).
	ModelLevel *string `json:"model_level"`
	// Устарел: строка модели для clab и раннера прежних версий.
	ModelHint     *string         `json:"model_hint,omitempty"`
	ContextBudget json.RawMessage `json:"context_budget,omitempty"`
}

type RepositoryView struct {
	Owner         string  `json:"owner"`
	Name          string  `json:"name"`
	DefaultBranch string  `json:"default_branch"`
	CloneURL      string  `json:"clone_url"`
	RoleHint      *string `json:"role_hint,omitempty"`
	Primary       bool    `json:"primary"`
}

type ArtifactRef struct {
	ID      uuid.UUID `json:"id"`
	Kind    string    `json:"kind"`
	Title   string    `json:"title"`
	Format  string    `json:"format"`
	Stage   string    `json:"stage,omitempty"`
	Status  string    `json:"status"`
	Version int       `json:"version"`
}

// Rejection — из-за чего этап начался заново.
type Rejection struct {
	FromStage               string     `json:"from_stage"`
	Reason                  string     `json:"reason,omitempty"`
	ProblemReportArtifactID *uuid.UUID `json:"problem_report_artifact_id,omitempty"`
}

// Review — контекст прогона перепроверки: чей результат проверяется.
// У обычного этапа он пуст.
type Review struct {
	ParentRunID      uuid.UUID     `json:"parent_run_id"`
	Gate             string        `json:"gate"`
	Transition       string        `json:"transition,omitempty"`
	SubjectRole      string        `json:"subject_role,omitempty"`
	SubjectArtifacts []ArtifactRef `json:"subject_artifacts"`
}

type ClaimContext struct {
	Repositories     []RepositoryView         `json:"repositories"`
	ProjectDocuments []domain.ProjectDocument `json:"project_documents"`
	PriorArtifacts   []ArtifactRef            `json:"prior_artifacts"`
	Rejection        *Rejection               `json:"rejection,omitempty"`
	Baselines        []postgres.TaskBaseline  `json:"baselines"`
	Drift            []postgres.DriftSignal   `json:"drift"`
	RepoRefs         []postgres.TaskRepoRef   `json:"repo_refs"`
	Review           *Review                  `json:"review,omitempty"`
}

// GateSpec — требование гейта до его прогона: агент должен знать условия
// заранее, а не узнавать о них на попытке перехода.
type GateSpec struct {
	Key      string          `json:"key"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Config   json.RawMessage `json:"config"`
	Blocking bool            `json:"blocking"`
}

type TransitionSpec struct {
	Transition string     `json:"transition"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	ToStatus   string     `json:"to_status"`
	Gates      []GateSpec `json:"gates"`
}

type TaskView struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	Description *string         `json:"description,omitempty"`
	Status      string          `json:"status"`
	StatusName  string          `json:"status_name"`
	Attempt     int             `json:"attempt"`
	Priority    string          `json:"priority"`
	Labels      []string        `json:"labels"`
	ExternalRef json.RawMessage `json:"external_ref"`
	// Системные свойства движок интерпретирует сам, проектные отдаются как
	// есть — их смысл знает роль, а не движок.
	Properties json.RawMessage `json:"properties"`
	// Тип задачи и родитель: по типу выбран цикл, по родителю агент понимает,
	// что работает над куском эпика (решение 0024).
	Type   string  `json:"type"`
	Parent *string `json:"parent,omitempty"`
	// Уровень модели задачи и настройки этапов (решение 0060) — постановка,
	// по которой разрешён execution.
	ModelLevel    *string         `json:"model_level"`
	StageSettings json.RawMessage `json:"stage_settings"`
}

// StageView — состояние прогона для агента.
type StageView struct {
	StageRunID uuid.UUID  `json:"stage_run_id"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"`
	Attempt    int        `json:"attempt"`
	ClaimedBy  *string    `json:"claimed_by,omitempty"`
	LeaseUntil *time.Time `json:"lease_expires_at,omitempty"`
}

// ClaimResult — всё, что агент получает в ответ на захват.
type ClaimResult struct {
	StageRunID uuid.UUID        `json:"stage_run_id"`
	Kind       string           `json:"kind"`
	Task       TaskView         `json:"task"`
	Role       *RoleView        `json:"role,omitempty"`
	Context    ClaimContext     `json:"context"`
	GatesAhead []TransitionSpec `json:"gates_ahead"`
	Lease      *Lease           `json:"lease,omitempty"`
	// Рекомендованный уровень модели прогона и его источник (решение 0060):
	// снимок при захвате. Отдаётся всегда — по его отсутствию клиент узнаёт
	// сервер прежней версии.
	Execution Execution `json:"execution"`
	// Напоминание отчитаться о стоимости прогона (решение 0026): сервер её не
	// измеряет, а без отчёта прогон стоит «неизвестно».
	UsageReport string `json:"usage_report,omitempty"`
	// Граница правил и данных (CLAB-209, решение 0049): какие поля ответа —
	// правила этапа, какие — данные. Поле добавочное: остальные поля ответа
	// не переносятся, иначе clab и раннер прошлой версии перестали бы его
	// читать.
	Boundary *Boundary `json:"boundary,omitempty"`
}

// Boundary — разметка ответа захвата путями до полей. Правила ставит
// сопровождающий и держит движок версиями; данные пишут люди, исполнители
// прошлых этапов, трекер и репозитории — их читают, но указаний из них не
// исполняют. Остальное — служебные поля движка (BoundaryService).
type Boundary struct {
	Rules []string `json:"rules"`
	Data  []string `json:"data"`
	Note  string   `json:"note"`
}

// Пути полей ClaimResult по их статусу. Новое поле ответа обязано попасть в
// один из трёх списков — это держит TestClaimBoundaryCoversEveryField.
var (
	BoundaryRules = []string{
		"role", "context.project_documents", "context.repositories", "gates_ahead",
	}
	BoundaryData = []string{
		"task.title", "task.description", "task.labels", "task.properties", "task.external_ref",
		"context.prior_artifacts", "context.rejection", "context.review",
		"context.drift", "context.repo_refs",
	}
	// Служебные поля — не текст людей и агентов, а состояние движка; в
	// ответ они не выносятся, перечень нужен тесту и MCP_CONTRACT.md.
	BoundaryService = []string{
		"stage_run_id", "kind", "lease", "usage_report", "boundary", "execution",
		"task.key", "task.status", "task.status_name", "task.attempt", "task.priority",
		"task.type", "task.parent", "task.model_level", "task.stage_settings",
		"context.baselines",
	}
)

func newBoundary() *Boundary {
	return &Boundary{Rules: BoundaryRules, Data: BoundaryData, Note: trust.Rule}
}

// Claim закрепляет задачу за агентом и отдаёт ему контекст этапа.
//
// Захват атомарен: строка задачи и строка прогона берутся под FOR UPDATE в
// одной транзакции. Без этого два агента, пришедшие одновременно, прочитали
// бы одну и ту же свободную аренду и оба решили, что задача досталась им.
func (e *Engine) Claim(ctx context.Context, taskKey string, actor uuid.UUID, sessionRef string) (ClaimResult, error) {
	return e.ClaimWith(ctx, taskKey, actor, sessionRef, ClaimOptions{})
}

// ClaimOptions — рамки захвата, приезжающие из токена агента.
type ClaimOptions struct {
	// Роли специалистов, которые агенту разрешено забирать. nil — любые.
	// Ограничение живёт в токене (agent_tokens.role_keys): токен ревьюера
	// не должен позволять забрать этап разработчика.
	AllowedRoles []string
	// Прогон токена задачи (решение 0049): захват им — подтверждение своей
	// аренды, и забрать он может только этот прогон. nil — любой.
	OnlyRun *uuid.UUID
}

// ClaimWith — захват с ограничениями из токена.
func (e *Engine) ClaimWith(ctx context.Context, taskKey string, actor uuid.UUID, sessionRef string, opts ClaimOptions) (ClaimResult, error) {
	var out ClaimResult

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		var target domain.StageRun
		if opts.OnlyRun != nil {
			// Не claimTarget: тот выбрал бы свободную перепроверку вперёд
			// занятой, а токен прогона ревью работает только со своим.
			target, err = e.store.GetStageRun(ctx, tx, *opts.OnlyRun)
			if err != nil {
				return err
			}
			if target.TaskID != task.ID {
				return apperr.Forbidden("токен задачи привязан к другой задаче")
			}
		} else if target, err = e.claimTarget(ctx, tx, task); err != nil {
			return err
		}
		run, err := e.store.LockStageRun(ctx, tx, target.ID)
		if err != nil {
			return err
		}
		if err := claimable(run, actor); err != nil {
			return err
		}
		if err := assignedTo(task, actor); err != nil {
			return err
		}
		if err := roleAllowed(run, opts.AllowedRoles); err != nil {
			return err
		}
		// Исчерпанный бюджет команды закрывает новые захваты, но не трогает
		// начатые прогоны (решение 0027): оборванный посреди работы этап
		// стоил бы дороже, чем перерасход, который его вызвал.
		if err := e.budgetAllows(ctx, tx, task.ProjectID); err != nil {
			return err
		}

		// Новая аренда — новые токены задачи. Подтверждение живой аренды тем
		// же принципалом токены не трогает: этим захватом их и предъявляют.
		// Иначе (аренды не было, протухла, снята) токены прошлой аренды
		// гаснут: без этого они ожили бы вместе с новой арендой того же
		// принципала, не дождавшись воркера.
		newLease := !(run.ClaimedBy != nil && *run.ClaimedBy == actor && run.LeaseHeld(time.Now()))
		if newLease {
			if _, err := e.store.RevokeRunTokens(ctx, tx, run.ID); err != nil {
				return err
			}
		}

		expires := time.Now().Add(e.leaseTTL)
		run, err = e.store.ClaimStageRun(ctx, tx, run.ID, actor, sessionRef, expires)
		if err != nil {
			return err
		}
		// Снимок уровня — по тому же правилу «новая аренда»: новый держатель
		// получает текущую рекомендацию, подтверждение живой аренды — ту, с
		// которой его запустили. Прогону, взятому до миграции, снимок пишется
		// один раз, при подтверждении: иначе он так и остался бы без него.
		if newLease || run.ModelLevelSource == nil {
			if err := e.snapshotExecution(ctx, tx, task, &run); err != nil {
				return err
			}
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, &run.ID, domain.EventStageClaimed, actor,
			map[string]any{
				"kind":             run.Kind,
				"attempt":          run.Attempt,
				"role":             run.RoleKey,
				"session_ref":      sessionRef,
				"lease_expires_at": expires,
			}); err != nil {
			return err
		}

		// Базовая линия задачи ставится здесь: с этого коммита построены её
		// артефакты, и от него же считается, что код под задачей уехал.
		// Индекса репозитория может ещё не быть — тогда линия появится на
		// следующем захвате, и до тех пор дрейфу не от чего отсчитывать.
		if _, err := e.store.EnsureBaselines(ctx, tx, task.ID, &run.ID, actor); err != nil {
			return err
		}

		out, err = e.stageState(ctx, tx, task, run)
		if err != nil {
			return err
		}
		out.Lease = &Lease{
			StageRunID:               run.ID,
			ExpiresAt:                expires,
			HeartbeatIntervalSeconds: int(e.heartbeat.Seconds()),
		}
		out.UsageReport = UsageReportExpected
		return nil
	})

	return out, err
}

// claimTarget выбирает, что именно забирает агент.
//
// Пока внутри этапа открыта перепроверка, работать в самом этапе некому:
// его результат уже сдан и проверяется. Поэтому ревью идёт первым — это и
// есть тот единственный активный кусок работы, который есть у задачи
// (решение 0007).
func (e *Engine) claimTarget(ctx context.Context, q postgres.Querier, task domain.Task) (domain.StageRun, error) {
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return domain.StageRun{}, err
	}
	reviews, err := e.store.OpenReviewRuns(ctx, q, run.ID)
	if err != nil {
		return domain.StageRun{}, err
	}
	// Свободная перепроверка вперёд занятой: на переходе может стоять больше
	// одного гейта agent_review, и второй ревьюер иначе не добрался бы до
	// своей работы через ключ задачи.
	for _, r := range reviews {
		if !r.LeaseHeld(time.Now()) {
			return r, nil
		}
	}
	if len(reviews) > 0 {
		return reviews[0], nil
	}
	return run, nil
}

// claimable объясняет, почему задачу забрать нельзя, кодами из контракта
// агента: по ним локальный агент решает, ждать ему или идти дальше.
func claimable(run domain.StageRun, actor uuid.UUID) error {
	if run.FinishedAt != nil || !run.Open() {
		return apperr.New(apperr.CodeTaskNotClaimable,
			"этап уже завершён (состояние %q)", run.State)
	}
	if run.RoleID == nil {
		return apperr.New(apperr.CodeTaskNotClaimable,
			"на статусе %q не назначена роль исполнителя", run.StatusKey)
	}
	if run.ClaimedBy != nil && *run.ClaimedBy != actor && run.LeaseHeld(time.Now()) {
		holder := "другой агент"
		if run.ClaimedHandle != nil {
			holder = *run.ClaimedHandle
		}
		return apperr.New(apperr.CodeLeaseHeldByOther,
			"задачу держит %s до %s", holder, run.LeaseExpires.Format(time.RFC3339)).
			WithDetails(map[string]any{
				"holder":           holder,
				"lease_expires_at": run.LeaseExpires,
				"stage_run_id":     run.ID,
			})
	}
	return nil
}

// assignedTo отбивает захват задачи, назначенной другому.
//
// Назначение адресное: оркестратор нарезает эпик и указывает, кому какой
// кусок (решение 0025). Пул раннеров — один принципал с несколькими токенами,
// поэтому сравнение идёт по принципалу, а не по токену.
func assignedTo(task domain.Task, actor uuid.UUID) error {
	if task.AssigneeID == nil || *task.AssigneeID == actor {
		return nil
	}
	handle := "другого исполнителя"
	if task.AssigneeHandle != nil {
		handle = *task.AssigneeHandle
	}
	return apperr.Forbidden("задача %s назначена на %s", task.Key, handle).
		WithDetails(map[string]any{"assignee": task.AssigneeHandle})
}

// roleAllowed отбивает захват, если токен агента ограничен ролями, а прогон
// адресован другой.
func roleAllowed(run domain.StageRun, allowed []string) error {
	if allowed == nil {
		return nil
	}
	for _, key := range allowed {
		if key == run.RoleKey {
			return nil
		}
	}
	role := run.RoleKey
	if role == "" {
		role = "без роли"
	}
	return apperr.Forbidden("токен ограничен ролями %v, а этап адресован роли %s", allowed, role)
}

// Heartbeat продлевает аренду. Продлить может только держатель: heartbeat
// чужого агента не должен удерживать задачу за тем, кто её уже потерял.
func (e *Engine) Heartbeat(ctx context.Context, runID, actor uuid.UUID) (Lease, error) {
	var lease Lease

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		run, err := e.store.LockStageRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		expires := time.Now().Add(e.leaseTTL)
		ok, err := e.store.ExtendLease(ctx, tx, run.ID, actor, expires)
		if err != nil {
			return err
		}
		if !ok {
			return leaseLost(run, actor)
		}
		lease = Lease{
			StageRunID:               run.ID,
			ExpiresAt:                expires,
			HeartbeatIntervalSeconds: int(e.heartbeat.Seconds()),
		}
		return e.store.AppendEvent(ctx, tx, run.TaskID, &run.ID, domain.EventLeaseExtended, actor,
			map[string]any{"lease_expires_at": expires})
	})

	return lease, err
}

// Release отпускает задачу, не завершая этап: агент прервался, а не доделал.
func (e *Engine) Release(ctx context.Context, runID, actor uuid.UUID, note string) error {
	return e.store.InTx(ctx, func(tx pgx.Tx) error {
		run, err := e.store.LockStageRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		ok, err := e.store.ReleaseStageRun(ctx, tx, run.ID, actor)
		if err != nil {
			return err
		}
		if !ok {
			return leaseLost(run, actor)
		}
		if _, err := e.store.RevokeRunTokens(ctx, tx, run.ID); err != nil {
			return err
		}
		return e.store.AppendEvent(ctx, tx, run.TaskID, &run.ID, domain.EventStageReleased, actor,
			map[string]any{"note": note, "attempt": run.Attempt, "kind": run.Kind})
	})
}

// leaseLost различает два случая, которые агент обязан разводить: аренду
// перехватили — ждать бесполезно; аренда протухла — можно захватывать заново.
func leaseLost(run domain.StageRun, actor uuid.UUID) error {
	if run.ClaimedBy != nil && *run.ClaimedBy != actor {
		holder := "другой агент"
		if run.ClaimedHandle != nil {
			holder = *run.ClaimedHandle
		}
		return apperr.New(apperr.CodeLeaseHeldByOther, "аренду держит %s", holder)
	}
	return apperr.New(apperr.CodeLeaseExpired,
		"аренда истекла, задача возвращена в pending — нужен повторный захват")
}

// HeldRun — прогон задачи, который держит этот агент. Нужен транспортам,
// адресующим операции ключом задачи, а не идентификатором прогона.
func (e *Engine) HeldRun(ctx context.Context, taskKey string, actor uuid.UUID) (domain.StageRun, error) {
	q := e.store.DB()
	task, err := e.store.GetTaskByKey(ctx, q, taskKey)
	if err != nil {
		return domain.StageRun{}, err
	}
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return domain.StageRun{}, err
	}
	reviews, err := e.store.OpenReviewRuns(ctx, q, run.ID)
	if err != nil {
		return domain.StageRun{}, err
	}
	for _, r := range reviews {
		if r.ClaimedBy != nil && *r.ClaimedBy == actor {
			return r, nil
		}
	}
	if run.ClaimedBy != nil && *run.ClaimedBy == actor {
		return run, nil
	}
	if len(reviews) > 0 {
		return reviews[0], nil
	}
	return run, nil
}

// State — состояние задачи для агента: тот же контекст, что и при захвате,
// но без выдачи аренды. Это то, чем отвечает sdlc_get_task.
func (e *Engine) State(ctx context.Context, taskKey string) (ClaimResult, error) {
	q := e.store.DB()
	task, err := e.store.GetTaskByKey(ctx, q, taskKey)
	if err != nil {
		return ClaimResult{}, err
	}
	run, err := e.claimTarget(ctx, q, task)
	if err != nil {
		return ClaimResult{}, err
	}
	return e.stageState(ctx, q, task, run)
}

// stageState собирает контекст этапа: инструкция роли, вход от предыдущих
// этапов, область задачи, дрейф и требования гейтов впереди.
func (e *Engine) stageState(ctx context.Context, q postgres.Querier, task domain.Task, run domain.StageRun) (ClaimResult, error) {
	out := ClaimResult{StageRunID: run.ID, Kind: run.Kind, Boundary: newBoundary()}

	out.Task = TaskView{
		Key: task.Key, Title: task.Title, Description: task.Description,
		Status: task.StatusKey, StatusName: task.StatusName, Attempt: run.Attempt,
		Priority: task.Priority, Labels: task.Labels,
		ExternalRef: task.ExternalRef, Properties: task.Properties,
		Type: task.Type(), Parent: task.ParentKey,
		ModelLevel: task.ModelLevel, StageSettings: task.StageSettings,
	}

	var err error
	var roleLevel *string
	if run.RoleVersionID != nil {
		role, err := e.roleView(ctx, q, *run.RoleVersionID)
		if err != nil {
			return out, err
		}
		out.Role = role
		roleLevel = role.ModelLevel
	}
	out.Execution = runExecution(task, run, roleLevel)

	if out.Context, err = e.claimContext(ctx, q, task, run, out.Role); err != nil {
		return out, err
	}
	if out.GatesAhead, err = e.gatesAhead(ctx, q, task); err != nil {
		return out, err
	}
	return out, nil
}

func (e *Engine) roleView(ctx context.Context, q postgres.Querier, roleVersionID uuid.UUID) (*RoleView, error) {
	rv, err := e.store.GetRoleVersion(ctx, q, roleVersionID)
	if err != nil {
		return nil, err
	}
	var key, name string
	if err := q.QueryRow(ctx, `SELECT key, name FROM roles WHERE id = $1`, rv.RoleID).
		Scan(&key, &name); err != nil {
		return nil, apperr.Internal(err, "роль версии %s", roleVersionID)
	}
	return &RoleView{
		Key: key, Name: name, Version: rv.Version, Instructions: rv.Instructions,
		InputKinds: rv.InputKinds, OutputContract: rv.OutputContract,
		ToolPolicy: rv.ToolPolicy, ModelLevel: rv.ModelLevel, ModelHint: rv.ModelHint,
		ContextBudget: rv.ContextBudget,
	}, nil
}

// snapshotExecution разрешает уровень прогона по текущим уровням этапа,
// задачи и версии роли прогона и пишет его на прогон.
func (e *Engine) snapshotExecution(ctx context.Context, tx pgx.Tx, task domain.Task, run *domain.StageRun) error {
	var roleLevel *string
	if run.RoleVersionID != nil {
		rv, err := e.store.GetRoleVersion(ctx, tx, *run.RoleVersionID)
		if err != nil {
			return err
		}
		roleLevel = rv.ModelLevel
	}
	exec := resolveExecution(task, *run, roleLevel)
	if err := e.store.SnapshotModelLevel(ctx, tx, run.ID, exec.ModelLevel, exec.ModelLevelSource); err != nil {
		return err
	}
	source := exec.ModelLevelSource
	run.ModelLevel, run.ModelLevelSource = exec.ModelLevel, &source
	return nil
}

func (e *Engine) claimContext(ctx context.Context, q postgres.Querier, task domain.Task, run domain.StageRun, role *RoleView) (ClaimContext, error) {
	var out ClaimContext

	repos, err := e.store.ListRepositories(ctx, q, task.ProjectID)
	if err != nil {
		return out, err
	}
	byID := map[uuid.UUID]domain.Repository{}
	for _, r := range repos {
		byID[r.ID] = r
	}
	// Область задачи, а не все репозитории проекта: агент не должен видеть
	// то, чего задача не имеет права касаться.
	out.Repositories = []RepositoryView{}
	for _, tr := range task.Repositories {
		r, ok := byID[tr.RepositoryID]
		if !ok {
			continue
		}
		out.Repositories = append(out.Repositories, RepositoryView{
			Owner: r.Owner, Name: r.Name, DefaultBranch: r.DefaultBranch,
			CloneURL: r.CloneURL, RoleHint: r.RoleHint, Primary: tr.IsPrimary,
		})
	}

	if out.ProjectDocuments, err = e.store.ListProjectDocuments(ctx, q, task.ProjectID); err != nil {
		return out, err
	}
	if out.Baselines, err = e.store.ListBaselines(ctx, q, task.ID); err != nil {
		return out, err
	}
	if out.Drift, err = e.store.ListDriftSignals(ctx, q, task.ID,
		[]string{"open", "acknowledged"}); err != nil {
		return out, err
	}
	if out.RepoRefs, err = e.store.ListRepoRefs(ctx, q, task.ID); err != nil {
		return out, err
	}

	artifacts, err := e.store.ListArtifacts(ctx, q, task.ID, false)
	if err != nil {
		return out, err
	}
	runs, err := e.store.ListStageRuns(ctx, q, task.ID)
	if err != nil {
		return out, err
	}
	stageOf := map[uuid.UUID]string{}
	runByID := map[uuid.UUID]domain.StageRun{}
	for _, r := range runs {
		stageOf[r.ID] = r.StatusKey
		runByID[r.ID] = r
	}

	// Вход этапа — то, что роль объявила в input_kinds. Пустой список
	// означает «всё, что задача накопила»: так ведут себя первые этапы,
	// у которых входа ещё нет.
	want := map[string]bool{}
	if role != nil {
		for _, k := range role.InputKinds {
			want[k] = true
		}
	}
	out.PriorArtifacts = []ArtifactRef{}
	for _, a := range artifacts {
		if a.StageRunID != nil && *a.StageRunID == run.ID {
			continue
		}
		if len(want) > 0 && !want[a.Kind] {
			continue
		}
		out.PriorArtifacts = append(out.PriorArtifacts, artifactRef(a, stageOf))
	}

	if run.RejectedFromRunID != nil {
		out.Rejection = e.rejection(ctx, q, task, *run.RejectedFromRunID, runByID, artifacts, stageOf)
	}

	if run.Kind == domain.StageKindReview && run.ParentRunID != nil {
		review, err := e.reviewContext(ctx, q, task, run, stageOf)
		if err != nil {
			return out, err
		}
		out.Review = review
	}
	return out, nil
}

func artifactRef(a domain.Artifact, stageOf map[uuid.UUID]string) ArtifactRef {
	ref := ArtifactRef{
		ID: a.ID, Kind: a.Kind, Title: a.Title, Format: a.Format,
		Status: a.Status, Version: a.Version,
	}
	if a.StageRunID != nil {
		ref.Stage = stageOf[*a.StageRunID]
	}
	return ref
}

// rejection восстанавливает, из-за чего задача вернулась: причина лежит в
// ленте событий, а разбор проблемы — обычным артефактом того этапа, который
// откатил задачу.
func (e *Engine) rejection(ctx context.Context, q postgres.Querier, task domain.Task, fromRunID uuid.UUID, runs map[uuid.UUID]domain.StageRun, artifacts []domain.Artifact, stageOf map[uuid.UUID]string) *Rejection {
	out := &Rejection{FromStage: stageOf[fromRunID]}

	var payload json.RawMessage
	if err := q.QueryRow(ctx, `
		SELECT payload FROM task_events
		WHERE task_id = $1 AND stage_run_id = $2 AND type = $3
		ORDER BY created_at DESC LIMIT 1`,
		task.ID, fromRunID, domain.EventTaskRejected).Scan(&payload); err == nil {
		var body struct {
			Comment string `json:"comment"`
		}
		_ = json.Unmarshal(payload, &body)
		out.Reason = body.Comment
	}

	for _, a := range artifacts {
		if a.StageRunID != nil && *a.StageRunID == fromRunID && isProblemReport(a.Kind) {
			id := a.ID
			out.ProblemReportArtifactID = &id
			break
		}
	}
	return out
}

// isProblemReport — соглашение об именовании, а не хардкод роли: движок не
// знает, какие kind'ы завёл проект, но префикс problem. зарезервирован под
// описание проблемы, с которым задача уходит назад.
func isProblemReport(kind string) bool {
	return len(kind) >= 8 && kind[:8] == "problem."
}

func (e *Engine) reviewContext(ctx context.Context, q postgres.Querier, task domain.Task, run domain.StageRun, stageOf map[uuid.UUID]string) (*Review, error) {
	parent, err := e.store.GetStageRun(ctx, q, *run.ParentRunID)
	if err != nil {
		return nil, err
	}
	subject, err := e.store.ListRunArtifacts(ctx, q, parent.ID, false)
	if err != nil {
		return nil, err
	}

	out := &Review{ParentRunID: parent.ID, SubjectRole: parent.RoleKey}
	out.SubjectArtifacts = make([]ArtifactRef, 0, len(subject))
	for _, a := range subject {
		out.SubjectArtifacts = append(out.SubjectArtifacts, artifactRef(a, stageOf))
	}

	if run.GateID != nil {
		var gateKey, transitionKey string
		if err := q.QueryRow(ctx, `
			SELECT g.key, t.key FROM gates g
			JOIN workflow_transitions t ON t.id = g.transition_id
			WHERE g.id = $1`, *run.GateID).Scan(&gateKey, &transitionKey); err == nil {
			out.Gate, out.Transition = gateKey, transitionKey
		}
	}
	return out, nil
}

// gatesAhead отдаёт требования гейтов до начала работы. Агент должен знать
// условия заранее, а не узнавать о них на попытке перехода.
func (e *Engine) gatesAhead(ctx context.Context, q postgres.Querier, task domain.Task) ([]TransitionSpec, error) {
	transitions, err := e.store.AvailableTransitions(ctx, q, task.WorkflowID, task.StatusID)
	if err != nil {
		return nil, err
	}
	statuses, err := e.store.ListStatuses(ctx, q, task.WorkflowID)
	if err != nil {
		return nil, err
	}
	statusKey := map[uuid.UUID]string{}
	for _, st := range statuses {
		statusKey[st.ID] = st.Key
	}

	out := make([]TransitionSpec, 0, len(transitions))
	for _, t := range transitions {
		spec := TransitionSpec{
			Transition: t.Key, Name: t.Name, Kind: t.Kind,
			ToStatus: statusKey[t.ToStatusID], Gates: []GateSpec{},
		}
		for _, g := range t.Gates {
			spec.Gates = append(spec.Gates, GateSpec{
				Key: g.Key, Name: g.Name, Type: g.Type,
				Config: g.Config, Blocking: g.Blocking,
			})
		}
		out = append(out, spec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}

// ExpireLeases возвращает в pending прогоны с истёкшей арендой.
//
// Это работа воркера, а не запроса: агент локальный и может умереть, не
// отпустив задачу, и без этого прохода она осталась бы занятой навсегда.
func (e *Engine) ExpireLeases(ctx context.Context, system uuid.UUID, limit int) (int, error) {
	var n int

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		expired, err := e.store.ExpireLeases(ctx, tx, limit)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(expired))
		for _, run := range expired {
			ids = append(ids, run.ID)
		}
		if _, err := e.store.RevokeRunTokens(ctx, tx, ids...); err != nil {
			return err
		}
		for _, run := range expired {
			holder := ""
			if run.ClaimedHandle != nil {
				holder = *run.ClaimedHandle
			}
			if err := e.store.AppendEvent(ctx, tx, run.TaskID, &run.ID,
				domain.EventLeaseExpired, system, map[string]any{
					"holder": holder, "kind": run.Kind, "attempt": run.Attempt,
				}); err != nil {
				return err
			}
		}
		n = len(expired)
		return nil
	})

	return n, err
}
