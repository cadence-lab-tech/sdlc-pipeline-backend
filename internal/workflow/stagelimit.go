// Предел времени этапа (CLAB-221, решение 0061): гейт stage_time_limit на
// переходе, которым задача входит в статус. Он ничего не держит на самом
// переходе, а после срока закрывает этап для новых захватов агентами; часы
// идут от stage_runs.entered_at по стенным часам и не сбрасываются ни
// heartbeat'ом, ни release, ни протуханием аренды. Решение дальше — за
// человеком: снять предел (waived), отменить задачу или провести другим
// переходом.
//
// Срок считает одна функция — limitFor: её зовут отказ захвата, видимость
// (stage_limit в карточке, предпросмотре и ответе захвата) и проход воркера,
// и разойтись в том, что считать просрочкой, им нельзя.
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// maxStageDuration — потолок предела. Больше года этап не ждут: такое число
// — опечатка в единицах, а не предел, и валидатор держит активацию.
const maxStageDuration = 366 * 24 * time.Hour

// errNoMaxDuration — в config гейта предела нет: гейт ничего не держит.
var errNoMaxDuration = errors.New("не задан")

// maxDuration читает предел из config гейта stage_time_limit: строка формата
// time.ParseDuration («90m», «4h», «24h»), строго больше нуля и не больше
// года. Число, null, пустая строка и «1d» — не предел, а ошибка в цикле.
// Разбор один на валидатор, вычислитель, захват и воркер. Вторым значением
// отдаётся строка как написана в цикле: её показывают людям.
func maxDuration(config json.RawMessage) (time.Duration, string, error) {
	if len(bytes.TrimSpace(config)) == 0 {
		return 0, "", errNoMaxDuration
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(config, &cfg); err != nil {
		return 0, "", fmt.Errorf("не прочитать: config — не объект JSON: %w", err)
	}
	raw, ok := cfg["max_duration"]
	if !ok {
		return 0, "", errNoMaxDuration
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, "", fmt.Errorf("нужна строка длительности вроде \"4h\", а не %s", string(raw))
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, "", errors.New("пустая строка; нужна длительность вроде \"4h\"")
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		hint := ""
		if strings.HasSuffix(text, "d") {
			hint = "; суток в формате нет, сутки — \"24h\""
		}
		return 0, "", fmt.Errorf("%q не длительность (единицы s, m, h: \"90m\", \"4h\")%s", text, hint)
	}
	if d <= 0 {
		return 0, "", fmt.Errorf("нужно больше нуля, а не %q", text)
	}
	if d > maxStageDuration {
		return 0, "", fmt.Errorf("%q больше года (%s)", text, maxStageDuration)
	}
	return d, text, nil
}

// stageTimeGate — гейт предела на переходе: первый по порядку. Второй на том
// же переходе валидатор не пропускает.
func stageTimeGate(t domain.Transition) *domain.Gate {
	for i := range t.Gates {
		if t.Gates[i].Type == domain.GateStageTimeLimit {
			return &t.Gates[i]
		}
	}
	return nil
}

// evalStageTimeLimit на своём переходе всегда passed: предел начинает
// действовать после входа в статус, а держать сам вход ему нечего.
func (e *Engine) evalStageTimeLimit(ctx context.Context, q postgres.Querier, in EvalInput) (GateOutcome, error) {
	_, text, err := maxDuration(in.Gate.Config)
	if err != nil {
		reason := "предел этапа не задан"
		if !errors.Is(err, errNoMaxDuration) {
			reason = "предел этапа задан неверно (max_duration: " + err.Error() + ") и не действует"
		}
		return base(in.Gate, domain.GateResultPassed, reason), nil
	}
	target, err := e.store.GetStatus(ctx, q, in.Transition.ToStatusID)
	if err != nil {
		return GateOutcome{}, err
	}
	out := base(in.Gate, domain.GateResultPassed,
		fmt.Sprintf("предел этапа в статусе %s — %s, отсчёт с входа", target.Key, text))
	out.Details = map[string]any{"status": target.Key, "max_duration": text}
	return out, nil
}

// StageLimit — предел времени этапа и сколько от него осталось. Блок
// отдаётся соседним полем к transitions и в ответе захвата; null — у этапа
// предела нет.
type StageLimit struct {
	StageRunID       uuid.UUID  `json:"stage_run_id"`
	Gate             string     `json:"gate"`
	Transition       string     `json:"transition"`
	Status           string     `json:"status"`
	MaxDuration      string     `json:"max_duration"`
	EnteredAt        time.Time  `json:"entered_at"`
	Deadline         time.Time  `json:"deadline"`
	ElapsedSeconds   int64      `json:"elapsed_seconds"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	Overdue          bool       `json:"overdue"`
	OverdueAt        *time.Time `json:"overdue_at"`
	Waived           bool       `json:"waived"`
	WaivedBy         *string    `json:"waived_by"`
	WaivedAt         *time.Time `json:"waived_at"`
}

// Holds — закрывает ли предел этап для новых захватов агентами: срок вышел,
// а человек предел не снял.
func (l *StageLimit) Holds() bool {
	return l != nil && l.Overdue && !l.Waived
}

// stageClock — срок этапа по стенным часам от входа: одно выражение на
// отказ захвата, видимость и проход воркера.
func stageClock(enteredAt time.Time, limit time.Duration, now time.Time) (deadline time.Time, overdue bool) {
	deadline = enteredAt.Add(limit)
	return deadline, !now.Before(deadline)
}

// limitFor — предел прогона этапа на момент now. nil — предела нет: прогон
// перепроверки, закрытый, открытый начальным статусом или до миграции, вход
// переходом без гейта или с негодной длительностью (валидатор такой цикл не
// активирует, а если он всё же активен, предел необязателен и без числа его
// нет).
func (e *Engine) limitFor(ctx context.Context, q postgres.Querier, run domain.StageRun, now time.Time) (*StageLimit, error) {
	if run.Kind != domain.StageKindStage || run.EnteredViaTransitionID == nil || run.FinishedAt != nil {
		return nil, nil
	}
	t, err := e.store.GetTransition(ctx, q, *run.EnteredViaTransitionID)
	if err != nil {
		if apperr.As(err).Code == apperr.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	gate := stageTimeGate(t)
	if gate == nil {
		return nil, nil
	}
	d, text, err := maxDuration(gate.Config)
	if err != nil {
		return nil, nil
	}
	deadline, overdue := stageClock(run.EnteredAt, d, now)
	out := &StageLimit{
		StageRunID: run.ID, Gate: gate.Key, Transition: t.Key, Status: run.StatusKey,
		MaxDuration: text, EnteredAt: run.EnteredAt, Deadline: deadline,
		ElapsedSeconds:   max(0, int64(now.Sub(run.EnteredAt)/time.Second)),
		RemainingSeconds: max(0, int64(deadline.Sub(now)/time.Second)),
		Overdue:          overdue, OverdueAt: run.OverdueAt,
	}
	decision, err := e.store.GateDecision(ctx, q, run.ID, gate.ID)
	if err != nil {
		return nil, err
	}
	if decision != nil && decision.Status == domain.GateResultWaived {
		out.Waived, out.WaivedAt = true, decision.DecidedAt
		if decision.DecidedBy != nil {
			who, err := e.store.GetPrincipal(ctx, q, *decision.DecidedBy)
			if err != nil {
				return nil, err
			}
			out.WaivedBy = &who.Handle
		}
	}
	return out, nil
}

// limitOfStage — предел этапа, к которому относится прогон: у перепроверки
// это родительский этап (её время входит во время этапа), у этапа — он сам.
func (e *Engine) limitOfStage(ctx context.Context, q postgres.Querier, run domain.StageRun, now time.Time) (*StageLimit, error) {
	if run.Kind == domain.StageKindReview && run.ParentRunID != nil {
		parent, err := e.store.GetStageRun(ctx, q, *run.ParentRunID)
		if err != nil {
			return nil, err
		}
		run = parent
	}
	return e.limitFor(ctx, q, run, now)
}

// StageLimit — предел текущего этапа задачи для карточки, предпросмотра
// переходов и sdlc_check_gates. nil — предела нет.
func (e *Engine) StageLimit(ctx context.Context, task domain.Task) (*StageLimit, error) {
	q := e.store.DB()
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return nil, err
	}
	return e.limitFor(ctx, q, run, time.Now())
}

// stageTimeAllows отбивает новый захват этапа, открытого дольше предела.
//
// Отбиваются исполнители, а не человек (принцип CLAB-192): принципал вида
// agent, раннер и любой захват токеном задачи. Человек под своей сессией
// захват получает и видит в ответе stage_limit.overdue. Подтверждение живой
// аренды тем же держателем и перепроверки сюда не доходят: агента посреди
// этапа не останавливаем, у ревью свой предел (CLAB-218). Срок считается по
// часам, отметки воркера отказ не ждёт.
func (e *Engine) stageTimeAllows(ctx context.Context, tx pgx.Tx, task domain.Task, run domain.StageRun, actor uuid.UUID, byTaskToken bool) error {
	limit, err := e.limitFor(ctx, tx, run, time.Now())
	if err != nil || !limit.Holds() {
		return err
	}
	if !byTaskToken {
		who, err := e.store.GetPrincipal(ctx, tx, actor)
		if err != nil {
			return err
		}
		if who.Kind == domain.PrincipalUser {
			return nil
		}
	}
	elapsed := (time.Duration(limit.ElapsedSeconds) * time.Second).String()
	return apperr.New(apperr.CodeStageTimeExceeded,
		"этап %s задачи %s открыт дольше предела: предел %s, прошло %s с входа переходом %s; "+
			"снять предел может человек — решение waived по гейту %q перехода %q",
		limit.Status, task.Key, limit.MaxDuration, elapsed, limit.Transition,
		limit.Gate, limit.Transition).
		WithDetails(map[string]any{
			"task":            task.Key,
			"stage_run_id":    run.ID,
			"gate":            limit.Gate,
			"transition":      limit.Transition,
			"status":          limit.Status,
			"max_duration":    limit.MaxDuration,
			"entered_at":      limit.EnteredAt,
			"deadline":        limit.Deadline,
			"elapsed_seconds": limit.ElapsedSeconds,
		})
}

// enteredVia проверяет решение по гейту stage_time_limit: оно снимает предел
// текущего этапа и имеет смысл только на переходе, которым этот этап открыт.
// Решение по такому гейту чужого перехода ничего бы не сняло, а лишь легло
// бы в gate_results ложным следом.
func enteredVia(task domain.Task, run domain.StageRun, transition domain.Transition, gate domain.Gate) error {
	if gate.Type != domain.GateStageTimeLimit {
		return nil
	}
	if run.EnteredViaTransitionID != nil && *run.EnteredViaTransitionID == transition.ID {
		return nil
	}
	return apperr.InvalidInput(
		"гейт %q перехода %q снимает предел этапа, открытого этим переходом, а текущий этап "+
			"задачи %s в статусе %s открыт не им; снимать предел нужно на переходе входа (clab gates %s)",
		gate.Key, transition.Key, task.Key, run.StatusKey, task.Key)
}

// MarkOverdueStages отмечает просроченные этапы и пишет в ленту по одному
// событию stage.overdue на прогон. Задачу не двигает, аренду не трогает:
// решение за человеком (CLAB-190 подписывается на это событие). limit —
// сколько этапов отметить за проход; остальные — на следующем тике.
func (e *Engine) MarkOverdueStages(ctx context.Context, system uuid.UUID, limit int) (int, error) {
	var n int
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		runs, err := e.store.TimeLimitedRuns(ctx, tx)
		if err != nil {
			return err
		}
		now := time.Now()
		for _, r := range runs {
			if n >= limit {
				break
			}
			d, text, err := maxDuration(r.GateConfig)
			if err != nil {
				continue
			}
			deadline, overdue := stageClock(r.EnteredAt, d, now)
			if !overdue {
				continue
			}
			marked, err := e.store.MarkStageOverdue(ctx, tx, r.RunID, now)
			if err != nil {
				return err
			}
			if !marked {
				continue
			}
			if err := e.store.AppendEvent(ctx, tx, r.TaskID, &r.RunID, domain.EventStageOverdue, system,
				map[string]any{
					"gate":            r.GateKey,
					"transition":      r.TransitionKey,
					"status":          r.StatusKey,
					"max_duration":    text,
					"elapsed_seconds": int64(now.Sub(r.EnteredAt) / time.Second),
					"entered_at":      r.EnteredAt,
					"deadline":        deadline,
				}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
