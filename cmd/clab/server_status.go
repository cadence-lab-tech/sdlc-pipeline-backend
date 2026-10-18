package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const statusHelp = `clab server status — the installation directory, how it was installed, the
version of the stack (VERSION) and of this clab, whether they go together, the
mode and address, and the services.

Usage: clab server status [--dir DIR] [--json]

Options:
  --dir DIR   the installation directory (default: $CADENCE_DIR, else the
              current directory or the nearest one above it)
  --json      print the same as JSON

It changes nothing and needs no Docker to report the files; without Docker
the services are left out.`

// serverStatus — что за установка и в каком она состоянии. Код выхода — 0,
// если установка найдена: это вопрос, а не проверка; несовместимость версий
// сказана в выводе, а команды, которые меняют стек, на ней откажут.
func serverStatus(ctx context.Context, e *serverEnv, args []string) error {
	var dir string
	pos, err := parseServerArgs(args, nil, map[string]*string{"--dir": &dir})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("unexpected argument: %s", pos[0])}
	}
	inst, err := openInstallation(e, dir)
	if err != nil {
		return err
	}

	compat, compatible := "yes", true
	if err := inst.compatible(); err != nil {
		compat, compatible = err.Error(), false
	}
	view := map[string]any{
		"dir":        inst.dir,
		"kind":       inst.kind(),
		"version":    inst.version,
		"clab":       e.cli,
		"compatible": compatible,
		"env":        inst.hasEnv,
	}
	if !compatible {
		view["compatibility"] = compat
	}
	if inst.hasEnv {
		view["mode"] = inst.getDefault("CADENCE_MODE", "tls")
		view["url"] = inst.publicURL()
	}
	services := ""
	dockerErr := requireDocker(ctx, e)
	if dockerErr == nil && inst.hasEnv {
		out, err := output(ctx, e.run, inst.compose("ps", "-a", "--format", "table {{.Service}}\t{{.State}}\t{{.Status}}"))
		if err == nil {
			services = strings.TrimRight(out, "\n")
		} else {
			services = "docker compose ps failed: " + err.Error()
		}
		view["services"] = services
	} else if dockerErr != nil {
		view["docker"] = dockerErr.Error()
	}

	if jsonOut {
		body, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(e.out, string(body))
		return nil
	}
	pairs := []string{
		"Installation:", inst.dir,
		"Installed as:", inst.kind(),
		"Stack:", inst.version + " (VERSION)",
		"clab:", e.cli,
	}
	if compatible {
		pairs = append(pairs, "Compatible:", "yes")
	} else {
		pairs = append(pairs, "Compatible:", "no - see below")
	}
	switch {
	case !inst.hasEnv:
		pairs = append(pairs, ".env:", "missing - not installed yet: ./install.sh")
	case dockerErr != nil:
		pairs = append(pairs, "Mode:", inst.getDefault("CADENCE_MODE", "tls"), "Address:", inst.publicURL(),
			"Services:", "unknown - "+dockerErr.Error())
	case services == "":
		pairs = append(pairs, "Mode:", inst.getDefault("CADENCE_MODE", "tls"), "Address:", inst.publicURL(),
			"Services:", "none - the stack is not started")
	default:
		pairs = append(pairs, "Mode:", inst.getDefault("CADENCE_MODE", "tls"), "Address:", inst.publicURL())
	}
	e.kv(pairs...)
	if inst.hasEnv && dockerErr == nil && services != "" {
		fmt.Fprintln(e.out, "Services:")
		for _, line := range strings.Split(services, "\n") {
			fmt.Fprintln(e.out, "  "+line)
		}
	}
	if !compatible {
		fmt.Fprintf(e.out, "! %s\n", compat)
	}
	return nil
}
