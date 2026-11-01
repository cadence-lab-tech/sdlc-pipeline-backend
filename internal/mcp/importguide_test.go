package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/importguide"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/trust"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Справка импорта про роль документами (CLAB-345, AC-11, AC-12): один текст
// у тула, промпта и clab import-guide, формат 3, различие документов проекта
// и роли, каталог связки.
func TestImportGuideDescribesDocuments(t *testing.T) {
	if ImportGuide != importguide.Text {
		t.Fatal("mcp.ImportGuide — не importguide.Text")
	}
	cs := connectPrompts(t)
	if prompt := promptText(t, cs, "import_rules", nil); !strings.Contains(prompt, importguide.Text) {
		t.Fatal("промпт import_rules не несёт справку целиком")
	}
	for _, want := range []string{
		trust.ImportRule, "похожие на попытку", "dry_run",
		`"version": 3`, "roles[].documents", `{"name", "kind", "content"}`,
		"строчные латинские буквы", "не длиннее 64 символов", "по умолчанию instructions",
		"пустой документ отклоняется", "один main.md",
		"документы проекта", "документы роли", "Правило выбора",
		"по темам", "clab export --dir", "clab import --dir", "--dry-run", "--force",
		"форматы связки (1 и 2)", "instructions и documents вместе не бывают",
	} {
		if !strings.Contains(importguide.Text, want) {
			t.Errorf("в справке нет %q", want)
		}
	}
	for _, stale := range []string{"roles[].instructions", `"version": 1`, "version: 1", "sdlc import", "sdlc export"} {
		if strings.Contains(importguide.Text, stale) {
			t.Errorf("в справке осталось %q", stale)
		}
	}
	if !strings.Contains(importguide.Text, importguide.Example) {
		t.Error("образец связки не входит в справку")
	}

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		switch tool.Name {
		case "sdlc_export_bundle", "sdlc_import_bundle":
			if !strings.Contains(tool.Description, "роли с документами") && !strings.Contains(tool.Description, "Роли — с документами") {
				t.Errorf("%s: описание не про документы ролей: %s", tool.Name, tool.Description)
			}
			if strings.Contains(tool.Description, "с инструкциями") || strings.Contains(tool.Description, "с другой инструкцией") {
				t.Errorf("%s: описание про инструкцию строкой: %s", tool.Name, tool.Description)
			}
		}
	}
}

// AC-12: образец связки из справки принимается импортом с dry_run (все
// исходы created), а тот же образец с instructions рядом с documents —
// отклоняется: справка не расходится с форматом.
//
// Через тул sdlc_import_bundle образец идёт без output_contract: схема
// аргументов тула отбивает поля json.RawMessage (CLAB-98, известный баг,
// не этой задачи). Целиком образец проверяет сервис, на котором стоит тул, —
// Authoring.Import, и clab import --file в TestBundleDirOnServer.
func TestImportGuideExampleAccepted(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	authoring := workflow.NewAuthoring(store, log, engine)
	srv := NewServer(log, store, engine, authoring, auth, "test")
	if err := auth.Bootstrap(ctx, "root", "bootstrap-token-clab-345-0123456789"); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, "bootstrap-token-clab-345-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	call := connectAs(t, srv, admin)

	var typed workflow.Bundle
	if err := json.Unmarshal([]byte(importguide.Example), &typed); err != nil {
		t.Fatalf("образец справки не разбирается связкой: %v", err)
	}
	report, err := authoring.Import(ctx, "SDLC", admin.Principal.ID, typed, true)
	if err != nil {
		t.Fatalf("образец отклонён: %v", err)
	}
	if len(report.Roles) != 1 || len(report.Documents) != 2 || len(report.Repositories) != 1 {
		t.Fatalf("отчёт: %+v", report)
	}
	for _, e := range append(append(report.Roles, report.Documents...), report.Repositories...) {
		if e.Outcome != "created" {
			t.Errorf("%s: исход %s, ждали created", e.Key, e.Outcome)
		}
	}
	var count int
	if err := store.DB().QueryRow(ctx, `SELECT count(*) FROM roles WHERE key = 'tech-writer'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry_run оставил роль: %d, %v", count, err)
	}

	var bundle map[string]any
	if err := json.Unmarshal([]byte(importguide.Example), &bundle); err != nil {
		t.Fatal(err)
	}
	role := bundle["roles"].([]any)[0].(map[string]any)
	delete(role, "output_contract")
	out, isErr := call("sdlc_import_bundle", map[string]any{"dry_run": true, "bundle": bundle})
	if isErr {
		t.Fatalf("тул отклонил образец: %v", out)
	}
	if roles, _ := out["roles"].([]any); len(roles) != 1 || roles[0].(map[string]any)["outcome"] != "created" {
		t.Fatalf("отчёт тула: %v", out)
	}

	role["instructions"] = "Ты техписатель."
	out, isErr = call("sdlc_import_bundle", map[string]any{"dry_run": true, "bundle": bundle})
	if !isErr || !strings.Contains(errorMessage(out), "не оба сразу") {
		t.Fatalf("роль с обоими полями принята: %v", out)
	}
}
