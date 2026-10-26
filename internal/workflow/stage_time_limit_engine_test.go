package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Интеграционные проверки предела времени этапа (CLAB-221) по настоящей
// схеме: переход входа у прогона, отказ захвата, снятие человеком и проход
// воркера.

// timedCycle — в development задача входит четырьмя переходами: start и
// rework без предела, reject_to_development с пределом limit и reopen с
// пределом 3h. Отсчёт и снятие у каждого входа свои.
func timedCycle(limit string) BundleWorkflow {
	yes := true
	timed := func(d string) []BundleGate {
		return []BundleGate{{Key: "time", Name: "Предел этапа", Type: domain.GateStageTimeLimit,
			Blocking: &yes, Config: json.RawMessage(`{"max_duration": "` + d + `"}`)}}
	}
	return BundleWorkflow{
		Name: "Timed", TaskTypes: []string{"task"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
			{Key: "development", Name: "Development", Category: "active", Role: "developer"},
			{Key: "review", Name: "Review", Category: "active", Role: "qa"},
			{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
			{Key: "cancelled", Name: "Cancelled", Category: "cancelled", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "Start", From: "new", To: "development", Kind: "advance"},
			{Key: "dev_done", Name: "Dev done", From: "development", To: "review", Kind: "advance"},
			{Key: "reject_to_development", Name: "Back", From: "review", To: "development", Kind: "reject", Gates: timed(limit)},
			{Key: "reopen", Name: "Reopen", From: "review", To: "development", Kind: "reject", Gates: timed("3h")},
			{Key: "rework", Name: "Rework", From: "review", To: "development", Kind: "reject"},
			{Key: "finish", Name: "Finish", From: "review", To: "done", Kind: "advance"},
			{Key: "cancel", Name: "Cancel", To: "cancelled", Kind: "cancel"},
		},
	}
}

type timedWorld struct {
	attemptsWorld
	agent, other, human domain.Principal
}

func newTimedWorld(t *testing.T, limit string) timedWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{timedCycle(limit)}}, false)
	if err != nil {
		t.Fatalf("импорт цикла: %v", err)
	}
	if len(report.Validations) != 1 || !report.Validations[0].Valid {
		t.Fatalf("цикл с пределом %s не прошёл проверку: %+v", limit, report.Validations)
	}
	id, err := uuid.Parse(report.Workflows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.Activate(ctx, id, system.ID); err != nil {
		t.Fatalf("активация цикла с пределом: %v", err)
	}
	return timedWorld{
		attemptsWorld: attemptsWorld{store: store, engine: engine, system: system},
		agent:         mustActor(t, store, "agent:runner"),
		other:         mustActor(t, store, "agent:other"),
		human:         mustActor(t, store, "user:lead"),
	}
}

// backdate сдвигает вход прогона в прошлое: часы этапа — stage_runs.entered_at,
// и «прошло два часа» проверяется без двух часов ожидания.
func (w timedWorld) backdate(t *testing.T, run domain.StageRun, by time.Duration) {
	t.Helper()
	if _, err := w.store.DB().Exec(context.Background(),
		`UPDATE stage_runs SET entered_at = entered_at - $2::interval WHERE id = $1`,
		run.ID, by.String()); err != nil {
		t.Fatal(err)
	}
}

func (w timedWorld) transitionID(t *testing.T, task domain.Task, key string) uuid.UUID {
	t.Helper()
	tr, err := w.store.GetTransitionByKey(context.Background(), w.store.DB(), task.WorkflowID, key)
	if err != nil {
		t.Fatal(err)
	}
	return tr.ID
}

func (w timedWorld) events(t *testing.T, task domain.Task, run domain.StageRun, typ string) []domain.TaskEvent {
	t.Helper()
	all, err := w.store.ListEvents(context.Background(), w.store.DB(), task.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.TaskEvent
	for _, ev := range all {
		if ev.Type == typ && ev.StageRunID != nil && *ev.StageRunID == run.ID {
			out = append(out, ev)
		}
	}
	return out
}

func (w timedWorld) limit(t *testing.T, key string) *StageLimit {
	t.Helper()
	task, _ := w.current(t, key)
	l, err := w.engine.StageLimit(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Критерии 7, 8, 10, 11, 16: прогон помнит переход входа, воркер по сроку
// пишет одно stage.overdue с цифрами и отметкой, задача и прогон остаются
// где были; закрытый до срока и открытый без предела прогоны события не
// получают.
func TestStageTimeLimitWorkerMarksOverdueOnce(t *testing.T) {
	ctx := context.Background()
	w := newTimedWorld(t, "2s")
	key := w.task(t, "Просрочка")

	// Начальный статус — без перехода входа.
	_, initial := w.current(t, key)
	if initial.EnteredViaTransitionID != nil {
		t.Fatalf("у прогона начального статуса есть переход входа: %v", initial.EnteredViaTransitionID)
	}
	// Вход переходом без гейта: переход помнится, предела нет.
	w.move(t, key, "start")
	task, run := w.current(t, key)
	if run.EnteredViaTransitionID == nil || *run.EnteredViaTransitionID != w.transitionID(t, task, "start") {
		t.Fatalf("переход входа start не записан: %v", run.EnteredViaTransitionID)
	}
	if l := w.limit(t, key); l != nil {
		t.Fatalf("вход без гейта дал предел: %+v", l)
	}

	// На самом переходе гейт ничего не держит.
	w.move(t, key, "dev_done")
	task, _ = w.current(t, key)
	if out := gateOutcome(t, ctx, w.engine, task, "reject_to_development", "time"); out.Status != domain.GateResultPassed ||
		!strings.Contains(out.Reason, "предел этапа в статусе development — 2s") {
		t.Fatalf("гейт на своём переходе: %+v", out)
	}
	w.move(t, key, "reject_to_development")
	task, run = w.current(t, key)
	if run.EnteredViaTransitionID == nil || *run.EnteredViaTransitionID != w.transitionID(t, task, "reject_to_development") {
		t.Fatalf("переход входа reject_to_development не записан: %v", run.EnteredViaTransitionID)
	}
	if l := w.limit(t, key); l == nil || l.Overdue || l.MaxDuration != "2s" || l.Transition != "reject_to_development" ||
		l.Gate != "time" || l.Status != "development" || !l.Deadline.Equal(run.EnteredAt.Add(2*time.Second)) {
		t.Fatalf("предел до срока: %+v", l)
	}

	// Вторая задача закрывает этап до срока — события не будет.
	closed := w.task(t, "Успела")
	w.move(t, closed, "start", "dev_done", "reject_to_development")
	_, closedRun := w.current(t, closed)
	w.move(t, closed, "dev_done")

	// До срока воркер молчит.
	if n, err := w.engine.MarkOverdueStages(ctx, w.system.ID, 100); err != nil || n != 0 {
		t.Fatalf("проход до срока: %d %v", n, err)
	}
	time.Sleep(time.Until(run.EnteredAt.Add(2100 * time.Millisecond)))

	if n, err := w.engine.MarkOverdueStages(ctx, w.system.ID, 100); err != nil || n != 1 {
		t.Fatalf("проход после срока отметил %d: %v", n, err)
	}
	for range 2 {
		if n, err := w.engine.MarkOverdueStages(ctx, w.system.ID, 100); err != nil || n != 0 {
			t.Fatalf("повторный проход отметил %d: %v", n, err)
		}
	}
	evs := w.events(t, task, run, domain.EventStageOverdue)
	if len(evs) != 1 {
		t.Fatalf("событий stage.overdue: %d", len(evs))
	}
	var p struct {
		Gate, Transition, Status string
		MaxDuration              string    `json:"max_duration"`
		Elapsed                  int64     `json:"elapsed_seconds"`
		EnteredAt                time.Time `json:"entered_at"`
		Deadline                 time.Time `json:"deadline"`
	}
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Gate != "time" || p.Transition != "reject_to_development" || p.Status != "development" ||
		p.MaxDuration != "2s" || p.Elapsed < 2 || !p.EnteredAt.Equal(run.EnteredAt) ||
		!p.Deadline.Equal(run.EnteredAt.Add(2*time.Second)) {
		t.Fatalf("payload stage.overdue: %s", evs[0].Payload)
	}
	if evs[0].ActorID == nil || *evs[0].ActorID != w.system.ID {
		t.Fatalf("stage.overdue не от системного принципала: %v", evs[0].ActorID)
	}

	// Отметка стоит, статус и прогон прежние, ничего не двигалось.
	task, after := w.current(t, key)
	if after.ID != run.ID || after.OverdueAt == nil || task.StatusKey != "development" || after.FinishedAt != nil ||
		!after.EnteredAt.Equal(run.EnteredAt) {
		t.Fatalf("после просрочки: статус %s, прогон %+v", task.StatusKey, after)
	}
	for _, typ := range []string{domain.EventTaskAdvanced, domain.EventTaskRejected, domain.EventLeaseExpired} {
		if evs := w.events(t, task, run, typ); len(evs) != 0 {
			t.Fatalf("просрочка вызвала %s: %d", typ, len(evs))
		}
	}
	if l := w.limit(t, key); l == nil || !l.Overdue || l.OverdueAt == nil || l.RemainingSeconds != 0 {
		t.Fatalf("предел после просрочки: %+v", l)
	}
	if evs := w.events(t, task, closedRun, domain.EventStageOverdue); len(evs) != 0 {
		t.Fatal("закрытый до срока прогон получил stage.overdue")
	}
}

// Критерии 9, 12–15, 17–19: часы не сбрасываются арендой; после срока
// агенты отбиваются кодом stage_time_exceeded и до прохода воркера, держатель
// живой аренды работает дальше, человек захват получает; снимает предел
// только человек и только waived, после снятия агенты снова забирают этап.
func TestStageTimeLimitRefusesAgentsUntilHumanWaives(t *testing.T) {
	ctx := context.Background()
	w := newTimedWorld(t, "1h")
	key := w.task(t, "Отказ захвата")
	w.move(t, key, "start", "dev_done", "reject_to_development")
	task, run := w.current(t, key)
	entered := run.EnteredAt

	// До срока: захват, heartbeat, release, повторный захват, протухание.
	claim := func(actor uuid.UUID, opts ClaimOptions) (ClaimResult, error) {
		return w.engine.ClaimWith(ctx, key, actor, "test", opts)
	}
	res, err := claim(w.agent.ID, ClaimOptions{})
	if err != nil || res.StageLimit == nil || res.StageLimit.Overdue {
		t.Fatalf("захват до срока: %+v %v", res.StageLimit, err)
	}
	if _, err := w.engine.Heartbeat(ctx, run.ID, w.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.engine.Release(ctx, run.ID, w.agent.ID, "перерыв"); err != nil {
		t.Fatal(err)
	}
	if _, err := claim(w.agent.ID, ClaimOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.DB().Exec(ctx,
		`UPDATE stage_runs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := w.engine.ExpireLeases(ctx, w.system.ID, 100); err != nil || n != 1 {
		t.Fatalf("протухание аренды: %d %v", n, err)
	}
	if _, after := w.current(t, key); after.ID != run.ID || !after.EnteredAt.Equal(entered) {
		t.Fatalf("аренда сдвинула часы этапа: %v → %v (%s)", entered, after.EnteredAt, after.ID)
	}

	// Агент держит живую аренду, когда выходит срок.
	if _, err := claim(w.agent.ID, ClaimOptions{}); err != nil {
		t.Fatal(err)
	}
	w.backdate(t, run, 2*time.Hour)
	_, run = w.current(t, key)
	if l := w.limit(t, key); l == nil || !l.Overdue || l.OverdueAt != nil {
		t.Fatalf("просрочка по часам, до воркера: %+v", l)
	}
	// Его не останавливают: подтверждение аренды, heartbeat, сдача.
	if res, err := claim(w.agent.ID, ClaimOptions{}); err != nil || !res.StageLimit.Overdue {
		t.Fatalf("подтверждение живой аренды держателем: %+v %v", res.StageLimit, err)
	}
	if _, err := w.engine.Heartbeat(ctx, run.ID, w.agent.ID); err != nil {
		t.Fatalf("heartbeat после срока: %v", err)
	}
	if _, err := w.engine.SubmitArtifact(ctx, key, w.agent.ID, SubmitArtifactInput{
		Kind: "dev.change_summary", Title: "Разбор", Format: "markdown",
		Content: []byte("сделано"), StageRunID: &run.ID}); err != nil {
		t.Fatalf("сдача после срока: %v", err)
	}

	// Новая аренда агента — отказ, и до прохода воркера тоже.
	if err := w.engine.Release(ctx, run.ID, w.agent.ID, "сдал"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []uuid.UUID{w.agent.ID, w.other.ID} {
		_, err := claim(actor, ClaimOptions{})
		e := apperr.As(err)
		if e.Code != apperr.CodeStageTimeExceeded || !strings.Contains(e.Message, key) ||
			!strings.Contains(e.Message, "1h") || !strings.Contains(e.Message, "waived") {
			t.Fatalf("захват агентом после срока: %v", err)
		}
		d, _ := e.Details.(map[string]any)
		if d["stage_run_id"] != run.ID || d["gate"] != "time" || d["transition"] != "reject_to_development" ||
			d["max_duration"] != "1h" || d["deadline"] == nil {
			t.Fatalf("details отказа: %+v", e.Details)
		}
	}
	// Человек под своей сессией захват получает и видит просрочку.
	res, err = claim(w.human.ID, ClaimOptions{})
	if err != nil || res.StageLimit == nil || !res.StageLimit.Overdue || res.StageLimit.Waived {
		t.Fatalf("захват человеком: %+v %v", res.StageLimit, err)
	}
	if err := w.engine.Release(ctx, run.ID, w.human.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Токен задачи — исполнитель, даже если аренду держал человек: новой
	// аренды им не взять.
	if _, err := claim(w.human.ID, ClaimOptions{OnlyRun: &run.ID}); apperr.As(err).Code != apperr.CodeStageTimeExceeded {
		t.Fatalf("новая аренда токеном задачи: %v", err)
	}

	decide := func(transition string, actor uuid.UUID, bound *TaskBinding, status string) error {
		_, err := w.engine.DecideGate(ctx, key, transition, "time", actor, bound, status, json.RawMessage(`{}`))
		return err
	}
	// Агент и токен задачи снять не могут, человек — только waived и только
	// на переходе входа.
	bound := &TaskBinding{TaskID: task.ID, StageRunID: run.ID, TaskKey: key}
	if err := decide("reject_to_development", w.agent.ID, bound, domain.GateResultWaived); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("токен задачи снял предел: %v", err)
	}
	if err := decide("reject_to_development", w.agent.ID, nil, domain.GateResultWaived); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент снял предел: %v", err)
	}
	for _, status := range []string{domain.GateResultPassed, domain.GateResultFailed} {
		if err := decide("reject_to_development", w.human.ID, nil, status); apperr.As(err).Code != apperr.CodeInvalidInput ||
			!strings.Contains(err.Error(), "waived") {
			t.Fatalf("%s по пределу этапа: %v", status, err)
		}
	}
	if err := decide("reopen", w.human.ID, nil, domain.GateResultWaived); apperr.As(err).Code != apperr.CodeInvalidInput ||
		!strings.Contains(err.Error(), "открыт не им") {
		t.Fatalf("снятие на чужом переходе входа: %v", err)
	}
	var rows int
	if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM gate_results WHERE stage_run_id = $1`, run.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("отказы записали решения: %d строк gate_results", rows)
	}

	// Человек снимает предел: решение в gate_results и ленте от его имени,
	// агенты снова забирают этап до конца прогона.
	if err := decide("reject_to_development", w.human.ID, nil, domain.GateResultWaived); err != nil {
		t.Fatalf("снятие человеком: %v", err)
	}
	gateID := stageTimeGate(mustTransition(t, w, task, "reject_to_development")).ID
	if r, err := w.store.GateDecision(ctx, w.store.DB(), run.ID, gateID); err != nil || r == nil ||
		r.Status != domain.GateResultWaived || r.DecidedBy == nil || *r.DecidedBy != w.human.ID {
		t.Fatalf("gate_results после снятия: %+v %v", r, err)
	}
	if evs := w.events(t, task, run, domain.EventGateDecided); len(evs) != 1 || *evs[0].ActorID != w.human.ID {
		t.Fatalf("gate.decided после снятия: %+v", evs)
	}
	res, err = claim(w.other.ID, ClaimOptions{})
	if err != nil || !res.StageLimit.Waived || res.StageLimit.WaivedBy == nil || *res.StageLimit.WaivedBy != w.human.Handle {
		t.Fatalf("захват агентом после снятия: %+v %v", res.StageLimit, err)
	}
	// Снятый предел воркер не отмечает.
	if n, err := w.engine.MarkOverdueStages(ctx, w.system.ID, 100); err != nil || n != 0 {
		t.Fatalf("воркер отметил снятый предел: %d %v", n, err)
	}

	// Критерий 21: новый вход тем же переходом — новый отсчёт, снятие не
	// переносится.
	w.move(t, key, "dev_done", "reject_to_development")
	_, next := w.current(t, key)
	w.backdate(t, next, 2*time.Hour)
	if l := w.limit(t, key); l == nil || l.Waived || !l.Overdue || l.StageRunID != next.ID {
		t.Fatalf("новый вход унаследовал снятие: %+v", l)
	}
	if _, err := claim(w.agent.ID, ClaimOptions{}); apperr.As(err).Code != apperr.CodeStageTimeExceeded {
		t.Fatalf("новый вход после срока: %v", err)
	}
	// Критерий 22: из просроченного этапа ведут другим переходом и отменяют.
	w.move(t, key, "dev_done")
	w.move(t, key, "reopen")
	_, reopened := w.current(t, key)
	w.backdate(t, reopened, 4*time.Hour)
	w.move(t, key, "cancel")
	if task, _ := w.current(t, key); task.StatusKey != "cancelled" {
		t.Fatalf("отмена просроченного этапа: %s", task.StatusKey)
	}
}

func mustTransition(t *testing.T, w timedWorld, task domain.Task, key string) domain.Transition {
	t.Helper()
	tr, err := w.store.GetTransitionByKey(context.Background(), w.store.DB(), task.WorkflowID, key)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// Критерии 6, 11, 25: вход переходом без гейта — даже когда у другого входа
// в тот же статус предел есть — и прогон до миграции предела не имеют: воркер
// молчит, захват проходит после любого времени.
func TestStageTimeLimitOnlyForGatedEntry(t *testing.T) {
	ctx := context.Background()
	w := newTimedWorld(t, "1h")
	key := w.task(t, "Без предела")
	w.move(t, key, "start", "dev_done", "rework")
	_, run := w.current(t, key)
	w.backdate(t, run, 1000*time.Hour)
	if l := w.limit(t, key); l != nil {
		t.Fatalf("вход rework без гейта дал предел: %+v", l)
	}
	if n, err := w.engine.MarkOverdueStages(ctx, w.system.ID, 100); err != nil || n != 0 {
		t.Fatalf("воркер отметил этап без предела: %d %v", n, err)
	}
	if _, err := w.engine.Claim(ctx, key, w.agent.ID, "test"); err != nil {
		t.Fatalf("захват этапа без предела: %v", err)
	}

	// Прогон «до миграции»: вошёл переходом с гейтом, но перехода не помнит.
	old := w.task(t, "До миграции")
	w.move(t, old, "start", "dev_done", "reject_to_development")
	_, oldRun := w.current(t, old)
	if _, err := w.store.DB().Exec(ctx,
		`UPDATE stage_runs SET entered_via_transition_id = NULL WHERE id = $1`, oldRun.ID); err != nil {
		t.Fatal(err)
	}
	w.backdate(t, oldRun, 1000*time.Hour)
	if l := w.limit(t, old); l != nil {
		t.Fatalf("прогон без перехода входа дал предел: %+v", l)
	}
	if n, err := w.engine.MarkOverdueStages(ctx, w.system.ID, 100); err != nil || n != 0 {
		t.Fatalf("воркер отметил прогон без перехода входа: %d %v", n, err)
	}
	if _, err := w.engine.Claim(ctx, old, w.agent.ID, "test"); err != nil {
		t.Fatalf("захват прогона без перехода входа: %v", err)
	}
}

// Критерий 15: открытая перепроверка просроченного этапа забирается
// ревьюером без отказа — у неё свой предел (CLAB-218).
func TestStageTimeLimitDoesNotRefuseReview(t *testing.T) {
	ctx := context.Background()
	w := newTimedWorld(t, "1h")
	key := w.task(t, "Перепроверка")
	w.move(t, key, "start", "dev_done", "reject_to_development")
	task, run := w.current(t, key)
	w.backdate(t, run, 2*time.Hour)
	if _, err := w.store.DB().Exec(ctx, `
		INSERT INTO stage_runs (task_id, status_id, kind, parent_run_id, gate_id, role_id, attempt, state)
		SELECT sr.task_id, sr.status_id, 'review', sr.id, g.id, ro.id, 1, 'pending'
		FROM stage_runs sr, gates g, workflow_transitions tr, roles ro
		WHERE sr.id = $1 AND g.transition_id = tr.id AND tr.workflow_id = $2
		  AND tr.key = 'reject_to_development' AND ro.key = 'reviewer' AND ro.project_id = $3`,
		run.ID, task.WorkflowID, task.ProjectID); err != nil {
		t.Fatal(err)
	}
	res, err := w.engine.Claim(ctx, key, w.agent.ID, "test")
	if err != nil || res.Kind != domain.StageKindReview {
		t.Fatalf("захват перепроверки просроченного этапа: %+v %v", res.Kind, err)
	}
	// Время перепроверки входит во время этапа: предел виден и в её ответе.
	if res.StageLimit == nil || !res.StageLimit.Overdue || res.StageLimit.StageRunID != run.ID {
		t.Fatalf("предел этапа в ответе перепроверки: %+v", res.StageLimit)
	}
}

// Критерий 4: связка переносит гейт без потерь, повторный импорт — unchanged.
func TestStageTimeLimitBundleRoundTrip(t *testing.T) {
	ctx := context.Background()
	w := newTimedWorld(t, "4h")
	authoring := NewAuthoring(w.store, testdb.Logger(), w.engine)
	exported, err := authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	var cycle *BundleWorkflow
	for i := range exported.Workflows {
		if exported.Workflows[i].Name == "Timed" {
			cycle = &exported.Workflows[i]
		}
	}
	if cycle == nil {
		t.Fatal("в экспорте нет цикла с пределом")
	}
	found := map[string]string{}
	for _, tr := range cycle.Transitions {
		for _, g := range tr.Gates {
			if g.Type == domain.GateStageTimeLimit {
				_, text, err := maxDuration(g.Config)
				if err != nil || g.Key != "time" {
					t.Fatalf("гейт %s в экспорте: %+v %v", tr.Key, g, err)
				}
				found[tr.Key] = text
			}
		}
	}
	if found["reject_to_development"] != "4h" || found["reopen"] != "3h" || len(found) != 2 {
		t.Fatalf("гейты предела в экспорте: %v", found)
	}
	again, err := authoring.Import(ctx, "SDLC", w.system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{*cycle}}, false)
	if err != nil || len(again.Workflows) != 1 || again.Workflows[0].Outcome != "unchanged" {
		t.Fatalf("повторный импорт: %+v %v", again.Workflows, err)
	}
	second, err := authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(exported.Workflows)
	b, _ := json.Marshal(second.Workflows)
	if string(a) != string(b) {
		t.Fatal("повторный экспорт не совпал с исходным")
	}
}

// Критерий 5: предел — свойство цикла, в ролях его нет ни в схеме, ни в
// связке.
func TestStageTimeLimitIsNotARoleSetting(t *testing.T) {
	ctx := context.Background()
	w := newTimedWorld(t, "4h")
	var n int
	if err := w.store.DB().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE column_name IN ('max_duration', 'stage_time_limit')
		  AND table_name IN ('roles', 'role_versions')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("у роли появилась настройка предела времени")
	}
	exported, err := NewAuthoring(w.store, testdb.Logger(), w.engine).Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := json.Marshal(exported.Roles)
	if len(exported.Roles) == 0 || strings.Contains(string(roles), "max_duration") {
		t.Fatalf("роли связки несут max_duration: %s", roles)
	}
}

// Критерий 2: черновик с негодным пределом ложится, но не активируется.
func TestStageTimeLimitInvalidDraftIsNotActivated(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{timedCycle("1d")}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Validations) != 1 || report.Validations[0].Valid {
		t.Fatalf("негодный предел прошёл проверку: %+v", report.Validations)
	}
	id, _ := uuid.Parse(report.Workflows[0].ID)
	if _, err := authoring.Activate(ctx, id, system.ID); err == nil {
		t.Fatal("цикл с max_duration 1d активирован")
	}
	head, err := store.WorkflowHead(ctx, store.DB(), id)
	if err != nil || head.State != domain.WorkflowDraft {
		t.Fatalf("черновик сменил состояние: %+v %v", head, err)
	}
}

// Критерий 27: up на базе с прогонами проходит, старые прогоны получают
// NULL; down удаляет гейты типа и колонки и возвращает CHECK к набору 000029.
func TestStageTimeLimitMigration(t *testing.T) {
	ctx := context.Background()
	w := newTimedWorld(t, "4h")
	db := w.store.DB()
	key := w.task(t, "Миграция")
	w.move(t, key, "start", "dev_done", "reject_to_development")
	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	insert := func(gateType string) error {
		_, err := db.Exec(ctx, `
			INSERT INTO gates (transition_id, key, name, type, config)
			SELECT id, 'probe_' || $1, 'probe', $1, '{"max_duration": "1h"}'
			FROM workflow_transitions WHERE key = 'rework' LIMIT 1`, gateType)
		return err
	}
	columns := func() int {
		var n int
		if err := db.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'stage_runs' AND column_name IN ('entered_via_transition_id', 'overdue_at')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := insert("no_such_gate"); err == nil {
		t.Fatal("CHECK пропустил незнакомый тип")
	}

	if _, err := db.Exec(ctx, read("000031_stage_time_limit.down.sql")); err != nil {
		t.Fatalf("down на базе с гейтами предела: %v", err)
	}
	var gates int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM gates WHERE type = 'stage_time_limit'`).Scan(&gates); err != nil {
		t.Fatal(err)
	}
	if gates != 0 || columns() != 0 {
		t.Fatalf("down оставил гейты (%d) или колонки (%d)", gates, columns())
	}
	if err := insert(domain.GateStageTimeLimit); err == nil {
		t.Fatal("после down CHECK принимает stage_time_limit")
	}
	if err := insert(domain.GateAttemptsLimit); err != nil {
		t.Fatalf("после down одиннадцатый тип не принимается: %v", err)
	}

	// up на базе с прогонами: старые получают NULL и предела не имеют.
	if _, err := db.Exec(ctx, read("000031_stage_time_limit.up.sql")); err != nil {
		t.Fatalf("up: %v", err)
	}
	var filled int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM stage_runs
		WHERE entered_via_transition_id IS NOT NULL OR overdue_at IS NOT NULL`).Scan(&filled); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM stage_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if filled != 0 || runs == 0 {
		t.Fatalf("после up у старых прогонов (%d) заполнены новые колонки: %d", runs, filled)
	}
	if err := insert(domain.GateStageTimeLimit); err != nil {
		t.Fatalf("после up stage_time_limit не принимается: %v", err)
	}
	if err := insert("no_such_gate"); err == nil {
		t.Fatal("после up CHECK пропустил незнакомый тип")
	}
}
