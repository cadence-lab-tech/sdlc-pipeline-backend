// Команда worker — фоновые задания, которые никто не дёргает запросом.
//
// В фазе 2 это одно задание: возврат задач с протухшей арендой. Агент
// локальный и может умереть, не отпустив задачу; без этого прохода она
// осталась бы занятой навсегда. В фазе 3 сюда встанут индексация
// репозиториев и детектор дрейфа.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/config"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

// systemActor — принципал, от имени которого воркер пишет в ленту задачи.
// Аудит ссылается на principals, а не на строки, поэтому у фонового процесса
// тоже есть своё лицо.
const systemActor = "sdlc-worker"

func main() {
	if err := run(); err != nil {
		slog.Error("воркер остановлен с ошибкой", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, cfg.MaxConns, cfg.ConnectTimeout)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := postgres.New(pool)
	// Миграции катит api: два процесса, накатывающие схему, разошлись бы в
	// порядке старта. Воркер работает по уже готовой схеме.
	engine := workflow.NewEngine(store, log, workflow.Options{
		StrictGates:       cfg.StrictGates,
		LeaseTTL:          cfg.LeaseTTL,
		HeartbeatInterval: cfg.HeartbeatInterval,
	})

	system, err := store.ResolveActor(ctx, store.DB(), systemActor, domain.PrincipalSystem)
	if err != nil {
		return err
	}

	log.Info("воркер запущен",
		"lease_sweep_interval", cfg.LeaseSweepInterval, "lease_ttl", cfg.LeaseTTL)

	ticker := time.NewTicker(cfg.LeaseSweepInterval)
	defer ticker.Stop()

	for {
		sweepLeases(ctx, engine, log, system.ID)

		select {
		case <-ctx.Done():
			log.Info("получен сигнал, останавливаемся")
			return nil
		case <-ticker.C:
		}
	}
}

// sweepLeases возвращает в pending всё, чья аренда истекла.
//
// Ошибка прохода не роняет воркер: база могла быть недоступна секунду, а
// следующий тик всё равно наступит.
func sweepLeases(ctx context.Context, engine *workflow.Engine, log *slog.Logger, system uuid.UUID) {
	n, err := engine.ExpireLeases(ctx, system, 100)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error("проход по протухшим арендам не удался", "error", err)
		return
	}
	if n > 0 {
		log.Info("аренды истекли, задачи возвращены в общий доступ", "count", n)
	}
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
