package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// CLAB-316: необязательные этапы цикла (решение 0066) в clab — флаг --stage,
// строка «Выбор этапов», назначение перехода с пропуском, строка «пропущено».

// Ответы сервера с выбором этапов: optional_stages есть всегда, пусть {}.
const (
	taskDesignOff = `{"key":"CLAB-1","seq":1,"title":"Кусок","description":"d","status_key":"new","priority":"normal",` +
		`"labels":[],"properties":{"type":"task"},"optional_stages":{"docs":true,"design":false}}`
	taskDesignOn = `{"key":"CLAB-1","seq":1,"title":"Кусок","description":"d","status_key":"new","priority":"normal",` +
		`"labels":[],"properties":{"type":"task"},"optional_stages":{"design":true}}`
	taskNoOptional = `{"key":"CLAB-1","seq":1,"title":"Кусок","description":"d","status_key":"new","priority":"normal",` +
		`"labels":[],"properties":{"type":"task"},"optional_stages":{}}`
	// Сервер до 0066: ключа optional_stages в задаче нет.
	taskOldServer = `{"key":"CLAB-1","seq":1,"title":"Кусок","description":"d","status_key":"new","priority":"normal",` +
		`"labels":[],"properties":{"type":"task"}}`
)

func runCreate(t *testing.T, status int, reply string, args ...string) (stdout string, err error, rec *inviteServer) {
	t.Helper()
	c, rec := newInviteClient(t, status, reply)
	stdout, _, err = capture(t, func() error {
		return cmdCreate(context.Background(), c, append([]string{"--project", "CLAB", "--title", "t"}, args...))
	})
	return stdout, err, rec
}

func runEdit(t *testing.T, status int, reply string, args ...string) (stdout string, err error, rec *inviteServer) {
	t.Helper()
	c, rec := newInviteClient(t, status, reply)
	stdout, _, err = capture(t, func() error {
		return cmdEdit(context.Background(), c, append([]string{"CLAB-1"}, args...))
	})
	return stdout, err, rec
}

// CLI-1, CLI-5: create кладёт выбор в тело и печатает набор задачи.
func TestCreateStageBody(t *testing.T) {
	withJSON(t, false)
	out, err, rec := runCreate(t, http.StatusCreated, taskDesignOff, "--stage", "design=off")
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != "POST" || rec.path != "/v1/projects/CLAB/tasks" ||
		!equalJSON(rec.body, map[string]any{"title": "t", "optional_stages": map[string]bool{"design": false}}) {
		t.Fatalf("%s %s %v", rec.method, rec.path, rec.body)
	}
	if !strings.Contains(out, "\nВыбор этапов: design выкл, docs вкл\n") {
		t.Fatalf("нет строки этапов:\n%s", out)
	}

	_, err, rec = runCreate(t, http.StatusCreated, taskDesignOff, "--stage", "design=on", "--stage", "docs=off")
	if err != nil || !equalJSON(rec.body, map[string]any{"title": "t",
		"optional_stages": map[string]bool{"design": true, "docs": false}}) {
		t.Fatalf("повторяемый флаг: %v %v", err, rec.body)
	}
}

// CLI-2: без --stage ключа в теле нет; пустой набор — строки нет.
func TestCreateWithoutStage(t *testing.T) {
	withJSON(t, false)
	out, err, rec := runCreate(t, http.StatusCreated, taskNoOptional)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := rec.body["optional_stages"]; has {
		t.Fatalf("ключ optional_stages в теле: %v", rec.body)
	}
	if strings.Contains(out, "Выбор этапов:") {
		t.Fatalf("строка этапов при пустом наборе:\n%s", out)
	}
	// Умолчания цикла видны и без --stage.
	out, err, _ = runCreate(t, http.StatusCreated, taskDesignOff)
	if err != nil || !strings.Contains(out, "Выбор этапов: design выкл, docs вкл") {
		t.Fatalf("умолчания цикла: %v\n%s", err, out)
	}
}

// CLI-3: отказ сервера печатается его текстом, второго запроса нет.
func TestCreateStageServerRefusal(t *testing.T) {
	withJSON(t, false)
	_, err, rec := runCreate(t, http.StatusBadRequest,
		`{"error":{"code":"invalid_input","message":"optional_stages: ключ done не необязательный этап, допустимы design, docs","details":{"allowed":["design","docs"]}}}`,
		"--stage", "done=off")
	if err == nil || rec.calls != 1 || !strings.Contains(err.Error(), "invalid_input") ||
		!strings.Contains(err.Error(), "допустимы design, docs") {
		t.Fatalf("ошибка %v, запросов %d", err, rec.calls)
	}
}

// CLI-4: ошибка формы — до запроса, с примером.
func TestStageFormErrors(t *testing.T) {
	withJSON(t, false)
	cases := [][]string{
		{"--stage", "design"},
		{"--stage", "design="},
		{"--stage", "=off"},
		{"--stage", " =off"},
		{"--stage", ""},
		{"--stage", "design=true"},
		{"--stage", "design=on=off"},
		{"--stage", "design=ON"},
		{"--stage", "design=on", "--stage", "design=off"},
	}
	for _, args := range cases {
		_, err, rec := runCreate(t, http.StatusCreated, taskDesignOff, args...)
		if err == nil || rec.calls != 0 {
			t.Fatalf("create %v: ошибка %v, запросов %d", args, err, rec.calls)
		}
		_, err, rec = runEdit(t, http.StatusOK, taskDesignOff, args...)
		if err == nil || rec.calls != 0 {
			t.Fatalf("edit %v: ошибка %v, запросов %d", args, err, rec.calls)
		}
	}
	// Сообщение называет форму и пример.
	var f optionalStageFlag
	for _, v := range []string{"design", "=off", "design=maybe"} {
		if err := f.Set(v); err == nil || !strings.Contains(err.Error(), "design=off") {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
}

// CLI-6: edit шлёт только optional_stages и печатает набор.
func TestEditStageBody(t *testing.T) {
	withJSON(t, false)
	out, err, rec := runEdit(t, http.StatusOK, taskDesignOn, "--stage", "design=on")
	if err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 || rec.method != "PATCH" || rec.path != "/v1/tasks/CLAB-1" ||
		!equalJSON(rec.body, map[string]any{"optional_stages": map[string]bool{"design": true}}) {
		t.Fatalf("%d %s %s %v", rec.calls, rec.method, rec.path, rec.body)
	}
	if !strings.Contains(out, "Выбор этапов: design вкл\n") {
		t.Fatalf("нет строки этапов:\n%s", out)
	}
}

// CLI-7: без флагов правки — «нечего менять» с --stage в списке.
func TestEditNothingToChangeNamesStage(t *testing.T) {
	withJSON(t, false)
	_, err, rec := runEdit(t, http.StatusOK, taskDesignOn)
	if err == nil || rec.calls != 0 || !strings.Contains(err.Error(), "нечего менять") ||
		!strings.Contains(err.Error(), "--stage") {
		t.Fatalf("ошибка %v, запросов %d", err, rec.calls)
	}
}

// CLI-8: отказы сервера на edit — его текстом, без «успешной» строки.
func TestEditStageRefusals(t *testing.T) {
	withJSON(t, false)
	cases := []struct {
		status int
		reply  string
		want   string
	}{
		{http.StatusConflict, `{"error":{"code":"conflict","message":"этап design — текущий статус задачи"}}`, "текущий статус задачи"},
		{http.StatusConflict, `{"error":{"code":"conflict","message":"задача закрыта"}}`, "задача закрыта"},
		{http.StatusForbidden, `{"error":{"code":"forbidden","message":"выбор этапов своей задачи — решение человека"}}`, "решение человека"},
	}
	for _, tc := range cases {
		out, err, rec := runEdit(t, tc.status, tc.reply, "--stage", "design=off")
		if err == nil || rec.calls != 1 || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%d: ошибка %v, запросов %d", tc.status, err, rec.calls)
		}
		if strings.Contains(out, "Выбор этапов:") || strings.Contains(out, "CLAB-1") {
			t.Fatalf("%d: напечатан успех:\n%s", tc.status, out)
		}
	}
}

// CLI-16: сервер прежней версии не должен молча терять выбор.
func TestOldServerLosesStagesLoudly(t *testing.T) {
	withJSON(t, false)
	out, err, _ := runCreate(t, http.StatusCreated, taskOldServer, "--stage", "design=off")
	if err == nil || !strings.Contains(err.Error(), "выбор этапов не записан") ||
		!strings.Contains(err.Error(), "CLAB-1") || !strings.Contains(err.Error(), "заведена") ||
		!strings.Contains(out, "CLAB-1") {
		t.Fatalf("create против старого сервера: %v\n%s", err, out)
	}
	out, err, _ = runEdit(t, http.StatusOK, taskOldServer, "--stage", "design=off", "--priority", "high")
	if err == nil || !strings.Contains(err.Error(), "выбор этапов не записан") ||
		!strings.Contains(err.Error(), "остальная правка CLAB-1 применена") || !strings.Contains(out, "Приоритет:") {
		t.Fatalf("edit против старого сервера: %v\n%s", err, out)
	}

	// Без --stage вывод прежний, без ошибки.
	out, err, _ = runCreate(t, http.StatusCreated, taskOldServer)
	if err != nil || out != "CLAB-1  Кусок  [task, new]\n" {
		t.Fatalf("create без --stage: %v %q", err, out)
	}
	out, err, _ = runEdit(t, http.StatusOK, taskOldServer, "--priority", "high")
	if err != nil || strings.Contains(out, "Выбор этапов:") {
		t.Fatalf("edit без --stage: %v\n%s", err, out)
	}
}

// CLI-15: флаг есть в справке команд и в usage.
func TestStageFlagInHelp(t *testing.T) {
	for _, c := range commands() {
		if c.name == "create" || c.name == "edit" {
			if !strings.Contains(c.usage, "--stage design=off") {
				t.Errorf("usage %s без примера --stage design=off: %s", c.name, c.usage)
			}
		}
	}
	for _, tc := range []struct {
		cmd  func(context.Context, *Client, []string) error
		args []string
	}{{cmdCreate, []string{"--help"}}, {cmdEdit, []string{"CLAB-1", "--help"}}} {
		c, _ := newInviteClient(t, http.StatusOK, taskDesignOn)
		_, stderr, err := capture(t, func() error { return tc.cmd(context.Background(), c, tc.args) })
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("--help: %v", err)
		}
		if !strings.Contains(stderr, "-stage") || !strings.Contains(stderr, "<статус>=on|off") {
			t.Errorf("в справке нет флага -stage:\n%s", stderr)
		}
	}
}

// CLI-14: --json — ответ сервера как есть.
func TestStageJSONIsServerReply(t *testing.T) {
	withJSON(t, true)
	check := func(name, out string, err error, want string) {
		t.Helper()
		var got, exp map[string]any
		if err != nil || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("%s: %v, не JSON: %q", name, err, out)
		}
		_ = json.Unmarshal([]byte(want), &exp)
		if !equalJSON(got, exp) {
			t.Fatalf("%s: JSON %v, ждали %v", name, got, exp)
		}
	}
	out, err, _ := runCreate(t, http.StatusCreated, taskDesignOff, "--stage", "design=off")
	check("create", out, err, taskDesignOff)
	out, err, _ = runEdit(t, http.StatusOK, taskDesignOn, "--stage", "design=on")
	check("edit", out, err, taskDesignOn)
}

// stagesState — ответ show и claim с выбором этапов и тремя видами переходов.
func stagesState(t *testing.T) string {
	return claimReply(t, func(r map[string]any) {
		r["task"].(map[string]any)["optional_stages"] = map[string]bool{"docs": true, "design": false}
		r["gates_ahead"] = []map[string]any{
			{"transition": "analysis_done", "kind": "advance", "to_status": "design", "lands_in": "development",
				"skips": []string{"design"}, "gates": []map[string]any{{"key": "approval", "blocking": true}}},
			{"transition": "reject_to_design", "kind": "reject", "to_status": "design", "lands_in": nil,
				"skips": []string{}, "unavailable": "stage_disabled", "gates": []map[string]any{{"key": "problem_report", "blocking": true}}},
			{"transition": "dev_done", "kind": "advance", "to_status": "testing", "lands_in": "testing",
				"skips": []string{}, "gates": []map[string]any{{"key": "artifacts", "blocking": true}}},
		}
	})
}

// CLI-13, CLI-9, CLI-11: show — строка этапов на месте и виды переходов.
func TestShowStagesLineAndTransitions(t *testing.T) {
	withJSON(t, false)
	c, _ := newInviteClient(t, http.StatusOK, stagesState(t))
	out, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	idx := func(prefix string) int {
		for i, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		return -1
	}
	ctxLine, stagesLine, transLine := idx("Контекст:"), idx("Выбор этапов: "), idx("Переходы:")
	if stagesLine != ctxLine+1 || stagesLine < 0 || transLine < stagesLine {
		t.Fatalf("строка этапов не после «Контекст»:\n%s", out)
	}
	if lines[stagesLine] != "Выбор этапов: design выкл, docs вкл" {
		t.Fatalf("строка этапов: %q", lines[stagesLine])
	}
	for _, want := range []string{
		"  analysis_done → development (пропуск: design)   гейты: approval\n",
		"  reject_to_design → недоступен: этап design выключен у задачи   гейты: problem_report\n",
		"  dev_done → testing   гейты: artifacts\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}

	// Пустой набор — как до 0066.
	empty := claimReply(t, func(r map[string]any) { r["task"].(map[string]any)["optional_stages"] = map[string]bool{} })
	c, _ = newInviteClient(t, http.StatusOK, empty)
	out, _, err = capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
	if err != nil || strings.Contains(out, "Выбор этапов:") {
		t.Fatalf("пустой набор: %v\n%s", err, out)
	}
}

// CLI-9: claim печатает тот же вид переходов.
func TestClaimStagesTransitions(t *testing.T) {
	withJSON(t, false)
	out, _, err, _ := runClaim(t, stagesState(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Выбор этапов: design выкл, docs вкл\n",
		"  analysis_done → development (пропуск: design)   гейты: approval\n",
		"  reject_to_design → недоступен: этап design выключен у задачи   гейты: problem_report\n",
		"  dev_done → testing   гейты: artifacts\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}
}

const transitionsWithSkips = `{"transitions":[` +
	`{"key":"analysis_done","name":"Анализ готов","kind":"advance","to_status":"design","lands_in":"development","skips":["design"],"allowed":true,` +
	`"gates":[{"gate_key":"artifacts","type":"artifacts_required","blocking":true,"status":"passed"}]},` +
	`{"key":"reject_to_design","name":"На дизайн","kind":"reject","to_status":"design","lands_in":null,"skips":[],"unavailable":"stage_disabled","allowed":false,` +
	`"gates":[{"gate_key":"problem_report","type":"problem_report","blocking":true,"status":"failed","reason":"нет"}]},` +
	`{"key":"dev_done","name":"Готово","kind":"advance","to_status":"testing","lands_in":"testing","skips":[],"allowed":true,"gates":[]}]}`

// CLI-10: gates — тот же вид; --transition; --json без изменений.
func TestGatesStages(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusOK, transitionsWithSkips)
	out, _, err := capture(t, func() error { return cmdGates(context.Background(), c, []string{"CLAB-1"}) })
	if err != nil || rec.path != "/v1/tasks/CLAB-1/transitions" {
		t.Fatalf("%v %s", err, rec.path)
	}
	for _, want := range []string{
		"✓ analysis_done → development (пропуск: design) (Анализ готов)\n",
		"✗ reject_to_design → недоступен: этап design выключен у задачи (На дизайн)\n    [failed] problem_report",
		"✓ dev_done → testing (Готово)\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}

	c, _ = newInviteClient(t, http.StatusOK, transitionsWithSkips)
	out, _, err = capture(t, func() error {
		return cmdGates(context.Background(), c, []string{"CLAB-1", "--transition", "reject_to_design"})
	})
	if err != nil || !strings.HasPrefix(out, "✗ reject_to_design → недоступен: этап design выключен у задачи") ||
		strings.Contains(out, "analysis_done") {
		t.Fatalf("--transition: %v\n%s", err, out)
	}

	withJSON(t, true)
	c, _ = newInviteClient(t, http.StatusOK, transitionsWithSkips)
	out, _, err = capture(t, func() error { return cmdGates(context.Background(), c, []string{"CLAB-1"}) })
	var got, want struct {
		Transitions []map[string]any `json:"transitions"`
	}
	if err != nil || json.Unmarshal([]byte(out), &got.Transitions) != nil {
		t.Fatalf("--json: %v\n%s", err, out)
	}
	_ = json.Unmarshal([]byte(transitionsWithSkips), &want)
	if !equalJSON(got.Transitions, want.Transitions) {
		t.Fatalf("--json: %v, ждали %v", got.Transitions, want.Transitions)
	}
}

// CLI-11, CLI-16: переход без пропуска и ответ сервера прежней версии —
// байт в байт прежний вид.
func TestGatesUnchangedWithoutSkips(t *testing.T) {
	withJSON(t, false)
	plain := `{"transitions":[{"key":"dev_done","name":"Готово","to_status":"testing","allowed":true,` +
		`"gates":[{"gate_key":"artifacts","type":"artifacts_required","blocking":true,"status":"passed"}]}]}`
	c, _ := newInviteClient(t, http.StatusOK, plain)
	out, _, err := capture(t, func() error { return cmdGates(context.Background(), c, []string{"CLAB-1"}) })
	want := "✓ dev_done → testing (Готово)\n    [passed] artifacts (artifacts_required)\n"
	if err != nil || out != want {
		t.Fatalf("%v\n%q\nждали %q", err, out, want)
	}
}

// CLI-12: transition печатает пропущенное после строки статуса.
func TestTransitionPrintsSkips(t *testing.T) {
	withJSON(t, false)
	run := func(reply string) string {
		t.Helper()
		c, rec := newInviteClient(t, http.StatusOK, reply)
		out, _, err := capture(t, func() error {
			return cmdTransition(context.Background(), c, []string{"CLAB-1", "--to", "analysis_done"})
		})
		if err != nil || rec.path != "/v1/tasks/CLAB-1/transitions/analysis_done" {
			t.Fatalf("%v %s", err, rec.path)
		}
		return out
	}
	// Прежний статус — previous_run, как отдаёт REST; разбор имён — TestTransitionFromRunName.
	base := `"task":{"key":"CLAB-1","status_key":"development"},"previous_run":{"status_key":"analysis"},"blocked":false,"gates":[]`
	if out := run(`{` + base + `,"skips":["design","docs"]}`); out != "CLAB-1: analysis → development\nпропущено: design, docs\n" {
		t.Fatalf("с пропуском: %q", out)
	}
	for _, reply := range []string{`{` + base + `,"skips":[]}`, `{` + base + `}`} {
		if out := run(reply); out != "CLAB-1: analysis → development\n" {
			t.Fatalf("без пропуска: %q", out)
		}
	}
	// Заблокированный переход ничего не пропустил.
	blocked := `{"task":{"key":"CLAB-1","status_key":"analysis"},"blocked":true,"skips":["design"],"gates":[]}`
	if out := run(blocked); strings.Contains(out, "пропущено") {
		t.Fatalf("заблокированный: %q", out)
	}
}

// CLAB-330: прежний статус берётся из previous_run (имя REST), а from_stage_run
// читается запасным вариантом; ни одного поля — прочерк без ошибки.
func TestTransitionFromRunName(t *testing.T) {
	withJSON(t, false)
	task := `"task":{"key":"CLAB-1","status_key":"development"},"blocked":false,"gates":[]`
	cases := []struct {
		name, reply, want string
	}{
		{"только previous_run", `{` + task + `,"previous_run":{"status_key":"analysis"}}`, "CLAB-1: analysis → development\n"},
		{"только from_stage_run", `{` + task + `,"from_stage_run":{"status_key":"analysis"}}`, "CLAB-1: analysis → development\n"},
		{"оба: берётся previous_run", `{` + task + `,"previous_run":{"status_key":"analysis"},"from_stage_run":{"status_key":"planning"}}`, "CLAB-1: analysis → development\n"},
		{"previous_run пуст: берётся from_stage_run", `{` + task + `,"previous_run":{"status_key":""},"from_stage_run":{"status_key":"analysis"}}`, "CLAB-1: analysis → development\n"},
		{"ни одного: прочерк", `{` + task + `}`, "CLAB-1: — → development\n"},
		{"с пропуском по previous_run", `{` + task + `,"previous_run":{"status_key":"analysis"},"skips":["design","docs"]}`, "CLAB-1: analysis → development\nпропущено: design, docs\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newInviteClient(t, http.StatusOK, tc.reply)
			out, _, err := capture(t, func() error {
				return cmdTransition(context.Background(), c, []string{"CLAB-1", "--to", "analysis_done"})
			})
			if err != nil || rec.path != "/v1/tasks/CLAB-1/transitions/analysis_done" {
				t.Fatalf("%v %s", err, rec.path)
			}
			if out != tc.want {
				t.Fatalf("вывод %q, нужен %q", out, tc.want)
			}
		})
	}
}

// CLI-14: --json у show, claim и transition — ответ сервера как есть.
func TestStagesJSONForReadCommands(t *testing.T) {
	withJSON(t, true)
	state := stagesState(t)
	c, _ := newInviteClient(t, http.StatusOK, state)
	out, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
	if err != nil || !jsonEqualStrings(t, out, state) {
		t.Fatalf("show --json: %v\n%s", err, out)
	}
	out, _, err, _ = runClaim(t, state)
	if err != nil || !jsonEqualStrings(t, out, state) {
		t.Fatalf("claim --json: %v\n%s", err, out)
	}
	tr := `{"task":{"key":"CLAB-1","status_key":"development"},"skips":["design"],"blocked":false,"gates":[]}`
	c, _ = newInviteClient(t, http.StatusOK, tr)
	out, _, err = capture(t, func() error {
		return cmdTransition(context.Background(), c, []string{"CLAB-1", "--to", "analysis_done"})
	})
	if err != nil || !jsonEqualStrings(t, out, tr) {
		t.Fatalf("transition --json: %v\n%s", err, out)
	}
}

func jsonEqualStrings(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return equalJSON(x, y)
}

// CLI-16: show и claim на ответе прежнего вида (без optional_stages, lands_in,
// skips, unavailable) — вывод прежний.
func TestReadCommandsOldServerShape(t *testing.T) {
	withJSON(t, false)
	out, _, err, _ := runClaim(t, claimReply(t, nil))
	if err != nil || strings.Contains(out, "Выбор этапов:") || strings.Contains(out, "пропуск") || strings.Contains(out, "недоступен") ||
		!strings.Contains(out, "  dev_done → testing   гейты: artifacts\n") {
		t.Fatalf("claim: %v\n%s", err, out)
	}
}

// CLI-17: CLI ходит только по существующим путям.
func TestStageCommandsUseExistingPaths(t *testing.T) {
	withJSON(t, false)
	var mu sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions"):
			_, _ = w.Write([]byte(transitionsWithSkips))
		case strings.HasSuffix(r.URL.Path, "/state"), strings.HasSuffix(r.URL.Path, "/claim"):
			_, _ = w.Write([]byte(stagesState(t)))
		case strings.Contains(r.URL.Path, "/transitions/"):
			_, _ = w.Write([]byte(`{"task":{"key":"CLAB-1","status_key":"development"},"skips":["design"],"gates":[]}`))
		default:
			_, _ = w.Write([]byte(taskDesignOff))
		}
	}))
	defer srv.Close()
	c := &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}
	ctx := context.Background()
	for _, run := range []func() error{
		func() error {
			return cmdCreate(ctx, c, []string{"--project", "CLAB", "--title", "t", "--stage", "design=off"})
		},
		func() error { return cmdEdit(ctx, c, []string{"CLAB-1", "--stage", "design=on"}) },
		func() error { return cmdShow(ctx, c, []string{"CLAB-1"}) },
		func() error { return cmdClaim(ctx, c, []string{"CLAB-1"}) },
		func() error { return cmdGates(ctx, c, []string{"CLAB-1"}) },
		func() error { return cmdTransition(ctx, c, []string{"CLAB-1", "--to", "analysis_done"}) },
	} {
		if _, _, err := capture(t, run); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]bool{
		"POST /v1/projects/CLAB/tasks":                    true,
		"PATCH /v1/tasks/CLAB-1":                          true,
		"GET /v1/tasks/CLAB-1/state":                      true,
		"POST /v1/tasks/CLAB-1/claim":                     true,
		"GET /v1/tasks/CLAB-1/transitions":                true,
		"POST /v1/tasks/CLAB-1/transitions/analysis_done": true,
	}
	for path := range seen {
		if !want[path] {
			t.Errorf("лишний запрос %s", path)
		}
	}
	for path := range want {
		if !seen[path] {
			t.Errorf("не было запроса %s", path)
		}
	}
}
