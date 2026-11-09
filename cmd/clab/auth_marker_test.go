package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-424, версия 2: команды clab auth берут хост проекта каталога из
// маркера. Конфиг — два входа (a — хост по умолчанию, b), маркер называет b.

func runAuth(t *testing.T, c *Client, args ...string) (string, string, error) {
	t.Helper()
	return capture(t, func() error { return cmdAuth(context.Background(), c, args) })
}

type authStatusOut struct {
	Config    string           `json:"config"`
	Hosts     []authHostStatus `json:"hosts"`
	Env       []string         `json:"env"`
	Directory *struct {
		MarkerPath   string `json:"marker_path"`
		Project      string `json:"project"`
		Host         string `json:"host"`
		LoggedIn     bool   `json:"logged_in"`
		Valid        bool   `json:"valid"`
		Handle       string `json:"handle"`
		Error        string `json:"error"`
		Hint         string `json:"hint"`
		OverriddenBy string `json:"overridden_by"`
	} `json:"directory"`
}

func statusJSON(t *testing.T, c *Client) authStatusOut {
	t.Helper()
	withJSON(t, true)
	out, _, err := runAuth(t, c, "status")
	if err != nil {
		t.Fatalf("auth status: %v", err)
	}
	var st authStatusOut
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("не JSON: %s", out)
	}
	return st
}

// AC-21: блок «Каталог» в трёх случаях — вход есть, входа нет, токен не
// принят; код 0, прежние ключи на месте.
func TestAuthStatusDirectory(t *testing.T) {
	a, b, cfg := twoHosts(t)
	root, cfgFile := markerEnv(t, cfg)
	marker := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("P", b.srv.URL))
	hint := "Войдите на " + b.srv.URL + ": clab auth login --host " + b.srv.URL

	st := statusJSON(t, newMarkerClient(t, globals{}))
	d := st.Directory
	if d == nil || d.MarkerPath != marker || d.Project != "P" || d.Host != b.srv.URL || !d.LoggedIn || !d.Valid ||
		d.Handle != "user:bob" || d.Hint != "" || d.OverriddenBy != "" {
		t.Fatalf("вход есть: %+v", d)
	}
	if st.Config != cfgFile || len(st.Hosts) != 2 || st.Env == nil {
		t.Fatalf("прежние ключи: %+v", st)
	}
	withJSON(t, false)
	out, _, err := runAuth(t, newMarkerClient(t, globals{}), "status")
	if err != nil || !strings.Contains(out, "Каталог: проект P (маркер "+marker+")") ||
		!strings.Contains(out, "хост "+b.srv.URL+" — вход есть: user:bob, токен действует") ||
		!strings.Contains(out, b.key+" — user:bob, токен действует (хост проекта каталога)") {
		t.Fatalf("текст:\n%s", out)
	}

	// Токен не принят: заглушка ждёт другой.
	bad := cfg
	bad.Hosts = map[string]HostConfig{a.key: a.hc(), b.key: {URL: b.srv.URL, Token: "sdlc_stale", Handle: "user:bob"}}
	if err := saveConfigAt(filepath.Dir(cfgFile), bad); err != nil {
		t.Fatal(err)
	}
	d = statusJSON(t, newMarkerClient(t, globals{})).Directory
	if d == nil || !d.LoggedIn || d.Valid || d.Error == "" || d.Hint != hint {
		t.Fatalf("токен не принят: %+v", d)
	}

	// Входа нет.
	none := Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}
	if err := saveConfigAt(filepath.Dir(cfgFile), none); err != nil {
		t.Fatal(err)
	}
	d = statusJSON(t, newMarkerClient(t, globals{})).Directory
	if d == nil || d.LoggedIn || d.Valid || d.Hint != hint {
		t.Fatalf("входа нет: %+v", d)
	}
	withJSON(t, false)
	out, _, err = runAuth(t, newMarkerClient(t, globals{}), "status")
	if err != nil || !strings.Contains(out, "входа нет. "+hint) {
		t.Fatalf("текст без входа:\n%s %v", out, err)
	}

	// Перекрыт флагом и окружением.
	if d = statusJSON(t, newMarkerClient(t, globals{host: a.srv.URL})).Directory; d == nil || d.OverriddenBy != hostFromFlag {
		t.Fatalf("--host: %+v", d)
	}
	t.Setenv("SDLC_API_URL", a.srv.URL)
	if d = statusJSON(t, newMarkerClient(t, globals{})).Directory; d == nil || d.OverriddenBy != hostFromEnv {
		t.Fatalf("SDLC_API_URL: %+v", d)
	}
	t.Setenv("SDLC_API_URL", "")

	// Маркер без host — хост по умолчанию.
	writeMarkerTestFile(t, marker, markerText("P", ""))
	d = statusJSON(t, newMarkerClient(t, globals{})).Directory
	if d == nil || d.Host != "" || !strings.Contains(d.Hint, "будет хост по умолчанию "+a.key) || !d.Valid {
		t.Fatalf("маркер без хоста: %+v", d)
	}
}

// AC-21: без маркера вывод auth status байт в байт прежний и ключа
// directory нет.
func TestAuthStatusWithoutMarkerUnchanged(t *testing.T) {
	a := newStubHost(t, "sdlc_token_A", "alice")
	cfg := Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}
	_, cfgFile := markerEnv(t, cfg)

	withJSON(t, false)
	out, _, err := runAuth(t, newMarkerClient(t, globals{}), "status")
	want := "* " + a.key + " — user:alice, токен действует\nКонфиг: " + cfgFile + "\n"
	if err != nil || out != want {
		t.Fatalf("вывод:\n%q\nвместо\n%q", out, want)
	}
	withJSON(t, true)
	out, _, _ = runAuth(t, newMarkerClient(t, globals{}), "status")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(out)
	}
	if _, ok := raw["directory"]; ok || len(raw) != 3 {
		t.Fatalf("ключи: %s", out)
	}
}

// AC-23: битый маркер — список входов выведен, предупреждение с путём, код
// 0, проза файла не напечатана.
func TestAuthStatusBrokenMarker(t *testing.T) {
	a := newStubHost(t, "sdlc_token_A", "alice")
	cfg := Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}
	root, _ := markerEnv(t, cfg)
	for name, body := range map[string]string{
		"атрибут":     "Секретная проза hunter2\n<!-- cadence-lab: project=P token=sdlc_leak -->\n",
		"два маркера": "Секретная проза hunter2\n<!-- cadence-lab: project=P -->\n<!-- cadence-lab: project=Q -->\n",
	} {
		path := writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), body)
		withJSON(t, false)
		out, _, err := runAuth(t, newMarkerClient(t, globals{}), "status")
		if err != nil || !strings.Contains(out, "* "+a.key+" — user:alice, токен действует") ||
			!strings.Contains(out, "Маркер каталога не принят: маркер "+path) ||
			strings.Contains(out, "hunter2") || strings.Contains(out, "sdlc_leak") {
			t.Fatalf("%s:\n%s %v", name, out, err)
		}
		d := statusJSON(t, newMarkerClient(t, globals{})).Directory
		if d == nil || !strings.Contains(d.Error, path) {
			t.Fatalf("%s: --json %+v", name, d)
		}
	}
}

// AC-22: login без --host входит на хост маркера; default_host остаётся
// прежним, а пустой — заполняется; явный --host переключает его.
func TestAuthLoginMarkerHost(t *testing.T) {
	a, b, _ := twoHosts(t)
	cfg := Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}
	root, cfgFile := markerEnv(t, cfg)
	marker := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("P", b.srv.URL))

	withJSON(t, false)
	out, errOut, err := runAuth(t, newMarkerClient(t, globals{}), "login", "--no-browser")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, errOut)
	}
	if !strings.HasPrefix(errOut, "Хост взят из маркера "+marker+": "+b.srv.URL+"\n") {
		t.Fatalf("хост и маркер не первой строкой:\n%s", errOut)
	}
	if len(a.requests()) != 0 || !hasRequest(b.requests(), "POST /v1/auth/cli") {
		t.Fatalf("запросы: a %v, b %v", a.requests(), b.requests())
	}
	got := readCfg(t, cfgFile)
	if got.DefaultHost != a.key || got.Hosts[b.key].Token != b.token || got.Hosts[a.key].Token != a.token {
		t.Fatalf("конфиг: %+v", got)
	}
	if !strings.Contains(out, "Хост по умолчанию не менялся: "+a.key) {
		t.Fatalf("вывод:\n%s", out)
	}

	// Пустой default_host заполняется хостом маркера.
	if err := saveConfigAt(filepath.Dir(cfgFile), Config{Hosts: map[string]HostConfig{}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runAuth(t, newMarkerClient(t, globals{}), "login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	if got := readCfg(t, cfgFile); got.DefaultHost != b.key {
		t.Fatalf("пустой default_host: %+v", got)
	}

	// Явный --host — как раньше: становится хостом по умолчанию.
	withStdin(t, a.token+"\n")
	if _, _, err := runAuth(t, newMarkerClient(t, globals{host: a.srv.URL}), "login", "--with-token"); err != nil {
		t.Fatal(err)
	}
	if got := readCfg(t, cfgFile); got.DefaultHost != a.key {
		t.Fatalf("--host: %+v", got)
	}
	// SDLC_API_URL сильнее маркера.
	t.Setenv("SDLC_API_URL", a.srv.URL)
	withStdin(t, a.token+"\n")
	before := len(b.requests())
	if _, errOut, err := runAuth(t, newMarkerClient(t, globals{}), "login", "--with-token"); err != nil || strings.Contains(errOut, "маркер") {
		t.Fatalf("SDLC_API_URL: %v %s", err, errOut)
	}
	if len(b.requests()) != before {
		t.Fatal("с SDLC_API_URL вход пошёл на хост маркера")
	}
}

// AC-22, AC-24: logout и token без --host — запись хоста маркера; записи нет
// — ошибка с подсказкой, токен другого хоста не печатается.
func TestAuthLogoutAndTokenMarkerHost(t *testing.T) {
	a, b, cfg := twoHosts(t)
	root, cfgFile := markerEnv(t, cfg)
	writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("P", b.srv.URL))

	withJSON(t, false)
	out, _, err := runAuth(t, newMarkerClient(t, globals{}), "token")
	if err != nil || out != b.token+"\n" {
		t.Fatalf("token: %q %v", out, err)
	}
	t.Setenv("SDLC_TOKEN", "sdlc_env")
	if out, _, _ = runAuth(t, newMarkerClient(t, globals{}), "token"); out != "sdlc_env\n" {
		t.Fatalf("SDLC_TOKEN: %q", out)
	}
	t.Setenv("SDLC_TOKEN", "")

	out, errOut, err := runAuth(t, newMarkerClient(t, globals{}), "logout")
	if err != nil || !strings.Contains(out, "Вышли с "+b.key) || !strings.Contains(errOut, "Хост взят из маркера") {
		t.Fatalf("logout: %q %q %v", out, errOut, err)
	}
	got := readCfg(t, cfgFile)
	if _, ok := got.Hosts[b.key]; ok || got.DefaultHost != a.key || got.Hosts[a.key].Token != a.token {
		t.Fatalf("конфиг после logout: %+v", got)
	}
	if !hasRequest(b.requests(), "DELETE /v1/auth/tokens/tok-bob") || hasRequest(a.requests(), "DELETE") {
		t.Fatalf("отзыв: a %v b %v", a.requests(), b.requests())
	}

	// Записи для хоста маркера больше нет.
	out, _, err = runAuth(t, newMarkerClient(t, globals{}), "token")
	if err == nil || strings.Contains(out, a.token) || !strings.Contains(err.Error(), "clab auth login --host "+b.srv.URL) {
		t.Fatalf("token без записи: %q %v", out, err)
	}
	if _, _, err = runAuth(t, newMarkerClient(t, globals{}), "logout"); err == nil || !strings.Contains(err.Error(), "в конфиге нет такого хоста") {
		t.Fatalf("logout без записи: %v", err)
	}
	if got := readCfg(t, cfgFile); got.DefaultHost != a.key {
		t.Fatalf("default_host тронут: %+v", got)
	}
	// Ни одна команда не отправила токен чужому хосту.
	if a.sawAuth(b.token) || b.sawAuth(a.token) {
		t.Fatal("токен ушёл на чужой хост")
	}
}

// AC-25: --with-token на хост из маркера — отказ до чтения stdin и без
// запросов; с явным --host — как раньше.
func TestAuthLoginWithTokenRefusesMarkerHost(t *testing.T) {
	_, b, _ := twoHosts(t)
	root, cfgFile := markerEnv(t, Config{Hosts: map[string]HostConfig{}})
	writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("P", b.srv.URL))

	withStdin(t, b.token+"\n")
	_, _, err := runAuth(t, newMarkerClient(t, globals{}), "login", "--with-token")
	if err == nil || !strings.Contains(err.Error(), "для --with-token укажите --host явно") {
		t.Fatalf("--with-token: %v", err)
	}
	if len(b.requests()) != 0 {
		t.Fatalf("запросы: %v", b.requests())
	}
	rest, _ := io.ReadAll(os.Stdin)
	if string(rest) != b.token+"\n" {
		t.Fatalf("stdin прочитан: %q", rest)
	}
	if _, err := os.Stat(cfgFile); err == nil && len(readCfg(t, cfgFile).Hosts) != 0 {
		t.Fatal("конфиг изменён")
	}

	withStdin(t, b.token+"\n")
	if _, _, err := runAuth(t, newMarkerClient(t, globals{host: b.srv.URL}), "login", "--with-token"); err != nil {
		t.Fatalf("--with-token --host: %v", err)
	}
}

// AC-8, AC-14 UC-14: битый маркер — login, logout, token без хоста отказывают
// ошибкой маркера с советом --host; с явным --host маркер не разбирается.
func TestAuthBrokenMarker(t *testing.T) {
	a, _, cfg := twoHosts(t)
	root, _ := markerEnv(t, cfg)
	path := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), "<!-- cadence-lab: project=P token=sdlc_leak -->\n")
	for _, args := range [][]string{{"login", "--no-browser"}, {"logout"}, {"token"}} {
		_, _, err := runAuth(t, newMarkerClient(t, globals{}), args...)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "--host") || strings.Contains(err.Error(), "sdlc_leak") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if len(a.requests()) != 0 {
		t.Fatalf("запросы: %v", a.requests())
	}
	reads := countMarkerReads(t)
	if out, _, err := runAuth(t, newMarkerClient(t, globals{host: a.srv.URL}), "token"); err != nil || out != a.token+"\n" {
		t.Fatalf("token --host: %q %v", out, err)
	}
	if *reads != 0 {
		t.Fatalf("с --host маркер прочитан %d раз", *reads)
	}
}
