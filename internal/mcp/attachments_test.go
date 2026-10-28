package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// CLAB-296, AC-40: sdlc_get_artifact не отдаёт байты файла — ни вложения,
// ни файла по контракту, — а отвечает путём HTTP; текстовый артефакт этапа
// отдаётся как раньше. AC-15: sdlc_get_task несёт список вложений.
func TestGetArtifactRefusesFiles(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
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

	secret := "секретные байты файла"
	file, err := store.SpoolBlob(strings.NewReader(secret))
	if err != nil {
		t.Fatal(err)
	}
	att, err := engine.AttachFile(ctx, "SDLC-1", admin.Principal.ID, workflow.AttachInput{
		FileName: "notes.txt", MediaType: "text/plain", File: file})
	if err != nil {
		t.Fatal(err)
	}
	var binary, text domain.Artifact
	if err := store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := store.GetTaskByKey(ctx, tx, "SDLC-1")
		if err != nil {
			return err
		}
		if binary, err = store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task: task, Kind: "analysis.brief", Title: "файл", Format: "binary",
			Content: []byte(secret + " по контракту"), MediaType: "image/png", ProducedBy: admin.Principal.ID,
		}); err != nil {
			return err
		}
		text, err = store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task: task, Kind: "analysis.acceptance_criteria", Title: "критерии",
			Content: []byte("# критерии"), ProducedBy: admin.Principal.ID,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for _, a := range []domain.Artifact{att, binary} {
		out, isErr := call("sdlc_get_artifact", map[string]any{"artifact_id": a.ID.String()})
		msg := errorMessage(out)
		if !isErr || !strings.Contains(msg, workflow.ContentPath(a.ID)) {
			t.Fatalf("%s: ждали отказ с путём, получили %v", a.Title, out)
		}
		if raw := mustJSON(t, out); bytes.Contains(raw, []byte("секретные")) {
			t.Fatalf("%s: байты файла в ответе тула: %s", a.Title, raw)
		}
	}
	out, isErr := call("sdlc_get_artifact", map[string]any{"artifact_id": text.ID.String()})
	if isErr || out["content"] != "# критерии" {
		t.Fatalf("текстовый артефакт: %v", out)
	}

	out, isErr = call("sdlc_get_task", map[string]any{"task_key": "SDLC-1"})
	if isErr {
		t.Fatalf("sdlc_get_task: %v", out)
	}
	ctxOut, _ := out["context"].(map[string]any)
	list, _ := ctxOut["attachments"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["content_path"] != workflow.ContentPath(att.ID) {
		t.Fatalf("вложения в sdlc_get_task: %v", ctxOut["attachments"])
	}
	for _, p := range ctxOut["prior_artifacts"].([]any) {
		if p.(map[string]any)["id"] == att.ID.String() {
			t.Fatal("вложение в prior_artifacts")
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
