package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Состояние приложения GitHub и сводка проекта на транспорте (CLAB-79).
// Провайдер — заглушка, считающая запросы /app; между ней и ручками стоит
// настоящий github.Provider с настоящим кэшем, как в cmd/api.

// providerAccess — тот же переходник, что githubAccess в cmd/api: движок
// знает про интерфейс, транспорт до провайдера — здесь.
type providerAccess struct{ provider *github.Provider }

func (g providerAccess) AppConfigured() bool { return g.provider.HasApp() }

func (g providerAccess) AppStatus(ctx context.Context, fresh bool) github.AppStatus {
	return g.provider.AppStatus(ctx, fresh)
}

func (g providerAccess) CheckRepo(ctx context.Context, installationID *int64, secretRef, owner, name string) error {
	client, err := g.provider.Client(ctx, github.Credentials{InstallationID: installationID, SecretRef: secretRef})
	if err != nil {
		return err
	}
	_, err = client.Repo(ctx, github.Repo{Owner: owner, Name: name})
	return err
}

// providerStub — GitHub, у которого спрашивают про приложение. Считает
// запросы /app; ответ меняется по ходу теста.
type providerStub struct {
	srv     *httptest.Server
	app     atomic.Int32
	handler atomic.Value
}

func newProviderStub(t *testing.T, handler http.HandlerFunc) *providerStub {
	t.Helper()
	stub := &providerStub{}
	stub.handler.Store(handler)
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			// Репозитории демонстрационного проекта: общий токен до них
			// достаёт.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			return
		}
		stub.app.Add(1)
		stub.handler.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *providerStub) reply(status int, body string) {
	s.handler.Store(stubReply(status, body))
}

func stubReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const stubBadCredentials = `{"message":"Bad credentials"}`

// appServer — сервер с приложением, которое ходит в заглушку. withApp=false —
// инстанс без приложения.
func appServer(t *testing.T, store *postgres.Store, auth *workflow.Auth, stub *providerStub, withApp bool) *Server {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	var app *github.App
	if withApp {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		app, err = github.NewApp(stub.srv.URL, "12345", pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	provider := github.NewProvider(github.New(stub.srv.URL, ""), app, nil)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true, GitHub: providerAccess{provider}})
	return NewServer(config.Config{Env: "test"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)
}

type appView struct {
	State              string     `json:"state"`
	Error              string     `json:"error"`
	CheckedAt          *time.Time `json:"checked_at"`
	ClockOffsetSeconds *int       `json:"clock_offset_seconds"`
}

type gitHubView struct {
	AppConfigured *bool            `json:"app_configured"`
	App           *appView         `json:"app"`
	Installations *json.RawMessage `json:"installations"`
	Repositories  *json.RawMessage `json:"repositories"`
}

type healthView struct {
	App                    *appView `json:"app"`
	UnreadableRepositories *int     `json:"unreadable_repositories"`
}

// getJSON — GET от имени актора заголовка (права в этом режиме выключены).
func getJSON(t *testing.T, srv *Server, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-SDLC-Actor", "user:lead")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: ответ не разбирается: %v\n%s", path, err, rec.Body)
		}
	}
	return rec
}

func headerAuth(store *postgres.Store) *workflow.Auth {
	return workflow.NewAuth(store, slog.New(slog.DiscardHandler), workflow.AuthHeader)
}

// A1, A12, A14, F9: без приложения провайдера не спрашивают; публичная ручка
// входа про приложение не говорит; в схеме состояния приложения нет.
func TestAppNotConfiguredOverHTTP(t *testing.T) {
	store, pool := testdb.Store(t)
	stub := newProviderStub(t, stubReply(http.StatusOK, `{}`))
	srv := appServer(t, store, headerAuth(store), stub, false)

	var gh gitHubView
	if rec := getJSON(t, srv, "/v1/projects/SDLC/github", &gh); rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	if gh.App == nil || gh.App.State != "not_configured" || gh.AppConfigured == nil || *gh.AppConfigured {
		t.Fatalf("настройки без приложения: %+v", gh)
	}
	gh = gitHubView{}
	if rec := getJSON(t, srv, "/v1/projects/SDLC/github/check", &gh); rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	if gh.App == nil || gh.App.State != "not_configured" {
		t.Fatalf("проверка без приложения: %+v", gh.App)
	}

	// F9: сводка без приложения — счётчик считается как обычно.
	var health healthView
	if rec := getJSON(t, srv, "/v1/projects/SDLC/health", &health); rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	if health.App == nil || health.App.State != "not_configured" || health.App.CheckedAt != nil ||
		health.UnreadableRepositories == nil || *health.UnreadableRepositories != 0 {
		t.Fatalf("сводка без приложения: %s", mustJSON(t, health))
	}
	if got := stub.app.Load(); got != 0 {
		t.Fatalf("провайдера спросили про приложение %d раз, а его нет", got)
	}

	// A12: публичная ручка страницы входа состояние приложения не отдаёт.
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/github", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var public map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &public); err != nil {
		t.Fatalf("ответ /v1/auth/github: %v: %s", err, rec.Body)
	}
	if _, ok := public["app"]; ok || rec.Code != http.StatusOK {
		t.Fatalf("публичная ручка: код %d, тело %s", rec.Code, rec.Body)
	}

	// A14: состояние приложения живёт в памяти — в схеме ему места нет.
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND (table_name ~ 'app_(status|state|check)' OR column_name ~ 'app_(status|state|check)'
		       OR column_name ~ 'clock_offset')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("в схеме %d колонок под состояние приложения", n)
	}
}

// A2, A3, A6, A8, A9, A10, A11, F2, F8: состояние приложения в ручках
// проекта, команды и в сводке; кэш и свежая проверка.
func TestAppStateOverHTTP(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	auth := headerAuth(store)
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if project.OwnerTeamKey == nil {
		t.Fatal("у демонстрационного проекта нет команды-владельца")
	}
	teamKey := *project.OwnerTeamKey

	t.Run("A2 A8: ключ принят, второй заход — из кэша", func(t *testing.T) {
		stub := newProviderStub(t, stubReply(http.StatusOK, `{"id":12345}`))
		srv := appServer(t, store, auth, stub, true)
		var first, second gitHubView
		getJSON(t, srv, "/v1/projects/SDLC/github", &first)
		getJSON(t, srv, "/v1/projects/SDLC/github", &second)
		if first.App == nil || first.App.State != "ok" || first.App.CheckedAt == nil || first.App.Error != "" ||
			first.App.ClockOffsetSeconds != nil {
			t.Fatalf("состояние: %s", mustJSON(t, first.App))
		}
		if first.AppConfigured == nil || !*first.AppConfigured {
			t.Fatal("app_configured потерял прежний смысл")
		}
		if second.App == nil || !second.App.CheckedAt.Equal(*first.App.CheckedAt) {
			t.Fatalf("второй ответ не из кэша: %s", mustJSON(t, second.App))
		}
		if got := stub.app.Load(); got != 1 {
			t.Fatalf("запросов /app %d вместо одного", got)
		}

		// A11: страница команды отдаёт то же состояние рядом с установками.
		var teamView gitHubView
		if rec := getJSON(t, srv, "/v1/teams/"+teamKey+"/github/installations", &teamView); rec.Code != http.StatusOK {
			t.Fatalf("код %d: %s", rec.Code, rec.Body)
		}
		if teamView.Installations == nil || teamView.App == nil ||
			mustJSON(t, teamView.App) != mustJSON(t, first.App) {
			t.Fatalf("установки команды: %s", mustJSON(t, teamView))
		}

		// F2: сводка — то же состояние без подробностей.
		var health healthView
		getJSON(t, srv, "/v1/projects/SDLC/health", &health)
		if health.App == nil || health.App.State != "ok" || health.App.CheckedAt == nil {
			t.Fatalf("сводка: %s", mustJSON(t, health))
		}
		if got := stub.app.Load(); got != 1 {
			t.Fatalf("сводка и страница команды сходили к провайдеру: запросов /app %d", got)
		}
	})

	t.Run("A3 A10 F8: ключ не принят, свежая проверка обновляет кэш", func(t *testing.T) {
		stub := newProviderStub(t, func(w http.ResponseWriter, _ *http.Request) {
			// Часы «разошлись» на три минуты: в настройках расхождение есть,
			// в сводке его быть не должно.
			w.Header().Set("Date", time.Now().Add(3*time.Minute).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(stubBadCredentials))
		})
		srv := appServer(t, store, auth, stub, true)

		var gh gitHubView
		if rec := getJSON(t, srv, "/v1/projects/SDLC/github", &gh); rec.Code != http.StatusOK {
			t.Fatalf("код %d: %s", rec.Code, rec.Body)
		}
		if gh.App == nil || gh.App.State != "rejected" || !strings.Contains(gh.App.Error, "Bad credentials") ||
			gh.App.CheckedAt == nil || gh.AppConfigured == nil || !*gh.AppConfigured {
			t.Fatalf("отказ: %s", mustJSON(t, gh))
		}
		if gh.App.ClockOffsetSeconds == nil || *gh.App.ClockOffsetSeconds < 170 {
			t.Fatalf("расхождение часов: %v", gh.App.ClockOffsetSeconds)
		}

		// F1, F2: сводка говорит сам факт — без текста провайдера и часов.
		rec := getJSON(t, srv, "/v1/projects/SDLC/health", nil)
		var raw struct {
			App map[string]json.RawMessage `json:"app"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("сводка: код %d, %v: %s", rec.Code, err, rec.Body)
		}
		if string(raw.App["state"]) != `"rejected"` || raw.App["checked_at"] == nil {
			t.Fatalf("сводка: %s", rec.Body)
		}
		if _, ok := raw.App["error"]; ok {
			t.Fatalf("в сводке текст провайдера: %s", rec.Body)
		}
		if _, ok := raw.App["clock_offset_seconds"]; ok {
			t.Fatalf("в сводке расхождение часов: %s", rec.Body)
		}

		// Провайдер починен. Кэш ещё держит отказ; свежая проверка его снимает.
		before := stub.app.Load()
		stub.reply(http.StatusOK, `{}`)
		gh = gitHubView{}
		getJSON(t, srv, "/v1/projects/SDLC/github", &gh)
		if gh.App.State != "rejected" || stub.app.Load() != before {
			t.Fatalf("настройки до проверки: %s, запросов /app прибавилось %d", gh.App.State, stub.app.Load()-before)
		}
		gh = gitHubView{}
		if rec := getJSON(t, srv, "/v1/projects/SDLC/github/check", &gh); rec.Code != http.StatusOK {
			t.Fatalf("код %d: %s", rec.Code, rec.Body)
		}
		if gh.App == nil || gh.App.State != "ok" || gh.Repositories == nil {
			t.Fatalf("свежая проверка: %s", mustJSON(t, gh))
		}
		gh = gitHubView{}
		getJSON(t, srv, "/v1/projects/SDLC/github", &gh)
		var health healthView
		getJSON(t, srv, "/v1/projects/SDLC/health", &health)
		if gh.App.State != "ok" || health.App.State != "ok" {
			t.Fatalf("после проверки: настройки %s, сводка %s", gh.App.State, health.App.State)
		}
		if got := stub.app.Load(); got != before+1 {
			t.Fatalf("после свежей проверки к провайдеру сходили ещё %d раз", got-before-1)
		}
	})

	t.Run("A6 F2: провайдеру плохо — не удалось проверить, ручка жива", func(t *testing.T) {
		stub := newProviderStub(t, stubReply(http.StatusInternalServerError, `{"message":"upstream"}`))
		srv := appServer(t, store, auth, stub, true)
		var gh gitHubView
		rec := getJSON(t, srv, "/v1/projects/SDLC/github", &gh)
		if rec.Code != http.StatusOK || gh.App == nil || gh.App.State != "unknown" || gh.App.Error == "" ||
			gh.Installations == nil {
			t.Fatalf("код %d: %s", rec.Code, rec.Body)
		}
		var health healthView
		if rec := getJSON(t, srv, "/v1/projects/SDLC/health", &health); rec.Code != http.StatusOK ||
			health.App.State != "unknown" || health.App.CheckedAt == nil {
			t.Fatalf("сводка: код %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("A9: проверка спрашивает про приложение и без репозиториев", func(t *testing.T) {
		stub := newProviderStub(t, stubReply(http.StatusOK, `{}`))
		srv := appServer(t, store, auth, stub, true)
		who, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
		if err != nil {
			t.Fatal(err)
		}
		engine := workflow.NewEngine(store, slog.New(slog.DiscardHandler), workflow.Options{})
		if _, err := engine.CreateProject(ctx, who.ID, postgres.CreateProjectInput{
			Key: "EMPTY", Name: "Без репозиториев", OwnerTeamID: *project.OwnerTeamID}); err != nil {
			t.Fatal(err)
		}
		// EMPTY — без репозиториев; у SDLC все репозитории на токене инстанса.
		for i, key := range []string{"EMPTY", "SDLC", "EMPTY"} {
			var gh gitHubView
			if rec := getJSON(t, srv, "/v1/projects/"+key+"/github/check", &gh); rec.Code != http.StatusOK {
				t.Fatalf("%s: код %d: %s", key, rec.Code, rec.Body)
			}
			if gh.App == nil || gh.App.State != "ok" {
				t.Fatalf("%s: в ответе проверки нет состояния приложения: %s", key, mustJSON(t, gh))
			}
			if got := int(stub.app.Load()); got != i+1 {
				t.Fatalf("%s: запросов /app %d после %d проверок", key, got, i+1)
			}
		}
		var sdlc struct {
			Repositories []struct {
				Source string `json:"source"`
			} `json:"repositories"`
		}
		getJSON(t, srv, "/v1/projects/SDLC/github/check", &sdlc)
		if len(sdlc.Repositories) == 0 {
			t.Fatal("у демонстрационного проекта нет репозиториев — проверка источника instance ничего не проверяет")
		}
		for _, r := range sdlc.Repositories {
			if r.Source != string(github.SourceInstance) {
				t.Fatalf("источник %q вместо instance", r.Source)
			}
		}
	})
}

// A7, F6, F7: сводку зовут на каждой странице все участники сразу — при
// пустом кэше одна проверка на всех, при висящем провайдере ответ за пять
// секунд и дальше без ожидания.
func TestProjectHealthDoesNotHammerProvider(t *testing.T) {
	store, _ := testdb.Store(t)
	auth := headerAuth(store)

	t.Run("F6: двадцать одновременных запросов — одна проверка", func(t *testing.T) {
		stub := newProviderStub(t, func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(time.Second)
			w.WriteHeader(http.StatusOK)
		})
		srv := appServer(t, store, auth, stub, true)
		handler := srv.Handler()

		const n = 20
		bodies := make([]string, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodGet, "/v1/projects/SDLC/health", nil)
				req.Header.Set("X-SDLC-Actor", "user:lead")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				bodies[i] = rec.Body.String()
			}()
		}
		wg.Wait()
		if got := stub.app.Load(); got != 1 {
			t.Fatalf("запросов /app %d вместо одного", got)
		}
		for i, body := range bodies {
			if body != bodies[0] || !strings.Contains(body, `"state":"ok"`) {
				t.Fatalf("ответ %d отличается:\n%s\n%s", i, body, bodies[0])
			}
		}
	})

	t.Run("A7 F7: провайдер висит", func(t *testing.T) {
		release := make(chan struct{})
		stub := newProviderStub(t, func(http.ResponseWriter, *http.Request) { <-release })
		defer close(release)
		srv := appServer(t, store, auth, stub, true)

		// Настройки и сводка приходят одновременно на пустой кэш.
		started := time.Now()
		var gh gitHubView
		var first healthView
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); getJSON(t, srv, "/v1/projects/SDLC/github", &gh) }()
		go func() { defer wg.Done(); getJSON(t, srv, "/v1/projects/SDLC/health", &first) }()
		wg.Wait()
		if took := time.Since(started); took > 5500*time.Millisecond {
			t.Fatalf("ручки ждали провайдера %s", took)
		}
		if gh.App == nil || gh.App.State != "unknown" || gh.Installations == nil {
			t.Fatalf("настройки при висящем провайдере: %s", mustJSON(t, gh))
		}
		if first.App == nil || first.App.State != "unknown" || first.UnreadableRepositories == nil {
			t.Fatalf("сводка при висящем провайдере: %s", mustJSON(t, first))
		}

		started = time.Now()
		var second healthView
		if rec := getJSON(t, srv, "/v1/projects/SDLC/health", &second); rec.Code != http.StatusOK {
			t.Fatalf("код %d", rec.Code)
		}
		if took := time.Since(started); took > 500*time.Millisecond {
			t.Fatalf("второй запрос сводки ждал %s: unknown не закэширован", took)
		}
		if second.App.State != "unknown" {
			t.Fatalf("вторая сводка: %s", mustJSON(t, second))
		}
		if got := stub.app.Load(); got != 1 {
			t.Fatalf("запросов /app %d вместо одного", got)
		}
	})
}

// F1, A11: права на ручки. Сводку видит участник уровня viewer, не
// участнику — тот же отказ, что у состояния индекса; установки команды с
// состоянием приложения — участнику команды, не участнику — прежний отказ.
func TestHealthAndTeamGitHubPermissions(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	stub := newProviderStub(t, stubReply(http.StatusOK, `{}`))
	srv := appServer(t, store, auth, stub, true)

	const rootToken = "bootstrap-token-0123456789"
	if err := auth.Bootstrap(ctx, "root", rootToken); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	token := func(handle string) string {
		t.Helper()
		if _, err := store.ResolveActor(ctx, store.DB(), handle, domain.PrincipalUser); err != nil {
			t.Fatal(err)
		}
		issued, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{
			Principal: "user:" + handle, Name: "test-" + handle,
		})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token
	}
	viewer, outsider, teammate := token("viewer"), token("outsider"), token("teammate")
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:viewer", domain.MemberViewer, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateTeam(ctx, admin, workflow.CreateTeamInput{Key: "acme", Name: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "acme", "user:teammate", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}

	call := func(path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// F1: участник уровня viewer получает сводку целиком.
	rec := call("/v1/projects/SDLC/health", viewer)
	var health healthView
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("сводка участнику viewer: код %d, %v: %s", rec.Code, err, rec.Body)
	}
	if health.App == nil || health.App.State != "ok" || health.App.CheckedAt == nil ||
		health.UnreadableRepositories == nil {
		t.Fatalf("сводка: %s", rec.Body)
	}
	// Настройки GitHub тому же участнику по-прежнему закрыты.
	if rec := call("/v1/projects/SDLC/github", viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("настройки GitHub участнику viewer: код %d", rec.Code)
	}

	// Не участнику — тот же отказ, что у состояния индекса.
	index, refused := call("/v1/projects/SDLC/index", outsider), call("/v1/projects/SDLC/health", outsider)
	if index.Code == http.StatusOK || refused.Code != index.Code {
		t.Fatalf("не участнику: индекс %d, сводка %d", index.Code, refused.Code)
	}
	var a, b errorBody
	_ = json.Unmarshal(index.Body.Bytes(), &a)
	_ = json.Unmarshal(refused.Body.Bytes(), &b)
	if a.Error.Code != b.Error.Code {
		t.Fatalf("коды отказа разошлись: %q и %q", a.Error.Code, b.Error.Code)
	}
	// Отказ не стоил похода к провайдеру.
	before := stub.app.Load()
	call("/v1/projects/SDLC/health", outsider)
	if got := stub.app.Load(); got != before {
		t.Fatal("отказ в сводке сходил к провайдеру")
	}

	// A11: участник команды видит установки и состояние приложения.
	rec = call("/v1/teams/acme/github/installations", teammate)
	var team gitHubView
	if err := json.Unmarshal(rec.Body.Bytes(), &team); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("установки участнику команды: код %d, %v: %s", rec.Code, err, rec.Body)
	}
	if team.Installations == nil || team.App == nil || team.App.State != "ok" {
		t.Fatalf("установки команды: %s", rec.Body)
	}
	// Не участнику команды — отказ, и состояния приложения в нём нет.
	rec = call("/v1/teams/acme/github/installations", outsider)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Fatalf("установки не участнику команды: код %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"app"`) {
		t.Fatalf("отказ несёт состояние приложения: %s", rec.Body)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
