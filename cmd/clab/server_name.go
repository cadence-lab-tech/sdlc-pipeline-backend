package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Имя установки (CLAB-164) — проект Docker Compose: префикс её контейнеров,
// томов и сети. С проектами внутри Cadence Lab оно не связано. Живёт в .env
// установки как COMPOSE_PROJECT_NAME; у установок до v0.7.1 строки нет, и они
// — cadence, как name: в docker-compose.yml пакета.
//
// Правило то же, что у lib.sh пакета (load_name, name_mismatch,
// name_in_use), тексты отказов те же: их читают cron клиентов и проверки
// ci/ в обоих режимах. Функции отдельные, а не внутри open(): ими же
// пользуется clab server install (CLAB-160) — выбор имени, проверка,
// «имя занято».

const defaultInstallationName = "cadence"

// installationNamePattern — правило compose для имени проекта.
var installationNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

const installationNameRule = "lowercase letters, digits, '-' and '_', starting with a letter or a digit"

func validInstallationName(name string) bool { return installationNamePattern.MatchString(name) }

// chooseInstallationName — имя новой установки: флаг --name, иначе
// COMPOSE_PROJECT_NAME окружения, иначе cadence. Оба заданы и различаются —
// ошибка в одном из них, а не выбор (install.sh, шаг 2).
func chooseInstallationName(flag, env string) (string, error) {
	if flag != "" && env != "" && flag != env {
		return "", fmt.Errorf("--name is '%s', but COMPOSE_PROJECT_NAME in the environment is '%s': give one of them, or the same name in both", flag, env)
	}
	name := flag
	if name == "" {
		name = env
	}
	if name == "" {
		name = defaultInstallationName
	}
	if !validInstallationName(name) {
		return "", fmt.Errorf("'%s' is not a valid installation name (--name): %s", name, installationNameRule)
	}
	return name, nil
}

// name — действующее имя установки: COMPOSE_PROJECT_NAME из .env, без
// строки (или с пустой) — cadence. fromFile — взято ли оно из .env.
// Окружение процесса здесь не участвует: оно только сверяется (checkName).
func (i *installation) name() (name string, fromFile bool, err error) {
	name = i.env["COMPOSE_PROJECT_NAME"]
	fromFile = name != ""
	if !fromFile {
		name = defaultInstallationName
	}
	if !validInstallationName(name) {
		return name, fromFile, fmt.Errorf("COMPOSE_PROJECT_NAME in .env is '%s', which is not a valid installation name: %s", name, installationNameRule)
	}
	return name, fromFile, nil
}

// callerName — COMPOSE_PROJECT_NAME окружения, в котором запущен clab;
// пустая переменная равна отсутствию.
func (i *installation) callerName() string { return i.e.getenv("COMPOSE_PROJECT_NAME") }

// checkName — lib.sh load_name: имя в .env верно, а COMPOSE_PROJECT_NAME
// окружения пуст или то же имя. Другое имя — другая установка, и команда
// отказывает до первого обращения к Docker.
func (i *installation) checkName() error {
	name, fromFile, err := i.name()
	if err != nil {
		return err
	}
	if caller := i.callerName(); caller != "" && caller != name {
		return nameMismatch(caller, name, fromFile, i.dir)
	}
	return nil
}

// nameMismatch — отказ, текст которого печатает lib.sh name_mismatch.
func nameMismatch(caller, name string, fromFile bool, dir string) error {
	if fromFile {
		return fmt.Errorf("COMPOSE_PROJECT_NAME is '%s' in the environment, but the installation in %s is '%s' (COMPOSE_PROJECT_NAME in its .env).\n"+
			"  Nothing was done: '%s' is another installation. Run the command without the variable:\n"+
			"    unset COMPOSE_PROJECT_NAME", caller, dir, name, caller)
	}
	return fmt.Errorf("COMPOSE_PROJECT_NAME is '%s' in the environment, but the installation in %s is '%s' (its .env has no COMPOSE_PROJECT_NAME: the default).\n"+
		"  Nothing was done. If '%s' is another installation, run the command without the variable:\n"+
		"    unset COMPOSE_PROJECT_NAME\n"+
		"  If this installation was installed as '%s' (before v0.7.1 the name was not kept in .env), add the name to its .env once:\n"+
		"    echo COMPOSE_PROJECT_NAME=%s >> %s",
		caller, dir, name, caller, caller, caller, filepath.Join(dir, ".env"))
}

// nameInUse — lib.sh name_in_use: где Docker этой машины уже держит проект
// compose с этим именем — каталог установки (метка её контейнеров),
// "containers" без метки или "volumes", если остались только тома. Пусто —
// имя свободно.
func nameInUse(ctx context.Context, r runner, name string) (string, error) {
	label := "label=com.docker.compose.project=" + name
	containers, err := output(ctx, r, cmdSpec{name: "docker", args: []string{"ps", "-a", "-q", "--filter", label}})
	if err != nil {
		return "", fmt.Errorf("could not ask Docker which installation names are taken (docker ps): %v", err)
	}
	volumes, err := output(ctx, r, cmdSpec{name: "docker", args: []string{"volume", "ls", "-q", "--filter", label}})
	if err != nil {
		return "", fmt.Errorf("could not ask Docker which installation names are taken (docker volume ls): %v", err)
	}
	if id := firstLine(containers); id != "" {
		dir, _ := output(ctx, r, cmdSpec{name: "docker", args: []string{"inspect", "-f",
			`{{index .Config.Labels "com.docker.compose.project.working_dir"}}`, id}})
		if dir = strings.TrimSpace(dir); dir != "" {
			return dir, nil
		}
		return "containers", nil
	}
	if strings.TrimSpace(volumes) != "" {
		return "volumes", nil
	}
	return "", nil
}
