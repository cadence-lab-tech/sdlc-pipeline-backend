// Package domain — сущности цикла. Структуры повторяют схему из migrations/:
// схема первична, здесь только её представление в Go.
//
// Статусы, роли и гейты сознательно не имеют типизированных перечислений с
// известными значениями: цикл настраивается проектом, движок работает с
// ключами как с данными.
package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type PrincipalKind string

const (
	PrincipalUser   PrincipalKind = "user"
	PrincipalAgent  PrincipalKind = "agent"
	PrincipalSystem PrincipalKind = "system"
)

type Principal struct {
	ID          uuid.UUID     `json:"id"`
	Kind        PrincipalKind `json:"kind"`
	Handle      string        `json:"handle"`
	DisplayName string        `json:"display_name"`
	Email       *string       `json:"email,omitempty"`
	// Глобальный администратор: заводит проекты и первых участников.
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}

// Уровни прав участника проекта. Это не роли специалистов (roles): «агент»
// может забирать задачи, «сопровождающий» — править цикл и ставить апрувы,
// «администратор» — управлять участниками и токенами. Порядок значим —
// каждый следующий уровень включает предыдущий.
type MemberRole string

const (
	MemberViewer     MemberRole = "viewer"
	MemberAgent      MemberRole = "agent"
	MemberMaintainer MemberRole = "maintainer"
	MemberAdmin      MemberRole = "admin"
)

// Rank отдаёт порядок уровня; неизвестный уровень ниже любого известного.
func (r MemberRole) Rank() int {
	switch r {
	case MemberViewer:
		return 1
	case MemberAgent:
		return 2
	case MemberMaintainer:
		return 3
	case MemberAdmin:
		return 4
	}
	return 0
}

func (r MemberRole) Valid() bool { return r.Rank() > 0 }

// Covers — достаточно ли уровня r для требования need.
func (r MemberRole) Covers(need MemberRole) bool { return r.Rank() >= need.Rank() }

type ProjectMember struct {
	ProjectID   uuid.UUID     `json:"project_id"`
	ProjectKey  string        `json:"project_key"`
	PrincipalID uuid.UUID     `json:"principal_id"`
	Handle      string        `json:"handle"`
	Kind        PrincipalKind `json:"kind"`
	DisplayName string        `json:"display_name"`
	Role        MemberRole    `json:"role"`
	// Роли специалистов, которые участник может забирать (решение 0030).
	// nil — любые роли проекта; пустой список — ни одной.
	RoleKeys  []string  `json:"role_keys"`
	CreatedAt time.Time `json:"created_at"`
}

// Роль в команде. Состав команды ведёт `lead`, он же заводит её проекты;
// уровень прав в проекте отсюда не выводится — его даёт участие команды в
// проекте (решение 0027).
type TeamRole string

const (
	TeamRoleMember TeamRole = "member"
	TeamRoleLead   TeamRole = "lead"
)

func (r TeamRole) Valid() bool { return r == TeamRoleMember || r == TeamRoleLead }

// Вид команды (решение 0033). Арендатор — команда: личная заводится при
// первом входе и участников не принимает, организацию заводит любой вошедший.
type TeamKind string

const (
	TeamPersonal     TeamKind = "personal"
	TeamOrganization TeamKind = "organization"
)

// Team — группа принципалов уровня инстанса. Команда участвует в проектах
// целиком, поэтому уход человека — одна строка, а не обход всех проектов.
type Team struct {
	ID          uuid.UUID `json:"id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	// Вид: личная команда одного человека или организация (решение 0033).
	// Личная помнит, чья она; превращённая в организацию — уже нет.
	Kind       TeamKind   `json:"kind"`
	PersonalOf *uuid.UUID `json:"personal_of,omitempty"`
	// Право заводить проекты принадлежит команде, а не человеку: лид
	// меняется, команда остаётся.
	CanCreateProjects bool       `json:"can_create_projects"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	ArchivedAt        *time.Time `json:"archived_at,omitempty"`
}

type TeamMember struct {
	TeamID      uuid.UUID     `json:"team_id"`
	TeamKey     string        `json:"team_key"`
	TeamKind    TeamKind      `json:"team_kind"`
	PrincipalID uuid.UUID     `json:"principal_id"`
	Handle      string        `json:"handle"`
	Kind        PrincipalKind `json:"kind"`
	DisplayName string        `json:"display_name"`
	Role        TeamRole      `json:"role"`
	CreatedAt   time.Time     `json:"created_at"`
}

// ProjectTeam — участие команды в проекте теми же уровнями, что у человека.
type ProjectTeam struct {
	ProjectID  uuid.UUID  `json:"project_id"`
	ProjectKey string     `json:"project_key"`
	TeamID     uuid.UUID  `json:"team_id"`
	TeamKey    string     `json:"team_key"`
	TeamName   string     `json:"team_name"`
	Level      MemberRole `json:"level"`
	// Роли специалистов для участников команды; nil — любые.
	RoleKeys  []string  `json:"role_keys"`
	CreatedAt time.Time `json:"created_at"`
}

// Виды токенов.
const (
	TokenAPI     = "api"
	TokenSession = "session"
)

// CLILoginRequest — запрос входа CLI через браузер (решение 0032). CLI
// показывает UserCode, человек подтверждает его в интерфейсе, CLI забирает
// токен по своему секрету; секрета в записи нет, только его хеш.
type CLILoginRequest struct {
	ID       uuid.UUID `json:"id"`
	UserCode string    `json:"user_code"`
	Label    string    `json:"label"`
	// Срок токена в днях, о котором просит CLI; 0 — бессрочно.
	TokenDays      int            `json:"token_days"`
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	ApprovedBy     *uuid.UUID     `json:"approved_by,omitempty"`
	ApprovedHandle *string        `json:"approved_handle,omitempty"`
	ApprovedKind   *PrincipalKind `json:"approved_kind,omitempty"`
	ApprovedAt     *time.Time     `json:"approved_at,omitempty"`
	DeniedAt       *time.Time     `json:"denied_at,omitempty"`
	ConsumedAt     *time.Time     `json:"consumed_at,omitempty"`
}

// Status — состояние запроса словом, как его видят CLI и страница
// подтверждения: pending, approved, denied, expired, consumed.
func (r CLILoginRequest) Status(now time.Time) string {
	switch {
	case r.ConsumedAt != nil:
		return "consumed"
	case r.DeniedAt != nil:
		return "denied"
	case r.ApprovedAt != nil:
		return "approved"
	case !now.Before(r.ExpiresAt):
		return "expired"
	default:
		return "pending"
	}
}

// TeamBudget — предел стоимости команды за окно (решение 0027). Расход —
// стоимость прогонов в проектах, которыми команда владеет.
type TeamBudget struct {
	TeamID uuid.UUID `json:"team_id"`
	// day, week или month: окно календарное, а не скользящее.
	Period    string    `json:"period"`
	LimitUSD  float64   `json:"limit_usd"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BudgetState — предел вместе с текущим расходом. Пустой Period означает, что
// предела нет или он не превышен.
type BudgetState struct {
	TeamKey  string  `json:"team_key,omitempty"`
	Period   string  `json:"period,omitempty"`
	LimitUSD float64 `json:"limit_usd,omitempty"`
	SpentUSD float64 `json:"spent_usd"`
}

// Exceeded — исчерпан ли предел.
func (b BudgetState) Exceeded() bool { return b.Period != "" && b.SpentUSD >= b.LimitUSD }

// AccessToken — предъявляемый токен. Сам токен в базе не хранится, только
// его хеш; целиком он показывается один раз при выдаче.
type AccessToken struct {
	ID          uuid.UUID     `json:"id"`
	PrincipalID uuid.UUID     `json:"principal_id"`
	Handle      string        `json:"handle"`
	Kind        PrincipalKind `json:"kind"`
	// Вид токена: api выдан руками и живёт долго, session выдан входом через
	// провайдера и живёт часы (решение 0027). Проверка прав от вида не
	// зависит — иначе в Auth появилась бы вторая ветка.
	TokenKind string `json:"token_kind"`
	// Токен, привязанный к проекту, вне него недействителен.
	ProjectID  *uuid.UUID `json:"project_id,omitempty"`
	ProjectKey *string    `json:"project_key,omitempty"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	// Роли специалистов, которые агент с этим токеном может забирать.
	// Пусто — любые роли проекта.
	RoleKeys   []string   `json:"role_keys"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type Project struct {
	ID          uuid.UUID       `json:"id"`
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Settings    json.RawMessage `json:"settings"`
	// Команда-владелец: её лид — администратор проекта по умолчанию.
	// Пусто у проектов, заведённых глобальным администратором.
	OwnerTeamID  *uuid.UUID `json:"owner_team_id,omitempty"`
	OwnerTeamKey *string    `json:"owner_team_key,omitempty"`
	// Установка GitHub App в организации проекта: ссылка, а не секрет
	// (решение 0028). Пусто — доступ идёт запасным путём.
	GitHubInstallationID *int64 `json:"github_installation_id,omitempty"`
	// Имя секрета с токеном GitHub для запасного пути; в базе имя, значение
	// приезжает из окружения.
	GitHubCredentialRef *string    `json:"github_credential_ref,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	ArchivedAt          *time.Time `json:"archived_at,omitempty"`
}

type Repository struct {
	ID            uuid.UUID `json:"id"`
	ProjectID     uuid.UUID `json:"project_id"`
	Provider      string    `json:"provider"`
	Owner         string    `json:"owner"`
	Name          string    `json:"name"`
	DefaultBranch string    `json:"default_branch"`
	CloneURL      string    `json:"clone_url"`
	Visibility    string    `json:"visibility"`
	AccessMode    string    `json:"access_mode"`
	RoleHint      *string   `json:"role_hint,omitempty"`
	// Имя секрета с токеном для этого репозитория — исключение из правила
	// проекта, побеждает любую установку. Значение живёт в окружении.
	CredentialRef *string `json:"credential_ref,omitempty"`
	// Установка приложения, которой репозиторий читается (решение 0034).
	// Пусто — учётные данные проекта или инстанса.
	InstallationID *int64 `json:"installation_id,omitempty"`
	// Репозиторий убран из установки на стороне GitHub: индексация стоит,
	// собранный индекс живёт.
	UnavailableAt *time.Time `json:"unavailable_at,omitempty"`
}

// Состояния версии цикла. Редактируется только черновик; активная версия
// иммутабельна, потому что по ней прямо сейчас идут задачи.
const (
	WorkflowDraft   = "draft"
	WorkflowActive  = "active"
	WorkflowRetired = "retired"
)

// Workflow — версия цикла целиком. Активных в проекте столько, сколько типов
// задач, но на один тип — ровно один (решение 0024).
type Workflow struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	State     string    `json:"state"`
	// Типы задач, которые ведёт этот цикл. Движок знает про тип ровно одно:
	// по нему выбирается цикл, а сам набор типов — данные проекта.
	TaskTypes   []string     `json:"task_types"`
	Statuses    []Status     `json:"statuses,omitempty"`
	Transitions []Transition `json:"transitions,omitempty"`
}

type Status struct {
	ID          uuid.UUID    `json:"id"`
	WorkflowID  uuid.UUID    `json:"workflow_id"`
	Key         string       `json:"key"`
	Name        string       `json:"name"`
	Description *string      `json:"description,omitempty"`
	Category    string       `json:"category"`
	Position    int          `json:"position"`
	IsInitial   bool         `json:"is_initial"`
	IsTerminal  bool         `json:"is_terminal"`
	WipLimit    *int         `json:"wip_limit,omitempty"`
	Color       *string      `json:"color,omitempty"`
	Roles       []StatusRole `json:"roles,omitempty"`
}

type StatusRole struct {
	RoleID    uuid.UUID `json:"role_id"`
	RoleKey   string    `json:"role_key"`
	RoleName  string    `json:"role_name"`
	IsPrimary bool      `json:"is_primary"`
	Position  int       `json:"position"`
}

// Transition. FromStatusID == nil означает «доступен из любого статуса».
type Transition struct {
	ID           uuid.UUID  `json:"id"`
	WorkflowID   uuid.UUID  `json:"workflow_id"`
	Key          string     `json:"key"`
	Name         string     `json:"name"`
	FromStatusID *uuid.UUID `json:"from_status_id"`
	ToStatusID   uuid.UUID  `json:"to_status_id"`
	Kind         string     `json:"kind"`
	Auto         bool       `json:"auto"`
	Position     int        `json:"position"`
	Gates        []Gate     `json:"gates,omitempty"`
}

// Типы гейтов. Движок не знает, что означает каждый статус, но знает набор
// проверок, которые умеет исполнять.
const (
	GateArtifactsRequired  = "artifacts_required"
	GateSchemaValidation   = "schema_validation"
	GateChecklist          = "checklist"
	GateHumanApproval      = "human_approval"
	GateAgentReview        = "agent_review"
	GateExternalCheck      = "external_check"
	GateRepoRefRequired    = "repo_ref_required"
	GateDriftClear         = "drift_clear"
	GateRepoScope          = "repo_scope"
	GateDependenciesClosed = "dependencies_closed"
)

type Gate struct {
	ID           uuid.UUID       `json:"id"`
	TransitionID uuid.UUID       `json:"transition_id"`
	Key          string          `json:"key"`
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Config       json.RawMessage `json:"config"`
	Blocking     bool            `json:"blocking"`
	Position     int             `json:"position"`
}

type Role struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Summary   *string   `json:"summary,omitempty"`
	// Роль не удаляется, а архивируется: на её версии ссылаются пройденные
	// этапы, и удаление переписало бы историю.
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	// Номер последней версии; заполняется в списках, чтобы UI не ходил
	// за версиями отдельным запросом на каждую роль.
	LatestVersion *int `json:"latest_version,omitempty"`
}

type RoleVersion struct {
	ID             uuid.UUID       `json:"id"`
	RoleID         uuid.UUID       `json:"role_id"`
	Version        int             `json:"version"`
	Instructions   string          `json:"instructions"`
	InputKinds     []string        `json:"input_kinds"`
	OutputContract json.RawMessage `json:"output_contract"`
	ToolPolicy     json.RawMessage `json:"tool_policy"`
	ModelHint      *string         `json:"model_hint,omitempty"`
	RepoScope      []string        `json:"repo_scope"`
	ContextBudget  json.RawMessage `json:"context_budget"`
	Changelog      *string         `json:"changelog,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

type Task struct {
	ID         uuid.UUID `json:"id"`
	ProjectID  uuid.UUID `json:"project_id"`
	ProjectKey string    `json:"project_key"`
	// Родительская задача: эпик для своих детей. Иерархия — ссылка, а не
	// префикс в ключе, поэтому ключи остаются плоскими (SDLC-42).
	ParentID    *uuid.UUID `json:"parent_id,omitempty"`
	ParentKey   *string    `json:"parent_key,omitempty"`
	Key         string     `json:"key"`
	Seq         int        `json:"seq"`
	Title       string     `json:"title"`
	Description *string    `json:"description,omitempty"`
	WorkflowID  uuid.UUID  `json:"workflow_id"`
	StatusID    uuid.UUID  `json:"status_id"`
	StatusKey   string     `json:"status_key"`
	StatusName  string     `json:"status_name"`
	Priority    string     `json:"priority"`
	// Адресное назначение: назначенную задачу может забрать только этот
	// принципал (решение 0025). Пусто — задача открыта любому подходящему.
	AssigneeID     *uuid.UUID      `json:"assignee_id,omitempty"`
	AssigneeHandle *string         `json:"assignee_handle,omitempty"`
	ExternalRef    json.RawMessage `json:"external_ref"`
	Labels         []string        `json:"labels"`
	// Свойства задачи (решение 0015). Системное multi_repo движок читает сам,
	// остальные отдаются агенту как контекст.
	Properties   json.RawMessage  `json:"properties"`
	Repositories []TaskRepository `json:"repositories,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	ClosedAt     *time.Time       `json:"closed_at,omitempty"`
}

// DefaultTaskType — тип задачи, если она его не называет. Набор типов
// задаёт проект (task_property_defs.type), но какой-то тип у задачи есть
// всегда: по нему выбирается цикл.
const DefaultTaskType = "task"

// Type — тип задачи: по нему задача привязана к своему циклу.
func (t Task) Type() string {
	var props struct {
		Type string `json:"type"`
	}
	if len(t.Properties) > 0 {
		_ = json.Unmarshal(t.Properties, &props)
	}
	if props.Type == "" {
		return DefaultTaskType
	}
	return props.Type
}

// MultiRepo — разрешено ли задаче охватывать несколько репозиториев.
func (t Task) MultiRepo() bool {
	var props struct {
		MultiRepo bool `json:"multi_repo"`
	}
	if len(t.Properties) == 0 {
		return false
	}
	_ = json.Unmarshal(t.Properties, &props)
	return props.MultiRepo
}

// Отношения между задачами. blocks задаёт порядок работ и читается гейтом
// dependencies_closed, остальные — контекст.
const (
	TaskLinkBlocks        = "blocks"
	TaskLinkRelatesTo     = "relates_to"
	TaskLinkDuplicates    = "duplicates"
	TaskLinkSharesSurface = "shares_surface"
	TaskLinkSplitFrom     = "split_from"
)

// TaskLinkRels — отношения, которые допускает схема.
var TaskLinkRels = []string{
	TaskLinkBlocks, TaskLinkRelatesTo, TaskLinkDuplicates,
	TaskLinkSharesSurface, TaskLinkSplitFrom,
}

func ValidTaskLinkRel(rel string) bool {
	for _, r := range TaskLinkRels {
		if r == rel {
			return true
		}
	}
	return false
}

// TaskPriorities — приоритеты, которые допускает CHECK в схеме
// (migrations/000003_tasks.up.sql). Перечень нужен наружу: правка приоритета
// обязана отбиваться понятной ошибкой раньше, чем ошибкой базы.
var TaskPriorities = []string{"low", "normal", "high", "urgent"}

func ValidTaskPriority(priority string) bool {
	for _, p := range TaskPriorities {
		if p == priority {
			return true
		}
	}
	return false
}

type TaskRepository struct {
	RepositoryID uuid.UUID `json:"repository_id"`
	Owner        string    `json:"owner"`
	Name         string    `json:"name"`
	RoleHint     *string   `json:"role_hint,omitempty"`
	IsPrimary    bool      `json:"is_primary"`
}

type TaskPropertyDef struct {
	ID            uuid.UUID       `json:"id"`
	Key           string          `json:"key"`
	Name          string          `json:"name"`
	Description   *string         `json:"description,omitempty"`
	ValueType     string          `json:"value_type"`
	DefaultValue  json.RawMessage `json:"default_value,omitempty"`
	EnumValues    []string        `json:"enum_values,omitempty"`
	IsSystem      bool            `json:"is_system"`
	ExposeToAgent bool            `json:"expose_to_agent"`
	Position      int             `json:"position"`
}

// Вид прогона. Перепроверка (гейт agent_review) идёт таким же прогоном этапа,
// но живёт внутри родительского и принадлежит гейту, который её запустил.
const (
	StageKindStage  = "stage"
	StageKindReview = "review"
)

// Состояния попытки прохождения этапа.
const (
	StagePending    = "pending"
	StageClaimed    = "claimed"
	StageInProgress = "in_progress"
	StageSubmitted  = "submitted"
	StageGating     = "gating"
	StagePassed     = "passed"
	StageFailed     = "failed"
	StageAbandoned  = "abandoned"
)

type StageRun struct {
	ID            uuid.UUID  `json:"id"`
	TaskID        uuid.UUID  `json:"task_id"`
	StatusID      uuid.UUID  `json:"status_id"`
	StatusKey     string     `json:"status_key"`
	Kind          string     `json:"kind"`
	RoleID        *uuid.UUID `json:"role_id,omitempty"`
	RoleKey       string     `json:"role_key,omitempty"`
	RoleVersionID *uuid.UUID `json:"role_version_id,omitempty"`
	Attempt       int        `json:"attempt"`
	State         string     `json:"state"`
	ClaimedBy     *uuid.UUID `json:"claimed_by,omitempty"`
	ClaimedHandle *string    `json:"claimed_by_handle,omitempty"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	LeaseExpires  *time.Time `json:"lease_expires_at,omitempty"`
	HeartbeatAt   *time.Time `json:"heartbeat_at,omitempty"`
	SessionRef    *string    `json:"agent_session_ref,omitempty"`
	// Заполнены только у прогона перепроверки: чей результат он проверяет
	// и по какому гейту запущен.
	ParentRunID *uuid.UUID `json:"parent_run_id,omitempty"`
	GateID      *uuid.UUID `json:"gate_id,omitempty"`
	// Этап, который откатил задачу сюда: из него собирается контекст
	// «из-за чего вернулись» при следующем захвате.
	RejectedFromRunID *uuid.UUID `json:"rejected_from_run_id,omitempty"`
	EnteredAt         time.Time  `json:"entered_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	Outcome           *string    `json:"outcome,omitempty"`
	Summary           *string    `json:"summary,omitempty"`
}

// LeaseHeld — держит ли прогон живую аренду на момент now.
//
// Состояние прогона здесь не смотрится намеренно: агент, сдавший артефакт
// (submitted), задачу не отпустил — он ещё будет дорабатывать результат и
// запрашивать переход. Аренда снимается явно: release, протухание, или
// уход этапа в gating.
func (r StageRun) LeaseHeld(now time.Time) bool {
	if r.ClaimedBy == nil || r.LeaseExpires == nil || r.FinishedAt != nil {
		return false
	}
	return r.LeaseExpires.After(now)
}

// Open — прогон, в котором ещё можно работать.
func (r StageRun) Open() bool {
	switch r.State {
	case StagePending, StageClaimed, StageInProgress, StageSubmitted:
		return true
	}
	return false
}

// StageRunUsage — что стоила работа агента на прогоне этапа (решение 0026).
// Числа необязательны: агент, запущенный руками, может не отчитаться, и тогда
// прогон стоит «неизвестно», а не ноль.
type StageRunUsage struct {
	ID               uuid.UUID       `json:"id"`
	RunID            uuid.UUID       `json:"run_id"`
	Model            string          `json:"model"`
	InputTokens      *int64          `json:"input_tokens,omitempty"`
	OutputTokens     *int64          `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64          `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64          `json:"cache_write_tokens,omitempty"`
	CostUSD          *float64        `json:"cost_usd,omitempty"`
	DurationMS       *int64          `json:"duration_ms,omitempty"`
	ReportedBy       *uuid.UUID      `json:"reported_by,omitempty"`
	ReportedAt       time.Time       `json:"reported_at"`
	Details          json.RawMessage `json:"details"`
}

// UsageSummary — стоимость задачи: своя и вместе с дочерними задачами.
type UsageSummary struct {
	CostUSD        float64 `json:"cost_usd"`
	SubtreeCostUSD float64 `json:"subtree_cost_usd"`
	Tokens         int64   `json:"tokens"`
	ReportedRuns   int     `json:"reported_runs"`
	// Прогоны, о которых никто не отчитался: их стоимость неизвестна.
	SilentRuns int `json:"silent_runs"`
}

// Результаты гейтов.
const (
	GateResultPending = "pending"
	GateResultPassed  = "passed"
	GateResultFailed  = "failed"
	GateResultWaived  = "waived"
	GateResultSkipped = "skipped"
)

type GateResult struct {
	ID           uuid.UUID       `json:"id"`
	StageRunID   uuid.UUID       `json:"stage_run_id"`
	GateID       uuid.UUID       `json:"gate_id"`
	TransitionID uuid.UUID       `json:"transition_id"`
	Attempt      int             `json:"attempt"`
	Status       string          `json:"status"`
	Details      json.RawMessage `json:"details"`
	DecidedBy    *uuid.UUID      `json:"decided_by,omitempty"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

const (
	ArtifactDraft      = "draft"
	ArtifactSubmitted  = "submitted"
	ArtifactApproved   = "approved"
	ArtifactRejected   = "rejected"
	ArtifactSuperseded = "superseded"
)

type Artifact struct {
	ID           uuid.UUID       `json:"id"`
	TaskID       uuid.UUID       `json:"task_id"`
	ProjectID    uuid.UUID       `json:"project_id"`
	StageRunID   *uuid.UUID      `json:"stage_run_id,omitempty"`
	Kind         string          `json:"kind"`
	Title        string          `json:"title"`
	Format       string          `json:"format"`
	BlobSHA      string          `json:"blob_sha"`
	SizeBytes    int64           `json:"size_bytes"`
	Version      int             `json:"version"`
	SupersedesID *uuid.UUID      `json:"supersedes_id,omitempty"`
	Status       string          `json:"status"`
	ProducedBy   *uuid.UUID      `json:"produced_by,omitempty"`
	ApprovedBy   *uuid.UUID      `json:"approved_by,omitempty"`
	Meta         json.RawMessage `json:"meta"`
	CreatedAt    time.Time       `json:"created_at"`
}

type TaskEvent struct {
	ID          uuid.UUID       `json:"id"`
	TaskID      uuid.UUID       `json:"task_id"`
	StageRunID  *uuid.UUID      `json:"stage_run_id,omitempty"`
	Type        string          `json:"type"`
	ActorID     *uuid.UUID      `json:"actor_id,omitempty"`
	ActorHandle *string         `json:"actor_handle,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Типы событий ленты задачи.
const (
	EventTaskCreated       = "task.created"
	EventStageEntered      = "stage.entered"
	EventArtifactSubmitted = "artifact.submitted"
	EventGateDecided       = "gate.decided"
	EventGateFailed        = "gate.failed"
	EventTaskAdvanced      = "task.advanced"
	EventTaskRejected      = "task.rejected"
	EventTaskCancelled     = "task.cancelled"
	EventStageClaimed      = "stage.claimed"
	EventStageReleased     = "stage.released"
	EventLeaseExtended     = "lease.extended"
	EventLeaseExpired      = "lease.expired"
	EventReviewRequested   = "review.requested"
	EventReviewCompleted   = "review.completed"
	EventRepoRefLinked     = "repo_ref.linked"
	EventDriftDetected     = "drift.detected"
	EventDriftResolved     = "drift.resolved"
	EventUsageReported     = "usage.reported"
	EventTaskUpdated       = "task.updated"
	// Задача из бэклога переехала на новую версию цикла при активации.
	EventTaskWorkflowMoved = "task.workflow_moved"
)

// TeamEvent — событие аудита уровня арендатора. Отдельная лента нужна там, где
// следу негде остаться на задаче: удалённый проект уносит свои task_events с
// собой, а команда-владелец остаётся.
type TeamEvent struct {
	ID          uuid.UUID       `json:"id"`
	TeamID      uuid.UUID       `json:"team_id"`
	Type        string          `json:"type"`
	ActorID     *uuid.UUID      `json:"actor_id,omitempty"`
	ActorHandle *string         `json:"actor_handle,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Типы событий ленты команды.
const (
	EventProjectDeleted = "project.deleted"
)

// ProjectDocument — материал проекта вне задач: API-registry, глоссарий,
// схема взаимодействия. Едет агенту в контексте захвата списком.
type ProjectDocument struct {
	ID         uuid.UUID       `json:"id"`
	ProjectID  uuid.UUID       `json:"project_id"`
	Kind       string          `json:"kind"`
	Title      string          `json:"title"`
	Source     string          `json:"source"`
	URL        *string         `json:"url,omitempty"`
	Repository *string         `json:"repository,omitempty"`
	RepoPath   *string         `json:"repo_path,omitempty"`
	BlobSHA    *string         `json:"blob_sha,omitempty"`
	Meta       json.RawMessage `json:"meta"`
}

// Тариф — набор пределов (решение 0033). nil — без предела. Команда без
// подписки живёт на free; квота проверяется рядом с правами, а не в домене.
type Plan struct {
	Code       string    `json:"code"`
	Name       string    `json:"name"`
	MembersMax *int      `json:"members_max,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Subscription — активная подписка команды на тариф. Одна активная на
// команду; смена тарифа закрывает прежнюю и заводит новую.
type Subscription struct {
	ID          uuid.UUID  `json:"id"`
	TeamID      uuid.UUID  `json:"team_id"`
	PlanCode    string     `json:"plan_code"`
	Status      string     `json:"status"`
	PeriodStart *time.Time `json:"period_start,omitempty"`
	PeriodEnd   *time.Time `json:"period_end,omitempty"`
	ExternalRef *string    `json:"external_ref,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// GitHubInstallation — установка GitHub App на аккаунт человека или
// организации (решение 0034). Принадлежит команде-арендатору; секретов не
// несёт — токен по ней выписывает ключ приложения.
type GitHubInstallation struct {
	ID             uuid.UUID  `json:"id"`
	InstallationID int64      `json:"installation_id"`
	AccountLogin   string     `json:"account_login"`
	AccountID      int64      `json:"account_id"`
	AccountType    string     `json:"account_type"`
	OwnerTeamID    uuid.UUID  `json:"owner_team_id"`
	OwnerTeamKey   string     `json:"owner_team_key"`
	ConnectedBy    uuid.UUID  `json:"connected_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	SuspendedAt    *time.Time `json:"suspended_at,omitempty"`
	RemovedAt      *time.Time `json:"removed_at,omitempty"`
}

// Active — установка живая: не снята и не приостановлена на стороне GitHub.
func (i GitHubInstallation) Active() bool { return i.SuspendedAt == nil && i.RemovedAt == nil }
