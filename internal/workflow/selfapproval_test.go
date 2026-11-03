package workflow

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Чистая часть расчёта (CLAB-380, AC-17, AC-C4): порядок оснований
// фиксирован, пустой набор — [] в JSON, не null; вид токена без значения —
// none.
func TestSelfBasisPure(t *testing.T) {
	for _, c := range []struct {
		lease, artifact bool
		want            []string
	}{
		{false, false, []string{}},
		{true, false, []string{SelfBasisLease}},
		{false, true, []string{SelfBasisArtifact}},
		{true, true, []string{SelfBasisLease, SelfBasisArtifact}},
	} {
		got := selfBasis(c.lease, c.artifact)
		if !slices.Equal(got, c.want) || got == nil {
			t.Fatalf("selfBasis(%v,%v) = %#v, ждали %#v", c.lease, c.artifact, got, c.want)
		}
	}
	if tokenKindOf(Actor{}) != TokenNone || tokenKindOf(Actor{TokenKind: domain.TokenSession}) != domain.TokenSession {
		t.Fatal("tokenKindOf")
	}
	// Типы в JSON: bool, массив строк, строки.
	raw, err := json.Marshal(decisionFields(Actor{Level: domain.MemberAgent},
		StageVerdict{Via: ViaStageRole}, selfBasis(false, false)))
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if back["self_approval"] != false || back["token_kind"] != "none" || back["stage_role"] != "" ||
		back["level"] != "agent" || back["via"] != "stage_role" {
		t.Fatalf("поля: %s", raw)
	}
	if basis, ok := back["self_basis"].([]any); !ok || len(basis) != 0 {
		t.Fatalf("self_basis не пустой массив: %s", raw)
	}
}

func (w rightsWorld) lastEvent(task domain.Task, typ string) map[string]any {
	w.t.Helper()
	var raw []byte
	if err := w.store.DB().QueryRow(w.ctx, `
		SELECT payload FROM task_events WHERE task_id = $1 AND type = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, task.ID, typ).Scan(&raw); err != nil {
		w.t.Fatalf("событие %s: %v", typ, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		w.t.Fatal(err)
	}
	return out
}

func (w rightsWorld) eventCount(task domain.Task, typ string) int {
	w.t.Helper()
	var n int
	if err := w.store.DB().QueryRow(w.ctx,
		`SELECT count(*) FROM task_events WHERE task_id = $1 AND type = $2`, task.ID, typ).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func strs(v any) []string {
	out := []string{}
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// AC-16, AC-17, AC-C3, AC-C4: поля gate.decided для основания × вердикт ×
// уровень × вид токена; прежние ключи на месте; режим без токена — none.
func TestGateDecidedSelfApprovalFields(t *testing.T) {
	w := newRightsWorld(t)
	colleague, err := w.store.ResolveActor(w.ctx, w.store.DB(), "colleague", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{"analyst"}
	agentLead := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: roles, TokenKind: domain.TokenAPI}
	agentColleague := Actor{Principal: colleague, Level: domain.MemberAgent, Roles: roles, TokenKind: domain.TokenSession}
	maintLead := Actor{Principal: w.lead, Level: domain.MemberMaintainer, TokenKind: domain.TokenAPI}
	maintColleague := Actor{Principal: colleague, Level: domain.MemberMaintainer, TokenKind: domain.TokenAPI}

	claim := func(task domain.Task, release bool) {
		t.Helper()
		if _, err := w.engine.Claim(w.ctx, task.Key, w.lead.ID, "test"); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if release {
			if err := w.engine.Release(w.ctx, w.run(task).ID, w.lead.ID, ""); err != nil {
				t.Fatalf("release: %v", err)
			}
		}
	}
	submit := func(task domain.Task, by uuid.UUID, title string) {
		t.Helper()
		if _, err := w.engine.SubmitArtifact(w.ctx, task.Key, by, SubmitArtifactInput{
			Kind: "analysis.brief", Title: title, Content: []byte("# " + title)}); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	cases := []struct {
		name   string
		setup  func(domain.Task)
		who    Actor
		status string
		via    string
		token  string
		level  string
		self   bool
		basis  []string
	}{
		{"agent без оснований", func(domain.Task) {}, agentColleague, domain.GateResultPassed,
			ViaStageRole, "session", "agent", false, []string{}},
		{"agent: аренда снята", func(tk domain.Task) { claim(tk, true) }, agentLead, domain.GateResultPassed,
			ViaStageRole, "api", "agent", true, []string{"lease"}},
		{"agent: аренда держится", func(tk domain.Task) { claim(tk, false) }, agentLead, domain.GateResultPassed,
			ViaStageRole, "api", "agent", true, []string{"lease"}},
		{"agent: артефакт без аренды", func(tk domain.Task) { submit(tk, w.lead.ID, "a") }, agentLead,
			domain.GateResultPassed, ViaStageRole, "api", "agent", true, []string{"artifact"}},
		{"agent: оба основания", func(tk domain.Task) { claim(tk, true); submit(tk, w.lead.ID, "a") }, agentLead,
			domain.GateResultPassed, ViaStageRole, "api", "agent", true, []string{"lease", "artifact"}},
		{"agent: вердикт failed тоже", func(tk domain.Task) { submit(tk, w.lead.ID, "a") }, agentLead,
			domain.GateResultFailed, ViaStageRole, "api", "agent", true, []string{"artifact"}},
		{"agent: чужой артефакт и аренда коллеги", func(tk domain.Task) { submit(tk, w.lead.ID, "a") }, agentColleague,
			domain.GateResultPassed, ViaStageRole, "session", "agent", false, []string{}},
		{"agent: заменённая версия", func(tk domain.Task) { submit(tk, w.lead.ID, "v1"); submit(tk, colleague.ID, "v2") },
			agentLead, domain.GateResultPassed, ViaStageRole, "api", "agent", true, []string{"artifact"}},
		{"maintainer с арендой", func(tk domain.Task) { claim(tk, false) }, maintLead, domain.GateResultPassed,
			ViaLevel, "api", "maintainer", true, []string{"lease"}},
		{"maintainer без оснований", func(tk domain.Task) { claim(tk, false) }, maintColleague,
			domain.GateResultPassed, ViaLevel, "api", "maintainer", false, []string{}},
		{"актор без токена (заголовок)", func(domain.Task) {},
			Actor{Principal: colleague, Level: domain.MemberAdmin}, domain.GateResultPassed,
			ViaLevel, "none", "admin", false, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := w.task("analysis")
			c.setup(task)
			if _, err := w.engine.DecideGateAs(w.ctx, task.Key, "analysis_done", "human_approval", c.who,
				c.status, json.RawMessage(`{"note":"x"}`), nil); err != nil {
				t.Fatal(err)
			}
			ev := w.lastEvent(task, domain.EventGateDecided)
			// Прежние ключи и значения (AC-16.1).
			if ev["gate"] != "human_approval" || ev["transition"] != "analysis_done" || ev["status"] != c.status ||
				ev["gate_id"] == "" || ev["details"].(map[string]any)["note"] != "x" {
				t.Fatalf("прежние ключи: %v", ev)
			}
			if ev["via"] != c.via || ev["token_kind"] != c.token || ev["level"] != c.level ||
				ev["stage_role"] != "analyst" || ev["self_approval"] != c.self || !slices.Equal(strs(ev["self_basis"]), c.basis) {
				t.Fatalf("поля: %v, ждали via=%s token=%s level=%s self=%v basis=%v", ev, c.via, c.token, c.level, c.self, c.basis)
			}
		})
	}

	// Артефакт прошлой попытки (другой stage_run_id) основания не даёт.
	t.Run("прошлая попытка", func(t *testing.T) {
		task := w.task("new")
		w.move(task.Key, "start")
		submit(task, w.lead.ID, "attempt 1")
		for _, g := range []string{"artifacts", "peer_review", "human_approval", "drift", "repo_scope"} {
			if _, err := w.engine.DecideGate(w.ctx, task.Key, "analysis_done", g, w.lead.ID, nil,
				domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
				t.Fatalf("waive %s: %v", g, err)
			}
		}
		w.move(task.Key, "analysis_done")
		if _, err := w.engine.DecideGate(w.ctx, task.Key, "reject_to_analysis", "problem_report", w.lead.ID, nil,
			domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		w.move(task.Key, "reject_to_analysis")
		back, _ := w.store.GetTaskByKey(w.ctx, w.store.DB(), task.Key)
		if back.StatusKey != "analysis" || w.run(back).Attempt < 2 {
			t.Fatalf("не вторая попытка: %s %d", back.StatusKey, w.run(back).Attempt)
		}
		if _, err := w.engine.DecideGateAs(w.ctx, task.Key, "analysis_done", "human_approval", agentLead,
			domain.GateResultPassed, json.RawMessage(`{}`), nil); err != nil {
			t.Fatal(err)
		}
		ev := w.lastEvent(back, domain.EventGateDecided)
		if ev["self_approval"] != false || len(strs(ev["self_basis"])) != 0 {
			t.Fatalf("артефакт прошлой попытки засчитан: %v", ev)
		}
	})

	// Токен задачи: чек-лист passed — via=task, token_kind=task, держатель
	// аренды — «свой» по аренде (R7).
	t.Run("токен задачи", func(t *testing.T) {
		task := w.task("architecture")
		claimed, err := w.engine.Claim(w.ctx, task.Key, w.lead.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		_ = claimed
		run := w.run(task)
		bound := Actor{Principal: w.lead, Level: domain.MemberMaintainer, TokenKind: domain.TokenTask,
			Bound: &TaskBinding{StageRunID: run.ID, TaskID: task.ID, TaskKey: task.Key}}
		if _, err := w.engine.DecideGateAs(w.ctx, task.Key, "architecture_done", "traceability", bound,
			domain.GateResultPassed, json.RawMessage(`{}`), nil); err != nil {
			t.Fatal(err)
		}
		ev := w.lastEvent(task, domain.EventGateDecided)
		if ev["via"] != ViaTask || ev["token_kind"] != "task" || ev["self_approval"] != true ||
			!slices.Equal(strs(ev["self_basis"]), []string{"lease"}) || ev["stage_role"] != "architect" {
			t.Fatalf("токен задачи: %v", ev)
		}
	})
}

// AC-18, AC-C1, AC-C6: task.advanced и task.rejected несут via и stage_role,
// прежние ключи на месте, self_approval нет; отказ 403 событий не пишет.
func TestTransitionEventsCarryVia(t *testing.T) {
	w := newRightsWorld(t)
	analyst := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}, TokenKind: domain.TokenAPI}
	other := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}, TokenKind: domain.TokenAPI}
	maintainer := Actor{Principal: w.lead, Level: domain.MemberMaintainer, TokenKind: domain.TokenAPI}

	task := w.task("analysis")
	for _, g := range []string{"artifacts", "peer_review", "human_approval", "drift", "repo_scope"} {
		if _, err := w.engine.DecideGate(w.ctx, task.Key, "analysis_done", g, w.lead.ID, nil,
			domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	before := w.eventCount(task, domain.EventTaskAdvanced)
	if _, err := w.engine.ExecuteAs(w.ctx, task.Key, "analysis_done", other, ""); !forbidden(err) {
		t.Fatalf("чужая роль: %v", err)
	}
	if w.eventCount(task, domain.EventTaskAdvanced) != before {
		t.Fatal("отказ записал task.advanced")
	}
	if _, err := w.engine.ExecuteAs(w.ctx, task.Key, "analysis_done", analyst, "готово"); err != nil {
		t.Fatal(err)
	}
	adv := w.lastEvent(task, domain.EventTaskAdvanced)
	if adv["via"] != ViaStageRole || adv["stage_role"] != "analyst" || adv["transition"] != "analysis_done" ||
		adv["from"] != "analysis" || adv["to"] != "architecture" || adv["comment"] != "готово" {
		t.Fatalf("task.advanced: %v", adv)
	}
	if _, ok := adv["self_approval"]; ok {
		t.Fatalf("self_approval на переходе: %v", adv)
	}

	// Возврат сопровождающим: via=level, роль статуса, откуда ушла задача.
	if _, err := w.engine.DecideGate(w.ctx, task.Key, "reject_to_analysis", "problem_report", w.lead.ID, nil,
		domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.ExecuteAs(w.ctx, task.Key, "reject_to_analysis", maintainer, ""); err != nil {
		t.Fatal(err)
	}
	rej := w.lastEvent(task, domain.EventTaskRejected)
	if rej["via"] != ViaLevel || rej["stage_role"] != "architect" || rej["from"] != "architecture" ||
		rej["to"] != "analysis" || rej["transition"] != "reject_to_analysis" {
		t.Fatalf("task.rejected: %v", rej)
	}
	if _, ok := rej["self_approval"]; ok {
		t.Fatalf("self_approval на возврате: %v", rej)
	}
}

// R4, R8: вид токена в Actor ставит Auth.Actor из Identity.Token; без
// токена (режим заголовка) — none, а не пустая строка.
func TestAuthActorTokenKind(t *testing.T) {
	w := newRightsWorld(t)
	log := testdb.Logger()
	task := w.task("new")
	for _, c := range []struct {
		name string
		mode string
		id   Identity
		want string
	}{
		{"заголовок", AuthHeader, Identity{Principal: w.lead}, TokenNone},
		{"api", AuthToken, Identity{Principal: w.lead, Token: &domain.AccessToken{TokenKind: domain.TokenAPI}}, domain.TokenAPI},
		{"сессия", AuthToken, Identity{Principal: w.lead, Token: &domain.AccessToken{TokenKind: domain.TokenSession}}, domain.TokenSession},
	} {
		a, err := NewAuth(w.store, log, c.mode).Actor(w.ctx, c.id, task.ProjectID)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if a.TokenKind != c.want {
			t.Fatalf("%s: вид токена %q, ждали %q", c.name, a.TokenKind, c.want)
		}
	}
}

// AC-20: апрув agent считается как апрув сопровождающего — два разных
// решающих дают CountApprovals = 2 (повтор того же — не третий), граница
// since действует как у апрува сопровождающего. Новые ключи payload подсчёт
// не затрагивают; устаревание после новой сдачи — TestStageAuthoritySeenAndStale.
func TestCountApprovalsWithAgentApprovals(t *testing.T) {
	w := newRightsWorld(t)
	colleague, err := w.store.ResolveActor(w.ctx, w.store.DB(), "colleague", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	a := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}, TokenKind: domain.TokenAPI}
	b := Actor{Principal: colleague, Level: domain.MemberAgent, Roles: []string{"analyst"}, TokenKind: domain.TokenAPI}
	task := w.task("analysis")
	run := w.run(task)
	transition, err := w.store.GetTransitionByKey(w.ctx, w.store.DB(), task.WorkflowID, "analysis_done")
	if err != nil {
		t.Fatal(err)
	}
	var gateID uuid.UUID
	for _, g := range transition.Gates {
		if g.Key == "human_approval" {
			gateID = g.ID
		}
	}
	for _, who := range []Actor{a, b, a} { // повтор a — тот же решающий
		if _, err := w.engine.DecideGateAs(w.ctx, task.Key, "analysis_done", "human_approval", who,
			domain.GateResultPassed, json.RawMessage(`{}`), nil); err != nil {
			t.Fatal(err)
		}
	}
	n, err := w.store.CountApprovals(w.ctx, w.store.DB(), run.ID, gateID, time.Time{})
	if err != nil || n != 2 {
		t.Fatalf("апрувов %d (%v), ждали 2 разных решающих", n, err)
	}
	n, err = w.store.CountApprovals(w.ctx, w.store.DB(), run.ID, gateID, time.Now().Add(time.Minute))
	if err != nil || n != 0 {
		t.Fatalf("апрувов до последней сдачи %d (%v), ждали 0", n, err)
	}
}
