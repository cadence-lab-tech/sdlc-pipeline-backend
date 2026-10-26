package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// CLAB-219: состояние учёта прогона одним определением и политика проекта
// «захват без учёта запрещён». Номера в комментариях — пункты критериев
// приёмки задачи.

type policyWorld struct {
	t      *testing.T
	ctx    context.Context
	store  *postgres.Store
	engine *Engine
	system domain.Principal
	human  domain.Principal
	a, b   domain.Principal
	pid    uuid.UUID
}

func newPolicyWorld(t *testing.T) policyWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	w := policyWorld{t: t, ctx: ctx, store: store, engine: engine, system: system}
	var err error
	if w.human, err = store.ResolveActor(ctx, store.DB(), "pol-human", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	if w.a, err = store.ResolveActor(ctx, store.DB(), "pol-agent-a", domain.PrincipalAgent); err != nil {
		t.Fatal(err)
	}
	if w.b, err = store.ResolveActor(ctx, store.DB(), "pol-agent-b", domain.PrincipalAgent); err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	w.pid = project.ID
	return w
}

// task — задача на статусе с ролью, никем не забранная.
func (w policyWorld) task(title string) domain.Task {
	w.t.Helper()
	task, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: title},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	res, err := w.engine.Execute(w.ctx, task.Key, "start", w.system.ID, "")
	if err != nil || res.Blocked {
		w.t.Fatalf("в работу: %v %+v", err, res.Gates)
	}
	return res.Task
}

func (w policyWorld) claim(key string, actor domain.Principal) (ClaimResult, error) {
	return w.engine.Claim(w.ctx, key, actor.ID, "test")
}

func (w policyWorld) mustClaim(key string, actor domain.Principal) ClaimResult {
	w.t.Helper()
	res, err := w.claim(key, actor)
	if err != nil {
		w.t.Fatalf("захват %s принципалом %s: %v", key, actor.Handle, err)
	}
	return res
}

// close закрывает прогон так, будто его закрыл переход ago назад: часы
// сдвигаются в базе, ждать окно в тесте незачем.
func (w policyWorld) close(run uuid.UUID, ago string) {
	w.t.Helper()
	if _, err := w.store.DB().Exec(w.ctx, `
		UPDATE stage_runs SET state = 'passed', outcome = 'advanced',
		       finished_at = now() - $2::interval,
		       claimed_by = NULL, claimed_at = NULL, lease_expires_at = NULL
		WHERE id = $1`, run, ago); err != nil {
		w.t.Fatal(err)
	}
}

// debtRun — прогон задачи, забранный actor и закрытый ago назад без учёта.
func (w policyWorld) debtRun(title string, actor domain.Principal, ago string) (domain.Task, uuid.UUID) {
	w.t.Helper()
	task := w.task(title)
	res := w.mustClaim(task.Key, actor)
	w.close(res.StageRunID, ago)
	return task, res.StageRunID
}

func (w policyWorld) set(in UsagePolicyInput) UsagePolicyChange {
	w.t.Helper()
	out, err := w.engine.SetUsagePolicy(w.ctx, "SDLC", w.human.ID, in)
	if err != nil {
		w.t.Fatalf("политика %+v: %v", in, err)
	}
	return out
}

// enabledAgo сдвигает момент включения в прошлое: прогоны, закрытые
// «давно», должны попасть после него.
func (w policyWorld) enabledAgo(ago string) {
	w.t.Helper()
	if _, err := w.store.DB().Exec(w.ctx, `
		UPDATE projects SET settings = jsonb_set(settings, '{usage_policy,enabled_at}',
		       to_jsonb(now() - $2::interval))
		WHERE id = $1`, w.pid, ago); err != nil {
		w.t.Fatal(err)
	}
}

func usageRefused(t *testing.T, err error) *apperr.Error {
	t.Helper()
	if err == nil {
		t.Fatal("захват прошёл, ожидали отказ usage_report_required")
	}
	e := apperr.As(err)
	if e.Code != apperr.CodeUsageReportRequired {
		t.Fatalf("код %s, ожидали usage_report_required: %v", e.Code, err)
	}
	return e
}

// Пункты 1–3, 6: одно определение на карточку, стоимость задачи и метрики.
func TestUsageStateOneDefinition(t *testing.T) {
	w := newPolicyWorld(t)

	before := w.costTotals()
	task := w.task("Четыре прогона")
	start, err := w.store.CurrentStageRun(w.ctx, w.store.DB(), task.ID, task.StatusID)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := w.store.ListStageRuns(w.ctx, w.store.DB(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// (а) прогон начального статуса без роли — уже есть, закрыт переходом.
	var noRole uuid.UUID
	for _, r := range runs {
		if r.RoleID == nil {
			noRole = r.ID
		}
	}
	if noRole == uuid.Nil {
		t.Fatalf("нет прогона без роли: %+v", runs)
	}
	// (б) прогон роли, ни разу не взятый, — текущий этап до захвата.
	states := func() map[uuid.UUID]string {
		t.Helper()
		got, err := w.store.RunUsageStates(w.ctx, w.store.DB(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if st := states(); st[noRole] != domain.UsageStateNotApplicable || st[start.ID] != domain.UsageStateNotApplicable {
		t.Fatalf("до захвата: %v", st)
	}

	// 3: в работе без строки — missing; после строки — reported.
	w.mustClaim(task.Key, w.a)
	if st := states()[start.ID]; st != domain.UsageStateMissing {
		t.Fatalf("в работе без учёта: %s", st)
	}
	// 2: отпущен и снова pending — по-прежнему missing.
	if err := w.engine.Release(w.ctx, start.ID, w.a.ID, "прервался"); err != nil {
		t.Fatal(err)
	}
	if st := states()[start.ID]; st != domain.UsageStateMissing {
		t.Fatalf("после release: %s", st)
	}
	run, err := w.store.GetStageRun(w.ctx, w.store.DB(), start.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "pending" || run.ClaimedBy != nil {
		t.Fatalf("после release прогон %s, держит %v", run.State, run.ClaimedBy)
	}

	// (г) — текущий: строка учёта без стоимости, признак подписки.
	w.mustClaim(task.Key, w.a)
	if _, err := w.engine.ReportUsage(w.ctx, task.Key, w.a.ID, ReportUsageInput{Model: "m", Billing: "subscription"}); err != nil {
		t.Fatal(err)
	}
	// (в) забранный и закрытый без строки — missing: заводим его следующей попыткой.
	var closed uuid.UUID
	if err := w.store.DB().QueryRow(w.ctx, `
		INSERT INTO stage_runs (task_id, status_id, role_id, attempt, state, finished_at, outcome)
		SELECT task_id, status_id, role_id, attempt + 1, 'passed', now(), 'advanced'
		FROM stage_runs WHERE id = $1 RETURNING id`, start.ID).Scan(&closed); err != nil {
		t.Fatal(err)
	}
	if err := w.store.AppendEvent(w.ctx, w.store.DB(), task.ID, &closed, domain.EventStageClaimed, w.b.ID, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	st := states()
	want := map[uuid.UUID]string{
		noRole: domain.UsageStateNotApplicable, closed: domain.UsageStateMissing,
		start.ID: domain.UsageStateReported,
	}
	for id, s := range want {
		if st[id] != s {
			t.Errorf("прогон %s: %s, ожидали %s", id, st[id], s)
		}
	}

	// 1: карточка — то же состояние, а reported прежний.
	card, err := w.store.CardStageRuns(w.ctx, w.store.DB(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	for _, r := range card {
		if r.Usage.State != st[r.ID] {
			t.Errorf("карточка %s: %s, запрос состояний %s", r.ID, r.Usage.State, st[r.ID])
		}
		if r.Usage.Reported != (r.Usage.State == domain.UsageStateReported) {
			t.Errorf("карточка %s: reported %v при state %s", r.ID, r.Usage.Reported, r.Usage.State)
		}
		if r.Usage.State == domain.UsageStateMissing {
			missing++
		}
	}

	// 6: TaskUsage и CostMetrics считают тем же определением.
	usage, err := w.engine.TaskUsage(w.ctx, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if usage.SilentRuns != missing || usage.SilentRuns != 1 || usage.ReportedRuns != 1 {
		t.Errorf("TaskUsage: silent %d reported %d, в карточке missing %d", usage.SilentRuns, usage.ReportedRuns, missing)
	}
	// В сиде проекта свои прогоны: сравнивается прирост за эту задачу.
	after := w.costTotals()
	if d := after.minus(before); d != (costTotals{silentByStatus: 1, silentByRole: 1, reportedByRole: 1, runsByStatus: 3}) {
		t.Errorf("CostMetrics за задачу: %+v (runs прежние — все прогоны, не только с учётом)", d)
	}
	if after.overflow {
		t.Errorf("CostMetrics: reported_runs + silent_runs > runs в каком-то разрезе")
	}
}

type costTotals struct {
	silentByStatus, silentByRole, reportedByRole, runsByStatus int
	overflow                                                   bool
}

func (a costTotals) minus(b costTotals) costTotals {
	return costTotals{
		silentByStatus: a.silentByStatus - b.silentByStatus, silentByRole: a.silentByRole - b.silentByRole,
		reportedByRole: a.reportedByRole - b.reportedByRole, runsByStatus: a.runsByStatus - b.runsByStatus,
	}
}

func (w policyWorld) costTotals() costTotals {
	w.t.Helper()
	metrics, err := w.store.CostMetrics(w.ctx, w.store.DB(), w.pid, time.Now().AddDate(-10, 0, 0))
	if err != nil {
		w.t.Fatal(err)
	}
	var out costTotals
	for _, m := range metrics {
		if m.Reported+m.Silent > m.Runs {
			out.overflow = true
		}
		switch m.Dimension {
		case "status":
			out.silentByStatus += m.Silent
			out.runsByStatus += m.Runs
		case "role":
			out.silentByRole += m.Silent
			out.reportedByRole += m.Reported
		}
	}
	return out
}

// Пункты 8, 11, 12, 10 (вид принципала): настройка политики.
func TestUsagePolicySettings(t *testing.T) {
	w := newPolicyWorld(t)

	// 8: по умолчанию выключена, окно не задано, действует 60.
	view, err := w.engine.UsagePolicy(w.ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if view.Mode != domain.UsagePolicyOff || view.GraceMinutes != nil || view.EffectiveGraceMinutes != 60 || view.EnabledAt != nil {
		t.Fatalf("по умолчанию: %+v", view)
	}
	_, _ = w.debtRun("Старый долг", w.a, "3 days")
	res := w.mustClaim(w.task("Без политики").Key, w.a)
	if res.UsageReport != UsageReportExpected {
		t.Fatalf("usage_report без политики: %q", res.UsageReport)
	}

	// 10: агентский принципал политику не меняет.
	if _, err := w.engine.SetUsagePolicy(w.ctx, "SDLC", w.a.ID, UsagePolicyInput{Mode: ptr("off")}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент меняет политику: %v", err)
	}

	// 9, 12: включение ставит момент; required поверх required его не двигает.
	on := w.set(UsagePolicyInput{Mode: ptr("required")})
	if on.Before.Mode != "off" || on.After.Mode != "required" || on.After.EnabledAt == nil {
		t.Fatalf("включение: %+v", on)
	}
	again := w.set(UsagePolicyInput{Mode: ptr("required")})
	if !again.After.EnabledAt.Equal(*on.After.EnabledAt) {
		t.Fatalf("повторное required сдвинуло момент: %v → %v", on.After.EnabledAt, again.After.EnabledAt)
	}
	off := w.set(UsagePolicyInput{Mode: ptr("off")})
	if off.After.EnabledAt != nil {
		t.Fatalf("выключение оставило момент: %+v", off.After)
	}
	w.enabledAgo("1 hour") // чтобы новый момент заведомо отличался
	reon := w.set(UsagePolicyInput{Mode: ptr("required")})
	if reon.After.EnabledAt == nil || !reon.After.EnabledAt.After(*on.After.EnabledAt) {
		t.Fatalf("повторное включение: было %v, стало %v", on.After.EnabledAt, reon.After.EnabledAt)
	}

	// 11: окно.
	for _, n := range []int{0, 30} {
		got := w.set(UsagePolicyInput{SetGrace: true, Grace: ptr(n)})
		if got.After.GraceMinutes == nil || *got.After.GraceMinutes != n || got.After.EffectiveGraceMinutes != n {
			t.Fatalf("окно %d: %+v", n, got.After)
		}
	}
	for _, bad := range []int{-1, domain.MaxUsageGraceMinutes + 1} {
		if _, err := w.engine.SetUsagePolicy(w.ctx, "SDLC", w.human.ID, UsagePolicyInput{SetGrace: true, Grace: ptr(bad)}); apperr.As(err).Code != apperr.CodeInvalidInput ||
			!strings.Contains(err.Error(), "от 0 до 10080") {
			t.Fatalf("окно %d: %v", bad, err)
		}
	}
	for _, raw := range []string{`1.5`, `"abc"`, `-1.0`, `true`} {
		if _, err := ParseUsageGrace(json.RawMessage(raw)); apperr.As(err).Code != apperr.CodeInvalidInput {
			t.Fatalf("окно %s: %v", raw, err)
		}
	}
	for _, raw := range []string{`null`, `"default"`} {
		if g, err := ParseUsageGrace(json.RawMessage(raw)); err != nil || g != nil {
			t.Fatalf("окно %s: %v %v", raw, g, err)
		}
	}
	if _, err := w.engine.SetUsagePolicy(w.ctx, "SDLC", w.human.ID, UsagePolicyInput{Mode: ptr("strict")}); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("неверный режим: %v", err)
	}
	if _, err := w.engine.SetUsagePolicy(w.ctx, "SDLC", w.human.ID, UsagePolicyInput{}); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("пустая правка: %v", err)
	}
	view, _ = w.engine.UsagePolicy(w.ctx, "SDLC")
	if *view.GraceMinutes != 30 || view.Mode != "required" {
		t.Fatalf("неверный ввод изменил настройки: %+v", view)
	}
	def := w.set(UsagePolicyInput{SetGrace: true})
	if def.After.GraceMinutes != nil || def.After.EffectiveGraceMinutes != 60 || def.After.Mode != "required" {
		t.Fatalf("default: %+v", def.After)
	}

	// Остальные ключи настроек правка не трогает.
	if _, err := w.store.DB().Exec(w.ctx, `UPDATE projects SET settings = settings || '{"other": 1}' WHERE id = $1`, w.pid); err != nil {
		t.Fatal(err)
	}
	w.set(UsagePolicyInput{Mode: ptr("off")})
	var other int
	if err := w.store.DB().QueryRow(w.ctx, `SELECT (settings->>'other')::int FROM projects WHERE id = $1`, w.pid).Scan(&other); err != nil || other != 1 {
		t.Fatalf("чужой ключ настроек: %d %v", other, err)
	}
}

// Пункты 13, 15–23: отказ захвата по политике и его границы.
func TestUsagePolicyRefusesAgentWithDebt(t *testing.T) {
	w := newPolicyWorld(t)
	w.set(UsagePolicyInput{Mode: ptr("required"), SetGrace: true, Grace: ptr(0)})
	w.enabledAgo("1 day")

	// 17: закрыт до включения — не долг.
	w.debtRun("До включения", w.a, "2 days")
	y := w.task("Следующая")
	res := w.mustClaim(y.Key, w.a)
	if res.UsageReport != UsageReportRequired {
		t.Fatalf("usage_report при политике: %q", res.UsageReport)
	}
	if err := w.engine.Release(w.ctx, res.StageRunID, w.a.ID, ""); err != nil {
		t.Fatal(err)
	}

	// 13: закрыт после включения, учёта нет — отказ с ключом и прогоном.
	x, debt := w.debtRun("Без отчёта", w.a, "1 hour")
	before, err := w.store.ListEvents(w.ctx, w.store.DB(), y.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.claim(y.Key, w.a)
	e := usageRefused(t, err)
	if !strings.Contains(e.Message, x.Key) || !strings.Contains(e.Message, debt.String()) ||
		!strings.Contains(e.Message, "clab usage "+x.Key+" --run "+debt.String()) {
		t.Fatalf("в тексте отказа нет задачи, прогона или способа погасить: %s", e.Message)
	}
	details, _ := json.Marshal(e.Details)
	var det struct {
		Debts []struct {
			Task       string `json:"task"`
			StageRunID string `json:"stage_run_id"`
			FinishedAt string `json:"finished_at"`
		} `json:"debts"`
		Total        int `json:"total"`
		GraceMinutes int `json:"grace_minutes"`
	}
	if err := json.Unmarshal(details, &det); err != nil {
		t.Fatal(err)
	}
	if det.Total != 1 || len(det.Debts) != 1 || det.Debts[0].Task != x.Key ||
		det.Debts[0].StageRunID != debt.String() || det.Debts[0].FinishedAt == "" || det.GraceMinutes != 0 {
		t.Fatalf("details: %s", details)
	}
	after, err := w.store.ListEvents(w.ctx, w.store.DB(), y.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("отказ оставил события: было %d, стало %d", len(before), len(after))
	}
	yRun, err := w.store.CurrentStageRun(w.ctx, w.store.DB(), y.ID, y.StatusID)
	if err != nil {
		t.Fatal(err)
	}
	if yRun.ClaimedBy != nil || yRun.State != "pending" {
		t.Fatalf("после отказа прогон взят: %s %v", yRun.State, yRun.ClaimedBy)
	}

	// 19: человек с тем же долгом забирает, ему — expected.
	hx, _ := w.debtRun("Долг человека", w.human, "1 hour")
	_ = hx
	hres := w.mustClaim(w.task("Человеку").Key, w.human)
	if hres.UsageReport != UsageReportExpected {
		t.Fatalf("usage_report человеку: %q", hres.UsageReport)
	}

	// 23: долг в этом проекте другой проект не видит.
	project, err := w.store.GetProjectByKey(w.ctx, w.store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	next, err := w.engine.CreateProject(w.ctx, w.system.ID, postgres.CreateProjectInput{
		Key: "NEXT", Name: "Соседний", OwnerTeamID: *project.OwnerTeamID})
	if err != nil {
		t.Fatal(err)
	}
	enabled := *mustPolicy(t, w).EnabledAt
	if _, total, err := w.store.UsageDebts(w.ctx, w.store.DB(), next.ID, w.a.ID, enabled, 0, 20); err != nil || total != 0 {
		t.Fatalf("долг в чужом проекте: %d %v", total, err)
	}

	// 15: отчёт о закрытом прогоне по --run без стоимости гасит долг.
	if _, err := w.engine.ReportUsage(w.ctx, x.Key, w.a.ID, ReportUsageInput{Model: "m", StageRunID: &debt}); err != nil {
		t.Fatalf("отчёт о закрытом прогоне: %v", err)
	}
	res = w.mustClaim(y.Key, w.a)
	if res.UsageReport != UsageReportRequired {
		t.Fatalf("usage_report после отчёта: %q", res.UsageReport)
	}

	// 20: долг появился, пока A держит Y, — подтверждение аренды проходит,
	// и токеном задачи тоже.
	w.debtRun("Пока держит", w.a, "1 hour")
	if _, err := w.claim(y.Key, w.a); err != nil {
		t.Fatalf("подтверждение своей аренды при долге: %v", err)
	}
	run := res.StageRunID
	if _, err := w.engine.ClaimWith(w.ctx, y.Key, w.a.ID, "token", ClaimOptions{OnlyRun: &run}); err != nil {
		t.Fatalf("захват токеном задачи при долге: %v", err)
	}
	// Новая аренда — отказ.
	_, err = w.claim(w.task("Новая").Key, w.a)
	usageRefused(t, err)

	// 22: выключение снимает отказ сразу.
	w.set(UsagePolicyInput{Mode: ptr("off")})
	res = w.mustClaim(w.task("После выключения").Key, w.a)
	if res.UsageReport != UsageReportExpected {
		t.Fatalf("usage_report после выключения: %q", res.UsageReport)
	}
}

func mustPolicy(t *testing.T, w policyWorld) domain.UsagePolicy {
	t.Helper()
	p, err := w.store.ProjectUsagePolicy(w.ctx, w.store.DB(), w.pid, false)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Пункты 16, 18, 21: окно, незакрытый прогон и учёт на прогоне, а не на
// принципале.
func TestUsagePolicyGraceAndSharedRuns(t *testing.T) {
	w := newPolicyWorld(t)
	w.set(UsagePolicyInput{Mode: ptr("required")}) // окно по умолчанию — 60
	w.enabledAgo("1 day")

	// 16: закрыт 10 минут назад — в окне.
	w.debtRun("Свежий", w.a, "10 minutes")
	res := w.mustClaim(w.task("В окне").Key, w.a)
	if err := w.engine.Release(w.ctx, res.StageRunID, w.a.ID, ""); err != nil {
		t.Fatal(err)
	}
	// 18: вернулся в pending после release и не закрыт — не долг.
	open := w.task("Отпущенный")
	held := w.mustClaim(open.Key, w.b)
	if err := w.engine.Release(w.ctx, held.StageRunID, w.b.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Протухание аренды — тоже pending без закрытия.
	expired := w.task("Протухший")
	lost := w.mustClaim(expired.Key, w.b)
	if _, err := w.store.DB().Exec(w.ctx, `UPDATE stage_runs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, lost.StageRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.ExpireLeases(w.ctx, w.system.ID, 100); err != nil {
		t.Fatal(err)
	}
	w.mustClaim(w.task("B без долга").Key, w.b)

	// 21: прогон забирали A и B, отчитался B — долга нет ни у кого.
	shared := w.task("Общий")
	first := w.mustClaim(shared.Key, w.a)
	if err := w.engine.Release(w.ctx, first.StageRunID, w.a.ID, ""); err != nil {
		t.Fatal(err)
	}
	w.mustClaim(shared.Key, w.b)
	w.close(first.StageRunID, "2 hours")
	enabled := *mustPolicy(t, w).EnabledAt
	if _, total, _ := w.store.UsageDebts(w.ctx, w.store.DB(), w.pid, w.b.ID, enabled, 60, 20); total != 1 {
		t.Fatalf("долг B до отчёта: %d", total)
	}
	runID := first.StageRunID
	if _, err := w.engine.ReportUsage(w.ctx, shared.Key, w.b.ID, ReportUsageInput{Model: "m", StageRunID: &runID}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []domain.Principal{w.a, w.b} {
		debts, _, err := w.store.UsageDebts(w.ctx, w.store.DB(), w.pid, p.ID, enabled, 60, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range debts {
			if d.StageRunID == runID {
				t.Errorf("у %s остался долг по общему прогону", p.Handle)
			}
		}
	}

	// 16: прогон, закрытый 61 минуту назад, — долг.
	stale, _ := w.debtRun("Несвежий", w.a, "61 minutes")
	_, err := w.claim(w.task("За окном").Key, w.a)
	if e := usageRefused(t, err); !strings.Contains(e.Message, stale.Key) {
		t.Fatalf("в отказе нет %s: %s", stale.Key, e.Message)
	}
}

// Список долгов ограничен, total — полное число.
func TestUsageDebtsLimit(t *testing.T) {
	w := newPolicyWorld(t)
	for i := 0; i < 3; i++ {
		w.debtRun("Долг", w.a, "1 hour")
	}
	w.set(UsagePolicyInput{Mode: ptr("required"), SetGrace: true, Grace: ptr(0)})
	w.enabledAgo("1 day")
	enabled := *mustPolicy(t, w).EnabledAt
	debts, total, err := w.store.UsageDebts(w.ctx, w.store.DB(), w.pid, w.a.ID, enabled, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(debts) != 2 || total != 3 {
		t.Fatalf("долгов %d из %d, ожидали 2 из 3", len(debts), total)
	}
	e := apperr.As(usageRefusal("SDLC-1", debts, total, 0))
	if !strings.Contains(e.Message, "и ещё 1") {
		t.Fatalf("в тексте нет остатка: %s", e.Message)
	}
}
