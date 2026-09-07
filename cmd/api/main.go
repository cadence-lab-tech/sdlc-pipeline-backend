// Команда api — REST-сервис SDLC Pipeline.
//
// В фазе 2 сюда же встанет MCP-сервер: он поднимется на том же процессе и тех
// же сервисах, потому что REST и MCP — два транспорта над одним ядром.
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

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/config"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/httpapi"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/mcp"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

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

	if cfg.AutoMigrate {
		if err := postgres.Migrate(ctx, pool, log); err != nil {
			return err
		}
		if err := postgres.ApplySeeds(ctx, pool, cfg.Seeds, log); err != nil {
			return err
		}
	}

	store := postgres.New(pool)
	engine := workflow.NewEngine(store, log, workflow.Options{
		StrictGates:       cfg.StrictGates,
		LeaseTTL:          cfg.LeaseTTL,
		HeartbeatInterval: cfg.HeartbeatInterval,
	})
	authoring := workflow.NewAuthoring(store, log, engine)
	if !cfg.StrictGates {
		log.Warn("SDLC_STRICT_GATES выключен: гейты нереализованных типов " +
			"пропускают переход — режим отладки цикла, не для продакшена")
	}

	var mcpServer *mcp.Server
	if cfg.MCPEnabled {
		mcpServer = mcp.NewServer(log, store, engine)
		log.Info("MCP-сервер поднят", "path", "/mcp/projects/{PROJECT}")
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewServer(cfg, log, store, engine, authoring, mcpServer).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.RequestTimeout,
		WriteTimeout:      cfg.RequestTimeout,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("сервис слушает", "addr", cfg.HTTPAddr, "env", cfg.Env)
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
	log.Info("остановлено штатно")
	return nil
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
