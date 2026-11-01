package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// CLAB-336: параметр states у repo_ref_required — какие состояния PR гейт
// засчитывает. Проверки идут по настоящей схеме: фильтр состояний — SQL, и
// согласие с task_repo_refs видно только на базе. Ключи статусов и переходов
// в тестах — данные теста, движок их не знает.

// twoRepoTask заводит мультирепозиторную задачу на acme/a и acme/b.
func twoRepoTask(t *testing.T, ctx context.Context, engine *Engine, system domain.Principal, repos ...string) domain.Task {
	t.Helper()
	for _, repo := range repos {
		if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: repo}); err != nil {
			t.Fatalf("привязка %s: %v", repo, err)
		}
	}
	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			Title:        "Задача на нескольких репозиториях",
			Properties:   json.RawMessage(`{"multi_repo":true}`),
			Repositories: repos,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// bindPR привязывает PR репозитория; state "" — состояние ещё не синхронизировано.
func bindPR(t *testing.T, ctx context.Context, store *postgres.Store, task domain.Task, repo, number, state string) {
	t.Helper()
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO task_repo_refs (task_id, repository_id, ref_type, ref, state)
		SELECT $1, r.id, 'pull_request', $3, nullif($4, '')
		FROM repositories r WHERE r.owner || '/' || r.name = $2`,
		task.ID, repo, number, state); err != nil {
		t.Fatalf("привязка PR %s: %v", repo, err)
	}
}

func evalRepoRef(t *testing.T, ctx context.Context, store *postgres.Store, engine *Engine, task domain.Task, config string) GateOutcome {
	t.Helper()
	out, err := engine.evalRepoRefRequired(ctx, store.DB(), EvalInput{
		Task: task, Gate: gate(domain.GateRepoRefRequired, config)})
	if err != nil {
		t.Fatalf("прогон гейта %s: %v", config, err)
	}
	return out
}

const perRepo = `"ref_type":"pull_request","per_repository":true`

// AC-1, AC-2: слитый PR в одном репозитории и открытый в другом
// засчитываются; закрытый без слияния — нет, причина называет репозиторий и
// допустимые состояния.
func TestRepoRefStatesCountMergedAndOpenPerRepository(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := twoRepoTask(t, ctx, engine, system, "acme/a", "acme/b")
	cfg := `{` + perRepo + `,"states":["open","merged"]}`

	bindPR(t, ctx, store, task, "acme/a", "10", "merged")
	if out := evalRepoRef(t, ctx, store, engine, task, cfg); out.Status != domain.GateResultFailed ||
		!strings.Contains(out.Reason, "acme/b") {
		t.Fatalf("в acme/b PR нет, ждали failed с репозиторием: %+v", out)
	}

	bindPR(t, ctx, store, task, "acme/b", "11", "open")
	out := evalRepoRef(t, ctx, store, engine, task, cfg)
	if out.Status != domain.GateResultPassed {
		t.Fatalf("слитый и открытый PR обязаны засчитываться: %+v", out)
	}
	if out.Details["states"] == nil || out.Details["per_repository"] == nil {
		t.Fatalf("в details нет states и счёта по репозиториям: %+v", out.Details)
	}

	// Закрытый без слияния в acme/a вместо слитого: гейт отбивает.
	if _, err := store.DB().Exec(ctx, `
		UPDATE task_repo_refs SET state = 'closed' WHERE task_id = $1 AND ref = '10'`, task.ID); err != nil {
		t.Fatal(err)
	}
	out = evalRepoRef(t, ctx, store, engine, task, cfg)
	if out.Status != domain.GateResultFailed ||
		out.Reason != "нет pull_request в состоянии open/merged в: acme/a" {
		t.Fatalf("закрытый PR не засчитывается: %+v", out)
	}
	if missing, _ := out.Details["missing"].([]string); len(missing) != 1 || missing[0] != "acme/a" {
		t.Fatalf("details.missing: %+v", out.Details)
	}

	// closed в списке — закрытый засчитан.
	if out := evalRepoRef(t, ctx, store, engine, task, `{`+perRepo+`,"states":["open","closed"]}`); out.Status != domain.GateResultPassed {
		t.Fatalf("closed в states: %+v", out)
	}
}

// AC-3: require_open работает как раньше.
func TestRepoRefRequireOpenUnchanged(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := twoRepoTask(t, ctx, engine, system, "acme/a", "acme/b")
	cfg := `{` + perRepo + `,"require_open":true}`

	bindPR(t, ctx, store, task, "acme/a", "10", "merged")
	bindPR(t, ctx, store, task, "acme/b", "11", "open")
	out := evalRepoRef(t, ctx, store, engine, task, cfg)
	if out.Status != domain.GateResultFailed || out.Reason != "нет pull_request в: acme/a" {
		t.Fatalf("слитый PR при require_open не засчитан: %+v", out)
	}
	if _, ok := out.Details["states"]; ok {
		t.Fatalf("без states в details его быть не должно: %+v", out.Details)
	}

	// Несинхронизированное состояние засчитано, как и прежде.
	if _, err := store.DB().Exec(ctx, `
		UPDATE task_repo_refs SET state = NULL WHERE task_id = $1 AND ref = '10'`, task.ID); err != nil {
		t.Fatal(err)
	}
	if out := evalRepoRef(t, ctx, store, engine, task, cfg); out.Status != domain.GateResultPassed {
		t.Fatalf("state IS NULL при require_open засчитывается: %+v", out)
	}

	// Ни require_open, ни states: состояние не важно.
	if _, err := store.DB().Exec(ctx, `
		UPDATE task_repo_refs SET state = 'declined' WHERE task_id = $1 AND ref = '10'`, task.ID); err != nil {
		t.Fatal(err)
	}
	if out := evalRepoRef(t, ctx, store, engine, task, `{`+perRepo+`}`); out.Status != domain.GateResultPassed {
		t.Fatalf("без фильтра состояние не важно: %+v", out)
	}
}

// AC-4: несинхронизированное состояние засчитывается, если в states есть
// open, и не засчитывается без него.
func TestRepoRefStatesNullState(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := twoRepoTask(t, ctx, engine, system, "acme/a")
	bindPR(t, ctx, store, task, "acme/a", "10", "")

	if out := evalRepoRef(t, ctx, store, engine, task, `{`+perRepo+`,"states":["open","merged"]}`); out.Status != domain.GateResultPassed {
		t.Fatalf("NULL при states с open: %+v", out)
	}
	out := evalRepoRef(t, ctx, store, engine, task, `{`+perRepo+`,"states":["merged"]}`)
	if out.Status != domain.GateResultFailed || !strings.Contains(out.Reason, "acme/a") {
		t.Fatalf("NULL при states [merged] обязан валить гейт: %+v", out)
	}
}

// AC-5: проверка цикла ловит противоречивую настройку.
func TestValidateRepoRefStates(t *testing.T) {
	dev := status("development", true, false)
	testing_ := status("testing", false, false)
	done := status("done", false, true)

	cycle := func(config string) domain.Workflow {
		g := domain.Gate{Key: "pull_request", Type: domain.GateRepoRefRequired,
			Blocking: true, Config: json.RawMessage(config)}
		return wf([]domain.Status{dev, testing_, done},
			transition("dev_done", &dev, testing_, g), transition("finish", &testing_, done))
	}
	cases := []struct {
		name, config string
		bad          bool
	}{
		{"open и merged", `{"ref_type":"pull_request","states":["open","merged"]}`, false},
		{"все четыре", `{"states":["open","merged","closed","declined"]}`, false},
		{"прежний require_open", `{"ref_type":"pull_request","require_open":true}`, false},
		{"require_open: false со states", `{"require_open":false,"states":["merged"]}`, false},
		{"без фильтра", `{"ref_type":"pull_request"}`, false},
		{"оба сразу", `{"require_open":true,"states":["open"]}`, true},
		{"пустой список", `{"states":[]}`, true},
		{"неизвестное значение", `{"states":["open","abandoned"]}`, true},
		{"строка вместо списка", `{"states":"open"}`, true},
		{"регистр важен", `{"states":["Open"]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validateWorkflow(cycle(tc.config), allImplemented, true)
			if !tc.bad {
				if !v.Valid {
					t.Fatalf("настройка верна: %v", v.Problems)
				}
				return
			}
			if v.Valid {
				t.Fatal("противоречивая настройка прошла проверку")
			}
			var found *Problem
			for i, p := range v.Problems {
				if p.Code == "repo_ref_states_invalid" {
					found = &v.Problems[i]
				}
			}
			if found == nil || found.Transition != "dev_done" || found.Gate != "pull_request" {
				t.Fatalf("нет проблемы с ключами перехода и гейта: %v", v.Problems)
			}
		})
	}
}

// returnCycle — цикл с выкладкой и возвратом из неё в разработку. Гейты PR
// стоят на выходе из разработки и из слияния; их настройка — параметр теста.
func returnCycle(prConfig string) BundleWorkflow {
	yes := true
	pr := func(key string) []BundleGate {
		return []BundleGate{{Key: key, Name: "PR привязан", Type: domain.GateRepoRefRequired,
			Blocking: &yes, Config: json.RawMessage(prConfig)}}
	}
	return BundleWorkflow{
		Name: "Return", TaskTypes: []string{"task"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
			{Key: "development", Name: "Development", Category: "active"},
			{Key: "testing", Name: "Testing", Category: "active"},
			{Key: "merge", Name: "Merge", Category: "active"},
			{Key: "release", Name: "Release", Category: "active"},
			{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
			{Key: "cancelled", Name: "Cancelled", Category: "cancelled", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "Start", From: "new", To: "development", Kind: "advance"},
			{Key: "dev_done", Name: "Dev done", From: "development", To: "testing", Kind: "advance", Gates: pr("pull_request")},
			{Key: "testing_passed", Name: "Tested", From: "testing", To: "merge", Kind: "advance"},
			{Key: "merge_ready", Name: "Merge ready", From: "merge", To: "release", Kind: "advance", Gates: pr("pr_open")},
			{Key: "release_failed", Name: "Release failed", From: "release", To: "development", Kind: "reject"},
			{Key: "released", Name: "Released", From: "release", To: "done", Kind: "advance"},
			{Key: "cancel", Name: "Cancel", To: "cancelled", Kind: "cancel"},
		},
	}
}

// AC-6: повторный круг после возврата из выкладки при уже слитом PR. Со
// states проходит без waived и без нового PR; с require_open тот же переход
// отбит причиной «нет pull_request».
func TestReturnLoopWithMergedPR(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, config string
		passes       bool
	}{
		{"states", `{"ref_type":"pull_request","per_repository":true,"states":["open","merged"]}`, true},
		{"require_open", `{"ref_type":"pull_request","per_repository":true,"require_open":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
			report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
				Version: BundleVersion, Workflows: []BundleWorkflow{returnCycle(tc.config)}}, false)
			if err != nil {
				t.Fatalf("импорт цикла: %v", err)
			}
			if len(report.Validations) != 1 || !report.Validations[0].Valid {
				t.Fatalf("цикл не прошёл проверку: %+v", report.Validations)
			}
			id, err := uuid.Parse(report.Workflows[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := authoring.Activate(ctx, id, system.ID); err != nil {
				t.Fatalf("активация: %v", err)
			}

			if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/a"}); err != nil {
				t.Fatal(err)
			}
			task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
				CreateTaskInput: postgres.CreateTaskInput{Title: "Возврат после выкладки", Repositories: []string{"acme/a"}}})
			if err != nil {
				t.Fatal(err)
			}
			move := func(transition string) TransitionResult {
				t.Helper()
				res, err := engine.Execute(ctx, task.Key, transition, system.ID, "")
				if err != nil {
					t.Fatalf("%s: %v", transition, err)
				}
				return res
			}
			passed := func(transition string) {
				t.Helper()
				if res := move(transition); res.Blocked {
					t.Fatalf("%s заблокирован: %+v", transition, res.Gates)
				}
			}

			// Первый круг: PR открыт, потом слит при выкладке.
			passed("start")
			bindPR(t, ctx, store, task, "acme/a", "106", "open")
			passed("dev_done")
			passed("testing_passed")
			passed("merge_ready")
			if _, err := store.DB().Exec(ctx, `
				UPDATE task_repo_refs SET state = 'merged' WHERE task_id = $1`, task.ID); err != nil {
				t.Fatal(err)
			}
			passed("release_failed")

			// Второй круг: PR слит, нового нет.
			res := move("dev_done")
			if !tc.passes {
				if !res.Blocked {
					t.Fatal("с require_open слитый PR не должен пускать дальше")
				}
				current, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
				if err != nil {
					t.Fatal(err)
				}
				out := gateOutcome(t, ctx, engine, current, "dev_done", "pull_request")
				if out.Status != domain.GateResultFailed || !strings.Contains(out.Reason, "нет pull_request") {
					t.Fatalf("причина отказа: %+v", out)
				}
				return
			}
			if res.Blocked {
				t.Fatalf("dev_done со слитым PR заблокирован: %+v", res.Gates)
			}
			passed("testing_passed")
			passed("merge_ready")
			passed("released")
		})
	}
}

// AC-5 по настоящему импорту: черновик с require_open и states вместе ложится,
// но проверка цикла возвращает проблему с ключами перехода и гейта, а
// активация отклоняется.
func TestRepoRefStatesConflictDraftIsNotActivated(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{
			returnCycle(`{"require_open":true,"states":["open","merged"]}`)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Validations) != 1 || report.Validations[0].Valid {
		t.Fatalf("противоречивая настройка прошла проверку: %+v", report.Validations)
	}
	var found bool
	for _, p := range report.Validations[0].Problems {
		if p.Code == "repo_ref_states_invalid" && p.Transition == "dev_done" && p.Gate == "pull_request" {
			found = true
		}
	}
	if !found {
		t.Fatalf("нет проблемы repo_ref_states_invalid: %+v", report.Validations[0].Problems)
	}
	id, _ := uuid.Parse(report.Workflows[0].ID)
	if _, err := authoring.Activate(ctx, id, system.ID); err == nil {
		t.Fatal("цикл с require_open и states активирован")
	}
	head, err := store.WorkflowHead(ctx, store.DB(), id)
	if err != nil || head.State != domain.WorkflowDraft {
		t.Fatalf("черновик сменил состояние: %+v %v", head, err)
	}
}
