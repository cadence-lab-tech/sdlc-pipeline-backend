package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// CLAB-146: clab claim печатает то, что агенту без MCP нужно для этапа, а
// clab whoami отвечает и без проекта.

const reviewArtifactID = "7d0a6c0e-0000-4000-8000-0000000000aa"

// longInstructions — инструкция длиннее 4000 символов с переводами строк:
// её нельзя ни усекать, ни склеивать в одну строку.
var longInstructions = func() string {
	var b strings.Builder
	b.WriteString("Ты разработчик.\n\nРеализация:\n")
	for i := 0; b.Len() < 9000; i++ {
		b.WriteString("- пункт инструкции, который нельзя потерять; строка с `кодом` и «кавычками»\n")
	}
	b.WriteString("Последняя строка инструкции.")
	return b.String()
}()

// claimReply — ответ захвата в форме workflow.ClaimResult.
func claimReply(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	reply := map[string]any{
		"stage_run_id": "5ca900b3-57f1-4df7-bd43-e69b594d53d4",
		"kind":         "stage",
		"task": map[string]any{
			"key": "CLAB-146", "title": "clab claim не печатает инструкцию роли",
			"status": "development", "status_name": "Development", "attempt": 2,
			"priority": "normal", "labels": []string{"bug"}, "type": "task", "parent": "CLAB-109",
			"external_ref": nil, "properties": map[string]any{"type": "task"},
		},
		"role": map[string]any{
			"key": "developer", "name": "Разработчик", "version": 13,
			"instructions": longInstructions, "input_kinds": []string{"analysis.requirements"},
			"output_contract": []map[string]any{
				{"kind": "dev.change_summary", "title": "Разбор изменений", "format": "markdown", "required": true},
				{"kind": "dev.merge_report", "title": "Отчёт о слиянии", "format": "markdown", "required": false},
			},
			"tool_policy": map[string]any{},
		},
		"context": map[string]any{
			"repositories": []map[string]any{{"owner": "cadence-lab-tech", "name": "sdlc-pipeline-backend",
				"default_branch": "main", "clone_url": "https://github.com/cadence-lab-tech/sdlc-pipeline-backend.git", "primary": true}},
			"project_documents": []any{},
			"prior_artifacts": []map[string]any{
				{"id": "30e8471f-3bac-4182-93dc-804b6942b42c", "kind": "analysis.requirements",
					"title": "Бизнес-требования и сценарии", "format": "markdown", "stage": "analysis", "status": "submitted", "version": 1},
				{"id": "6f00b1de-0000-4000-8000-000000000002", "kind": "problem.report",
					"title": "Тесты падают на пустом списке", "format": "markdown", "stage": "testing", "status": "submitted", "version": 3},
			},
			"rejection": map[string]any{
				"from_stage": "testing",
				"reason": "Критерий 3 не выполнен: при пустом списке входов вывод молчит. " +
					strings.Repeat("Подробности причины возврата, которые нельзя обрезать. ", 6) + "КОНЕЦ ПРИЧИНЫ",
				"problem_report_artifact_id": "6f00b1de-0000-4000-8000-000000000002",
			},
			"baselines": []any{}, "drift": []any{}, "repo_refs": []any{},
		},
		"gates_ahead": []map[string]any{{
			"transition": "dev_done", "name": "Реализация готова", "kind": "advance", "to_status": "testing",
			"gates": []map[string]any{{"key": "artifacts", "name": "Сданы", "type": "artifacts_required", "config": map[string]any{}, "blocking": true}},
		}},
		"lease":        map[string]any{"stage_run_id": "5ca900b3-57f1-4df7-bd43-e69b594d53d4", "expires_at": "2026-10-20T16:38:43Z", "heartbeat_interval_seconds": 300},
		"usage_report": "expected",
	}
	if mutate != nil {
		mutate(reply)
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func runClaim(t *testing.T, reply string, args ...string) (stdout, stderr string, err error, srv *inviteServer) {
	t.Helper()
	c, rec := newInviteClient(t, http.StatusOK, reply)
	out, errOut, err := capture(t, func() error {
		return cmdClaim(context.Background(), c, append([]string{"CLAB-146"}, args...))
	})
	if rec.method != http.MethodPost || rec.path != "/v1/tasks/CLAB-146/claim" {
		t.Fatalf("запрос %s %s", rec.method, rec.path)
	}
	return out, errOut, err, rec
}

// Критерии 1–5: инструкция целиком, контракт, входы, возврат целиком,
// прежняя сводка на месте.
func TestClaimPrintsInstructionsContractAndInputs(t *testing.T) {
	withJSON(t, false)
	out, _, err, _ := runClaim(t, claimReply(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(longInstructions) <= 4000 {
		t.Fatal("фикстура инструкции короче 4000 символов")
	}
	if !strings.Contains(out, "Инструкция роли developer v13:\n"+longInstructions+"\n") {
		t.Fatalf("инструкции нет целиком:\n%s", tail(out))
	}
	for _, want := range []string{
		// Сводка, как раньше (критерий 5).
		"CLAB-146  clab claim не печатает инструкцию роли",
		"development (Development), попытка 2",
		"developer (Разработчик) v13",
		"5ca900b3 stage, аренда до",
		"cadence-lab-tech/sdlc-pipeline-backend (основной)",
		"dev_done → testing   гейты: artifacts",
		// Контракт (критерий 2).
		"Сдать:",
		"dev.change_summary", "Разбор изменений",
		"dev.merge_report", "Отчёт о слиянии",
		// Входы (критерий 3).
		"Входные артефакты:",
		"analysis.requirements", "Бизнес-требования и сценарии", "30e8471f-3bac-4182-93dc-804b6942b42c",
		"problem.report", "v3", "testing", "6f00b1de-0000-4000-8000-000000000002",
		// Возврат (критерий 4).
		"Возврат с этапа testing:",
		"КОНЕЦ ПРИЧИНЫ",
		"описание проблемы: 6f00b1de-0000-4000-8000-000000000002",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q", want)
		}
	}
	contract := lineWith(out, "dev.change_summary")
	optional := lineWith(out, "dev.merge_report")
	if !strings.Contains(contract, "да") || !strings.Contains(optional, "нет") {
		t.Errorf("обязательность не видна: %q / %q", contract, optional)
	}
	// Усечённая строка причины из show в claim не дублируется.
	if strings.Contains(out, "…") {
		t.Errorf("в claim что-то усечено:\n%s", out[:min(len(out), 3000)])
	}
}

// Критерии 3–4: пустые входы — явное «нет»; перепроверка — чей результат и
// какие артефакты.
func TestClaimEmptyInputsAndReview(t *testing.T) {
	withJSON(t, false)
	reply := claimReply(t, func(r map[string]any) {
		r["kind"] = "review"
		ctx := r["context"].(map[string]any)
		ctx["prior_artifacts"] = []any{}
		delete(ctx, "rejection")
		ctx["review"] = map[string]any{
			"parent_run_id": "11111111-0000-4000-8000-000000000001", "gate": "code_review",
			"transition": "dev_done", "subject_role": "developer",
			"subject_artifacts": []map[string]any{{"id": reviewArtifactID, "kind": "dev.change_summary",
				"title": "Разбор", "format": "markdown", "status": "submitted", "version": 2}},
		}
	})
	out, _, err, _ := runClaim(t, reply)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Входные артефакты: нет",
		"Перепроверка: гейт code_review перехода dev_done, проверяется результат роли developer:",
		"dev.change_summary", "v2", reviewArtifactID,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out[:min(len(out), 2000)])
		}
	}
	if strings.Contains(out, "Возврат") {
		t.Error("возврата нет, а секция напечатана")
	}
}

// Критерий 6: --instructions-file пишет инструкцию байт в байт, в выводе —
// путь и размер вместо текста.
func TestClaimInstructionsFile(t *testing.T) {
	withJSON(t, false)
	path := filepath.Join(t.TempDir(), "role.md")
	out, _, err, _ := runClaim(t, claimReply(t, nil), "--instructions-file", path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != longInstructions {
		t.Fatal("файл инструкции не совпадает с role.instructions")
	}
	want := "Инструкция роли developer v13: " + path + " (" + itoa(utf8.RuneCountInString(longInstructions)) + " симв.)"
	if !strings.Contains(out, want) {
		t.Errorf("нет строки %q:\n%s", want, tail(out))
	}
	if strings.Contains(out, "Последняя строка инструкции.") {
		t.Error("с файлом инструкция всё равно напечатана")
	}
	if !strings.Contains(out, "Сдать:") || !strings.Contains(out, "Входные артефакты:") {
		t.Error("с файлом пропали остальные секции")
	}
}

// Критерий 7: файл не записался — ошибка с путём, но сводка с прогоном уже
// напечатана: аренда взята, агенту нужен id, чтобы отпустить.
func TestClaimInstructionsFileFailure(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		withJSON(t, asJSON)
		path := filepath.Join(t.TempDir(), "нет-каталога", "role.md")
		out, _, err, _ := runClaim(t, claimReply(t, nil), "--instructions-file", path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("json=%v: ошибка без пути: %v", asJSON, err)
		}
		if !strings.Contains(out, "5ca900b3") {
			t.Errorf("json=%v: до ошибки не напечатан прогон:\n%s", asJSON, out[:min(len(out), 500)])
		}
	}
}

// Критерии 6 и 8: --json — ответ сервера без изменений, с файлом и без.
func TestClaimJSONPassThrough(t *testing.T) {
	withJSON(t, true)
	reply := claimReply(t, nil)
	var want any
	if err := json.Unmarshal([]byte(reply), &want); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--instructions-file", filepath.Join(t.TempDir(), "role.md")}} {
		out, _, err, _ := runClaim(t, reply, args...)
		if err != nil {
			t.Fatal(err)
		}
		var got any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("--json %v: не JSON: %v", args, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("--json %v изменил ответ сервера", args)
		}
		if len(args) > 0 {
			if got, err := os.ReadFile(args[1]); err != nil || string(got) != longInstructions {
				t.Fatalf("--json с файлом: файл %v", err)
			}
		}
	}
}

// Критерий 9: флаг виден в --help и в usage команды.
func TestClaimHelpShowsInstructionsFile(t *testing.T) {
	withJSON(t, false)
	_, errOut, _ := capture(t, func() error {
		return cmdClaim(context.Background(), &Client{}, []string{"--help"})
	})
	if !strings.Contains(errOut, "-instructions-file") {
		t.Fatalf("--help без флага:\n%s", errOut)
	}
	for _, c := range commands() {
		if c.name == "claim" && !strings.Contains(c.usage, "--instructions-file") {
			t.Fatalf("usage claim: %q", c.usage)
		}
	}
}

// showGolden — вывод clab show на фикстуре claimReply, снятый с main до
// CLAB-146 (кроме строки аренды, которая зависит от часового пояса).
const showGolden = `CLAB-146  clab claim не печатает инструкцию роли
Статус:       development (Development), попытка 2, тип task, приоритет normal
Эпик:         CLAB-109
Роль:         developer (Разработчик) v13
Прогон:       5ca900b3 stage, аренда до <LEASE>, heartbeat каждые 300 с
Репозитории:  cadence-lab-tech/sdlc-pipeline-backend (основной)
Контекст:     артефактов ранее 2, сигналов дрейфа 0
Возврат:      Критерий 3 не выполнен: при пустом списке входов вывод молчит. Подробности причины возврата, которые нельзя обрезать. П…
Переходы:
  dev_done → testing   гейты: artifacts
`

// Критерий 10: clab show печатает ровно то же, что до изменения.
func TestShowUnchanged(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusOK, claimReply(t, nil))
	out, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/v1/tasks/CLAB-146/state" {
		t.Fatalf("запрос %s", rec.path)
	}
	lease := lineWith(out, "Прогон:")
	start := strings.Index(lease, "аренда до ") + len("аренда до ")
	end := strings.Index(lease, ", heartbeat")
	if start < len("аренда до ") || end < start {
		t.Fatalf("строка прогона: %q", lease)
	}
	got := strings.Replace(out, lease[start:end], "<LEASE>", 1)
	if got != showGolden {
		t.Fatalf("clab show изменился:\n%s\nожидалось:\n%s", got, showGolden)
	}
}

func lineWith(out, needle string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func tail(s string) string {
	if len(s) > 1500 {
		return "…" + s[len(s)-1500:]
	}
	return s
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// ── whoami ──────────────────────────────────────────────────────────────────

// whoamiServer отвечает по маршрутам; маршрута нет в карте — 404, как у
// сервера, у которого этой ручки ещё нет.
func whoamiServer(t *testing.T, routes map[string]string) (*Client, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		calls = append(calls, path)
		body, ok := routes[path]
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"нет такого"}}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, token: "clab_x", http: srv.Client()}, &calls
}

const (
	meReply       = `{"principal":{"kind":"user","handle":"alice"},"authenticated":true,"memberships":[]}`
	instanceReply = `{"version":"v0.11.0","tenancy":"single","project_creation":"teams"}`
	rolesReply    = `{"roles":[{"key":"analytics","name":"Аналитик"},{"key":"developer","name":"Разработчик"},{"key":"test","name":"Тестировщик"}]}`
)

func projectReply(access string) string {
	reply := `{"project":{"key":"CLAB","name":"Cadence Lab"},"repositories":[]`
	if access != "" {
		reply += `,"access":` + access
	}
	return reply + `}`
}

// Вне репозитория и без SDLC_PROJECT whoami не ищет проект по git remote
// текущего каталога этого репозитория.
func outsideRepo(t *testing.T) {
	t.Helper()
	t.Setenv("SDLC_PROJECT", "")
	t.Chdir(t.TempDir())
}

// Критерий 11: без проекта — кто, API, инстанс и подсказка, exit 0.
func TestWhoamiWithoutProject(t *testing.T) {
	outsideRepo(t)
	routes := map[string]string{"/v1/auth/me": meReply, "/v1/instance": instanceReply}

	withJSON(t, false)
	c, calls := whoamiServer(t, routes)
	out, _, err := capture(t, func() error { return cmdWhoami(context.Background(), c, nil) })
	if err != nil {
		t.Fatalf("whoami без проекта: %v", err)
	}
	for _, want := range []string{"user:alice (по токену)", c.base, "v0.11.0, профиль single", "Проект:", "не задан — --project, SDLC_PROJECT"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	for _, call := range *calls {
		if strings.HasPrefix(call, "/v1/projects") {
			t.Errorf("без проекта whoami сходил в %s", call)
		}
	}

	withJSON(t, true)
	c, _ = whoamiServer(t, routes)
	out, _, err = capture(t, func() error { return cmdWhoami(context.Background(), c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--json не JSON: %q", out)
	}
	instance, _ := v["instance"].(map[string]any)
	project, present := v["project"]
	if v["principal"] != "user:alice" || v["authenticated"] != true || v["api"] != c.base ||
		instance["version"] != "v0.11.0" || !present || project != nil {
		t.Fatalf("--json: %s", out)
	}
}

// Критерий 12: с проектом — ключ, имя, уровень и роли из access сервера.
func TestWhoamiWithProject(t *testing.T) {
	cases := map[string]struct{ access, roles string }{
		"сужено":          {`{"level":"agent","allowed_roles":["developer"]}`, "developer"},
		"любые":           {`{"level":"maintainer","allowed_roles":null}`, "любые"},
		"ни одной":        {`{"level":"viewer","allowed_roles":[]}`, "ни одной"},
		"несколько ролей": {`{"level":"agent","allowed_roles":["developer","test"]}`, "developer, test"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			outsideRepo(t)
			t.Setenv("SDLC_PROJECT", "CLAB")
			withJSON(t, false)
			c, _ := whoamiServer(t, map[string]string{
				"/v1/auth/me": meReply, "/v1/instance": instanceReply,
				"/v1/projects/CLAB": projectReply(tc.access), "/v1/projects/CLAB/roles": rolesReply,
			})
			out, _, err := capture(t, func() error { return cmdWhoami(context.Background(), c, nil) })
			if err != nil {
				t.Fatal(err)
			}
			var level struct {
				Level string `json:"level"`
			}
			_ = json.Unmarshal([]byte(tc.access), &level)
			for _, want := range []string{"CLAB — Cadence Lab", "Уровень:", level.Level,
				"Можно забирать:  " + tc.roles + "\n", "analytics, developer, test"} {
				if !strings.Contains(out, want) {
					t.Errorf("в выводе нет %q:\n%s", want, out)
				}
			}
		})
	}

	// --json: проект с доступом как у сервера и ключами ролей.
	outsideRepo(t)
	withJSON(t, true)
	c, _ := whoamiServer(t, map[string]string{
		"/v1/auth/me": meReply, "/v1/instance": instanceReply,
		"/v1/projects/CLAB": projectReply(`{"level":"agent","allowed_roles":null}`), "/v1/projects/CLAB/roles": rolesReply,
	})
	out, _, err := capture(t, func() error { return cmdWhoami(context.Background(), c, []string{"--project", "CLAB"}) })
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Project struct {
			Key    string `json:"key"`
			Access struct {
				Level        string    `json:"level"`
				AllowedRoles *[]string `json:"allowed_roles"`
			} `json:"access"`
			Roles []string `json:"roles"`
		} `json:"project"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if v.Project.Key != "CLAB" || v.Project.Access.Level != "agent" || v.Project.Access.AllowedRoles != nil ||
		!reflect.DeepEqual(v.Project.Roles, []string{"analytics", "developer", "test"}) {
		t.Fatalf("--json с проектом: %s", out)
	}
}

// Критерий 13: явно названный несуществующий проект — ошибка сервера.
func TestWhoamiUnknownProject(t *testing.T) {
	outsideRepo(t)
	withJSON(t, false)
	c, _ := whoamiServer(t, map[string]string{"/v1/auth/me": meReply, "/v1/instance": instanceReply})
	_, _, err := capture(t, func() error { return cmdWhoami(context.Background(), c, []string{"--project", "NOPE"}) })
	var apiErr *APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("несуществующий проект: %v", err)
	}
}

// Критерий 14: сервер без access и без /instance — строк нет, падения нет.
func TestWhoamiAgainstOldServer(t *testing.T) {
	outsideRepo(t)
	t.Setenv("SDLC_PROJECT", "CLAB")
	withJSON(t, false)
	c, _ := whoamiServer(t, map[string]string{
		"/v1/auth/me": meReply, "/v1/projects/CLAB": projectReply(""), "/v1/projects/CLAB/roles": rolesReply,
	})
	out, _, err := capture(t, func() error { return cmdWhoami(context.Background(), c, nil) })
	if err != nil {
		t.Fatalf("старый сервер: %v", err)
	}
	if !strings.Contains(out, "CLAB — Cadence Lab") || !strings.Contains(out, "user:alice") {
		t.Fatalf("вывод:\n%s", out)
	}
	for _, absent := range []string{"Инстанс:", "Уровень:", "Можно забирать:"} {
		if strings.Contains(out, absent) {
			t.Errorf("против старого сервера напечатано %q:\n%s", absent, out)
		}
	}
}
