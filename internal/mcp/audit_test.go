package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// auditRow — запись журнала, как её видит тест.
type auditRow struct {
	Project, Task, Principal, TokenName, TokenPrefix, TokenKind *string
	TokenID                                                     *uuid.UUID
	Channel, Action, Outcome                                    string
	Status                                                      *int
	Params                                                      json.RawMessage
}

func auditRows(t *testing.T, pool *pgxpool.Pool, where string, args ...any) []auditRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT pr.key, tk.key, p.handle, a.token_id, a.token_name, a.token_prefix, a.token_kind,
		       a.channel, a.action, a.outcome, a.http_status, a.params
		FROM audit_log a
		LEFT JOIN projects pr ON pr.id = a.project_id
		LEFT JOIN tasks tk ON tk.id = a.task_id
		LEFT JOIN principals p ON p.id = a.principal_id
		WHERE `+where+` ORDER BY a.created_at, a.id`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Project, &r.Task, &r.Principal, &r.TokenID, &r.TokenName, &r.TokenPrefix,
			&r.TokenKind, &r.Channel, &r.Action, &r.Outcome, &r.Status, &r.Params); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// argsFor собирает аргументы, которые проходят схему тула: обязательные поля
// заполнены значениями своего типа. Тулу этого хватает, чтобы дойти до
// обработчика, — что он ответит, журналу всё равно.
func argsFor(schema map[string]any) map[string]any {
	args := map[string]any{}
	props, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)
	for _, r := range required {
		name, _ := r.(string)
		prop, _ := props[name].(map[string]any)
		args[name] = valueFor(name, prop)
	}
	return args
}

func valueFor(name string, prop map[string]any) any {
	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	typ, _ := prop["type"].(string)
	if typ == "" {
		// Срез и указатель схема описывает как ["null", "array"]: берётся
		// не-null тип.
		types, _ := prop["type"].([]any)
		for _, x := range types {
			if s, _ := x.(string); s != "null" {
				typ = s
			}
		}
	}
	switch typ {
	case "integer", "number":
		return 1
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return argsFor(prop)
	}
	switch {
	case name == "task_key" || name == "parent":
		return "SDLC-1"
	case strings.HasSuffix(name, "_id"):
		return uuid.NewString()
	}
	return "x"
}

// Каждый тул пишет в журнал ровно одну запись (CLAB-210, критерии 2, 3, 6,
// 10): канал mcp, действие — имя тула, проект подключения, принципал, токен
// со снимком и видом, параметры, исход. Перебираются все зарегистрированные
// тулы — новый тул без записи валит тест. heartbeat записи не даёт.
func TestEveryToolCallIsAudited(t *testing.T) {
	ctx := context.Background()
	store, pool := testdb.Store(t)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(log, store, engine, workflow.NewAuthoring(store, log, engine), auth, "test")

	const rootToken = "bootstrap-token-0123456789"
	if err := auth.Bootstrap(ctx, "root", rootToken); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, rootToken)
	if err != nil {
		t.Fatal(err)
	}

	cs := clientSession(t, srv, admin)
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) < 20 {
		t.Fatalf("тулов %d — список не тот", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		var schema map[string]any
		raw, _ := json.Marshal(tool.InputSchema)
		_ = json.Unmarshal(raw, &schema)
		args := argsFor(schema)

		if _, err := pool.Exec(ctx, `DELETE FROM audit_log`); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool.Name, Arguments: args}); err != nil {
			t.Fatalf("%s %v: протокольная ошибка %v", tool.Name, args, err)
		}
		rows := auditRows(t, pool, "true")
		if tool.Name == heartbeatTool {
			if len(rows) != 0 {
				t.Fatalf("heartbeat записан: %+v", rows)
			}
			continue
		}
		if len(rows) != 1 {
			t.Fatalf("%s: записей %d, нужна одна", tool.Name, len(rows))
		}
		r := rows[0]
		switch {
		case r.Channel != "mcp" || r.Action != tool.Name:
			t.Fatalf("%s: канал %s, действие %s", tool.Name, r.Channel, r.Action)
		case str(r.Project) != "SDLC":
			t.Fatalf("%s: проект %q", tool.Name, str(r.Project))
		case str(r.Principal) != "root":
			t.Fatalf("%s: принципал %q", tool.Name, str(r.Principal))
		case r.TokenID == nil || *r.TokenID != admin.Token.ID || str(r.TokenPrefix) != admin.Token.Prefix ||
			str(r.TokenName) != admin.Token.Name || str(r.TokenKind) != "api":
			t.Fatalf("%s: токен %v %q %q %q", tool.Name, r.TokenID, str(r.TokenName), str(r.TokenPrefix), str(r.TokenKind))
		case r.Outcome == "":
			t.Fatalf("%s: нет исхода", tool.Name)
		case r.Status != nil:
			t.Fatalf("%s: у вызова тула HTTP-статус %d", tool.Name, *r.Status)
		}
		if _, ok := args["task_key"]; ok && str(r.Task) != "SDLC-1" {
			t.Fatalf("%s: задача %q из task_key не определилась", tool.Name, str(r.Task))
		}
	}
}

// clientSession — клиент SDK к серверу от имени id в проекте SDLC.
func clientSession(t *testing.T, srv *Server, id workflow.Identity) *sdk.ClientSession {
	t.Helper()
	ctx := WithIdentity(WithProject(context.Background(), "SDLC"), id)
	serverT, clientT := sdk.NewInMemoryTransports()
	ss, err := srv.mcp.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// Тело артефакта — размер и sha256, совпадающий с blobs (критерий 12);
// отказ по уровню — запись forbidden с принципалом и токеном (критерий 8);
// heartbeat при живой аренде не пишется (критерий 3); сбой записи журнала
// не меняет ответ тула (критерий 11).
func TestToolAuditDetails(t *testing.T) {
	ctx := context.Background()
	store, pool := testdb.Store(t)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(log, store, engine, workflow.NewAuthoring(store, log, engine), auth, "test")

	if err := auth.Bootstrap(ctx, "root", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
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
	bot, err := auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	asAdmin, asBot := connectAs(t, srv, admin), connectAs(t, srv, bot)

	// Отказ по уровню: агент правит роль.
	if out, isErr := asBot("sdlc_put_role", map[string]any{"key": "analyst", "name": "A", "instructions": "x"}); !isErr {
		t.Fatalf("агент поправил роль: %v", out)
	}
	denied := auditRows(t, pool, "a.action = 'sdlc_put_role'")
	if len(denied) != 1 || denied[0].Outcome != "forbidden" || str(denied[0].Principal) != "bot" ||
		str(denied[0].TokenName) != "bot" || str(denied[0].Project) != "SDLC" {
		t.Fatalf("отказ по уровню: %+v", denied)
	}

	// Задача, захват, сдача артефакта.
	created, isErr := asAdmin("sdlc_create_task", map[string]any{"title": "Журнал"})
	if isErr {
		t.Fatalf("sdlc_create_task: %v", created)
	}
	key, _ := created["key"].(string)
	if out, isErr := asAdmin("sdlc_request_transition", map[string]any{"task_key": key, "transition": "start"}); isErr {
		t.Fatalf("start: %v", out)
	}
	claimed, isErr := asAdmin("sdlc_claim_task", map[string]any{"task_key": key})
	if isErr {
		t.Fatalf("sdlc_claim_task: %v", claimed)
	}
	kind := firstContractKind(t, claimed)
	const content = "# Разбор\n\nсодержимое артефакта для журнала"
	if out, isErr := asAdmin("sdlc_submit_artifact", map[string]any{
		"task_key": key, "kind": kind, "title": "Т", "content": content,
	}); isErr {
		t.Fatalf("sdlc_submit_artifact: %v", out)
	}
	var blobSHA string
	if err := pool.QueryRow(ctx, `
		SELECT a.blob_sha FROM artifacts a JOIN tasks t ON t.id = a.task_id WHERE t.key = $1`, key).Scan(&blobSHA); err != nil {
		t.Fatal(err)
	}
	submitted := auditRows(t, pool, "a.action = 'sdlc_submit_artifact'")
	if len(submitted) != 1 || str(submitted[0].Task) != key || submitted[0].Outcome != "ok" {
		t.Fatalf("сдача: %+v", submitted)
	}
	var params struct {
		Content struct {
			Size   int    `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"content"`
	}
	if err := json.Unmarshal(submitted[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	if strings.Contains(string(submitted[0].Params), "содержимое") || params.Content.Size != len(content) ||
		params.Content.SHA256 != hex.EncodeToString(sum[:]) || params.Content.SHA256 != strings.TrimSpace(blobSHA) {
		t.Fatalf("content в параметрах: %s, blob %s", submitted[0].Params, blobSHA)
	}

	// heartbeat живой аренды не пишется.
	if out, isErr := asAdmin("sdlc_heartbeat", map[string]any{"task_key": key}); isErr {
		t.Fatalf("heartbeat: %v", out)
	}
	if rows := auditRows(t, pool, "a.action = 'sdlc_heartbeat'"); len(rows) != 0 {
		t.Fatalf("heartbeat записан: %+v", rows)
	}

	// Сбой хранилища журнала: ответ тула тот же, записи нет.
	before, _ := asAdmin("sdlc_whoami", map[string]any{})
	srv.Audit().FailingSinkForTest(errors.New("журнал недоступен"))
	after, isErr := asAdmin("sdlc_whoami", map[string]any{})
	if isErr {
		t.Fatalf("сбой журнала уронил тул: %v", after)
	}
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	if string(b1) != string(b2) {
		t.Fatalf("ответ изменился при сбое журнала:\n%s\n%s", b1, b2)
	}
	if rows := auditRows(t, pool, "a.action = 'sdlc_whoami'"); len(rows) != 1 {
		t.Fatalf("whoami: записей %d — вторая не должна была лечь", len(rows))
	}
}

// Учётные данные внутри ссылок не доходят до audit_log и по MCP (CLAB-226,
// критерии 5, 9): ссылка с userinfo в repository, url документа с токеном,
// те же ссылки внутри связки. Отказ тула пишется с теми же очищенными
// параметрами, документ сохранён с исходной ссылкой.
func TestToolAuditRedactsURLCredentials(t *testing.T) {
	ctx := context.Background()
	store, pool := testdb.Store(t)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(log, store, engine, workflow.NewAuthoring(store, log, engine), auth, "test")
	if err := auth.Bootstrap(ctx, "root", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	call := connectAs(t, srv, admin)

	const (
		repoURL = "https://alice:ghp_MCPMARK1@github.com/acme/app.git"
		docURL  = "https://files.example.com/spec.pdf?access_token=MCPMARK2&v=2"
	)
	// Тул ждёт owner/name или ссылку без userinfo: ссылку с ним он отбивает,
	// и запись об отказе тоже не должна нести токен.
	_, _ = call("sdlc_add_repository", map[string]any{"repository": repoURL})
	if out, isErr := call("sdlc_put_document", map[string]any{"kind": "guideline", "title": "Спека", "url": docURL}); isErr {
		t.Fatalf("sdlc_put_document: %v", out)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT url FROM project_documents WHERE title = 'Спека'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != docURL {
		t.Fatalf("данные домена изменились: %q", stored)
	}
	_, _ = call("sdlc_import_bundle", map[string]any{"dry_run": true, "bundle": map[string]any{"version": 1,
		"repositories": []any{map[string]any{"repository": "acme/app", "clone_url": "https://bob:MCPMARK3@github.com/acme/app.git"}},
		"documents":    []any{map[string]any{"kind": "guideline", "title": "S3", "url": "https://b.s3.amazonaws.com/o?X-Amz-Credential=AKIAMCPMARK4&X-Amz-Signature=MCPMARK5"}},
	}})

	rows := auditRows(t, pool, "a.action IN ('sdlc_add_repository', 'sdlc_put_document', 'sdlc_import_bundle')")
	if len(rows) != 3 {
		t.Fatalf("записей %d, нужно три: %+v", len(rows), rows)
	}
	for _, r := range rows {
		text := string(r.Params)
		for _, marker := range []string{"ghp_MCPMARK1", "alice", "MCPMARK2", "MCPMARK3", "bob:", "AKIAMCPMARK4", "MCPMARK5"} {
			if strings.Contains(text, marker) {
				t.Fatalf("%s: в параметрах %q: %s", r.Action, marker, text)
			}
		}
		var want []string
		switch r.Action {
		case "sdlc_add_repository":
			want = []string{"https://[redacted]@github.com/acme/app.git"}
		case "sdlc_put_document":
			want = []string{"files.example.com/spec.pdf?access_token=[redacted]&v=2"}
		case "sdlc_import_bundle":
			want = []string{"https://[redacted]@github.com/acme/app.git", "o?X-Amz-Credential=[redacted]&X-Amz-Signature=[redacted]"}
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Fatalf("%s: нет %q: %s", r.Action, w, text)
			}
		}
	}
}

func firstContractKind(t *testing.T, claimed map[string]any) string {
	t.Helper()
	role, _ := claimed["role"].(map[string]any)
	contract, _ := role["output_contract"].([]any)
	for _, c := range contract {
		if item, ok := c.(map[string]any); ok {
			if kind, _ := item["kind"].(string); kind != "" {
				return kind
			}
		}
	}
	t.Fatalf("в ответе захвата нет контракта: %v", claimed)
	return ""
}
