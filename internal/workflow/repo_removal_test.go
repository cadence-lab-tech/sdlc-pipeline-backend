package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Отвязка репозитория от проекта: удаление необратимо, поэтому проверяется и
// то, что уходит, и то, что остаётся, и то, при каких условиях отвязка не
// проходит вовсе.

// repoWithKnowledge привязывает репозиторий и набивает его знанием: прогон
// индексации, файл, чанк, документ из репозитория. Без этого счётчики
// удалённого и каскад проверять не на чем.
func repoWithKnowledge(t *testing.T, ctx context.Context, store *postgres.Store, engine *Engine, actor domain.Principal) domain.Repository {
	t.Helper()
	if _, err := engine.SetProjectGitHub(ctx, "SDLC", SetGitHubInput{InstallationID: ptr(int64(42))}); err != nil {
		t.Fatal(err)
	}
	repo, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/api", RoleHint: "api"})
	if err != nil {
		t.Fatal(err)
	}

	run, _, err := store.EnqueueIndexRun(ctx, store.DB(), postgres.EnqueueIndexRunInput{
		RepositoryID: repo.ID, Branch: "main", CommitSHA: strings.Repeat("a", 40), Trigger: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	var fileID uuid.UUID
	if err := store.DB().QueryRow(ctx, `
		INSERT INTO code_files (index_run_id, repository_id, path, lang, size_bytes)
		VALUES ($1, $2, 'main.go', 'go', 10) RETURNING id`, run.ID, repo.ID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO code_chunks (code_file_id, project_id, ord, start_line, end_line, text)
		VALUES ($1, $2, 0, 1, 10, 'package main')`, fileID, repo.ProjectID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := engine.PutDocument(ctx, "SDLC", actor.ID, PutDocumentInput{
		Kind: "architecture", Title: "Архитектура API", Repository: "acme/api", RepoPath: "docs/ARCHITECTURE.md",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.PutDocument(ctx, "SDLC", actor.ID, PutDocumentInput{
		Kind: "guideline", Title: "Соглашения", Content: []byte("# Правила"),
	}); err != nil {
		t.Fatal(err)
	}
	return repo
}

// Репозиторий под живой задачей не отвязывается: иначе у работающего агента
// исчезнет из-под ног и код, и базовая точка.
func TestRemoveRepositoryRefusedWhileTaskIsLive(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	repo := repoWithKnowledge(t, ctx, store, engine, system)

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Живая задача", Repositories: []string{"acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = engine.RemoveRepository(ctx, "SDLC", "acme/api", system.ID)
	if apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("отвязка под живой задачей прошла: %v", err)
	}
	if !strings.Contains(err.Error(), task.Key) {
		t.Fatalf("отказ не называет задачу: %v", err)
	}
	if _, err := store.FindRepository(ctx, store.DB(), repo.ProjectID, "acme/api"); err != nil {
		t.Fatalf("отказ всё-таки что-то удалил: %v", err)
	}

	// Терминальность берётся из цикла: задача, дошедшая до терминального
	// статуса, отвязке больше не мешает.
	if _, err := engine.Execute(ctx, task.Key, "cancel", system.ID, "не нужна"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.RemoveRepository(ctx, "SDLC", "acme/api", system.ID); err != nil {
		t.Fatalf("отвязка после закрытия задачи: %v", err)
	}
}

// Отвязка уносит знание репозитория и документы из него, не трогает ни блобы,
// ни чужие документы, закрывает висящие сигналы дрейфа и возвращает счётчики.
func TestRemoveRepositoryTakesKnowledgeWithIt(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	repo := repoWithKnowledge(t, ctx, store, engine, system)

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Закрытая задача", Repositories: []string{"acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, task.Key, "cancel", system.ID, "сделана"); err != nil {
		t.Fatal(err)
	}
	// Сигнал дрейфа переживает каскад: его repository_id обнуляется, и
	// разобрать сигнал было бы нечем.
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO task_drift_signals (task_id, repository_id, kind, severity, state, to_commit)
		VALUES ($1, $2, 'baseline_drift', 'low', 'open', $3)`,
		task.ID, repo.ID, strings.Repeat("c", 40)); err != nil {
		t.Fatal(err)
	}

	blobsBefore := countRows(t, ctx, store, `SELECT count(*) FROM blobs`)

	removed, counters, err := engine.RemoveRepository(ctx, "SDLC", "acme/api", system.ID)
	if err != nil {
		t.Fatalf("отвязка: %v", err)
	}
	if removed.ID != repo.ID {
		t.Fatalf("отвязан не тот репозиторий: %s", removed.ID)
	}
	if counters.IndexRuns != 1 || counters.CodeFiles != 1 || counters.CodeChunks != 1 ||
		counters.Documents != 1 || counters.Drift != 1 {
		t.Fatalf("счётчики удалённого: %+v", counters)
	}

	for _, q := range []string{
		fmt.Sprintf(`SELECT count(*) FROM repositories WHERE id = '%s'`, repo.ID),
		fmt.Sprintf(`SELECT count(*) FROM repo_index_runs WHERE repository_id = '%s'`, repo.ID),
		fmt.Sprintf(`SELECT count(*) FROM code_files WHERE repository_id = '%s'`, repo.ID),
		fmt.Sprintf(`SELECT count(*) FROM task_repositories WHERE repository_id = '%s'`, repo.ID),
		`SELECT count(*) FROM code_chunks ch LEFT JOIN code_files f ON f.id = ch.code_file_id WHERE f.id IS NULL`,
		`SELECT count(*) FROM project_documents WHERE source = 'repo_path' AND repository_id IS NULL`,
	} {
		if n := countRows(t, ctx, store, q); n != 0 {
			t.Fatalf("после отвязки осталось %d строк: %s", n, q)
		}
	}

	// Документы: свой уходит, чужой остаётся.
	docs, err := engine.ListDocuments(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.Title == "Архитектура API" {
			t.Fatal("документ из отвязанного репозитория остался")
		}
	}
	if countRows(t, ctx, store, `SELECT count(*) FROM project_documents WHERE title = 'Соглашения'`) != 1 {
		t.Fatal("отвязка задела документ, не связанный с репозиторием")
	}

	// Блобы общие по sha256: их чистит сборка мусора, а не отвязка.
	if after := countRows(t, ctx, store, `SELECT count(*) FROM blobs`); after != blobsBefore {
		t.Fatalf("блобов было %d, стало %d", blobsBefore, after)
	}

	// Сигнал дрейфа закрыт, и закрыт от имени того, кто отвязал.
	var state, resolution string
	var resolvedBy *uuid.UUID
	if err := store.DB().QueryRow(ctx, `
		SELECT state, coalesce(resolution, ''), resolved_by FROM task_drift_signals WHERE task_id = $1`,
		task.ID).Scan(&state, &resolution, &resolvedBy); err != nil {
		t.Fatal(err)
	}
	if state != "dismissed" || resolution == "" || resolvedBy == nil || *resolvedBy != system.ID {
		t.Fatalf("сигнал дрейфа: %s %q %v", state, resolution, resolvedBy)
	}

	// Повторная отвязка — уже нечего отвязывать.
	if _, _, err := engine.RemoveRepository(ctx, "SDLC", "acme/api", system.ID); apperr.As(err).Code != apperr.CodeUnknownRepository {
		t.Fatalf("повторная отвязка: %v", err)
	}

	// Вебхук по отвязанному репозиторию не заводит прогон индексации.
	push := fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"deleted":false,
		"repository":{"name":"api","owner":{"login":"acme"}},"installation":{"id":42}}`,
		strings.Repeat("d", 40))
	out, err := engine.HandleGitHubEvent(ctx, "push", "d-detached", []byte(push))
	if err != nil {
		t.Fatal(err)
	}
	if out.Queued != 0 || out.Ignored == "" {
		t.Fatalf("событие по отвязанному репозиторию обслужено: %+v", out)
	}

	// Повторная привязка начинается с чистого листа: новая строка, новый
	// индекс, удалённые документы не возвращаются.
	again, created, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/api"})
	if err != nil {
		t.Fatalf("повторная привязка: %v", err)
	}
	if !created || again.ID == repo.ID {
		t.Fatalf("повторная привязка вернула прежнюю строку: created=%v id=%s", created, again.ID)
	}
	if n := countRows(t, ctx, store, fmt.Sprintf(`SELECT count(*) FROM code_files WHERE repository_id = '%s'`, again.ID)); n != 0 {
		t.Fatalf("у заново привязанного репозитория нашлось %d файлов", n)
	}
}

func countRows(t *testing.T, ctx context.Context, store *postgres.Store, query string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
