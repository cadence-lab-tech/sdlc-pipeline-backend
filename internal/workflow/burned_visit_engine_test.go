package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// CLAB-288, критерий 12 (R6, решение человека по Q1): заход и попытка —
// разные вещи. Аренда сгорела, воркер вернул прогон в pending, тот же
// принципал забрал этап снова — это тот же прогон, та же попытка и то же
// entered_at: предел попыток (CLAB-217) сгоревший заход не тратит, а время
// этапа (CLAB-221) он не сбрасывает — оно считается от entered_at.
func TestBurnedVisitIsNotAnAttempt(t *testing.T) {
	ctx := context.Background()
	w := newBurnedWorld(t)
	key := w.task(t, "Сгоревший заход")
	w.move(t, key, "start")
	task, run := w.current(t, key)
	if run.Attempt != 1 {
		t.Fatalf("первый вход: попытка %d", run.Attempt)
	}
	nextBefore, err := w.store.NextStageAttempt(ctx, w.store.DB(), task.ID, task.StatusID)
	if err != nil {
		t.Fatal(err)
	}

	orchestrator := mustActor(t, w.store, "user:orchestrator")
	claim := func(session string) uuid.UUID {
		t.Helper()
		res, err := w.engine.Claim(ctx, key, orchestrator.ID, session)
		if err != nil {
			t.Fatalf("захват %s: %v", session, err)
		}
		return res.Lease.StageRunID
	}
	burn := func() {
		t.Helper()
		if _, err := w.store.DB().Exec(ctx,
			`UPDATE stage_runs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, run.ID); err != nil {
			t.Fatal(err)
		}
		if n, err := w.engine.ExpireLeases(ctx, w.system.ID, 10); err != nil || n != 1 {
			t.Fatalf("протухание: %d %v", n, err)
		}
	}

	// Два сгоревших захода подряд и третий, живой.
	for i, session := range []string{"заход-1", "заход-2", "заход-3"} {
		if got := claim(session); got != run.ID {
			t.Fatalf("%s взял прогон %s, а не %s", session, got, run.ID)
		}
		if i < 2 {
			burn()
		}
	}

	task, again := w.current(t, key)
	if again.ID != run.ID || again.Attempt != run.Attempt || !again.EnteredAt.Equal(run.EnteredAt) {
		t.Fatalf("после сгоревших заходов: прогон %s попытка %d entered_at %s, было %s %d %s",
			again.ID, again.Attempt, again.EnteredAt, run.ID, run.Attempt, run.EnteredAt)
	}
	if again.ClaimedBy == nil || *again.ClaimedBy != orchestrator.ID {
		t.Fatalf("аренда третьего захода: %v", again.ClaimedBy)
	}
	nextAfter, err := w.store.NextStageAttempt(ctx, w.store.DB(), task.ID, task.StatusID)
	if err != nil || nextAfter != nextBefore {
		t.Fatalf("NextStageAttempt %d → %d (%v): сгоревший заход открыл попытку", nextBefore, nextAfter, err)
	}
	if n := w.devRuns(t, task); n != 1 {
		t.Fatalf("прогонов development %d, ждали 1", n)
	}

	// Лента: два lease.expired и три stage.claimed — все с попыткой 1.
	events, err := w.store.ListEvents(ctx, w.store.DB(), task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, ev := range events {
		if ev.StageRunID == nil || *ev.StageRunID != run.ID {
			continue
		}
		switch ev.Type {
		case domain.EventLeaseExpired, domain.EventStageClaimed:
			counts[ev.Type]++
			if p := eventPayload(t, ev); p["attempt"] != float64(1) {
				t.Errorf("%s с попыткой %v", ev.Type, p["attempt"])
			}
		}
	}
	if counts[domain.EventLeaseExpired] != 2 || counts[domain.EventStageClaimed] != 3 {
		t.Fatalf("лента прогона: %v", counts)
	}

	// Гейт attempts_limit (предел 2) видит ту же попытку: откат с проверки —
	// вторая попытка, в пределе.
	w.move(t, key, "dev_done")
	task, _ = w.current(t, key)
	if out := gateOutcome(t, ctx, w.engine, task, "reject_to_development", "attempts"); out.Status != domain.GateResultPassed {
		t.Fatalf("предел попыток после сгоревших заходов: %+v", out)
	}
	res := w.move(t, key, "reject_to_development")
	if res.NewRun == nil || res.NewRun.Attempt != 2 {
		t.Fatalf("откат после сгоревших заходов: %+v", res.NewRun)
	}
}

// newBurnedWorld — цикл предела попыток (attemptsCycle) с ролью на
// development: захват этапа без роли исполнителя невозможен.
func newBurnedWorld(t *testing.T) attemptsWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	cycle := attemptsCycle()
	for i := range cycle.Statuses {
		if cycle.Statuses[i].Key == "development" {
			cycle.Statuses[i].Role = "developer"
		}
	}
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{cycle}}, false)
	if err != nil || len(report.Validations) != 1 || !report.Validations[0].Valid {
		t.Fatalf("импорт цикла: %+v %v", report.Validations, err)
	}
	id, err := uuid.Parse(report.Workflows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.Activate(ctx, id, system.ID); err != nil {
		t.Fatalf("активация: %v", err)
	}
	return attemptsWorld{store: store, engine: engine, system: system}
}

func eventPayload(t *testing.T, ev domain.TaskEvent) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}
