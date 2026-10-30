package workflow

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Гейт stage_time_limit запрошенного перехода при пропуске выключенного
// этапа (CLAB-333, решение 0066): предел перехода не действует, действует
// предел последнего обходного. Мир — skipCycle: analysis_done в design (1h),
// design_done в docs (без предела), docs_done в development (1h).

// timeGate — исход гейта «time» перехода analysis_done в предпросмотре.
func (w skipWorld) timeGate(t *testing.T, task domain.Task) (TransitionPreview, GateOutcome) {
	t.Helper()
	previews, err := w.engine.Preview(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		if p.Key != "analysis_done" {
			continue
		}
		for _, g := range p.Gates {
			if g.GateKey == "time" {
				return p, g
			}
		}
	}
	t.Fatal("нет гейта time у analysis_done")
	return TransitionPreview{}, GateOutcome{}
}

// recorded — вердикт, записанный в gate_results прогона analysis.
func (w skipWorld) recorded(t *testing.T, task domain.Task, run domain.StageRun) domain.GateResult {
	t.Helper()
	ctx := context.Background()
	var gateID string
	if err := w.store.DB().QueryRow(ctx, `
		SELECT g.id FROM gates g JOIN workflow_transitions tr ON tr.id = g.transition_id
		WHERE tr.workflow_id = $1 AND tr.key = 'analysis_done' AND g.key = 'time'`, task.WorkflowID).Scan(&gateID); err != nil {
		t.Fatal(err)
	}
	results, err := w.store.ListGateResults(ctx, w.store.DB(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.GateID.String() == gateID {
			return r
		}
	}
	t.Fatalf("в gate_results нет гейта time: %+v", results)
	return domain.GateResult{}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// AC-1, AC-2, AC-3, AC-4: вердикт при пропуске, его ключевое в первых 100
// символах и совпадение с фактическим пределом прогона на посадке.
func TestSkipStageLimitVerdict(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)

	cases := []struct {
		name, stages string
		lands, via   string
		skips        []string
		effective    string // пусто — предела на месте посадки нет
		wantInReason []string
	}{
		{"обход без предела", `{"design": false, "docs": true}`, "docs", "design_done",
			[]string{"design"}, "", []string{"не действует", "design", "docs", "предела там нет", "design_done"}},
		{"цепочка с пределом обходного", `{"design": false, "docs": false}`, "development", "docs_done",
			[]string{"design", "docs"}, "1h", []string{"не действует", "design, docs", "development", "предел 1h", "docs_done"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := w.create(t, c.name, "", c.stages)
			w.move(t, task.Key, "start")
			cur, run := w.current(t, task.Key)

			p, g := w.timeGate(t, cur)
			if p.LandsIn == nil || *p.LandsIn != c.lands || !reflect.DeepEqual(p.Skips, c.skips) {
				t.Fatalf("переход: %+v", p)
			}
			if g.Status != domain.GateResultPassed {
				t.Fatalf("вердикт при пропуске: %+v", g)
			}
			want := map[string]any{
				"applies": false, "status": "design", "skips": c.skips,
				"lands_in": c.lands, "via": c.via,
			}
			if c.effective != "" {
				want["effective_max_duration"] = c.effective
			}
			if jsonOf(t, g.Details) != jsonOf(t, want) {
				t.Fatalf("details: %s, ждали %s", jsonOf(t, g.Details), jsonOf(t, want))
			}
			if _, has := g.Details["max_duration"]; has {
				t.Fatalf("max_duration описывает предел, который не действует: %v", g.Details)
			}
			if strings.Contains(g.Reason, "предел этапа в статусе") {
				t.Fatalf("прежний текст про пропущенный этап: %q", g.Reason)
			}
			for _, s := range c.wantInReason {
				if !strings.Contains(g.Reason, s) {
					t.Errorf("в reason нет %q: %q", s, g.Reason)
				}
			}
			// AC-3: ключевое помещается в 100 символов — столько печатает
			// clab gates.
			head := string([]rune(g.Reason)[:min(100, len([]rune(g.Reason)))])
			for _, s := range append([]string{"не действует", c.lands}, c.skips...) {
				if !strings.Contains(head, s) {
					t.Errorf("в первых 100 символах нет %q: %q", s, head)
				}
			}

			// AC-4: после перехода предел прогона на посадке — ровно тот,
			// что назван в гейте; в gate_results лёг тот же вердикт.
			res := w.move(t, task.Key, "analysis_done")
			if res.Task.StatusKey != c.lands || !reflect.DeepEqual(res.Skips, c.skips) {
				t.Fatalf("посадка: %s %v", res.Task.StatusKey, res.Skips)
			}
			var got GateOutcome
			for _, o := range res.Gates {
				if o.GateKey == "time" {
					got = o
				}
			}
			if got.Reason != g.Reason || jsonOf(t, got.Details) != jsonOf(t, g.Details) || got.Status != g.Status {
				t.Fatalf("результат перехода %+v расходится с предпросмотром %+v", got, g)
			}
			rec := w.recorded(t, cur, run)
			// jsonb переставляет ключи: сравниваем разобранное.
			var recDetails, wantDetails map[string]any
			if err := json.Unmarshal(rec.Details, &recDetails); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(jsonOf(t, g.Details)), &wantDetails); err != nil {
				t.Fatal(err)
			}
			if rec.Status != g.Status || !reflect.DeepEqual(recDetails, wantDetails) {
				t.Fatalf("gate_results %s %s расходится с предпросмотром", rec.Status, rec.Details)
			}
			landed, _ := w.current(t, task.Key)
			limit, err := w.engine.StageLimit(ctx, landed)
			if err != nil {
				t.Fatal(err)
			}
			if c.effective == "" {
				if limit != nil {
					t.Fatalf("гейт сказал «предела нет», а предел есть: %+v", limit)
				}
				return
			}
			if limit == nil || limit.Transition != c.via || limit.MaxDuration != c.effective {
				t.Fatalf("stage_limit %+v расходится с гейтом (%s, %s)", limit, c.via, c.effective)
			}
		})
	}
}

// Конфиг гейта запрошенного перехода при пропуске не читается: негодный
// max_duration не меняет вердикта (не действует из-за пропуска).
func TestSkipStageLimitIgnoresRequestedConfig(t *testing.T) {
	w := newSkipWorld(t)
	task := w.create(t, "Негодный конфиг", "", `{"design": false, "docs": true}`)
	w.move(t, task.Key, "start")
	cur, _ := w.current(t, task.Key)
	_, before := w.timeGate(t, cur)
	if _, err := w.store.DB().Exec(context.Background(), `
		UPDATE gates SET config = '{"max_duration": "abc"}'
		WHERE key = 'time' AND transition_id = (
			SELECT id FROM workflow_transitions WHERE workflow_id = $1 AND key = 'analysis_done')`, cur.WorkflowID); err != nil {
		t.Fatal(err)
	}
	_, after := w.timeGate(t, cur)
	if after.Reason != before.Reason || jsonOf(t, after.Details) != jsonOf(t, before.Details) {
		t.Fatalf("вердикт зависит от config запрошенного перехода: %+v / %+v", before, after)
	}
}

// AC-6: без пропуска вердикт прежний байт в байт; недоступный возврат в
// выключенный этап тоже считается как раньше.
func TestSkipStageLimitUnchangedWithoutSkip(t *testing.T) {
	w := newSkipWorld(t)
	task := w.create(t, "Без пропуска", "", `{"design": true}`)
	w.move(t, task.Key, "start")
	cur, _ := w.current(t, task.Key)
	p, g := w.timeGate(t, cur)
	if len(p.Skips) != 0 || g.Reason != "предел этапа в статусе design — 1h, отсчёт с входа" ||
		jsonOf(t, g.Details) != `{"max_duration":"1h","status":"design"}` {
		t.Fatalf("без пропуска: %+v %+v", p, g)
	}

	// Недоступный возврат: назначения нет, пропусков нет — прежний текст.
	e := w.engine
	tr, err := w.store.GetTransitionByKey(context.Background(), w.store.DB(), cur.WorkflowID, "analysis_done")
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.evalStageTimeLimit(context.Background(), w.store.DB(), EvalInput{
		Task: cur, Transition: tr, Gate: *stageTimeGate(tr),
		Landing: landing{Unavailable: UnavailableStageDisabled, Disabled: "design", Skipped: []domain.Status{}}})
	if err != nil || out.Reason != g.Reason || jsonOf(t, out.Details) != jsonOf(t, g.Details) {
		t.Fatalf("недоступный переход: %v %+v", err, out)
	}
}
