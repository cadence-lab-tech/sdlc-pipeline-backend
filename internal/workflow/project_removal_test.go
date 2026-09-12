package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Удаление проекта необратимо и уносит всё сразу (CLAB-10). Проверяется, что
// уходит, что остаётся и что каскада схемы самого по себе не хватает.

// Таблицы, в которых после удаления проекта не должно остаться ни строки.
// Список перечислен целиком намеренно: новая таблица со ссылкой на проект,
// забытая в удалении, иначе всплывёт только на проде.
var projectOwnedTables = []string{
	"tasks", "stage_runs", "gate_results", "task_events", "task_repositories",
	"task_repo_refs", "task_drift_signals", "task_baselines", "task_links",
	"artifacts", "artifact_links", "artifact_chunks",
	"repositories", "repo_index_runs", "code_files", "code_chunks",
	"document_chunks", "project_documents", "project_members", "project_teams",
	"agent_tokens", "roles", "role_versions", "workflows", "workflow_statuses",
	"workflow_transitions", "gates", "invites", "stage_run_usage",
}

// Проект, набитый всем, что на него ссылается: задача с прогонами и цепочкой
// версий артефакта, отклонённый прогон, репозиторий с индексом, документы
// обоих видов, участник, токен проекта, приглашение, сигнал дрейфа, расход.
func projectWithEverything(t *testing.T, ctx context.Context, store *postgres.Store, engine *Engine, actor domain.Principal) {
	t.Helper()

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
	// Документ из репозитория: его источник обнуляется каскадом репозиториев и
	// нарушил бы project_documents_source_shape, пойди удаление в другом порядке.
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

	task, err := engine.CreateTask(ctx, "SDLC", actor.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Живая задача", Repositories: []string{"acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, task.Key, "start", actor.ID, "поехали"); err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.Claim(ctx, task.Key, actor.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Две версии одного артефакта: supersedes_id — самоссылка без каскада.
	first, err := engine.SubmitArtifact(ctx, task.Key, actor.ID, SubmitArtifactInput{
		Kind: "analysis.brief", Title: "Бриф", Content: []byte("# Бриф")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SubmitArtifact(ctx, task.Key, actor.ID, SubmitArtifactInput{
		Kind: "analysis.brief", Title: "Бриф v2", Content: []byte("# Бриф v2")}); err != nil {
		t.Fatal(err)
	}
	// Цепочка должна быть настоящей: без supersedes_id самоссылка не проверена.
	if n := countRows(t, ctx, store,
		fmt.Sprintf(`SELECT count(*) FROM artifacts WHERE supersedes_id = '%s'`, first.ID)); n != 1 {
		t.Fatalf("вторая версия артефакта не ссылается на первую: %d", n)
	}
	// Отклонённый прогон: rejected_from_run_id — тоже самоссылка без каскада.
	if _, err := store.DB().Exec(ctx, `
		UPDATE stage_runs SET rejected_from_run_id = id WHERE task_id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO task_drift_signals (task_id, repository_id, kind, severity, state, to_commit)
		VALUES ($1, $2, 'baseline_drift', 'low', 'open', $3)`,
		task.ID, repo.ID, strings.Repeat("c", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReportUsage(ctx, task.Key, actor.ID, ReportUsageInput{
		StageRunID: &claimed.StageRunID, Model: "claude-opus-5", CostUSD: ptr(1.5),
	}); err != nil {
		t.Fatal(err)
	}
}

// Каскад схемы сам по себе проекта не удаляет: задачи и прогоны ссылаются на
// цикл и роли того же проекта связями без каскада. Тест держит порядок
// удаления: без него ручка падает на stage_runs_role_id_fkey.
func TestPlainCascadeCannotDeleteProject(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	projectWithEverything(t, ctx, store, engine, system)

	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `DELETE FROM projects WHERE id = $1`, project.ID); err == nil {
		t.Fatal("прямое удаление проекта прошло: порядок в DeleteProject больше не нужен — проверить схему")
	}
}

// Удаление уносит проект целиком, пишет событие на команде-владельце и не
// трогает ни блобы, ни соседний проект.
func TestDeleteProjectTakesEverythingWithIt(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, system := setupPhase4(t, AuthHeader)
	projectWithEverything(t, ctx, store, engine, system)

	if _, err := auth.SetMember(ctx, Identity{Principal: system}, "SDLC", "agent:bot", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO agent_tokens (principal_id, project_id, name, token_hash)
		SELECT $1, id, 'ci', 'hash-clab-10' FROM projects WHERE key = 'SDLC'`, system.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO invites (project_id, level, token_hash, created_by, expires_at)
		SELECT id, 'viewer', 'inv-clab-10', $1, now() + interval '7 days'
		FROM projects WHERE key = 'SDLC'`, system.ID); err != nil {
		t.Fatal(err)
	}

	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	// Соседний проект той же команды: удаление обязано пройти мимо него.
	neighbour, err := engine.CreateProject(ctx, system.ID, postgres.CreateProjectInput{
		Key: "NEXT", Name: "Соседний", OwnerTeamID: *project.OwnerTeamID})
	if err != nil {
		t.Fatal(err)
	}
	blobsBefore := countRows(t, ctx, store, `SELECT count(*) FROM blobs`)

	deleted, counters, err := engine.DeleteProject(ctx, "SDLC", system.ID)
	if err != nil {
		t.Fatalf("удаление проекта: %v", err)
	}
	if deleted.ID != project.ID {
		t.Fatalf("удалён не тот проект: %s", deleted.ID)
	}
	if counters.Tasks == 0 || counters.StageRuns == 0 || counters.Artifacts != 2 ||
		counters.Repositories == 0 || counters.IndexRuns != 1 || counters.CodeFiles != 1 ||
		counters.CodeChunks != 1 || counters.Documents != 2 || counters.Members == 0 ||
		counters.Tokens != 1 || counters.Roles == 0 || counters.Workflows == 0 ||
		counters.Invites != 1 || counters.CostUSD != 1.5 {
		t.Fatalf("счётчики удалённого: %+v", counters)
	}

	// Считается вся таблица, а не строки удалённого проекта: осиротевшая
	// строка без владельца видна только так. Остаться могут лишь строки
	// соседнего проекта.
	for _, table := range projectOwnedTables {
		n := countRows(t, ctx, store, fmt.Sprintf(`SELECT count(*) FROM %s`, table))
		if n != 0 && !neighbourOwns(t, ctx, store, table, neighbour.ID, n) {
			t.Fatalf("после удаления в %s осталось %d строк", table, n)
		}
	}
	if _, err := store.GetProjectByKey(ctx, store.DB(), "SDLC"); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("проект всё ещё находится: %v", err)
	}
	if _, err := store.GetProjectByKey(ctx, store.DB(), "NEXT"); err != nil {
		t.Fatalf("удаление задело соседний проект: %v", err)
	}

	// Блобы общие по sha256: их чистит сборка мусора, а не удаление проекта.
	if after := countRows(t, ctx, store, `SELECT count(*) FROM blobs`); after != blobsBefore {
		t.Fatalf("блобов было %d, стало %d", blobsBefore, after)
	}

	// След остаётся на команде-владельце: лента задачи ушла вместе с задачей.
	events, err := store.ListTeamEvents(ctx, store.DB(), *project.OwnerTeamID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != domain.EventProjectDeleted {
		t.Fatalf("событие аудита: %+v", events)
	}
	if events[0].ActorID == nil || *events[0].ActorID != system.ID {
		t.Fatalf("событие без актора: %+v", events[0])
	}
	var payload struct {
		Key     string `json:"project_key"`
		Removed struct {
			Tasks   int     `json:"tasks"`
			CostUSD float64 `json:"cost_usd"`
		} `json:"removed"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Key != "SDLC" || payload.Removed.Tasks != counters.Tasks || payload.Removed.CostUSD != 1.5 {
		t.Fatalf("payload события: %s", events[0].Payload)
	}

	// Повторное удаление — уже нечего удалять.
	if _, _, err := engine.DeleteProject(ctx, "SDLC", system.ID); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("повторное удаление: %v", err)
	}
}

// neighbourOwns — все ли оставшиеся в таблице строки принадлежат соседнему
// проекту. Таблицы без project_id считаются пустыми только буквально.
func neighbourOwns(t *testing.T, ctx context.Context, store *postgres.Store, table string, projectID uuid.UUID, total int) bool {
	t.Helper()
	var n int
	err := store.DB().QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE project_id = $1`, table), projectID).Scan(&n)
	if err != nil {
		return false
	}
	return n == total
}
