package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Предложения версий ролей (CLAB-381, решение 0072 п. 5). Номера —
// критерии приёмки анализа CLAB-381.

type proposalWorld struct {
	t     *testing.T
	ctx   context.Context
	store *postgres.Store
	eng   *Engine
	a     *Authoring
	auth  *Auth
	admin Identity
}

func newProposalWorld(t *testing.T) proposalWorld {
	t.Helper()
	store, engine, a, auth, _ := setupPhase4(t, AuthToken)
	ctx := context.Background()
	return proposalWorld{t: t, ctx: ctx, store: store, eng: engine, a: a, auth: auth, admin: adminIdentity(t, ctx, auth)}
}

// member — участник проекта SDLC с уровнем и ролями участия и его личный
// токен, суженный tokenRoles (nil — без сужения).
func (w proposalWorld) member(raw string, level domain.MemberRole, roleKeys, tokenRoles []string) Identity {
	w.t.Helper()
	kind, handle := SplitHandle(raw, domain.PrincipalUser)
	if kind == domain.PrincipalUser {
		if _, err := w.store.ResolveActor(w.ctx, w.store.DB(), handle, kind); err != nil {
			w.t.Fatal(err)
		}
	}
	if _, err := w.auth.SetMember(w.ctx, w.admin, "SDLC", raw, level, roleKeys); err != nil {
		w.t.Fatal(err)
	}
	issued, err := w.auth.IssueToken(w.ctx, w.admin, IssueTokenInput{
		Name: "personal", Principal: raw, ProjectKey: "SDLC", RoleKeys: tokenRoles, MemberRole: level,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	id, err := w.auth.Authenticate(w.ctx, issued.Token)
	if err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w proposalWorld) count(sql string, args ...any) int {
	w.t.Helper()
	return countN(w.t, w.store, sql, args...)
}

func (w proposalWorld) latest(role string) int {
	w.t.Helper()
	return w.count(`SELECT coalesce(max(v.version), 0) FROM role_versions v JOIN roles r ON r.id = v.role_id
		JOIN projects p ON p.id = r.project_id WHERE p.key = 'SDLC' AND r.key = $1`, role)
}

// runVersion — номер версии роли, на которой открыт текущий прогон задачи.
func (w proposalWorld) runVersion(task string) int {
	w.t.Helper()
	return w.count(`SELECT rv.version FROM stage_runs sr JOIN role_versions rv ON rv.id = sr.role_version_id
		JOIN tasks t ON t.id = sr.task_id WHERE t.key = $1 AND sr.kind = 'stage'
		ORDER BY sr.entered_at DESC LIMIT 1`, task)
}

func (w proposalWorld) propose(id Identity, role string, in RoleVersionInput) (domain.RoleProposal, error) {
	return w.a.ProposeRoleVersion(w.ctx, w.auth, id, "SDLC", role, in)
}

func (w proposalWorld) mustPropose(id Identity, role string, in RoleVersionInput) domain.RoleProposal {
	w.t.Helper()
	p, err := w.propose(id, role, in)
	if err != nil {
		w.t.Fatalf("подача: %v", err)
	}
	return p
}

func simpleProposal(text string) RoleVersionInput {
	return RoleVersionInput{Instructions: text, Changelog: ptrTo("правка: " + text)}
}

func wantProposalCode(t *testing.T, err error, want apperr.Code, what string) {
	t.Helper()
	if code(err) != want {
		t.Fatalf("%s: ждали %s, а %v", what, want, err)
	}
}

// AC-1, AC-3, AC-4: подача кладёт pending от последней версии и не трогает
// role_versions; ссылка на библиотеку разрешается при подаче и закрепляется;
// негодное отбивается тем же текстом, что у прямой версии.
func TestRoleProposalSubmit(t *testing.T) {
	w := newProposalWorld(t)
	if _, err := w.a.CreateInstruction(w.ctx, "SDLC", w.admin.Principal.ID, InstructionInput{Key: "git", Name: "git", Content: gitV1}); err != nil {
		t.Fatal(err)
	}
	dev := w.member("user:dev", domain.MemberAgent, []string{"analyst"}, nil)
	n, versions := w.latest("analyst"), w.count(`SELECT count(*) FROM role_versions`)

	p := w.mustPropose(dev, "analyst", RoleVersionInput{
		Documents: []RoleDocumentInput{
			{Name: "main.md", Content: "Ты аналитик, версия из предложения.\n"},
			{Instruction: &InstructionRef{Key: "git", Version: 1}},
		},
		OutputContract: json.RawMessage(`[{"kind":"analysis.brief","required":true}]`),
		ModelLevel:     ptrTo("medium"), Changelog: ptrTo("  добавил git  "),
	})
	if p.State != domain.ProposalPending || p.BaseVersion != n || p.CreatedBy != dev.Principal.ID ||
		p.Role != "analyst" || p.Changelog != "добавил git" || p.ResultVersion != nil || p.DecidedBy != nil {
		t.Fatalf("предложение: %+v", p)
	}
	if len(p.Documents) != 2 || p.Documents[1].Name != "git.md" || p.Documents[1].Instruction == nil ||
		p.Documents[1].Instruction.Version != 1 || p.Documents[1].SHA256 != shaHex(gitV1) {
		t.Fatalf("документы предложения: %+v", p.Documents)
	}
	if w.latest("analyst") != n || w.count(`SELECT count(*) FROM role_versions`) != versions {
		t.Fatal("подача завела версию роли")
	}
	// Прогон этапа роли открывается на действующей версии, а не на предложении.
	if _, err := w.eng.Execute(w.ctx, "SDLC-1", "start", w.admin.Principal.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := w.runVersion("SDLC-1"); got != n {
		t.Fatalf("прогон на версии %d, ждали %d", got, n)
	}

	// AC-4: новая версия инструкции после подачи предложение не меняет.
	if _, err := w.a.AddInstructionVersion(w.ctx, "SDLC", "git", w.admin.Principal.ID, InstructionInput{Content: gitV2}); err != nil {
		t.Fatal(err)
	}
	got, err := w.a.GetRoleProposal(w.ctx, "SDLC", "analyst", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Documents[1].Instruction.Version != 1 || deref(got.Documents[1].Content) != gitV1 || got.Documents[1].SHA256 != shaHex(gitV1) {
		t.Fatalf("предложение после v2 инструкции: %+v", got.Documents[1])
	}
	list, err := w.a.ListRoleProposals(w.ctx, "SDLC", "analyst", "pending")
	if err != nil || len(list) != 1 || list[0].ID != p.ID || len(list[0].Documents) != 2 || list[0].Documents[0].Content != nil {
		t.Fatalf("список pending: %+v %v", list, err)
	}
	if list, err := w.a.ListRoleProposals(w.ctx, "SDLC", "analyst", "approved"); err != nil || len(list) != 0 {
		t.Fatalf("список approved: %+v %v", list, err)
	}
	_, err = w.a.ListRoleProposals(w.ctx, "SDLC", "analyst", "draft")
	wantProposalCode(t, err, apperr.CodeInvalidInput, "неизвестное состояние")

	// AC-3: отказы при подаче — те же тексты, что у прямой версии, строк нет.
	before := w.count(`SELECT count(*) FROM role_version_proposals`)
	for name, in := range map[string]RoleVersionInput{
		"оба поля":              {Instructions: "x", Documents: []RoleDocumentInput{{Name: "main.md", Content: "y"}}},
		"пустой набор":          {Documents: []RoleDocumentInput{}},
		"пустая инструкция":     {Instructions: "   "},
		"имя не по форме":       {Documents: []RoleDocumentInput{{Name: "Main.md", Content: "x"}}},
		"дубль имени":           {Documents: []RoleDocumentInput{{Name: "a.md", Content: "x"}, {Name: "a.md", Content: "y"}}},
		"контракт без kind":     {Instructions: "x", OutputContract: json.RawMessage(`[{"kind":" "}]`)},
		"уровень вне набора":    {Instructions: "x", ModelLevel: ptrTo("max")},
		"нет инструкции":        {Documents: []RoleDocumentInput{{Instruction: &InstructionRef{Key: "nope", Version: 1}}}},
		"нет версии инструкции": {Documents: []RoleDocumentInput{{Instruction: &InstructionRef{Key: "git", Version: 9}}}},
	} {
		in.Changelog = ptrTo("правка")
		_, perr := w.propose(dev, "analyst", in)
		_, verr := w.a.AddRoleVersion(w.ctx, "SDLC", "analyst", w.admin.Principal.ID, in)
		if code(perr) != apperr.CodeInvalidInput || verr == nil || apperr.As(perr).Message != apperr.As(verr).Message {
			t.Errorf("%s: подача %v, версия %v", name, perr, verr)
		}
	}
	for name, cl := range map[string]*string{"без changelog": nil, "пустой changelog": ptrTo(""), "пробельный changelog": ptrTo(" \n ")} {
		_, err := w.propose(dev, "analyst", RoleVersionInput{Instructions: "x", Changelog: cl})
		wantProposalCode(t, err, apperr.CodeInvalidInput, name)
	}
	// Роль в архиве — 400 (участник с любыми ролями, иначе отказ права раньше).
	anyRole := w.member("user:any", domain.MemberAgent, nil, nil)
	if _, err := w.a.CreateRole(w.ctx, "SDLC", w.admin.Principal.ID, CreateRoleInput{Key: "writer", Name: "writer",
		Version: &RoleVersionInput{Instructions: "Пиши."}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.ArchiveRole(w.ctx, "SDLC", "writer", true); err != nil {
		t.Fatal(err)
	}
	_, err = w.propose(anyRole, "writer", simpleProposal("x"))
	wantProposalCode(t, err, apperr.CodeInvalidInput, "роль в архиве")
	if after := w.count(`SELECT count(*) FROM role_version_proposals`); after != before {
		t.Fatalf("отказы завели предложения: %d → %d", before, after)
	}
}

// AC-2: таблица «уровень × набор ролей × роль предложения» — одна функция
// права; при отказе строки нет.
func TestRoleProposalRights(t *testing.T) {
	w := newProposalWorld(t)
	pid := projectID(t, w.store)
	ids := map[string]Identity{
		"agent [analyst]":         w.member("user:a1", domain.MemberAgent, []string{"analyst"}, nil),
		"agent []":                w.member("user:a2", domain.MemberAgent, []string{}, nil),
		"agent NULL":              w.member("user:a3", domain.MemberAgent, nil, nil),
		"viewer NULL":             w.member("user:v1", domain.MemberViewer, nil, nil),
		"agent NULL, токен [dev]": w.member("user:a4", domain.MemberAgent, nil, []string{"developer"}),
		"агент-бот [analyst]":     w.member("agent:bot", domain.MemberAgent, []string{"analyst"}, nil),
		"maintainer NULL":         w.member("user:m1", domain.MemberMaintainer, nil, nil),
	}
	for _, c := range []struct {
		who, role string
		want      apperr.Code
	}{
		{"agent [analyst]", "analyst", ""},
		{"agent [analyst]", "developer", apperr.CodeForbidden},
		{"agent []", "analyst", apperr.CodeForbidden},
		{"agent NULL", "analyst", ""},
		{"agent NULL", "qa", ""},
		{"viewer NULL", "analyst", apperr.CodeForbidden},
		{"agent NULL, токен [dev]", "analyst", apperr.CodeForbidden},
		{"agent NULL, токен [dev]", "developer", ""},
		{"агент-бот [analyst]", "analyst", ""},
		{"maintainer NULL", "reviewer", ""},
	} {
		if got := code(w.auth.MayProposeRole(w.ctx, ids[c.who], pid, c.role)); got != c.want {
			t.Errorf("%s → %s: право %q, ждали %q", c.who, c.role, got, c.want)
		}
		before := w.count(`SELECT count(*) FROM role_version_proposals`)
		_, err := w.propose(ids[c.who], c.role, simpleProposal(c.who+" "+c.role))
		if code(err) != c.want {
			t.Errorf("%s → %s: подача %v, ждали %q", c.who, c.role, err, c.want)
		}
		if after := w.count(`SELECT count(*) FROM role_version_proposals`); (c.want == "") != (after == before+1) {
			t.Errorf("%s → %s: строк %d → %d", c.who, c.role, before, after)
		}
	}
	_, err := w.propose(ids["agent NULL"], "no-such-role", simpleProposal("x"))
	wantProposalCode(t, err, apperr.CodeNotFound, "несуществующая роль")
	// Токен задачи — отказ белого списка при любом уровне держателя.
	task := taskIdentity(w.admin.Principal, true)
	if err := w.auth.MayProposeRole(w.ctx, task, pid, "analyst"); code(err) != apperr.CodeForbidden ||
		!strings.Contains(err.Error(), ExecutorRefusal) {
		t.Fatalf("токен задачи: %v", err)
	}
}

// AC-6, AC-9: утверждение заводит N+1 из содержимого предложения с
// автором предложения; документы, закрепления и поля равны; открытый
// прогон остаётся на N, новый берёт N+1.
func TestRoleProposalApprove(t *testing.T) {
	w := newProposalWorld(t)
	if _, err := w.a.CreateInstruction(w.ctx, "SDLC", w.admin.Principal.ID, InstructionInput{Key: "git", Name: "git", Content: gitV1}); err != nil {
		t.Fatal(err)
	}
	dev := w.member("user:dev", domain.MemberAgent, []string{"analyst"}, nil)
	maint := w.member("user:maint", domain.MemberMaintainer, nil, nil)
	viewer := w.member("user:viewer", domain.MemberViewer, nil, nil)
	bot := w.member("agent:bot", domain.MemberMaintainer, nil, nil)
	n := w.latest("analyst")

	p := w.mustPropose(dev, "analyst", RoleVersionInput{
		Documents: []RoleDocumentInput{
			{Name: "main.md", Content: "Ты аналитик из предложения.\n"},
			{Instruction: &InstructionRef{Key: "git", Version: 1}},
			{Name: "check.md", Kind: "checklist", Content: "- [ ] критерии\n"},
		},
		InputKinds:     []string{"task.brief"},
		OutputContract: json.RawMessage(`[{"kind":"analysis.brief","required":true}]`),
		ModelLevel:     ptrTo("low"), RepoScope: []string{"backend"},
		Changelog: ptrTo("чек-лист и git"),
	})
	if _, err := w.eng.Execute(w.ctx, "SDLC-1", "start", w.admin.Principal.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.AddInstructionVersion(w.ctx, "SDLC", "git", w.admin.Principal.ID, InstructionInput{Content: gitV2}); err != nil {
		t.Fatal(err)
	}

	// AC-8: агент (в том числе автор), viewer, агент-принципал уровня
	// maintainer, токен задачи — отказ, состояние прежнее.
	for name, id := range map[string]Identity{
		"автор agent": dev, "viewer": viewer, "агент maintainer": bot,
		"токен задачи": taskIdentity(maint.Principal, true),
	} {
		_, err := w.a.ApproveRoleProposal(w.ctx, w.auth, id, "SDLC", "analyst", p.ID)
		wantProposalCode(t, err, apperr.CodeForbidden, name)
		_, err = w.a.RejectRoleProposal(w.ctx, w.auth, id, "SDLC", "analyst", p.ID, "нет")
		wantProposalCode(t, err, apperr.CodeForbidden, name+" (reject)")
	}
	if w.latest("analyst") != n {
		t.Fatal("отказ завёл версию")
	}

	out, err := w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != domain.ProposalApproved || out.ResultVersion == nil || *out.ResultVersion != n+1 ||
		out.DecidedBy == nil || *out.DecidedBy != maint.Principal.ID || out.DecidedAt == nil ||
		out.DecidedByName == nil || *out.DecidedByName != "maint" {
		t.Fatalf("утверждённое: %+v", out)
	}
	rv, err := w.a.GetRoleVersion(w.ctx, "SDLC", "analyst", n+1)
	if err != nil {
		t.Fatal(err)
	}
	var createdBy uuid.UUID
	if err := w.store.DB().QueryRow(w.ctx, `SELECT created_by FROM role_versions WHERE id = $1`, rv.ID).Scan(&createdBy); err != nil {
		t.Fatal(err)
	}
	if createdBy != dev.Principal.ID {
		t.Fatalf("created_by версии %s, ждали автора %s", createdBy, dev.Principal.ID)
	}
	if deref(rv.Changelog) != "чек-лист и git" || deref(rv.ModelLevel) != "low" ||
		strings.Join(rv.InputKinds, ",") != "task.brief" || strings.Join(rv.RepoScope, ",") != "backend" {
		t.Fatalf("поля версии: %+v", rv)
	}
	if !sameDocuments(p.Documents, rv.Documents) || rv.Documents[1].Instruction == nil || rv.Documents[1].Instruction.Version != 1 {
		t.Fatalf("документы версии %+v, предложения %+v", rv.Documents, p.Documents)
	}
	for i := range p.Documents {
		if rv.Documents[i].SHA256 != p.Documents[i].SHA256 {
			t.Fatalf("sha документа %d разошёлся", i)
		}
	}
	if rv.Instructions != ConcatDocuments(rv.Documents) {
		t.Fatal("instructions версии — не склейка документов")
	}
	// Открытый прогон — на прежней версии, новый — на утверждённой.
	if got := w.runVersion("SDLC-1"); got != n {
		t.Fatalf("открытый прогон переехал на %d", got)
	}
	if _, err := w.eng.Execute(w.ctx, "SDLC-3", "start", w.admin.Principal.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := w.runVersion("SDLC-3"); got != n+1 {
		t.Fatalf("новый прогон на %d, ждали %d", got, n+1)
	}

	// AC-8: повторное решение — 409, состояние и номер прежние.
	_, err = w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", p.ID)
	wantProposalCode(t, err, apperr.CodeConflict, "повторное утверждение")
	_, err = w.a.RejectRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", p.ID, "поздно")
	wantProposalCode(t, err, apperr.CodeConflict, "отклонение утверждённого")
	_, err = w.a.WithdrawRoleProposal(w.ctx, w.auth, dev, "SDLC", "analyst", p.ID, "")
	wantProposalCode(t, err, apperr.CodeConflict, "отзыв утверждённого")
	if w.latest("analyst") != n+1 {
		t.Fatal("повторное решение завело версию")
	}
	// Предложение чужой роли в пути — 404.
	_, err = w.a.GetRoleProposal(w.ctx, "SDLC", "architect", p.ID)
	wantProposalCode(t, err, apperr.CodeNotFound, "чужая роль")
	_, err = w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "architect", p.ID)
	wantProposalCode(t, err, apperr.CodeNotFound, "утверждение по чужой роли")
}

// Отвязанный автором документ остаётся отвязанным и после утверждения:
// перенос закрепления отработал при подаче, повторно он не идёт.
func TestRoleProposalKeepsUnpin(t *testing.T) {
	w := newProposalWorld(t)
	if _, err := w.a.CreateInstruction(w.ctx, "SDLC", w.admin.Principal.ID, InstructionInput{Key: "git", Name: "git", Content: gitV1}); err != nil {
		t.Fatal(err)
	}
	roleWithGit(t, w.a, w.admin.Principal, "writer", RoleDocumentInput{Instruction: &InstructionRef{Key: "git", Version: 1}})
	dev := w.member("user:dev", domain.MemberAgent, []string{"writer"}, nil)
	maint := w.member("user:maint", domain.MemberMaintainer, nil, nil)
	p := w.mustPropose(dev, "writer", RoleVersionInput{
		Documents: []RoleDocumentInput{
			{Name: "main.md", Content: "Ты writer, правка.\n"},
			{Name: "git.md", Content: gitV1, Unpin: true},
		},
		Changelog: ptrTo("отвязал git"),
	})
	if p.Documents[1].Instruction != nil {
		t.Fatalf("отвязанный документ закреплён при подаче: %+v", p.Documents[1])
	}
	out, err := w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "writer", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := w.a.GetRoleVersion(w.ctx, "SDLC", "writer", *out.ResultVersion)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Documents[1].Instruction != nil {
		t.Fatalf("утверждение закрепило отвязанный документ: %+v", rv.Documents[1])
	}
}

// AC-7: база отстала (прямая версия или другое утверждённое предложение) —
// stale_role_base с номерами, версии нет, предложение pending; затем его
// можно отклонить или отозвать.
func TestRoleProposalStaleBase(t *testing.T) {
	w := newProposalWorld(t)
	dev := w.member("user:dev", domain.MemberAgent, []string{"analyst"}, nil)
	maint := w.member("user:maint", domain.MemberMaintainer, nil, nil)
	n := w.latest("analyst")
	p1 := w.mustPropose(dev, "analyst", simpleProposal("первое"))
	p2 := w.mustPropose(dev, "analyst", simpleProposal("второе"))
	if _, err := w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", p1.ID); err != nil {
		t.Fatal(err)
	}
	_, err := w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", p2.ID)
	e := apperr.As(err)
	details, _ := e.Details.(map[string]any)
	if e.Code != apperr.CodeStaleRoleBase || details["base_version"] != n || details["latest_version"] != n+1 {
		t.Fatalf("устаревшая база: %v %+v", err, e.Details)
	}
	if w.latest("analyst") != n+1 {
		t.Fatal("устаревшее предложение завело версию")
	}
	got, err := w.a.GetRoleProposal(w.ctx, "SDLC", "analyst", p2.ID)
	if err != nil || got.State != domain.ProposalPending {
		t.Fatalf("после отказа: %+v %v", got, err)
	}
	if out, err := w.a.RejectRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", p2.ID, "подайте от новой базы"); err != nil ||
		out.State != domain.ProposalRejected || deref(out.DecisionNote) != "подайте от новой базы" {
		t.Fatalf("отклонение устаревшего: %+v %v", out, err)
	}

	// Прямая версия сопровождающего между подачей и утверждением — то же.
	p3 := w.mustPropose(dev, "analyst", simpleProposal("третье"))
	if p3.BaseVersion != n+1 {
		t.Fatalf("база третьего %d, ждали %d", p3.BaseVersion, n+1)
	}
	if _, err := w.a.AddRoleVersion(w.ctx, "SDLC", "analyst", maint.Principal.ID, RoleVersionInput{Instructions: "прямая правка"}); err != nil {
		t.Fatal(err)
	}
	_, err = w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", p3.ID)
	wantProposalCode(t, err, apperr.CodeStaleRoleBase, "после прямой версии")
	if out, err := w.a.WithdrawRoleProposal(w.ctx, w.auth, dev, "SDLC", "analyst", p3.ID, ""); err != nil || out.State != domain.ProposalWithdrawn {
		t.Fatalf("отзыв устаревшего: %+v %v", out, err)
	}
}

// AC-10, AC-8: отклонение — с заметкой и только сопровождающим; отзыв —
// автор или сопровождающий, чужой агент — 403.
func TestRoleProposalRejectWithdraw(t *testing.T) {
	w := newProposalWorld(t)
	dev := w.member("user:dev", domain.MemberAgent, []string{"analyst"}, nil)
	other := w.member("user:other", domain.MemberAgent, []string{"analyst"}, nil)
	maint := w.member("user:maint", domain.MemberMaintainer, nil, nil)
	admin := w.member("user:padmin", domain.MemberAdmin, nil, nil)

	p := w.mustPropose(dev, "analyst", simpleProposal("на отклонение"))
	for _, note := range []string{"", "  \n"} {
		_, err := w.a.RejectRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", p.ID, note)
		wantProposalCode(t, err, apperr.CodeInvalidInput, "отклонение без заметки")
	}
	_, err := w.a.RejectRoleProposal(w.ctx, w.auth, dev, "SDLC", "analyst", p.ID, "сам себе")
	wantProposalCode(t, err, apperr.CodeForbidden, "отклоняет агент")
	out, err := w.a.RejectRoleProposal(w.ctx, w.auth, admin, "SDLC", "analyst", p.ID, " поправьте контракт ")
	if err != nil || out.State != domain.ProposalRejected || deref(out.DecisionNote) != "поправьте контракт" || out.ResultVersion != nil {
		t.Fatalf("отклонение: %+v %v", out, err)
	}

	q := w.mustPropose(dev, "analyst", simpleProposal("на отзыв"))
	_, err = w.a.WithdrawRoleProposal(w.ctx, w.auth, other, "SDLC", "analyst", q.ID, "")
	wantProposalCode(t, err, apperr.CodeForbidden, "отзывает чужой агент")
	if out, err := w.a.WithdrawRoleProposal(w.ctx, w.auth, dev, "SDLC", "analyst", q.ID, "передумал"); err != nil ||
		out.State != domain.ProposalWithdrawn || *out.DecidedBy != dev.Principal.ID || deref(out.DecisionNote) != "передумал" {
		t.Fatalf("отзыв автором: %+v %v", out, err)
	}
	r := w.mustPropose(dev, "analyst", simpleProposal("отзовёт сопровождающий"))
	if out, err := w.a.WithdrawRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", r.ID, ""); err != nil ||
		out.State != domain.ProposalWithdrawn || out.DecisionNote != nil {
		t.Fatalf("отзыв сопровождающим: %+v %v", out, err)
	}
	_, err = w.a.WithdrawRoleProposal(w.ctx, w.auth, taskIdentity(dev.Principal, false), "SDLC", "analyst", r.ID, "")
	wantProposalCode(t, err, apperr.CodeForbidden, "токен задачи")
}

// AC-12: утверждение атомарно — сбой addVersion не оставляет ни версии, ни
// смены состояния; параллельные утверждения одного предложения дают одну
// версию и один 409; два предложения от одной базы — одну версию и один
// stale_role_base.
func TestRoleProposalApproveAtomic(t *testing.T) {
	w := newProposalWorld(t)
	dev := w.member("user:dev", domain.MemberAgent, []string{"analyst"}, nil)
	maint := w.member("user:maint", domain.MemberMaintainer, nil, nil)
	n := w.latest("analyst")

	broken := w.mustPropose(dev, "analyst", simpleProposal("сломанное"))
	// Тип документа схема не проверяет — его проверяет сервис; подложенный
	// в обход подачи, он валит addVersion внутри утверждения.
	if _, err := w.store.DB().Exec(w.ctx, `UPDATE role_version_proposal_documents SET kind = 'BAD KIND' WHERE proposal_id = $1`, broken.ID); err != nil {
		t.Fatal(err)
	}
	_, err := w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", broken.ID)
	wantProposalCode(t, err, apperr.CodeInvalidInput, "сбой addVersion")
	if got, _ := w.a.GetRoleProposal(w.ctx, "SDLC", "analyst", broken.ID); got.State != domain.ProposalPending || w.latest("analyst") != n {
		t.Fatalf("после сбоя: %s, версия %d", got.State, w.latest("analyst"))
	}

	race := func(ids ...uuid.UUID) []error {
		var wg sync.WaitGroup
		errs := make([]error, len(ids))
		for i, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = w.a.ApproveRoleProposal(w.ctx, w.auth, maint, "SDLC", "analyst", id)
			}()
		}
		wg.Wait()
		return errs
	}
	one := w.mustPropose(dev, "analyst", simpleProposal("одно"))
	errs := race(one.ID, one.ID)
	if !((errs[0] == nil && code(errs[1]) == apperr.CodeConflict) || (errs[1] == nil && code(errs[0]) == apperr.CodeConflict)) {
		t.Fatalf("два утверждения одного: %v", errs)
	}
	if w.latest("analyst") != n+1 {
		t.Fatalf("версий после гонки %d, ждали %d", w.latest("analyst"), n+1)
	}
	a := w.mustPropose(dev, "analyst", simpleProposal("а"))
	b := w.mustPropose(dev, "analyst", simpleProposal("б"))
	errs = race(a.ID, b.ID)
	if !((errs[0] == nil && code(errs[1]) == apperr.CodeStaleRoleBase) || (errs[1] == nil && code(errs[0]) == apperr.CodeStaleRoleBase)) {
		t.Fatalf("два предложения от одной базы: %v", errs)
	}
	if w.latest("analyst") != n+2 {
		t.Fatalf("версий после второй гонки %d, ждали %d", w.latest("analyst"), n+2)
	}
}

// AC-5 (движок): отказ на прямую версию для agent подсказывает
// предложение; для viewer и токена задачи — как был.
func TestRoleVersionRefusalHint(t *testing.T) {
	w := newProposalWorld(t)
	pid := projectID(t, w.store)
	dev := w.member("user:dev", domain.MemberAgent, []string{"analyst"}, nil)
	viewer := w.member("user:viewer", domain.MemberViewer, nil, nil)
	refuse := func(id Identity) error {
		return w.auth.RoleVersionRefusal(w.ctx, id, pid, w.auth.Require(w.ctx, id, pid, domain.MemberMaintainer))
	}
	if err := refuse(dev); code(err) != apperr.CodeForbidden || !strings.Contains(err.Error(), "clab role propose") {
		t.Fatalf("agent: %v", err)
	}
	if err := refuse(viewer); code(err) != apperr.CodeForbidden || strings.Contains(err.Error(), "propose") {
		t.Fatalf("viewer: %v", err)
	}
}
