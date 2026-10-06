package indexer

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Синхронизация состояния PR под гейт external_check (CLAB-84): PR читается
// теми же учётными данными, что и его репозиторий при индексации, —
// github.CredentialRefs.Resolve через ForRepository, без своего порядка.
// Провайдер настоящий, API и приложение — на httptest (newCredentialProvider),
// база — тестовая: очередь синхронизации живёт в SQL.

const (
	backendRepo  = "sdlc-pipeline-backend"
	frontendRepo = "sdlc-pipeline-frontend"
	docsRepo     = "sdlc-pipeline-docs"
)

type syncFixture struct {
	store *postgres.Store
	pool  *pgxpool.Pool
	probe *credentialProbe
	ix    *Indexer
	log   *bytes.Buffer
}

func newSyncFixture(t *testing.T, instanceToken string) *syncFixture {
	t.Helper()
	store, pool := testStore(t)
	probe, provider := newCredentialProvider(t, instanceToken)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ix := New(store, NewGitHubSource(provider), nil, log, Options{ChunkChars: 400})
	return &syncFixture{store: store, pool: pool, probe: probe, ix: ix, log: &buf}
}

// exec объявляет учётные данные прямо в базе — так, как их положили бы
// настройки проекта и репозитория.
func (f *syncFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// installation заводит установку приложения на команду демонстрационного
// проекта.
func (f *syncFixture) installation(t *testing.T, id int64) {
	t.Helper()
	ctx := context.Background()
	project, err := f.store.GetProjectByKey(ctx, f.store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	who, err := f.store.ResolveActor(ctx, f.store.DB(), "lead", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpsertInstallation(ctx, f.store.DB(), postgres.InstallationInput{
		InstallationID: id, AccountLogin: "cadence-lab-tech", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: *project.OwnerTeamID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

// linkPR привязывает к демонстрационной задаче SDLC-1 открытый PR
// репозитория проекта.
func (f *syncFixture) linkPR(t *testing.T, repo string, number int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO task_repo_refs (task_id, repository_id, ref_type, ref, state)
		SELECT t.id, r.id, 'pull_request', $2, 'open'
		FROM tasks t
		JOIN repositories r ON r.project_id = t.project_id
		WHERE t.key = 'SDLC-1' AND r.name = $1
		RETURNING id`, repo, strconv.Itoa(number)).Scan(&id); err != nil {
		t.Fatalf("привязка PR %s#%d: %v", repo, number, err)
	}
	return id
}

type refSnapshot struct {
	State, Head, Conclusion string
	SyncedAt                *time.Time
}

func (f *syncFixture) ref(t *testing.T, id uuid.UUID) refSnapshot {
	t.Helper()
	var s refSnapshot
	if err := f.pool.QueryRow(context.Background(), `
		SELECT coalesce(state, ''), coalesce(head_sha, ''),
		       coalesce(checks->>'conclusion', ''), synced_at
		FROM task_repo_refs WHERE id = $1`, id).Scan(&s.State, &s.Head, &s.Conclusion, &s.SyncedAt); err != nil {
		t.Fatal(err)
	}
	return s
}

// pulled — с какими токенами читался PR.
func (f *syncFixture) pulled(repo string, number int) []string {
	pulls, _ := f.probe.pullRequests()
	return authorizations(pulls, fmt.Sprintf("/repos/cadence-lab-tech/%s/pulls/%d", repo, number))
}

// checked — с какими токенами читались проверки коммита.
func (f *syncFixture) checked(repo, head string) []string {
	_, checks := f.probe.pullRequests()
	return authorizations(checks, fmt.Sprintf("/repos/cadence-lab-tech/%s/commits/%s/check-runs", repo, head))
}

func authorizations(reqs []probeRequest, path string) []string {
	out := []string{}
	for _, r := range reqs {
		if r.Path == path {
			out = append(out, r.Authorization)
		}
	}
	return out
}

// Критерии 1, 3 и 4: инстанс без токена, у репозитория установка — PR и его
// проверки читаются токеном установки. Соседняя ссылка, у репозитория
// которой выбранный источник отказал, не читается ничем другим, и проход
// это не останавливает.
func TestSyncRepoRefsReadsWithRepositoryCredentials(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t, "")

	f.installation(t, 11)
	f.installation(t, 22)
	f.exec(t, `UPDATE repositories SET installation_id = 11 WHERE name = $1`, backendRepo)
	// У проекта есть и установка, и секрет: на них отказавший секрет
	// фронтенда не подменяется.
	f.exec(t, `UPDATE projects SET github_installation_id = 22, github_credential_ref = 'project-secret'
		WHERE key = 'SDLC'`)
	f.exec(t, `UPDATE repositories SET credential_ref = 'nowhere' WHERE name = $1`, frontendRepo)

	good := f.linkPR(t, backendRepo, 128)
	broken := f.linkPR(t, frontendRepo, 129)

	n, err := f.ix.SyncRepoRefs(ctx, 50)
	if err != nil || n != 1 {
		t.Fatalf("синхронизировано %d, ошибка %v; ждали 1 без ошибки", n, err)
	}

	got := f.ref(t, good)
	if got.State != "open" || got.Head != "head-128" || got.Conclusion != "success" || got.SyncedAt == nil {
		t.Fatalf("PR репозитория с установкой не синхронизирован: %+v", got)
	}
	if a := f.pulled(backendRepo, 128); len(a) != 1 || a[0] != "Bearer ghs_installation_11" {
		t.Fatalf("PR читался с %q, ждали один запрос токеном установки 11", a)
	}
	if a := f.checked(backendRepo, "head-128"); len(a) != 1 || a[0] != "Bearer ghs_installation_11" {
		t.Fatalf("проверки PR читались с %q, ждали один запрос токеном установки 11", a)
	}

	// Отказавший источник: к провайдеру за PR не ходили, отметки нет.
	if a := f.pulled(frontendRepo, 129); len(a) != 0 {
		t.Fatalf("PR с отказавшим секретом читали с %q", a)
	}
	if s := f.ref(t, broken); s.SyncedAt != nil {
		t.Fatalf("у нечитаемой ссылки сдвинулась отметка синхронизации: %+v", s)
	}
	logged := f.log.String()
	if strings.Count(logged, "level=WARN") != 1 {
		t.Fatalf("ждали одно предупреждение, в логе:\n%s", logged)
	}
	for _, want := range []string{"task=SDLC-1", "repository=cadence-lab-tech/" + frontendRepo, "pr=129", "nowhere"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("в предупреждении нет %q:\n%s", want, logged)
		}
	}
}

// Критерий 2: из нескольких объявленных источников PR читается тем, который
// выбирает Resolve (решение 0034); ничего не объявлено — токеном инстанса.
func TestSyncRepoRefsFollowsDecision0034Order(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *syncFixture)
		want  string
	}{
		{"секрет репозитория раньше установки проекта", func(t *testing.T, f *syncFixture) {
			f.exec(t, `UPDATE projects SET github_installation_id = 22 WHERE key = 'SDLC'`)
			f.exec(t, `UPDATE repositories SET credential_ref = 'repo-secret' WHERE name = $1`, backendRepo)
		}, "Bearer token-repo"},
		{"установка проекта раньше секрета проекта", func(t *testing.T, f *syncFixture) {
			f.exec(t, `UPDATE projects SET github_installation_id = 22, github_credential_ref = 'project-secret'
				WHERE key = 'SDLC'`)
		}, "Bearer ghs_installation_22"},
		{"только секрет проекта", func(t *testing.T, f *syncFixture) {
			f.exec(t, `UPDATE projects SET github_credential_ref = 'project-secret' WHERE key = 'SDLC'`)
		}, "Bearer token-project"},
		{"ничего не объявлено — токен инстанса", func(*testing.T, *syncFixture) {}, "Bearer token-instance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSyncFixture(t, "token-instance")
			tc.setup(t, f)
			f.linkPR(t, backendRepo, 128)

			if n, err := f.ix.SyncRepoRefs(context.Background(), 50); err != nil || n != 1 {
				t.Fatalf("синхронизировано %d, ошибка %v", n, err)
			}
			if a := f.pulled(backendRepo, 128); len(a) != 1 || a[0] != tc.want {
				t.Fatalf("PR читался с %q, ждали %q", a, tc.want)
			}
			if a := f.checked(backendRepo, "head-128"); len(a) != 1 || a[0] != tc.want {
				t.Fatalf("проверки читались с %q, ждали %q", a, tc.want)
			}
		})
	}
}

// Решение человека 1: репозитории со снятой или приостановленной установкой
// и убранные из установки (unavailable_at) синхронизация пропускает тем же
// условием, что и индексация, — без запросов и без предупреждений.
func TestSyncRepoRefsSkipsUnavailableRepositories(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t, "token-instance")

	f.installation(t, 11)
	f.installation(t, 12)
	f.exec(t, `UPDATE github_installations SET suspended_at = now() WHERE installation_id = 11`)
	f.exec(t, `UPDATE github_installations SET removed_at = now() WHERE installation_id = 12`)
	f.exec(t, `UPDATE repositories SET installation_id = 11 WHERE name = $1`, backendRepo)
	f.exec(t, `UPDATE repositories SET installation_id = 12 WHERE name = $1`, frontendRepo)
	f.exec(t, `UPDATE repositories SET unavailable_at = now() WHERE name = $1`, docsRepo)
	f.exec(t, `INSERT INTO repositories (project_id, owner, name, clone_url)
		SELECT id, 'cadence-lab-tech', 'sdlc-pipeline-live', 'https://github.com/cadence-lab-tech/sdlc-pipeline-live.git'
		FROM projects WHERE key = 'SDLC'`)

	suspended := f.linkPR(t, backendRepo, 128)
	removed := f.linkPR(t, frontendRepo, 129)
	unavailable := f.linkPR(t, docsRepo, 130)
	live := f.linkPR(t, "sdlc-pipeline-live", 131)

	toSync, err := f.store.RepoRefsToSync(ctx, f.store.DB(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(toSync) != 1 || toSync[0].ID != live {
		ids := []uuid.UUID{}
		for _, r := range toSync {
			ids = append(ids, r.ID)
		}
		t.Fatalf("в выборке %v, ждали только живую ссылку %s (приостановленная %s, снятая %s, недоступная %s)",
			ids, live, suspended, removed, unavailable)
	}

	if n, err := f.ix.SyncRepoRefs(ctx, 50); err != nil || n != 1 {
		t.Fatalf("синхронизировано %d, ошибка %v", n, err)
	}
	for repo, number := range map[string]int{backendRepo: 128, frontendRepo: 129, docsRepo: 130} {
		if a := f.pulled(repo, number); len(a) != 0 {
			t.Fatalf("PR недоступного репозитория %s читали с %q", repo, a)
		}
	}
	if logged := f.log.String(); strings.Contains(logged, "level=WARN") {
		t.Fatalf("по недоступным репозиториям не должно быть предупреждений:\n%s", logged)
	}
}

// Решение человека 2: ссылки, которые не читаются, не держат очередь. Их не
// меньше лимита прохода, и на bf48b46 они стояли первыми вечно (synced_at
// NULLS FIRST, а неудача отметку не двигала) — читаемая ссылка не
// обновлялась никогда. Неудачная попытка уводит ссылку в конец очереди.
func TestSyncRepoRefsUnreadableDoNotStarveQueue(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t, "token-instance")
	const limit = 2

	readable := f.linkPR(t, backendRepo, 128)
	f.exec(t, `UPDATE task_repo_refs SET synced_at = now() - interval '1 hour' WHERE id = $1`, readable)
	before := *f.ref(t, readable).SyncedAt

	unreadable := []uuid.UUID{}
	for n := missingPullRequest; n < missingPullRequest+limit; n++ {
		unreadable = append(unreadable, f.linkPR(t, backendRepo, n))
	}

	refreshed := 0
	requests := 0
	for pass := 1; pass <= 3 && refreshed == 0; pass++ {
		if _, err := f.ix.SyncRepoRefs(ctx, limit); err != nil {
			t.Fatal(err)
		}
		pulls, _ := f.probe.pullRequests()
		if got := len(pulls) - requests; got > limit {
			t.Fatalf("проход %d прочитал %d PR при лимите %d", pass, got, limit)
		}
		requests = len(pulls)
		if s := f.ref(t, readable).SyncedAt; s != nil && s.After(before) {
			refreshed = pass
		}
	}
	if refreshed == 0 {
		t.Fatalf("читаемая ссылка не синхронизирована за 3 прохода: очередь заняли нечитаемые")
	}
	for _, id := range unreadable {
		if s := f.ref(t, id); s.SyncedAt != nil {
			t.Fatalf("у нечитаемой ссылки появилась отметка синхронизации: %+v", s)
		}
	}
}
