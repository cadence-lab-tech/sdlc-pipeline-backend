package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Имя установки (CLAB-164, критерий 14): действующее имя из .env или
// cadence, compose получает его, даже если окружение задаёт другое,
// backup/restore/upgrade отказывают при расхождении до Docker, status
// предупреждает, неверное имя в .env — ошибка.

// withName — .env установки с COMPOSE_PROJECT_NAME (пусто — без строки).
func withName(t *testing.T, dir, name string) {
	t.Helper()
	body := readFile(t, filepath.Join(dir, ".env"))
	if name != "" {
		body = "COMPOSE_PROJECT_NAME=" + name + "\n" + body
	}
	mustWrite(t, filepath.Join(dir, ".env"), body, 0o600)
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func TestInstallationName(t *testing.T) {
	cases := []struct {
		line     string // строка .env; "-" — строки нет
		name     string
		fromFile bool
		err      bool
	}{
		{"-", "cadence", false, false},
		{"COMPOSE_PROJECT_NAME=", "cadence", false, false},
		{"COMPOSE_PROJECT_NAME=cadence-stage", "cadence-stage", true, false},
		{"COMPOSE_PROJECT_NAME='prod_2'", "prod_2", true, false},
		{"COMPOSE_PROJECT_NAME=Bad_Name", "Bad_Name", true, true},
		{"COMPOSE_PROJECT_NAME=-x", "-x", true, true},
	}
	for _, c := range cases {
		dir := installed(t, "v0.6.1", nil)
		if c.line != "-" {
			mustWrite(t, filepath.Join(dir, ".env"), c.line+"\n"+readFile(t, filepath.Join(dir, ".env")), 0o600)
		}
		e, _, _ := testEnv(t, dir, newFakeRunner(), map[string]string{"COMPOSE_PROJECT_NAME": "from-env"})
		inst, err := openInstallation(e, "")
		if err != nil {
			t.Fatal(err)
		}
		name, fromFile, err := inst.name()
		if name != c.name || fromFile != c.fromFile || (err != nil) != c.err {
			t.Errorf("%s: got %q %v %v, want %q %v error=%v", c.line, name, fromFile, err, c.name, c.fromFile, c.err)
		}
		if err != nil && !strings.Contains(err.Error(), "not a valid installation name") {
			t.Errorf("%s: %v", c.line, err)
		}
	}
}

func TestChooseInstallationName(t *testing.T) {
	cases := []struct {
		flag, env, want string
		err             string
	}{
		{"", "", "cadence", ""},
		{"stage", "", "stage", ""},
		{"", "stage", "stage", ""},
		{"stage", "stage", "stage", ""},
		{"stage", "prod", "", "--name is 'stage', but COMPOSE_PROJECT_NAME in the environment is 'prod'"},
		{"Bad_Name", "", "", "'Bad_Name' is not a valid installation name"},
		{"-x", "", "", "'-x' is not a valid installation name"},
	}
	for _, c := range cases {
		got, err := chooseInstallationName(c.flag, c.env)
		if c.err == "" && (err != nil || got != c.want) {
			t.Errorf("%q %q: %q %v, want %q", c.flag, c.env, got, err, c.want)
		}
		if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%q %q: %v, want %q", c.flag, c.env, err, c.err)
		}
	}
}

// Старая установка без строки — cadence для compose, даже если окружение
// чисто; имя из .env — для compose, а не переменная окружения.
func TestComposeEnvName(t *testing.T) {
	for _, c := range []struct{ line, env, want string }{
		{"", "", "cadence"},
		{"stage", "", "stage"},
		{"stage", "stage", "stage"},
	} {
		dir := installed(t, "v0.6.1", nil)
		withName(t, dir, c.line)
		e, _, _ := testEnv(t, dir, newFakeRunner(), map[string]string{"COMPOSE_PROJECT_NAME": c.env})
		inst, err := openInstallation(e, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := envValue(inst.composeEnv(), "COMPOSE_PROJECT_NAME"); got != c.want {
			t.Errorf(".env %q, env %q: compose gets %q, want %q", c.line, c.env, got, c.want)
		}
	}
}

// Критерий 8: расхождение — отказ backup, restore и upgrade до любого
// обращения к Docker; новых файлов нет. Текст называет оба имени и шаг.
func TestNameMismatchRefuses(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		line string // имя в .env; пусто — старая установка без строки
		hint string
	}{
		{"stage", "unset COMPOSE_PROJECT_NAME"},
		{"", "echo COMPOSE_PROJECT_NAME=other >> "},
	} {
		for _, args := range [][]string{
			{"backup"},
			{"restore", "--yes", "backups/cadence-v0.6.1-20261008-030000.sql.gz"},
			{"upgrade", "v0.6.2"},
		} {
			dir := installed(t, "v0.6.1", nil)
			withName(t, dir, c.line)
			mustWrite(t, filepath.Join(dir, "backups", "cadence-v0.6.1-20261008-030000.sql.gz"), gzipped(t, "x"), 0o600)
			before := tree(t, dir)
			r := newFakeRunner()
			e, _, errw := testEnv(t, dir, r, map[string]string{"COMPOSE_PROJECT_NAME": "other"})
			code := runServer(ctx, e, args)
			name := c.line
			if name == "" {
				name = "cadence"
			}
			msg := errw.String()
			if code != 1 || !strings.Contains(msg, "'other'") || !strings.Contains(msg, "'"+name+"'") || !strings.Contains(msg, c.hint) {
				t.Errorf(".env %q, %v: code %d\n%s", c.line, args, code, msg)
			}
			if c.line == "" && !strings.Contains(msg, filepath.Join(dir, ".env")) {
				t.Errorf("%v: the hint must name the .env of the installation:\n%s", args, msg)
			}
			if len(r.calls) != 0 {
				t.Errorf(".env %q, %v: Docker was asked before the refusal: %v", c.line, args, r.calls)
			}
			entries, _ := os.ReadDir(filepath.Join(dir, "backups"))
			if len(entries) != 1 || tree(t, dir) != before {
				t.Errorf(".env %q, %v: the refusal changed files", c.line, args)
			}
		}
	}
}

// Критерий 10 и совпадающее имя: пустая переменная равна отсутствию, то же
// имя — не расхождение; compose получает имя установки.
func TestNameSameOrEmptyPasses(t *testing.T) {
	for _, c := range []struct{ line, env string }{
		{"stage", "stage"},
		{"stage", ""},
		{"", ""},
		{"", "cadence"},
	} {
		dir := installed(t, "v0.6.1", nil)
		withName(t, dir, c.line)
		r := newFakeRunner()
		e, _, errw := testEnv(t, dir, r, map[string]string{"COMPOSE_PROJECT_NAME": c.env})
		if code := runServer(context.Background(), e, []string{"backup"}); code != 0 {
			t.Fatalf(".env %q, env %q: %s", c.line, c.env, errw)
		}
		want := c.line
		if want == "" {
			want = "cadence"
		}
		got := envValue(r.envs["docker compose ps -q --status running postgres"], "COMPOSE_PROJECT_NAME")
		if got != want {
			t.Errorf(".env %q, env %q: compose ran with %q, want %q", c.line, c.env, got, want)
		}
	}
}

// Неверное имя в .env — отказ команд, которые меняют стек.
func TestNameInvalidInEnvRefuses(t *testing.T) {
	dir := installed(t, "v0.6.1", nil)
	withName(t, dir, "Bad_Name")
	r := newFakeRunner()
	e, _, errw := testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 1 ||
		!strings.Contains(errw.String(), "COMPOSE_PROJECT_NAME in .env is 'Bad_Name', which is not a valid installation name") || len(r.calls) != 0 {
		t.Errorf("code %d, calls %v\n%s", code, r.calls, errw)
	}
}

// Критерии 7 и 9: status называет имя и откуда оно, перечисляет сервисы по
// нему, при расхождении предупреждает и выходит с 0.
func TestServerStatusName(t *testing.T) {
	ctx := context.Background()
	dir := installed(t, "v0.6.1", nil)

	r := newFakeRunner()
	e, out, _ := testEnv(t, dir, r, nil)
	if code := runServer(ctx, e, []string{"status"}); code != 0 || !strings.Contains(out.String(), "cadence (default)") {
		t.Errorf("old installation: %d\n%s", code, out)
	}

	withName(t, dir, "stage")
	r = newFakeRunner()
	e, out, _ = testEnv(t, dir, r, map[string]string{"COMPOSE_PROJECT_NAME": "other"})
	if code := runServer(ctx, e, []string{"status"}); code != 0 {
		t.Fatalf("a mismatch must not fail status: %d", code)
	}
	text := out.String()
	for _, want := range []string{"Name:", "stage (.env)", "! COMPOSE_PROJECT_NAME is 'other' in the environment, but this installation is 'stage'"} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}
	ps := "docker compose ps -a --format table {{.Service}}\t{{.State}}\t{{.Status}}"
	if got := envValue(r.envs[ps], "COMPOSE_PROJECT_NAME"); got != "stage" {
		t.Errorf("services must be listed for stage, compose ran with %q", got)
	}

	jsonOut = true
	defer func() { jsonOut = false }()
	out.Reset()
	if code := runServer(ctx, e, []string{"status"}); code != 0 {
		t.Fatal(code)
	}
	var view map[string]any
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["name"] != "stage" || view["name_from"] != ".env" || !strings.Contains(view["name_warning"].(string), "'other'") {
		t.Errorf("--json: %v", view)
	}

	// Неверное имя: status говорит об этом, compose не зовёт.
	mustWrite(t, filepath.Join(dir, ".env"), strings.Replace(readFile(t, filepath.Join(dir, ".env")), "=stage", "=Bad_Name", 1), 0o600)
	jsonOut = false
	r = newFakeRunner()
	e, out, _ = testEnv(t, dir, r, nil)
	if code := runServer(ctx, e, []string{"status"}); code != 0 || !strings.Contains(out.String(), "not a valid installation name") || r.called("docker compose ps") {
		t.Errorf("invalid name: %d %v\n%s", code, r.calls, out)
	}
}

// stubRunner отвечает на вызовы по точной строке; остальное — ошибка.
type stubRunner map[string]string

func (s stubRunner) run(_ context.Context, c cmdSpec) error {
	out, ok := s[c.String()]
	if !ok {
		return errExit1
	}
	if c.stdout != nil {
		c.stdout.Write([]byte(out))
	}
	return nil
}

// nameInUse — для clab server install (CLAB-160) и та же проверка, что у
// install.sh: метка compose на контейнерах или томах.
func TestNameInUse(t *testing.T) {
	ctx := context.Background()
	ps := "docker ps -a -q --filter label=com.docker.compose.project=cadence"
	vols := "docker volume ls -q --filter label=com.docker.compose.project=cadence"
	inspect := `docker inspect -f {{index .Config.Labels "com.docker.compose.project.working_dir"}} c1`
	cases := []struct {
		r    stubRunner
		want string
		err  bool
	}{
		{stubRunner{ps: "", vols: ""}, "", false},
		{stubRunner{ps: "c1\nc2\n", vols: "cadence_pgdata\n", inspect: "/opt/cadence\n"}, "/opt/cadence", false},
		{stubRunner{ps: "c1\n", vols: "", inspect: "\n"}, "containers", false},
		{stubRunner{ps: "", vols: "cadence_pgdata\n"}, "volumes", false},
		{stubRunner{vols: ""}, "", true},
	}
	for i, c := range cases {
		got, err := nameInUse(ctx, c.r, "cadence")
		if got != c.want || (err != nil) != c.err {
			t.Errorf("case %d: %q %v, want %q error=%v", i, got, err, c.want, c.err)
		}
	}
}
