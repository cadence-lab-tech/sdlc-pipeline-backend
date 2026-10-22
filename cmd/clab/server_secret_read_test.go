package main

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Тесты чтения файла секрета, отданного сервису (CLAB-252): на этой машине
// permission denied, файл читает одноразовый контейнер alpine:3. Отказ по
// правам подставляется (верстак работает от root, chmod его не даёт),
// Docker — подставной исполнитель. Номера — критерии приёмки CLAB-252.

// secretRunner — fakeRunner, который вдобавок отвечает за одноразовый
// контейнер чтения secrets/: отдаёт files по пути в контейнере.
type secretRunner struct {
	*fakeRunner
	files map[string]string // /s/<путь> → содержимое
	fail  string            // stderr отказа docker run; пусто — читает
	reads []cmdSpec
}

func newSecretRunner(files map[string]string) *secretRunner {
	return &secretRunner{fakeRunner: newFakeRunner(), files: files}
}

func (r *secretRunner) run(ctx context.Context, c cmdSpec) error {
	args := strings.Join(c.args, " ")
	if c.name != "docker" || !strings.HasPrefix(args, "run ") || !strings.Contains(args, " "+helperImage+" cat ") {
		return r.fakeRunner.run(ctx, c)
	}
	r.fakeRunner.mu.Lock()
	r.fakeRunner.calls = append(r.fakeRunner.calls, c.String())
	r.fakeRunner.mu.Unlock()
	r.reads = append(r.reads, c)
	if r.fail != "" {
		io.WriteString(c.stderr, r.fail+"\n")
		return exitErr(125)
	}
	body, ok := r.files[c.args[len(c.args)-1]]
	if !ok {
		io.WriteString(c.stderr, "cat: can't open '"+c.args[len(c.args)-1]+"': No such file or directory\n")
		return exitErr(1)
	}
	io.WriteString(c.stdout, body)
	return nil
}

// deniedRead — чтение на этой машине, которому файлы secrets/ отвечают
// permission denied, как файл 65532:65532 400 пользователю хоста.
func deniedRead(p string) ([]byte, error) {
	if strings.Contains(p, string(filepath.Separator)+"secrets"+string(filepath.Separator)) {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
	}
	return os.ReadFile(p)
}

// secretsFilesEnv — .env установки в режиме секретов-файлов; extra — строки
// токена.
func secretsFilesEnv(api *fakeAPI, extra string) string {
	return "CADENCE_MODE=proxy\nCOMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.secrets.yml:compose.override.yml\n" +
		"CADENCE_HTTP_ADDR=" + api.addr() + "\nSDLC_BOOTSTRAP_TEAM=acme\n" + extra
}

// AC-2, AC-3, AC-1: токен файлом 65532:400 — и формой .env, и из compose
// config — читает контейнер; значение без \r\n, ни в одном аргументе его
// нет, команда — ровно docker run --rm --log-driver none --network none
// -v <dir>/secrets:/s:ro alpine:3 cat /s/<файл>.
func TestStarterTokenFileReadByContainer(t *testing.T) {
	for _, tc := range []struct {
		name, env, apiEnv, file string
	}{
		{name: ".env", env: "SDLC_ADMIN_TOKEN_FILE=/secrets/admin_token\n", file: "admin_token"},
		{name: "compose config", env: "SDLC_ADMIN_TOKEN=\n",
			apiEnv: `{"SDLC_ADMIN_TOKEN": "", "SDLC_ADMIN_TOKEN_FILE": "/run/secrets/admin_token"}`, file: "admin_token"},
		{name: "nested", env: "SDLC_ADMIN_TOKEN_FILE=/secrets/tokens/admin\n", file: "tokens/admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t, "v0.7.0", false)
			api.token = testAdminToken
			dir := starterInstalled(t, api, testStarterBundle, secretsFilesEnv(api, tc.env))
			mustWrite(t, filepath.Join(dir, "secrets", filepath.FromSlash(tc.file)), testAdminToken+"\r\n", 0o400)

			r := newSecretRunner(map[string]string{"/s/" + tc.file: testAdminToken + "\r\n"})
			r.apiEnvironment = tc.apiEnv
			e, out, errw := testEnv(t, t.TempDir(), r.fakeRunner, nil)
			e.run = r
			e.readFile = deniedRead
			if code := starter(e, "--dir", dir, "--project", "ACME"); code != 0 {
				t.Fatalf("code %d\n%s", code, errw)
			}
			if strings.Contains(out.String()+errw.String(), testAdminToken) {
				t.Errorf("the token is in the output:\n%s%s", out, errw)
			}
			noTokenInCalls(t, r.fakeRunner, testAdminToken)
			for _, a := range api.auth {
				if a != "Bearer "+testAdminToken {
					t.Errorf("Authorization %q", a)
				}
			}
			if len(api.auth) == 0 {
				t.Error("no request reached the API")
			}
			if len(r.reads) != 1 {
				t.Fatalf("container reads: %d", len(r.reads))
			}
			c := r.reads[0]
			want := []string{"run", "--rm", "--log-driver", "none", "--network", "none",
				"-v", filepath.Join(dir, "secrets") + ":/s:ro", "alpine:3", "cat", "/s/" + tc.file}
			if strings.Join(c.args, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("container read:\n got %q\nwant %q", c.args, want)
			}
			if c.name != "docker" || c.stdin != nil || c.stdout == e.out || c.stderr == e.errw {
				t.Errorf("container read goes to the user or takes stdin: %+v", c)
			}
		})
	}
}

// AC-5: файл читается или токен в .env — контейнера нет; нет файла,
// каталог, пустой файл — прежние ошибки, тоже без контейнера.
func TestStarterTokenWithoutContainer(t *testing.T) {
	api := newFakeAPI(t, "v0.7.0", false)
	api.token = testAdminToken
	run := func(t *testing.T, env string, denied bool, prep func(dir string)) (int, string, *secretRunner) {
		t.Helper()
		dir := starterInstalled(t, api, testStarterBundle, secretsFilesEnv(api, env))
		if prep != nil {
			prep(dir)
		}
		r := newSecretRunner(map[string]string{"/s/admin_token": testAdminToken})
		e, _, errw := testEnv(t, t.TempDir(), r.fakeRunner, nil)
		e.run = r
		if denied {
			e.readFile = deniedRead
		}
		code := starter(e, "--dir", dir, "--project", "ACME")
		return code, errw.String(), r
	}

	t.Run("readable file", func(t *testing.T) {
		code, errw, r := run(t, "SDLC_ADMIN_TOKEN_FILE=/secrets/admin_token\n", false, func(dir string) {
			mustWrite(t, filepath.Join(dir, "secrets", "admin_token"), testAdminToken+"\n", 0o600)
		})
		if code != 0 || len(r.reads) != 0 {
			t.Errorf("code %d, container reads %d\n%s", code, len(r.reads), errw)
		}
	})
	t.Run("token in .env", func(t *testing.T) {
		code, errw, r := run(t, "SDLC_ADMIN_TOKEN="+testAdminToken+"\n", true, nil)
		if code != 0 || len(r.reads) != 0 {
			t.Errorf("code %d, container reads %d\n%s", code, len(r.reads), errw)
		}
	})
	for _, tc := range []struct {
		name, want string
		prep       func(dir string)
	}{
		{name: "no file", want: "no such file or directory"},
		{name: "directory", want: "is a directory", prep: func(dir string) {
			os.MkdirAll(filepath.Join(dir, "secrets", "admin_token"), 0o700)
		}},
		{name: "empty", want: "is empty", prep: func(dir string) {
			mustWrite(t, filepath.Join(dir, "secrets", "admin_token"), "\n", 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, errw, r := run(t, "SDLC_ADMIN_TOKEN_FILE=/secrets/admin_token\n", false, tc.prep)
			if code != 1 || !strings.Contains(errw, "SDLC_ADMIN_TOKEN_FILE") || !strings.Contains(errw, tc.want) || len(r.reads) != 0 {
				t.Errorf("code %d, container reads %d\n%s", code, len(r.reads), errw)
			}
		})
	}
}

// AC-6: контейнер не прочитал (docker отказал или вывод пуст) — ошибка
// шага с переменной, путём, permission denied и «Secrets in files», без
// значения и без запросов к API.
func TestStarterTokenContainerFails(t *testing.T) {
	for _, tc := range []struct {
		name, fail, body, want string
	}{
		{name: "docker refuses", fail: "docker: Error response from daemon: pull access denied for alpine", want: "pull access denied"},
		// Как docker на деле: причина, пустая строка и подсказка --help.
		{name: "docker refuses with the hint", fail: "Unable to find image 'alpine:3' locally\n" +
			"docker: Error response from daemon: failed to resolve reference \"docker.io/library/alpine:3\": network is unreachable\n\n" +
			"Run 'docker run --help' for more information", want: "network is unreachable"},
		{name: "empty output", body: "\n", want: "it read nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t, "v0.7.0", false)
			api.token = testAdminToken
			dir := starterInstalled(t, api, testStarterBundle, secretsFilesEnv(api, "SDLC_ADMIN_TOKEN_FILE=/secrets/admin_token\n"))
			host := filepath.Join(dir, "secrets", "admin_token")
			mustWrite(t, host, testAdminToken+"\n", 0o400)
			r := newSecretRunner(map[string]string{"/s/admin_token": tc.body})
			r.fail = tc.fail
			e, out, errw := testEnv(t, t.TempDir(), r.fakeRunner, nil)
			e.run = r
			e.readFile = deniedRead
			if code := starter(e, "--dir", dir, "--project", "ACME"); code != 1 {
				t.Fatalf("code %d", code)
			}
			msg := errw.String()
			for _, s := range []string{"SDLC_ADMIN_TOKEN_FILE", host, "permission denied", "alpine:3", tc.want, `"Secrets in files"`, "65532", "440"} {
				if !strings.Contains(msg, s) {
					t.Errorf("the error lacks %q:\n%s", s, msg)
				}
			}
			if strings.Contains(out.String()+msg, testAdminToken) {
				t.Errorf("the token is in the output:\n%s%s", out, msg)
			}
			if len(api.auth) != 0 || api.instanceHit != 0 {
				t.Errorf("requests to the API: %v, instance %d", api.auth, api.instanceHit)
			}
		})
	}
}

// R5: тот же механизм у файлов внешней базы (backup): пароль, отданный
// сервису, читает контейнер, и он уходит клиенту бэкапа в PGPASSWORD, а не
// в аргументы.
func TestBackupPasswordFileReadByContainer(t *testing.T) {
	dir := fileInstalled(t, "v0.7.0", "SDLC_DATABASE_URL='"+extSafeURL+"'\nSDLC_DATABASE_PASSWORD_FILE=/run/secrets/db_password\n",
		map[string]string{"db_password": extPassword + "\n"})
	r := newSecretRunner(map[string]string{"/s/db_password": extPassword + "\n"})
	r.postgresRunning = false
	e, out, errw := testEnv(t, dir, r.fakeRunner, nil)
	e.run = r
	e.readFile = deniedRead
	if code := runServer(context.Background(), e, []string{"backup"}); code != 0 {
		t.Fatalf("code %d\n%s", code, errw)
	}
	if len(r.reads) == 0 {
		t.Fatal("the password file was not read by the container")
	}
	call := "docker compose run --rm --no-deps -T -e PGPASSWORD db-client pg_dump -d " + extSafeURL + " " + strings.Join(dumpOptions, " ")
	if !r.called(call) || !envHas(r.envs[call], "PGPASSWORD="+extPassword) {
		t.Errorf("the backup client gets no PGPASSWORD from the file: %v", r.calls)
	}
	noPasswordIn(t, r.fakeRunner, out.String(), errw.String())
}
