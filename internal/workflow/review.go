// Гейты фазы 2: перепроверка другой ролью и проверка содержимого артефакта.
//
// Оба гейта работают только с конфигурацией и данными задачи: имя роли
// ревьюера и kind артефакта приезжают из настроек цикла, движок про
// «ревьюера» и «вердикт» ничего не знает.
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// ── agent_review ────────────────────────────────────────────────────────────

type agentReviewConfig struct {
	// Кого зовут проверять. Роль обязана отличаться от исполнителя этапа —
	// иначе перепроверка вырождается в «агент проверяет сам себя».
	ReviewerRoleKey string `json:"reviewer_role_key"`
	// Чем ревьюер отчитывается и что считается «прошло».
	ArtifactKind string `json:"artifact_kind"`
	PassOn       string `json:"pass_on"`
	VerdictField string `json:"verdict_field"`
}

func (c *agentReviewConfig) defaults() {
	if c.ArtifactKind == "" {
		c.ArtifactKind = "review.verdict"
	}
	if c.PassOn == "" {
		c.PassOn = "pass"
	}
	if c.VerdictField == "" {
		c.VerdictField = "verdict"
	}
}

// evalAgentReview — состояние перепроверки. Гейт ничего не запускает: он
// только смотрит. Прогон ревьюера заводит Execute, потому что это побочный
// эффект, а Preview обязан оставаться чистой проверкой.
func (e *Engine) evalAgentReview(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg agentReviewConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	cfg.defaults()

	if cfg.ReviewerRoleKey == "" {
		return base(in.Gate, domain.GateResultFailed,
			"в конфигурации гейта не указан reviewer_role_key"), nil
	}

	review, err := e.store.LatestReviewRun(ctx, q, in.StageRun.ID, in.Gate.ID)
	if err != nil {
		return GateOutcome{}, err
	}
	if review == nil {
		out := base(in.Gate, domain.GateResultPending, "перепроверка не запускалась")
		out.Details = map[string]any{"reviewer_role": cfg.ReviewerRoleKey}
		return out, nil
	}

	details := map[string]any{
		"reviewer_role":  cfg.ReviewerRoleKey,
		"review_run_id":  review.ID,
		"review_state":   review.State,
		"review_attempt": review.Attempt,
	}

	if review.FinishedAt == nil {
		reason := "перепроверка запущена, ревьюер ещё не сдал вердикт"
		if review.ClaimedHandle != nil {
			reason = "перепроверку ведёт " + *review.ClaimedHandle
		}
		out := base(in.Gate, domain.GateResultPending, reason)
		out.Details = details
		return out, nil
	}

	// Исполнитель доработал результат после вердикта — вердикт вынесен о том,
	// чего уже нет. Гейт возвращается в pending, и следующий запрос перехода
	// заведёт новую попытку перепроверки — если предел перепроверок ещё не
	// выбран.
	stale := reviewStale(*review, in.StageArtifacts)
	if stale {
		details["stale"] = true
	}
	limit, failed, err := e.reviewLimit(ctx, q, in.Gate, in.StageRun.ID)
	if err != nil {
		return GateOutcome{}, err
	}
	reached := limit > 0 && failed >= limit
	if stale && !reached {
		out := base(in.Gate, domain.GateResultPending,
			"результат доработан после перепроверки — нужна повторная")
		out.Details = details
		return out, nil
	}

	verdict, findings, err := e.reviewVerdict(ctx, q, *review, cfg)
	if err != nil {
		return GateOutcome{}, err
	}
	details["verdict"] = verdict
	if findings != nil {
		details["findings"] = findings
	}

	// Предел выбран, и последний вердикт не pass: новой перепроверки не будет
	// ни на старом результате, ни на доработанном. Вердикт failed, а не
	// pending — агенту закрывать нечего, следующий круг даёт человек.
	if reached && verdict != cfg.PassOn {
		details["failed_reviews"] = failed
		details["max_attempts"] = limit
		out := base(in.Gate, domain.GateResultFailed, fmt.Sprintf(
			"перепроверок %d из %d; новую не заведёт, снять может человек — решение waived по этому гейту",
			failed, limit))
		out.Details = details
		return out, nil
	}
	if stale {
		out := base(in.Gate, domain.GateResultPending,
			"результат доработан после перепроверки — нужна повторная")
		out.Details = details
		return out, nil
	}

	if verdict == "" {
		out := base(in.Gate, domain.GateResultFailed,
			"перепроверка закрыта без артефакта "+cfg.ArtifactKind)
		out.Details = details
		return out, nil
	}
	if verdict != cfg.PassOn {
		out := base(in.Gate, domain.GateResultFailed,
			fmt.Sprintf("вердикт ревьюера: %s", verdict))
		out.Details = details
		return out, nil
	}

	out := base(in.Gate, domain.GateResultPassed, "")
	out.Details = details
	return out, nil
}

// reviewLimit — предел перепроверок гейта (config.max_attempts) и сколько
// их провалено на этом родительском прогоне. limit 0 — предела нет: не задан
// или задан неверно. Валидатор негодный предел не активирует, а если он всё
// же в активном цикле, гейт ведёт себя как без него, а не падает. Разбор тот
// же maxAttempts, что у attempts_limit; типизированного поля в
// agentReviewConfig нет намеренно — 2.5 уронил бы json.Unmarshal и с ним
// каждый Preview.
func (e *Engine) reviewLimit(ctx context.Context, q postgres.Querier, gate domain.Gate, parentRunID uuid.UUID) (limit, failed int, err error) {
	limit, err = maxAttempts(gate.Config)
	if err != nil {
		return 0, 0, nil
	}
	failed, err = e.store.CountFailedReviews(ctx, q, parentRunID, gate.ID)
	if err != nil {
		return 0, 0, err
	}
	return limit, failed, nil
}

// reviewVerdict достаёт вердикт из артефакта, сданного в прогоне ревью.
func (e *Engine) reviewVerdict(ctx context.Context, q postgres.Querier, review domain.StageRun, cfg agentReviewConfig) (string, any, error) {
	artifacts, err := e.store.ListRunArtifacts(ctx, q, review.ID, true)
	if err != nil {
		return "", nil, err
	}
	// Последний по времени: ревьюер мог переcдать вердикт внутри своего же
	// прогона, и в силе последний.
	for i := len(artifacts) - 1; i >= 0; i-- {
		a := artifacts[i]
		if a.Kind != cfg.ArtifactKind {
			continue
		}
		content, _, err := e.store.BlobContent(ctx, q, a.BlobSHA)
		if err != nil {
			return "", nil, err
		}
		var body map[string]any
		if err := json.Unmarshal(content, &body); err != nil {
			// Вердикт не разобрался — это не «прошло»: гейт обязан удержать
			// переход, а не пропустить его на нечитаемом артефакте.
			return "unparseable", nil, nil
		}
		verdict, _ := body[cfg.VerdictField].(string)
		return verdict, body["findings"], nil
	}
	return "", nil, nil
}

// spawnReviews запускает перепроверки для гейтов, которые её ждут.
//
// Вызывается из Execute при заблокированном переходе: агент попросил перейти,
// значит свою работу считает законченной — самое время звать проверяющего.
// Этап уходит в gating и агента больше не держит.
func (e *Engine) spawnReviews(ctx context.Context, tx pgx.Tx, task domain.Task, run domain.StageRun, t domain.Transition, outcomes []GateOutcome, stageArtifacts []domain.Artifact, actor uuid.UUID) error {
	started := false

	for i, g := range t.Gates {
		if g.Type != domain.GateAgentReview || outcomes[i].Status != domain.GateResultPending {
			continue
		}
		var cfg agentReviewConfig
		if err := json.Unmarshal(g.Config, &cfg); err != nil {
			return apperr.InvalidInput("конфигурация гейта %q: %v", g.Key, err)
		}
		cfg.defaults()
		if cfg.ReviewerRoleKey == "" {
			continue
		}

		existing, err := e.store.LatestReviewRun(ctx, tx, run.ID, g.ID)
		if err != nil {
			return err
		}
		// Незакрытая перепроверка уже идёт — второй прогон того же гейта был
		// бы вторым исполнителем внутри одного этапа.
		if existing != nil && existing.FinishedAt == nil {
			continue
		}
		// Закрытая перепроверка перезапускается только если исполнитель с тех
		// пор что-то сдал. Иначе агент, дёргая переход по кругу, гонял бы
		// ревьюера по одному и тому же результату.
		if existing != nil && !reviewStale(*existing, stageArtifacts) {
			continue
		}
		// Предел перепроверок выбран — вычислитель уже не отдаёт pending, но
		// побочный эффект сверяется со счётом сам: разойтись им нельзя.
		limit, failed, err := e.reviewLimit(ctx, tx, g, run.ID)
		if err != nil {
			return err
		}
		if limit > 0 && failed >= limit {
			continue
		}

		role, err := e.store.GetRoleByKey(ctx, tx, task.ProjectID, cfg.ReviewerRoleKey)
		if err != nil {
			return err
		}
		if run.RoleID != nil && *run.RoleID == role.ID {
			return apperr.InvalidInput(
				"гейт %q зовёт на перепроверку ту же роль, что и исполнителя этапа", g.Key)
		}
		versions, err := e.store.ListRoleVersions(ctx, tx, role.ID)
		if err != nil {
			return err
		}
		if len(versions) == 0 {
			return apperr.InvalidInput("у роли %q нет ни одной версии инструкции", role.Key)
		}

		review, err := e.store.CreateReviewRun(ctx, tx, run, g.ID, role.ID, versions[0].ID)
		if err != nil {
			return err
		}
		started = true

		if err := e.store.AppendEvent(ctx, tx, task.ID, &review.ID, domain.EventReviewRequested, actor,
			map[string]any{
				"gate": g.Key, "transition": t.Key,
				"reviewer_role": role.Key, "parent_run_id": run.ID.String(),
			}); err != nil {
			return err
		}
	}

	if !started {
		return nil
	}
	// Этап переходит в gating: работать в нём некому, пока идёт перепроверка.
	if _, err := tx.Exec(ctx, `
		UPDATE stage_runs SET state = 'gating', claimed_by = NULL, claimed_at = NULL,
		       lease_expires_at = NULL, agent_session_ref = NULL
		WHERE id = $1 AND finished_at IS NULL`, run.ID); err != nil {
		return translateExec(err)
	}
	// Аренда снята — токены задачи этого прогона гаснут вместе с ней.
	_, err := e.store.RevokeRunTokens(ctx, tx, run.ID)
	return err
}

// reviewStale — сдавал ли исполнитель что-нибудь после того, как ревью
// закрылось. Именно это отличает «вердикт актуален» от «вердикт про прошлую
// версию результата».
func reviewStale(review domain.StageRun, stageArtifacts []domain.Artifact) bool {
	if review.FinishedAt == nil {
		return false
	}
	for _, a := range stageArtifacts {
		if a.CreatedAt.After(*review.FinishedAt) {
			return true
		}
	}
	return false
}

// completeReview закрывает прогон перепроверки вердиктом и записывает
// результат гейта на родительский этап.
//
// Момент закрытия — сдача артефакта, объявленного гейтом: у ревьюера нет
// собственного перехода, его работа и есть этот артефакт.
func (e *Engine) completeReview(ctx context.Context, tx pgx.Tx, task domain.Task, review domain.StageRun, artifact domain.Artifact, actor uuid.UUID) error {
	if review.GateID == nil || review.ParentRunID == nil {
		return nil
	}
	gate, transition, err := e.gateByID(ctx, tx, *review.GateID)
	if err != nil {
		return err
	}
	var cfg agentReviewConfig
	if err := json.Unmarshal(gate.Config, &cfg); err != nil {
		return apperr.InvalidInput("конфигурация гейта %q: %v", gate.Key, err)
	}
	cfg.defaults()
	if artifact.Kind != cfg.ArtifactKind {
		return nil
	}

	verdict, findings, err := e.reviewVerdict(ctx, tx, review, cfg)
	if err != nil {
		return err
	}
	passed := verdict == cfg.PassOn

	outcome := "rejected"
	status := domain.GateResultFailed
	if passed {
		outcome, status = "advanced", domain.GateResultPassed
	}
	if err := e.store.FinishStageRun(ctx, tx, review.ID, domain.StagePassed, outcome); err != nil {
		return err
	}

	details := map[string]any{
		"verdict": verdict, "review_run_id": review.ID.String(),
		"artifact_id": artifact.ID.String(),
	}
	if findings != nil {
		details["findings"] = findings
	}
	if err := e.store.RecordGateResult(ctx, tx, *review.ParentRunID, gate.ID, transition.ID,
		status, details, &actor); err != nil {
		return err
	}

	// Родительский этап выходит из gating: при pass исполнителю остаётся
	// повторить переход, при fail — доработать результат.
	if _, err := tx.Exec(ctx, `
		UPDATE stage_runs SET state = 'submitted'
		WHERE id = $1 AND state = 'gating' AND finished_at IS NULL`, *review.ParentRunID); err != nil {
		return translateExec(err)
	}

	return e.store.AppendEvent(ctx, tx, task.ID, &review.ID, domain.EventReviewCompleted, actor,
		map[string]any{
			"gate": gate.Key, "transition": transition.Key,
			"verdict": verdict, "passed": passed,
			"parent_run_id": review.ParentRunID.String(),
		})
}

// resetReview снимает результат перепроверки, когда исполнитель сдал новую
// версию результата: старый вердикт вынесен о том, чего уже нет.
//
// Вместе с вердиктом ревью снимается и решение человека по human_approval
// (решение 0042): апрув ставился тому, что лежало в задаче до этой сдачи.
// Счёт апрувов для min_approvals ведётся по ленте событий и сам отсекает
// старые по времени сдачи (CountApprovals).
func (e *Engine) resetReview(ctx context.Context, tx pgx.Tx, run domain.StageRun) error {
	_, err := tx.Exec(ctx, `
		DELETE FROM gate_results gr
		USING gates g
		WHERE gr.gate_id = g.id
		  AND gr.stage_run_id = $1
		  AND g.type = ANY($2)`, run.ID, []string{domain.GateAgentReview, domain.GateHumanApproval})
	return translateExec(err)
}

func (e *Engine) gateByID(ctx context.Context, q postgres.Querier, gateID uuid.UUID) (domain.Gate, domain.Transition, error) {
	var g domain.Gate
	var t domain.Transition
	err := q.QueryRow(ctx, `
		SELECT g.id, g.transition_id, g.key, g.name, g.type, g.config, g.blocking, g.position,
		       t.id, t.workflow_id, t.key, t.name, t.from_status_id, t.to_status_id, t.kind
		FROM gates g
		JOIN workflow_transitions t ON t.id = g.transition_id
		WHERE g.id = $1`, gateID,
	).Scan(&g.ID, &g.TransitionID, &g.Key, &g.Name, &g.Type, &g.Config, &g.Blocking, &g.Position,
		&t.ID, &t.WorkflowID, &t.Key, &t.Name, &t.FromStatusID, &t.ToStatusID, &t.Kind)
	if err != nil {
		return g, t, apperr.NotFound("гейт не найден")
	}
	return g, t, nil
}

// ── schema_validation ───────────────────────────────────────────────────────

type schemaValidationConfig struct {
	ArtifactKind string `json:"artifact_kind"`
	// Ожидаемые значения полей: {"verdict":"pass"} — вложенное подмножество.
	Expect map[string]any `json:"expect"`
	// Схема прямо в гейте. Если её нет, берётся схема из output_contract той
	// роли, что сдала артефакт: контракт роли для того и заведён.
	Schema json.RawMessage `json:"schema"`
	// "stage" (по умолчанию) или "task" — где искать артефакт.
	Scope string `json:"scope"`
}

// evalSchemaValidation проверяет, что артефакт валиден и содержит ожидаемое.
func (e *Engine) evalSchemaValidation(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	var cfg schemaValidationConfig
	if err := json.Unmarshal(in.Gate.Config, &cfg); err != nil {
		return GateOutcome{}, fmt.Errorf("конфигурация гейта %q: %w", in.Gate.Key, err)
	}
	if cfg.ArtifactKind == "" {
		return base(in.Gate, domain.GateResultFailed,
			"в конфигурации гейта не указан artifact_kind"), nil
	}

	pool := in.StageArtifacts
	if cfg.Scope == "task" {
		pool = in.TaskArtifacts
	}
	var found *domain.Artifact
	for i := range pool {
		if pool[i].Kind == cfg.ArtifactKind {
			if found == nil || pool[i].Version > found.Version {
				found = &pool[i]
			}
		}
	}
	if found == nil {
		out := base(in.Gate, domain.GateResultFailed, "артефакт "+cfg.ArtifactKind+" не сдан")
		out.Details = map[string]any{"artifact_kind": cfg.ArtifactKind}
		return out, nil
	}

	content, _, err := e.store.BlobContent(ctx, q, found.BlobSHA)
	if err != nil {
		return GateOutcome{}, err
	}

	schema := cfg.Schema
	if len(schema) == 0 {
		if schema, err = e.contractSchema(ctx, q, *found); err != nil {
			return GateOutcome{}, err
		}
	}
	if len(schema) == 0 && len(cfg.Expect) == 0 {
		out := base(in.Gate, domain.GateResultPassed, "проверять нечего: ни схемы, ни ожиданий")
		out.Details = map[string]any{"artifact_id": found.ID, "artifact_kind": found.Kind}
		return out, nil
	}

	var doc any
	if err := json.Unmarshal(content, &doc); err != nil {
		out := base(in.Gate, domain.GateResultFailed,
			"артефакт "+cfg.ArtifactKind+" не разбирается как JSON")
		out.Details = map[string]any{"artifact_id": found.ID, "parse_error": err.Error()}
		return out, nil
	}

	problems := []string{}
	if len(schema) > 0 {
		errs, err := validateAgainstSchema(schema, doc)
		if err != nil {
			return GateOutcome{}, err
		}
		problems = append(problems, errs...)
	}
	problems = append(problems, expectMismatches(cfg.Expect, doc, "")...)

	details := map[string]any{
		"artifact_id": found.ID, "artifact_kind": found.Kind, "version": found.Version,
	}
	if len(problems) == 0 {
		out := base(in.Gate, domain.GateResultPassed, "")
		out.Details = details
		return out, nil
	}

	details["problems"] = problems
	out := base(in.Gate, domain.GateResultFailed,
		fmt.Sprintf("%s не проходит проверку: %s", cfg.ArtifactKind, strings.Join(problems, "; ")))
	out.Details = details
	return out, nil
}

// contractSchema достаёт схему артефакта из output_contract роли, которая его
// сдала. Роль объявляет контракт один раз, а гейту остаётся только сослаться
// на kind — иначе схему пришлось бы дублировать в настройках цикла.
func (e *Engine) contractSchema(ctx context.Context, q postgres.Querier, a domain.Artifact) (json.RawMessage, error) {
	if a.StageRunID == nil {
		return nil, nil
	}
	run, err := e.store.GetStageRun(ctx, q, *a.StageRunID)
	if err != nil || run.RoleVersionID == nil {
		return nil, nil
	}
	rv, err := e.store.GetRoleVersion(ctx, q, *run.RoleVersionID)
	if err != nil {
		return nil, nil
	}

	var contract []struct {
		Kind   string          `json:"kind"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(rv.OutputContract, &contract); err != nil {
		return nil, nil
	}
	for _, c := range contract {
		if c.Kind == a.Kind {
			return c.Schema, nil
		}
	}
	return nil, nil
}

func validateAgainstSchema(schema json.RawMessage, doc any) ([]string, error) {
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, apperr.InvalidInput("схема артефакта не разбирается: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("artifact.json", raw); err != nil {
		return nil, apperr.InvalidInput("схема артефакта некорректна: %v", err)
	}
	sch, err := c.Compile("artifact.json")
	if err != nil {
		return nil, apperr.InvalidInput("схема артефакта некорректна: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return flattenValidation(ve), nil
		}
		return []string{err.Error()}, nil
	}
	return nil, nil
}

// expectMismatches сверяет ожидаемые значения как вложенное подмножество:
// в артефакте может быть что угодно сверх этого, важно лишь совпадение
// перечисленных полей.
func expectMismatches(expect map[string]any, doc any, prefix string) []string {
	if len(expect) == 0 {
		return nil
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return []string{prefix + " не объект"}
	}

	keys := make([]string, 0, len(expect))
	for k := range expect {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := []string{}
	for _, k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		got, present := obj[k]
		if !present {
			out = append(out, "нет поля "+path)
			continue
		}
		if nested, ok := expect[k].(map[string]any); ok {
			out = append(out, expectMismatches(nested, got, path)...)
			continue
		}
		if !sameJSON(expect[k], got) {
			out = append(out, fmt.Sprintf("%s = %v, ожидалось %v", path, got, expect[k]))
		}
	}
	return out
}

// sameJSON сравнивает значения после нормализации через JSON: 1 и 1.0
// приходят из разных источников одним и тем же числом.
func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(x, y)
}

func translateExec(err error) error {
	if err == nil {
		return nil
	}
	return apperr.Internal(err, "запись состояния прогона")
}

// flattenValidation разворачивает дерево ошибок схемы в плоский список:
// агенту нужен перечень «что именно не так», а не структура валидатора.
func flattenValidation(ve *jsonschema.ValidationError) []string {
	if ve == nil {
		return nil
	}
	// У листа Error() печатает ровно одну строку вида "at '/verdict': ...".
	// Свой формат тут не нужен — важно, что именно валидатор счёл ошибкой.
	if len(ve.Causes) == 0 {
		return []string{strings.TrimSpace(ve.Error())}
	}
	out := []string{}
	for _, c := range ve.Causes {
		out = append(out, flattenValidation(c)...)
	}
	return out
}
