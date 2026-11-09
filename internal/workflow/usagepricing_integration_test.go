package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// CLAB-426, решение 0077 п. 4.11: сумма строки учёта по прайсу версии
// настройки агентов, снятой на прогон. Деньги проверяются числами; номера AC —
// критерии приёмки задачи. Фикстура: версия N активна, прайс as_of 2026-11-01,
// модель M: input 3, output 15, cache_read 0.3, cache_write 3.75.

type pricingWorld struct {
	billingWorld
	human Actor
}

func newPricingWorld(t *testing.T) pricingWorld {
	t.Helper()
	w := pricingWorld{billingWorld: newBillingWorld(t)}
	p, err := w.store.ResolveActor(context.Background(), w.store.DB(), "pricer", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	w.human = Actor{Principal: p, Level: domain.MemberMaintainer}
	return w
}

// priceDoc — документ версии с прайсом модели M (output — параметр, чтобы
// версии отличались ценой).
func priceDoc(output string) string {
	return `{"agents":{"claude":{"levels":{"high":{"model":"M"}}}},"pricing":{"as_of":"2026-11-01","models":{
		"M":{"input":3,"output":` + output + `,"cache_read":0.3,"cache_write":3.75},
		"NOCACHE":{"input":3,"output":15},
		"HALF":{"input":0.5,"output":0},
		"QUARTER":{"input":0.25,"output":0},
		"ONE":{"input":1,"output":0}}}}`
}

// activate заводит и активирует версию, отдаёт её номер.
func (w pricingWorld) activate(t *testing.T, doc string) int {
	t.Helper()
	ctx := context.Background()
	v, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", w.human, AgentSettingsDraftInput{Document: json.RawMessage(doc)})
	if err != nil {
		t.Fatalf("черновик: %v", err)
	}
	if _, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", w.human, &v.Version); err != nil {
		t.Fatalf("активация %d: %v", v.Version, err)
	}
	return v.Version
}

type usageIn struct {
	model           string
	in, out, cr, cw *int64
	cost            *float64
	billing         string
}

func tokens(in, out int64) usageIn { return usageIn{model: "M", in: i64(in), out: i64(out)} }

func (w pricingWorld) report(t *testing.T, key string, u usageIn) domain.StageRunUsage {
	t.Helper()
	out, err := w.engine.ReportUsage(context.Background(), key, w.system.ID, ReportUsageInput{
		Model: u.model, InputTokens: u.in, OutputTokens: u.out,
		CacheReadTokens: u.cr, CacheWriteTokens: u.cw, CostUSD: u.cost, Billing: u.billing,
	})
	if err != nil {
		t.Fatalf("отчёт %+v: %v", u, err)
	}
	return out
}

// stored — строка в базе как есть: суммы текстом numeric, без float64.
type stored struct {
	Cost, Source, Reported *string
	Version                *int
}

func (s stored) String() string {
	str := func(p *string) string {
		if p == nil {
			return "NULL"
		}
		return *p
	}
	v := "NULL"
	if s.Version != nil {
		v = string(rune('0' + *s.Version))
	}
	return "cost=" + str(s.Cost) + " source=" + str(s.Source) + " version=" + v + " reported=" + str(s.Reported)
}

func (w pricingWorld) row(t *testing.T, id uuid.UUID) stored {
	t.Helper()
	var s stored
	if err := w.store.DB().QueryRow(context.Background(), `
		SELECT u.cost_usd::text, u.cost_source, asv.version, u.reported_cost_usd::text
		FROM stage_run_usage u
		LEFT JOIN agent_settings_versions asv ON asv.id = u.pricing_version_id
		WHERE u.id = $1`, id).Scan(&s.Cost, &s.Source, &s.Version, &s.Reported); err != nil {
		t.Fatal(err)
	}
	return s
}

func (w pricingWorld) want(t *testing.T, id uuid.UUID, cost, source string, version int, reported string) {
	t.Helper()
	got := w.row(t, id)
	pick := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	wantV := (*int)(nil)
	if version != 0 {
		wantV = &version
	}
	exp := stored{Cost: pick(cost), Source: pick(source), Version: wantV, Reported: pick(reported)}
	if got.String() != exp.String() {
		t.Fatalf("строка учёта: %s\nждали:        %s", got, exp)
	}
}

func fl(v float64) *float64 { return &v }

// AC-1, AC-2, AC-6, AC-11 (ответ), AC-13 (событие): сумма по прайсу и оценка
// агента рядом.
func TestReportUsageByPricing(t *testing.T) {
	ctx := context.Background()
	w := newPricingWorld(t)
	ver := w.activate(t, priceDoc("15"))
	task, _ := w.task(t, "По прайсу", "")

	// AC-1: без --cost сумму считает сервер, оценки агента нет.
	u := w.report(t, task.Key, tokens(1_000_000, 1_000_000))
	w.want(t, u.ID, "18.000000", "pricing", ver, "")
	if u.CostUSD == nil || *u.CostUSD != 18 || u.CostSource == nil || *u.CostSource != "pricing" ||
		u.PricingVersion == nil || *u.PricingVersion != ver || u.ReportedCostUSD != nil {
		t.Fatalf("ответ: %+v", u)
	}
	raw, _ := json.Marshal(u)
	for _, key := range []string{`"cost_source":"pricing"`, `"pricing_version":1`, `"reported_cost_usd":null`, `"warnings":[]`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("в ответе нет %s: %s", key, raw)
		}
	}

	// Точная дробь, не 0.01220699….
	u = w.report(t, task.Key, tokens(1234, 567))
	w.want(t, u.ID, "0.012207", "pricing", ver, "")

	// Кэш с ценами и без цен (предупреждения нет).
	u = w.report(t, task.Key, usageIn{model: "M", in: i64(1_000_000), out: i64(1_000_000), cr: i64(2_000_000), cw: i64(1_000_000)})
	w.want(t, u.ID, "22.350000", "pricing", ver, "")
	u = w.report(t, task.Key, usageIn{model: "NOCACHE", in: i64(1_000_000), out: i64(1_000_000), cr: i64(2_000_000), cw: i64(1_000_000)})
	w.want(t, u.ID, "18.000000", "pricing", ver, "")
	if len(u.Warnings) != 0 {
		t.Fatalf("кэш без цены: предупреждение %v", u.Warnings)
	}

	// Половина от нуля и меньше половины; нули — это ноль, а не «неизвестно».
	u = w.report(t, task.Key, usageIn{model: "HALF", in: i64(1), out: i64(0)})
	w.want(t, u.ID, "0.000001", "pricing", ver, "")
	u = w.report(t, task.Key, usageIn{model: "QUARTER", in: i64(1), out: i64(0)})
	w.want(t, u.ID, "0.000000", "pricing", ver, "")
	u = w.report(t, task.Key, tokens(0, 0))
	w.want(t, u.ID, "0.000000", "pricing", ver, "")

	// AC-2: оценка агента рядом, главная — сумма по прайсу; названный ноль — ноль.
	withCost := tokens(1_000_000, 1_000_000)
	withCost.cost = fl(20)
	u = w.report(t, task.Key, withCost)
	w.want(t, u.ID, "18.000000", "pricing", ver, "20.000000")
	if u.ReportedCostUSD == nil || *u.ReportedCostUSD != 20 || *u.CostUSD != 18 {
		t.Fatalf("ответ с оценкой: %+v", u)
	}
	withCost.cost = fl(0)
	u = w.report(t, task.Key, withCost)
	w.want(t, u.ID, "18.000000", "pricing", ver, "0.000000")

	// AC-13: в ленте итоговая сумма, источник, версия и оценка.
	var payload map[string]any
	if err := w.store.DB().QueryRow(ctx, `
		SELECT payload FROM task_events WHERE task_id = $1 AND type = $2
		  AND payload->>'reported_cost_usd' = '20' ORDER BY created_at DESC LIMIT 1`,
		task.ID, domain.EventUsageReported).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["cost_usd"] != 18.0 || payload["cost_source"] != "pricing" || payload["pricing_version"] != float64(ver) ||
		payload["model"] != "M" || payload["stage_run_id"] == nil {
		t.Fatalf("payload события: %v", payload)
	}
}

// AC-3: модели нет в прайсе — как сегодня; точное совпадение, регистр и пробелы
// значимы.
func TestReportUsageModelNotInPrice(t *testing.T) {
	w := newPricingWorld(t)
	w.activate(t, priceDoc("15"))
	task, _ := w.task(t, "Чужая модель", "")
	for _, model := range []string{"m-other", "model-X", "m", " M", "M "} {
		u := usageIn{model: model, in: i64(1_000_000), out: i64(1_000_000), cost: fl(5)}
		w.want(t, w.report(t, task.Key, u).ID, "5.000000", "reported", 0, "5.000000")
		u.cost = nil
		got := w.report(t, task.Key, u)
		w.want(t, got.ID, "", "", 0, "")
		if got.CostUSD != nil || got.CostSource != nil || len(got.Warnings) != 0 {
			t.Fatalf("%q без суммы: %+v", model, got)
		}
	}
}

// AC-4: нет прайса — «как сегодня» в трёх видах: у проекта нет настройки; у
// версии нет pricing; у прогона нет снимка при том, что активная версия с
// прайсом есть. Заодно: цена берётся из снимка, а не из текущей активной.
func TestReportUsageNoPrice(t *testing.T) {
	w := newPricingWorld(t)

	// 1. У проекта нет настройки.
	t0, _ := w.task(t, "До настройки", "")
	req := tokens(1_000_000, 1_000_000)
	req.cost = fl(7)
	w.want(t, w.report(t, t0.Key, req).ID, "7.000000", "reported", 0, "7.000000")
	req.cost = nil
	w.want(t, w.report(t, t0.Key, req).ID, "", "", 0, "")

	// 2. В документе версии нет pricing.
	w.activate(t, `{"agents":{"claude":{"levels":{"high":{"model":"M"}}}}}`)
	t1, _ := w.task(t, "Версия без прайса", "")
	req.cost = fl(7)
	w.want(t, w.report(t, t1.Key, req).ID, "7.000000", "reported", 0, "7.000000")

	// 3. Активная версия с прайсом есть, но прогон t0 взят до снимка (NULL):
	// активная версия ему не указ.
	v2 := w.activate(t, priceDoc("15"))
	req.cost = fl(7)
	w.want(t, w.report(t, t0.Key, req).ID, "7.000000", "reported", 0, "7.000000")
	// И t1 снят на версию без прайса — новая активная его не касается.
	w.want(t, w.report(t, t1.Key, req).ID, "7.000000", "reported", 0, "7.000000")

	// Контроль: прогон, взятый после активации, считается по прайсу.
	t2, _ := w.task(t, "После активации", "")
	req.cost = nil
	w.want(t, w.report(t, t2.Key, req).ID, "18.000000", "pricing", v2, "")
}

// AC-5: модель есть в прайсе, а токенов нет — отчёт принят, предупреждение.
func TestReportUsageIncompleteTokens(t *testing.T) {
	ctx := context.Background()
	w := newPricingWorld(t)
	w.activate(t, priceDoc("15"))
	task, runID := w.task(t, "Неполные токены", "")

	for _, tc := range []struct {
		name string
		u    usageIn
		cost string
		miss string
	}{
		{"только сумма", usageIn{model: "M", cost: fl(7)}, "7.000000", "input_tokens и output_tokens"},
		{"только input", usageIn{model: "M", in: i64(100)}, "", "output_tokens"},
		{"только output", usageIn{model: "M", out: i64(100), cost: fl(1)}, "1.000000", "input_tokens"},
	} {
		got := w.report(t, task.Key, tc.u)
		source := ""
		reported := ""
		if tc.cost != "" {
			source, reported = "reported", tc.cost
		}
		w.want(t, got.ID, tc.cost, source, 0, reported)
		if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], tc.miss) ||
			!strings.Contains(got.Warnings[0], "не посчитана по прайсу") {
			t.Fatalf("%s: предупреждения %v", tc.name, got.Warnings)
		}
	}
	// Модели вне прайса — без предупреждения.
	if got := w.report(t, task.Key, usageIn{model: "other", cost: fl(1)}); len(got.Warnings) != 0 {
		t.Fatalf("модель вне прайса: %v", got.Warnings)
	}
	var n int
	if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM stage_run_usage WHERE run_id = $1`, runID).Scan(&n); err != nil || n != 4 {
		t.Fatalf("строк учёта: %d %v", n, err)
	}
}

// AC-7, AC-8: подписка по прайсу — условная стоимость, в бюджет не идёт; бюджет
// считает сумму по прайсу, а не оценку агента.
func TestPricingSubscriptionAndBudget(t *testing.T) {
	ctx := context.Background()
	w := newPricingWorld(t)
	ver := w.activate(t, priceDoc("15"))
	if _, err := w.auth.SetTeamBudget(ctx, w.admin, "platform", "month", 18); err != nil {
		t.Fatal(err)
	}
	task, runID := w.task(t, "Бюджет по прайсу", "")

	// Подписка: 18 по прайсу, условная стоимость, бюджет не растёт.
	sub := tokens(1_000_000, 1_000_000)
	sub.billing = "subscription"
	u := w.report(t, task.Key, sub)
	w.want(t, u.ID, "18.000000", "pricing", ver, "")
	usage, err := w.engine.TaskUsage(ctx, task.Key)
	if err != nil || !near(usage.SubscriptionCostUSD, 18) || usage.APICostUSD != 0 || !near(usage.CostUSD, 18) {
		t.Fatalf("подписка: %+v %v", usage, err)
	}
	budgets, err := w.auth.TeamBudgets(ctx, w.admin, "platform")
	if err != nil || budgets.Budgets[0].SpentUSD != 0 || !near(budgets.Budgets[0].SubscriptionUSD, 18) {
		t.Fatalf("бюджет при подписке: %+v %v", budgets, err)
	}
	if err := w.engine.Release(ctx, runID, w.system.ID, "перерыв"); err != nil {
		t.Fatal(err)
	}
	again, err := w.engine.Claim(ctx, task.Key, w.system.ID, "после подписки")
	if err != nil {
		t.Fatalf("захват при подписке на пределе: %v", err)
	}

	// Без признака: оценка агента 1, по прайсу 18 — бюджет исчерпан по 18.
	api := tokens(1_000_000, 1_000_000)
	api.cost = fl(1)
	u = w.report(t, task.Key, api)
	w.want(t, u.ID, "18.000000", "pricing", ver, "1.000000")
	if err := w.engine.Release(ctx, again.StageRunID, w.system.ID, "перерыв"); err != nil {
		t.Fatal(err)
	}
	_, err = w.engine.Claim(ctx, task.Key, w.system.ID, "третий")
	if apperr.As(err).Code != apperr.CodeForbidden || !strings.Contains(err.Error(), "потрачено 18.00 из 18.00 USD") {
		t.Fatalf("захват при исчерпанном бюджете: %v", err)
	}
	budgets, err = w.auth.TeamBudgets(ctx, w.admin, "platform")
	if err != nil || !near(budgets.Budgets[0].SpentUSD, 18) {
		t.Fatalf("spent_usd: %+v %v", budgets, err)
	}
}

// AC-9: активация новой версии прошлое не пересчитывает и не влияет на
// прогон, забранный раньше.
func TestPricingVersionSnapshot(t *testing.T) {
	ctx := context.Background()
	w := newPricingWorld(t)
	v1 := w.activate(t, priceDoc("15"))
	taskA, _ := w.task(t, "A при v1", "")
	taskB, _ := w.task(t, "B при v1", "")
	a := w.report(t, taskA.Key, tokens(1_000_000, 1_000_000))
	w.want(t, a.ID, "18.000000", "pricing", v1, "")

	v2 := w.activate(t, priceDoc("30"))
	if v2 == v1 {
		t.Fatal("версия не сменилась")
	}
	// Строка A не пересчитана.
	w.want(t, a.ID, "18.000000", "pricing", v1, "")
	// B взят при v1 и ещё идёт: считается по v1.
	b := w.report(t, taskB.Key, tokens(1_000_000, 1_000_000))
	w.want(t, b.ID, "18.000000", "pricing", v1, "")
	// C взят после активации.
	taskC, _ := w.task(t, "C при v2", "")
	c := w.report(t, taskC.Key, tokens(1_000_000, 1_000_000))
	w.want(t, c.ID, "33.000000", "pricing", v2, "")

	// Прогон A, взятый новой арендой после активации, снимок обновляет.
	var runA uuid.UUID
	if err := w.store.DB().QueryRow(ctx, `SELECT run_id FROM stage_run_usage WHERE id = $1`, a.ID).Scan(&runA); err != nil {
		t.Fatal(err)
	}
	if err := w.engine.Release(ctx, runA, w.system.ID, "перерыв"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.Claim(ctx, taskA.Key, w.system.ID, "заново"); err != nil {
		t.Fatal(err)
	}
	a2 := w.report(t, taskA.Key, tokens(1_000_000, 1_000_000))
	w.want(t, a2.ID, "33.000000", "pricing", v2, "")
	w.want(t, a.ID, "18.000000", "pricing", v1, "")
}

// AC-12: карточка и чтение — cost_usd как прежде, источник прогона отдельно.
func TestPricingRunUsageSource(t *testing.T) {
	ctx := context.Background()
	w := newPricingWorld(t)
	v1 := w.activate(t, priceDoc("15"))

	run := func(key string, runID uuid.UUID) domain.RunUsage {
		t.Helper()
		task, err := w.store.GetTaskByKey(ctx, w.store.DB(), key)
		if err != nil {
			t.Fatal(err)
		}
		runs, err := w.store.CardStageRuns(ctx, w.store.DB(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.ID == runID {
				return r.Usage
			}
		}
		t.Fatalf("прогона %s нет в карточке", runID)
		return domain.RunUsage{}
	}

	// По прайсу + по прайсу той же версии: pricing, версия одна.
	task, runID := w.task(t, "Источник прогона", "")
	w.report(t, task.Key, tokens(1_000_000, 1_000_000))
	two := tokens(1_000_000, 0)
	two.cost = fl(9)
	w.report(t, task.Key, two) // 3 по прайсу, оценка 9
	got := run(task.Key, runID)
	raw, _ := json.Marshal(got)
	if got.CostSource == nil || *got.CostSource != "pricing" || len(got.PricingVersions) != 1 || got.PricingVersions[0] != v1 ||
		got.CostUSD == nil || !near(*got.CostUSD, 21) || got.ReportedCostUSD == nil || !near(*got.ReportedCostUSD, 9) {
		t.Fatalf("pricing: %s", raw)
	}

	// Плюс строка «как сообщена» — mixed.
	w.report(t, task.Key, usageIn{model: "other", cost: fl(2)})
	got = run(task.Key, runID)
	if got.CostSource == nil || *got.CostSource != "mixed" || !near(*got.CostUSD, 23) || !near(*got.ReportedCostUSD, 11) {
		t.Fatalf("mixed: %+v", got)
	}

	// Только как сообщена — reported, версий нет, массив пустой.
	t2, run2 := w.task(t, "Как сообщена", "")
	w.report(t, t2.Key, usageIn{model: "other", cost: fl(2)})
	got = run(t2.Key, run2)
	raw, _ = json.Marshal(got)
	if got.CostSource == nil || *got.CostSource != "reported" || !strings.Contains(string(raw), `"pricing_versions":[]`) {
		t.Fatalf("reported: %s", raw)
	}

	// Строка без суммы и строка прошлых времён (источник NULL, но сумма есть)
	// источника не дают; NoRunUsage — null и пустой массив.
	t3, run3 := w.task(t, "Без источника", "")
	w.report(t, t3.Key, usageIn{model: "other"})
	if _, err := w.store.DB().Exec(ctx, `
		INSERT INTO stage_run_usage (run_id, model, cost_usd, reported_by) VALUES ($1, 'old', 4, $2)`, run3, w.system.ID); err != nil {
		t.Fatal(err)
	}
	got = run(t3.Key, run3)
	raw, _ = json.Marshal(got)
	if got.CostSource != nil || !near(*got.CostUSD, 4) || got.ReportedCostUSD != nil ||
		!strings.Contains(string(raw), `"cost_source":null`) || !strings.Contains(string(raw), `"reported_cost_usd":null`) {
		t.Fatalf("без источника: %s", raw)
	}
	raw, _ = json.Marshal(domain.NoRunUsage())
	if !strings.Contains(string(raw), `"cost_source":null`) || !strings.Contains(string(raw), `"pricing_versions":[]`) {
		t.Fatalf("NoRunUsage: %s", raw)
	}

	// ListRunUsage читает те же колонки, что RecordUsage.
	rows, err := w.store.ListRunUsage(ctx, w.store.DB(), runID)
	if err != nil || len(rows) != 3 || rows[0].CostSource == nil || *rows[0].CostSource != "pricing" ||
		rows[0].PricingVersion == nil || *rows[0].PricingVersion != v1 || rows[0].Warnings == nil {
		t.Fatalf("ListRunUsage: %+v %v", rows, err)
	}
}

// AC-14: границы отчёта — чужой прогон, диапазон, учёт по политике.
func TestReportUsageBoundaries(t *testing.T) {
	ctx := context.Background()
	w := newPricingWorld(t)
	w.activate(t, priceDoc("15"))
	task, runID := w.task(t, "Границы", "")
	count := func() (rows, events int) {
		if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM stage_run_usage WHERE run_id = $1`, runID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id = $1 AND type = $2`,
			task.ID, domain.EventUsageReported).Scan(&events); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Отчитывается тот, кто забирал: чужой — 403, строки нет.
	other, err := w.store.ResolveActor(ctx, w.store.DB(), "stranger", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.engine.ReportUsage(ctx, task.Key, other.ID, ReportUsageInput{
		Model: "M", InputTokens: i64(1), OutputTokens: i64(1), StageRunID: &runID})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("чужой отчёт: %v", err)
	}

	// Сумма по прайсу и названная агентом, не помещающиеся в numeric(12,6), —
	// 400 с текстом о диапазоне, ни строки, ни события.
	for name, in := range map[string]ReportUsageInput{
		"по прайсу":            {Model: "M", InputTokens: i64(0), OutputTokens: i64(1_000_000_000_000)},
		"названная":            {Model: "M", InputTokens: i64(1), OutputTokens: i64(1), CostUSD: fl(1_000_000)},
		"названная без прайса": {Model: "other", CostUSD: fl(5e9)},
	} {
		_, err := w.engine.ReportUsage(ctx, task.Key, w.system.ID, in)
		if apperr.As(err).Code != apperr.CodeInvalidInput || !strings.Contains(err.Error(), "не помещается") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if rows, events := count(); rows != 0 || events != 0 {
		t.Fatalf("отказанные отчёты оставили следы: строк %d, событий %d", rows, events)
	}

	// Верхняя граница проходит: 999999.999999.
	u := w.report(t, task.Key, usageIn{model: "ONE", in: i64(999_999_999_999), out: i64(0)})
	w.want(t, u.ID, "999999.999999", "pricing", 1, "")

	// Строка любого источника гасит долг по политике: состояние прогона —
	// reported (учёт считает строки, не источник).
	states, err := w.store.RunUsageStates(ctx, w.store.DB(), task.ID)
	if err != nil || states[runID] != domain.UsageStateReported {
		t.Fatalf("состояние учёта: %v %v", states[runID], err)
	}
}

// AC-10: миграция 000048 только добавляет. На заполненной базе старые строки
// сохраняют суммы и признак оплаты, новые колонки NULL; down снимает только
// добавленное; up → down → up проходит; CHECK держит значения.
func TestUsagePricingMigrationOnFilledBase(t *testing.T) {
	ctx := context.Background()
	w := newPricingWorld(t)
	db := w.store.DB()
	ver := w.activate(t, priceDoc("15"))
	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	columns := func() int {
		var n int
		if err := db.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'stage_run_usage'
			  AND column_name IN ('cost_source', 'pricing_version_id', 'reported_cost_usd')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	task, runID := w.task(t, "До апгрейда", "")

	// Состояние до миграции: строки пишутся без новых колонок.
	if _, err := db.Exec(ctx, read("000048_usage_pricing.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if columns() != 0 {
		t.Fatal("down оставил колонки")
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO stage_run_usage (run_id, model, input_tokens, output_tokens, cost_usd, billing, duration_ms, reported_by, details)
		VALUES ($1, 'claude-opus-5', 100, 20, 2.5, 'api', 1000, $2, '{"reported_by":"clab-runner"}'),
		       ($1, 'claude-sonnet-5', NULL, NULL, 1.25, 'subscription', NULL, $2, '{}'),
		       ($1, 'no-cost', NULL, NULL, NULL, NULL, NULL, $2, '{}')`, runID, w.system.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var s string
		if err := db.QueryRow(ctx, `
			SELECT coalesce(jsonb_agg(jsonb_build_array(model, input_tokens, output_tokens, cost_usd, billing, duration_ms, details)
			                          ORDER BY model)::text, '')
			FROM stage_run_usage WHERE run_id = $1`, runID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	counts := func() [5]int {
		var c [5]int
		if err := db.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM projects), (SELECT count(*) FROM tasks), (SELECT count(*) FROM stage_runs),
			       (SELECT count(*) FROM artifacts), (SELECT count(*) FROM stage_run_usage)`).
			Scan(&c[0], &c[1], &c[2], &c[3], &c[4]); err != nil {
			t.Fatal(err)
		}
		return c
	}
	countsBefore := counts()

	if _, err := db.Exec(ctx, read("000048_usage_pricing.up.sql")); err != nil {
		t.Fatalf("up на заполненной базе: %v", err)
	}
	if columns() != 3 {
		t.Fatal("up не завёл колонки")
	}
	var filled int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM stage_run_usage
		WHERE cost_source IS NOT NULL OR pricing_version_id IS NOT NULL OR reported_cost_usd IS NOT NULL`).Scan(&filled); err != nil || filled != 0 {
		t.Fatalf("старые строки получили значения: %d %v", filled, err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("строки изменились:\nдо    %s\nпосле %s", before, after)
	}
	usage, err := w.engine.TaskUsage(ctx, task.Key)
	if err != nil || !near(usage.CostUSD, 3.75) || !near(usage.APICostUSD, 2.5) || !near(usage.SubscriptionCostUSD, 1.25) {
		t.Fatalf("стоимость после апгрейда: %+v %v", usage, err)
	}

	// CHECK — негативная проверка.
	for _, sql := range []string{
		`INSERT INTO stage_run_usage (run_id, model, cost_source) VALUES ('` + runID.String() + `', 'm', 'x')`,
		`INSERT INTO stage_run_usage (run_id, model, cost_source) VALUES ('` + runID.String() + `', 'm', 'pricing')`,
		`INSERT INTO stage_run_usage (run_id, model, reported_cost_usd) VALUES ('` + runID.String() + `', 'm', -1)`,
		`INSERT INTO stage_run_usage (run_id, model, cost_source, pricing_version_id)
		 VALUES ('` + runID.String() + `', 'm', 'pricing', gen_random_uuid())`,
	} {
		if _, err := db.Exec(ctx, sql); err == nil {
			t.Errorf("CHECK/FK пропустил: %s", sql)
		}
	}

	// Новая строка по прайсу, затем down → данные прежних колонок целы.
	u := w.report(t, task.Key, tokens(1_000_000, 1_000_000))
	w.want(t, u.ID, "18.000000", "pricing", ver, "")
	if _, err := db.Exec(ctx, read("000048_usage_pricing.down.sql")); err != nil {
		t.Fatalf("down с данными: %v", err)
	}
	if columns() != 0 {
		t.Fatal("второй down оставил колонки")
	}
	countsAfter := counts()
	countsBefore[4]++ // строка по прайсу осталась, пропали только колонки
	if countsAfter != countsBefore {
		t.Fatalf("down тронул данные: было %v, стало %v", countsBefore, countsAfter)
	}
	if _, err := db.Exec(ctx, read("000048_usage_pricing.up.sql")); err != nil {
		t.Fatalf("второй up: %v", err)
	}
}
