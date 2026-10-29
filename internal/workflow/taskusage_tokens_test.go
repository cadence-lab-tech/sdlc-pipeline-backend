package workflow

import (
	"context"
	"testing"
)

// CLAB-273: токены задачи считаются по полям отдельно. Строка учёта, где
// сообщён только один из двух счётчиков, не выпадает из суммы целиком;
// определение совпадает с CostMetrics и с учётом прогонов в карточке.

// reportTokens — строка учёта с произвольным набором сообщённых полей.
func (w billingWorld) reportTokens(t *testing.T, key, model string, in, out *int64, cost *float64) {
	t.Helper()
	if _, err := w.engine.ReportUsage(context.Background(), key, w.system.ID, ReportUsageInput{
		Model: model, InputTokens: in, OutputTokens: out, CostUSD: cost,
	}); err != nil {
		t.Fatalf("отчёт %s in=%v out=%v: %v", model, in, out, err)
	}
}

// AC1-AC5.
func TestTaskUsageTokensCountFieldsSeparately(t *testing.T) {
	ctx := context.Background()
	w := newBillingWorld(t)

	// AC5: задача без единой строки — tokens это число 0.
	empty, _ := w.task(t, "Без учёта", "")
	if u, err := w.engine.TaskUsage(ctx, empty.Key); err != nil || u.Tokens != 0 {
		t.Fatalf("задача без строк: tokens=%d, err=%v; ждали 0", u.Tokens, err)
	}

	task, runID := w.task(t, "Смешанные строки", "")
	one := 1.0
	w.reportTokens(t, task.Key, "tokens-mix", ptr(int64(100)), nil, nil)         // только input
	w.reportTokens(t, task.Key, "tokens-mix", nil, ptr(int64(20)), nil)          // только output
	w.reportTokens(t, task.Key, "tokens-mix", ptr(int64(5)), ptr(int64(7)), nil) // полная
	w.reportTokens(t, task.Key, "tokens-mix", nil, nil, &one)                    // только стоимость

	// AC1: 100 + 20 + 5 + 7. На коде до правки — 12 (выпали обе частичные).
	usage, err := w.engine.TaskUsage(ctx, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Tokens != 132 {
		t.Fatalf("TaskUsage.Tokens = %d, ждали 132", usage.Tokens)
	}
	if !near(usage.CostUSD, 1) {
		t.Errorf("стоимость изменилась: %v", usage.CostUSD)
	}

	// AC2: сумма сходится с учётом прогона в карточке (nil как 0).
	runs, err := w.store.CardStageRuns(ctx, w.store.DB(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var card int64
	found := false
	for _, r := range runs {
		if r.ID == runID {
			found = true
		}
		if r.Usage.InputTokens != nil {
			card += *r.Usage.InputTokens
		}
		if r.Usage.OutputTokens != nil {
			card += *r.Usage.OutputTokens
		}
	}
	if !found {
		t.Fatalf("прогона %s нет в карточке", runID)
	}
	if card != usage.Tokens {
		t.Errorf("карточка: %d токенов по прогонам, TaskUsage: %d", card, usage.Tokens)
	}

	// AC3: разрез model в метриках — та же сумма.
	metrics, err := w.engine.ProjectMetrics(ctx, "SDLC", 0)
	if err != nil {
		t.Fatal(err)
	}
	var byModel int64 = -1
	for _, m := range metrics.Cost {
		if m.Dimension == "model" && m.Key == "tokens-mix" {
			byModel = m.Tokens
		}
	}
	if byModel != usage.Tokens {
		t.Errorf("метрики по модели: %d, TaskUsage: %d", byModel, usage.Tokens)
	}
}

// AC4: поле по-прежнему считается по поддереву, ребёнок с одним output
// попадает и в свою сумму, и в сумму эпика.
func TestTaskUsageTokensSubtreePartialRow(t *testing.T) {
	ctx := context.Background()
	w := newBillingWorld(t)
	parent, _ := w.task(t, "Эпик токенов", "")
	child, _ := w.task(t, "Ребёнок токенов", parent.Key)

	w.reportTokens(t, parent.Key, "m", ptr(int64(10)), nil, nil)
	w.reportTokens(t, child.Key, "m", nil, ptr(int64(50)), nil)

	cu, err := w.engine.TaskUsage(ctx, child.Key)
	if err != nil {
		t.Fatal(err)
	}
	if cu.Tokens != 50 {
		t.Errorf("tokens ребёнка = %d, ждали 50", cu.Tokens)
	}
	pu, err := w.engine.TaskUsage(ctx, parent.Key)
	if err != nil {
		t.Fatal(err)
	}
	if pu.Tokens != 60 {
		t.Errorf("tokens эпика = %d, ждали 60 (10 своих + 50 ребёнка)", pu.Tokens)
	}
}
