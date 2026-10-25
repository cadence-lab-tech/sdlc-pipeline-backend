// Package workflow — движок цикла: доступные переходы, прогон гейтов, перевод
// задачи между статусами.
//
// Движок ничего не знает про «аналитику» и «разработку». Он знает про статусы,
// переходы и типы гейтов; что означает конкретный статус — данные проекта.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Options — рамки работы движка, приезжающие из конфигурации.
type Options struct {
	// StrictGates=false: гейт нереализованного типа отдаёт skipped и переход
	// пропускает. Так цикл можно прогонять целиком, пока фаза 3 не написана.
	// StrictGates=true: такой гейт валит переход — значение для продакшена.
	StrictGates bool
	// Сколько живёт аренда без heartbeat и как часто его слать. Агент
	// локальный и может умереть, не отпустив задачу: без этой пары задача
	// осталась бы занятой навсегда.
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	// Векторизация запросов поиска. nil — поиск остаётся полнотекстовым:
	// без провайдера эмбеддингов система обязана работать, просто хуже.
	Embedder embed.Embedder
	// Проверка доступа к репозиториям учётными данными проекта (решение
	// 0028). nil — приложение не настроено: привязка репозитория проверяется
	// только на формат, как в фазах 1-5.
	GitHub GitHubAccess
}

const (
	defaultLeaseTTL          = 30 * time.Minute
	defaultHeartbeatInterval = 5 * time.Minute
)

type Engine struct {
	store      *postgres.Store
	log        *slog.Logger
	strict     bool
	leaseTTL   time.Duration
	heartbeat  time.Duration
	embedder   embed.Embedder
	evaluators map[string]evaluator
	// Проверка доступа к репозиториям учётными данными проекта (решение
	// 0028). nil — приложение не настроено, и привязка репозитория ничем
	// не ограничена, как в фазах 1-5.
	github GitHubAccess
}

func NewEngine(store *postgres.Store, log *slog.Logger, opts Options) *Engine {
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = defaultLeaseTTL
	}
	if opts.HeartbeatInterval <= 0 || opts.HeartbeatInterval >= opts.LeaseTTL {
		opts.HeartbeatInterval = defaultHeartbeatInterval
	}
	e := &Engine{
		store: store, log: log, strict: opts.StrictGates,
		leaseTTL: opts.LeaseTTL, heartbeat: opts.HeartbeatInterval,
		embedder: opts.Embedder, github: opts.GitHub,
	}
	e.registerEvaluators()
	return e
}

// TransitionPreview — переход вместе с текущим вердиктом его гейтов.
type TransitionPreview struct {
	Key      string        `json:"key"`
	Name     string        `json:"name"`
	Kind     string        `json:"kind"`
	ToStatus string        `json:"to_status"`
	Allowed  bool          `json:"allowed"`
	Gates    []GateOutcome `json:"gates"`
}

// TransitionResult — итог выполненного (или заблокированного) перехода.
type TransitionResult struct {
	Task    domain.Task      `json:"task"`
	Blocked bool             `json:"blocked"`
	Gates   []GateOutcome    `json:"gates"`
	FromRun domain.StageRun  `json:"from_stage_run"`
	NewRun  *domain.StageRun `json:"new_stage_run,omitempty"`
}

// Preview прогоняет гейты всех доступных переходов, ничего не меняя.
// Это то, что рисует карточка задачи: какие кнопки активны и чего не хватает.
func (e *Engine) Preview(ctx context.Context, task domain.Task) ([]TransitionPreview, error) {
	q := e.store.DB()

	transitions, err := e.store.AvailableTransitions(ctx, q, task.WorkflowID, task.StatusID)
	if err != nil {
		return nil, err
	}
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return nil, err
	}
	stageArtifacts, taskArtifacts, err := e.artifacts(ctx, q, task, run)
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

	out := make([]TransitionPreview, 0, len(transitions))
	for _, t := range transitions {
		outcomes, allowed, err := e.evaluate(ctx, q, task, run, t, stageArtifacts, taskArtifacts)
		if err != nil {
			return nil, err
		}
		out = append(out, TransitionPreview{
			Key: t.Key, Name: t.Name, Kind: t.Kind,
			ToStatus: statusKey[t.ToStatusID], Allowed: allowed, Gates: outcomes,
		})
	}
	return out, nil
}

// Execute переводит задачу по переходу, если гейты пропускают.
//
// Всё идёт одной транзакцией под блокировкой строки задачи: иначе два
// одновременных запроса прогонят гейты на одном состоянии и переведут задачу
// дважды. Результаты гейтов пишутся в любом случае — заблокированный переход
// должен оставить в базе объяснение, почему он не состоялся.
func (e *Engine) Execute(ctx context.Context, taskKey, transitionKey string, actor uuid.UUID, comment string) (TransitionResult, error) {
	return e.ExecuteBound(ctx, taskKey, transitionKey, actor, nil, comment)
}

// ExecuteBound — Execute с привязкой токена задачи (Auth.Bound): исполнитель
// этапа двигает только свою задачу и только вперёд или возвратом (решение
// человека к 0049). nil — переход человека или агента с личным токеном,
// правила те же, что у Execute. Транспорты зовут его, а не Execute.
func (e *Engine) ExecuteBound(ctx context.Context, taskKey, transitionKey string, actor uuid.UUID, bound *TaskBinding, comment string) (TransitionResult, error) {
	var res TransitionResult

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}

		// Переход ищется в цикле самой задачи, а не в активном цикле проекта:
		// задача, начатая на прошлой версии workflow, доходит по ней же.
		transition, err := e.store.GetTransitionByKey(ctx, tx, task.WorkflowID, transitionKey)
		if err != nil {
			return err
		}
		if transition.FromStatusID != nil && *transition.FromStatusID != task.StatusID {
			return apperr.New(apperr.CodeTransitionNotAllowed,
				"переход %q недоступен из статуса %q", transitionKey, task.StatusKey)
		}
		if bound != nil {
			if err := executorTransition(bound, task, transition); err != nil {
				return err
			}
		}

		run, err := e.store.CurrentStageRun(ctx, tx, task.ID, task.StatusID)
		if err != nil {
			return err
		}
		stageArtifacts, taskArtifacts, err := e.artifacts(ctx, tx, task, run)
		if err != nil {
			return err
		}

		outcomes, allowed, err := e.evaluate(ctx, tx, task, run, transition, stageArtifacts, taskArtifacts)
		if err != nil {
			return err
		}

		for i, o := range outcomes {
			gate := transition.Gates[i]
			// Решения человека (checklist, human_approval) уже записаны своим
			// актором — перезаписывать их вердиктом движка нельзя.
			if o.Type == domain.GateChecklist || o.Type == domain.GateHumanApproval {
				continue
			}
			// Снятие гейта, который решает только человек, — тоже его
			// решение: перезапись вердиктом движка стёрла бы, кто дал попытку.
			if o.Status == domain.GateResultWaived && humanOnly(o.Type) {
				continue
			}
			if err := e.store.RecordGateResult(ctx, tx, run.ID, gate.ID, transition.ID,
				o.Status, o.Details, nil); err != nil {
				return err
			}
		}

		res.Task = task
		res.FromRun = run
		res.Gates = outcomes

		if !allowed {
			res.Blocked = true
			// Асинхронные гейты запускаются здесь, а не в Preview: агент
			// попросил перейти — значит свою работу считает законченной, и
			// самое время звать проверяющего. Preview обязан оставаться
			// чистой проверкой без побочных эффектов.
			if err := e.spawnReviews(ctx, tx, task, run, transition, outcomes,
				stageArtifacts, actor); err != nil {
				return err
			}
			return e.store.AppendEvent(ctx, tx, task.ID, &run.ID, domain.EventGateFailed, actor,
				map[string]any{"transition": transition.Key, "gates": outcomes})
		}

		toStatus, err := e.store.GetStatus(ctx, tx, transition.ToStatusID)
		if err != nil {
			return err
		}

		runState, outcome, eventType := finishSemantics(transition.Kind)
		if err := e.store.FinishStageRun(ctx, tx, run.ID, runState, outcome); err != nil {
			return err
		}
		if err := e.store.SetTaskStatus(ctx, tx, task.ID, toStatus.ID, toStatus.IsTerminal); err != nil {
			return err
		}

		var rejectedFrom *uuid.UUID
		if transition.Kind == "reject" {
			rejectedFrom = &run.ID
		}
		newRun, err := e.store.EnterStatus(ctx, tx, task.ID, toStatus.ID, rejectedFrom)
		if err != nil {
			return err
		}
		res.NewRun = &newRun

		payload := map[string]any{
			"transition": transition.Key,
			"from":       task.StatusKey,
			"to":         toStatus.Key,
		}
		if comment != "" {
			payload["comment"] = comment
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, &run.ID, eventType, actor, payload); err != nil {
			return err
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, &newRun.ID, domain.EventStageEntered, actor,
			map[string]any{"status": toStatus.Key, "attempt": newRun.Attempt}); err != nil {
			return err
		}

		task.StatusID = toStatus.ID
		task.StatusKey = toStatus.Key
		task.StatusName = toStatus.Name
		res.Task = task
		return nil
	})

	return res, err
}

// finishSemantics переводит вид перехода в то, чем закончилась попытка этапа.
//
// Откат — это не провал этапа: ревьюер, нашедший проблему, свою работу сделал.
// Поэтому state остаётся passed, а факт отката живёт в outcome.
func finishSemantics(kind string) (state, outcome, eventType string) {
	switch kind {
	case "reject":
		return domain.StagePassed, "rejected", domain.EventTaskRejected
	case "cancel":
		return domain.StageAbandoned, "cancelled", domain.EventTaskCancelled
	default:
		return domain.StagePassed, "advanced", domain.EventTaskAdvanced
	}
}

func (e *Engine) evaluate(ctx context.Context, q postgres.Querier, task domain.Task, run domain.StageRun, t domain.Transition, stageArtifacts, taskArtifacts []domain.Artifact) ([]GateOutcome, bool, error) {
	outcomes := make([]GateOutcome, 0, len(t.Gates))
	allowed := true

	for _, gate := range t.Gates {
		decision, err := e.store.GateDecision(ctx, q, run.ID, gate.ID)
		if err != nil {
			return nil, false, err
		}

		in := EvalInput{
			Task: task, StageRun: run, Transition: t, Gate: gate,
			StageArtifacts: stageArtifacts, TaskArtifacts: taskArtifacts,
			Decision: decision,
		}

		var outcome GateOutcome
		if decision != nil && decision.Status == domain.GateResultWaived {
			// Человек явно снял гейт — движок его не перепроверяет.
			outcome = base(gate, domain.GateResultWaived, "снят вручную")
		} else if eval, ok := e.evaluators[gate.Type]; ok {
			outcome, err = eval(ctx, q, in)
			if err != nil {
				return nil, false, apperr.Internal(err, "гейт %q", gate.Key)
			}
		} else {
			outcome = e.unimplemented(gate)
		}

		if gate.Blocking && !outcome.Passed() {
			allowed = false
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, allowed, nil
}

func (e *Engine) unimplemented(gate domain.Gate) GateOutcome {
	if e.strict {
		return base(gate, domain.GateResultFailed,
			"тип гейта \""+gate.Type+"\" ещё не реализован, а SDLC_STRICT_GATES включён")
	}
	e.log.Warn("гейт пропущен: тип не реализован",
		"gate", gate.Key, "type", gate.Type)
	return base(gate, domain.GateResultSkipped,
		"тип гейта \""+gate.Type+"\" появится в следующей фазе")
}

func (e *Engine) artifacts(ctx context.Context, q postgres.Querier, task domain.Task, run domain.StageRun) (stage, all []domain.Artifact, err error) {
	all, err = e.store.ListArtifacts(ctx, q, task.ID, false)
	if err != nil {
		return nil, nil, err
	}
	stage = make([]domain.Artifact, 0, len(all))
	for _, a := range all {
		if a.StageRunID != nil && *a.StageRunID == run.ID {
			stage = append(stage, a)
		}
	}
	return stage, all, nil
}

// DecideGate записывает решение человека или агента по гейту.
//
// Само по себе решение задачу не двигает: переход остаётся отдельным действием,
// потому что гейтов на переходе несколько и закрывать их можно в любом порядке.
//
// bound — привязка токена задачи (Auth.Bound), nil у людей и личных токенов.
// Исполнитель этапа решает только passed по чек-листу своего этапа, а гейты
// из humanOnlyGates (human_approval, attempts_limit) не закрывает, даже если
// держатель аренды — человек.
func (e *Engine) DecideGate(ctx context.Context, taskKey, transitionKey, gateKey string, actor uuid.UUID, bound *TaskBinding, status string, details json.RawMessage) (GateOutcome, error) {
	var out GateOutcome

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		transition, err := e.store.GetTransitionByKey(ctx, tx, task.WorkflowID, transitionKey)
		if err != nil {
			return err
		}

		var gate *domain.Gate
		for i := range transition.Gates {
			if transition.Gates[i].Key == gateKey {
				gate = &transition.Gates[i]
				break
			}
		}
		if gate == nil {
			return apperr.NotFound("на переходе %q нет гейта %q", transitionKey, gateKey)
		}

		run, err := e.store.CurrentStageRun(ctx, tx, task.ID, task.StatusID)
		if err != nil {
			return err
		}

		rule, onlyHuman := humanOnlyGates[gate.Type]

		// Токен задачи апрув не ставит, даже если аренду держит человек
		// (/clab-run): проверка по виду токена, а не только принципала.
		if bound != nil && onlyHuman {
			return executorRefused(bound, fmt.Sprintf("гейт %q закрывает человек", gate.Key))
		}
		if bound != nil {
			if err := executorGate(bound, task, run, transition, *gate, status); err != nil {
				return err
			}
		}

		// Апрув человека ставит человек. Агент, представившийся человеком,
		// в режиме токенов не пройдёт аутентификацию; в режиме заголовка
		// это защита от случайного самоапрува агентом, а не от злого умысла.
		if onlyHuman {
			who, err := e.store.GetPrincipal(ctx, tx, actor)
			if err != nil {
				return err
			}
			if who.Kind != domain.PrincipalUser {
				return apperr.Forbidden("гейт %q закрывает человек, а не %s %q",
					gate.Key, who.Kind, who.Handle)
			}
			if rule.waiveOnly && status != domain.GateResultWaived {
				return apperr.InvalidInput(
					"по гейту %q вердикт выносит движок; человек может только снять его: "+
						"status waived, а не %q", gate.Key, status)
			}
		}

		if err := e.store.RecordGateResult(ctx, tx, run.ID, gate.ID, transition.ID,
			status, json.RawMessage(details), &actor); err != nil {
			return err
		}

		// Событие пишется всегда: по нему считаются независимые апрувы для
		// min_approvals, которого в одной строке gate_results не выразить.
		if err := e.store.AppendEvent(ctx, tx, task.ID, &run.ID, domain.EventGateDecided, actor,
			map[string]any{
				"gate_id":    gate.ID.String(),
				"gate":       gate.Key,
				"transition": transition.Key,
				"status":     status,
				"details":    json.RawMessage(details),
			}); err != nil {
			return err
		}

		out = base(*gate, status, "")
		return nil
	})

	return out, err
}

// humanGate — как человек решает гейт, который решает только он.
type humanGate struct {
	// waiveOnly: вердикт выносит движок, а человек может лишь снять гейт —
	// waived. passed и failed здесь не решение, а подмена вычисления.
	waiveOnly bool
}

// humanOnlyGates — типы гейтов, решение по которым выносит только человек:
// принципал вида user и не токеном задачи. Предел этапа (решение 0061)
// добавляет сюда свой тип строкой, а не второй копией проверки.
var humanOnlyGates = map[string]humanGate{
	domain.GateHumanApproval: {},
	domain.GateAttemptsLimit: {waiveOnly: true},
}

func humanOnly(gateType string) bool {
	_, ok := humanOnlyGates[gateType]
	return ok
}

func validGateDecision(status string) bool {
	switch status {
	case domain.GateResultPassed, domain.GateResultFailed, domain.GateResultWaived:
		return true
	}
	return false
}

// ValidateDecision — какие вердикты человек может выставить руками.
// pending и skipped исключены: это состояния движка, а не решения.
func ValidateDecision(status string) error {
	if !validGateDecision(status) {
		return apperr.InvalidInput(
			"вердикт по гейту может быть passed, failed или waived, а не %q", status)
	}
	return nil
}
