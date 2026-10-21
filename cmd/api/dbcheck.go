package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/secrets"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// dbcheck — предпроверка базы SDLC_DATABASE_URL (CLAB-127): `/api dbcheck
// [--install] [--json]`. Образ distroless, поэтому проверку, как и
// healthcheck, делает сам бинарник — тем же драйвером и TLS, что сервис.
// Код выхода: 0 — база годится (предупреждения допустимы), 1 — нет,
// 2 — ошибка в аргументах.
func dbcheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dbcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	install := fs.Bool("install", false, "the database is for a new installation: an existing Cadence schema is a warning")
	timeout := fs.Duration("timeout", 15*time.Second, "time to connect and check")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: /api dbcheck [--install] [--json] [--timeout 15s]\n\n"+
			"Checks the database of SDLC_DATABASE_URL (or SDLC_DATABASE_URL_FILE, SDLC_DATABASE_PASSWORD_FILE)\n"+
			"for this version of Cadence Lab: the URL and its TLS,\n"+
			"the connection, the server version, the extensions vector and pgcrypto, the privileges of the\n"+
			"role and the state of the schema. Only reads. Exit status 0 - ready, 1 - not ready.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	// Адрес — тем же разбором, что у сервиса: значением, файлом
	// SDLC_DATABASE_URL_FILE или с паролем из SDLC_DATABASE_PASSWORD_FILE
	// (решение 0052). Ошибка чтения — пункт url отчёта, без значения.
	var report postgres.CheckReport
	if dsn, err := secrets.OS().DatabaseURL(); err != nil {
		report = postgres.FailedCheck("url", err.Error(),
			"secrets in files: docs/admin.md, \"Secrets in files\"")
	} else {
		report = postgres.CheckDatabase(context.Background(), dsn,
			postgres.CheckOptions{Install: *install, Timeout: *timeout})
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printReport(stdout, report)
	}
	if !report.OK {
		return 1
	}
	return 0
}

// printReport — пункты с префиксами скриптов пакета (lib.sh: ✓, !, ✗) и
// подсказкой строкой ниже.
func printReport(w io.Writer, r postgres.CheckReport) {
	for _, c := range r.Checks {
		mark := "✓"
		switch c.Status {
		case postgres.CheckWarn:
			mark = "!"
		case postgres.CheckFail:
			mark = "✗"
		}
		fmt.Fprintf(w, "%s %s: %s\n", mark, c.Name, c.Detail)
		if c.Hint != "" && c.Status != postgres.CheckOK {
			fmt.Fprintf(w, "    %s\n", c.Hint)
		}
	}
	if r.OK {
		fmt.Fprintf(w, "✓ the database %s is ready for Cadence Lab %s\n", r.Target, version)
	} else {
		fmt.Fprintf(w, "✗ the database %s is not ready for Cadence Lab %s: see above\n", orUnknown(r.Target), version)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
