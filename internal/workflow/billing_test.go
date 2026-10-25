package workflow

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// CLAB-216, решение 0062: признак оплаты строки учёта. Деньги — строки api и
// без признака, условная стоимость — subscription; бюджет команды считает
// только деньги, чтение отдаёт обе суммы раздельно.

type billingWorld struct {
	store  *postgres.Store
	engine *Engine
	auth   *Auth
	system domain.Principal
	admin  Identity
	team   domain.Team
}

func newBillingWorld(t *testing.T) billingWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, _, auth, system := setupPhase4(t, AuthToken)
	w := billingWorld{store: store, engine: engine, auth: auth, system: system,
		admin: adminIdentity(t, ctx, auth)}
	if _, err := auth.CreateTeam(ctx, w.admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatal(err)
	}
	team, err := store.GetTeamByKey(ctx, store.DB(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	w.team = team
	if _, err := store.DB().Exec(ctx,
		`UPDATE projects SET owner_team_id = $1 WHERE key = 'SDLC'`, team.ID); err != nil {
		t.Fatal(err)
	}
	return w
}

// task — задача в работе, этап забран системным актором.
func (w billingWorld) task(t *testing.T, title, parent string) (domain.Task, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	task, err := w.engine.CreateTask(ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: title, ParentKey: parent},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := w.engine.Execute(ctx, task.Key, "start", w.system.ID, ""); err != nil || res.Blocked {
		t.Fatalf("в работу: %v %+v", err, res.Gates)
	}
	claimed, err := w.engine.Claim(ctx, task.Key, w.system.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return task, claimed.StageRunID
}

func (w billingWorld) report(t *testing.T, key, model string, cost float64, billing string) domain.StageRunUsage {
	t.Helper()
	u, err := w.engine.ReportUsage(context.Background(), key, w.system.ID, ReportUsageInput{
		Model: model, CostUSD: &cost, Billing: billing,
	})
	if err != nil {
		t.Fatalf("отчёт %s %v %q: %v", model, cost, billing, err)
	}
	return u
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func fptr(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// AC3, AC5, AC6 (одна проверка в движке на все транспорты), AC7.
func TestReportUsageBilling(t *testing.T) {
	ctx := context.Background()
	w := newBillingWorld(t)
	task, runID := w.task(t, "Признак оплаты", "")

	for _, tc := range []struct {
		in   string
		want *string
	}{
		{"", nil},
		{"api", ptr("api")},
		{"subscription", ptr("subscription")},
	} {
		u := w.report(t, task.Key, "m", 0.1, tc.in)
		if (u.Billing == nil) != (tc.want == nil) || (u.Billing != nil && *u.Billing != *tc.want) {
			t.Fatalf("%q: в ответе billing %v", tc.in, u.Billing)
		}
		var stored *string
		if err := w.store.DB().QueryRow(ctx, `SELECT billing FROM stage_run_usage WHERE id = $1`, u.ID).
			Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if (stored == nil) != (tc.want == nil) || (stored != nil && *stored != *tc.want) {
			t.Fatalf("%q: в базе billing %v", tc.in, stored)
		}
		raw, _ := json.Marshal(u)
		if !strings.Contains(string(raw), `"billing":`) {
			t.Fatalf("в ответе нет поля billing: %s", raw)
		}
	}

	var before int
	if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM stage_run_usage WHERE run_id = $1`, runID).
		Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"x", "API", "Subscription", " api"} {
		cost := 1.0
		_, err := w.engine.ReportUsage(ctx, task.Key, w.system.ID, ReportUsageInput{
			Model: "m", CostUSD: &cost, Billing: bad,
		})
		if apperr.As(err).Code != apperr.CodeInvalidInput ||
			!strings.Contains(err.Error(), "api, subscription") {
			t.Fatalf("%q: отказ %v, ждали invalid_input с перечнем", bad, err)
		}
	}
	var after int
	if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM stage_run_usage WHERE run_id = $1`, runID).
		Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("отказанный отчёт записал строку: было %d, стало %d", before, after)
	}

	// Событие несёт признак, если он указан; без признака поля нет.
	rows, err := w.store.DB().Query(ctx, `
		SELECT payload FROM task_events WHERE task_id = $1 AND type = $2 ORDER BY created_at`,
		task.ID, domain.EventUsageReported)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []any
	for rows.Next() {
		var payload map[string]any
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		v, has := payload["billing"]
		if !has {
			v = "<нет>"
		}
		got = append(got, v)
	}
	if len(got) != 3 || got[0] != "<нет>" || got[1] != "api" || got[2] != "subscription" {
		t.Fatalf("billing в событиях usage.reported: %v", got)
	}
}

// AC11, AC12, AC13: подписка в бюджет не входит никогда, api и строка без
// признака — входят; текст отказа прежний.
func TestBudgetCountsMoneyOnly(t *testing.T) {
	ctx := context.Background()
	w := newBillingWorld(t)
	if _, err := w.auth.SetTeamBudget(ctx, w.admin, "platform", "day", 1); err != nil {
		t.Fatal(err)
	}
	task, runID := w.task(t, "Работа на подписке", "")
	w.report(t, task.Key, "claude-opus-5", 5, "subscription")
	if err := w.engine.Release(ctx, runID, w.system.ID, "перерыв"); err != nil {
		t.Fatal(err)
	}

	again, err := w.engine.Claim(ctx, task.Key, w.system.ID, "второй")
	if err != nil {
		t.Fatalf("захват при подписке выше предела: %v", err)
	}
	budgets, err := w.auth.TeamBudgets(ctx, w.admin, "platform")
	if err != nil {
		t.Fatal(err)
	}
	if len(budgets.Budgets) != 1 || budgets.Budgets[0].Period != "day" ||
		budgets.Budgets[0].SpentUSD != 0 || !near(budgets.Budgets[0].SubscriptionUSD, 5) {
		t.Fatalf("бюджет: %+v", budgets.Budgets)
	}
	raw, _ := json.Marshal(budgets.Budgets[0])
	if !strings.Contains(string(raw), `"spent_usd":0`) || !strings.Contains(string(raw), `"subscription_usd":5`) {
		t.Fatalf("ответ бюджета: %s", raw)
	}

	for _, billing := range []string{"api", ""} {
		u := w.report(t, task.Key, "claude-opus-5", 1, billing)
		if err := w.engine.Release(ctx, again.StageRunID, w.system.ID, "перерыв"); err != nil {
			t.Fatal(err)
		}
		_, err := w.engine.Claim(ctx, task.Key, w.system.ID, "третий")
		if apperr.As(err).Code != apperr.CodeForbidden {
			t.Fatalf("%q: захват при исчерпанных деньгах: %v", billing, err)
		}
		if !strings.Contains(err.Error(), "platform") || !strings.Contains(err.Error(), "день") ||
			!strings.Contains(err.Error(), "потрачено 1.00 из 1.00 USD") {
			t.Fatalf("%q: текст отказа: %v", billing, err)
		}
		budgets, err := w.auth.TeamBudgets(ctx, w.admin, "platform")
		if err != nil {
			t.Fatal(err)
		}
		if !near(budgets.Budgets[0].SpentUSD, 1) || !near(budgets.Budgets[0].SubscriptionUSD, 5) {
			t.Fatalf("%q: бюджет %+v", billing, budgets.Budgets)
		}

		// Строку убираем — захват снова открыт: следующий круг проверяет
		// строку без признака на том же месте.
		if _, err := w.store.DB().Exec(ctx, `DELETE FROM stage_run_usage WHERE id = $1`, u.ID); err != nil {
			t.Fatal(err)
		}
		if again, err = w.engine.Claim(ctx, task.Key, w.system.ID, "снова"); err != nil {
			t.Fatalf("%q: захват после снятия строки: %v", billing, err)
		}
	}
}

// AC14, AC15, AC16, AC17: деньги, «не указано» и условная стоимость
// раздельно в TaskUsage, карточке и метриках; cost_usd — по-прежнему всё.
func TestUsageReadSplitsBilling(t *testing.T) {
	ctx := context.Background()
	w := newBillingWorld(t)
	parent, parentRun := w.task(t, "Задача со счётом", "")
	w.report(t, parent.Key, "m-api", 1, "api")
	w.report(t, parent.Key, "m-none", 2, "")
	w.report(t, parent.Key, "m-sub", 4, "subscription")
	child, childRun := w.task(t, "Ребёнок на подписке", parent.Key)
	w.report(t, child.Key, "m-sub", 8, "subscription")

	usage, err := w.engine.TaskUsage(ctx, parent.Key)
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]float64{
		"cost_usd":                      {usage.CostUSD, 7},
		"api_cost_usd":                  {usage.APICostUSD, 3},
		"unspecified_cost_usd":          {usage.UnspecifiedCostUSD, 2},
		"subscription_cost_usd":         {usage.SubscriptionCostUSD, 4},
		"subtree_cost_usd":              {usage.SubtreeCostUSD, 15},
		"subtree_api_cost_usd":          {usage.SubtreeAPICostUSD, 3},
		"subtree_unspecified_cost_usd":  {usage.SubtreeUnspecifiedCostUSD, 2},
		"subtree_subscription_cost_usd": {usage.SubtreeSubscriptionCostUSD, 12},
	} {
		if !near(pair[0], pair[1]) {
			t.Errorf("TaskUsage.%s = %v, ждали %v", name, pair[0], pair[1])
		}
	}
	childUsage, err := w.engine.TaskUsage(ctx, child.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !near(childUsage.CostUSD, 8) || childUsage.APICostUSD != 0 || !near(childUsage.SubscriptionCostUSD, 8) {
		t.Fatalf("TaskUsage ребёнка: %+v", childUsage)
	}

	// Карточка: прогон с тремя видами строк и прогон только на подписке —
	// у второго денег нет, и это null, а не ноль.
	check := func(task domain.Task, runID uuid.UUID, cost, api, unspecified, sub any) {
		t.Helper()
		runs, err := w.store.CardStageRuns(ctx, w.store.DB(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.ID != runID {
				continue
			}
			got := []any{fptr(r.Usage.CostUSD), fptr(r.Usage.APICostUSD),
				fptr(r.Usage.UnspecifiedCostUSD), fptr(r.Usage.SubscriptionCostUSD)}
			want := []any{cost, api, unspecified, sub}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s: учёт прогона %v, ждали %v", task.Key, got, want)
				}
			}
			raw, _ := json.Marshal(r.Usage)
			for _, key := range []string{`"api_cost_usd":`, `"unspecified_cost_usd":`, `"subscription_cost_usd":`, `"cost_usd":`, `"reports":`} {
				if !strings.Contains(string(raw), key) {
					t.Fatalf("%s: в учёте прогона нет %s: %s", task.Key, key, raw)
				}
			}
			return
		}
		t.Fatalf("%s: прогона %s нет в карточке", task.Key, runID)
	}
	check(parent, parentRun, 7.0, 3.0, 2.0, 4.0)
	check(child, childRun, 8.0, nil, nil, 8.0)

	metrics, err := w.engine.ProjectMetrics(ctx, "SDLC", 0)
	if err != nil {
		t.Fatal(err)
	}
	sums := map[string][3]float64{}
	models := map[string]postgres.CostMetric{}
	for _, m := range metrics.Cost {
		if !near(m.CostUSD, m.APICostUSD+m.SubscriptionCostUSD) || m.UnspecifiedCostUSD > m.APICostUSD+1e-9 {
			t.Fatalf("инвариант нарушен: %+v", m)
		}
		s := sums[m.Dimension]
		s[0] += m.APICostUSD
		s[1] += m.UnspecifiedCostUSD
		s[2] += m.SubscriptionCostUSD
		sums[m.Dimension] = s
		if m.Dimension == "model" {
			models[m.Key] = m
		}
	}
	for _, dim := range []string{"status", "role", "model"} {
		if s := sums[dim]; !near(s[0], 3) || !near(s[1], 2) || !near(s[2], 12) {
			t.Fatalf("разрез %s: деньги %v, не указано %v, условная %v", dim, s[0], s[1], s[2])
		}
	}
	if m := models["m-sub"]; !near(m.SubscriptionCostUSD, 12) || m.APICostUSD != 0 {
		t.Fatalf("модель m-sub: %+v", m)
	}
	if m := models["m-none"]; !near(m.APICostUSD, 2) || !near(m.UnspecifiedCostUSD, 2) {
		t.Fatalf("модель m-none: %+v", m)
	}
	if m := models["m-api"]; !near(m.APICostUSD, 1) || m.UnspecifiedCostUSD != 0 {
		t.Fatalf("модель m-api: %+v", m)
	}
}

// AC1, AC2: миграция только добавляет. Строки, записанные до неё, получают
// NULL, сохраняют остальные поля и остаются в бюджете; CHECK держит значения;
// down снимает колонку, up → down → up проходит.
func TestBillingMigrationOnFilledBase(t *testing.T) {
	ctx := context.Background()
	w := newBillingWorld(t)
	db := w.store.DB()
	if _, err := w.auth.SetTeamBudget(ctx, w.admin, "platform", "month", 100); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	hasColumn := func() bool {
		var n int
		if err := db.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'stage_run_usage' AND column_name = 'billing'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	task, runID := w.task(t, "До апгрейда", "")
	if _, err := db.Exec(ctx, read("000030_usage_billing.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if hasColumn() {
		t.Fatal("down оставил колонку billing")
	}
	// Состояние до миграции: строки пишутся без колонки, как старым сервисом.
	if _, err := db.Exec(ctx, `
		INSERT INTO stage_run_usage (run_id, model, input_tokens, output_tokens, cost_usd, duration_ms, reported_by, details)
		VALUES ($1, 'claude-opus-5', 100, 20, 2.5, 1000, $2, '{"reported_by":"clab-runner"}'),
		       ($1, 'claude-sonnet-5', NULL, NULL, 1.25, NULL, $2, '{}')`, runID, w.system.ID); err != nil {
		t.Fatal(err)
	}
	type row struct {
		Model    string
		In, Out  *int64
		Cost     *float64
		Duration *int64
		Details  string
	}
	snapshot := func() []row {
		rows, err := db.Query(ctx, `
			SELECT model, input_tokens, output_tokens, cost_usd::float8, duration_ms, details::text
			FROM stage_run_usage WHERE run_id = $1 ORDER BY model`, runID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.Model, &r.In, &r.Out, &r.Cost, &r.Duration, &r.Details); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	before := snapshot()

	if _, err := db.Exec(ctx, read("000030_usage_billing.up.sql")); err != nil {
		t.Fatalf("up на заполненной базе: %v", err)
	}
	if !hasColumn() {
		t.Fatal("up не завёл колонку billing")
	}
	var notNull int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM stage_run_usage WHERE billing IS NOT NULL`).Scan(&notNull); err != nil {
		t.Fatal(err)
	}
	if notNull != 0 {
		t.Fatalf("строки до миграции получили признак: %d", notNull)
	}
	after := snapshot()
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("строки изменились:\nдо   %s\nпосле %s", beforeJSON, afterJSON)
	}

	// Старые строки — деньги: входят в spent_usd и в деньги задачи.
	budgets, err := w.auth.TeamBudgets(ctx, w.admin, "platform")
	if err != nil {
		t.Fatal(err)
	}
	if !near(budgets.Budgets[0].SpentUSD, 3.75) || budgets.Budgets[0].SubscriptionUSD != 0 {
		t.Fatalf("бюджет после апгрейда: %+v", budgets.Budgets)
	}
	usage, err := w.engine.TaskUsage(ctx, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !near(usage.CostUSD, 3.75) || !near(usage.APICostUSD, 3.75) || !near(usage.UnspecifiedCostUSD, 3.75) {
		t.Fatalf("стоимость задачи после апгрейда: %+v", usage)
	}

	// CHECK — негативная проверка: и прямой INSERT, и UPDATE.
	for _, sql := range []string{
		`INSERT INTO stage_run_usage (run_id, model, billing) VALUES ('` + runID.String() + `', 'm', 'x')`,
		`INSERT INTO stage_run_usage (run_id, model, billing) VALUES ('` + runID.String() + `', 'm', 'API')`,
		`UPDATE stage_run_usage SET billing = '' WHERE run_id = '` + runID.String() + `'`,
	} {
		if _, err := db.Exec(ctx, sql); err == nil {
			t.Errorf("CHECK пропустил: %s", sql)
		}
	}

	// Повторный down → up на той же базе тоже проходит.
	if _, err := db.Exec(ctx, read("000030_usage_billing.down.sql")); err != nil {
		t.Fatalf("второй down: %v", err)
	}
	if _, err := db.Exec(ctx, read("000030_usage_billing.up.sql")); err != nil {
		t.Fatalf("второй up: %v", err)
	}
}
