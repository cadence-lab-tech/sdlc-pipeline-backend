package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

type auditFixture struct {
	t      *testing.T
	srv    *Server
	pool   *pgxpool.Pool
	auth   *workflow.Auth
	admin  workflow.Identity
	root   string
	bot    string
	botTok uuid.UUID
}

func newAuditFixture(t *testing.T) *auditFixture {
	t.Helper()
	ctx := context.Background()
	store, pool := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	authoring := workflow.NewAuthoring(store, log, engine)
	mcpServer := mcp.NewServer(log, store, engine, authoring, auth, "test")
	srv := NewServer(config.Config{Env: "test"}, log, store, engine, authoring, auth, mcpServer)

	const root = "bootstrap-token-0123456789"
	if err := auth.Bootstrap(ctx, "root", root); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetMember(ctx, admin, "SDLC", "agent:bot", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	issued, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{Name: "bot", Principal: "agent:bot", ProjectKey: "SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	return &auditFixture{t: t, srv: srv, pool: pool, auth: auth, admin: admin,
		root: root, bot: issued.Token, botTok: issued.Record.ID}
}

func (f *auditFixture) do(method, path, token string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

type restAuditRow struct {
	Project, Task, Principal, TokenName, TokenKind, RequestID *string
	TokenID                                                   *uuid.UUID
	Channel, Action, Outcome                                  string
	Status                                                    *int
	Params                                                    string
}

func (f *auditFixture) rows(where string, args ...any) []restAuditRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT pr.key, tk.key, p.handle, a.token_name, a.token_kind, a.request_id, a.token_id,
		       a.channel, a.action, a.outcome, a.http_status, a.params::text
		FROM audit_log a
		LEFT JOIN projects pr ON pr.id = a.project_id
		LEFT JOIN tasks tk ON tk.id = a.task_id
		LEFT JOIN principals p ON p.id = a.principal_id
		WHERE `+where+` ORDER BY a.created_at, a.id`, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []restAuditRow
	for rows.Next() {
		var r restAuditRow
		if err := rows.Scan(&r.Project, &r.Task, &r.Principal, &r.TokenName, &r.TokenKind, &r.RequestID,
			&r.TokenID, &r.Channel, &r.Action, &r.Outcome, &r.Status, &r.Params); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (f *auditFixture) count() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func s(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Изменяющие вызовы REST пишутся одной записью: канал rest, действие —
// метод и шаблон маршрута, параметры пути и тела, статус и код (CLAB-210,
// критерии 4–7, 12, 13). Чтения, поиск POST'ом и heartbeat не пишутся.
func TestRESTMutationsAreAudited(t *testing.T) {
	f := newAuditFixture(t)

	// Каждый изменяющий вызов — ровно одна запись с ожидаемой целью.
	cases := []struct {
		method, path string
		body         any
		action       string
		project      string
		task         string
	}{
		{"PATCH", "/v1/tasks/SDLC-3/scope", map[string]any{"add_repositories": []string{"acme/nope"}, "note": "n"},
			"PATCH /v1/tasks/:taskKey/scope", "SDLC", "SDLC-3"},
		{"POST", "/v1/tasks/SDLC-3/artifacts", map[string]any{"kind": "analysis.brief", "title": "T", "content": "секретное тело артефакта"},
			"POST /v1/tasks/:taskKey/artifacts", "SDLC", "SDLC-3"},
		{"POST", "/v1/projects/SDLC/import", map[string]any{"bundle": map[string]any{}, "dry_run": true},
			"POST /v1/projects/:projectKey/import", "SDLC", ""},
		{"POST", "/v1/auth/tokens", map[string]any{"name": "ci", "project": "SDLC"},
			"POST /v1/auth/tokens", "SDLC", ""},
		{"POST", "/v1/projects/SDLC/roles/analyst/versions", map[string]any{"instructions": strings.Repeat("и", 5000)},
			"POST /v1/projects/:projectKey/roles/:roleKey/versions", "SDLC", ""},
		{"POST", "/v1/teams", map[string]any{"key": "acme", "name": "Acme"},
			"POST /v1/teams", "", ""},
	}
	var issuedToken string
	for _, tc := range cases {
		before := f.count()
		rec := f.do(tc.method, tc.path, f.root, tc.body)
		if tc.path == "/v1/auth/tokens" {
			var out struct {
				Token string `json:"token"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			issuedToken = out.Token
		}
		if f.count() != before+1 {
			t.Fatalf("%s %s: записей прибавилось %d, нужна одна (код %d)", tc.method, tc.path, f.count()-before, rec.Code)
		}
		got := f.rows("a.action = $1", tc.action)
		r := got[len(got)-1]
		switch {
		case r.Channel != "rest" || s(r.Project) != tc.project || s(r.Task) != tc.task:
			t.Fatalf("%s: канал %s, проект %q, задача %q", tc.action, r.Channel, s(r.Project), s(r.Task))
		case r.Status == nil || *r.Status != rec.Code:
			t.Fatalf("%s: статус %v, ответ %d", tc.action, r.Status, rec.Code)
		case (rec.Code < 400) != (r.Outcome == "ok"):
			t.Fatalf("%s: исход %s при коде %d", tc.action, r.Outcome, rec.Code)
		case s(r.Principal) != "root" || s(r.TokenKind) != "api" || r.TokenID == nil || s(r.RequestID) == "":
			t.Fatalf("%s: принципал %q, токен %v %q, request_id %q", tc.action, s(r.Principal), r.TokenID, s(r.TokenKind), s(r.RequestID))
		}
	}
	if issuedToken == "" {
		t.Fatal("токен не выдан")
	}

	// Тело артефакта — размер и sha256; длинная инструкция — тоже; выданный
	// токен в журнал не попал.
	art := f.rows("a.action = 'POST /v1/tasks/:taskKey/artifacts'")[0]
	sum := sha256.Sum256([]byte("секретное тело артефакта"))
	if strings.Contains(art.Params, "секретное") || !strings.Contains(art.Params, hex.EncodeToString(sum[:])) {
		t.Fatalf("content артефакта: %s", art.Params)
	}
	role := f.rows("a.action = 'POST /v1/projects/:projectKey/roles/:roleKey/versions'")[0]
	if strings.Contains(role.Params, "иииии") || !strings.Contains(role.Params, `"sha256"`) {
		t.Fatalf("длинная строка: %s", role.Params)
	}

	// Не пишутся: успешные чтения, поиск и просмотр приглашения POST'ом.
	before := f.count()
	for _, rq := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/projects/SDLC", nil},
		{"GET", "/v1/tasks/SDLC-3", nil},
		{"POST", "/v1/projects/SDLC/search", map[string]any{"query": "x"}},
		{"POST", "/v1/invites/lookup", map[string]any{"token": "clab_inv_nope"}},
	} {
		rec := f.do(rq.method, rq.path, f.root, rq.body)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Fatalf("%s %s: %d", rq.method, rq.path, rec.Code)
		}
	}
	if f.count() != before {
		t.Fatalf("чтения записаны: %+v", f.rows("true")[before:])
	}

	// Секреты: код входа CLI из пути, токен приглашения из тела.
	f.do("POST", "/v1/auth/cli/SECRETCODE-42", f.root, map[string]any{"approve": true})
	f.do("POST", "/v1/invites/accept", f.root, map[string]any{"token": "clab_inv_supersecret"})
	all := f.rows("true")
	for _, r := range all {
		for _, secret := range []string{"SECRETCODE-42", "clab_inv_supersecret", f.root, f.bot, issuedToken} {
			if strings.Contains(r.Params, secret) {
				t.Fatalf("%s: в параметрах секрет %q: %s", r.Action, secret, r.Params)
			}
		}
	}
	if len(f.rows("a.action = 'POST /v1/auth/cli/:code'")) != 1 || len(f.rows("a.action = 'POST /v1/invites/accept'")) != 1 {
		t.Fatal("вход CLI или приём приглашения не записаны")
	}

	// Сбой записи журнала не меняет ответ.
	f.srv.audit.FailingSinkForTest(errors.New("журнал недоступен"))
	n := f.count()
	if rec := f.do("POST", "/v1/teams", f.root, map[string]any{"key": "beta", "name": "Beta"}); rec.Code != http.StatusCreated {
		t.Fatalf("сбой журнала уронил вызов: %d %s", rec.Code, rec.Body)
	}
	if f.count() != n {
		t.Fatal("запись легла в отказавшее хранилище")
	}
}

// Учётные данные внутри ссылок не доходят до audit_log (CLAB-226, критерии
// 9, 10): clone_url с userinfo, url документа с токеном в строке запроса,
// ссылка в строке запроса самого вызова. Хост и путь в записи видны, набор
// ключей params прежний, а ответы и данные домена — исходные ссылки.
func TestRESTAuditRedactsURLCredentials(t *testing.T) {
	f := newAuditFixture(t)
	const (
		cloneURL = "https://alice:ghp_RESTMARK1@github.com/acme/app.git"
		docURL   = "https://files.example.com/spec.pdf?token=RESTMARK2&v=2"
	)
	rec := f.do("POST", "/v1/projects/SDLC/repositories", f.root,
		map[string]any{"repository": "acme/app", "clone_url": cloneURL})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("репозиторий: %d %s", rec.Code, rec.Body)
	}
	var repo struct {
		CloneURL string `json:"clone_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &repo)
	if repo.CloneURL != cloneURL {
		t.Fatalf("ответ изменился: clone_url %q", repo.CloneURL)
	}
	rec = f.do("POST", "/v1/projects/SDLC/documents?from=https://bob:RESTMARK3@h.example.com/x", f.root,
		map[string]any{"kind": "guideline", "title": "Спека", "url": docURL})
	if rec.Code != http.StatusCreated {
		t.Fatalf("документ: %d %s", rec.Code, rec.Body)
	}
	var doc struct {
		Document struct {
			URL string `json:"url"`
		} `json:"document"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc.Document.URL != docURL {
		t.Fatalf("ответ изменился: url %q", doc.Document.URL)
	}
	var storedClone, storedURL string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT r.clone_url, d.url FROM repositories r, project_documents d
		WHERE r.name = 'app' AND d.title = 'Спека'`).Scan(&storedClone, &storedURL); err != nil {
		t.Fatal(err)
	}
	if storedClone != cloneURL || storedURL != docURL {
		t.Fatalf("данные домена изменились: %q %q", storedClone, storedURL)
	}

	repoRows := f.rows("a.action = 'POST /v1/projects/:projectKey/repositories'")
	docRows := f.rows("a.action = 'POST /v1/projects/:projectKey/documents'")
	if len(repoRows) != 1 || len(docRows) != 1 {
		t.Fatalf("записей: репозиторий %d, документ %d", len(repoRows), len(docRows))
	}
	for _, r := range f.rows("true") {
		for _, marker := range []string{"ghp_RESTMARK1", "alice", "RESTMARK2", "RESTMARK3", "bob:"} {
			if strings.Contains(r.Params, marker) {
				t.Fatalf("%s: в параметрах %q: %s", r.Action, marker, r.Params)
			}
		}
	}
	if !strings.Contains(repoRows[0].Params, "https://[redacted]@github.com/acme/app.git") {
		t.Fatalf("хост и путь репозитория: %s", repoRows[0].Params)
	}
	if !strings.Contains(docRows[0].Params, "files.example.com/spec.pdf?token=[redacted]&v=2") ||
		!strings.Contains(docRows[0].Params, "https://[redacted]@h.example.com/x") {
		t.Fatalf("хост и путь документа: %s", docRows[0].Params)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(docRows[0].Params), &params); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"path", "query", "body"} {
		if _, ok := params[k]; !ok {
			t.Fatalf("в params нет %q: %s", k, docRows[0].Params)
		}
	}
}

// Отказы видны (критерии 8, 9): 403 по уровню — с принципалом и токеном,
// 401 по отозванному токену — со ссылкой на него и проектом из пути, и на
// /v1, и на /mcp; запрос вовсе без токена не пишется.
func TestRESTDenialsAreAudited(t *testing.T) {
	f := newAuditFixture(t)

	rec := f.do("POST", "/v1/projects/SDLC/roles/analyst/versions", f.bot, map[string]any{"instructions": "x"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("агент завёл версию роли: %d", rec.Code)
	}
	forbidden := f.rows("a.outcome = 'forbidden'")
	if len(forbidden) != 1 || s(forbidden[0].Principal) != "bot" || s(forbidden[0].TokenName) != "bot" ||
		forbidden[0].TokenID == nil || *forbidden[0].TokenID != f.botTok || s(forbidden[0].Project) != "SDLC" ||
		!strings.Contains(forbidden[0].Params, `"instructions": "x"`) {
		t.Fatalf("отказ 403: %+v", forbidden)
	}

	// 403 на чтении тоже пишется: это попытка выйти за рамки.
	if rec := f.do("GET", "/v1/projects/SDLC/audit", f.bot, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("агент прочёл журнал: %d", rec.Code)
	}
	if len(f.rows("a.action = 'GET /v1/projects/:projectKey/audit' AND a.outcome = 'forbidden'")) != 1 {
		t.Fatal("отказ на чтении не записан")
	}

	if err := f.auth.RevokeToken(context.Background(), f.admin, f.botTok); err != nil {
		t.Fatal(err)
	}
	if rec := f.do("POST", "/v1/tasks/SDLC-1/claim", f.bot, map[string]any{}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("отозванный токен: %d", rec.Code)
	}
	req := httptest.NewRequest("POST", "/mcp/projects/SDLC", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+f.bot)
	req.Header.Set("Content-Type", "application/json")
	mrec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(mrec, req)
	if mrec.Code != http.StatusUnauthorized {
		t.Fatalf("MCP с отозванным токеном: %d", mrec.Code)
	}
	unauth := f.rows("a.outcome = 'unauthenticated'")
	if len(unauth) != 2 {
		t.Fatalf("отказы 401: %+v", unauth)
	}
	for _, r := range unauth {
		if r.TokenID == nil || *r.TokenID != f.botTok || s(r.Principal) != "bot" || s(r.Project) != "SDLC" {
			t.Fatalf("401 без ссылки на токен или проекта: %+v", r)
		}
	}
	if unauth[0].Channel != "rest" || s(unauth[0].Task) != "SDLC-1" || unauth[1].Channel != "mcp" || unauth[1].Action != "mcp" {
		t.Fatalf("каналы отказов: %+v", unauth)
	}

	// Без токена и с неизвестным токеном: сканер не засоряет журнал, а
	// неизвестный токен пишется без ссылки.
	before := f.count()
	if rec := f.do("POST", "/v1/tasks/SDLC-1/claim", "", map[string]any{}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", rec.Code)
	}
	if f.count() != before {
		t.Fatal("запрос без токена записан")
	}
	f.do("POST", "/v1/tasks/SDLC-1/claim", "clab_unknown_token_xyz", map[string]any{})
	unknown := f.rows("a.outcome = 'unauthenticated' AND a.token_id IS NULL")
	if len(unknown) != 1 || unknown[0].Principal != nil || strings.Contains(unknown[0].Params, "clab_unknown") {
		t.Fatalf("неизвестный токен: %+v", unknown)
	}
}

// Чтение журнала (критерии 15, 16, 18): maintainer видит задачу и проект,
// администратор — инстанс, новые сверху, курсор, фильтры; чтения записей
// не дают. Вид токена — из agent_tokens (критерий 10).
func TestAuditReadEndpoints(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	// Сессия интерфейса: вид session.
	const session = "session-token-0123456789abcdef"
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO agent_tokens (principal_id, name, token_hash, token_prefix, kind, expires_at)
		SELECT id, 'ui', $1, 'session-tok', 'session', now() + interval '1 hour'
		FROM principals WHERE handle = 'root'`, workflow.HashToken(session)); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		f.do("PATCH", "/v1/tasks/SDLC-3/scope", f.root, map[string]any{"note": "n" + string(rune('0'+i))})
	}
	f.do("PATCH", "/v1/tasks/SDLC-3/scope", session, map[string]any{"note": "из интерфейса"})
	f.do("POST", "/v1/teams", f.root, map[string]any{"key": "acme", "name": "Acme"})

	type page struct {
		Entries []struct {
			ID        string  `json:"id"`
			At        string  `json:"at"`
			TaskKey   *string `json:"task_key"`
			Principal *struct {
				Handle string `json:"handle"`
				Kind   string `json:"kind"`
			} `json:"principal"`
			Token *struct {
				ID     *string `json:"id"`
				Name   *string `json:"name"`
				Prefix *string `json:"prefix"`
				Kind   *string `json:"kind"`
			} `json:"token"`
			Channel string          `json:"channel"`
			Action  string          `json:"action"`
			Params  json.RawMessage `json:"params"`
			Outcome struct {
				OK     bool    `json:"ok"`
				Code   *string `json:"code"`
				Status *int    `json:"status"`
			} `json:"outcome"`
			RequestID *string `json:"request_id"`
		} `json:"entries"`
		Next *string `json:"next"`
	}
	get := func(path, token string, want int) page {
		t.Helper()
		rec := f.do("GET", path, token, nil)
		if rec.Code != want {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		var p page
		if want == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}

	before := f.count()
	task := get("/v1/tasks/SDLC-3/audit", f.root, http.StatusOK)
	if len(task.Entries) != 4 || task.Next != nil {
		t.Fatalf("журнал задачи: %d записей", len(task.Entries))
	}
	newest := task.Entries[0]
	if newest.Token == nil || s(newest.Token.Kind) != "session" || s(newest.Token.Name) != "ui" ||
		newest.Principal == nil || newest.Principal.Handle != "root" || newest.Principal.Kind != "user" ||
		s(newest.TaskKey) != "SDLC-3" || newest.Channel != "rest" || newest.Action != "PATCH /v1/tasks/:taskKey/scope" ||
		newest.Outcome.Status == nil || newest.RequestID == nil || newest.ID == "" || newest.At == "" {
		t.Fatalf("форма записи: %+v", newest)
	}
	if s(task.Entries[1].Token.Kind) != "api" {
		t.Fatalf("личный токен: %+v", task.Entries[1].Token)
	}

	// Курсор: две страницы по два.
	first := get("/v1/tasks/SDLC-3/audit?limit=2", f.root, http.StatusOK)
	if len(first.Entries) != 2 || first.Next == nil {
		t.Fatalf("первая страница: %+v", first)
	}
	second := get("/v1/tasks/SDLC-3/audit?limit=2&cursor="+*first.Next, f.root, http.StatusOK)
	if len(second.Entries) != 2 || second.Entries[0].ID == first.Entries[1].ID || second.Next != nil {
		t.Fatalf("вторая страница: %+v", second)
	}

	// Проект с фильтрами.
	proj := get("/v1/projects/SDLC/audit?channel=rest&task=SDLC-3&principal=user:root&outcome=ok", f.root, http.StatusOK)
	if len(proj.Entries) == 0 {
		t.Fatal("фильтры проекта ничего не нашли")
	}
	if p := get("/v1/projects/SDLC/audit?channel=mcp", f.root, http.StatusOK); len(p.Entries) != 0 {
		t.Fatalf("фильтр канала: %d", len(p.Entries))
	}
	if p := get("/v1/projects/SDLC/audit?token=session-tok", f.root, http.StatusOK); len(p.Entries) != 1 {
		t.Fatalf("фильтр токена по префиксу: %d", len(p.Entries))
	}
	if p := get("/v1/projects/SDLC/audit?since=2000-01-01&until=2000-01-02", f.root, http.StatusOK); len(p.Entries) != 0 {
		t.Fatalf("фильтр периода: %d", len(p.Entries))
	}
	get("/v1/projects/SDLC/audit?channel=ftp", f.root, http.StatusBadRequest)

	// Инстанс: записи без проекта.
	inst := get("/v1/audit", f.root, http.StatusOK)
	if len(inst.Entries) != 1 || inst.Entries[0].Action != "POST /v1/teams" {
		t.Fatalf("журнал инстанса: %+v", inst.Entries)
	}
	if f.count() != before {
		t.Fatal("чтение журнала записано")
	}

	// Права: агент и наблюдатель — 403; инстанс — только администратор.
	get("/v1/tasks/SDLC-3/audit", f.bot, http.StatusForbidden)
	get("/v1/audit", f.bot, http.StatusForbidden)
	if _, err := f.srv.store.ResolveActor(ctx, f.srv.store.DB(), "mia", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.SetMember(ctx, f.admin, "SDLC", "user:mia", domain.MemberMaintainer, nil); err != nil {
		t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{Name: "mia", Principal: "user:mia", ProjectKey: "SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	get("/v1/projects/SDLC/audit", issued.Token, http.StatusOK)
	get("/v1/audit", issued.Token, http.StatusForbidden)
}

// Удаление проекта уносит его журнал; след удаления остаётся в журнале
// инстанса и в team_events (критерий 21).
func TestProjectDeletionDropsAudit(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	if rec := f.do("POST", "/v1/projects", f.root, map[string]any{"key": "ACME", "name": "Acme"}); rec.Code != http.StatusCreated {
		t.Fatalf("проект: %d %s", rec.Code, rec.Body)
	}
	var projectID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM projects WHERE key = 'ACME'`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	f.do("POST", "/v1/projects/ACME/documents", f.root, map[string]any{"kind": "guideline", "title": "Д", "content": "текст"})
	if n := len(f.rows("a.project_id = $1", projectID)); n < 1 {
		t.Fatalf("журнал проекта: %d", n)
	}
	if rec := f.do("DELETE", "/v1/projects/ACME", f.root, nil); rec.Code >= 300 {
		t.Fatalf("удаление: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.rows("a.project_id = $1", projectID)); n != 0 {
		t.Fatalf("журнал удалённого проекта остался: %d", n)
	}
	gone := f.rows("a.action = 'DELETE /v1/projects/:projectKey'")
	if len(gone) != 1 || gone[0].Project != nil || !strings.Contains(gone[0].Params, "ACME") {
		t.Fatalf("след удаления: %+v", gone)
	}
	var events int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM team_events WHERE type = 'project.deleted'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("team_events.project.deleted: %d", events)
	}
}

// Режим header: токена нет — поля токена пустые, принципал из заголовка
// (критерий 10).
func TestHeaderModeAudit(t *testing.T) {
	store, pool := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthHeader)
	srv := NewServer(config.Config{Env: "test", DefaultActor: "anonymous"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)

	req := httptest.NewRequest("PATCH", "/v1/tasks/SDLC-3/scope", strings.NewReader(`{"note":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ActorHeader, "agent:somebody")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	f := &auditFixture{t: t, pool: pool}
	got := f.rows("true")
	if len(got) != 1 || s(got[0].Principal) != "somebody" || got[0].TokenID != nil || got[0].TokenName != nil || got[0].TokenKind != nil {
		t.Fatalf("режим header: %+v", got)
	}
}

// Вызов тула через HTTP-транспорт MCP даёт одну запись — тула, без записи
// REST поверх неё, и с request_id запроса (критерий 5). Вид токена — текст
// без CHECK: вид токена задачи (CLAB-212) ложится в журнал как есть
// (критерий 10).
func TestMCPOverHTTPAndTokenKind(t *testing.T) {
	f := newAuditFixture(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sdlc_whoami","arguments":{}}}`
	req := httptest.NewRequest("POST", "/mcp/projects/SDLC", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.root)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Request-Id", "req-mcp-1")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "access_level") {
		t.Fatalf("tools/call: %d %s", rec.Code, rec.Body)
	}
	got := f.rows("true")
	if len(got) != 1 || got[0].Channel != "mcp" || got[0].Action != "sdlc_whoami" || s(got[0].RequestID) != "req-mcp-1" ||
		s(got[0].Project) != "SDLC" || got[0].Status != nil {
		t.Fatalf("вызов тула по HTTP: %+v", got)
	}

	f.srv.audit.Record(context.Background(), workflow.AuditEntry{
		Target: postgres.AuditTarget{ProjectKey: "SDLC", TaskKey: "SDLC-1"},
		Identity: workflow.Identity{Principal: f.admin.Principal, Token: &domain.AccessToken{
			ID: f.admin.Token.ID, Name: "task-token", Prefix: "clab_task_ab", TokenKind: "task"}},
		Channel: workflow.AuditMCP, Action: "sdlc_submit_artifact", Outcome: "ok",
	})
	task := f.rows("a.token_kind = 'task'")
	if len(task) != 1 || s(task[0].Task) != "SDLC-1" || s(task[0].TokenName) != "task-token" {
		t.Fatalf("вид токена задачи: %+v", task)
	}
}

// Отказ белого списка токена задачи (CLAB-208) пишется в журнал (CLAB-210)
// по обоим каналам: слой executorOnly стоит после auditREST, а обёртка add
// пишет вызов и тогда, когда executorMay его отбил.
func TestExecutorDenialsAreAudited(t *testing.T) {
	f := newAuditFixture(t)
	for _, path := range []string{"/v1/tasks/SDLC-1/transitions/start", "/v1/tasks/SDLC-1/claim"} {
		if rec := f.do("POST", path, f.root, map[string]any{}); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	rec := f.do("POST", "/v1/tasks/SDLC-1/task-token", f.root, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %s", rec.Code, rec.Body)
	}
	var issued struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}

	if rec := f.do("PATCH", "/v1/tasks/SDLC-1/scope", issued.Token,
		map[string]any{"properties": map[string]any{"multi_repo": true}}); rec.Code != http.StatusForbidden {
		t.Fatalf("область токеном задачи: %d %s", rec.Code, rec.Body)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sdlc_put_role","arguments":{"key":"analyst","instructions":"x"}}}`
	req := httptest.NewRequest("POST", "/mcp/projects/SDLC", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+issued.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	mrec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(mrec, req)
	if !strings.Contains(mrec.Body.String(), "действие человека, не исполнителя этапа") {
		t.Fatalf("тул токеном задачи: %d %s", mrec.Code, mrec.Body)
	}

	got := f.rows("a.token_kind = 'task' AND a.outcome = 'forbidden'")
	if len(got) != 2 ||
		got[0].Channel != "rest" || got[0].Action != "PATCH /v1/tasks/:taskKey/scope" || s(got[0].Task) != "SDLC-1" ||
		got[0].Status == nil || *got[0].Status != http.StatusForbidden ||
		got[1].Channel != "mcp" || got[1].Action != "sdlc_put_role" {
		t.Fatalf("отказы токена задачи в журнале: %+v", got)
	}
}
