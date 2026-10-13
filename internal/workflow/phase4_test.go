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
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Интеграционные проверки фазы 4 по настоящей схеме: перенос связки, права по
// токенам, вебхуки. Всё это живёт в связке с базой и на моках не ловится.
//
//	SDLC_TEST_DATABASE_URL=postgres://sdlc:sdlc@localhost:55432/sdlc_test go test ./internal/workflow/

func setupPhase4(t *testing.T, mode string) (*postgres.Store, *Engine, *Authoring, *Auth, domain.Principal) {
	t.Helper()
	store, _ := testdb.Store(t)
	log := testdb.Logger()
	engine := NewEngine(store, log, Options{StrictGates: true})
	authoring := NewAuthoring(store, log, engine)
	auth := NewAuth(store, log, mode)
	system, err := store.ResolveActor(context.Background(), store.DB(), "test-system", domain.PrincipalSystem)
	if err != nil {
		t.Fatal(err)
	}
	return store, engine, authoring, auth, system
}

func TestImportBundleIsIdempotentAndVersioned(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)

	bundle := Bundle{
		Version: BundleVersion,
		Repositories: []BundleRepository{
			{Repository: "https://github.com/acme/api.git", RoleHint: "backend"},
			// Уже есть в сиде — обязан стать updated, а не дублем.
			{Repository: "cadence-lab-tech/sdlc-pipeline-backend", RoleHint: "backend"},
		},
		Roles: []BundleRole{
			{Key: "writer", Name: "Техписатель", Instructions: "Пиши документацию.",
				OutputContract: json.RawMessage(`[{"kind":"docs.page","required":true}]`)},
			// Существующая роль с той же инструкцией из сида.
			{Key: "qa", Name: "QA", Instructions: "не совпадает с сидом"},
		},
		Documents: []BundleDocument{
			{Kind: "guideline", Title: "Соглашения", Content: "# Правила\n\nНе хардкодить статусы."},
			{Kind: "architecture", Title: "Архитектура", Repository: "acme/api", RepoPath: "docs/ARCHITECTURE.md"},
		},
	}

	// Сухой прогон ничего не меняет.
	dry, err := authoring.Import(ctx, "SDLC", system.ID, bundle, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.DryRun || len(dry.Roles) != 2 || dry.Roles[0].Outcome != "created" {
		t.Fatalf("отчёт сухого прогона: %+v", dry)
	}
	if _, err := store.GetRoleByKey(ctx, store.DB(), projectID(t, store), "writer"); err == nil {
		t.Fatal("сухой прогон завёл роль")
	}

	first, err := authoring.Import(ctx, "SDLC", system.ID, bundle, false)
	if err != nil {
		t.Fatalf("импорт: %v", err)
	}
	want := map[string]string{"acme/api": "created", "cadence-lab-tech/sdlc-pipeline-backend": "updated"}
	for _, r := range first.Repositories {
		if want[r.Key] != r.Outcome {
			t.Errorf("репозиторий %s: %s, ожидали %s", r.Key, r.Outcome, want[r.Key])
		}
	}
	if first.Roles[0].Outcome != "created" || first.Roles[1].Outcome != "new_version" {
		t.Fatalf("роли после первого импорта: %+v", first.Roles)
	}
	if first.Documents[0].Outcome != "created" || first.Documents[1].Outcome != "created" {
		t.Fatalf("документы после первого импорта: %+v", first.Documents)
	}

	// Повтор той же связки — ничего не меняется и версий не прибавляется.
	second, err := authoring.Import(ctx, "SDLC", system.ID, bundle, false)
	if err != nil {
		t.Fatalf("повторный импорт: %v", err)
	}
	for _, r := range second.Roles {
		if r.Outcome != "unchanged" {
			t.Errorf("роль %s при повторе: %s", r.Key, r.Outcome)
		}
	}
	for _, d := range second.Documents {
		if d.Outcome != "unchanged" {
			t.Errorf("документ %s при повторе: %s", d.Key, d.Outcome)
		}
	}

	// Другая инструкция — новая версия, старая остаётся.
	bundle.Roles[0].Instructions = "Пиши документацию и проверяй ссылки."
	bundle.Documents[0].Content = "# Правила v2"
	third, err := authoring.Import(ctx, "SDLC", system.ID, bundle, false)
	if err != nil {
		t.Fatalf("третий импорт: %v", err)
	}
	if third.Roles[0].Outcome != "new_version" || third.Documents[0].Outcome != "updated" {
		t.Fatalf("третий импорт: роли %+v, документы %+v", third.Roles, third.Documents)
	}
	role, err := authoring.GetRole(ctx, "SDLC", "writer")
	if err != nil {
		t.Fatal(err)
	}
	if len(role.Versions) != 2 || role.Latest.Instructions != bundle.Roles[0].Instructions {
		t.Fatalf("у роли writer %d версий, последняя %q", len(role.Versions), role.Latest.Instructions)
	}

	// Экспорт отдаёт то, что импортировали, и активный цикл.
	exported, err := authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Workflows) == 0 || len(exported.Workflows[0].Statuses) == 0 {
		t.Fatal("экспорт без активного цикла")
	}
	foundDoc := false
	for _, d := range exported.Documents {
		if d.Title == "Соглашения" && d.Content == "# Правила v2" {
			foundDoc = true
		}
	}
	if !foundDoc {
		t.Fatal("экспорт не вернул обновлённый документ")
	}

	// Цикл из экспорта импортируется черновиком и проходит проверку графа.
	// Связка первой версии несла цикл одиночным полем — она обязана применяться
	// и после того, как формат стал списком.
	wf := exported.Workflows[0]
	wf.Name = "Перенесённый цикл"
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{Version: 1, Workflow: &wf}, false)
	if err != nil {
		t.Fatalf("импорт цикла: %v", err)
	}
	if len(report.Workflows) != 1 || report.Workflows[0].Outcome != "draft_created" {
		t.Fatalf("цикл: %+v", report.Workflows)
	}
	if len(report.Validations) != 1 || !report.Validations[0].Valid {
		t.Fatalf("перенесённый цикл не прошёл проверку: %+v", report.Validations)
	}
	id, _ := uuid.Parse(report.Workflows[0].ID)
	head, err := store.WorkflowHead(ctx, store.DB(), id)
	if err != nil || head.State != domain.WorkflowDraft {
		t.Fatalf("импортированный цикл должен быть черновиком: %v %+v", err, head)
	}
}

func TestTokensAndPermissions(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, _ := setupPhase4(t, AuthToken)

	if err := auth.Bootstrap(ctx, "admin", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
		t.Fatalf("администратор по bootstrap-токену: %v", err)
	}
	if !admin.Principal.IsAdmin || !admin.Authenticated() {
		t.Fatalf("bootstrap не сделал администратора: %+v", admin.Principal)
	}
	if _, err := auth.Authenticate(ctx, "no-such-token"); apperr.As(err).Code != apperr.CodeUnauthenticated {
		t.Fatalf("чужой токен: %v", err)
	}

	// Администратор выдаёт агенту токен, ограниченный проектом и ролью.
	issued, err := auth.IssueToken(ctx, admin, IssueTokenInput{
		Name: "ci-reviewer", Principal: "agent:reviewer-bot", ProjectKey: "SDLC",
		RoleKeys: []string{"reviewer"},
	})
	if err != nil {
		t.Fatalf("выдача токена агенту: %v", err)
	}
	if !strings.HasPrefix(issued.Token, TokenPrefix) || issued.Record.ProjectKey == nil {
		t.Fatalf("выданный токен: %+v", issued)
	}
	bot, err := auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	pid := projectID(t, store)
	if err := auth.Require(ctx, bot, pid, domain.MemberAgent); err != nil {
		t.Fatalf("агент обязан быть участником уровня agent: %v", err)
	}
	if err := auth.Require(ctx, bot, pid, domain.MemberMaintainer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент не должен быть сопровождающим: %v", err)
	}
	if err := auth.Require(ctx, bot, uuid.New(), domain.MemberViewer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("токен привязан к проекту и вне него недействителен: %v", err)
	}

	// Ограничение по ролям срабатывает на захвате: этап аналитика ревьюеру
	// не достаётся.
	if _, err := engine.Execute(ctx, "SDLC-1", "start", admin.Principal.ID, ""); err != nil {
		t.Fatalf("старт задачи: %v", err)
	}
	allowed, err := auth.AllowedRoles(ctx, bot, pid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.ClaimWith(ctx, "SDLC-1", bot.Principal.ID, "test", ClaimOptions{AllowedRoles: allowed})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("захват чужой роли: %v", err)
	}

	// Не участник не видит проекта; участник видит.
	stranger := Identity{Principal: mustActor(t, store, "user:stranger")}
	if err := auth.Require(ctx, stranger, pid, domain.MemberViewer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("посторонний: %v", err)
	}
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:stranger", domain.MemberViewer, nil); err != nil {
		t.Fatal(err)
	}
	if err := auth.Require(ctx, stranger, pid, domain.MemberViewer); err != nil {
		t.Fatalf("после добавления: %v", err)
	}
	projects, err := auth.VisibleProjects(ctx, stranger)
	if err != nil || len(projects) != 1 {
		t.Fatalf("видимые проекты: %v %d", err, len(projects))
	}

	// Свой токен может выдать себе любой участник; чужой — нет.
	if _, err := auth.IssueToken(ctx, stranger, IssueTokenInput{Name: "mine"}); err != nil {
		t.Fatalf("свой токен: %v", err)
	}
	_, err = auth.IssueToken(ctx, stranger, IssueTokenInput{Name: "evil", Principal: "agent:x", ProjectKey: "SDLC"})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("наблюдатель выдал токен другому: %v", err)
	}

	// Отзыв: после него токен недействителен.
	if err := auth.RevokeToken(ctx, admin, issued.Record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, issued.Token); apperr.As(err).Code != apperr.CodeUnauthenticated {
		t.Fatalf("отозванный токен прошёл: %v", err)
	}

	// Последнего администратора проекта снять нельзя.
	mustActor(t, store, "user:owner")
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:owner", domain.MemberAdmin, nil); err != nil {
		t.Fatal(err)
	}
	if err := auth.RemoveMember(ctx, admin, "SDLC", "user:owner"); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("последний администратор: %v", err)
	}

	// Апрув человека агент поставить не может даже в режиме заголовка.
	agent := mustActor(t, store, "agent:analyst-bot")
	_, err = engine.DecideGate(ctx, "SDLC-1", "analysis_done", "human_approval", agent.ID,
		domain.GateResultPassed, json.RawMessage(`{}`))
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент поставил human_approval: %v", err)
	}
}

func TestGitHubWebhooks(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	// Доставка несёт установку, и репозиторий должен читаться ею (решение
	// 0034): у демонстрационного проекта установка по умолчанию.
	if _, err := engine.SetProjectGitHub(ctx, "SDLC", SetGitHubInput{InstallationID: ptr(int64(42))}); err != nil {
		t.Fatal(err)
	}
	push := []byte(`{"ref":"refs/heads/main","after":"abc123abc123abc123abc123abc123abc123abc1",
		"repository":{"name":"sdlc-pipeline-backend","owner":{"login":"cadence-lab-tech"}},
		"installation":{"id":42}}`)
	out, err := engine.HandleGitHubEvent(ctx, "push", "d-1", push)
	if err != nil {
		t.Fatal(err)
	}
	if out.Queued != 1 {
		t.Fatalf("push должен поставить один прогон, поставил %d: %+v", out.Queued, out)
	}
	// Повтор доставки — не повтор работы.
	again, err := engine.HandleGitHubEvent(ctx, "push", "d-1", push)
	if err != nil || !again.Duplicate {
		t.Fatalf("повторная доставка: %v %+v", err, again)
	}
	// Тот же коммит другой доставкой — очередь уже содержит его.
	third, err := engine.HandleGitHubEvent(ctx, "push", "d-2", push)
	if err != nil || third.Queued != 0 {
		t.Fatalf("тот же коммит: %v %+v", err, third)
	}
	// Ветка не под наблюдением — игнорируется.
	feature := []byte(`{"ref":"refs/heads/feature","after":"def456def456def456def456def456def456def4",
		"repository":{"name":"sdlc-pipeline-backend","owner":{"login":"cadence-lab-tech"}},
		"installation":{"id":42}}`)
	if out, err := engine.HandleGitHubEvent(ctx, "push", "d-3", feature); err != nil || out.Queued != 0 {
		t.Fatalf("ветка вне наблюдения: %v %+v", err, out)
	}

	// PR, привязанный к задаче, получает состояние из события и сбрасывает
	// отметку синхронизации.
	if _, err := engine.Execute(ctx, "SDLC-1", "start", system.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.LinkRepoRef(ctx, "SDLC-1", system.ID, LinkRepoRefInput{
		Repository: "backend", Ref: "128", State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRepoRefChecks(ctx, store.DB(), refID(t, store, "SDLC-1"), "open", "old", "", nil); err != nil {
		t.Fatal(err)
	}
	pr := []byte(`{"action":"synchronize","number":128,
		"repository":{"name":"sdlc-pipeline-backend","owner":{"login":"cadence-lab-tech"}},
		"pull_request":{"state":"open","merged":false,"html_url":"https://github.com/x/pull/128","head":{"sha":"newhead"}}}`)
	out, err = engine.HandleGitHubEvent(ctx, "pull_request", "d-4", pr)
	if err != nil || out.Touched != 1 {
		t.Fatalf("pull_request: %v %+v", err, out)
	}
	toSync, err := store.RepoRefsToSync(ctx, store.DB(), 10)
	if err != nil || len(toSync) != 1 {
		t.Fatalf("PR обязан снова ждать синхронизации: %v %d", err, len(toSync))
	}

	checks := []byte(`{"check_run":{"head_sha":"newhead"},
		"repository":{"name":"sdlc-pipeline-backend","owner":{"login":"cadence-lab-tech"}}}`)
	if err := store.UpdateRepoRefChecks(ctx, store.DB(), refID(t, store, "SDLC-1"), "open", "newhead", "", nil); err != nil {
		t.Fatal(err)
	}
	out, err = engine.HandleGitHubEvent(ctx, "check_run", "d-5", checks)
	if err != nil || out.Touched != 1 {
		t.Fatalf("check_run: %v %+v", err, out)
	}

	// Метрики считаются по накопленному без ошибок.
	if _, err := engine.ProjectMetrics(ctx, "SDLC", 24*time.Hour); err != nil {
		t.Fatalf("метрики: %v", err)
	}
}

func projectID(t *testing.T, store *postgres.Store) uuid.UUID {
	t.Helper()
	p, err := store.GetProjectByKey(context.Background(), store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func mustActor(t *testing.T, store *postgres.Store, raw string) domain.Principal {
	t.Helper()
	kind, handle := SplitHandle(raw, domain.PrincipalUser)
	p, err := store.ResolveActor(context.Background(), store.DB(), handle, kind)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func refID(t *testing.T, store *postgres.Store, taskKey string) uuid.UUID {
	t.Helper()
	task, err := store.GetTaskByKey(context.Background(), store.DB(), taskKey)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := store.ListRepoRefs(context.Background(), store.DB(), task.ID)
	if err != nil || len(refs) == 0 {
		t.Fatalf("нет привязанных PR: %v", err)
	}
	return refs[0].ID
}

// Апрув человека устаревает от новой сдачи артефактов (решение 0042,
// CLAB-103): апрув, поставленный до того, как аналитик сдал результат,
// переход не открывает; после повторного апрува — открывает.
func TestHumanApprovalExpiresOnSubmission(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	human, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}

	res, err := engine.Execute(ctx, "SDLC-1", "start", system.ID, "")
	if err != nil || res.Blocked {
		t.Fatalf("старт: %v %+v", err, res.Gates)
	}
	task := res.Task

	// Апрув до сдачи — и сдача после него.
	if _, err := engine.DecideGate(ctx, task.Key, "analysis_done", "human_approval",
		human.ID, domain.GateResultPassed, nil); err != nil {
		t.Fatal(err)
	}
	if g := gateOutcome(t, ctx, engine, task, "analysis_done", "human_approval"); g.Status != domain.GateResultPassed {
		t.Fatalf("апрув до сдачи должен быть виден, пока сдачи нет: %+v", g)
	}
	for _, kind := range []string{"analysis.brief", "analysis.acceptance_criteria"} {
		if _, err := engine.SubmitArtifact(ctx, task.Key, system.ID, SubmitArtifactInput{
			Kind: kind, Title: kind, Format: "markdown", Content: []byte("# " + kind),
		}); err != nil {
			t.Fatal(err)
		}
	}
	g := gateOutcome(t, ctx, engine, task, "analysis_done", "human_approval")
	if g.Status != domain.GateResultPending || !strings.Contains(g.Reason, "устарели") {
		t.Fatalf("апрув до сдачи обязан устареть: %+v", g)
	}
	if stale, _ := g.Details["stale_approvals"].(int); stale != 1 {
		t.Fatalf("в деталях нет устаревшего апрува: %+v", g.Details)
	}

	// Повторный апрув после сдачи открывает гейт.
	if _, err := engine.DecideGate(ctx, task.Key, "analysis_done", "human_approval",
		human.ID, domain.GateResultPassed, nil); err != nil {
		t.Fatal(err)
	}
	g = gateOutcome(t, ctx, engine, task, "analysis_done", "human_approval")
	if g.Status != domain.GateResultPassed {
		t.Fatalf("апрув после сдачи: %+v", g)
	}

	// Пересдача после апрува снимает его снова.
	if _, err := engine.SubmitArtifact(ctx, task.Key, system.ID, SubmitArtifactInput{
		Kind: "analysis.brief", Title: "v2", Format: "markdown", Content: []byte("# v2"),
	}); err != nil {
		t.Fatal(err)
	}
	if g := gateOutcome(t, ctx, engine, task, "analysis_done", "human_approval"); g.Status != domain.GateResultPending {
		t.Fatalf("пересдача после апрува: %+v", g)
	}
}
