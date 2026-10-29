package main

// clab heartbeat — продление аренды этапа (CLAB-288, решение 0063). Аренду
// держит клиент: сервер её не продлевает и истёкшую не воскрешает, а токен
// задачи живёт, пока жива аренда (решение 0049). Три режима:
//
//	clab heartbeat K                      одно продление, ответ сервера JSON'ом (как раньше)
//	clab heartbeat K --every 5m [--for 2h] продлевать, пока аренда наша
//	clab heartbeat K [--every 5m] -- cmd   продлевать, пока работает команда
//
// Команда после «--» идёт без учётных данных clab (CLAB-306): ни токена, ни
// адреса, ни проекта, ни действующего лица, а CLAB_CONFIG_DIR — пустой
// временный каталог. Иначе вложенный clab, сценарий проверки или make с
// вызовом CLI унаследовали бы токен задачи рабочего инстанса и ушли бы на него.
// --with-credentials возвращает прежнее наследование.
//
// Цикл закреплён за прогоном из первого ответа: следующий прогон той же
// задачи он не продлевает, даже если его забрал тот же принципал. Про роли и
// этапы CLI ничего не знает — только прогон и аренду.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// leaseReply — ответ POST /tasks/{key}/heartbeat (workflow.Lease).
type leaseReply struct {
	StageRunID               string    `json:"stage_run_id"`
	ExpiresAt                time.Time `json:"expires_at"`
	HeartbeatIntervalSeconds int       `json:"heartbeat_interval_seconds"`
}

func cmdHeartbeat(ctx context.Context, c *Client, args []string) error {
	task, rest := popTask(args)
	flagArgs, command, wrapped := splitCommand(rest)
	var every, forDur time.Duration
	var withCreds bool
	fs, err := flags("heartbeat", flagArgs, func(fs *flag.FlagSet) {
		fs.DurationVar(&every, "every", 0, "продлевать с этим интервалом (5m, 30s); по умолчанию — интервал heartbeat из ответа сервера")
		fs.DurationVar(&forDur, "for", 0, "перестать продлевать через это время; без него — пока аренда наша или до сигнала")
		fs.BoolVar(&withCreds, "with-credentials", false, "передать команде учётные данные clab (SDLC_TOKEN, SDLC_API_URL, SDLC_PROJECT, SDLC_ACTOR, CLAB_CONFIG_DIR); по умолчанию команда идёт без них")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("лишние аргументы %q: команду передают после --", fs.Args())
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case set["every"] && every <= 0:
		return fmt.Errorf("--every должен быть больше нуля")
	case set["for"] && forDur <= 0:
		return fmt.Errorf("--for должен быть больше нуля")
	case wrapped && len(command) == 0:
		return fmt.Errorf("после -- нет команды: clab heartbeat %s -- <команда> [аргументы]", task)
	case wrapped && set["for"]:
		return fmt.Errorf("--for не сочетается с командой: продление идёт, пока она работает")
	case !wrapped && withCreds:
		return fmt.Errorf("--with-credentials относится к команде после --: clab heartbeat %s --with-credentials -- <команда>", task)
	}

	if !wrapped && !set["every"] && !set["for"] {
		// Одно продление — прежний вывод без изменений (R4).
		var out any
		if err := c.post(ctx, "/tasks/"+task+"/heartbeat", map[string]any{}, &out); err != nil {
			return err
		}
		return print(out)
	}

	h := &heartbeater{c: c, task: task, every: every, withCredentials: withCreds}
	if !wrapped {
		h.out, h.warn = os.Stdout, os.Stderr
		return h.loop(ctx, forDur)
	}
	h.out, h.warn = os.Stderr, os.Stderr
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	code, err := h.wrap(ctx, command, os.Stdin, os.Stdout, os.Stderr, sigs)
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// splitCommand отделяет флаги heartbeat от команды после первого «--».
func splitCommand(args []string) (flagArgs, command []string, wrapped bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

type heartbeater struct {
	c     *Client
	task  string
	every time.Duration
	// Прогон из первого ответа: дальше продлевается только он.
	runID string
	// out — строки о ходе продления, warn — сбои. У обёртки оба — stderr:
	// stdout принадлежит команде.
	out, warn io.Writer
	// withCredentials — команда обёртки получает окружение clab как есть;
	// иначе без учётных данных (isolatedEnv).
	withCredentials bool
}

// credentialVars — переменные, которыми clab берёт хост, токен, проект и
// действующее лицо; тот же набор показывает clab auth status.
var credentialVars = []string{"SDLC_TOKEN", "SDLC_API_URL", "SDLC_PROJECT", "SDLC_ACTOR", "CLAB_CONFIG_DIR"}

// isolatedEnv — окружение родителя без учётных данных clab и с пустым
// CLAB_CONFIG_DIR. Пустой каталог, а не снятая переменная: без неё clab
// возьмёт ~/.config/clab, то есть личный токен человека. Пустой конфиг —
// «хост не настроен» (CLAB-147), и без явного адреса вложенный clab в сеть
// не ходит.
func isolatedEnv(env []string, configDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if isCredentialVar(name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CLAB_CONFIG_DIR="+configDir)
}

func isCredentialVar(name string) bool {
	for _, v := range credentialVars {
		// В Windows имена переменных окружения не различают регистр.
		if name == v || (runtime.GOOS == "windows" && strings.EqualFold(name, v)) {
			return true
		}
	}
	return false
}

// first — первое продление: закрепляет прогон и интервал. Отказ сервера
// возвращается как есть — exit напечатает код и текст.
func (h *heartbeater) first(ctx context.Context) error {
	var l leaseReply
	if err := h.c.post(ctx, "/tasks/"+h.task+"/heartbeat", map[string]any{}, &l); err != nil {
		return err
	}
	if l.StageRunID == "" {
		return fmt.Errorf("сервер не назвал прогон аренды (stage_run_id): продлевать нечего")
	}
	h.runID = l.StageRunID
	remaining := time.Until(l.ExpiresAt).Round(time.Second)
	if h.every == 0 {
		h.every = time.Duration(l.HeartbeatIntervalSeconds) * time.Second
		if h.every <= 0 {
			// Сервер интервала не назвал — треть оставшегося срока.
			h.every = max(remaining/3, time.Second)
		}
	}
	if h.every >= remaining {
		return fmt.Errorf("интервал продления %s не меньше оставшегося срока аренды %s (до %s): аренда истечёт между продлениями — задайте --every меньше",
			h.every, remaining, when(&l.ExpiresAt))
	}
	fmt.Fprintf(h.out, "clab heartbeat: аренда прогона %s продлена до %s, дальше — каждые %s\n",
		short(h.runID), when(&l.ExpiresAt), h.every)
	return nil
}

// keep продлевает аренду закреплённого прогона каждые h.every, пока жив ctx.
// Возвращает причину, если аренды у цикла больше нет; пустую строку — если
// его остановили.
func (h *heartbeater) keep(ctx context.Context) string {
	ticker := time.NewTicker(h.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-ticker.C:
		}
		var l leaseReply
		err := h.c.post(ctx, "/tasks/"+h.task+"/heartbeat", map[string]any{"stage_run_id": h.runID}, &l)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return ""
		case leaseGone(err):
			return err.Error()
		default:
			fmt.Fprintf(h.warn, "clab heartbeat: продление не удалось, повтор через %s: %v\n", h.every, err)
		}
	}
}

// leaseGone — ответ значит, что аренды у этого цикла больше нет: снята,
// истекла, перехвачена, токен погас. 4xx не лечится повтором; сеть, 5xx и
// 429 — лечится, их цикл переживает.
func leaseGone(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500 {
		return false
	}
	return apiErr.Status >= 400
}

// loop — режим --every/--for: код 0, когда аренда ушла, вышло --for или
// пришёл сигнал; ненулевой — только если не прошло первое продление.
func (h *heartbeater) loop(ctx context.Context, forDur time.Duration) error {
	if err := h.first(ctx); err != nil {
		return err
	}
	lctx := ctx
	if forDur > 0 {
		var cancel context.CancelFunc
		lctx, cancel = context.WithTimeout(ctx, forDur)
		defer cancel()
	}
	reason := h.keep(lctx)
	switch {
	case reason != "":
		fmt.Fprintf(h.out, "clab heartbeat: аренды прогона %s больше нет, продление остановлено: %s\n", short(h.runID), reason)
	case ctx.Err() != nil:
		fmt.Fprintf(h.out, "clab heartbeat: остановлено сигналом\n")
	default:
		fmt.Fprintf(h.out, "clab heartbeat: --for %s истекло, продление остановлено\n", forDur)
	}
	return nil
}

// wrap — режим «-- команда»: команда получает stdin, stdout и stderr,
// аренда продлевается, пока она работает, код выхода — её код. INT и TERM,
// пришедшие clab, уходят команде (из терминала Ctrl-C группе переднего плана
// команда получит и сама). Аренда потеряна посреди работы — предупреждение,
// но команда доделывает своё: прервать её значило бы потерять и работу.
func (h *heartbeater) wrap(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer, sigs <-chan os.Signal) (int, error) {
	if err := h.first(ctx); err != nil {
		return 0, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if !h.withCredentials {
		// Изолируется только дочерний процесс: клиент обёртки собран
		// до разбора и продлевает аренду прежними учётными данными.
		dir, err := os.MkdirTemp("", "clab-heartbeat-")
		if err != nil {
			return 0, fmt.Errorf("пустой каталог конфига для команды: %w", err)
		}
		defer os.RemoveAll(dir)
		cmd.Env = isolatedEnv(os.Environ(), dir)
		fmt.Fprintf(h.warn, "clab heartbeat: команда идёт без учётных данных clab (SDLC_TOKEN, SDLC_API_URL, SDLC_PROJECT, SDLC_ACTOR, CLAB_CONFIG_DIR не переданы); вернуть их — --with-credentials\n")
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("команда не запустилась: %w", err)
	}

	// Сигнал отменяет ctx main, а продлевать надо, пока команда не вышла.
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	kept := make(chan struct{})
	go func() {
		defer close(kept)
		if reason := h.keep(lctx); reason != "" {
			fmt.Fprintf(h.warn, "clab heartbeat: аренда прогона %s потеряна, команда продолжает работу (%s); сдать работу токеном задачи уже не выйдет — сохраните несданное\n",
				short(h.runID), reason)
		}
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	for {
		select {
		case s := <-sigs:
			_ = cmd.Process.Signal(s)
		case err := <-waited:
			cancel()
			<-kept
			return exitStatus(argv[0], err)
		}
	}
}

// exitStatus — код выхода команды; убитая сигналом — 128+номер, как в shell.
func exitStatus(name string, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return ee.ExitCode(), nil
}
