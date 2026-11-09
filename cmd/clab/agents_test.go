package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-427: clab agents — настройка агентов проекта (решение 0077 п. 4.8).
// Тесты идут со стабом сервера; сервер настройки — CLAB-425.

const agentsDocument = `{"agents":{"claude":{"models":["claude-haiku-4-5","claude-opus-5-5"],` +
	`"levels":{"high":{"model":"claude-opus-5-5","effort":"high"},"low":{"model":"claude-haiku-4-5"}}},` +
	`"codex":{"levels":{"high":{"model":"gpt-6.1-sol","effort":"high"}}}},` +
	`"pricing":{"as_of":"2026-11-01","models":{"claude-opus-5-5":{"input":1,"output":2,"cache_read":0.1,"cache_write":1.25},` +
	`"gpt-6.1-sol":{"input":1.5,"output":2}}}}`

func agentsVersionReply(version int, state, doc string) string {
	return `{"project":"CLAB","version":` + itoa(version) + `,"state":"` + state + `","document":` + doc + `,` +
		`"changelog":"новая модель","sha256":"abcdef0123456789","created_by":"user:alice","created_at":"2026-11-01T10:00:00Z",` +
		`"activated_by":"user:alice","activated_at":"2026-11-01T11:00:00Z","warnings":["агент mystery неизвестен набору"]}`
}

type agentsReq struct {
	method, path string
	body         map[string]any
}

// agentsStub отвечает по таблице «метод путь»; всё, чего в ней нет, — 404 без
// кода, как роутер сервера без ручки. Запросы записываются.
type agentsStub struct {
	reqs []agentsReq
}

type agentsReply struct {
	status int
	ctype  string
	body   string
}

func newAgentsStub(t *testing.T, routes map[string]agentsReply) (*Client, *agentsStub) {
	t.Helper()
	stub := &agentsStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := agentsReq{method: r.Method, path: r.URL.Path}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &req.body)
		}
		stub.reqs = append(stub.reqs, req)
		w.Header().Set(headerServerVersion, "v0.27.1")
		rep, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("404 page not found"))
			return
		}
		if rep.ctype == "" {
			rep.ctype = "application/json"
		}
		w.Header().Set("Content-Type", rep.ctype)
		if rep.status == 0 {
			rep.status = http.StatusOK
		}
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, token: "clab_x", http: srv.Client()}, stub
}

func (s *agentsStub) writes() int {
	n := 0
	for _, r := range s.reqs {
		if r.method != http.MethodGet {
			n++
		}
	}
	return n
}

const agentsBasePath = "/v1/projects/CLAB/agent-settings"

func runAgents(t *testing.T, c *Client, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	withJSON(t, false)
	return capture(t, func() error {
		return cmdAgents(context.Background(), c, append(args, "--project", "CLAB"))
	})
}

// AC-1: шапка, таблица агентов с «—» у пустого усилия, доступные модели,
// прайс с датой и «—» у ненайденной цены кэша; без фигурных скобок; --json —
// тело ответа как есть.
func TestAgentsShowActive(t *testing.T) {
	reply := agentsVersionReply(3, "active", agentsDocument)
	c, stub := newAgentsStub(t, map[string]agentsReply{"GET " + agentsBasePath: {body: reply}})
	out, _, err := runAgents(t, c, "show")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Проект:", "CLAB", "3 (active)", "user:alice", "новая модель",
		"АГЕНТ", "УРОВЕНЬ", "МОДЕЛЬ", "УСИЛИЕ",
		"Доступные модели (claude): claude-haiku-4-5, claude-opus-5-5",
		"Прайс на 2026-11-01, USD за 1 млн токенов",
		"! агент mystery неизвестен набору",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "{}") {
		t.Errorf("фигурные скобки документа в выводе:\n%s", out)
	}
	if low := lineWith(out, "claude-haiku-4-5  "); !strings.HasSuffix(strings.TrimSpace(low), "—") {
		t.Errorf("у low нет усилия «—»: %q", low)
	}
	if gpt := lineWith(out, "1.5"); !strings.HasSuffix(strings.TrimSpace(gpt), "—") {
		t.Errorf("у gpt нет «—» вместо цены кэша: %q", gpt)
	}
	if len(stub.reqs) != 1 || stub.writes() != 0 {
		t.Errorf("запросы: %+v", stub.reqs)
	}

	withJSON(t, true)
	jsonOut, _, err := capture(t, func() error {
		return cmdAgents(context.Background(), c, []string{"show", "--project", "CLAB"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	_ = json.Unmarshal([]byte(jsonOut), &got)
	_ = json.Unmarshal([]byte(reply), &want)
	if !equalJSON(got, want) {
		t.Errorf("--json не равен ответу сервера:\n%s", jsonOut)
	}
}

// AC-2: --version N, версия retired, проект без настройки.
func TestAgentsShowVersionAndNone(t *testing.T) {
	c, stub := newAgentsStub(t, map[string]agentsReply{
		"GET " + agentsBasePath + "/versions/1": {body: agentsVersionReply(1, "retired", `{}`)},
		"GET " + agentsBasePath:                 {body: `{"version":null}`},
	})
	out, _, err := runAgents(t, c, "show", "--version", "1")
	if err != nil || !strings.Contains(out, "1 (retired)") {
		t.Fatalf("%v\n%s", err, out)
	}
	if stub.reqs[0].path != agentsBasePath+"/versions/1" {
		t.Errorf("путь %s", stub.reqs[0].path)
	}

	out, _, err = runAgents(t, c, "show")
	if err != nil {
		t.Fatalf("нет настройки — не ошибка: %v", err)
	}
	if !strings.Contains(out, "у проекта нет настройки агентов: исполнители берут таблицу привязки по умолчанию") {
		t.Errorf("фраза:\n%s", out)
	}
	withJSON(t, true)
	jsonOut, _, _ := capture(t, func() error {
		return cmdAgents(context.Background(), c, []string{"show", "--project", "CLAB"})
	})
	if !strings.Contains(jsonOut, `"version": null`) {
		t.Errorf("--json: %s", jsonOut)
	}
}

// AC-3: versions.
func TestAgentsVersions(t *testing.T) {
	list := `{"versions":[` +
		`{"version":3,"state":"draft","changelog":"черновик","sha256":"1234567890","created_by":"user:alice","created_at":"2026-11-02T10:00:00Z","activated_by":null,"activated_at":null},` +
		`{"version":2,"state":"active","changelog":null,"sha256":"abcdef0123","created_by":"user:alice","created_at":"2026-11-01T10:00:00Z","activated_by":"user:bob","activated_at":"2026-11-01T11:00:00Z"}]}`
	c, _ := newAgentsStub(t, map[string]agentsReply{"GET " + agentsBasePath + "/versions": {body: list}})
	out, _, err := runAgents(t, c, "versions")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ВЕРСИЯ", "СОСТОЯНИЕ", "draft", "active", "user:bob", "1234567", "abcdef0", "черновик"} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "12345678") {
		t.Errorf("sha256 длиннее семи знаков:\n%s", out)
	}
}

func writeAgentsFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// AC-4: тело draft по флагам; второй draft — conflict сервера.
func TestAgentsDraftBodies(t *testing.T) {
	file := writeAgentsFile(t, `{"agents":{}}`)
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"draft"}, map[string]any{"from": "active"}},
		{[]string{"draft", "--empty"}, map[string]any{"from": "empty"}},
		{[]string{"draft", "--file", file}, map[string]any{"document": map[string]any{"agents": map[string]any{}}}},
		{[]string{"draft", "--changelog", "заметка"}, map[string]any{"from": "active", "changelog": "заметка"}},
	}
	for _, tc := range cases {
		c, stub := newAgentsStub(t, map[string]agentsReply{
			"POST " + agentsBasePath + "/draft": {status: 201, body: agentsVersionReply(4, "draft", `{}`)},
		})
		out, _, err := runAgents(t, c, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if !strings.Contains(out, "черновик, версия 4") {
			t.Errorf("%v: вывод %q", tc.args, out)
		}
		if len(stub.reqs) != 1 || !equalJSON(stub.reqs[0].body, tc.want) {
			t.Errorf("%v: тело %v, ждали %v", tc.args, stub.reqs, tc.want)
		}
	}

	c, _ := newAgentsStub(t, map[string]agentsReply{
		"POST " + agentsBasePath + "/draft": {status: 409, body: `{"error":{"code":"conflict","message":"черновик уже есть"}}`},
	})
	_, _, err := runAgents(t, c, "draft")
	var api *APIError
	if !errors.As(err, &api) || api.Code != "conflict" {
		t.Errorf("ждали conflict сервера: %v", err)
	}
}

// AC-5: put из файла и stdin; не JSON — отказ до запроса с путём файла; нет
// черновика — совет.
func TestAgentsPut(t *testing.T) {
	file := writeAgentsFile(t, `{"agents":{}}`)
	c, stub := newAgentsStub(t, map[string]agentsReply{
		"PUT " + agentsBasePath + "/draft": {body: agentsVersionReply(4, "draft", `{}`)},
	})
	if _, _, err := runAgents(t, c, "put", "--file", file, "--changelog", "правка"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"document": map[string]any{"agents": map[string]any{}}, "changelog": "правка"}
	if len(stub.reqs) != 1 || stub.reqs[0].method != "PUT" || !equalJSON(stub.reqs[0].body, want) {
		t.Errorf("запрос %+v", stub.reqs)
	}

	// stdin
	r, w, _ := os.Pipe()
	_, _ = w.WriteString(`{"pricing":{}}`)
	_ = w.Close()
	prev := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = prev })
	c2, stub2 := newAgentsStub(t, map[string]agentsReply{
		"PUT " + agentsBasePath + "/draft": {body: agentsVersionReply(4, "draft", `{}`)},
	})
	if _, _, err := runAgents(t, c2, "put", "--file", "-"); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(stub2.reqs[0].body, map[string]any{"document": map[string]any{"pricing": map[string]any{}}}) {
		t.Errorf("stdin: %+v", stub2.reqs)
	}

	// не JSON и нечитаемый файл: ни одного запроса, в сообщении путь файла
	bad := writeAgentsFile(t, `{"agents": секрет-sk-123`)
	c3, stub3 := newAgentsStub(t, nil)
	_, _, err := runAgents(t, c3, "put", "--file", bad)
	if err == nil || !strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), "sk-123") {
		t.Errorf("не JSON: %v", err)
	}
	if _, _, err = runAgents(t, c3, "put", "--file", filepath.Join(t.TempDir(), "нет.json")); err == nil {
		t.Error("нечитаемый файл прошёл")
	}
	if len(stub3.reqs) != 0 {
		t.Errorf("запросы до проверки файла: %+v", stub3.reqs)
	}

	// нет черновика
	c4, _ := newAgentsStub(t, map[string]agentsReply{
		"PUT " + agentsBasePath + "/draft": {status: 404, body: `{"error":{"code":"not_found","message":"черновика нет"}}`},
	})
	_, _, err = runAgents(t, c4, "put", "--file", file)
	if err == nil || !strings.Contains(err.Error(), "clab agents draft") {
		t.Errorf("совет про draft: %v", err)
	}
	if _, _, err = runAgents(t, c4, "put"); err == nil {
		t.Error("put без --file прошёл")
	}
}

// AC-6: diff — изменено, добавлено, убрано; без изменений; без черновика;
// без активной версии; --json; записей нет.
func TestAgentsDiff(t *testing.T) {
	active := `{"agents":{"claude":{"levels":{"low":{"model":"haiku"},"high":{"model":"opus-4"}}}}}`
	draft := `{"agents":{"claude":{"levels":{"high":{"model":"opus-5"}}},"codex":{"levels":{"high":{"model":"gpt"}}}}}`
	routes := map[string]agentsReply{
		"GET " + agentsBasePath + "/versions":   {body: `{"versions":[{"version":2,"state":"active"},{"version":3,"state":"draft"}]}`},
		"GET " + agentsBasePath + "/versions/3": {body: agentsVersionReply(3, "draft", draft)},
		"GET " + agentsBasePath + "/versions/2": {body: agentsVersionReply(2, "active", active)},
		"GET " + agentsBasePath:                 {body: agentsVersionReply(2, "active", active)},
	}
	c, stub := newAgentsStub(t, routes)
	out, _, err := runAgents(t, c, "diff")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`изменено   agents.claude.levels.high.model: "opus-4" → "opus-5"`,
		`добавлено  agents.codex.levels.high.model = "gpt"`,
		`убрано     agents.claude.levels.low.model (было "haiku")`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q:\n%s", want, out)
		}
	}
	if stub.writes() != 0 {
		t.Errorf("diff пишет: %+v", stub.reqs)
	}

	// --version 2 против активной 2: без изменений
	out, _, err = runAgents(t, c, "diff", "--version", "2")
	if err != nil || strings.TrimSpace(out) != "без изменений" {
		t.Errorf("%v %q", err, out)
	}

	// --json
	withJSON(t, true)
	js, _, err := capture(t, func() error {
		return cmdAgents(context.Background(), c, []string{"diff", "--project", "CLAB"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var ch []agentChange
	if json.Unmarshal([]byte(js), &ch) != nil || len(ch) != 3 || ch[0].Path == "" || ch[0].Op == "" {
		t.Errorf("--json: %s", js)
	}

	// нет черновика и нет --version
	c2, _ := newAgentsStub(t, map[string]agentsReply{
		"GET " + agentsBasePath + "/versions": {body: `{"versions":[{"version":2,"state":"active"}]}`},
	})
	_, _, err = runAgents(t, c2, "diff")
	if err == nil || !strings.Contains(err.Error(), "clab agents draft") {
		t.Errorf("совет: %v", err)
	}

	// нет активной версии — всё «добавлено»
	c3, _ := newAgentsStub(t, map[string]agentsReply{
		"GET " + agentsBasePath + "/versions":   {body: `{"versions":[{"version":1,"state":"draft"}]}`},
		"GET " + agentsBasePath + "/versions/1": {body: agentsVersionReply(1, "draft", `{"agents":{"claude":{"levels":{"low":{"model":"haiku"}}}}}`)},
		"GET " + agentsBasePath:                 {body: `{"version":null}`},
	})
	out, _, err = runAgents(t, c3, "diff")
	if err != nil || !strings.HasPrefix(out, "добавлено  agents.claude.levels.low.model") {
		t.Errorf("%v\n%s", err, out)
	}
}

// AC-7: activate и discard.
func TestAgentsActivateAndDiscard(t *testing.T) {
	c, stub := newAgentsStub(t, map[string]agentsReply{
		"POST " + agentsBasePath + "/draft/activate": {body: `{"activated":` + agentsVersionReply(3, "active", `{}`) + `,"retired":2}`},
		"DELETE " + agentsBasePath + "/draft":        {status: 204},
	})
	out, _, err := runAgents(t, c, "activate")
	if err != nil || strings.TrimSpace(out) != "активирована версия 3, снята 2" {
		t.Fatalf("%v %q", err, out)
	}
	if stub.reqs[0].body != nil {
		t.Errorf("тело activate без --version: %v", stub.reqs[0].body)
	}
	if _, _, err = runAgents(t, c, "activate", "--version", "3"); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(stub.reqs[1].body, map[string]any{"version": float64(3)}) {
		t.Errorf("тело %v", stub.reqs[1].body)
	}
	out, _, err = runAgents(t, c, "discard")
	if err != nil || strings.TrimSpace(out) != "черновик удалён" {
		t.Fatalf("%v %q", err, out)
	}
	if last := stub.reqs[len(stub.reqs)-1]; last.method != "DELETE" {
		t.Errorf("метод %s", last.method)
	}
	withJSON(t, true)
	js, _, _ := capture(t, func() error {
		return cmdAgents(context.Background(), c, []string{"discard", "--project", "CLAB"})
	})
	if !strings.Contains(js, `"discarded": true`) {
		t.Errorf("--json: %s", js)
	}

	// первая активация: снятой нет
	c2, _ := newAgentsStub(t, map[string]agentsReply{
		"POST " + agentsBasePath + "/draft/activate": {body: `{"activated":` + agentsVersionReply(1, "active", `{}`) + `,"retired":null}`},
	})
	out, _, _ = runAgents(t, c2, "activate")
	if strings.TrimSpace(out) != "активирована версия 1" {
		t.Errorf("%q", out)
	}

	// номер не тот и нет черновика — коды сервера
	c3, _ := newAgentsStub(t, map[string]agentsReply{
		"POST " + agentsBasePath + "/draft/activate": {status: 409, body: `{"error":{"code":"conflict","message":"черновик теперь версия 4"}}`},
		"DELETE " + agentsBasePath + "/draft":        {status: 404, body: `{"error":{"code":"not_found","message":"черновика нет"}}`},
	})
	var api *APIError
	if _, _, err = runAgents(t, c3, "activate", "--version", "3"); !errors.As(err, &api) || api.Code != "conflict" {
		t.Errorf("conflict: %v", err)
	}
	if _, _, err = runAgents(t, c3, "discard"); !errors.As(err, &api) || api.Code != "not_found" {
		t.Errorf("not_found: %v", err)
	}
}

// AC-8: ошибки проверки по полям; значения секрета в выводе нет.
func TestAgentsValidationByField(t *testing.T) {
	file := writeAgentsFile(t, `{"agents":{"claude":{"levels":{"high":{"model":"sk-ant-SECRET123"}}}}}`)
	cases := []struct {
		details, message, want string
	}{
		{`{"path":"agents.claude.levels.high.temperature"}`,
			"настройка агентов, agents.claude.levels.high.temperature: поле неизвестно",
			"документ не принят: agents.claude.levels.high.temperature: поле неизвестно"},
		{`{"path":""}`, "настройка агентов, документ: размер больше предела",
			"документ не принят: документ в целом: размер больше предела"},
		{`{"path":"agents.claude.levels.high.model"}`,
			"настройка агентов, agents.claude.levels.high.model: похоже на секрет",
			"документ не принят: agents.claude.levels.high.model: похоже на секрет"},
	}
	for _, tc := range cases {
		c, _ := newAgentsStub(t, map[string]agentsReply{
			"PUT " + agentsBasePath + "/draft": {status: 400,
				body: `{"error":{"code":"invalid_input","message":"` + tc.message + `","details":` + tc.details + `}}`},
		})
		_, _, err := runAgents(t, c, "put", "--file", file)
		if err == nil || err.Error() != tc.want {
			t.Errorf("ждали %q, получили %v", tc.want, err)
		}
		if err != nil && (strings.Contains(err.Error(), "SECRET123") || strings.Contains(err.Error(), "{")) {
			t.Errorf("в ошибке значение или JSON: %v", err)
		}
	}
}

// AC-9: отказ прав с причиной; один запрос, повторов нет.
func TestAgentsForbidden(t *testing.T) {
	file := writeAgentsFile(t, `{}`)
	cases := []struct{ reason, want string }{
		{"level", "править настройку агентов может человек уровня maintainer и выше"},
		{"human_action", "агентскому принципалу правка закрыта"},
	}
	for _, tc := range cases {
		forbidden := agentsReply{status: 403,
			body: `{"error":{"code":"forbidden","message":"нет права","details":{"reason":"` + tc.reason + `"}}}`}
		for _, args := range [][]string{{"put", "--file", file}, {"draft"}, {"activate"}, {"discard"}} {
			c, stub := newAgentsStub(t, map[string]agentsReply{
				"PUT " + agentsBasePath + "/draft":           forbidden,
				"POST " + agentsBasePath + "/draft":          forbidden,
				"POST " + agentsBasePath + "/draft/activate": forbidden,
				"DELETE " + agentsBasePath + "/draft":        forbidden,
			})
			_, _, err := runAgents(t, c, args...)
			if err == nil || err.Error() != tc.want {
				t.Errorf("%v/%s: %v", args, tc.reason, err)
			}
			if len(stub.reqs) != 1 {
				t.Errorf("%v: %d запросов, ждали один", args, len(stub.reqs))
			}
		}
	}
	// 403 без причины (токен задачи, белый список) — текст сервера как есть
	c, _ := newAgentsStub(t, map[string]agentsReply{
		"POST " + agentsBasePath + "/draft": {status: 403, body: `{"error":{"code":"forbidden","message":"токену задачи закрыто"}}`},
	})
	_, _, err := runAgents(t, c, "draft")
	var api *APIError
	if !errors.As(err, &api) || api.Message != "токену задачи закрыто" {
		t.Errorf("%v", err)
	}
}

// AC-10: сервер без настройки — один отказ с версией сервера, без HTML и
// паники; ошибка домена под него не попадает.
func TestAgentsOldServer(t *testing.T) {
	file := writeAgentsFile(t, `{}`)
	replies := map[string]agentsReply{
		"404":   {status: 404, ctype: "text/plain", body: "404 page not found"},
		"405":   {status: 405, ctype: "text/plain", body: "Method Not Allowed"},
		"html":  {status: 200, ctype: "text/html", body: "<!doctype html><html><body>SPA</body></html>"},
		"junk":  {status: 200, body: "not json {"},
		"alien": {status: 200, body: `{"hello":"world"}`},
	}
	subs := [][]string{{"show"}, {"versions"}, {"draft"}, {"put", "--file", file}, {"diff", "--version", "2"}, {"activate"}, {"discard"}}
	for name, rep := range replies {
		for _, args := range subs {
			routes := map[string]agentsReply{}
			for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
				for _, p := range []string{"", "/versions", "/versions/2", "/draft", "/draft/activate"} {
					routes[m+" "+agentsBasePath+p] = rep
				}
			}
			c, _ := newAgentsStub(t, routes)
			_, _, err := runAgents(t, c, args...)
			// DELETE с 200 без тела и без разбора — ответ чужой ручки не отличить;
			// остальное обязано отказать.
			if err == nil {
				if args[0] == "discard" && (name == "html" || name == "junk" || name == "alien") {
					continue
				}
				t.Errorf("%s %v: нет ошибки", name, args)
				continue
			}
			msg := err.Error()
			if !errors.Is(err, errOldServer) ||
				!strings.Contains(msg, "clab agents: сервер v0.27.1 не знает настройки агентов — нужен сервер v0.28.0 или новее") ||
				strings.Contains(strings.ToLower(msg), "<html") || strings.Contains(msg, "panic") {
				t.Errorf("%s %v: %q", name, args, msg)
			}
		}
	}

	// ошибка домена (нет версии 99) — не «сервер старый»
	c, _ := newAgentsStub(t, map[string]agentsReply{
		"GET " + agentsBasePath + "/versions/99": {status: 404, body: `{"error":{"code":"not_found","message":"версии 99 нет"}}`},
	})
	_, _, err := runAgents(t, c, "show", "--version", "99")
	var api *APIError
	if errors.Is(err, errOldServer) || !errors.As(err, &api) || api.Code != "not_found" {
		t.Errorf("not_found принят за старый сервер: %v", err)
	}
}

// AC-18: справка и регистрация.
func TestAgentsHelpAndRegistration(t *testing.T) {
	found := false
	for _, cmd := range commands() {
		if cmd.name == "agents" {
			found = true
			for _, sub := range agentsSubcommands {
				if !strings.Contains(cmd.usage, "clab agents "+sub) {
					t.Errorf("в справке нет примера %s", sub)
				}
			}
		}
	}
	if !found {
		t.Fatal("команда agents не зарегистрирована")
	}
	c, _ := newAgentsStub(t, nil)
	ctx := context.Background()
	if _, _, err := capture(t, func() error { return cmdAgents(ctx, c, []string{"--help"}) }); err != nil {
		t.Errorf("--help: %v", err)
	}
	if _, _, err := capture(t, func() error { return cmdAgents(ctx, c, []string{"show", "--help"}) }); err != nil {
		t.Errorf("show --help: %v", err)
	}
	if _, _, err := capture(t, func() error { return cmdAgents(ctx, c, nil) }); err == nil || errors.Is(err, flag.ErrHelp) {
		t.Errorf("без подкоманды ждали ошибку: %v", err)
	}
	_, _, err := capture(t, func() error { return cmdAgents(ctx, c, []string{"bogus"}) })
	if err == nil {
		t.Fatal("неизвестная подкоманда прошла")
	}
	for _, sub := range agentsSubcommands {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("в перечне нет %s: %v", sub, err)
		}
	}
}
