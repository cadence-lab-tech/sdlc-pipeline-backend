package workflow

import (
	"encoding/json"
	"math"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// CLAB-220: разрез closed_cost метрик — стоимость на закрытую задачу по
// типам. Номера в комментариях — пункты критериев приёмки задачи.

// closedTask заводит задачу и ведёт её до первого прогона роли, забранного
// принципалом a: начальный прогон без роли закрыт переходом (not_applicable),
// прогон роли открыт (missing, пока нет строки учёта).
func (w policyWorld) closedTask(title, parent string) (domain.Task, uuid.UUID) {
	w.t.Helper()
	task, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: title, ParentKey: parent},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	res, err := w.engine.Execute(w.ctx, task.Key, "start", w.system.ID, "")
	if err != nil || res.Blocked {
		w.t.Fatalf("в работу: %v %+v", err, res.Gates)
	}
	claimed := w.mustClaim(task.Key, w.a)
	return res.Task, claimed.StageRunID
}

// use добавляет строку учёта прямо в прогон: тест строит данные, а не
// проверяет захват. billing "" — строка без признака.
func (w policyWorld) use(run uuid.UUID, cost *float64, billing string) {
	w.t.Helper()
	var b *string
	if billing != "" {
		b = &billing
	}
	if _, err := w.store.DB().Exec(w.ctx, `
		INSERT INTO stage_run_usage (run_id, model, cost_usd, billing, reported_by)
		VALUES ($1, 'm', $2, $3, $4)`, run, cost, b, w.a.ID); err != nil {
		w.t.Fatal(err)
	}
}

func f(v float64) *float64 { return &v }

// extraRun — ещё один прогон того же этапа (следующая попытка), закрытый;
// claimed — забирали ли его (событие stage.claimed).
func (w policyWorld) extraRun(task domain.Task, from uuid.UUID, claimed bool) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.store.DB().QueryRow(w.ctx, `
		INSERT INTO stage_runs (task_id, status_id, role_id, attempt, state, finished_at, outcome)
		SELECT task_id, status_id, role_id, attempt + 1, 'passed', now(), 'advanced'
		FROM stage_runs WHERE id = $1 RETURNING id`, from).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	if claimed {
		if err := w.store.AppendEvent(w.ctx, w.store.DB(), task.ID, &id, domain.EventStageClaimed, w.a.ID, map[string]any{}); err != nil {
			w.t.Fatal(err)
		}
	}
	return id
}

// finish закрывает задачу: статус категории done (или cancelled) и closed_at
// ago назад.
func (w policyWorld) finish(task domain.Task, category, ago string) {
	w.t.Helper()
	if _, err := w.store.DB().Exec(w.ctx, `
		UPDATE tasks SET closed_at = now() - $3::interval,
		       status_id = (SELECT ws.id FROM workflow_statuses ws JOIN workflows wf ON wf.id = ws.workflow_id
		                    WHERE wf.project_id = $2 AND wf.state = 'active' AND ws.category = $4 LIMIT 1)
		WHERE id = $1`, task.ID, w.pid, ago, category); err != nil {
		w.t.Fatal(err)
	}
}

func (w policyWorld) asEpic(task domain.Task) {
	w.t.Helper()
	if _, err := w.store.DB().Exec(w.ctx,
		`UPDATE tasks SET properties = jsonb_set(properties, '{type}', '"epic"') WHERE id = $1`, task.ID); err != nil {
		w.t.Fatal(err)
	}
}

func closedRow(t *testing.T, rows []postgres.ClosedTaskCostMetric, typ string) postgres.ClosedTaskCostMetric {
	t.Helper()
	for _, r := range rows {
		if r.TaskType == typ {
			return r
		}
	}
	t.Fatalf("в closed_cost нет строки %q: %+v", typ, rows)
	return postgres.ClosedTaskCostMetric{}
}

func median(v []float64) float64 {
	sort.Float64s(v)
	if n := len(v); n%2 == 1 {
		return v[n/2]
	} else {
		return (v[n/2-1] + v[n/2]) / 2
	}
}

func avg(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func sum(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s
}

func wantF(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s: %v, ожидали %v", what, got, want)
	}
}

// Пункты 1–5: набор данных критериев. A, B, G, C — задачи, E — эпик с
// дочерней C, X — отменена, D — закрыта до окна.
func TestClosedCostByType(t *testing.T) {
	w := newPolicyWorld(t)
	baseline, err := w.engine.ProjectMetrics(w.ctx, "SDLC", 0)
	if err != nil {
		t.Fatal(err)
	}

	// A: прогон роли с двумя строками учёта (api и подписка) — прогон один,
	// счётчики не задваиваются; плюс начальный прогон без роли.
	a, aRun := w.closedTask("A", "")
	w.use(aRun, f(1.0), "api")
	w.use(aRun, f(0.5), "subscription")
	// B: забранный прогон без учёта (missing) и прогон со строкой без признака.
	b, bRun := w.closedTask("B", "")
	bExtra := w.extraRun(b, bRun, true)
	w.use(bExtra, f(0.3), "")
	// G: ещё одна задача типа task — чётное число для медианы.
	g, gRun := w.closedTask("G", "")
	w.use(gRun, f(0.5), "api")
	// E — эпик, C — его дочерняя; прогон C старше окна.
	e, eRun := w.closedTask("E", "")
	w.asEpic(e)
	w.use(eRun, nil, "") // строка без стоимости: reported, в суммы не входит
	eExtra := w.extraRun(e, eRun, true)
	w.use(eExtra, f(2.0), "api")
	w.use(eExtra, f(1.0), "subscription")
	c, cRun := w.closedTask("C", e.Key)
	w.use(cRun, f(4.0), "api")
	if _, err := w.store.DB().Exec(w.ctx,
		`UPDATE stage_runs SET entered_at = now() - interval '100 days' WHERE task_id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	// X отменена, D закрыта давно: обе в разрез не входят.
	x, xRun := w.closedTask("X", "")
	w.use(xRun, f(9.0), "api")
	d, dRun := w.closedTask("D", "")
	w.use(dRun, f(8.0), "api")
	for _, task := range []domain.Task{a, b, g, c, e} {
		w.finish(task, "done", "1 hour")
	}
	w.finish(x, "cancelled", "1 hour")
	w.finish(d, "done", "60 days")

	m, err := w.engine.ProjectMetrics(w.ctx, "SDLC", 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.ClosedCost == nil {
		t.Fatal("closed_cost: nil вместо среза")
	}

	// 2: две строки, X и D нигде, сумма closed_tasks равна throughput.closed.
	task, epic := closedRow(t, m.ClosedCost, "task"), closedRow(t, m.ClosedCost, "epic")
	total := 0
	for _, r := range m.ClosedCost {
		total += r.ClosedTasks
	}
	baseTotal := 0
	for _, r := range baseline.ClosedCost {
		baseTotal += r.ClosedTasks
	}
	if total-baseTotal != 5 || total != m.Throughput.Closed {
		t.Errorf("closed_tasks по строкам %d (было %d), throughput.closed %d", total, baseTotal, m.Throughput.Closed)
	}
	base := closedRow(t, append(baseline.ClosedCost, postgres.ClosedTaskCostMetric{TaskType: "task"}), "task")
	if task.ClosedTasks-base.ClosedTasks != 4 || epic.ClosedTasks != 1 {
		t.Errorf("task %d (было %d), epic %d", task.ClosedTasks, base.ClosedTasks, epic.ClosedTasks)
	}

	// 3: суммы, средние и медианы сверены с TaskUsage каждой закрытой задачи.
	var api, sub, unspec []float64
	for _, key := range []string{a.Key, b.Key, g.Key, c.Key} {
		u, err := w.engine.TaskUsage(w.ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		api = append(api, u.APICostUSD)
		sub = append(sub, u.SubscriptionCostUSD)
		unspec = append(unspec, u.UnspecifiedCostUSD)
	}
	if base.ClosedTasks == 0 { // сид проекта без закрытых задач: строка целиком наша
		wantF(t, "task api Σ", task.APICostUSD, sum(api))
		wantF(t, "task api среднее", task.AvgAPICostUSD, avg(api))
		wantF(t, "task api медиана (чётное число — среднее двух)", task.MedianAPICostUSD, median(api))
		wantF(t, "task условно Σ", task.SubscriptionCostUSD, sum(sub))
		wantF(t, "task условно среднее", task.AvgSubscriptionUSD, avg(sub))
		wantF(t, "task условно медиана (нули входят)", task.MedianSubscription, median(sub))
		wantF(t, "task без признака Σ", task.UnspecifiedCostUSD, sum(unspec))
	}
	// Явные числа набора: A 1.0, B 0.3, G 0.5, C 4.0; подписка только у A.
	wantF(t, "api Σ", sum(api), 5.8)
	wantF(t, "api медиана", median(api), 0.75)
	wantF(t, "подписка среднее", avg(sub), 0.125)
	wantF(t, "подписка медиана", median(sub), 0)

	// 4: эпик несёт поддерево (прогоны C, в том числе до окна), C учтена и в
	// строке task.
	eu, err := w.engine.TaskUsage(w.ctx, e.Key)
	if err != nil {
		t.Fatal(err)
	}
	wantF(t, "epic api Σ = subtree_api_cost_usd", epic.APICostUSD, eu.SubtreeAPICostUSD)
	wantF(t, "epic условно Σ = subtree_subscription_cost_usd", epic.SubscriptionCostUSD, eu.SubtreeSubscriptionCostUSD)
	wantF(t, "epic api Σ", epic.APICostUSD, 6.0)
	wantF(t, "epic среднее = сумма при одной задаче", epic.AvgAPICostUSD, 6.0)
	wantF(t, "epic медиана", epic.MedianAPICostUSD, 6.0)
	wantF(t, "epic условно Σ", epic.SubscriptionCostUSD, 1.0)

	// 5: missing только у B; not_applicable (начальные прогоны) в долю не
	// входит, но в runs входит; состояния совпадают с TaskUsage.
	if task.IncompleteTasks-base.IncompleteTasks != 1 || task.Silent-base.Silent != 1 {
		t.Errorf("неполных %d, silent_runs %d", task.IncompleteTasks-base.IncompleteTasks, task.Silent-base.Silent)
	}
	reported := task.Reported - base.Reported
	if reported != 4 { // A, B (extra), G, C
		t.Errorf("reported_runs %d, ожидали 4", reported)
	}
	if task.Runs-base.Runs <= reported+1 {
		t.Errorf("runs %d должны включать not_applicable сверх reported+silent", task.Runs-base.Runs)
	}
	if base.ClosedTasks == 0 {
		wantF(t, "silent_share", task.SilentShare, 1.0/5.0)
	}
	if epic.IncompleteTasks != 0 || epic.Silent != 0 || epic.SilentShare != 0 || epic.Reported != 3 {
		t.Errorf("epic: %+v", epic)
	}
	// Состояния совпадают с TaskUsage каждой закрытой задачи.
	var usageSilent, usageReported int
	for _, key := range []string{a.Key, b.Key, g.Key, c.Key} {
		u, _ := w.engine.TaskUsage(w.ctx, key)
		usageSilent += u.SilentRuns
		usageReported += u.ReportedRuns
	}
	if usageSilent != task.Silent-base.Silent || usageReported != reported {
		t.Errorf("TaskUsage: silent %d reported %d; разрез: %d %d", usageSilent, usageReported, task.Silent-base.Silent, reported)
	}

	// Старые разделы ответа на месте и прежнего смысла.
	if len(m.Cost) == 0 || m.Throughput.Closed != baseline.Throughput.Closed+5 {
		t.Errorf("прежние разделы: cost %d, closed %d", len(m.Cost), m.Throughput.Closed)
	}
}

// Пункт 6: неполнота не выглядит дешевизной. Единственная закрытая задача, у
// которой все прогоны missing и нет ни одной строки учёта: silent_share = 1,
// стоимость нулевая; строка учёта без стоимости — reported и долю не растит.
func TestClosedCostIncompleteIsNotCheap(t *testing.T) {
	w := newPolicyWorld(t)
	baseline, err := w.engine.ProjectMetrics(w.ctx, "SDLC", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range baseline.ClosedCost {
		if r.TaskType == "task" {
			t.Skip("в сиде проекта уже есть закрытые задачи: доля 1 не проверяется")
		}
	}

	f1, run := w.closedTask("Все без учёта", "")
	w.extraRun(f1, run, true)
	w.finish(f1, "done", "1 hour")
	m, err := w.engine.ProjectMetrics(w.ctx, "SDLC", 0)
	if err != nil {
		t.Fatal(err)
	}
	row := closedRow(t, m.ClosedCost, "task")
	if row.IncompleteTasks != 1 || row.Silent != 2 || row.Reported != 0 || row.SilentShare != 1 {
		t.Errorf("все прогоны missing: %+v", row)
	}
	if row.APICostUSD != 0 || row.SubscriptionCostUSD != 0 {
		t.Errorf("стоимость неполной задачи должна быть нижней границей 0: %+v", row)
	}

	// Строка учёта без стоимости гасит missing прогона, суммы не трогает.
	w.use(run, nil, "")
	m, err = w.engine.ProjectMetrics(w.ctx, "SDLC", 0)
	if err != nil {
		t.Fatal(err)
	}
	row = closedRow(t, m.ClosedCost, "task")
	if row.Silent != 1 || row.Reported != 1 || row.SilentShare != 0.5 || row.APICostUSD != 0 {
		t.Errorf("строка без стоимости: %+v", row)
	}
}

// Пункт 7: период без закрытых задач — [] в JSON, не null; неверный период —
// прежний отказ.
func TestClosedCostEmptyPeriod(t *testing.T) {
	w := newPolicyWorld(t)
	m, err := w.engine.ProjectMetrics(w.ctx, "SDLC", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ClosedCost json.RawMessage `json:"closed_cost"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.ClosedCost) != "[]" {
		t.Errorf("closed_cost: %s", got.ClosedCost)
	}
	if _, err := w.engine.ProjectMetrics(w.ctx, "SDLC", 6*365*24*60*60*1e9); err == nil {
		t.Error("период больше пяти лет должен отбиваться")
	}
}
