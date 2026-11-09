package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Проверка документа настройки агентов (CLAB-425, решение 0077 п. 4.2) без
// базы: таблица отказов с путями, границы с обеих сторон, секреты,
// предупреждения, канонический sha256, право по классу актора. Номера —
// критерии приёмки аналитики CLAB-425.

// exampleAgentSettings — пример из 0077, п. 4.1.
const exampleAgentSettings = `{
  "agents": {
    "claude": {
      "models": ["claude-haiku-4-5", "claude-sonnet-5", "claude-opus-5-5"],
      "levels": {
        "low": {"model": "claude-haiku-4-5"},
        "medium": {"model": "claude-sonnet-5"},
        "high": {"model": "claude-opus-5-5", "effort": "high"}
      }
    },
    "codex": {
      "levels": {
        "high": {"model": "gpt-6.1-sol", "effort": "high"}
      }
    }
  },
  "pricing": {
    "as_of": "2026-11-01",
    "models": {
      "claude-opus-5-5": {"input": 1.0, "output": 2.0, "cache_read": 0.1, "cache_write": 1.25},
      "gpt-6.1-sol": {"input": 1.0, "output": 2.0, "cache_read": 0.1}
    }
  }
}`

func pathOf(err error) (string, bool) {
	e := apperr.As(err)
	if e.Code != apperr.CodeInvalidInput {
		return "", false
	}
	d, _ := e.Details.(map[string]any)
	p, ok := d["path"].(string)
	return p, ok
}

// level — документ с одним агентом claude и одной записью уровня high.
func level(entry string) string {
	return `{"agents": {"claude": {"levels": {"high": ` + entry + `}}}}`
}

func price(entry string) string {
	return `{"pricing": {"as_of": "2026-11-01", "models": {"m": ` + entry + `}}}`
}

// AC-5: пример принимается без отказов и предупреждений, каждое нарушение —
// 400 с ожидаемым путём, границы — с обеих сторон.
func TestValidateAgentSettingsTable(t *testing.T) {
	warnings, err := ValidateAgentSettings(json.RawMessage(exampleAgentSettings))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("пример 0077: %v %v", warnings, err)
	}

	m200, m201 := strings.Repeat("m", 200), strings.Repeat("m", 201)
	e32, e33 := strings.Repeat("e", 32), strings.Repeat("e", 33)
	a32, a33 := "a"+strings.Repeat("b", 31), "a"+strings.Repeat("b", 32)
	for _, c := range []struct {
		name, doc, path string
	}{
		// пусто — проходит
		{"пустой документ", `{}`, "-"},
		{"корневое неизвестное", `{"foo": 1}`, "foo"},
		{"неизвестное у уровня", level(`{"model": "x", "temperature": 0.2}`), "agents.claude.levels.high.temperature"},
		{"неизвестное у прайса", `{"pricing": {"as_of": "2026-11-01", "currency": "usd"}}`, "pricing.currency"},
		{"неизвестное у агента", `{"agents": {"claude": {"default": "x"}}}`, "agents.claude.default"},
		{"неизвестное у цены", price(`{"input": 1, "output": 1, "batch": 1}`), "pricing.models.m.batch"},
		{"уровень ultra", `{"agents": {"claude": {"levels": {"ultra": {"model": "x"}}}}}`, "agents.claude.levels.ultra"},
		{"агент с заглавной", `{"agents": {"Claude": {}}}`, "agents.Claude"},
		{"агент 32 символа", `{"agents": {"` + a32 + `": {}}}`, "-"},
		{"агент 33 символа", `{"agents": {"` + a33 + `": {}}}`, "agents." + a33},
		{"модель пустая", level(`{"model": ""}`), "agents.claude.levels.high.model"},
		{"модель нет", level(`{"effort": "high"}`), "agents.claude.levels.high.model"},
		{"модель 200", level(`{"model": "` + m200 + `"}`), "-"},
		{"модель 201", level(`{"model": "` + m201 + `"}`), "agents.claude.levels.high.model"},
		{"модель с пробелом", level(`{"model": "claude opus"}`), "agents.claude.levels.high.model"},
		{"модель с табуляцией", level(`{"model": "claude\topus"}`), "agents.claude.levels.high.model"},
		{"модель с управляющим", level(`{"model": "claude\u0007"}`), "agents.claude.levels.high.model"},
		{"модель не строка", level(`{"model": 5}`), "agents.claude.levels.high.model"},
		{"models с пробелом", `{"agents": {"claude": {"models": ["a", "b c"]}}}`, "agents.claude.models[1]"},
		{"models пустая строка", `{"agents": {"claude": {"models": [""]}}}`, "agents.claude.models[0]"},
		{"models не массив", `{"agents": {"claude": {"models": "a"}}}`, "agents.claude.models"},
		{"модель вне models", `{"agents": {"claude": {"models": ["a"], "levels": {"low": {"model": "b"}}}}}`, "agents.claude.levels.low.model"},
		{"models не задан — любая", `{"agents": {"claude": {"levels": {"low": {"model": "b"}}}}}`, "-"},
		{"models пустой — ни одной", `{"agents": {"claude": {"models": [], "levels": {"low": {"model": "b"}}}}}`, "agents.claude.levels.low.model"},
		{"effort пустой", level(`{"model": "x", "effort": ""}`), "agents.claude.levels.high.effort"},
		{"effort 32", level(`{"model": "x", "effort": "` + e32 + `"}`), "-"},
		{"effort 33", level(`{"model": "x", "effort": "` + e33 + `"}`), "agents.claude.levels.high.effort"},
		{"effort с пробелом", level(`{"model": "x", "effort": "very high"}`), "agents.claude.levels.high.effort"},
		{"effort не строка", level(`{"model": "x", "effort": 3}`), "agents.claude.levels.high.effort"},
		{"цена отрицательная", price(`{"input": -0.01, "output": 1}`), "pricing.models.m.input"},
		{"цена строкой", price(`{"input": "1", "output": 1}`), "pricing.models.m.input"},
		{"цена бесконечная", price(`{"input": 1, "output": 1e999}`), "pricing.models.m.output"},
		{"цена больше предела", price(`{"input": 1000000.01, "output": 1}`), "pricing.models.m.input"},
		{"цена 0 и 1000000", price(`{"input": 0, "output": 1000000}`), "-"},
		{"кэш отрицательный", price(`{"input": 1, "output": 1, "cache_write": -1}`), "pricing.models.m.cache_write"},
		{"нет output", price(`{"input": 1}`), "pricing.models.m.output"},
		{"нет input", price(`{"output": 1}`), "pricing.models.m.input"},
		{"ключ прайса с пробелом", `{"pricing": {"as_of": "2026-11-01", "models": {"a b": {"input": 1, "output": 1}}}}`, "pricing.models.a b"},
		{"pricing без as_of", `{"pricing": {"models": {}}}`, "pricing.as_of"},
		{"as_of месяц 13", `{"pricing": {"as_of": "2026-13-01"}}`, "pricing.as_of"},
		{"as_of без нулей", `{"pricing": {"as_of": "2026-2-1"}}`, "pricing.as_of"},
		{"as_of по-русски", `{"pricing": {"as_of": "01.11.2026"}}`, "pricing.as_of"},
		{"as_of 30 февраля", `{"pricing": {"as_of": "2026-02-30"}}`, "pricing.as_of"},
		{"документ массив", `[]`, ""},
		{"документ строка", `"x"`, ""},
		{"документ null", `null`, ""},
		{"документ два объекта", `{} {}`, ""},
		{"не JSON", `{`, ""},
	} {
		_, err := ValidateAgentSettings(json.RawMessage(c.doc))
		if c.path == "-" {
			if err != nil {
				t.Errorf("%s: отказ %v", c.name, err)
			}
			continue
		}
		got, ok := pathOf(err)
		if !ok || got != c.path {
			t.Errorf("%s: путь %q (%v), ожидали %q", c.name, got, err, c.path)
		}
	}
}

// AC-5: предел размера — по байтам JSON документа, 65536 проходит, 65537 нет.
func TestValidateAgentSettingsSize(t *testing.T) {
	doc := sizedAgentSettings
	at, over := doc(AgentSettingsMaxBytes), doc(AgentSettingsMaxBytes+1)
	if len(at) != 65536 || len(over) != 65537 {
		t.Fatalf("размеры заготовок: %d %d", len(at), len(over))
	}
	if _, err := ValidateAgentSettings(at); err != nil {
		t.Fatalf("65536 байт: %v", err)
	}
	if p, ok := pathOf(func() error { _, err := ValidateAgentSettings(over); return err }()); !ok || p != "" {
		t.Fatalf("65537 байт: путь %q %v", p, ok)
	}
}

// AC-13: секрет по имени ключа и по значению sdlc_… — отказ с путём, в
// тексте отказа значения нет; похожее без признаков секрета проходит.
func TestValidateAgentSettingsSecrets(t *testing.T) {
	const secret = "sdlc_SeCrEt0123456789abcdef"
	for _, c := range []struct {
		name, doc, path string
	}{
		{"поле token", `{"token": "x"}`, "token"},
		{"поле Secret", `{"agents": {"claude": {"Secret": "x"}}}`, "agents.claude.Secret"},
		{"поле api_key", `{"pricing": {"api_key": "x"}}`, "pricing.api_key"},
		{"поле apikey", `{"apikey": "x"}`, "apikey"},
		{"поле password в уровне", level(`{"model": "x", "password": "x"}`), "agents.claude.levels.high.password"},
		{"ключ агента с token", `{"agents": {"my-token-bot": {}}}`, "agents.my-token-bot"},
		{"ключ прайса с secret", `{"pricing": {"as_of": "2026-11-01", "models": {"secret-model": {"input": 1, "output": 1}}}}`, "pricing.models.secret-model"},
		{"значение model", level(`{"model": "` + secret + `"}`), "agents.claude.levels.high.model"},
		{"значение effort", level(`{"model": "x", "effort": "` + secret + `"}`), "agents.claude.levels.high.effort"},
		{"элемент models", `{"agents": {"claude": {"models": ["a", "` + secret + `"]}}}`, "agents.claude.models[1]"},
		{"ключ pricing.models", `{"pricing": {"as_of": "2026-11-01", "models": {"` + secret + `": {"input": 1, "output": 1}}}}`, "pricing.models.[sdlc_…]"},
		{"секрет в неизвестном поле", `{"foo": {"bar": "` + secret + `"}}`, "foo.bar"},
	} {
		_, err := ValidateAgentSettings(json.RawMessage(c.doc))
		got, ok := pathOf(err)
		if !ok || got != c.path {
			t.Errorf("%s: путь %q (%v), ожидали %q", c.name, got, err, c.path)
			continue
		}
		if raw, _ := json.Marshal(apperr.As(err)); strings.Contains(err.Error(), secret) || strings.Contains(string(raw), secret) {
			t.Errorf("%s: значение секрета в отказе: %s", c.name, err)
		}
	}
	if _, err := ValidateAgentSettings(json.RawMessage(level(`{"model": "gpt-6.1-sol"}`))); err != nil {
		t.Fatalf("обычная модель отбита: %v", err)
	}
}

// AC-6: агент вне набора — предупреждение, известные — без.
func TestValidateAgentSettingsWarnings(t *testing.T) {
	w, err := ValidateAgentSettings(json.RawMessage(`{"agents": {"acme-cli": {}, "claude": {}, "codex": {}, "generic": {}}}`))
	if err != nil || len(w) != 1 || !strings.Contains(w[0], "acme-cli") {
		t.Fatalf("предупреждения: %v %v", w, err)
	}
}

// AC-9: sha256 — по каноническому JSON: порядок ключей и пробелы не важны,
// изменённое значение — другое.
func TestAgentSettingsSHA256Canonical(t *testing.T) {
	a, _, err := AgentSettingsSHA256(json.RawMessage(`{"pricing": {"as_of": "2026-11-01"}, "agents": {"claude": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := AgentSettingsSHA256(json.RawMessage(`{"agents":{"claude":{}},"pricing":{"as_of":"2026-11-01"}}`))
	c, _, _ := AgentSettingsSHA256(json.RawMessage(`{"agents":{"claude":{}},"pricing":{"as_of":"2026-11-02"}}`))
	if a != b || a == c || len(a) != 64 {
		t.Fatalf("sha256: %s %s %s", a, b, c)
	}
}

// AC-11 без базы: право записи по классу актора и причины отказа.
func TestAgentSettingsWriteRight(t *testing.T) {
	user := domain.Principal{ID: uuid.New(), Kind: domain.PrincipalUser}
	bot := domain.Principal{ID: uuid.New(), Kind: domain.PrincipalAgent}
	bound := &TaskBinding{TaskKey: "SDLC-1"}
	for _, c := range []struct {
		name   string
		actor  Actor
		reason string
	}{
		{"maintainer user", Actor{Principal: user, Level: domain.MemberMaintainer}, ""},
		{"admin user", Actor{Principal: user, Level: domain.MemberAdmin}, ""},
		{"maintainer bot", Actor{Principal: bot, Level: domain.MemberMaintainer}, DenyHumanAction},
		{"admin bot", Actor{Principal: bot, Level: domain.MemberAdmin}, DenyHumanAction},
		{"agent bot", Actor{Principal: bot, Level: domain.MemberAgent}, DenyHumanAction},
		{"employee", Actor{Principal: user, Level: domain.MemberAgent}, DenyLevel},
		{"viewer", Actor{Principal: user, Level: domain.MemberViewer}, DenyLevel},
		{"task token", Actor{Principal: user, Level: domain.MemberAdmin, Bound: bound}, "task"},
	} {
		err := AgentSettingsWriteRight(c.actor)
		switch c.reason {
		case "":
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
		case "task":
			if apperr.As(err).Code != apperr.CodeForbidden || !strings.HasPrefix(apperr.As(err).Message, ExecutorRefusal) {
				t.Errorf("%s: %v", c.name, err)
			}
		default:
			e := apperr.As(err)
			d, _ := e.Details.(map[string]any)
			if e.Code != apperr.CodeForbidden || d["reason"] != c.reason {
				t.Errorf("%s: %v %v, ожидали %s", c.name, err, e.Details, c.reason)
			}
		}
	}
}

// Модели уровня: агент без записи уровня не попадает, effort — если задан.
func TestAgentModelsFor(t *testing.T) {
	got, err := agentModelsFor(json.RawMessage(exampleAgentSettings), "high")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != `{"claude":{"model":"claude-opus-5-5","effort":"high"},"codex":{"model":"gpt-6.1-sol","effort":"high"}}` {
		t.Fatalf("high: %s", raw)
	}
	got, _ = agentModelsFor(json.RawMessage(exampleAgentSettings), "low")
	if raw, _ := json.Marshal(got); string(raw) != `{"claude":{"model":"claude-haiku-4-5"}}` {
		t.Fatalf("low: %s", raw)
	}
}

// sizedAgentSettings — годный документ ровно size байт компактного JSON:
// список моделей агента claude, добитый последней моделью нужной длины.
func sizedAgentSettings(size int) json.RawMessage {
	head, tail := `{"agents":{"claude":{"models":[`, `]}}}`
	room := size - len(head) - len(tail)
	var items []string
	for {
		sep := 0
		if len(items) > 0 {
			sep = 1
		}
		// Последняя модель забирает остаток, если он помещается в 200 символов.
		if last := room - sep - 2; last >= 1 && last <= 200 {
			items = append(items, strings.Repeat("m", last))
			break
		}
		items = append(items, strings.Repeat("m", 100))
		room -= sep + 102
	}
	return json.RawMessage(head + `"` + strings.Join(items, `","`) + `"` + tail)
}
