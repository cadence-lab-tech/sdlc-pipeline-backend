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
	CreatedAt   time.Time     `json:"created_at"`
}

type Project struct {
	ID          uuid.UUID       `json:"id"`
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Settings    json.RawMessage `json:"settings"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ArchivedAt  *time.Time      `json:"archived_at,omitempty"`
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
}

// Workflow — версия цикла целиком. Активная ровно одна на проект.
type Workflow struct {
	ID          uuid.UUID    `json:"id"`
	ProjectID   uuid.UUID    `json:"project_id"`
	Name        string       `json:"name"`
	Version     int          `json:"version"`
	State       string       `json:"state"`
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
	GateArtifactsRequired = "artifacts_required"
	GateSchemaValidation  = "schema_validation"
	GateChecklist         = "checklist"
	GateHumanApproval     = "human_approval"
	GateAgentReview       = "agent_review"
	GateExternalCheck     = "external_check"
	GateRepoRefRequired   = "repo_ref_required"
	GateDriftClear        = "drift_clear"
	GateRepoScope         = "repo_scope"
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
}

type Task struct {
	ID          uuid.UUID       `json:"id"`
	ProjectID   uuid.UUID       `json:"project_id"`
	ProjectKey  string          `json:"project_key"`
	EpicID      *uuid.UUID      `json:"epic_id,omitempty"`
	Key         string          `json:"key"`
	Seq         int             `json:"seq"`
	Title       string          `json:"title"`
	Description *string         `json:"description,omitempty"`
	WorkflowID  uuid.UUID       `json:"workflow_id"`
	StatusID    uuid.UUID       `json:"status_id"`
	StatusKey   string          `json:"status_key"`
	StatusName  string          `json:"status_name"`
	Priority    string          `json:"priority"`
	AssigneeID  *uuid.UUID      `json:"assignee_id,omitempty"`
	ExternalRef json.RawMessage `json:"external_ref"`
	Labels      []string        `json:"labels"`
	// Свойства задачи (решение 0015). Системное multi_repo движок читает сам,
	// остальные отдаются агенту как контекст.
	Properties   json.RawMessage  `json:"properties"`
	Repositories []TaskRepository `json:"repositories,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	ClosedAt     *time.Time       `json:"closed_at,omitempty"`
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
	RoleID        *uuid.UUID `json:"role_id,omitempty"`
	RoleVersionID *uuid.UUID `json:"role_version_id,omitempty"`
	Attempt       int        `json:"attempt"`
	State         string     `json:"state"`
	ClaimedBy     *uuid.UUID `json:"claimed_by,omitempty"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	LeaseExpires  *time.Time `json:"lease_expires_at,omitempty"`
	EnteredAt     time.Time  `json:"entered_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Outcome       *string    `json:"outcome,omitempty"`
	Summary       *string    `json:"summary,omitempty"`
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
)
