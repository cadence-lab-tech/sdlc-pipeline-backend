package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-260: уровень модели в clab (решение 0060) — флаги записи, строки
// сводки claim и show, сервер прежней версии.

// editedTaskLevels — ответ сервера с уровнями: ключ model_level есть всегда.
const editedTaskLevels = `{"key":"CLAB-58","seq":58,"title":"Новый заголовок","status_key":"development",` +
	`"priority":"high","labels":["bug"],"properties":{"type":"task"},"model_level":"medium",` +
	`"stage_settings":{"testing":{"model_level":"low"},"development":{"model_level":"high"}},` +
	`"created_at":"2026-09-12T10:00:00Z","updated_at":"2026-09-12T12:00:00Z"}`

const editedTaskNoLevels = `{"key":"CLAB-58","seq":58,"title":"Новый заголовок","status_key":"development",` +
	`"priority":"high","labels":["bug"],"properties":{"type":"task"},"model_level":null,"stage_settings":{},` +
	`"created_at":"2026-09-12T10:00:00Z","updated_at":"2026-09-12T12:00:00Z"}`

// AC-1, AC-2: поле в теле есть ровно тогда, когда передан флаг; пустое
// значение уходит "" и снимает уровень.
func TestEditModelLevelBody(t *testing.T) {
	withJSON(t, false)
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"--model-level", "high"}, map[string]any{"model_level": "high"}},
		{[]string{"--model-level", ""}, map[string]any{"model_level": ""}},
		{[]string{"--model-level-for", "development=low", "--model-level-for", "testing=medium"},
			map[string]any{"stage_settings": map[string]any{
				"development": map[string]any{"model_level": "low"},
				"testing":     map[string]any{"model_level": "medium"}}}},
		{[]string{"--model-level-for", "development="},
			map[string]any{"stage_settings": map[string]any{"development": map[string]any{"model_level": ""}}}},
		{[]string{"--model-level", "medium", "--model-level-for", "development=high", "--priority", "high"},
			map[string]any{"model_level": "medium", "priority": "high",
				"stage_settings": map[string]any{"development": map[string]any{"model_level": "high"}}}},
		// Без флагов уровня полей нет вовсе: прочая правка уровни не трогает.
		{[]string{"--priority", "low"}, map[string]any{"priority": "low"}},
	}
	for _, tc := range cases {
		c, rec := newInviteClient(t, http.StatusOK, editedTaskLevels)
		stdout, _, err := capture(t, func() error {
			return cmdEdit(context.Background(), c, append([]string{"CLAB-58"}, tc.args...))
		})
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if rec.calls != 1 || rec.method != "PATCH" || rec.path != "/v1/tasks/CLAB-58" {
			t.Fatalf("%v: %d запросов, %s %s", tc.args, rec.calls, rec.method, rec.path)
		}
		if !equalJSON(rec.body, tc.want) {
			t.Fatalf("%v: тело %v, ждали %v", tc.args, rec.body, tc.want)
		}
		// Правку видно: уровни после приоритета и трекера, этапы по ключу.
		if !strings.Contains(stdout, "Уровень: medium   Этапы: development=high, testing=low\n") {
			t.Fatalf("%v: нет строки уровней:\n%s", tc.args, stdout)
		}
	}

	c, _ := newInviteClient(t, http.StatusOK, editedTaskNoLevels)
	stdout, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--model-level", ""})
	})
	if err != nil || !strings.Contains(stdout, "Уровень: —   Этапы: —\n") {
		t.Fatalf("снятые уровни: %v\n%s", err, stdout)
	}
}

// AC-3: ошибка формы --model-level-for — до запроса, код ненулевой.
func TestEditModelLevelForFormErrors(t *testing.T) {
	withJSON(t, false)
	cases := [][]string{
		{"--model-level-for", "development"},
		{"--model-level-for", "=high"},
		{"--model-level-for", " =high"},
		{"--model-level-for", ""},
		{"--model-level-for", "development=high", "--model-level-for", "development=low"},
	}
	for _, args := range cases {
		c, rec := newInviteClient(t, http.StatusOK, editedTaskLevels)
		_, stderr, err := capture(t, func() error {
			return cmdEdit(context.Background(), c, append([]string{"CLAB-58"}, args...))
		})
		if err == nil || rec.calls != 0 {
			t.Fatalf("%v: ошибка %v, запросов %d", args, err, rec.calls)
		}
		if !strings.Contains(stderr+err.Error(), "<статус>=<уровень>") && !strings.Contains(err.Error(), "дважды") {
			t.Fatalf("%v: в ошибке нет формы: %v\n%s", args, err, stderr)
		}
	}
}

// AC-3: значения и ключи проверяет сервер; его 400 и 403 CLI печатает как
// есть, с перечнем допустимых.
func TestEditModelLevelServerRefusal(t *testing.T) {
	withJSON(t, false)
	cases := []struct {
		status int
		reply  string
		want   string
	}{
		{http.StatusBadRequest,
			`{"error":{"code":"invalid_input","message":"уровень модели задачи: уровень модели \"ultra\" не поддерживается, допустимы low, medium, high","details":{"allowed":["low","medium","high"]}}}`,
			"допустимы low, medium, high"},
		{http.StatusBadRequest,
			`{"error":{"code":"invalid_input","message":"stage_settings: ключи done не статусы цикла задачи с ролью-исполнителем, допустимы analysis, development","details":{"invalid_keys":["done"]}}}`,
			"ключи done не статусы цикла"},
		{http.StatusForbidden,
			`{"error":{"code":"forbidden","message":"токен задачи пишет только в свою задачу и её детей"}}`,
			"токен задачи пишет только в свою задачу"},
	}
	for _, tc := range cases {
		c, rec := newInviteClient(t, tc.status, tc.reply)
		_, _, err := capture(t, func() error {
			return cmdEdit(context.Background(), c, []string{"CLAB-58", "--model-level", "ultra", "--model-level-for", "done=high"})
		})
		if err == nil || rec.calls != 1 || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%d: ошибка %v, запросов %d", tc.status, err, rec.calls)
		}
	}
}

// AC-3: «нечего менять» называет и новые флаги.
func TestEditNothingToChangeNamesLevelFlags(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusOK, editedTaskLevels)
	_, _, err := capture(t, func() error { return cmdEdit(context.Background(), c, []string{"CLAB-58"}) })
	if err == nil || rec.calls != 0 {
		t.Fatalf("ошибка %v, запросов %d", err, rec.calls)
	}
	for _, want := range []string{"нечего менять", "--model-level", "--model-level-for"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("в %q нет %q", err, want)
		}
	}
}

// AC-4: create кладёт уровни в тело заведения и печатает их.
func TestCreateModelLevels(t *testing.T) {
	withJSON(t, false)
	reply := `{"key":"CLAB-300","title":"Кусок","status_key":"new","properties":{"type":"task"},` +
		`"model_level":"medium","stage_settings":{"testing":{"model_level":"low"}}}`
	c, rec := newInviteClient(t, http.StatusCreated, reply)
	stdout, _, err := capture(t, func() error {
		return cmdCreate(context.Background(), c, []string{"--project", "CLAB", "--title", "Кусок",
			"--parent", "CLAB-230", "--model-level", "medium", "--model-level-for", "testing=low"})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"title": "Кусок", "parent_key": "CLAB-230", "model_level": "medium",
		"stage_settings": map[string]any{"testing": map[string]any{"model_level": "low"}}}
	if rec.method != "POST" || rec.path != "/v1/projects/CLAB/tasks" || !equalJSON(rec.body, want) {
		t.Fatalf("%s %s %v", rec.method, rec.path, rec.body)
	}
	if !strings.Contains(stdout, "CLAB-300  Кусок") || !strings.Contains(stdout, "Уровень: medium   Этапы: testing=low") {
		t.Fatalf("вывод create:\n%s", stdout)
	}

	// Без флагов уровня — ни полей, ни строки: вывод как раньше.
	c, rec = newInviteClient(t, http.StatusCreated, reply)
	stdout, _, err = capture(t, func() error {
		return cmdCreate(context.Background(), c, []string{"--project", "CLAB", "--title", "Кусок"})
	})
	if err != nil || !equalJSON(rec.body, map[string]any{"title": "Кусок"}) || strings.Contains(stdout, "Уровень:") {
		t.Fatalf("create без уровней: %v %v\n%s", err, rec.body, stdout)
	}

	// Неверный ключ этапа — 400 сервера, задача не заведена.
	c, _ = newInviteClient(t, http.StatusBadRequest,
		`{"error":{"code":"invalid_input","message":"stage_settings: ключи done не статусы цикла задачи с ролью-исполнителем"}}`)
	_, _, err = capture(t, func() error {
		return cmdCreate(context.Background(), c, []string{"--project", "CLAB", "--title", "Кусок", "--model-level-for", "done=low"})
	})
	if err == nil || !strings.Contains(err.Error(), "ключи done") {
		t.Fatalf("400 на create: %v", err)
	}
}

// AC-6: сервер прежней версии молча выбрасывает уровень — clab говорит об
// этом ошибкой, а без флагов уровня работает как раньше.
func TestOldServerLosesLevelsLoudly(t *testing.T) {
	withJSON(t, false)
	// editedTask — ответ сервера до 0060: ключа model_level нет.
	c, rec := newInviteClient(t, http.StatusOK, editedTask)
	stdout, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--model-level", "high", "--priority", "high"})
	})
	if err == nil || !strings.Contains(err.Error(), "уровень модели не записан") ||
		!strings.Contains(err.Error(), "остальная правка CLAB-58 применена") {
		t.Fatalf("edit против старого сервера: %v", err)
	}
	if rec.calls != 1 || !strings.Contains(stdout, "Приоритет: high") || strings.Contains(stdout, "Уровень:") {
		t.Fatalf("правка прочих полей не показана или показан уровень:\n%s", stdout)
	}

	c, _ = newInviteClient(t, http.StatusOK, editedTask)
	if _, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--model-level-for", "development=high"})
	}); err == nil || !strings.Contains(err.Error(), "уровень модели не записан") {
		t.Fatalf("edit --model-level-for против старого сервера: %v", err)
	}

	created := `{"key":"CLAB-301","title":"Кусок","status_key":"new","properties":{"type":"task"}}`
	c, _ = newInviteClient(t, http.StatusCreated, created)
	stdout, _, err = capture(t, func() error {
		return cmdCreate(context.Background(), c, []string{"--project", "CLAB", "--title", "Кусок", "--model-level", "high"})
	})
	if err == nil || !strings.Contains(err.Error(), "уровень модели не записан") ||
		!strings.Contains(err.Error(), "задача CLAB-301 заведена без уровней") || !strings.Contains(stdout, "CLAB-301") {
		t.Fatalf("create против старого сервера: %v\n%s", err, stdout)
	}

	// Без флагов уровня — как сегодня.
	c, _ = newInviteClient(t, http.StatusOK, editedTask)
	stdout, _, err = capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--priority", "high"})
	})
	if err != nil || strings.Contains(stdout, "Уровень:") {
		t.Fatalf("edit без уровней против старого сервера: %v\n%s", err, stdout)
	}
	c, _ = newInviteClient(t, http.StatusCreated, created)
	if _, _, err := capture(t, func() error {
		return cmdCreate(context.Background(), c, []string{"--project", "CLAB", "--title", "Кусок"})
	}); err != nil {
		t.Fatalf("create без уровней против старого сервера: %v", err)
	}
}

// AC-8: --json — ответ сервера как есть, и ошибка старого сервера та же.
func TestModelLevelJSONIsServerReply(t *testing.T) {
	withJSON(t, true)
	c, _ := newInviteClient(t, http.StatusOK, editedTaskLevels)
	stdout, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--model-level", "medium"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("не JSON: %s", stdout)
	}
	_ = json.Unmarshal([]byte(editedTaskLevels), &want)
	if !equalJSON(got, want) {
		t.Fatalf("JSON %v, ждали %v", got, want)
	}

	c, _ = newInviteClient(t, http.StatusOK, editedTask)
	if _, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--model-level", "medium"})
	}); err == nil {
		t.Fatal("--json не отменяет ошибку старого сервера")
	}

	reply := claimReply(t, func(r map[string]any) {
		r["execution"] = map[string]any{"model_level": "high", "model_level_source": "stage"}
		task := r["task"].(map[string]any)
		task["model_level"] = "medium"
		task["stage_settings"] = map[string]any{"development": map[string]any{"model_level": "high"}}
	})
	stdout, _, err, _ = runClaim(t, reply)
	if err != nil {
		t.Fatal(err)
	}
	var claim map[string]any
	if err := json.Unmarshal([]byte(stdout), &claim); err != nil {
		t.Fatalf("claim --json не JSON: %s", stdout[:min(len(stdout), 300)])
	}
	var wantClaim map[string]any
	_ = json.Unmarshal([]byte(reply), &wantClaim)
	if !equalJSON(claim, wantClaim) {
		t.Fatal("claim --json не совпадает с ответом сервера")
	}
}

// AC-5: role put кладёт model_level в роль связки; без флага поля нет.
func TestRolePutModelLevel(t *testing.T) {
	withJSON(t, false)
	file := filepath.Join(t.TempDir(), "dev.md")
	if err := os.WriteFile(file, []byte("Ты разработчик."), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args  []string
		level any
	}{
		{[]string{"--model-level", "medium"}, "medium"},
		{nil, nil},
		{[]string{"--model-level", ""}, nil},
	} {
		c, rec := newInviteClient(t, http.StatusOK, `{"roles":[{"key":"developer","action":"new_version"}]}`)
		args := append([]string{"put", "--project", "CLAB", "--key", "developer", "--file", file}, tc.args...)
		if _, _, err := capture(t, func() error { return cmdRole(context.Background(), c, args) }); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if rec.method != "POST" || rec.path != "/v1/projects/CLAB/import" {
			t.Fatalf("%s %s", rec.method, rec.path)
		}
		roles := rec.body["roles"].([]any)
		role := roles[0].(map[string]any)
		if got, ok := role["model_level"]; got != tc.level || (tc.level == nil && ok) {
			t.Fatalf("%v: model_level=%v (есть: %v)", tc.args, got, ok)
		}
	}
}

// AC-7, AC-8: строки уровня в сводке claim и show.
func TestSummaryModelLevelLines(t *testing.T) {
	withJSON(t, false)
	medium := "medium"
	cases := []struct {
		execution   map[string]any
		taskLevel   any
		stages      any
		recommended string
		levels      string
	}{
		{map[string]any{"model_level": "high", "model_level_source": "stage"}, medium,
			map[string]any{"testing": map[string]any{"model_level": "low"}, "development": map[string]any{"model_level": "high"}},
			"Рекомендованный уровень: high (этап задачи)", "Уровни задачи: задача medium; этапы development=high, testing=low"},
		{map[string]any{"model_level": "medium", "model_level_source": "task"}, medium, map[string]any{},
			"Рекомендованный уровень: medium (задача)", "Уровни задачи: задача medium; этапы —"},
		{map[string]any{"model_level": "low", "model_level_source": "role"}, nil, map[string]any{},
			"Рекомендованный уровень: low (роль)", "Уровни задачи: не заданы"},
		{map[string]any{"model_level": nil, "model_level_source": "none"}, nil, nil,
			"Рекомендованный уровень: не задан", "Уровни задачи: не заданы"},
		// Снимок прогона и текущие уровни задачи расходятся — печатаются как есть.
		{map[string]any{"model_level": "high", "model_level_source": "task"}, nil,
			map[string]any{"testing": map[string]any{"model_level": "low"}},
			"Рекомендованный уровень: high (задача)", "Уровни задачи: задача —; этапы testing=low"},
	}
	base := func(r map[string]any) {}
	plain, _, err, _ := runClaim(t, claimReply(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "Рекомендованный уровень") || strings.Contains(plain, "Уровни задачи") {
		t.Fatalf("сервер без execution, а строки уровня есть:\n%s", plain[:500])
	}
	for _, tc := range cases {
		reply := claimReply(t, func(r map[string]any) {
			r["execution"] = tc.execution
			task := r["task"].(map[string]any)
			task["model_level"] = tc.taskLevel
			task["stage_settings"] = tc.stages
		})
		out, _, err, _ := runClaim(t, reply)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(out, "\n")
		at := -1
		for i, l := range lines {
			if strings.HasPrefix(l, "Прогон:") {
				at = i
			}
		}
		if at < 0 || lines[at+1] != tc.recommended || lines[at+2] != tc.levels {
			t.Fatalf("%v: после «Прогон» ждали\n%s\n%s\nа вывод:\n%s", tc.execution, tc.recommended, tc.levels,
				strings.Join(lines[:min(len(lines), 14)], "\n"))
		}
		// Остальная сводка — байт в байт как у сервера без execution.
		rest := strings.Join(append(append([]string{}, lines[:at+1]...), lines[at+3:]...), "\n")
		if rest != plain {
			t.Fatalf("%v: строки уровня сдвинули остальной вывод", tc.execution)
		}

		// show — та же сводка из GET …/state.
		c, rec := newInviteClient(t, http.StatusOK, reply)
		shown, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
		if err != nil || rec.path != "/v1/tasks/CLAB-146/state" {
			t.Fatalf("show: %v %s", err, rec.path)
		}
		if !strings.Contains(shown, "\n"+tc.recommended+"\n"+tc.levels+"\n") {
			t.Fatalf("show без строк уровня:\n%s", shown)
		}
	}
}

// AC-14: справка называет новые флаги и правило «пустое снимает».
func TestModelLevelFlagsInHelp(t *testing.T) {
	usage := map[string]string{}
	for _, c := range commands() {
		usage[c.name] = c.usage
	}
	for name, wants := range map[string][]string{
		"create": {"--model-level", "--model-level-for"},
		"edit":   {"--model-level", "--model-level-for", "снимает"},
		"role":   {"--model-level", "без --model-level у неё уровня нет"},
	} {
		for _, want := range wants {
			if !strings.Contains(usage[name], want) {
				t.Errorf("справка %s без %q: %s", name, want, usage[name])
			}
		}
	}
	if !strings.Contains(roleLevelUsage, "теряет молча") || !strings.Contains(roleLevelUsage, "Без флага") {
		t.Errorf("описание флага role put: %s", roleLevelUsage)
	}
}
