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
//	SDLC_API_URL          адрес API, обязателен: https://<хост>/v1; конфиг
//	                      clab auth login раннер не читает
//	SDLC_TOKEN            токен раннера: очередь, захват, аренда, отчёт о
//	                      стоимости; агенту не передаётся
//	SDLC_ACTOR            кто действует в режиме заголовка (отладка)
//	SDLC_PROJECT          проект, за которым следит раннер
//	SDLC_RUNNER_ROLES     роли через запятую; пусто — любые, какие даёт токен
//	SDLC_RUNNER_SLOTS     сколько агентов держать одновременно (по умолчанию 1)
//	SDLC_RUNNER_INTERVAL  как часто опрашивать очередь (по умолчанию 15s)
//	SDLC_RUNNER_COMMAND   чем запускать агента (по умолчанию Claude Code)
//	SDLC_RUNNER_TIMEOUT   сколько ждать агента, прежде чем считать его мёртвым
//	SDLC_RUNNER_MAX_FAILURES
//	                      сколько сбоев подряд прогону прощается (по умолчанию
//	                      3): после стольких раннер его не забирает до своего
//	                      перезапуска. Сбой — агент упал или убит по таймауту,
//	                      не подготовлена копия, не выпущен токен задачи
//	SDLC_RUNNER_RETRY_DELAY
//	                      пауза перед повторным захватом после первого сбоя
//	                      (по умолчанию 1m); с каждым сбоем удваивается
//	SDLC_RUNNER_RETRY_DELAY_MAX
//	                      потолок этой паузы (по умолчанию 15m)
//	SDLC_RUNNER_LIMIT_PAUSE
//	                      пауза всех слотов, когда агент упёрся в лимит
//	                      подписки, а время сброса не назвал (по умолчанию 1h);
//	                      названное время раннер ждёт само
//	SDLC_RUNNER_ONCE      забрать одну задачу и выйти: удобно в CI
//	SDLC_RUNNER_DRY_RUN   ничего не запускать: захватить, напечатать argv,
//	                      уровень, его источник и модель, отпустить задачу
//	SDLC_RUNNER_MODEL_LEVELS
//	                      какая модель отвечает рекомендованному уровню
//	                      (решение 0060): low=haiku,medium=sonnet,high=opus,
//	                      любую запись можно опустить; значение идёт в
//	                      {model}. Уровня нет в таблице или он не задан —
//	                      model_hint роли (устарел, в журнале предупреждение),
//	                      нет и его — --model снимается. Сервер без уровней
//	                      — model_hint, как раньше. Опечатка — отказ старта
//	SDLC_RUNNER_REPOS     каталог с клонами репозиториев проекта; клон ищется
//	                      по origin. С ним агент работает в git worktree своей
//	                      задачи, без него — в каталоге раннера, и тогда больше
//	                      одного слота не дадут
//	SDLC_RUNNER_WORKTREES куда класть рабочие копии задач (по умолчанию
//	                      <SDLC_RUNNER_REPOS>/.worktrees)
//	SDLC_RUNNER_ENV       что ещё передать агенту из окружения раннера: имена
//	                      и префиксы через запятую, GH_TOKEN,AWS_*. Имена
//	                      SDLC_* и CLAB_* не передаются никогда — о них
//	                      раннер предупреждает при старте
//
// Агент получает не токен раннера, а токен задачи (решение 0049): после
// захвата раннер выпускает его на захваченный прогон, кладёт в mcp.json и в
// конфиг clab агента (CLAB_CONFIG_DIR — временный каталог) и отзывает, когда
// агент закончил; сам токен гаснет с арендой. Окружение агента — по списку
// разрешённых, без наследования: PATH, HOME, USER, LOGNAME, SHELL, TMPDIR,
// TERM, TZ, LANG, LC_*, XDG_*, прокси (HTTP_PROXY, HTTPS_PROXY, NO_PROXY и
// в нижнем регистре), SSL_CERT_FILE, SSL_CERT_DIR, NODE_EXTRA_CA_CERTS,
// SSH_AUTH_SOCK, ANTHROPIC_*, CLAUDE_*, плюс SDLC_RUNNER_ENV; SDLC_API_URL,
// SDLC_PROJECT, SDLC_TOKEN (токен задачи) и CLAB_CONFIG_DIR задаёт раннер.
// Команда по умолчанию запускает Claude Code с --strict-mcp-config: агент
// видит только сервер из mcp.json раннера, не серверы из настроек
// сотрудника. За свою SDLC_RUNNER_COMMAND отвечает администратор. Сервер
// без выпуска токена задачи (до v0.11.0) — агент работает токеном раннера,
// в журнале предупреждение.
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
		"roles", cfg.Roles, "slots", cfg.Slots, "command", cfg.Command,
		"repos", cfg.ReposDir, "worktrees", cfg.WorktreesDir)
	if cfg.ReposDir == "" {
		log.Warn("SDLC_RUNNER_REPOS не задан: агент работает в каталоге раннера, без рабочей копии задачи")
	}
	log.Info("защита от петли захватов", "max_failures", cfg.MaxFailures, "retry_delay", cfg.RetryDelay,
		"retry_delay_max", cfg.RetryDelayMax, "limit_pause", cfg.LimitPause)
	if len(cfg.ModelLevels) > 0 {
		log.Info("таблица уровней модели", "model_levels", modelLevelsString(cfg.ModelLevels))
	}
	if len(cfg.AgentEnvRefused) > 0 {
		log.Warn("SDLC_RUNNER_ENV: имена SDLC_* и CLAB_* агенту не передаются, раннер задаёт их сам",
			"ignored", cfg.AgentEnvRefused)
	}

	if err := r.run(ctx); err != nil {
		log.Error("раннер остановлен", "error", err)
		os.Exit(1)
	}
}
