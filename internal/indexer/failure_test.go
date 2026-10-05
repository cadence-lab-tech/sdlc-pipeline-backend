package indexer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Сигнал «репозиторий не читается» (CLAB-79). Его пишет воркер и читает api,
// поэтому проверяется на связке «источник плюс база»: правило живёт в SQL,
// а серия провалов чтения головы — в таблице, которую заполняет Enqueue.

// scriptedSource — источник, у которого голова каждой ветки задаётся
// отдельно: sha либо ошибка. Дерево и содержимое — от fakeSource.
type scriptedSource struct {
	fakeSource
	// heads — "имя@ветка" → sha; errs — "имя@ветка" → отказ.
	heads map[string]string
	errs  map[string]error
}

func (s *scriptedSource) ForRepository(context.Context, Credentials) (Source, error) { return s, nil }

func (s *scriptedSource) Head(_ context.Context, repo Repo, branch string) (string, error) {
	key := repo.Name + "@" + branch
	if err := s.errs[key]; err != nil {
		return "", err
	}
	if head, ok := s.heads[key]; ok {
		return head, nil
	}
	return "", fmt.Errorf("репозиторий %s недоступен", repo)
}

// failureOf — failure строки состояния индекса по репозиторию.
func failureOf(t *testing.T, store *postgres.Store, projectID uuid.UUID, repository string) (postgres.IndexStatusRow, *postgres.IndexFailure) {
	t.Helper()
	rows, err := store.IndexStatus(context.Background(), store.DB(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Repository == repository {
			return r, r.Failure
		}
	}
	t.Fatalf("репозитория %s нет в состоянии индекса", repository)
	return postgres.IndexStatusRow{}, nil
}

// retryNow снимает паузу между попытками: ждать пять минут тесту незачем.
func retryNow(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE repo_index_runs SET next_attempt_at = now() WHERE state = 'queued'`); err != nil {
		t.Fatal(err)
	}
}

func demoProject(t *testing.T, store *postgres.Store) uuid.UUID {
	t.Helper()
	project, err := store.GetProjectByKey(context.Background(), store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	return project.ID
}

// B1, B2, B3, B8: одна неудачная попытка прогона — ещё не сигнал; вторая —
// сигнал; прогон в failed — сигнал с полным текстом; завершённый прогон его
// снимает.
func TestRunFailureSignal(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})
	projectID := demoProject(t, store)

	if _, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{Repository: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	// Голова читается, а дерева коммита у источника нет: прогон падает на
	// листинге, и это не постоянная ошибка — он уходит на повтор.
	src := &fakeSource{repo: "api", head: "c1", commits: map[string]map[string]string{}}
	ix := newTestIndexer(t, store, src)
	if n, err := ix.Enqueue(ctx); err != nil || n != 1 {
		t.Fatalf("постановка в очередь: %d, %v", n, err)
	}
	if _, failure := failureOf(t, store, projectID, "acme/api"); failure != nil {
		t.Fatalf("сигнал у прогона, который ещё не начинался: %+v", failure)
	}

	fail := func() {
		t.Helper()
		retryNow(t, pool)
		if run, err := ix.RunNext(ctx); err != nil || run != nil {
			t.Fatalf("прогон: %+v, %v", run, err)
		}
	}

	// B2: первая попытка упала — строка как раньше, сигнала нет.
	fail()
	row, failure := failureOf(t, store, projectID, "acme/api")
	if failure != nil {
		t.Fatalf("сигнал после одной неудачной попытки: %+v", failure)
	}
	if deref(row.State) != "queued" || deref(row.Error) == "" || row.Attempt == nil || *row.Attempt != 1 {
		t.Fatalf("строка после первой попытки: state=%q error=%q attempt=%v", deref(row.State), deref(row.Error), row.Attempt)
	}

	// B3: вторая попытка упала, прогон ещё в очереди — сигнал есть.
	fail()
	row, failure = failureOf(t, store, projectID, "acme/api")
	if deref(row.State) != "queued" || *row.Attempt != 2 {
		t.Fatalf("строка после второй попытки: state=%q attempt=%d", deref(row.State), *row.Attempt)
	}
	if failure == nil || failure.Source != "run" || failure.Error != deref(row.Error) || failure.Since.IsZero() {
		t.Fatalf("сигнал после второй попытки: %+v", failure)
	}
	since := failure.Since

	// B1: попытки исчерпаны — прогон в failed, в сигнале полный текст ошибки.
	fail()
	row, failure = failureOf(t, store, projectID, "acme/api")
	if deref(row.State) != "failed" {
		t.Fatalf("прогон в состоянии %q после трёх попыток", deref(row.State))
	}
	if failure == nil || failure.Source != "run" || !strings.Contains(failure.Error, "коммита c1 нет") ||
		failure.Error != deref(row.Error) {
		t.Fatalf("сигнал у проваленного прогона: %+v", failure)
	}
	if !failure.Since.Equal(since) {
		t.Fatalf("начало серии сдвинулось: %s → %s", since, failure.Since)
	}
	if n, err := store.CountUnreadableRepositories(ctx, store.DB(), projectID); err != nil || n != 1 {
		t.Fatalf("счётчик нечитаемых: %d, %v", n, err)
	}

	// B8: источник починили — плановый обход переоткрывает прогон, и
	// завершённый прогон снимает сигнал.
	src.commits["c1"] = map[string]string{"internal/workflow/engine.go": engineV1}
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, failure := failureOf(t, store, projectID, "acme/api"); failure == nil {
		t.Fatal("сигнал снят до того, как прогон завершился")
	}
	retryNow(t, pool)
	if run, err := ix.RunNext(ctx); err != nil || run == nil {
		t.Fatalf("прогон после починки: %+v, %v", run, err)
	}
	row, failure = failureOf(t, store, projectID, "acme/api")
	if failure != nil || deref(row.State) != "completed" {
		t.Fatalf("после завершённого прогона: state=%q failure=%+v", deref(row.State), failure)
	}
	// Остальные репозитории демонстрационного проекта этому источнику
	// недоступны и за два прохода сами стали нечитаемыми: счётчик обязан
	// совпасть с числом строк с failure, и acme/api среди них уже нет.
	rows, err := store.IndexStatus(ctx, store.DB(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	unreadable := 0
	for _, r := range rows {
		if r.Failure != nil {
			unreadable++
		}
	}
	if n, err := store.CountUnreadableRepositories(ctx, store.DB(), projectID); err != nil || n != unreadable {
		t.Fatalf("счётчик нечитаемых после починки: %d при %d строках с failure, %v", n, unreadable, err)
	}
}

// Серия у прогонов считается от последнего завершённого: новый коммит
// каждый день не делает репозиторий «сломавшимся вчера».
func TestRunFailureSinceIsStartOfStreak(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})
	projectID := demoProject(t, store)
	repo, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{Repository: "acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO repo_index_runs (repository_id, branch, commit_sha, state, attempt, error, created_at, finished_at) VALUES
		    ($1, 'main', 'c0', 'completed', 1, NULL,   now() - interval '20 days', now() - interval '20 days'),
		    ($1, 'main', 'c1', 'failed',    3, 'упал', now() - interval '18 days', now() - interval '18 days'),
		    ($1, 'main', 'c2', 'failed',    3, 'упал', now() - interval '9 days',  now() - interval '9 days'),
		    ($1, 'main', 'c3', 'failed',    3, 'упал снова', now() - interval '1 day', now() - interval '1 day')`,
		repo.ID); err != nil {
		t.Fatal(err)
	}
	_, failure := failureOf(t, store, projectID, "acme/api")
	if failure == nil || failure.Error != "упал снова" {
		t.Fatalf("сигнал: %+v", failure)
	}
	if age := time.Since(failure.Since); age < 17*24*time.Hour || age > 19*24*time.Hour {
		t.Fatalf("«не читается с» %s назад вместо восемнадцати дней", age)
	}
}

// rejectingApp — провайдер, не принимающий приложение: 401 на обмен
// утверждения на токен установки. calls считает походы.
func rejectingApp(t *testing.T) (Source, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			// Репозитории без установки ходят общим клиентом — им отвечают
			// «не найдено», как приватному репозиторию без токена.
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := github.NewApp(srv.URL, "12345", pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	if err != nil {
		t.Fatal(err)
	}
	return NewGitHubSource(github.NewProvider(github.New(srv.URL, ""), app, nil)), &calls
}

// addInstalledRepo привязывает к демонстрационному проекту репозиторий на
// установке команды-владельца.
func addInstalledRepo(t *testing.T, store *postgres.Store, pool *pgxpool.Pool, engine *workflow.Engine, installation int64, repository string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	who, err := store.ResolveActor(ctx, store.DB(), "lead", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: installation, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: *project.OwnerTeamID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}
	repo, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repositories SET installation_id = $2 WHERE id = $1`,
		repo.ID, installation); err != nil {
		t.Fatal(err)
	}
	return repo.ID
}

// B4-B7, B10, B11: приложение отбито, прогон не заводится вовсе — провал
// чтения головы оставляет след в базе, со второго прохода он виден, начало
// серии не двигается, а прочитанная голова его снимает.
func TestHeadFailureSignal(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})
	projectID := demoProject(t, store)
	repoID := addInstalledRepo(t, store, pool, engine, 507, "acme/api")

	runs := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM repo_index_runs WHERE repository_id = $1`, repoID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	rejected, _ := rejectingApp(t)
	ix := newTestIndexer(t, store, rejected)

	// B4: один неудачный проход — сигнала нет.
	if n, err := ix.Enqueue(ctx); err != nil || n != 0 {
		t.Fatalf("первый проход: %d, %v", n, err)
	}
	if _, failure := failureOf(t, store, projectID, "acme/api"); failure != nil {
		t.Fatalf("сигнал после одного прохода: %+v", failure)
	}

	// B5: второй проход — сигнал с отказом приложения, прогонов не прибавилось.
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	_, failure := failureOf(t, store, projectID, "acme/api")
	if failure == nil || failure.Source != "head" {
		t.Fatalf("сигнал после двух проходов: %+v", failure)
	}
	if !strings.HasPrefix(failure.Error, github.ErrAppRejected.Error()) || !strings.Contains(failure.Error, "Bad credentials") {
		t.Fatalf("текст сигнала: %q", failure.Error)
	}
	if got := runs(); got != 0 {
		t.Fatalf("завелось прогонов: %d", got)
	}
	since := failure.Since

	// B6: третий проход начало серии не двигает.
	time.Sleep(20 * time.Millisecond)
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, failure = failureOf(t, store, projectID, "acme/api"); failure == nil || !failure.Since.Equal(since) {
		t.Fatalf("начало серии сдвинулось: %s → %+v", since, failure)
	}

	// B11: воркер перезапущен — сигнал в базе, а не в его памяти.
	ix = newTestIndexer(t, store, rejected)
	if _, failure = failureOf(t, store, projectID, "acme/api"); failure == nil || !failure.Since.Equal(since) {
		t.Fatalf("сигнал не пережил перезапуск воркера: %+v", failure)
	}

	// B10: индексацию выключили — строка сигнала не несёт; включили — снова.
	setEnabled := func(on bool) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE repositories SET index_enabled = $2 WHERE id = $1`, repoID, on); err != nil {
			t.Fatal(err)
		}
	}
	setEnabled(false)
	if _, failure = failureOf(t, store, projectID, "acme/api"); failure != nil {
		t.Fatalf("сигнал у репозитория с выключенной индексацией: %+v", failure)
	}
	setEnabled(true)
	if _, failure = failureOf(t, store, projectID, "acme/api"); failure == nil {
		t.Fatal("сигнал пропал после включения индексации обратно")
	}

	// Приостановленная установка — репозиторий вне целей, сигнала нет.
	if _, err := pool.Exec(ctx, `UPDATE github_installations SET suspended_at = now() WHERE installation_id = 507`); err != nil {
		t.Fatal(err)
	}
	if _, failure = failureOf(t, store, projectID, "acme/api"); failure != nil {
		t.Fatalf("сигнал у репозитория приостановленной установки: %+v", failure)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_installations SET suspended_at = NULL WHERE installation_id = 507`); err != nil {
		t.Fatal(err)
	}

	// B7: голова прочиталась — сигнала нет, прогон заведён.
	working := &fakeSource{repo: "api", head: "c1",
		commits: map[string]map[string]string{"c1": {"internal/workflow/engine.go": engineV1}}}
	ix.source = working
	if n, err := ix.Enqueue(ctx); err != nil || n != 1 {
		t.Fatalf("проход после починки: %d, %v", n, err)
	}
	if _, failure = failureOf(t, store, projectID, "acme/api"); failure != nil {
		t.Fatalf("сигнал после прочитанной головы: %+v", failure)
	}
	if run, err := ix.RunNext(ctx); err != nil || run == nil {
		t.Fatalf("прогон: %+v, %v", run, err)
	}

	// B7, вторая половина: снова сломалось и снова починилось, а коммит уже
	// проиндексирован — прогон не заводится, но сигнал снимается.
	ix.source = rejected
	for range 2 {
		if _, err := ix.Enqueue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, failure = failureOf(t, store, projectID, "acme/api"); failure == nil || failure.Source != "head" {
		t.Fatalf("сигнал после повторной поломки: %+v", failure)
	}
	if failure.Since.Before(since) || failure.Since.Equal(since) {
		t.Fatalf("новая серия унаследовала начало старой: %s", failure.Since)
	}
	ix.source = working
	if n, err := ix.Enqueue(ctx); err != nil || n != 0 {
		t.Fatalf("проход по уже проиндексированному коммиту: %d, %v", n, err)
	}
	if _, failure = failureOf(t, store, projectID, "acme/api"); failure != nil {
		t.Fatalf("сигнал не снят прочитанной головой: %+v", failure)
	}
	if got := runs(); got != 1 {
		t.Fatalf("прогонов %d вместо одного", got)
	}
}

// B9: ветки нет у провайдера — класс ошибки не фильтруется, сигнал тот же.
// Серия считается по ветке: читаемая соседняя ветка её не снимает, а
// ветка, которую перестали отслеживать, сигнала не держит.
func TestHeadFailureForMissingBranch(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})
	projectID := demoProject(t, store)

	repo, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{
		Repository: "acme/api", WatchBranches: []string{"main", "release"},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := &scriptedSource{
		fakeSource: fakeSource{repo: "api", commits: map[string]map[string]string{
			"c1": {"internal/workflow/engine.go": engineV1}}},
		heads: map[string]string{"api@main": "c1"},
		errs: map[string]error{
			"api@release": fmt.Errorf("%w: /repos/acme/api/branches/release", github.ErrNotFound)},
	}
	ix := newTestIndexer(t, store, src)

	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, failure := failureOf(t, store, projectID, "acme/api"); failure != nil {
		t.Fatalf("сигнал после одного прохода: %+v", failure)
	}
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	_, failure := failureOf(t, store, projectID, "acme/api")
	if failure == nil || failure.Source != "head" {
		t.Fatalf("сигнал по удалённой ветке: %+v", failure)
	}
	if !strings.Contains(failure.Error, "не найдено") || !strings.Contains(failure.Error, "branches/release") ||
		!strings.Contains(failure.Error, "нет ветки либо у токена нет доступа") {
		t.Fatalf("текст причины: %q", failure.Error)
	}

	// Ветку убрали из отслеживаемых — читать её больше не должны, и сигнал
	// по ней не держится.
	if _, err := pool.Exec(ctx, `UPDATE repositories SET watch_branches = '{main}' WHERE id = $1`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, failure := failureOf(t, store, projectID, "acme/api"); failure != nil {
		t.Fatalf("сигнал по ветке, которую не отслеживают: %+v", failure)
	}
}

// F3, F4, F5: счётчик сводки равен числу строк состояния индекса с failure —
// правило одно. Пять репозиториев в состояниях B1, B3, B5, B2 и B10 дают
// три; починка уменьшает оба числа одинаково.
func TestUnreadableCountMatchesIndexStatus(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})
	projectID := demoProject(t, store)

	health := func() int {
		t.Helper()
		out, err := engine.ProjectHealth(ctx, "SDLC")
		if err != nil {
			t.Fatal(err)
		}
		// Без доступа к провайдеру приложение «не настроено», а счётчик
		// считается как обычно (F9).
		if out.App.State != github.AppNotConfigured || out.App.CheckedAt != nil {
			t.Fatalf("состояние приложения в сводке: %+v", out.App)
		}
		return out.UnreadableRepositories
	}
	withFailure := func() int {
		t.Helper()
		rows, err := store.IndexStatus(ctx, store.DB(), projectID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			if r.Failure != nil {
				n++
			}
		}
		return n
	}

	// F4: нечитаемых нет — ноль, а не отсутствие поля.
	if got := health(); got != 0 || withFailure() != 0 {
		t.Fatalf("на чистом проекте нечитаемых %d, строк с failure %d", got, withFailure())
	}

	ids := map[string]uuid.UUID{}
	for _, name := range []string{"b1", "b3", "b5", "b2", "b10"} {
		repo, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{Repository: "acme/" + name})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = repo.ID
	}
	run := func(name, state string, attempt int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO repo_index_runs (repository_id, branch, commit_sha, state, attempt, error)
			VALUES ($1, 'main', 'c1', $2, $3, 'github 401 на токен установки')`,
			ids[name], state, attempt); err != nil {
			t.Fatal(err)
		}
	}
	headFails := func(name string, passes int) {
		t.Helper()
		for range passes {
			if err := store.RecordHeadFailure(ctx, store.DB(), ids[name], "main", "голова не читается"); err != nil {
				t.Fatal(err)
			}
		}
	}
	run("b1", "failed", 3) // B1
	run("b3", "queued", 2) // B3
	headFails("b5", 2)     // B5
	run("b2", "queued", 1) // B2: одна попытка — не сигнал
	headFails("b10", 2)    // B10: индексация выключена
	if _, err := pool.Exec(ctx, `UPDATE repositories SET index_enabled = false WHERE id = $1`, ids["b10"]); err != nil {
		t.Fatal(err)
	}

	if got, rows := health(), withFailure(); got != 3 || rows != 3 {
		t.Fatalf("нечитаемых %d, строк с failure %d — ожидалось по три", got, rows)
	}

	// F5: чтение восстановилось у двух — оба числа уменьшились на два.
	if _, err := pool.Exec(ctx, `
		UPDATE repo_index_runs SET state = 'completed', error = NULL, finished_at = now()
		WHERE repository_id = $1`, ids["b1"]); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearHeadFailure(ctx, store.DB(), ids["b5"], "main"); err != nil {
		t.Fatal(err)
	}
	if got, rows := health(), withFailure(); got != 1 || rows != 1 {
		t.Fatalf("после починки нечитаемых %d, строк с failure %d — ожидалось по одному", got, rows)
	}
}
