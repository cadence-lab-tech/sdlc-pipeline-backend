package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// CLAB-426: расчёт суммы по прайсу — чистая функция, без базы. Номера — свои
// критерии приёмки задачи (AC-1, AC-3, AC-5, AC-6, AC-14).

const priceDocM = `{"pricing":{"as_of":"2026-11-01","models":{
	"M":{"input":3,"output":15,"cache_read":0.3,"cache_write":3.75},
	"NOCACHE":{"input":3,"output":15},
	"HALF":{"input":0.5,"output":0},
	"QUARTER":{"input":0.25,"output":0},
	"ONE":{"input":1,"output":0}}}}`

func i64(v int64) *int64 { return &v }

func TestCostMicroUSD(t *testing.T) {
	tok := func(in, out int64, cr, cw *int64) usageTokens {
		return usageTokens{input: i64(in), output: i64(out), cacheRead: cr, cacheWrite: cw}
	}
	for _, tc := range []struct {
		name  string
		model string
		t     usageTokens
		want  int64 // миллионные доли доллара
	}{
		{"AC-1 миллион и миллион", "M", tok(1_000_000, 1_000_000, nil, nil), 18_000_000},
		{"AC-1 точная дробь", "M", tok(1234, 567, nil, nil), 12_207},
		{"AC-1 кэш с ценами", "M", tok(1_000_000, 1_000_000, i64(2_000_000), i64(1_000_000)), 22_350_000},
		{"AC-1 кэш без цен", "NOCACHE", tok(1_000_000, 1_000_000, i64(2_000_000), i64(1_000_000)), 18_000_000},
		{"AC-1 половина от нуля", "HALF", tok(1, 0, nil, nil), 1},
		{"AC-6 нули", "M", tok(0, 0, nil, nil), 0},
		{"AC-6 меньше половины", "QUARTER", tok(1, 0, nil, nil), 0},
		{"кэш-токены без цены не входят, цена есть у другого", "M", tok(0, 0, i64(1_000_000), nil), 300_000},
		{"граница диапазона", "ONE", tok(999_999_999_999, 0, nil, nil), 999_999_999_999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			price, ok := modelPriceIn([]byte(priceDocM), tc.model)
			if !ok {
				t.Fatalf("цены %s нет", tc.model)
			}
			got, err := costMicroUSD(price, tc.t)
			if err != nil || got != tc.want {
				t.Fatalf("получили %d, %v; ждали %d", got, err, tc.want)
			}
		})
	}

	// AC-14: сумма за пределом numeric(12,6) — invalid_input с текстом.
	price, _ := modelPriceIn([]byte(priceDocM), "M")
	for _, tc := range []usageTokens{
		{input: i64(0), output: i64(1_000_000_000_000)},
		{input: i64(9_000_000_000_000_000_000), output: i64(0)},
	} {
		_, err := costMicroUSD(price, tc)
		if apperr.As(err).Code != apperr.CodeInvalidInput || !strings.Contains(err.Error(), "не помещается") {
			t.Fatalf("за пределом: %v", err)
		}
	}
	one, _ := modelPriceIn([]byte(priceDocM), "ONE")
	if _, err := costMicroUSD(one, tok(1_000_000_000_000, 0, nil, nil)); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("ровно 1 000 000 USD: %v", err)
	}
	if _, err := costMicroUSD(price, usageTokens{input: i64(-1), output: i64(0)}); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("отрицательные токены: %v", err)
	}
}

// AC-3, AC-4: цена есть только при точном совпадении модели и при pricing
// в документе; иначе «как сегодня».
func TestModelPriceLookup(t *testing.T) {
	for _, tc := range []struct {
		name, doc, model string
		want             bool
	}{
		{"точное имя", priceDocM, "M", true},
		{"другая модель", priceDocM, "m-other", false},
		{"регистр", priceDocM, "m", false},
		{"пробел в начале", priceDocM, " M", false},
		{"пробел в конце", priceDocM, "M ", false},
		{"пустая модель", priceDocM, "", false},
		{"нет pricing", `{}`, "M", false},
		{"pricing без models", `{"pricing":{"as_of":"2026-11-01"}}`, "M", false},
		{"документ не объект", `[]`, "M", false},
		{"цена не число", `{"pricing":{"models":{"M":{"input":"3","output":1}}}}`, "M", false},
		{"нет цены output", `{"pricing":{"models":{"M":{"input":3}}}}`, "M", false},
	} {
		if _, ok := modelPriceIn(json.RawMessage(tc.doc), tc.model); ok != tc.want {
			t.Errorf("%s: цена найдена = %v, ждали %v", tc.name, ok, tc.want)
		}
	}
}

// AC-5: предупреждение называет недостающий счётчик.
func TestPricingWarnings(t *testing.T) {
	if w := pricingWarnings(3, "M", usageTokens{input: i64(1), output: i64(2)}); w != nil {
		t.Fatalf("оба счётчика есть: %v", w)
	}
	for _, tc := range []struct {
		t    usageTokens
		want []string
	}{
		{usageTokens{}, []string{"input_tokens и output_tokens"}},
		{usageTokens{input: i64(1)}, []string{"output_tokens"}},
		{usageTokens{output: i64(1)}, []string{"input_tokens"}},
	} {
		w := pricingWarnings(3, "M", tc.t)
		if len(w) != 1 || !strings.Contains(w[0], tc.want[0]) || !strings.Contains(w[0], "версии 3") ||
			!strings.Contains(w[0], "не посчитана по прайсу") {
			t.Errorf("%+v: %v", tc.t, w)
		}
	}
}

func TestReportedCostRange(t *testing.T) {
	for c, want := range map[float64]bool{0: true, 18: true, 999999.999999: true, 999999.9999994: true,
		999999.9999995: false, 1_000_000: false, 1e12: false} {
		if got := reportedCostInRange(c); got != want {
			t.Errorf("%v: %v, ждали %v", c, got, want)
		}
	}
}
