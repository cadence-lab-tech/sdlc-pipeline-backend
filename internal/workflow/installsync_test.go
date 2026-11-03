package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github/githubtest"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Опрос установок GitHub App (CLAB-395): база приходит к тому же состоянию,
// что дал бы вебхук, только по полному ответу провайдера, без лишних UPDATE и
// не дороже запроса на установку за проход.

type syncFixture struct {
	t       *testing.T
	ctx     context.Context
	store   *postgres.Store
	engine  *Engine
	gh      *githubtest.Server
	src     *github.Provider
	project domain.Project
	team    domain.Team
	who     domain.Principal
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	store, engine, _, _, _ := setupPhase4(t, AuthHeader)
	project, team := ownerTeam(t, store)
	gh := githubtest.New(t)
	return &syncFixture{
		t: t, ctx: context.Background(), store: store, engine: engine,
		gh: gh, src: gh.Provider(t), project: project, team: team,
		who: mustActor(t, store, "user:lead"),
	}
}

// connect подключает установку команде, как кнопка в настройках.
func (f *syncFixture) connect(id int64) {
	f.t.Helper()
	if _, err := f.store.UpsertInstallation(f.ctx, f.store.DB(), postgres.InstallationInput{
		InstallationID: id, AccountLogin: fmt.Sprintf("acct-%d", id), AccountID: id, AccountType: "Organization",
		OwnerTeamID: f.team.ID, ConnectedBy: f.who.ID,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// repo привязывает репозиторий проекту через установку.
func (f *syncFixture) repo(id int64, full string) {
	f.t.Helper()
	owner, name, _ := strings.Cut(full, "/")
	if _, err := f.store.DB().Exec(f.ctx, `INSERT INTO repositories
		(project_id, owner, name, clone_url, installation_id) VALUES ($1, $2, $3, $4, $5)`,
		f.project.ID, owner, name, "https://github.com/"+full+".git", id); err != nil {
		f.t.Fatal(err)
	}
}

func (f *syncFixture) sync() []InstallationChange {
	f.t.Helper()
	changes, err := f.engine.SyncInstallations(f.ctx, f.src)
	if err != nil {
		f.t.Fatalf("сверка: %v", err)
	}
	return changes
}

func (f *syncFixture) installation(id int64) domain.GitHubInstallation {
	f.t.Helper()
	i, err := f.store.GetInstallation(f.ctx, f.store.DB(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return i
}

func (f *syncFixture) unavailableAt(full string) *time.Time {
	f.t.Helper()
	owner, name, _ := strings.Cut(full, "/")
	var at *time.Time
	if err := f.store.DB().QueryRow(f.ctx, `SELECT unavailable_at FROM repositories
		WHERE project_id = $1 AND owner = $2 AND name = $3`, f.project.ID, owner, name).Scan(&at); err != nil {
		f.t.Fatal(err)
	}
	return at
}

// snapshot — состояние всего, что сверка может поменять: установки и
// доступность репозиториев, со временем последнего изменения строки.
func (f *syncFixture) snapshot() string {
	f.t.Helper()
	rows, err := f.store.DB().Query(f.ctx, `
		SELECT 'i' || installation_id || ' ' || coalesce(suspended_at::text, '-') || ' ' ||
		       coalesce(removed_at::text, '-') || ' ' || updated_at::text
		FROM github_installations
		UNION ALL
		SELECT 'r' || owner || '/' || name || ' ' || coalesce(unavailable_at::text, '-')
		FROM repositories
		ORDER BY 1`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		lines = append(lines, s)
	}
	return strings.Join(lines, "\n")
}

func changeSet(changes []InstallationChange) string {
	var out []string
	for _, c := range changes {
		out = append(out, fmt.Sprintf("%d %s %s", c.InstallationID, c.Change, c.Repository))
	}
	return strings.TrimSpace(strings.Join(out, "; "))
}

// AC-3, AC-4, AC-6: приостановка, возобновление, снятие; репозиторий убран и
// возвращён; повтор ничего не меняет; неподключённая установка не заводится.
func TestInstallationSyncFollowsProvider(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(601)
	f.connect(602)
	f.repo(601, "acme/api")
	f.repo(601, "acme/web")
	f.repo(602, "beta/core")
	// Установка проекта без строки в github_installations и её репозиторий:
	// вебхук их не трогает, опрос — тоже.
	f.gh.Installations[601] = &githubtest.Installation{Repos: []string{"acme/api", "acme/web"}}
	f.gh.Installations[602] = &githubtest.Installation{Repos: []string{"beta/core"}}
	f.gh.Installations[603] = &githubtest.Installation{Suspended: true}

	if got := f.sync(); len(got) != 0 {
		t.Fatalf("согласованное состояние дало изменения: %s", changeSet(got))
	}
	if _, err := f.store.GetInstallation(f.ctx, f.store.DB(), 603); err == nil {
		t.Fatal("опрос завёл установку, которую никто не подключал")
	}

	// Репозиторий убран из установки.
	f.gh.Installations[601].Repos = []string{"acme/api"}
	if got := changeSet(f.sync()); got != "601 repository_unavailable acme/web" {
		t.Fatalf("убранный репозиторий: %q", got)
	}
	first := f.unavailableAt("acme/web")
	if first == nil || f.unavailableAt("acme/api") != nil {
		t.Fatalf("доступность после снятия: web=%v api=%v", first, f.unavailableAt("acme/api"))
	}
	if names := targetNames(t, f.store); names["acme/web"] || !names["acme/api"] {
		t.Fatalf("цели индексации после снятия репозитория: %v", names)
	}
	// Повтор с тем же ответом: метка времени та же, изменений нет.
	before := f.snapshot()
	if got := f.sync(); len(got) != 0 {
		t.Fatalf("повтор дал изменения: %s", changeSet(got))
	}
	if after := f.snapshot(); after != before {
		t.Fatalf("повтор переписал строки:\n%s\n---\n%s", before, after)
	}

	// Репозиторий вернули.
	f.gh.Installations[601].Repos = []string{"acme/api", "acme/web"}
	if got := changeSet(f.sync()); got != "601 repository_available acme/web" {
		t.Fatalf("возвращённый репозиторий: %q", got)
	}
	if f.unavailableAt("acme/web") != nil {
		t.Fatal("возвращённый репозиторий остался недоступным")
	}

	// Приостановка и возобновление.
	f.gh.Installations[601].Suspended = true
	if got := changeSet(f.sync()); got != "601 installation_suspended" {
		t.Fatalf("приостановка: %q", got)
	}
	if f.installation(601).SuspendedAt == nil {
		t.Fatal("suspended_at не проставлен")
	}
	if names := targetNames(t, f.store); names["acme/api"] || names["acme/web"] || !names["beta/core"] {
		t.Fatalf("цели индексации при приостановке: %v", names)
	}
	f.gh.Installations[601].Suspended = false
	if got := changeSet(f.sync()); got != "601 installation_unsuspended" {
		t.Fatalf("возобновление: %q", got)
	}
	if f.installation(601).SuspendedAt != nil {
		t.Fatal("suspended_at не снят")
	}
	if names := targetNames(t, f.store); !names["acme/api"] || !names["acme/web"] {
		t.Fatalf("цели индексации после возобновления: %v", names)
	}

	// Снятие: в полном списке нет, запрос по идентификатору — 404.
	delete(f.gh.Installations, 602)
	f.gh.ResetRequests()
	if got := changeSet(f.sync()); got != "602 installation_removed" {
		t.Fatalf("снятие: %q", got)
	}
	if f.gh.Requests(githubtest.KindGet) != 1 {
		t.Fatalf("снятие не подтверждено запросом по идентификатору: %d", f.gh.Requests(githubtest.KindGet))
	}
	if f.installation(602).RemovedAt == nil {
		t.Fatal("removed_at не проставлен")
	}
	if names := targetNames(t, f.store); names["beta/core"] {
		t.Fatalf("репозиторий снятой установки индексируется: %v", names)
	}

	// Снятая установка снова видна у провайдера — опрос её не оживляет:
	// подключают заново кнопкой.
	f.gh.Installations[602] = &githubtest.Installation{Repos: []string{"beta/core"}}
	f.gh.ResetRequests()
	if got := f.sync(); len(got) != 0 {
		t.Fatalf("снятая установка ожила опросом: %s", changeSet(got))
	}
	if f.installation(602).RemovedAt == nil {
		t.Fatal("removed_at снят опросом")
	}
	if f.gh.Requests(githubtest.KindGet) != 0 || f.gh.Requests(githubtest.KindRepos) != 1 {
		t.Fatalf("за снятой установкой ходили: get=%d repos=%d",
			f.gh.Requests(githubtest.KindGet), f.gh.Requests(githubtest.KindRepos))
	}
}

// Переименованный репозиторий не считается убранным: провайдер отдаёт его по
// старому имени с новым full_name. Публичный репозиторий вне установки —
// убран, как сказал бы вебхук, хотя читается и так.
func TestInstallationSyncConfirmsMissingRepositories(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(611)
	f.repo(611, "acme/old")
	f.repo(611, "acme/pub")
	f.repo(611, "acme/gone")
	f.gh.Installations[611] = &githubtest.Installation{Repos: []string{"acme/new"}}
	f.gh.Renamed["acme/old"] = "acme/new"
	f.gh.Public["acme/pub"] = true

	got := changeSet(f.sync())
	if got != "611 repository_unavailable acme/gone; 611 repository_unavailable acme/pub" {
		t.Fatalf("перепроверка пропавших: %q", got)
	}
	if f.unavailableAt("acme/old") != nil {
		t.Fatal("переименованный репозиторий помечен недоступным")
	}
	// Уже недоступные повторно не перепроверяются: запрос репозитория — один
	// раз на пропажу, а не на каждый проход.
	f.gh.ResetRequests()
	f.sync()
	if n := f.gh.Requests(githubtest.KindRepo); n != 1 {
		t.Fatalf("перепроверок на повторе %d, ждали одну (переименованный)", n)
	}
}

// AC-5: сбой провайдера ничего не меняет у той установки, к которой
// относится, и проход завершается ошибкой. Сбой списка установок не меняет
// ничего вовсе.
func TestInstallationSyncLeavesStateOnFailure(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(621)
	f.connect(622)
	f.repo(621, "acme/api")
	f.repo(621, "acme/web")
	f.repo(622, "beta/core")
	// Причины для изменений есть у обеих: 621 потеряла репозиторий, 622
	// приостановлена.
	f.gh.Installations[621] = &githubtest.Installation{Repos: []string{"acme/api"}}
	f.gh.Installations[622] = &githubtest.Installation{Suspended: true, Repos: []string{"beta/core"}}
	before := f.snapshot()

	cases := []struct {
		name string
		kind string
		code int
		want error
	}{
		{"список: 5xx", githubtest.KindList, http.StatusBadGateway, nil},
		{"список: 429", githubtest.KindList, http.StatusTooManyRequests, nil},
		{"список: 403 по лимиту", githubtest.KindList, http.StatusForbidden, nil},
		{"список: сеть", githubtest.KindList, githubtest.Drop, nil},
		{"список: приложение отбито", githubtest.KindList, http.StatusUnauthorized, github.ErrAppRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.gh.Fail = map[string]int{tc.kind: tc.code}
			defer func() { f.gh.Fail = map[string]int{} }()
			changes, err := f.engine.SyncInstallations(f.ctx, f.src)
			if err == nil {
				t.Fatal("сбой провайдера не дал ошибки прохода")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("ошибка %v, ждали %v", err, tc.want)
			}
			if len(changes) != 0 {
				t.Fatalf("изменения при сбое списка: %s", changeSet(changes))
			}
			if after := f.snapshot(); after != before {
				t.Fatalf("база изменилась при сбое:\n%s\n---\n%s", before, after)
			}
		})
	}

	// Сбой списка репозиториев одной установки: её репозитории не трогаются,
	// вторая установка сверяется, проход — с ошибкой.
	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{"репозитории: 5xx", func() { f.gh.Fail = map[string]int{githubtest.KindRepos: http.StatusBadGateway} }},
		{"репозитории: сеть", func() { f.gh.Fail = map[string]int{githubtest.KindRepos: githubtest.Drop} }},
		{"репозитории: токен не выписан", func() { f.gh.Fail = map[string]int{githubtest.KindToken: http.StatusInternalServerError} }},
		{"репозитории: оборванная пагинация", func() { f.gh.UnderreportTotal = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 622 возвращается к исходному, чтобы каждый случай видел её
			// приостановку заново.
			if _, err := f.store.SetInstallationState(f.ctx, f.store.DB(), 622, postgres.InstallationUnsuspended); err != nil {
				t.Fatal(err)
			}
			tc.setup()
			defer func() { f.gh.Fail = map[string]int{}; f.gh.UnderreportTotal = false }()
			changes, err := f.engine.SyncInstallations(f.ctx, f.src)
			if err == nil || !strings.Contains(err.Error(), "установка 621") {
				t.Fatalf("сбой чтения репозиториев 621: %v", err)
			}
			if got := changeSet(changes); got != "622 installation_suspended" {
				t.Fatalf("изменения при сбое одной установки: %q", got)
			}
			if f.unavailableAt("acme/web") != nil {
				t.Fatal("репозиторий помечен недоступным по неполному ответу")
			}
		})
	}
}

// AC-7: на M установок — ceil(M/100) запросов списка и по одному запросу
// репозиториев на живую установку с репозиториями; приостановленные и
// снятые репозитории не запрашивают, число запросов от числа репозиториев не
// зависит.
func TestInstallationSyncRequestBudget(t *testing.T) {
	f := newSyncFixture(t)
	for id := int64(1000); id < 1150; id++ {
		f.gh.Installations[id] = &githubtest.Installation{}
	}
	f.connect(1001)
	f.connect(1002)
	f.connect(1003)
	f.connect(1004)
	for i := range 40 {
		full := fmt.Sprintf("acme/r%02d", i)
		f.repo(1001, full)
		f.gh.Installations[1001].Repos = append(f.gh.Installations[1001].Repos, full)
	}
	f.repo(1002, "beta/core")
	f.gh.Installations[1002].Repos = []string{"beta/core"}
	f.repo(1003, "gamma/core")
	f.gh.Installations[1003].Suspended = true
	f.repo(1004, "delta/core")
	if _, err := f.store.SetInstallationState(f.ctx, f.store.DB(), 1004, postgres.InstallationRemoved); err != nil {
		t.Fatal(err)
	}

	for pass := range 2 {
		f.gh.ResetRequests()
		got := f.sync()
		if pass == 0 && changeSet(got) != "1003 installation_suspended" {
			t.Fatalf("первый проход: %s", changeSet(got))
		}
		if pass == 1 && len(got) != 0 {
			t.Fatalf("второй проход: %s", changeSet(got))
		}
		for kind, want := range map[string]int{
			githubtest.KindList:   2, // 150 установок — две страницы
			githubtest.KindGet:    0,
			githubtest.KindRepos:  2, // 1001 и 1002; 1003 приостановлена, 1004 снята
			githubtest.KindRepo:   0,
			githubtest.KindBranch: 0,
		} {
			if n := f.gh.Requests(kind); n != want {
				t.Fatalf("проход %d: запросов %s %d, ждали %d", pass, kind, n, want)
			}
		}
		// Токены выписываются раз на установку и живут в кэше.
		if n := f.gh.Requests(githubtest.KindToken); pass == 0 && n != 2 || pass == 1 && n != 0 {
			t.Fatalf("проход %d: токенов выписано %d", pass, n)
		}
	}
}

// AC-8: вебхук и опрос в любом порядке дают одно и то же состояние, и опрос
// не переписывает строку, которую вебхук уже согласовал.
func TestInstallationSyncAgreesWithWebhook(t *testing.T) {
	f := newSyncFixture(t)
	f.connect(631)
	f.repo(631, "acme/api")
	f.repo(631, "acme/web")
	f.gh.Installations[631] = &githubtest.Installation{Repos: []string{"acme/api", "acme/web"}}
	event := func(name, body string) {
		t.Helper()
		if _, err := f.engine.HandleGitHubEvent(f.ctx, name, uuid.NewString(), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	// Вебхук, затем опрос.
	event("installation", `{"action":"suspend","installation":{"id":631}}`)
	f.gh.Installations[631].Suspended = true
	afterWebhook := f.snapshot()
	if got := f.sync(); len(got) != 0 {
		t.Fatalf("опрос после вебхука: %s", changeSet(got))
	}
	if after := f.snapshot(); after != afterWebhook {
		t.Fatalf("опрос переписал согласованное вебхуком:\n%s\n---\n%s", afterWebhook, after)
	}

	// Опрос, затем вебхук: время приостановки — время первого.
	f.gh.Installations[631].Suspended = false
	f.sync()
	f.gh.Installations[631].Suspended = true
	f.sync()
	first := f.installation(631).SuspendedAt
	event("installation", `{"action":"suspend","installation":{"id":631}}`)
	if got := f.installation(631).SuspendedAt; got == nil || !got.Equal(*first) {
		t.Fatalf("вебхук после опроса переписал suspended_at: %v → %v", first, got)
	}
	f.gh.Installations[631].Suspended = false
	event("installation", `{"action":"unsuspend","installation":{"id":631}}`)

	// Репозиторий: вебхук снял, опрос видит то же; повторный вебхук время не
	// переписывает.
	event("installation_repositories", `{"action":"removed","installation":{"id":631},
		"repositories_removed":[{"full_name":"acme/web"}]}`)
	f.gh.Installations[631].Repos = []string{"acme/api"}
	removed := f.unavailableAt("acme/web")
	if got := f.sync(); len(got) != 0 {
		t.Fatalf("опрос после вебхука репозиториев: %s", changeSet(got))
	}
	event("installation_repositories", `{"action":"removed","installation":{"id":631},
		"repositories_removed":[{"full_name":"acme/web"}]}`)
	if got := f.unavailableAt("acme/web"); got == nil || !got.Equal(*removed) {
		t.Fatalf("повторное снятие переписало unavailable_at: %v → %v", removed, got)
	}
	// Опрос вернул, вебхук «added» после — то же состояние.
	f.gh.Installations[631].Repos = []string{"acme/api", "acme/web"}
	if got := changeSet(f.sync()); got != "631 repository_available acme/web" {
		t.Fatalf("возврат опросом: %q", got)
	}
	event("installation_repositories", `{"action":"added","installation":{"id":631},
		"repositories_added":[{"full_name":"acme/web"}]}`)
	if f.unavailableAt("acme/web") != nil {
		t.Fatal("вебхук после опроса снова снял репозиторий")
	}
}

// Без подключённых установок опрос в провайдера не ходит вовсе.
func TestInstallationSyncWithoutInstallationsMakesNoRequests(t *testing.T) {
	f := newSyncFixture(t)
	f.gh.Installations[1] = &githubtest.Installation{}
	if got := f.sync(); len(got) != 0 {
		t.Fatalf("изменения без установок: %s", changeSet(got))
	}
	if n := f.gh.Requests(githubtest.KindList); n != 0 {
		t.Fatalf("без подключённых установок список запрошен %d раз", n)
	}
}

// AC-5, перепроверки: сбой запроса, которым подтверждается снятие установки
// или пропажа репозитория, не считается подтверждением. Ложное «снята»
// останавливает индексацию клиента, а сбой (5xx, лимит, обрыв) не доказывает,
// что GitHub ответил 404 (CLAB-395, тестировщик).
func TestInstallationSyncFailedRecheckConfirmsNothing(t *testing.T) {
	failures := []struct {
		name string
		code int
	}{
		{"5xx", http.StatusBadGateway},
		{"429", http.StatusTooManyRequests},
		{"403 по лимиту", http.StatusForbidden},
		{"сеть", githubtest.Drop},
	}
	for _, tc := range failures {
		t.Run("установка: "+tc.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.connect(631) // у GitHub её нет ни в списке, ни по идентификатору
			f.repo(631, "acme/a")
			before := f.snapshot()
			f.gh.Fail = map[string]int{githubtest.KindGet: tc.code}
			changes, err := f.engine.SyncInstallations(f.ctx, f.src)
			if err == nil {
				t.Fatal("сбой подтверждения снятия не дал ошибки прохода")
			}
			if len(changes) != 0 || f.snapshot() != before {
				t.Fatalf("установка снята по сбою запроса: %s", changeSet(changes))
			}
			if f.installation(631).RemovedAt != nil {
				t.Fatal("removed_at проставлен по сбою")
			}
		})
		t.Run("репозиторий: "+tc.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.connect(632)
			f.repo(632, "acme/b")
			f.gh.Installations[632] = &githubtest.Installation{Repos: []string{"acme/other"}}
			before := f.snapshot()
			f.gh.Fail = map[string]int{githubtest.KindRepo: tc.code}
			changes, err := f.engine.SyncInstallations(f.ctx, f.src)
			if err == nil {
				t.Fatal("сбой перепроверки репозитория не дал ошибки прохода")
			}
			if len(changes) != 0 || f.snapshot() != before {
				t.Fatalf("репозиторий помечен по сбою запроса: %s", changeSet(changes))
			}
			if f.unavailableAt("acme/b") != nil {
				t.Fatal("unavailable_at проставлен по сбою")
			}
		})
		t.Run("список репозиториев: "+tc.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.connect(633)
			f.repo(633, "acme/c")
			f.gh.Installations[633] = &githubtest.Installation{Repos: []string{"acme/other"}}
			before := f.snapshot()
			f.gh.Fail = map[string]int{githubtest.KindRepos: tc.code}
			changes, err := f.engine.SyncInstallations(f.ctx, f.src)
			if err == nil {
				t.Fatal("сбой списка репозиториев не дал ошибки прохода")
			}
			if len(changes) != 0 || f.snapshot() != before {
				t.Fatalf("база изменилась по сбою списка репозиториев: %s", changeSet(changes))
			}
		})
	}
}
