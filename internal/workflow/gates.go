package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// EvalInput — всё, что нужно гейту для вердикта.
//
// Гейт не знает, на каком статусе стоит задача и как называется роль: он видит
// только свою конфигурацию и состояние задачи. Иначе типы гейтов начали бы
// зависеть от конкретного цикла, а цикл — это данные проекта.
type EvalInput struct {
	Task       domain.Task
	StageRun   domain.StageRun
	Transition domain.Transition
	Gate       domain.Gate
	// Артефакты, сданные в текущей попытке этапа.
	StageArtifacts []domain.Artifact
	// Актуальные артефакты задачи целиком.
	TaskArtifacts []domain.Artifact
	// Записанное решение по гейту, если человек или агент его выносил.
	Decision *domain.GateResult
}

// GateOutcome — результат одного гейта.
type GateOutcome struct {
	GateKey  string         `json:"gate_key"`
	GateName string         `json:"gate_name"`
	Type     string         `json:"type"`
	Blocking bool           `json:"blocking"`
	Status   string         `json:"status"`
	Reason   string         `json:"reason,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

// Passed — прошёл ли гейт настолько, чтобы не держать переход.
// waived и skipped переход не держат: первое — осознанное решение человека,
// второе — гейт, неприменимый в текущей фазе.
func (o GateOutcome) Passed() bool {
	switch o.Status {
	case domain.GateResultPassed, domain.GateResultWaived, domain.GateResultSkipped:
		return true
	}
	return false
}

type evaluator func(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error)

func (e *Engine) registerEvaluators() {
	e.evaluators = map[string]evaluator{
		domain.GateArtifactsRequired:  e.evalArtifactsRequired,
		domain.GateChecklist:          e.evalChecklist,
		domain.GateHumanApproval:      e.evalHumanApproval,
		domain.GateRepoRefRequired:    e.evalRepoRefRequired,
		domain.GateRepoScope:          e.evalRepoScope,
		domain.GateDriftClear:         e.evalDriftClear,
		domain.GateSchemaValidation:   e.evalSchemaValidation,
		domain.GateAgentReview:        e.evalAgentReview,
		domain.GateExternalCheck:      e.evalExternalCheck,
		domain.GateDependenciesClosed: e.evalDependenciesClosed,
	}
}

func base(g domain.Gate, status, reason string) GateOutcome {
	return GateOutcome{
		GateKey: g.Key, GateName: g.Name, Type: g.Type,
		Blocking: g.Blocking, Status: status, Reason: reason,
	}
}

// ── artifacts_required ──────────────────────────────────────────────────────

type artifactsRequiredConfig struct {
	Kinds []string `json:"kinds"`
	// Требуемый статус артефакта: пусто — любой актуальный.
	Status string `json:"status"`
	// "stage" (по умолчанию) — артефакт должен быть сдан в текущей попытке;
	// "task" — годится любой актуальный артефакт задачи.
	//
	// По умолчанию именно stage: после отката задача заходит на статус заново,
	// и артефакты прошлой попытки не должны закрывать гейт вместо работы.
	Scope string `json:"scope"`
}

func (e *Engine) evalArtifactsRequired(_ context.Context, _ postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg artifactsRequiredConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	if len(cfg.Kinds) == 0 {
		return base(in.Gate, domain.GateResultPassed, "список требуемых артефактов пуст"), nil
	}

	pool := in.StageArtifacts
	if cfg.Scope == "task" {
		pool = in.TaskArtifacts
	}

	present := map[string]bool{}
	for _, a := range pool {
		if cfg.Status != "" && a.Status != cfg.Status {
			continue
		}
		present[a.Kind] = true
	}

	var missing []string
	for _, kind := range cfg.Kinds {
		if !present[kind] {
			missing = append(missing, kind)
		}
	}

	if len(missing) == 0 {
		out := base(in.Gate, domain.GateResultPassed, "")
		out.Details = map[string]any{"required": cfg.Kinds}
		return out, nil
	}

	out := base(in.Gate, domain.GateResultFailed,
		"не сданы артефакты: "+strings.Join(missing, ", "))
	out.Details = map[string]any{"required": cfg.Kinds, "missing": missing}
	return out, nil
}

// ── checklist ───────────────────────────────────────────────────────────────

type checklistConfig struct {
	Items []struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Required bool   `json:"required"`
	} `json:"items"`
}

func (e *Engine) evalChecklist(_ context.Context, _ postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg checklistConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}

	checked := map[string]bool{}
	if in.Decision != nil {
		var d struct {
			Checked []string `json:"checked"`
		}
		_ = json.Unmarshal(in.Decision.Details, &d)
		for _, id := range d.Checked {
			checked[id] = true
		}
	}

	var missing []map[string]string
	for _, item := range cfg.Items {
		if item.Required && !checked[item.ID] {
			missing = append(missing, map[string]string{"id": item.ID, "text": item.Text})
		}
	}

	if len(missing) == 0 {
		return base(in.Gate, domain.GateResultPassed, ""), nil
	}

	out := base(in.Gate, domain.GateResultPending,
		fmt.Sprintf("не отмечено пунктов чек-листа: %d", len(missing)))
	out.Details = map[string]any{"unchecked": missing}
	return out, nil
}

// ── human_approval ──────────────────────────────────────────────────────────

type humanApprovalConfig struct {
	MinApprovals int `json:"min_approvals"`
}

func (e *Engine) evalHumanApproval(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg humanApprovalConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	if cfg.MinApprovals <= 0 {
		cfg.MinApprovals = 1
	}

	if in.Decision != nil && in.Decision.Status == domain.GateResultFailed {
		out := base(in.Gate, domain.GateResultFailed, "человек отклонил этап")
		out.Details = map[string]any{"decision": json.RawMessage(in.Decision.Details)}
		return out, nil
	}

	// Считаются только апрувы после последней сдачи на этапе: человек
	// утверждает то, что видел, а не то, что сдадут потом (решение 0042).
	since := latestSubmission(in.StageArtifacts)
	got, err := e.store.CountApprovals(ctx, q, in.StageRun.ID, in.Gate.ID, since)
	if err != nil {
		return GateOutcome{}, err
	}

	out := base(in.Gate, domain.GateResultPending,
		fmt.Sprintf("апрувов %d из %d", got, cfg.MinApprovals))
	if got >= cfg.MinApprovals {
		out = base(in.Gate, domain.GateResultPassed, "")
	}
	details := map[string]any{"approvals": got, "min_approvals": cfg.MinApprovals}
	if got < cfg.MinApprovals && !since.IsZero() {
		ever, err := e.store.CountApprovals(ctx, q, in.StageRun.ID, in.Gate.ID, time.Time{})
		if err != nil {
			return GateOutcome{}, err
		}
		if stale := ever - got; stale > 0 {
			out.Reason += fmt.Sprintf("; апрувов до последней сдачи артефактов: %d, они устарели", stale)
			details["stale_approvals"] = stale
		}
	}
	out.Details = details
	return out, nil
}

// latestSubmission — момент последней сдачи на этапе; нулевое время, если
// на этапе ничего не сдавали.
func latestSubmission(artifacts []domain.Artifact) time.Time {
	var latest time.Time
	for _, a := range artifacts {
		if a.CreatedAt.After(latest) {
			latest = a.CreatedAt
		}
	}
	return latest
}

// ── repo_ref_required ───────────────────────────────────────────────────────

type repoRefConfig struct {
	RefType string `json:"ref_type"`
	// MinCount считается на репозиторий при PerRepository, иначе на задачу.
	MinCount      int  `json:"min_count"`
	RequireOpen   bool `json:"require_open"`
	PerRepository bool `json:"per_repository"`
}

// evalRepoRefRequired проверяет, что к задаче привязан PR (или ветка/коммит).
//
// Код изменений живёт в PR, а не в артефакте (решение 0005), поэтому у
// мультирепозиторной задачи PR нужен в каждом репозитории её области — иначе
// половина работы уедет дальше по циклу без следа.
func (e *Engine) evalRepoRefRequired(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg repoRefConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	if cfg.RefType == "" {
		cfg.RefType = "pull_request"
	}
	if cfg.MinCount <= 0 {
		cfg.MinCount = 1
	}

	// Состояние PR подтягивает синхронизация с GitHub (фаза 3). Пока оно не
	// заполнено, require_open отбивает только заведомо закрытое — иначе гейт
	// блокировал бы задачу за отсутствие данных, а не за отсутствие PR.
	rows, err := q.Query(ctx, `
		SELECT r.owner || '/' || r.name AS repo, count(rr.id)
		FROM task_repositories tr
		JOIN repositories r ON r.id = tr.repository_id
		LEFT JOIN task_repo_refs rr
		       ON rr.task_id = tr.task_id
		      AND rr.repository_id = tr.repository_id
		      AND rr.ref_type = $2
		      AND (NOT $3 OR rr.state IS NULL
		           OR rr.state NOT IN ('closed', 'merged', 'declined'))
		WHERE tr.task_id = $1
		GROUP BY 1 ORDER BY 1`, in.Task.ID, cfg.RefType, cfg.RequireOpen)
	if err != nil {
		return GateOutcome{}, err
	}
	defer rows.Close()

	counts := map[string]int{}
	total := 0
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			return GateOutcome{}, err
		}
		counts[repo] = n
		total += n
	}
	if err := rows.Err(); err != nil {
		return GateOutcome{}, err
	}

	if len(counts) == 0 {
		out := base(in.Gate, domain.GateResultFailed, "у задачи не объявлена область репозиториев")
		return out, nil
	}

	var missing []string
	if cfg.PerRepository {
		for repo, n := range counts {
			if n < cfg.MinCount {
				missing = append(missing, repo)
			}
		}
		sort.Strings(missing)
	} else if total < cfg.MinCount {
		missing = append(missing, "задача целиком")
	}

	details := map[string]any{"ref_type": cfg.RefType, "per_repository": counts}
	if len(missing) == 0 {
		out := base(in.Gate, domain.GateResultPassed, "")
		out.Details = details
		return out, nil
	}

	out := base(in.Gate, domain.GateResultFailed,
		fmt.Sprintf("нет %s в: %s", cfg.RefType, strings.Join(missing, ", ")))
	details["missing"] = missing
	out.Details = details
	return out, nil
}

// ── repo_scope ──────────────────────────────────────────────────────────────

type repoScopeConfig struct {
	// Для мультирепозиторной задачи выход за область — повод расширить её,
	// для однорепозиторной — повод завести отдельную задачу. Переход гейт
	// держит в обоих случаях: расширение области решает человек, не агент.
	AllowExpansionWhenMultiRepo bool `json:"allow_expansion_when_multi_repo"`
	// Требовать, чтобы область вообще была объявлена. По умолчанию да:
	// без области детектору дрейфа не за чем следить.
	RequireScope *bool `json:"require_scope"`
}

func (e *Engine) evalRepoScope(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg repoScopeConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	requireScope := cfg.RequireScope == nil || *cfg.RequireScope

	if requireScope && len(in.Task.Repositories) == 0 {
		return base(in.Gate, domain.GateResultFailed,
			"область задачи не объявлена: не указан ни один репозиторий"), nil
	}

	rows, err := q.Query(ctx, `
		SELECT DISTINCT r.owner || '/' || r.name
		FROM artifact_code_refs acr
		JOIN artifacts a    ON a.id = acr.artifact_id
		JOIN repositories r ON r.id = acr.repository_id
		WHERE acr.task_id = $1
		  AND a.status <> 'superseded'
		  AND NOT EXISTS (
			SELECT 1 FROM task_repositories tr
			WHERE tr.task_id = acr.task_id AND tr.repository_id = acr.repository_id)
		ORDER BY 1`, in.Task.ID)
	if err != nil {
		return GateOutcome{}, err
	}
	defer rows.Close()

	var outside []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return GateOutcome{}, err
		}
		outside = append(outside, repo)
	}
	if err := rows.Err(); err != nil {
		return GateOutcome{}, err
	}

	scope := make([]string, 0, len(in.Task.Repositories))
	for _, r := range in.Task.Repositories {
		scope = append(scope, r.Owner+"/"+r.Name)
	}

	if len(outside) == 0 {
		out := base(in.Gate, domain.GateResultPassed, "")
		out.Details = map[string]any{"scope": scope, "multi_repo": in.Task.MultiRepo()}
		return out, nil
	}

	remedy := "задача однорепозиторная: вынесите работу по этим репозиториям в отдельную задачу"
	if in.Task.MultiRepo() && cfg.AllowExpansionWhenMultiRepo {
		remedy = "расширьте область задачи, добавив эти репозитории"
	}

	out := base(in.Gate, domain.GateResultFailed,
		fmt.Sprintf("артефакты ссылаются на репозитории вне области (%s); %s",
			strings.Join(outside, ", "), remedy))
	out.Details = map[string]any{
		"scope": scope, "outside_scope": outside, "multi_repo": in.Task.MultiRepo(),
	}
	return out, nil
}

// ── drift_clear ─────────────────────────────────────────────────────────────

type driftClearConfig struct {
	MinSeverity  string   `json:"min_severity"`
	IncludeKinds []string `json:"include_kinds"`
}

// severityAtLeast возвращает уровни, которые считаются блокирующими.
// Порог тем строже, чем ближе этап к коду: аналитику двигают только серьёзные
// расхождения, разработку — любые.
func severityAtLeast(min string) []string {
	switch min {
	case "high":
		return []string{"high"}
	case "medium":
		return []string{"medium", "high"}
	default:
		return []string{"low", "medium", "high"}
	}
}

func (e *Engine) evalDriftClear(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg driftClearConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	kinds := cfg.IncludeKinds
	if kinds == nil {
		kinds = []string{}
	}

	rows, err := q.Query(ctx, `
		SELECT kind, severity, count(*)
		FROM task_drift_signals
		WHERE task_id = $1
		  AND state IN ('open', 'acknowledged')
		  AND severity = ANY($2)
		  AND (cardinality($3::text[]) = 0 OR kind = ANY($3))
		GROUP BY kind, severity
		ORDER BY kind, severity`,
		in.Task.ID, severityAtLeast(cfg.MinSeverity), kinds)
	if err != nil {
		return GateOutcome{}, err
	}
	defer rows.Close()

	signals := []map[string]any{}
	total := 0
	for rows.Next() {
		var kind, severity string
		var n int
		if err := rows.Scan(&kind, &severity, &n); err != nil {
			return GateOutcome{}, err
		}
		signals = append(signals, map[string]any{"kind": kind, "severity": severity, "count": n})
		total += n
	}
	if err := rows.Err(); err != nil {
		return GateOutcome{}, err
	}

	details := map[string]any{"min_severity": cfg.MinSeverity, "signals": signals}
	if total == 0 {
		out := base(in.Gate, domain.GateResultPassed, "")
		out.Details = details
		return out, nil
	}

	out := base(in.Gate, domain.GateResultFailed,
		fmt.Sprintf("открытых расхождений: %d — разберите их и переставьте базовую линию", total))
	out.Details = details
	return out, nil
}

// ── external_check ──────────────────────────────────────────────────────────

type externalCheckConfig struct {
	// Пока провайдер один; поле оставлено, чтобы конфигурация гейта не
	// переписывалась, когда появится второй.
	Provider string `json:"provider"`
	// Какое заключение считается успешным. По умолчанию success.
	RequiredConclusion string `json:"required_conclusion"`
	// Имена конкретных проверок. Пусто — годится общий вердикт по коммиту.
	Contexts []string `json:"contexts"`
	RefType  string   `json:"ref_type"`
}

// checksSnapshot — состояние проверок, принесённое воркером в task_repo_refs.
type checksSnapshot struct {
	Conclusion string `json:"conclusion"`
	Runs       []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"runs"`
}

// evalExternalCheck проверяет CI на привязанном к задаче PR.
//
// Гейт не ходит в сеть: он исполняется внутри транзакции перехода вместе с
// остальными, и сетевой вызов означал бы открытую транзакцию на время
// таймаута чужого API. Состояние приносит воркер, гейт читает готовое —
// поэтому «данных ещё нет» здесь честно отдаётся как pending, а не как
// провал: непришедший ответ GitHub не является отрицательным заключением.
func (e *Engine) evalExternalCheck(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg externalCheckConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	if cfg.RequiredConclusion == "" {
		cfg.RequiredConclusion = "success"
	}
	if cfg.RefType == "" {
		cfg.RefType = "pull_request"
	}

	rows, err := q.Query(ctx, `
		SELECT r.owner || '/' || r.name, rr.ref, coalesce(rr.head_sha, ''),
		       rr.checks, rr.synced_at IS NOT NULL
		FROM task_repo_refs rr
		JOIN repositories r ON r.id = rr.repository_id
		WHERE rr.task_id = $1 AND rr.ref_type = $2
		  AND (rr.state IS NULL OR rr.state NOT IN ('closed', 'declined'))
		ORDER BY 1, rr.ref`, in.Task.ID, cfg.RefType)
	if err != nil {
		return GateOutcome{}, err
	}
	defer rows.Close()

	type refState struct {
		repo, ref, head string
		checks          checksSnapshot
		synced          bool
	}
	var refs []refState
	for rows.Next() {
		var r refState
		var raw []byte
		if err := rows.Scan(&r.repo, &r.ref, &r.head, &raw, &r.synced); err != nil {
			return GateOutcome{}, err
		}
		_ = json.Unmarshal(raw, &r.checks)
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		return GateOutcome{}, err
	}

	if len(refs) == 0 {
		out := base(in.Gate, domain.GateResultFailed,
			"к задаче не привязан "+cfg.RefType+", проверять нечего")
		return out, nil
	}

	details := map[string]any{"required_conclusion": cfg.RequiredConclusion}
	var pending, failed []string
	perRef := []map[string]any{}

	for _, r := range refs {
		item := map[string]any{"repository": r.repo, "ref": r.ref, "commit": r.head}
		switch {
		case !r.synced:
			item["state"] = "unknown"
			pending = append(pending, r.repo+"#"+r.ref+" — состояние проверок ещё не приезжало")
		case len(cfg.Contexts) > 0:
			byName := map[string]string{}
			status := map[string]string{}
			for _, run := range r.checks.Runs {
				byName[run.Name] = run.Conclusion
				status[run.Name] = run.Status
			}
			for _, name := range cfg.Contexts {
				conclusion, ok := byName[name]
				switch {
				case !ok:
					pending = append(pending, r.repo+"#"+r.ref+": проверка "+name+" не запускалась")
				case status[name] != "completed":
					pending = append(pending, r.repo+"#"+r.ref+": проверка "+name+" ещё идёт")
				case conclusion != cfg.RequiredConclusion:
					failed = append(failed, r.repo+"#"+r.ref+": "+name+" — "+conclusion)
				}
			}
			item["checks"] = byName
		case r.checks.Conclusion == "":
			item["state"] = "pending"
			pending = append(pending, r.repo+"#"+r.ref+" — проверки ещё идут")
		case r.checks.Conclusion != cfg.RequiredConclusion:
			item["state"] = r.checks.Conclusion
			failed = append(failed, r.repo+"#"+r.ref+" — "+r.checks.Conclusion)
		default:
			item["state"] = r.checks.Conclusion
		}
		perRef = append(perRef, item)
	}
	details["refs"] = perRef

	switch {
	case len(failed) > 0:
		out := base(in.Gate, domain.GateResultFailed, "проверки не прошли: "+strings.Join(failed, "; "))
		details["failed"] = failed
		out.Details = details
		return out, nil
	case len(pending) > 0:
		out := base(in.Gate, domain.GateResultPending, strings.Join(pending, "; "))
		details["pending"] = pending
		out.Details = details
		return out, nil
	default:
		out := base(in.Gate, domain.GateResultPassed, "")
		out.Details = details
		return out, nil
	}
}

// ── dependencies_closed ─────────────────────────────────────────────────────

type dependenciesConfig struct {
	// children — дочерние задачи (всё поддерево), blockers — задачи, которые
	// блокируют эту, both — и те и другие. По умолчанию children: гейт заводят
	// прежде всего на эпик.
	Scope string `json:"scope"`
	// Считать ли отменённую задачу закрытой. По умолчанию нет: отменённый
	// кусок эпика — повод посмотреть, не потерялась ли работа.
	AllowCancelled bool `json:"allow_cancelled"`
}

// evalDependenciesClosed держит переход, пока не закрыто то, от чего задача
// зависит: дети эпика или блокирующие задачи.
//
// Судит по категории статуса, а не по его ключу: какие статусы завёл проект,
// движок не знает, а категория `done` есть у любого цикла — её требует
// проверка цикла при активации.
func (e *Engine) evalDependenciesClosed(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg dependenciesConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	children := cfg.Scope != "blockers"
	blockers := cfg.Scope == "blockers" || cfg.Scope == "both"

	deps, err := e.store.TaskDependencies(ctx, q, in.Task.ID, children, blockers)
	if err != nil {
		return GateOutcome{}, err
	}

	open := []map[string]any{}
	closed := 0
	for _, d := range deps {
		if d.Category == "done" || (cfg.AllowCancelled && d.Category == "cancelled") {
			closed++
			continue
		}
		open = append(open, map[string]any{
			"key": d.Key, "title": d.Title, "status": d.StatusKey, "relation": d.Relation,
		})
	}

	details := map[string]any{
		"scope": cfg.Scope, "total": len(deps), "closed": closed, "open": open,
	}
	if len(open) == 0 {
		out := base(in.Gate, domain.GateResultPassed, "")
		out.Details = details
		return out, nil
	}

	keys := make([]string, 0, len(open))
	for _, d := range open {
		keys = append(keys, d["key"].(string))
	}
	sort.Strings(keys)
	out := base(in.Gate, domain.GateResultFailed,
		"не закрыты задачи: "+strings.Join(keys, ", "))
	out.Details = details
	return out, nil
}
