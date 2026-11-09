// Сумма строки учёта по прайсу версии настройки агентов (CLAB-426, решение
// 0077 п. 4.11). Токены — первичный факт (решение 0026), а сумму, когда цена
// известна, считает сервер: суммы разных агентов становятся сравнимыми, а
// бюджет команды не зависит от того, как агент посчитал сам.
//
// Расчёт — чистая функция над документом версии: без базы, без имён агентов
// и моделей в коде, только то, что лежит в pricing.models.<id>.
package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// maxCostMicroUSD — наибольшая сумма numeric(12,6) в миллионных долях
// доллара: 999999.999999.
const maxCostMicroUSD = int64(999_999_999_999)

// modelPrice — цены одной модели, USD за 1 000 000 токенов. Цены кэша
// необязательны: нет цены — токены кэша в сумму не идут.
type modelPrice struct {
	input, output         *big.Rat
	cacheRead, cacheWrite *big.Rat
}

// usageTokens — счётчики отчёта; nil — «не сообщили».
type usageTokens struct {
	input, output, cacheRead, cacheWrite *int64
}

// modelPriceIn достаёт цены модели из документа версии настройки агентов.
// Совпадение точное: регистр и пробелы значимы. false — цены нет: нет
// pricing, нет модели или цена не разбирается (документ проверяет
// ValidateAgentSettings при записи, так что последнее — страховка, а не путь).
func modelPriceIn(doc json.RawMessage, model string) (modelPrice, bool) {
	var d struct {
		Pricing *struct {
			Models map[string]map[string]json.RawMessage `json:"models"`
		} `json:"pricing"`
	}
	if err := json.Unmarshal(doc, &d); err != nil || d.Pricing == nil {
		return modelPrice{}, false
	}
	raw, ok := d.Pricing.Models[model]
	if !ok {
		return modelPrice{}, false
	}
	rat := func(field string, required bool) (*big.Rat, bool) {
		v, ok := raw[field]
		if !ok {
			return nil, !required
		}
		r, ok := new(big.Rat).SetString(string(bytes.TrimSpace(v)))
		if !ok || r.Sign() < 0 {
			return nil, false
		}
		return r, true
	}
	var p modelPrice
	var ok1, ok2, ok3, ok4 bool
	if p.input, ok1 = rat("input", true); !ok1 {
		return modelPrice{}, false
	}
	if p.output, ok2 = rat("output", true); !ok2 {
		return modelPrice{}, false
	}
	if p.cacheRead, ok3 = rat("cache_read", false); !ok3 {
		return modelPrice{}, false
	}
	if p.cacheWrite, ok4 = rat("cache_write", false); !ok4 {
		return modelPrice{}, false
	}
	return p, true
}

// costMicroUSD — сумма в миллионных долях доллара:
// Σ токены × цена / 1 000 000 долларов — это ровно Σ токены × цена
// миллионных долей, поэтому считается без деления и без float64. Округление
// до 6 знаков (numeric(12,6)) — половина от нуля. Нужны входные и выходные
// токены; кэш входит, только если есть и токены, и цена.
// Сумма за пределом numeric(12,6) — invalid_input.
func costMicroUSD(p modelPrice, t usageTokens) (int64, error) {
	if t.input == nil || t.output == nil {
		return 0, fmt.Errorf("costMicroUSD: нужны входные и выходные токены")
	}
	sum := new(big.Rat)
	add := func(tokens *int64, price *big.Rat) error {
		if tokens == nil || price == nil {
			return nil
		}
		if *tokens < 0 {
			return apperr.InvalidInput("токены не могут быть отрицательными: %d", *tokens)
		}
		sum.Add(sum, new(big.Rat).Mul(new(big.Rat).SetInt64(*tokens), price))
		return nil
	}
	for _, term := range []struct {
		tokens *int64
		price  *big.Rat
	}{{t.input, p.input}, {t.output, p.output}, {t.cacheRead, p.cacheRead}, {t.cacheWrite, p.cacheWrite}} {
		if err := add(term.tokens, term.price); err != nil {
			return 0, err
		}
	}

	// floor(sum + 1/2) = floor((2·num + den) / (2·den)) при sum >= 0.
	num := new(big.Int).Mul(sum.Num(), big.NewInt(2))
	num.Add(num, sum.Denom())
	micro := new(big.Int).Quo(num, new(big.Int).Mul(sum.Denom(), big.NewInt(2)))
	if !micro.IsInt64() || micro.Int64() > maxCostMicroUSD {
		return 0, errCostOutOfRange(fmt.Sprintf("посчитана по прайсу: %s USD", formatMicro(micro)))
	}
	return micro.Int64(), nil
}

// errCostOutOfRange — сумма не помещается в numeric(12,6). Одно правило на
// оба источника: и посчитанная по прайсу, и названная агентом (раньше база
// отвечала на вторую внутренней ошибкой).
func errCostOutOfRange(what string) error {
	return apperr.InvalidInput(
		"сумма (%s) не помещается в учёт: допустимо меньше 1 000 000 USD; проверьте токены и цену", what)
}

// formatMicro — миллионные доли доллара как десятичная запись.
func formatMicro(micro *big.Int) string {
	r := new(big.Rat).SetFrac(micro, big.NewInt(1_000_000))
	return r.FloatString(6)
}

// reportedCostInRange — названная агентом сумма помещается в numeric(12,6)
// после округления до 6 знаков.
func reportedCostInRange(c float64) bool {
	return c < float64(maxCostMicroUSD)/1e6+0.5e-6
}

// pricingWarnings — чего не хватает отчёту, чтобы сумма пошла по прайсу.
func pricingWarnings(version int, model string, t usageTokens) []string {
	var missing []string
	if t.input == nil {
		missing = append(missing, "input_tokens")
	}
	if t.output == nil {
		missing = append(missing, "output_tokens")
	}
	if len(missing) == 0 {
		return nil
	}
	names := missing[0]
	if len(missing) == 2 {
		names = missing[0] + " и " + missing[1]
	}
	return []string{fmt.Sprintf(
		"модель %q есть в прайсе версии %d, но не даны %s: сумма не посчитана по прайсу, "+
			"строка записана как сообщена (или без суммы); укажите оба счётчика — входные и выходные токены",
		model, version, names)}
}
