// Команда clab-runner — раннер саб-агентов (решение 0025).
//
// Оркестратор не запускает саб-агента напрямую: он создаёт дочерние задачи, а
// раннер их находит. Между ними нет соединения — только состояние задачи в
// базе, поэтому падение любого участника не подвешивает работу.
//
// Раннер живёт там, где есть CLI агента и код: на машине разработчика, в
// контейнере сборки, в CI. С бэкендом он говорит по REST, запущенный им агент
// — по MCP.
//
//	SDLC_API_URL          адрес API (по умолчанию http://localhost:8088/v1)
//	SDLC_TOKEN            токен раннера; он же уезжает запущенному агенту
//	SDLC_ACTOR            кто действует в режиме заголовка (отладка)
//	SDLC_PROJECT          проект, за которым следит раннер
//	SDLC_RUNNER_ROLES     роли через запятую; пусто — любые, какие даёт токен
//	SDLC_RUNNER_SLOTS     сколько агентов держать одновременно (по умолчанию 1)
//	SDLC_RUNNER_INTERVAL  как часто опрашивать очередь (по умолчанию 15s)
//	SDLC_RUNNER_COMMAND   чем запускать агента (по умолчанию Claude Code)
//	SDLC_RUNNER_TIMEOUT   сколько ждать агента, прежде чем считать его мёртвым
//	SDLC_RUNNER_ONCE      забрать одну задачу и выйти: удобно в CI
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig()
	if err != nil {
		log.Error("настройки раннера", "error", err)
		os.Exit(2)
	}

	r := newRunner(cfg, log)
	log.Info("раннер запущен", "version", version, "project", cfg.Project,
		"roles", cfg.Roles, "slots", cfg.Slots, "command", cfg.Command)

	if err := r.run(ctx); err != nil {
		log.Error("раннер остановлен", "error", err)
		os.Exit(1)
	}
}
