package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Уровень модели на роли, задаче и этапе задачи, разрешение при захвате и
// снимок на прогоне (CLAB-259, решение 0060). Номера — критерии приёмки
// анализа CLAB-259.

// ── без базы: форма, слияние, порядок ───────────────────────────────────────

func TestStagePatchShape(t *testing.T) {
	for _, c := range []struct {
		name, raw, contains string
	}{
		{"не объект", `["development"]`, "stage_settings — объект"},
		{"этап не объект", `{"development": "high"}`, "stage_settings.development — объект"},
		{"неизвестное поле", `{"development": {"model": "opus"}}`, `поле "model" не поддерживается`},
		{"значение вне трёх", `{"testing": {"model_level": "huge"}}`, "допустимы low, medium, high"},
		{"значение не строка", `{"testing": {"model_level": 3}}`, "строка"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseStagePatch(json.RawMessage(c.raw))
			if code(err) != apperr.CodeInvalidInput || !strings.Contains(err.Error(), c.contains) {
				t.Fatalf("ожидали invalid_input с %q: %v", c.contains, err)
			}
		})
	}
	for _, raw := range []string{``, `null`, `{}`} {
		if p, err := parseStagePatch(json.RawMessage(raw)); err != nil || len(p) != 0 {
			t.Fatalf("%q — пустой запрос: %v %v", raw, p, err)
		}
	}
}

// AC-6: слияние по ключам и полям, пустое снимает, этап без полей уходит.
func TestMergeStageSettings(t *testing.T) {
	current := json.RawMessage(`{"development":{"model_level":"high"},"testing":{"model_level":"low"}}`)
	for _, c := range []struct{ patch, want string }{
		{`{"testing":{"model_level":"medium"}}`, `{"development":{"model_level":"high"},"testing":{"model_level":"medium"}}`},
		{`{"development":{"model_level":""}}`, `{"testing":{"model_level":"low"}}`},
		{`{"qa":{"model_level":"high"}}`, `{"development":{"model_level":"high"},"qa":{"model_level":"high"},"testing":{"model_level":"low"}}`},
		{`{"qa":{"model_level":""}}`, `{"development":{"model_level":"high"},"testing":{"model_level":"low"}}`},
		{`{}`, `{"development":{"model_level":"high"},"testing":{"model_level":"low"}}`},
	} {
		patch, err := parseStagePatch(json.RawMessage(c.patch))
		if err != nil {
			t.Fatal(err)
		}
		got, err := mergeStageSettings(current, patch)
		if err != nil || string(got) != c.want {
			t.Fatalf("%s: %s, ожидали %s (%v)", c.patch, got, c.want, err)
		}
	}
	if got, _ := mergeStageSettings(nil, stagePatch{"qa": {"model_level": ""}}); string(got) != `{}` {
		t.Fatalf("пустое на заведении: %s", got)
	}
}

// AC-9: порядок этап → задача → роль → none, пустое пропускается.
func TestResolveExecutionOrder(t *testing.T) {
	str := func(s string) *string { return &s }
	run := domain.StageRun{StatusKey: "development"}
	for _, c := range []struct {
		name          string
		settings      string
		task, role    *string
		level, source string
	}{
		{"только роль", `{}`, nil, str("low"), "low", "role"},
		{"задача", `{}`, str("medium"), str("low"), "medium", "task"},
		{"этап", `{"development":{"model_level":"high"}}`, str("medium"), str("low"), "high", "stage"},
		{"этап другого статуса", `{"testing":{"model_level":"high"}}`, str("medium"), str("low"), "medium", "task"},
		{"этап другого статуса без задачи", `{"testing":{"model_level":"high"}}`, nil, str("low"), "low", "role"},
		{"пустые значения пропускаются", `{"development":{}}`, str(""), str(""), "", "none"},
		{"всё пусто", `{}`, nil, nil, "", "none"},
	} {
		task := domain.Task{ModelLevel: c.task, StageSettings: json.RawMessage(c.settings)}
		got := resolveExecution(task, run, c.role)
		level := ""
		if got.ModelLevel != nil {
			level = *got.ModelLevel
		}
		if level != c.level || got.ModelLevelSource != c.source {
			t.Errorf("%s: %q/%s, ожидали %q/%s", c.name, level, got.ModelLevelSource, c.level, c.source)
		}
	}
}

// ── по настоящей схеме ──────────────────────────────────────────────────────

type levelWorld struct {
	store     *postgres.Store
	engine    *Engine
	authoring *Authoring
	system    domain.Principal
	pool      domain.Principal
	other     domain.Principal
}

func newLevelWorld(t *testing.T) levelWorld {
	t.Helper()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	ctx := context.Background()
	pool, err := store.ResolveActor(ctx, store.DB(), "pool", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveActor(ctx, store.DB(), "other", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	return levelWorld{store: store, engine: engine, authoring: authoring, system: system, pool: pool, other: other}
}

func (w levelWorld) create(t *testing.T, title string, level *string, settings string) domain.Task {
	t.Helper()
	in := CreateTaskInput{CreateTaskInput: postgres.CreateTaskInput{Title: title, ModelLevel: level}}
	if settings != "" {
		in.StageSettings = json.RawMessage(settings)
	}
	task, err := w.engine.CreateTask(context.Background(), "SDLC", w.system.ID, in)
	if err != nil {
		t.Fatalf("заведение %s: %v", title, err)
	}
	return task
}

func (w levelWorld) start(t *testing.T, key string) {
	t.Helper()
	if _, err := w.engine.Execute(context.Background(), key, "start", w.system.ID, ""); err != nil {
		t.Fatalf("старт %s: %v", key, err)
	}
}

// roleVersion заводит новую версию роли с уровнем: прогон пинит версию при
// заходе на статус, поэтому версия ставится до старта задачи.
func (w levelWorld) roleVersion(t *testing.T, role string, level string) {
	t.Helper()
	cur, err := w.authoring.GetRole(context.Background(), "SDLC", role)
	if err != nil {
		t.Fatal(err)
	}
	in := RoleVersionInput{
		Instructions: cur.Latest.Instructions, InputKinds: cur.Latest.InputKinds,
		OutputContract: cur.Latest.OutputContract, ToolPolicy: cur.Latest.ToolPolicy,
		ModelHint: str("opus"), ModelLevel: &level,
	}
	if _, err := w.authoring.AddRoleVersion(context.Background(), "SDLC", role, w.system.ID, in); err != nil {
		t.Fatalf("версия роли %s: %v", role, err)
	}
}

func (w levelWorld) claim(t *testing.T, who domain.Principal, key string) ClaimResult {
	t.Helper()
	res, err := w.engine.ClaimWith(context.Background(), key, who.ID, "test", ClaimOptions{})
	if err != nil {
		t.Fatalf("захват %s: %v", key, err)
	}
	return res
}

// snapshot — пара на строке прогона: "level/source", пустые — "-".
func (w levelWorld) snapshot(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var level, source *string
	if err := w.store.DB().QueryRow(context.Background(),
		`SELECT model_level, model_level_source FROM stage_runs WHERE id = $1`, runID).
		Scan(&level, &source); err != nil {
		t.Fatal(err)
	}
	return pairOf(level, source)
}

func pairOf(level, source *string) string {
	l, s := "-", "-"
	if level != nil {
		l = *level
	}
	if source != nil {
		s = *source
	}
	return l + "/" + s
}

func execPair(e Execution) string { return pairOf(e.ModelLevel, &e.ModelLevelSource) }

func (w levelWorld) tasks(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.store.DB().QueryRow(context.Background(), `SELECT count(*) FROM tasks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AC-5, AC-7: заведение с уровнями, отказы без следа.
func TestCreateTaskModelLevels(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)

	task := w.create(t, "С уровнями", str("high"), `{"development": {"model_level": "high"}, "qa": {"model_level": ""}}`)
	if task.ModelLevel == nil || *task.ModelLevel != "high" ||
		string(task.StageSettings) != `{"development": {"model_level": "high"}}` {
		t.Fatalf("уровни заведённой задачи: %v %s", task.ModelLevel, task.StageSettings)
	}
	got, err := w.store.GetTaskByKey(ctx, w.store.DB(), task.Key)
	if err != nil || *got.ModelLevel != "high" || stageModelLevel(got.StageSettings, "development") != "high" {
		t.Fatalf("чтение задачи: %v %v %s", err, got.ModelLevel, got.StageSettings)
	}
	plain := w.create(t, "Без уровней", str(""), "")
	if plain.ModelLevel != nil || string(plain.StageSettings) != `{}` {
		t.Fatalf("пустое на заведении — не задано: %v %s", plain.ModelLevel, plain.StageSettings)
	}
	raw, _ := json.Marshal(plain)
	if !strings.Contains(string(raw), `"model_level":null`) || !strings.Contains(string(raw), `"stage_settings":{}`) {
		t.Fatalf("ответ задачи без уровней: %s", raw)
	}

	before := w.tasks(t)
	for _, c := range []struct {
		name       string
		level      *string
		settings   string
		taskType   string
		contains   string
		allowedKey bool
	}{
		{"терминальный статус без роли", nil, `{"done": {"model_level": "low"}}`, "", "допустимы analysis, architecture, development, code_review, qa", true},
		{"опечатка в ключе", nil, `{"developmnet": {"model_level": "low"}}`, "", "developmnet", true},
		{"статус чужого цикла", nil, `{"decomposition": {"model_level": "low"}}`, "", "decomposition", true},
		{"ключ цикла задач на эпике", nil, `{"development": {"model_level": "low"}}`, "epic", "допустимы decomposition", true},
		{"значение", nil, `{"development": {"model_level": "huge"}}`, "", "low, medium, high", false},
		{"уровень задачи", str("max"), ``, "", "low, medium, high", false},
		{"поле этапа", nil, `{"development": {"profile": "x"}}`, "", "profile", false},
		{"не объект", nil, `"high"`, "", "stage_settings — объект", false},
	} {
		in := CreateTaskInput{CreateTaskInput: postgres.CreateTaskInput{
			Title: c.name, TaskType: c.taskType, ModelLevel: c.level,
		}}
		if c.settings != "" {
			in.StageSettings = json.RawMessage(c.settings)
		}
		_, err := w.engine.CreateTask(ctx, "SDLC", w.system.ID, in)
		if code(err) != apperr.CodeInvalidInput || !strings.Contains(err.Error(), c.contains) {
			t.Errorf("%s: ожидали invalid_input с %q: %v", c.name, c.contains, err)
			continue
		}
		if c.allowedKey {
			details, _ := apperr.As(err).Details.(map[string]any)
			if _, ok := details["allowed_keys"]; !ok {
				t.Errorf("%s: в отказе нет перечня допустимых ключей: %v", c.name, details)
			}
		}
	}
	if after := w.tasks(t); after != before {
		t.Fatalf("отказ завёл задачу: %d → %d", before, after)
	}
	var events int
	if err := w.store.DB().QueryRow(ctx, `
		SELECT count(*) FROM task_events WHERE type = $1 AND payload->>'title' IN
		('терминальный статус без роли', 'значение', 'не объект')`, domain.EventTaskCreated).Scan(&events); err != nil || events != 0 {
		t.Fatalf("отказ оставил событие: %v %d", err, events)
	}
	var payload json.RawMessage
	if err := w.store.DB().QueryRow(ctx, `
		SELECT payload FROM task_events WHERE task_id = $1 AND type = $2`, task.ID, domain.EventTaskCreated).
		Scan(&payload); err != nil || !strings.Contains(string(payload), `"model_level": "high"`) {
		t.Fatalf("событие заведения без уровней: %v %s", err, payload)
	}
}

// AC-6: правка уровней, слияние, отказы без изменений, лента.
func TestUpdateTaskModelLevels(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)
	task := w.create(t, "Правка", str("medium"),
		`{"development": {"model_level": "high"}, "qa": {"model_level": "low"}}`)
	update := func(in UpdateTaskInput) (domain.Task, error) {
		in.Level = domain.MemberMaintainer
		return w.engine.UpdateTask(ctx, task.Key, w.system.ID, in)
	}
	settings := func(raw string) *json.RawMessage { r := json.RawMessage(raw); return &r }

	out, err := update(UpdateTaskInput{StageSettings: settings(`{"qa": {"model_level": "medium"}}`)})
	if err != nil || string(out.StageSettings) != `{"qa": {"model_level": "medium"}, "development": {"model_level": "high"}}` ||
		*out.ModelLevel != "medium" {
		t.Fatalf("слияние этапа: %v %s %v", err, out.StageSettings, out.ModelLevel)
	}
	out, err = update(UpdateTaskInput{StageSettings: settings(`{"development": {"model_level": ""}}`)})
	if err != nil || string(out.StageSettings) != `{"qa": {"model_level": "medium"}}` {
		t.Fatalf("снятие этапа: %v %s", err, out.StageSettings)
	}
	out, err = update(UpdateTaskInput{Title: str("Правка уровней")})
	if err != nil || *out.ModelLevel != "medium" || string(out.StageSettings) != `{"qa": {"model_level": "medium"}}` {
		t.Fatalf("правка без полей уровня их тронула: %v %v %s", err, out.ModelLevel, out.StageSettings)
	}
	out, err = update(UpdateTaskInput{ModelLevel: str("")})
	if err != nil || out.ModelLevel != nil {
		t.Fatalf("снятие уровня задачи: %v %v", err, out.ModelLevel)
	}

	for _, c := range []struct {
		name string
		in   UpdateTaskInput
		want string
	}{
		{"ключ", UpdateTaskInput{StageSettings: settings(`{"done": {"model_level": "low"}}`)}, "допустимы"},
		{"значение этапа", UpdateTaskInput{StageSettings: settings(`{"qa": {"model_level": "max"}}`)}, "low, medium, high"},
		{"значение задачи", UpdateTaskInput{ModelLevel: str("max"), Title: str("не дойдёт")}, "low, medium, high"},
		{"поле этапа", UpdateTaskInput{StageSettings: settings(`{"qa": {"model": "opus"}}`)}, "model"},
		{"ничего", UpdateTaskInput{}, "model_level, stage_settings"},
	} {
		_, err := update(c.in)
		if code(err) != apperr.CodeInvalidInput || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: ожидали invalid_input с %q: %v", c.name, c.want, err)
		}
	}
	got, _ := w.store.GetTaskByKey(ctx, w.store.DB(), task.Key)
	if got.ModelLevel != nil || string(got.StageSettings) != `{"qa": {"model_level": "medium"}}` || got.Title != "Правка уровней" {
		t.Fatalf("отказ изменил задачу: %v %s %q", got.ModelLevel, got.StageSettings, got.Title)
	}

	var payload json.RawMessage
	if err := w.store.DB().QueryRow(ctx, `
		SELECT payload FROM task_events WHERE task_id = $1 AND type = $2
		ORDER BY created_at, id LIMIT 1`, task.ID, domain.EventTaskUpdated).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var first struct {
		Fields  []string                              `json:"fields"`
		Changed map[string]map[string]json.RawMessage `json:"changed"`
	}
	if err := json.Unmarshal(payload, &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Fields) != 1 || first.Fields[0] != "stage_settings" ||
		!strings.Contains(string(first.Changed["stage_settings"]["from"]), `"low"`) ||
		!strings.Contains(string(first.Changed["stage_settings"]["to"]), `"medium"`) {
		t.Fatalf("событие правки этапа: %s", payload)
	}
	var last json.RawMessage
	if err := w.store.DB().QueryRow(ctx, `
		SELECT payload FROM task_events WHERE task_id = $1 AND type = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, task.ID, domain.EventTaskUpdated).Scan(&last); err != nil ||
		!strings.Contains(string(last), `"model_level": {"to": null, "from": "medium"}`) {
		t.Fatalf("событие снятия уровня задачи: %v %s", err, last)
	}
}

// AC-9, AC-10: пять случаев разрешения сквозь захват, снимок равен ответу.
func TestClaimResolvesExecution(t *testing.T) {
	w := newLevelWorld(t)
	w.roleVersion(t, "analyst", "medium")

	cases := []struct {
		name     string
		level    *string
		settings string
		want     string
	}{
		{"только роль", nil, "", "medium/role"},
		{"задача", str("high"), "", "high/task"},
		{"этап", str("high"), `{"analysis": {"model_level": "low"}}`, "low/stage"},
		{"этап другого статуса", str("high"), `{"development": {"model_level": "low"}}`, "high/task"},
		{"этап другого статуса без задачи", nil, `{"development": {"model_level": "low"}}`, "medium/role"},
	}
	keys := make([]string, len(cases))
	for i, c := range cases {
		keys[i] = w.create(t, c.name, c.level, c.settings).Key
		w.start(t, keys[i])
	}
	// Всё пусто: версия роли без уровня, и задача заходит на статус после неё.
	w.roleVersion(t, "analyst", "")
	empty := w.create(t, "всё пусто", nil, "").Key
	w.start(t, empty)
	cases = append(cases, struct {
		name     string
		level    *string
		settings string
		want     string
	}{"всё пусто", nil, "", "-/none"})
	keys = append(keys, empty)

	for i, c := range cases {
		res := w.claim(t, w.pool, keys[i])
		if got := execPair(res.Execution); got != c.want {
			t.Errorf("%s: execution %s, ожидали %s", c.name, got, c.want)
		}
		if got := w.snapshot(t, res.StageRunID); got != c.want {
			t.Errorf("%s: снимок %s, ожидали %s", c.name, got, c.want)
		}
		if res.Role == nil || res.Role.ModelHint == nil || *res.Role.ModelHint != "opus" {
			t.Errorf("%s: role.model_hint пропал: %+v", c.name, res.Role)
		}
		wantRole := "medium"
		if c.name == "всё пусто" {
			wantRole = ""
		}
		if got := deref(res.Role.ModelLevel); got != wantRole {
			t.Errorf("%s: role.model_level %q, ожидали %q", c.name, got, wantRole)
		}
		raw, _ := json.Marshal(res)
		if !strings.Contains(string(raw), `"execution":{"model_level":`) {
			t.Errorf("%s: execution не в ответе: %s", c.name, raw)
		}
		// AC-13: раннер прежней версии разбирает ответ своей структурой
		// (копия claimResult из cmd/clab-runner/client.go) и берёт model_hint.
		var runner struct {
			StageRunID string `json:"stage_run_id"`
			Role       *struct {
				Key        string  `json:"key"`
				ModelHint  *string `json:"model_hint"`
				ToolPolicy struct {
					Allow []string `json:"allow"`
					Deny  []string `json:"deny"`
				} `json:"tool_policy"`
			} `json:"role"`
			Lease *struct {
				ExpiresAt                string `json:"expires_at"`
				HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
			} `json:"lease"`
		}
		if err := json.Unmarshal(raw, &runner); err != nil || runner.Role == nil ||
			deref(runner.Role.ModelHint) != "opus" || runner.Lease == nil {
			t.Errorf("%s: ответ не разбирается раннером прежней версии: %v %+v", c.name, err, runner)
		}
	}
}

// AC-10: снимок держится до новой аренды, State отдаёт снимок или то, что
// получит следующий захват.
func TestExecutionSnapshotLifecycle(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)
	task := w.create(t, "Снимок", str("low"), "")
	w.start(t, task.Key)
	setLevel := func(level string) {
		t.Helper()
		if _, err := w.engine.UpdateTask(ctx, task.Key, w.system.ID, UpdateTaskInput{
			ModelLevel: &level, Level: domain.MemberMaintainer,
		}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		st, err := w.engine.State(ctx, task.Key)
		if err != nil {
			t.Fatal(err)
		}
		return execPair(st.Execution)
	}

	// До захвата снимка нет, State отдаёт текущее разрешение.
	cur, _ := w.store.GetTaskByKey(ctx, w.store.DB(), task.Key)
	run, err := w.store.CurrentStageRun(ctx, w.store.DB(), cur.ID, cur.StatusID)
	if err != nil {
		t.Fatal(err)
	}
	if w.snapshot(t, run.ID) != "-/-" || state() != "low/task" {
		t.Fatalf("до захвата: снимок %s, state %s", w.snapshot(t, run.ID), state())
	}

	res := w.claim(t, w.pool, task.Key)
	setLevel("high")
	if state() != "low/task" || w.snapshot(t, res.StageRunID) != "low/task" {
		t.Fatalf("правка после захвата тронула идущий прогон: %s", state())
	}
	if _, err := w.engine.Heartbeat(ctx, res.StageRunID, w.pool.ID); err != nil {
		t.Fatal(err)
	}
	again := w.claim(t, w.pool, task.Key)
	if execPair(again.Execution) != "low/task" || w.snapshot(t, res.StageRunID) != "low/task" {
		t.Fatalf("подтверждение живой аренды пересчитало снимок: %s", execPair(again.Execution))
	}

	// release → новый захват: текущие уровни.
	if err := w.engine.Release(ctx, res.StageRunID, w.pool.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Снимок у отпущенного прогона остаётся: State отдаёт его, а не
	// текущее разрешение (решение человека на апруве анализа).
	if state() != "low/task" {
		t.Fatalf("после release State не отдаёт снимок прогона: %s", state())
	}
	if got := execPair(w.claim(t, w.pool, task.Key).Execution); got != "high/task" {
		t.Fatalf("захват после release: %s", got)
	}

	// Протухание → захват другим: тоже текущие.
	setLevel("medium")
	if _, err := w.store.DB().Exec(ctx,
		`UPDATE stage_runs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, res.StageRunID); err != nil {
		t.Fatal(err)
	}
	if got := execPair(w.claim(t, w.other, task.Key).Execution); got != "medium/task" {
		t.Fatalf("захват после протухания: %s", got)
	}
	// Тот же держатель после протухания своей аренды — тоже новая аренда.
	setLevel("low")
	if _, err := w.store.DB().Exec(ctx,
		`UPDATE stage_runs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, res.StageRunID); err != nil {
		t.Fatal(err)
	}
	if got := execPair(w.claim(t, w.other, task.Key).Execution); got != "low/task" {
		t.Fatalf("захват своей протухшей аренды: %s", got)
	}

	// Прогон, взятый до миграции: снимок пишется один раз, при подтверждении.
	if _, err := w.store.DB().Exec(ctx,
		`UPDATE stage_runs SET model_level = NULL, model_level_source = NULL WHERE id = $1`, res.StageRunID); err != nil {
		t.Fatal(err)
	}
	setLevel("high")
	if got := execPair(w.claim(t, w.other, task.Key).Execution); got != "high/task" || w.snapshot(t, res.StageRunID) != "high/task" {
		t.Fatalf("подтверждение прогона без снимка: %s %s", got, w.snapshot(t, res.StageRunID))
	}
	setLevel("low")
	if got := execPair(w.claim(t, w.other, task.Key).Execution); got != "high/task" {
		t.Fatalf("повторное подтверждение переписало снимок: %s", got)
	}

	// Закрытый прогон своей пары не меняет.
	if _, err := w.engine.Execute(ctx, task.Key, "cancel", w.system.ID, ""); err != nil {
		t.Fatal(err)
	}
	setLevel("medium")
	if err := w.store.InTx(ctx, func(tx pgx.Tx) error {
		return w.store.SnapshotModelLevel(ctx, tx, res.StageRunID, str("low"), "task")
	}); err != nil {
		t.Fatal(err)
	}
	if got := w.snapshot(t, res.StageRunID); got != "high/task" {
		t.Fatalf("закрытый прогон переписан: %s", got)
	}
	runs, err := w.store.ListStageRuns(ctx, w.store.DB(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.ID == res.StageRunID && pairOf(r.ModelLevel, r.ModelLevelSource) != "high/task" {
			t.Fatalf("stage_runs[] задачи: %s", pairOf(r.ModelLevel, r.ModelLevelSource))
		}
	}
}

// AC-11: перепроверка разрешает по статусу родителя и версии роли ревьюера.
func TestReviewRunExecution(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)
	w.roleVersion(t, "analyst", "medium")

	toReview := func(key string) ClaimResult {
		t.Helper()
		w.start(t, key)
		parent := w.claim(t, w.pool, key)
		for _, kind := range []string{"analysis.brief", "analysis.acceptance_criteria"} {
			if _, err := w.engine.SubmitArtifact(ctx, key, w.pool.ID, SubmitArtifactInput{
				Kind: kind, Title: kind, Content: []byte("# " + kind),
			}); err != nil {
				t.Fatal(err)
			}
		}
		res, err := w.engine.Execute(ctx, key, "analysis_done", w.pool.ID, "")
		if err != nil || !res.Blocked {
			t.Fatalf("переход до ревью %s: %v %+v", key, err, res)
		}
		return parent
	}
	claimReview := func(key string, parent ClaimResult, want string) {
		t.Helper()
		review := w.claim(t, w.other, key)
		if review.Kind != domain.StageKindReview {
			t.Fatalf("%s: захвачен не прогон ревью: %s", key, review.Kind)
		}
		if got := execPair(review.Execution); got != want || w.snapshot(t, review.StageRunID) != want {
			t.Errorf("%s: перепроверка %s (снимок %s), ожидали %s", key, got, w.snapshot(t, review.StageRunID), want)
		}
		if got := w.snapshot(t, parent.StageRunID); got != execPair(parent.Execution) {
			t.Errorf("%s: родительский прогон переписан: %s, был %s", key, got, execPair(parent.Execution))
		}
	}

	// Уровень только у роли исполнителя, у ревьюера нет — none.
	bare := w.create(t, "ревьюер без уровня", nil, "")
	bareParent := toReview(bare.Key)
	claimReview(bare.Key, bareParent, "-/none")

	w.roleVersion(t, "reviewer", "low")
	staged := w.create(t, "этап задан", nil, `{"analysis": {"model_level": "high"}}`)
	stagedParent := toReview(staged.Key)
	claimReview(staged.Key, stagedParent, "high/stage")

	roleOnly := w.create(t, "уровень у ревьюера", nil, "")
	roleParent := toReview(roleOnly.Key)
	if execPair(roleParent.Execution) != "medium/role" {
		t.Fatalf("исполнитель этапа: %s", execPair(roleParent.Execution))
	}
	claimReview(roleOnly.Key, roleParent, "low/role")
}

// AC-1, AC-2: миграция только добавляет; база с данными на 000027 после
// 000028 читается, захватывается и двигается; ограничения схемы держат пару.
func TestModelLevelMigrationOnFilledBase(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)
	db := w.store.DB()

	// Данные до апгрейда: открытый захваченный прогон, закрытый прогон,
	// задача в new и версия роли.
	w.start(t, "SDLC-1")
	open := w.claim(t, w.pool, "SDLC-1")
	w.start(t, "SDLC-3")
	w.claim(t, w.pool, "SDLC-3")
	if _, err := w.engine.Execute(ctx, "SDLC-3", "cancel", w.system.ID, ""); err != nil {
		t.Fatal(err)
	}

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
			WHERE (table_name, column_name) IN (('role_versions', 'model_level'), ('tasks', 'model_level'),
			      ('tasks', 'stage_settings'), ('stage_runs', 'model_level'), ('stage_runs', 'model_level_source'))`).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := db.Exec(ctx, read("000028_model_level.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if columns() != 0 {
		t.Fatal("down оставил колонки уровня")
	}
	// Состояние 000027 с данными: строки пишутся без новых колонок.
	if _, err := db.Exec(ctx, `UPDATE tasks SET title = title || ' (до апгрейда)' WHERE key = 'SDLC-2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, read("000028_model_level.up.sql")); err != nil {
		t.Fatalf("up на заполненной базе: %v", err)
	}
	if columns() != 5 {
		t.Fatal("up завёл не все колонки")
	}
	var badTasks, badRuns, badRoles int
	if err := db.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM tasks WHERE model_level IS NOT NULL OR stage_settings <> '{}'),
		       (SELECT count(*) FROM stage_runs WHERE model_level IS NOT NULL OR model_level_source IS NOT NULL),
		       (SELECT count(*) FROM role_versions WHERE model_level IS NOT NULL)`).
		Scan(&badTasks, &badRuns, &badRoles); err != nil {
		t.Fatal(err)
	}
	if badTasks+badRuns+badRoles != 0 {
		t.Fatalf("старые строки получили значения: задачи %d, прогоны %d, роли %d", badTasks, badRuns, badRoles)
	}

	// Чтение, захват и переход работают; прогон без снимка получает его при
	// подтверждении.
	task, err := w.store.GetTaskByKey(ctx, db, "SDLC-1")
	if err != nil || task.ModelLevel != nil || string(task.StageSettings) != `{}` {
		t.Fatalf("чтение задачи после апгрейда: %v %v %s", err, task.ModelLevel, task.StageSettings)
	}
	runs, err := w.store.ListStageRuns(ctx, db, task.ID)
	if err != nil || len(runs) == 0 || runs[len(runs)-1].ModelLevelSource != nil {
		t.Fatalf("прогоны после апгрейда: %v %+v", err, runs)
	}
	if again := w.claim(t, w.pool, "SDLC-1"); again.StageRunID != open.StageRunID || execPair(again.Execution) != "-/none" {
		t.Fatalf("подтверждение после апгрейда: %s %s", again.StageRunID, execPair(again.Execution))
	}
	if _, err := w.engine.Execute(ctx, "SDLC-2", "start", w.system.ID, ""); err != nil {
		t.Fatalf("переход после апгрейда: %v", err)
	}

	// Ограничения схемы — негативные проверки.
	for _, c := range []struct{ name, sql string }{
		{"уровень задачи", `UPDATE tasks SET model_level = 'huge' WHERE key = 'SDLC-1'`},
		{"stage_settings не объект", `UPDATE tasks SET stage_settings = '[]' WHERE key = 'SDLC-1'`},
		{"уровень версии роли", `UPDATE role_versions SET model_level = 'max' WHERE version = 1`},
		{"источник", `UPDATE stage_runs SET model_level_source = 'auto' WHERE id = '` + open.StageRunID.String() + `'`},
		{"none с уровнем", `UPDATE stage_runs SET model_level = 'low', model_level_source = 'none' WHERE id = '` + open.StageRunID.String() + `'`},
		{"уровень без источника", `UPDATE stage_runs SET model_level = 'low', model_level_source = NULL WHERE id = '` + open.StageRunID.String() + `'`},
		{"источник без уровня", `UPDATE stage_runs SET model_level = NULL, model_level_source = 'task' WHERE id = '` + open.StageRunID.String() + `'`},
	} {
		if _, err := db.Exec(ctx, c.sql); err == nil {
			t.Errorf("%s: CHECK пропустил", c.name)
		}
	}

	// Повторный down → up на этой же базе тоже проходит.
	if _, err := db.Exec(ctx, read("000028_model_level.down.sql")); err != nil {
		t.Fatalf("второй down: %v", err)
	}
	if _, err := db.Exec(ctx, read("000028_model_level.up.sql")); err != nil {
		t.Fatalf("второй up: %v", err)
	}
}

// AC-3, AC-4, AC-12: уровень версии роли и связка роли; цикл не тронут.
func TestRoleModelLevelAndBundle(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)

	before, err := w.authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	wfBefore, _ := json.Marshal(before.Workflow)

	if _, err := w.authoring.AddRoleVersion(ctx, "SDLC", "qa", w.system.ID, RoleVersionInput{
		Instructions: "x", ModelLevel: str("huge"),
	}); code(err) != apperr.CodeInvalidInput || !strings.Contains(err.Error(), "low, medium, high") {
		t.Fatalf("неверный уровень версии роли: %v", err)
	}
	role, err := w.authoring.CreateRole(ctx, "SDLC", w.system.ID, CreateRoleInput{
		Key: "writer", Name: "Техписатель",
		Version: &RoleVersionInput{Instructions: "Пиши.", ModelLevel: str("medium"), ModelHint: str("sonnet")},
	})
	if err != nil || deref(role.Latest.ModelLevel) != "medium" || deref(role.Latest.ModelHint) != "sonnet" {
		t.Fatalf("роль с уровнем: %v %+v", err, role.Latest)
	}
	plain, err := w.authoring.AddRoleVersion(ctx, "SDLC", "writer", w.system.ID, RoleVersionInput{
		Instructions: "Пиши кратко.", ModelLevel: str(""),
	})
	if err != nil || plain.ModelLevel != nil {
		t.Fatalf("пустой уровень — версия без уровня: %v %v", err, plain.ModelLevel)
	}
	if _, err := w.authoring.AddRoleVersion(ctx, "SDLC", "writer", w.system.ID, RoleVersionInput{
		Instructions: "Пиши кратко.", ModelLevel: str("high"),
	}); err != nil {
		t.Fatal(err)
	}

	exported, err := w.authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range exported.Roles {
		raw, _ := json.Marshal(r)
		has := strings.Contains(string(raw), `"model_level"`)
		if r.Key == "writer" && (!has || r.ModelLevel != "high") {
			t.Fatalf("у роли с уровнем нет model_level в связке: %s", raw)
		}
		if r.Key != "writer" && has {
			t.Fatalf("у роли без уровня model_level в связке: %s", raw)
		}
	}
	wfAfter, _ := json.Marshal(exported.Workflow)
	if string(wfBefore) != string(wfAfter) || strings.Contains(string(wfAfter), "model_level") {
		t.Fatalf("раздел цикла связки изменился:\n%s\n%s", wfBefore, wfAfter)
	}

	// Повтор выгруженного — всё unchanged.
	again, err := w.authoring.Import(ctx, "SDLC", w.system.ID, Bundle{Version: BundleVersion, Roles: exported.Roles}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again.Roles {
		if r.Outcome != "unchanged" {
			t.Errorf("повтор связки: роль %s — %s", r.Key, r.Outcome)
		}
	}
	// Другой уровень — новая версия; связка прежней версии без поля — тоже.
	writer := exported.Roles[0]
	for _, r := range exported.Roles {
		if r.Key == "writer" {
			writer = r
		}
	}
	for _, level := range []string{"low", ""} {
		writer.ModelLevel = level
		rep, err := w.authoring.Import(ctx, "SDLC", w.system.ID, Bundle{Version: BundleVersion, Roles: []BundleRole{writer}}, false)
		if err != nil || rep.Roles[0].Outcome != "new_version" {
			t.Fatalf("уровень %q в связке: %v %+v", level, err, rep.Roles)
		}
		got, _ := w.authoring.GetRole(ctx, "SDLC", "writer")
		if deref(got.Latest.ModelLevel) != level {
			t.Fatalf("уровень после импорта: %v, ожидали %q", got.Latest.ModelLevel, level)
		}
	}
	// Неверное значение: отказ всей связки, в dry_run тоже; ничего не применено.
	versions := func() int {
		got, _ := w.authoring.GetRole(ctx, "SDLC", "writer")
		return len(got.Versions)
	}
	n := versions()
	for _, dry := range []bool{true, false} {
		bad := Bundle{Version: BundleVersion, Roles: []BundleRole{
			{Key: "writer", Name: "Техписатель", Instructions: "Пиши иначе.", ModelLevel: "medium"},
			{Key: "qa", Name: "QA", Instructions: "x", ModelLevel: "huge"},
		}}
		if _, err := w.authoring.Import(ctx, "SDLC", w.system.ID, bad, dry); code(err) != apperr.CodeInvalidInput {
			t.Fatalf("неверный уровень в связке (dry_run=%v): %v", dry, err)
		}
	}
	if versions() != n {
		t.Fatal("отказ связки применил роль")
	}
}
