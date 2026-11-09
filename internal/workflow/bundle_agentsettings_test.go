package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Настройка агентов в связке (CLAB-428). Номера AC — критерии приёмки
// аналитики задачи.

func asAny(t *testing.T, doc string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func settingsBundle(t *testing.T, doc, changelog string) Bundle {
	t.Helper()
	return Bundle{Version: BundleVersion, AgentSettings: &BundleAgentSettings{Document: asAny(t, doc), Changelog: changelog}}
}

func settingsRows(t *testing.T, w levelWorld) (total int, drafts int) {
	t.Helper()
	if err := w.store.DB().QueryRow(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE state = 'draft') FROM agent_settings_versions`).Scan(&total, &drafts); err != nil {
		t.Fatal(err)
	}
	return total, drafts
}

func settingsEntry(t *testing.T, r ImportReport, err error) ImportEntry {
	t.Helper()
	if err != nil {
		t.Fatalf("импорт: %v", err)
	}
	if r.AgentSettings == nil || r.AgentSettings.Key != "agent_settings" {
		t.Fatalf("в отчёте нет строки agent_settings: %+v", r)
	}
	return *r.AgentSettings
}

const (
	settingsDocA = `{"agents":{"claude":{"levels":{"high":{"model":"claude-opus-5-5"}}}},"pricing":{"as_of":"2026-11-01","models":{"claude-opus-5-5":{"input":1.0,"output":2.0}}}}`
	settingsDocB = `{"agents":{"claude":{"levels":{"high":{"model":"claude-opus-5-6"}}}}}`
	settingsDocC = `{"agents":{"claude":{"levels":{"high":{"model":"claude-opus-5-7"}}}}}`
)

// AC-7, AC-8, AC-10, AC-11, AC-20: исходы по образцу цикла.
func TestImportAgentSettingsOutcomes(t *testing.T) {
	w := newLevelWorld(t)
	ctx := context.Background()
	who := w.human(t)

	// Активной нет, черновика нет → draft_created, changelog по умолчанию.
	rep, err := w.authoring.ImportAs(ctx, "SDLC", who, settingsBundle(t, settingsDocA, ""), true)
	if e := settingsEntry(t, rep, err); e.Outcome != "draft_created" {
		t.Fatalf("сухой прогон: %+v", e)
	}
	if rep.AgentSettingsChange != nil {
		t.Fatal("сухой прогон ничего не писал, а «было → стало» есть")
	}
	if total, _ := settingsRows(t, w); total != 0 {
		t.Fatalf("сухой прогон оставил %d строк", total)
	}
	rep, err = w.authoring.ImportAs(ctx, "SDLC", who, settingsBundle(t, settingsDocA, ""), false)
	e := settingsEntry(t, rep, err)
	if e.Outcome != "draft_created" || !strings.Contains(e.Detail, "версия 1") || e.ID == "" {
		t.Fatalf("первый импорт: %+v", e)
	}
	if rep.AgentSettingsChange == nil || rep.AgentSettingsChange.Action != AgentSettingsCreated ||
		rep.AgentSettingsChange.After == nil || rep.AgentSettingsChange.Before != nil {
		t.Fatalf("изменение для журнала: %+v", rep.AgentSettingsChange)
	}
	v, err := w.engine.AgentSettings(ctx, "SDLC", nil)
	if err != nil || v != nil {
		t.Fatalf("импорт активировал настройку: %v %v", v, err)
	}
	draft, err := w.engine.AgentSettings(ctx, "SDLC", ptr(1))
	if err != nil || draft.State != "draft" || draft.Changelog == nil || *draft.Changelog != "импорт связки" {
		t.Fatalf("черновик: %+v %v", draft, err)
	}

	// То же, с иной записью чисел, порядком ключей и пробелами → unchanged.
	same := `{ "pricing": {"models": {"claude-opus-5-5": {"output": 2, "input": 1}}, "as_of": "2026-11-01"}, "agents": {"claude": {"levels": {"high": {"model": "claude-opus-5-5"}}}} }`
	rep, err = w.authoring.ImportAs(ctx, "SDLC", who, settingsBundle(t, same, "другой changelog"), false)
	if e := settingsEntry(t, rep, err); e.Outcome != "unchanged" || !strings.Contains(e.Detail, "черновиком") || e.ID == "" {
		t.Fatalf("тот же документ: %+v", e)
	}
	if total, _ := settingsRows(t, w); total != 1 {
		t.Fatalf("unchanged изменил число строк: %d", total)
	}
	if rep.AgentSettingsChange != nil {
		t.Fatal("у unchanged «было → стало» нет")
	}

	// Отличается, черновик есть → updated в той же строке, changelog из связки.
	rep, err = w.authoring.ImportAs(ctx, "SDLC", who, settingsBundle(t, settingsDocB, "новые модели"), false)
	e = settingsEntry(t, rep, err)
	if e.Outcome != "updated" || !strings.Contains(e.Detail, "версии 1") {
		t.Fatalf("замена черновика: %+v", e)
	}
	if rep.AgentSettingsChange == nil || rep.AgentSettingsChange.Action != AgentSettingsReplaced || rep.AgentSettingsChange.Before == nil {
		t.Fatalf("изменение: %+v", rep.AgentSettingsChange)
	}
	if total, drafts := settingsRows(t, w); total != 1 || drafts != 1 {
		t.Fatalf("после замены строк %d, черновиков %d", total, drafts)
	}
	draft, _ = w.engine.AgentSettings(ctx, "SDLC", ptr(1))
	if *draft.Changelog != "новые модели" || !strings.Contains(string(draft.Document), "5-6") {
		t.Fatalf("черновик после замены: %+v", draft)
	}

	// Сухой прогон отличающегося: updated, а черновик не тронут.
	rep, err = w.authoring.ImportAs(ctx, "SDLC", who, settingsBundle(t, settingsDocC, ""), true)
	if e := settingsEntry(t, rep, err); e.Outcome != "updated" {
		t.Fatalf("сухой прогон замены: %+v", e)
	}
	draft, _ = w.engine.AgentSettings(ctx, "SDLC", ptr(1))
	if !strings.Contains(string(draft.Document), "5-6") {
		t.Fatalf("сухой прогон изменил черновик: %s", draft.Document)
	}

	// Активируем: совпадает с активной при другом черновике → unchanged,
	// черновик не тронут; активная не меняется ни одним импортом.
	if _, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", who, ptr(1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsDraftInput{Document: json.RawMessage(settingsDocC)}); err != nil {
		t.Fatal(err)
	}
	rep, err = w.authoring.ImportAs(ctx, "SDLC", who, settingsBundle(t, settingsDocB, ""), false)
	if e := settingsEntry(t, rep, err); e.Outcome != "unchanged" || !strings.Contains(e.Detail, "активной версией 1") || !strings.Contains(e.Detail, "черновик версии 2") {
		t.Fatalf("совпал с активной: %+v", e)
	}
	// Отличается от активной и от черновика → черновик заменён, активная жива.
	rep, err = w.authoring.ImportAs(ctx, "SDLC", who, settingsBundle(t, settingsDocA, ""), false)
	if e := settingsEntry(t, rep, err); e.Outcome != "updated" || !strings.Contains(e.Detail, "версии 2") {
		t.Fatalf("замена после активации: %+v", e)
	}
	act, _ := w.engine.AgentSettings(ctx, "SDLC", nil)
	if act == nil || act.Version != 1 || !strings.Contains(string(act.Document), "5-6") {
		t.Fatalf("активная версия изменилась: %+v", act)
	}
}

// AC-1, AC-2: экспорт несёт активную версию; черновик и пустой проект поля
// не дают, связка без поля не содержит ключа.
func TestExportAgentSettings(t *testing.T) {
	w := newLevelWorld(t)
	ctx := context.Background()
	who := w.human(t)

	b, err := w.authoring.Export(ctx, "SDLC")
	if err != nil || b.AgentSettings != nil {
		t.Fatalf("проект без настройки: %+v %v", b.AgentSettings, err)
	}
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "agent_settings") || b.Version != 3 {
		t.Fatalf("связка без настройки несёт ключ или не 3-й формат: %s", raw)
	}

	if _, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsDraftInput{Document: json.RawMessage(settingsDocA)}); err != nil {
		t.Fatal(err)
	}
	if b, err = w.authoring.Export(ctx, "SDLC"); err != nil || b.AgentSettings != nil {
		t.Fatalf("черновик попал в связку: %+v %v", b.AgentSettings, err)
	}

	if _, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", who, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsDraftInput{Document: json.RawMessage(settingsDocB)}); err != nil {
		t.Fatal(err)
	}
	b, err = w.authoring.Export(ctx, "SDLC")
	if err != nil || b.AgentSettings == nil {
		t.Fatalf("активной настройки нет в связке: %v", err)
	}
	got, _ := json.Marshal(b.AgentSettings.Document)
	want, _, _ := AgentSettingsSHA256(got)
	have, _, _ := AgentSettingsSHA256(json.RawMessage(settingsDocA))
	if want != have {
		t.Fatalf("документ связки не равен активному: %s", got)
	}

	// Круг: экспорт → импорт в тот же проект даёт unchanged, а не новую версию.
	rep, err := w.authoring.ImportAs(ctx, "SDLC", who, b, false)
	if e := settingsEntry(t, rep, err); e.Outcome != "unchanged" || !strings.Contains(e.Detail, "активной") {
		t.Fatalf("круг export → import: %+v", e)
	}
}

// AC-13: отказ проверки — до первой записи, откат целиком, сухой прогон
// отбивается так же, значение секрета в тексте отказа не появляется.
func TestImportAgentSettingsRejectedRollsBackBundle(t *testing.T) {
	w := newLevelWorld(t)
	ctx := context.Background()
	who := w.human(t)

	secret := "sdlc_topsecretvalue0123456789"
	long := strings.Repeat("я", 2001)
	cases := []struct {
		name, doc, changelog, path string
	}{
		{"секрет по значению", `{"agents":{"claude":{"levels":{"high":{"model":"` + secret + `"}}}}}`, "", ""},
		{"секрет по ключу", `{"agents":{"claude":{"api_key":"x"}}}`, "", ""},
		{"неизвестное поле", `{"foo":1}`, "", "foo"},
		{"changelog длиннее 2000", `{}`, long, "changelog"},
	}
	for _, c := range cases {
		for _, dry := range []bool{false, true} {
			b := settingsBundle(t, c.doc, c.changelog)
			b.Roles = []BundleRole{{Key: "ghost", Name: "Призрак", Instructions: "не должна появиться"}}
			rep, err := w.authoring.ImportAs(ctx, "SDLC", who, b, dry)
			e := apperr.As(err)
			if err == nil || e.Code != apperr.CodeInvalidInput {
				t.Fatalf("%s (dry=%v): %v", c.name, dry, err)
			}
			if strings.Contains(e.Message, secret) || false {
				t.Fatalf("%s: значение секрета в отказе: %s", c.name, e.Message)
			}
			d, _ := e.Details.(map[string]any)
			if d["bundle_field"] != "agent_settings" {
				t.Fatalf("%s: нет bundle_field: %v", c.name, e.Details)
			}
			if c.path != "" && d["path"] != c.path {
				t.Fatalf("%s: путь %v, ждали %q", c.name, d["path"], c.path)
			}
			if rep.AgentSettings != nil || len(rep.Roles) != 0 {
				t.Fatalf("%s: отчёт не пуст: %+v", c.name, rep)
			}
		}
	}
	roles, err := w.authoring.ListRoles(ctx, "SDLC", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if r.Key == "ghost" {
			t.Fatal("роль из отбитой связки записана")
		}
	}
	if total, _ := settingsRows(t, w); total != 0 {
		t.Fatalf("отбитая связка оставила %d версий", total)
	}

	// Нет document — отказ с путём document; не объект — отказ.
	for _, doc := range []any{nil, "строка", []any{1}} {
		b := Bundle{Version: BundleVersion, AgentSettings: &BundleAgentSettings{Document: doc}}
		if _, err := w.authoring.ImportAs(ctx, "SDLC", who, b, false); apperr.As(err).Code != apperr.CodeInvalidInput {
			t.Fatalf("документ %v: %v", doc, err)
		}
	}
}

// AC-11: сухой прогон связки с ролью и настройкой отдаёт оба исхода и не
// оставляет ни роли, ни версии.
func TestImportAgentSettingsDryRunWithRoles(t *testing.T) {
	w := newLevelWorld(t)
	ctx := context.Background()
	who := w.human(t)
	b := settingsBundle(t, settingsDocA, "")
	b.Roles = []BundleRole{{Key: "writer", Name: "Писатель", Instructions: "текст"}}
	rep, err := w.authoring.ImportAs(ctx, "SDLC", who, b, true)
	if e := settingsEntry(t, rep, err); e.Outcome != "draft_created" || len(rep.Roles) != 1 {
		t.Fatalf("сухой прогон: %+v", rep)
	}
	roles, _ := w.authoring.ListRoles(ctx, "SDLC", true)
	for _, r := range roles {
		if r.Key == "writer" {
			t.Fatal("сухой прогон записал роль")
		}
	}
	if total, _ := settingsRows(t, w); total != 0 {
		t.Fatalf("сухой прогон оставил %d версий", total)
	}
}

// AC-14: бот (не user) с maintainer и admin отбивается 403 human_action до
// любой записи — и в боевом, и в сухом прогоне; связка без поля работает.
func TestImportAgentSettingsBotRefused(t *testing.T) {
	w := newLevelWorld(t)
	ctx := context.Background()
	for _, level := range []domain.MemberRole{domain.MemberMaintainer, domain.MemberAdmin} {
		bot := Actor{Principal: w.pool, Level: level}
		for _, dry := range []bool{false, true} {
			b := settingsBundle(t, settingsDocA, "")
			b.Roles = []BundleRole{{Key: "ghost", Name: "Призрак", Instructions: "x"}}
			_, err := w.authoring.ImportAs(ctx, "SDLC", bot, b, dry)
			e := apperr.As(err)
			d, _ := e.Details.(map[string]any)
			if e.Code != apperr.CodeForbidden || d["reason"] != DenyHumanAction {
				t.Fatalf("бот %s dry=%v: %v", level, dry, err)
			}
		}
		if total, _ := settingsRows(t, w); total != 0 {
			t.Fatalf("бот записал настройку: %d", total)
		}
		// Та же роль без поля применяется.
		plain := Bundle{Version: BundleVersion, Roles: []BundleRole{{Key: "ghost", Name: "Призрак", Instructions: "x"}}}
		if _, err := w.authoring.ImportAs(ctx, "SDLC", bot, plain, false); err != nil {
			t.Fatalf("связка бота без настройки: %v", err)
		}
	}
	// Человек применяет.
	if _, err := w.authoring.ImportAs(ctx, "SDLC", w.human(t), settingsBundle(t, settingsDocA, ""), false); err != nil {
		t.Fatal(err)
	}
	// Import без актора связку с настройкой не принимает вовсе.
	if _, err := w.authoring.Import(ctx, "SDLC", w.human(t).Principal.ID, settingsBundle(t, settingsDocA, ""), false); err == nil {
		t.Fatal("Import принял связку с agent_settings мимо права")
	}
}

// AC-16: связка без поля — прежний отчёт, без ключа agent_settings.
func TestImportWithoutAgentSettingsHasNoKey(t *testing.T) {
	w := newLevelWorld(t)
	rep, err := w.authoring.ImportAs(context.Background(), "SDLC", w.human(t),
		Bundle{Version: BundleVersion, Roles: []BundleRole{{Key: "writer", Name: "Писатель", Instructions: "текст"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "agent_settings") {
		t.Fatalf("в отчёте без настройки есть ключ: %s", raw)
	}
}
