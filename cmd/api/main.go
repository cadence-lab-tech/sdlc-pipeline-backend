// Команда api — REST-сервис Cadence Lab и MCP-сервер в одном процессе:
// два транспорта над одним ядром (решение 0011).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/httpapi"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oidc"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/tracing"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// version подставляется при сборке: -ldflags "-X main.version=…" (Makefile,
// Dockerfile). Та же строка уходит в /v1/instance, MCP initialize и лог.
var version = "dev"

func main() {
	// Образ собирается из distroless: ни shell, ни curl внутри нет, и
	// healthcheck в compose дёргает сам бинарник.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			slog.Error("проба не прошла", "error", err)
			os.Exit(1)
		}
		return
	}
	// Предпроверка базы перед установкой и апгрейдом (CLAB-127).
	if len(os.Args) > 1 && os.Args[1] == "dbcheck" {
		os.Exit(dbcheck(os.Args[2:], os.Stdout, os.Stderr))
	}
	// Копия бэкапов вне машины, в S3 (CLAB-132).
	if len(os.Args) > 1 && os.Args[1] == "backup-s3" {
		os.Exit(backupS3(os.Args[2:], os.Stdout, os.Stderr))
	}

	if err := run(); err != nil {
		slog.Error("сервис остановлен с ошибкой", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	// Трассировки — стандартные OTEL_*, а не SDLC_*: их знают сборщики и
	// платформенные команды. Неподдерживаемое значение роняет старт до
	// подключения к базе, как неверная настройка в config.Load; экспортёр
	// же сети не трогает, и недоступный сборщик старт не роняет.
	traceSettings, err := tracing.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	traces, err := tracing.Start(context.Background(), traceSettings, version, log)
	if err != nil {
		return err
	}

	// Контекст живёт до сигнала: и подключение к базе, и миграции должны
	// прерываться по Ctrl+C, иначе контейнер приходится убивать силой.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, cfg.MaxConns, cfg.ConnectTimeout)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("база подключена", "max_conns", cfg.MaxConns)

	// Метрики эксплуатации — на своём порту (SDLC_METRICS_ADDR), не на
	// основном: основной смотрит в интернет, а /metrics отдаётся без токена.
	// Состояния базы api не отдаёт — реплик может быть несколько, его
	// считает воркер.
	var apiMetrics *metrics.API
	var metricsSrv *metrics.Server
	if cfg.MetricsAddr != "" {
		reg := metrics.New("api", version)
		reg.Pool(pool)
		apiMetrics = metrics.NewAPI(reg)
		if metricsSrv, err = metrics.Start(cfg.MetricsAddr, reg, log); err != nil {
			return err
		}
	}

	if cfg.AutoMigrate {
		if err := postgres.Migrate(ctx, pool, log); err != nil {
			return err
		}
		if err := postgres.ApplySeeds(ctx, pool, cfg.Seeds, log); err != nil {
			return err
		}
	}

	store := postgres.New(pool)
	if err := useBlobStore(cfg, store, log); err != nil {
		return err
	}
	// Учётные данные GitHub выбираются по проекту: установка приложения либо
	// именованный секрет (решение 0028). Ключ приложения — единственный
	// секрет; без него остаётся общий токен инстанса.
	app, err := github.LoadApp(cfg.GitHubAPIURL, cfg.GitHubAppID, cfg.GitHubAppKeyFile)
	if err != nil {
		return err
	}
	gh := githubAccess{github.NewProvider(
		github.New(cfg.GitHubAPIURL, cfg.GitHubToken), app, github.SecretFromEnv)}
	if app != nil {
		log.Info("доступ к GitHub через приложение", "app_id", cfg.GitHubAppID)
	}

	// Индексацию ведёт воркер, но вектор поискового запроса считается
	// здесь: поиск синхронный, и ждать фонового прохода ему нечего. Модель
	// не той размерности останавливает старт (embed.New).
	embedder, err := embed.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	engine := workflow.NewEngine(store, log, workflow.Options{
		StrictGates:       cfg.StrictGates,
		LeaseTTL:          cfg.LeaseTTL,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Embedder:          embedder,
		GitHub:            gh,
	})
	authoring := workflow.NewAuthoring(store, log, engine)
	if !cfg.StrictGates {
		log.Warn("SDLC_STRICT_GATES выключен: гейты нереализованных типов " +
			"пропускают переход — режим отладки цикла, не для продакшена")
	}

	auth := workflow.NewAuth(store, log, cfg.AuthMode).
		OpenProjectCreation(cfg.ProjectCreation == "open").
		Tenancy(cfg.Tenancy)
	if err := auth.Bootstrap(ctx, cfg.AdminHandle, cfg.AdminToken); err != nil {
		return err
	}
	if err := auth.BootstrapTeam(ctx, cfg.AdminHandle, cfg.BootstrapTeam, cfg.BootstrapTeamName); err != nil {
		return err
	}
	if cfg.OIDCIssuer != "" {
		auth.UseOIDC(oidc.New(oidc.Config{
			Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID,
			GroupsClaim: cfg.OIDCGroupsClaim,
		}), cfg.SessionTTL)
		log.Info("вход через корпоративный провайдер", "issuer", cfg.OIDCIssuer,
			"session_ttl", cfg.SessionTTL)
	}
	if cfg.GitHubAppClientID != "" {
		auth.UseGitHub(github.NewOAuth(cfg.GitHubWebURL, cfg.GitHubAPIURL,
			cfg.GitHubAppClientID, cfg.GitHubAppClientSecret), cfg.SessionTTL).
			GitHubApp(cfg.GitHubAppSlug)
		log.Info("вход через GitHub", "client_id", cfg.GitHubAppClientID, "app_slug", cfg.GitHubAppSlug)
	}
	switch {
	case auth.Enforced():
		log.Info("аутентификация по токенам", "admin", cfg.AdminHandle,
			"project_creation", cfg.ProjectCreation,
			"bootstrap_token", cfg.AdminToken != "")
	default:
		log.Warn("SDLC_AUTH_MODE=header: актор берётся из заголовка, права не " +
			"проверяются — режим отладки цикла, не для продакшена")
	}
	if cfg.GitHubWebhookSecret != "" {
		log.Info("вебхуки GitHub включены", "path", "/webhooks/github")
	}

	var mcpServer *mcp.Server
	if cfg.MCPEnabled {
		mcpServer = mcp.NewServer(log, store, engine, authoring, auth, version)
		log.Info("MCP-сервер поднят", "path", "/mcp/projects/{PROJECT}")
	}

	api := httpapi.NewServer(cfg, log, store, engine, authoring, auth, mcpServer).
		WithVersion(version).WithMetrics(apiMetrics).
		WithTracing(traces.Provider())
	handler := api.Handler()
	logRequestLimits(log, cfg)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.RequestTimeout,
		WriteTimeout:      cfg.RequestTimeout,
		IdleTimeout:       120 * time.Second,
	}
	// Долгий опрос событий (CLAB-323) остановку не держит: ожидающие
	// отвечают сразу пустым списком, клиент переподключится к другой реплике.
	srv.RegisterOnShutdown(api.StopWaiting)

	errc := make(chan error, 1)
	go func() {
		log.Info("сервис слушает", "addr", cfg.HTTPAddr, "env", cfg.Env, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("получен сигнал, останавливаемся")
	}

	// Отдельный контекст: ctx уже отменён сигналом, а незавершённым запросам
	// нужно дать доиграть — иначе транзакция перехода оборвётся на середине.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// Спаны доигранных запросов уходят после остановки сервера и в том же
	// сроке: недоступный сборщик остановку не продлевает, хвост теряется.
	if err := traces.Shutdown(shutdownCtx); err != nil {
		log.Warn("трассировки: накопленные спаны не отправлены до остановки", "error", err)
	}
	// Метрики гасятся последними: до конца остановки Prometheus видит, что
	// процесс ещё жив и доигрывает запросы.
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("остановлено штатно")
	return nil
}

// useBlobStore передаёт хранилищу пределы содержимого и включает вынос
// крупного, если задан каталог или бакет. Без них всё лежит inline, а
// содержимое больше min(4 МиБ, SDLC_BLOB_MAX_BYTES) отбивается.
func useBlobStore(cfg config.Config, store *postgres.Store, log *slog.Logger) error {
	blobs, err := blobstore.Open(cfg.BlobS3, cfg.BlobDir)
	if err != nil {
		return err
	}
	if blobs == nil {
		store.UseBlobStore(nil, cfg.BlobInlineMaxBytes, cfg.BlobMaxBytes)
		log.Info("внешнее хранилище содержимого не настроено, всё лежит в базе",
			"max_bytes", min(blobstore.InlineMaxBytes, cfg.BlobMaxBytes))
		return nil
	}
	store.UseBlobStore(blobs, cfg.BlobInlineMaxBytes, cfg.BlobMaxBytes)
	log.Info("внешнее хранилище содержимого", "store", blobs,
		"inline_max_bytes", cfg.BlobInlineMaxBytes, "max_bytes", cfg.BlobMaxBytes)
	return nil
}

// logRequestLimits пишет сроки и предел тела запроса (CLAB-295): их задаёт
// администратор под свои проекты, и оборванная загрузка разбирается по этой
// строке журнала. Ноль срока — «без срока».
func logRequestLimits(log *slog.Logger, cfg config.Config) {
	log.Info("сроки и предел тела запроса",
		"request_timeout", cfg.RequestTimeout,
		"upload_timeout", cfg.UploadTimeout,
		"download_timeout", cfg.DownloadTimeout,
		"max_body_bytes", httpapi.BodyLimit(cfg.BlobMaxBytes))
}

// healthcheck ходит в собственную пробу готовности по локальному адресу.
func healthcheck() error {
	addr := os.Getenv("SDLC_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz вернул %d", resp.StatusCode)
	}
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

// githubAccess — доступ к репозиторию учётными данными проекта. Движок знает
// про интерфейс, транспорт до провайдера живёт здесь: правило «репозиторий вне
// установки не привязывается» — доменное, а поход в сеть — нет.
type githubAccess struct{ provider *github.Provider }

func (g githubAccess) AppConfigured() bool { return g.provider.HasApp() }

func (g githubAccess) AppStatus(ctx context.Context, fresh bool) github.AppStatus {
	return g.provider.AppStatus(ctx, fresh)
}

func (g githubAccess) CheckRepo(ctx context.Context, installationID *int64, secretRef, owner, name string) error {
	client, err := g.provider.Client(ctx, github.Credentials{
		InstallationID: installationID, SecretRef: secretRef,
	})
	if err != nil {
		return err
	}
	_, err = client.Repo(ctx, github.Repo{Owner: owner, Name: name})
	return err
}
