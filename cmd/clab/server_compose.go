package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// cmdSpec — запуск внешней команды: docker, docker compose, git.
type cmdSpec struct {
	dir    string
	env    []string // nil — окружение процесса
	name   string
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// detach — команда, после прерывания которой у демона остаётся
	// состояние, которое надо убрать (docker run временного реестра):
	// она идёт в своей группе процессов, и Ctrl-C терминала до неё не
	// доходит, а отмена ctx её не обрывает — запрос доходит до конца, и
	// уборка видит то, что он создал (CLAB-202, AC-8). Предел — detachTimeout.
	detach bool
}

// detachTimeout — предел команды с detach: она не обрывается сигналом, но и
// висеть бесконечно ей нельзя.
const detachTimeout = 2 * time.Minute

func (c cmdSpec) String() string { return strings.Join(append([]string{c.name}, c.args...), " ") }

// runner исполняет внешние команды. Настоящий — os/exec; в тестах —
// подставной, который отвечает за docker и git без Docker в верстаке.
type runner interface {
	run(ctx context.Context, c cmdSpec) error
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, c cmdSpec) error {
	if c.detach {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), detachTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	if c.detach {
		ownProcessGroup(cmd)
	}
	cmd.Dir = c.dir
	cmd.Env = c.env
	cmd.Stdin = c.stdin
	cmd.Stdout = c.stdout
	cmd.Stderr = c.stderr
	return cmd.Run()
}

// lookPath — есть ли команда в PATH; в тестах подменяется.
var lookPath = exec.LookPath

// output — stdout команды строкой; stderr уходит к человеку, если задан.
func output(ctx context.Context, r runner, c cmdSpec) (string, error) {
	var out bytes.Buffer
	c.stdout = &out
	if c.stderr == nil {
		c.stderr = io.Discard
	}
	err := r.run(ctx, c)
	return out.String(), err
}

// requireDocker — lib.sh require_docker: Docker с плагином compose v2 и
// отвечающий демон.
func requireDocker(ctx context.Context, e *serverEnv) error {
	if _, err := lookPath("docker"); err != nil {
		return errors.New("Docker is not installed. Install Docker Engine with the compose plugin: https://docs.docker.com/engine/install/")
	}
	if _, err := output(ctx, e.run, cmdSpec{name: "docker", args: []string{"compose", "version"}}); err != nil {
		return errors.New("Docker Compose v2 is missing ('docker compose version' failed). Install the docker-compose-plugin package: https://docs.docker.com/compose/install/linux/")
	}
	if _, err := output(ctx, e.run, cmdSpec{name: "docker", args: []string{"info"}}); err != nil {
		return errors.New("The Docker daemon does not answer. Start it (systemctl start docker) or run as a user in the docker group.")
	}
	return nil
}

// docker — команда docker в каталоге установки с её окружением.
func (i *installation) docker(args ...string) cmdSpec {
	return cmdSpec{dir: i.dir, env: i.composeEnv(), name: "docker", args: args}
}

// compose — docker compose в каталоге установки: COMPOSE_FILE и прочее
// приходят из .env через окружение, версия образов — из VERSION.
func (i *installation) compose(args ...string) cmdSpec {
	return i.docker(append([]string{"compose"}, args...)...)
}

// composeRun — docker compose с выводом к человеку; stdout можно заглушить,
// как >/dev/null у скриптов.
func (i *installation) composeRun(ctx context.Context, quiet bool, args ...string) error {
	c := i.compose(args...)
	c.stdout, c.stderr = i.e.out, i.e.errw
	if quiet {
		c.stdout = io.Discard
	}
	return i.e.run.run(ctx, c)
}

// containerOf — lib.sh container_of: ID контейнера сервиса, запущенного или
// остановленного.
func (i *installation) containerOf(ctx context.Context, service string) string {
	out, err := output(ctx, i.e.run, i.compose("ps", "-a", "-q", service))
	if err != nil {
		return ""
	}
	return firstLine(out)
}

// running — lib.sh running: у сервиса есть запущенный контейнер.
func (i *installation) running(ctx context.Context, service string) bool {
	out, err := output(ctx, i.e.run, i.compose("ps", "-q", "--status", "running", service))
	return err == nil && strings.TrimSpace(out) != ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if j := strings.IndexByte(s, '\n'); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// uiProbes — адреса, по которым waitReady ждёт интерфейс, по порядку.
// Порт ui зависит от версии образа, а не от версии clab: до v0.14.0 образ
// слушает только :80 (root), с v0.14.0 — :80 и :8080 (CLAB-236), дальше —
// только :8080 (CLAB-248). Готовность при upgrade ждёт clab исходной версии,
// поэтому проверка терпима к обоим портам: clab этой версии дождётся стека
// любой из них, а образ без :80 выходит через релиз после неё (CLAB-247).
var uiProbes = []string{"http://ui:8080/", "http://ui:80/"}

// waitReady — lib.sh wait_ready: API здоров по своей проверке (/readyz:
// сервис поднят и база отвечает), а интерфейс отвечает через Caddy на одном
// из uiProbes. Ожидание считается шагами по 3 секунды, как в скрипте:
// нулевой таймаут — «не готов» без единой проверки, на этом стоит сценарий
// отката в ci/.
func (i *installation) waitReady(ctx context.Context, timeout int) bool {
	for waited := 0; waited < timeout; waited += 3 {
		if ctx.Err() != nil {
			return false
		}
		if api := i.containerOf(ctx, "api"); api != "" {
			health, _ := output(ctx, i.e.run, i.docker("inspect", "-f", "{{.State.Health.Status}}", api))
			if strings.TrimSpace(health) == "healthy" && i.uiAnswers(ctx) {
				return true
			}
		}
		i.e.sleep(3 * time.Second)
	}
	return false
}

// uiAnswers — интерфейс отвечает на одном из uiProbes: wget из контейнера
// caddy, как в lib.sh, прямо в ui. Маршрут Caddyfile пакета этим не
// проверяется — он сменит порт вместе с образом в одном релизе (CLAB-248).
func (i *installation) uiAnswers(ctx context.Context) bool {
	for _, url := range uiProbes {
		if _, err := output(ctx, i.e.run, i.compose("exec", "-T", "caddy", "wget", "-q", "-O", "/dev/null", url)); err == nil {
			return true
		}
	}
	return false
}

// showFailure — что скрипты печатают, когда стек не поднялся: состояние
// сервисов и хвост логов.
func (i *installation) showFailure(ctx context.Context, services ...string) {
	_ = i.composeRun(ctx, false, "ps")
	_ = i.composeRun(ctx, false, append([]string{"logs", "--tail", "40"}, services...)...)
}

// readyTimeout — CADENCE_READY_TIMEOUT в секундах, по умолчанию 300.
func (i *installation) readyTimeout() (int, error) {
	raw := i.getDefault("CADENCE_READY_TIMEOUT", "300")
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n < 0 || fmt.Sprint(n) != strings.TrimSpace(raw) {
		return 0, fmt.Errorf("CADENCE_READY_TIMEOUT is '%s', expected a number of seconds", raw)
	}
	return n, nil
}
