package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Токен задачи (CLAB-212, решение 0049): выпуск по аренде, жизнь с арендой,
// привязка к своей задаче, круг чтения. Интеграционные: жизнь токена — это
// свойство связки «токен плюс прогон», и на моках она не проверяется.

// taskTokenWorld — инстанс в режиме токенов: администратор, пул раннеров
// уровня agent с личным токеном и две запущенные задачи (SDLC-1, SDLC-3).
type taskTokenWorld struct {
	store  *postgres.Store
	engine *Engine
	auth   *Auth
	admin  Identity
	pool   Identity
	other  Identity
}

func newTaskTokenWorld(t *testing.T, mode string) taskTokenWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, _, auth, _ := setupPhase4(t, mode)
	if err := auth.Bootstrap(ctx, "admin", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	personal := func(handle string) Identity {
		t.Helper()
		issued, err := auth.IssueToken(ctx, admin, IssueTokenInput{
			Name: "runner", Principal: "agent:" + handle, ProjectKey: "SDLC",
		})
		if err != nil {
			t.Fatal(err)
		}
		id, err := auth.Authenticate(ctx, issued.Token)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	w := taskTokenWorld{store: store, engine: engine, auth: auth, admin: admin,
		pool: personal("pool"), other: personal("other")}
	for _, key := range []string{"SDLC-1", "SDLC-3"} {
		if _, err := engine.Execute(ctx, key, "start", admin.Principal.ID, ""); err != nil {
			t.Fatalf("старт %s: %v", key, err)
		}
	}
	return w
}

func (w taskTokenWorld) claim(t *testing.T, id Identity, key string) ClaimResult {
	t.Helper()
	res, err := w.engine.ClaimWith(context.Background(), key, id.Principal.ID, "test", ClaimOptions{})
	if err != nil {
		t.Fatalf("захват %s: %v", key, err)
	}
	return res
}

func (w taskTokenWorld) issue(t *testing.T, id Identity, key string) (IssuedTaskToken, Identity) {
	t.Helper()
	ctx := context.Background()
	issued, err := w.auth.IssueTaskToken(ctx, id, key)
	if err != nil {
		t.Fatalf("выпуск токена задачи %s: %v", key, err)
	}
	tid, err := w.auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("свежий токен задачи не принят: %v", err)
	}
	return issued, tid
}

func code(err error) apperr.Code {
	if err == nil {
		return ""
	}
	return apperr.As(err).Code
}

// mustDead — токен задачи больше не принимается, и сказано почему.
func (w taskTokenWorld) mustDead(t *testing.T, token, what string) {
	t.Helper()
	_, err := w.auth.Authenticate(context.Background(), token)
	if code(err) != apperr.CodeUnauthenticated || !strings.Contains(err.Error(), "токен задачи погас") {
		t.Fatalf("%s: токен задачи жив или отказ не тот: %v", what, err)
	}
}

func (w taskTokenWorld) revokedAt(t *testing.T, tokenID uuid.UUID) bool {
	t.Helper()
	var revoked bool
	if err := w.store.DB().QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM agent_tokens WHERE id = $1`, tokenID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	return revoked
}

func (w taskTokenWorld) taskTokens(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.store.DB().QueryRow(context.Background(),
		`SELECT count(*) FROM agent_tokens WHERE kind = 'task'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Критерии 4–7: выпуск держателю живой аренды, отказы и два токена на прогон.
func TestTaskTokenIssue(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	claimed := w.claim(t, w.pool, "SDLC-1")

	// Без аренды и на чужую аренду — коды контракта, записи нет.
	if _, err := w.auth.IssueTaskToken(ctx, w.pool, "SDLC-3"); code(err) != apperr.CodeLeaseExpired {
		t.Fatalf("выпуск без аренды: %v", err)
	}
	if _, err := w.auth.IssueTaskToken(ctx, w.other, "SDLC-1"); code(err) != apperr.CodeLeaseHeldByOther {
		t.Fatalf("выпуск на чужую аренду: %v", err)
	}
	if n := w.taskTokens(t); n != 0 {
		t.Fatalf("отказ оставил %d токенов задачи", n)
	}

	issued, tid := w.issue(t, w.pool, "SDLC-1")
	rec := issued.Record
	if rec.TokenKind != domain.TokenTask || rec.StageRunID == nil || *rec.StageRunID != claimed.StageRunID ||
		rec.TaskKey == nil || *rec.TaskKey != "SDLC-1" || rec.ExpiresAt != nil ||
		rec.ProjectKey == nil || *rec.ProjectKey != "SDLC" ||
		len(rec.RoleKeys) != 1 || rec.RoleKeys[0] != claimed.Role.Key {
		t.Fatalf("запись токена задачи: %+v", rec)
	}
	if issued.TaskKey != "SDLC-1" || issued.StageRunID != claimed.StageRunID ||
		issued.Role != claimed.Role.Key || issued.ProjectKey != "SDLC" || issued.LeaseExpiresAt.IsZero() {
		t.Fatalf("ответ выпуска: %+v", issued)
	}
	if b := tid.TaskToken(); b == nil || b.TaskKey != "SDLC-1" || b.StageRunID != claimed.StageRunID {
		t.Fatalf("привязка не разобрана: %+v", b)
	}

	// Токеном задачи токены не выпускаются — ни задачи, ни личные (крит. 6).
	if _, err := w.auth.IssueTaskToken(ctx, tid, "SDLC-1"); code(err) != apperr.CodeForbidden {
		t.Fatalf("токен задачи выпустил токен задачи: %v", err)
	}
	if _, err := w.auth.IssueToken(ctx, tid, IssueTokenInput{Name: "escape"}); code(err) != apperr.CodeForbidden {
		t.Fatalf("токен задачи выпустил личный токен: %v", err)
	}

	// Второй выпуск — второй живой токен; release гасит оба (крит. 7, 15).
	second, _ := w.issue(t, w.pool, "SDLC-1")
	if err := w.engine.Release(ctx, claimed.StageRunID, w.pool.Principal.ID, "агент упал"); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []IssuedTaskToken{issued, second} {
		w.mustDead(t, tok.Token, "после release")
		if !w.revokedAt(t, tok.Record.ID) {
			t.Fatalf("после release у токена %s нет revoked_at", tok.Record.Name)
		}
	}

	// Погасшие токены задачи в списке токенов не видны, личные — на месте.
	list, err := w.auth.ListTokens(ctx, w.pool, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if tok.TokenKind == domain.TokenTask {
			t.Fatalf("погасший токен задачи в списке: %+v", tok)
		}
	}
	if len(list) != 1 {
		t.Fatalf("личный токен пропал из списка: %+v", list)
	}
}

// Критерии 15–18, 20: протухание, повторный захват, переход, удаление задачи.
func TestTaskTokenLivesWithLease(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	run := w.claim(t, w.pool, "SDLC-1").StageRunID
	first, _ := w.issue(t, w.pool, "SDLC-1")

	// Подтверждающий захват тем же принципалом при живой аренде токен не гасит.
	w.claim(t, w.pool, "SDLC-1")
	if _, err := w.auth.Authenticate(ctx, first.Token); err != nil {
		t.Fatalf("подтверждение аренды погасило токен: %v", err)
	}

	// Аренда протухла: до воркера токен уже мёртв, после — отозван.
	if _, err := w.store.DB().Exec(ctx,
		`UPDATE stage_runs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	w.mustDead(t, first.Token, "протухшая аренда до воркера")
	if w.revokedAt(t, first.Record.ID) {
		t.Fatal("revoked_at до прохода воркера")
	}
	system := mustActor(t, w.store, "system:test-worker")
	if _, err := w.engine.ExpireLeases(ctx, system.ID, 10); err != nil {
		t.Fatal(err)
	}
	if !w.revokedAt(t, first.Record.ID) {
		t.Fatal("воркер протухших не отозвал токен задачи")
	}

	// Тот же принципал забрал снова: старый мёртв, новый работает.
	w.claim(t, w.pool, "SDLC-1")
	w.mustDead(t, first.Token, "после повторного захвата")
	fresh, _ := w.issue(t, w.pool, "SDLC-1")

	// Повторный захват снятой аренды без воркера: токены прошлой аренды
	// гаснут сразу, а не «оживают» вместе с новой арендой того же принципала.
	if _, err := w.store.DB().Exec(ctx,
		`UPDATE stage_runs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	w.claim(t, w.pool, "SDLC-1")
	w.mustDead(t, fresh.Token, "захват после протухания без воркера")
	if !w.revokedAt(t, fresh.Record.ID) {
		t.Fatal("захват после протухания не отозвал токен прошлой аренды")
	}

	// Переход закрывает этап — токен гаснет и в новом этапе не захватывает.
	last, _ := w.issue(t, w.pool, "SDLC-1")
	if _, err := w.engine.Execute(ctx, "SDLC-1", "cancel", w.admin.Principal.ID, "снята"); err != nil {
		t.Fatal(err)
	}
	w.mustDead(t, last.Token, "после перехода")
	if !w.revokedAt(t, last.Record.ID) {
		t.Fatal("переход не отозвал токен задачи")
	}

	// Удаление задачи уносит её токены задачи; личные не тронуты (крит. 20).
	w.claim(t, w.pool, "SDLC-3")
	w.issue(t, w.pool, "SDLC-3")
	if _, err := w.store.DB().Exec(ctx, `DELETE FROM tasks WHERE key = 'SDLC-3'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := w.store.DB().QueryRow(ctx, `
		SELECT count(*) FROM agent_tokens t
		WHERE t.kind = 'task' AND NOT EXISTS (SELECT 1 FROM stage_runs sr WHERE sr.id = t.stage_run_id)`).
		Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("после удаления задачи висят %d токенов задачи", left)
	}
	if _, err := w.auth.Authenticate(ctx, "bootstrap-token-0123456789"); err != nil {
		t.Fatalf("удаление задачи задело личный токен: %v", err)
	}
}

// Критерии 10, 18, 19: перепроверка гасит токен родительского прогона,
// токен прогона ревью захватывает только свой прогон и гаснет на вердикте,
// переход вперёд гасит токен этапа.
func TestTaskTokenReviewAndForwardTransition(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	parent := w.claim(t, w.pool, "SDLC-1")
	stageTok, stageID := w.issue(t, w.pool, "SDLC-1")

	for _, kind := range []string{"analysis.brief", "analysis.acceptance_criteria"} {
		if _, err := w.engine.SubmitArtifact(ctx, "SDLC-1", w.pool.Principal.ID, SubmitArtifactInput{
			Kind: kind, Title: kind, Content: []byte("# " + kind), StageRunID: &parent.StageRunID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := w.engine.Execute(ctx, "SDLC-1", "analysis_done", stageID.Principal.ID, "")
	if err != nil || !res.Blocked {
		t.Fatalf("переход до ревью: %v %+v", err, res)
	}
	w.mustDead(t, stageTok.Token, "родительский прогон в gating")
	if !w.revokedAt(t, stageTok.Record.ID) {
		t.Fatal("уход в gating не отозвал токен этапа")
	}

	// Ревью забирает тот же пул; токен ревью захватывает именно его прогон.
	review := w.claim(t, w.pool, "SDLC-1")
	if review.Kind != domain.StageKindReview {
		t.Fatalf("захвачен не прогон ревью: %s", review.Kind)
	}
	revTok, revID := w.issue(t, w.pool, "SDLC-1")
	if *revTok.Record.StageRunID != review.StageRunID {
		t.Fatalf("токен ревью на прогоне %s, а не %s", *revTok.Record.StageRunID, review.StageRunID)
	}
	onlyRun, err := w.auth.OwnRun(revID, "SDLC-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := w.engine.ClaimWith(ctx, "SDLC-1", revID.Principal.ID, "sub", ClaimOptions{OnlyRun: onlyRun})
	if err != nil || again.StageRunID != review.StageRunID {
		t.Fatalf("подтверждение аренды ревью: %v %s", err, again.StageRunID)
	}
	if _, err := w.auth.Authenticate(ctx, revTok.Token); err != nil {
		t.Fatalf("подтверждение погасило токен ревью: %v", err)
	}
	// Явный чужой прогон той же задачи — 403.
	if _, err := w.auth.OwnRun(revID, "SDLC-1", &parent.StageRunID); code(err) != apperr.CodeForbidden {
		t.Fatalf("токен ревью адресовал прогон этапа: %v", err)
	}
	other := parent.StageRunID
	if _, err := w.engine.ClaimWith(ctx, "SDLC-1", revID.Principal.ID, "sub",
		ClaimOptions{OnlyRun: &other}); err == nil {
		t.Fatal("захват закрытого родительского прогона прошёл")
	}

	if _, err := w.engine.SubmitArtifact(ctx, "SDLC-1", revID.Principal.ID, SubmitArtifactInput{
		Kind: "review.verdict", Title: "Вердикт", Format: "json",
		Content: []byte(`{"verdict":"pass","findings":[]}`), StageRunID: onlyRun,
	}); err != nil {
		t.Fatal(err)
	}
	w.mustDead(t, revTok.Token, "после закрытия перепроверки")

	// Апрув человека, повторный захват этапа — новый токен, переход вперёд.
	if _, err := w.engine.DecideGate(ctx, "SDLC-1", "analysis_done", "human_approval",
		w.admin.Principal.ID, nil, domain.GateResultPassed, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	w.claim(t, w.pool, "SDLC-1")
	fwd, fwdID := w.issue(t, w.pool, "SDLC-1")
	res, err = w.engine.Execute(ctx, "SDLC-1", "analysis_done", fwdID.Principal.ID, "готово")
	if err != nil || res.Blocked {
		t.Fatalf("переход вперёд: %v %+v", err, res)
	}
	w.mustDead(t, fwd.Token, "после перехода вперёд")

	// В новом этапе погасший токен не работает, а живой личный — захватывает.
	if _, err := w.auth.Authenticate(ctx, fwd.Token); code(err) != apperr.CodeUnauthenticated {
		t.Fatal("погасший токен ожил в новом этапе")
	}
}

// Критерии 9 и 11: запись — только своя задача и свой прогон, чтение — круг
// задачи. Пул держит две задачи, токен одной не трогает другую.
func TestTaskTokenBindingAndReadCircle(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	w.claim(t, w.pool, "SDLC-1")
	b := w.claim(t, w.pool, "SDLC-3")
	_, tid := w.issue(t, w.pool, "SDLC-1")

	if err := w.auth.RequireOwnTaskKey(tid, "SDLC-3"); code(err) != apperr.CodeForbidden {
		t.Fatalf("запись в чужую задачу пула: %v", err)
	}
	if _, err := w.auth.OwnRun(tid, "SDLC-3", nil); code(err) != apperr.CodeForbidden {
		t.Fatalf("аренда чужой задачи пула: %v", err)
	}
	if _, err := w.auth.OwnRun(tid, "", &b.StageRunID); code(err) != apperr.CodeForbidden {
		t.Fatalf("чужой прогон по идентификатору: %v", err)
	}
	if err := w.auth.RequireOwnTaskKey(tid, ""); code(err) != apperr.CodeForbidden ||
		!strings.Contains(err.Error(), "parent") {
		t.Fatalf("задача без родителя токеном задачи: %v", err)
	}
	onlyRun, _ := w.auth.OwnRun(tid, "SDLC-1", nil)
	if _, err := w.engine.ClaimWith(ctx, "SDLC-3", tid.Principal.ID, "x",
		ClaimOptions{OnlyRun: onlyRun}); code(err) != apperr.CodeForbidden {
		t.Fatalf("захват задачи B прогоном задачи A: %v", err)
	}

	// Круг SDLC-1: родитель SDLC-4, ребёнок D, blocks SDLC-1→SDLC-2,
	// relates_to Y→SDLC-1; Z вне круга.
	child, err := w.engine.CreateTask(ctx, "SDLC", w.admin.Principal.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "D", ParentKey: "SDLC-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	y, err := w.engine.CreateTask(ctx, "SDLC", w.admin.Principal.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.LinkTasks(ctx, y.Key, "SDLC-1", domain.TaskLinkRelatesTo, "", w.admin.Principal.ID, false); err != nil {
		t.Fatal(err)
	}
	z, err := w.engine.CreateTask(ctx, "SDLC", w.admin.Principal.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(key string) error {
		task, err := w.store.GetTaskByKey(ctx, w.store.DB(), key)
		if err != nil {
			t.Fatal(err)
		}
		return w.auth.RequireReadable(ctx, tid, task.ID)
	}
	for _, key := range []string{"SDLC-1", "SDLC-4", child.Key, "SDLC-2", y.Key} {
		if err := read(key); err != nil {
			t.Errorf("%s в круге, но не читается: %v", key, err)
		}
	}
	for _, key := range []string{"SDLC-3", z.Key} {
		if err := read(key); code(err) != apperr.CodeForbidden {
			t.Errorf("%s вне круга, но читается: %v", key, err)
		}
	}

	// Личный токен того же принципала — как раньше (эпик 1–3): ни одна
	// проверка токена задачи на нём не срабатывает.
	if err := w.auth.RequireOwnTaskKey(w.pool, "SDLC-3"); err != nil {
		t.Fatal(err)
	}
	zt, _ := w.store.GetTaskByKey(ctx, w.store.DB(), z.Key)
	if err := w.auth.RequireReadable(ctx, w.pool, zt.ID); err != nil {
		t.Fatal(err)
	}
	if w.auth.SummaryOnly(w.pool) || !w.auth.SummaryOnly(tid) {
		t.Fatal("сводка списков: не тому")
	}
}

// Критерий 13: поиск токеном задачи — код только области задачи, артефакты
// только круга чтения, документы как есть.
func TestTaskTokenSearchBound(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	w.claim(t, w.pool, "SDLC-1") // область: sdlc-pipeline-backend
	_, tid := w.issue(t, w.pool, "SDLC-1")
	pid := projectID(t, w.store)

	repos, err := w.store.ListRepositories(ctx, w.store.DB(), pid)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		run, _, err := w.store.EnqueueIndexRun(ctx, w.store.DB(), postgres.EnqueueIndexRunInput{
			RepositoryID: r.ID, Branch: "main", CommitSHA: strings.Repeat("b", 40), Trigger: "manual",
		})
		if err != nil {
			t.Fatal(err)
		}
		var fileID uuid.UUID
		if err := w.store.DB().QueryRow(ctx, `
			INSERT INTO code_files (index_run_id, repository_id, path, lang, size_bytes)
			VALUES ($1, $2, 'quasar.go', 'go', 10) RETURNING id`, run.ID, r.ID).Scan(&fileID); err != nil {
			t.Fatal(err)
		}
		if _, err := w.store.DB().Exec(ctx, `
			INSERT INTO code_chunks (code_file_id, project_id, ord, start_line, end_line, text)
			VALUES ($1, $2, 0, 1, 10, 'func quasar() {}')`, fileID, pid); err != nil {
			t.Fatal(err)
		}
		if _, err := w.store.DB().Exec(ctx, `
			UPDATE repo_index_runs SET state = 'completed', finished_at = now() WHERE id = $1`, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"SDLC-1", "SDLC-3"} {
		task, err := w.store.GetTaskByKey(ctx, w.store.DB(), key)
		if err != nil {
			t.Fatal(err)
		}
		a, err := w.engine.SubmitArtifact(ctx, key, w.admin.Principal.ID, SubmitArtifactInput{
			Kind: "analysis.brief", Title: "quasar " + key, Content: []byte("quasar"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.store.ReplaceArtifactChunks(ctx, w.store.DB(), a.ID, pid, task.ID,
			[]postgres.TextChunkInput{{Text: "quasar " + key, TextSHA: "sha-" + key}}); err != nil {
			t.Fatal(err)
		}
	}
	doc, _, err := w.engine.PutDocument(ctx, "SDLC", w.admin.Principal.ID, PutDocumentInput{
		Kind: "guideline", Title: "quasar", Content: []byte("quasar"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.ReplaceDocumentChunks(ctx, w.store.DB(), doc.ID, pid,
		[]postgres.TextChunkInput{{Text: "quasar rules", TextSHA: "doc"}}); err != nil {
		t.Fatal(err)
	}

	search := func(id Identity, repos []string) []postgres.SearchHit {
		t.Helper()
		bound, err := w.auth.SearchBound(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		res, err := w.engine.SearchContext(ctx, "SDLC", SearchInput{
			Query: "quasar", Repositories: repos, TopK: 50, Bound: bound,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Hits
	}
	summary := func(hits []postgres.SearchHit) (code []string, tasks []string, docs int) {
		for _, h := range hits {
			switch h.Scope {
			case postgres.ScopeCode:
				code = append(code, h.Repository)
			case postgres.ScopeArtifacts:
				tasks = append(tasks, h.TaskKey)
			case postgres.ScopeDocuments:
				docs++
			}
		}
		return
	}

	// Личный токен — весь проект, как раньше.
	codeAll, tasksAll, docsAll := summary(search(w.pool, nil))
	if len(codeAll) != 3 || len(tasksAll) != 2 || docsAll != 1 {
		t.Fatalf("личный токен: код %v, артефакты %v, документы %d", codeAll, tasksAll, docsAll)
	}
	// Токен задачи без repositories — область задачи.
	code1, tasks1, docs1 := summary(search(tid, nil))
	if len(code1) != 1 || code1[0] != "cadence-lab-tech/sdlc-pipeline-backend" ||
		len(tasks1) != 1 || tasks1[0] != "SDLC-1" || docs1 != 1 {
		t.Fatalf("токен задачи: код %v, артефакты %v, документы %d", code1, tasks1, docs1)
	}
	// Запрошенный репозиторий вне области отброшен, а не добавлен.
	code2, _, _ := summary(search(tid, []string{"cadence-lab-tech/sdlc-pipeline-frontend"}))
	if len(code2) != 0 {
		t.Fatalf("репозиторий вне области в выдаче: %v", code2)
	}
}

// Критерий 14: /auth/me токеном задачи — только проект токена, без команд.
func TestTaskTokenMe(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	w.claim(t, w.pool, "SDLC-1")
	_, tid := w.issue(t, w.pool, "SDLC-1")

	me, err := w.auth.Me(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if me.Token == nil || me.Token.TokenKind != domain.TokenTask || me.Token.TaskKey == nil ||
		*me.Token.TaskKey != "SDLC-1" || len(me.Teams) != 0 || me.CanCreateProjects {
		t.Fatalf("me токеном задачи: %+v", me)
	}
	for _, m := range me.Memberships {
		if m.ProjectKey != "SDLC" {
			t.Fatalf("участие вне проекта токена: %+v", m)
		}
	}
}

// Критерий 21: в режиме заголовка выпуск работает, ограничения токена задачи
// не применяются — прав там нет вовсе (0049 п. 2).
func TestTaskTokenHeaderMode(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthHeader)
	w.claim(t, w.pool, "SDLC-1")
	w.claim(t, w.pool, "SDLC-3")
	_, tid := w.issue(t, w.pool, "SDLC-1")

	if w.auth.Bound(tid) != nil || w.auth.SummaryOnly(tid) {
		t.Fatal("в режиме заголовка токен задачи ограничен")
	}
	if err := w.auth.RequireOwnTaskKey(tid, "SDLC-3"); err != nil {
		t.Fatalf("в режиме заголовка: %v", err)
	}
	if _, err := w.auth.IssueToken(ctx, tid, IssueTokenInput{Name: "x"}); err != nil {
		t.Fatalf("в режиме заголовка выпуск токеном задачи: %v", err)
	}
}

// Критерий 3: миграция откатывается и накатывается заново на базе с
// токенами api и session, и они после этого работают.
func TestTaskTokenMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	w.claim(t, w.pool, "SDLC-1")
	w.issue(t, w.pool, "SDLC-1")
	if _, err := w.store.DB().Exec(ctx, `
		INSERT INTO agent_tokens (principal_id, name, token_hash, kind, expires_at)
		VALUES ($1, 'sess', 'session-hash', 'session', now() + interval '1 hour')`, w.admin.Principal.ID); err != nil {
		t.Fatal(err)
	}

	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if _, err := w.store.DB().Exec(ctx, read("000027_task_tokens.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if w.taskTokens(t) != 0 {
		t.Fatal("down оставил токены задачи")
	}
	if _, err := w.store.DB().Exec(ctx, read("000027_task_tokens.up.sql")); err != nil {
		t.Fatalf("повторный up: %v", err)
	}
	if _, err := w.auth.Authenticate(ctx, "bootstrap-token-0123456789"); err != nil {
		t.Fatalf("токен api после down/up: %v", err)
	}
	var sessions int
	if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM agent_tokens WHERE kind = 'session'`).
		Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("сессия после down/up: %v %d", err, sessions)
	}

	// Ограничения схемы: токен задачи без прогона и личный с прогоном.
	if _, err := w.store.DB().Exec(ctx, `
		INSERT INTO agent_tokens (principal_id, name, token_hash, kind)
		VALUES ($1, 'bad', 'bad-hash', 'task')`, w.admin.Principal.ID); err == nil {
		t.Fatal("токен задачи без прогона записан")
	}
	if _, err := w.store.DB().Exec(ctx, `
		INSERT INTO agent_tokens (principal_id, name, token_hash, kind)
		VALUES ($1, 'bad', 'bad-hash2', 'robot')`, w.admin.Principal.ID); err == nil {
		t.Fatal("неизвестный вид токена записан")
	}
}
