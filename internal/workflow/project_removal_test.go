package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	"task_property_defs",
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
	// Соседний проект той же команды: удаление обязано пройти мимо него. Он
	// не пустой намеренно — проверять надо, что уцелело содержимое, а не что
	// строка проекта осталась.
	neighbour, err := engine.CreateProject(ctx, system.ID, postgres.CreateProjectInput{
		Key: "NEXT", Name: "Соседний", OwnerTeamID: *project.OwnerTeamID})
	if err != nil {
		t.Fatal(err)
	}
	neighbourRepo, _, err := engine.AddRepository(ctx, "NEXT", AddRepositoryInput{Repository: "acme/web", RoleHint: "web"})
	if err != nil {
		t.Fatal(err)
	}
	neighbourRun, _, err := store.EnqueueIndexRun(ctx, store.DB(), postgres.EnqueueIndexRunInput{
		RepositoryID: neighbourRepo.ID, Branch: "main", CommitSHA: strings.Repeat("b", 40), Trigger: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO code_files (index_run_id, repository_id, path, lang, size_bytes)
		VALUES ($1, $2, 'web.ts', 'ts', 10)`, neighbourRun.ID, neighbourRepo.ID); err != nil {
		t.Fatal(err)
	}
	blobsBefore := countRows(t, ctx, store, `SELECT count(*) FROM blobs`)
	// Снимок содержимого до вызова: счётчики в ответе сверяются с ним, а не с
	// подобранными числами — ошибка на единицу иначе проходит незамеченной.
	before := projectContents(t, ctx, store, project.ID)

	deleted, counters, err := engine.DeleteProject(ctx, "SDLC", system.ID)
	if err != nil {
		t.Fatalf("удаление проекта: %v", err)
	}
	if deleted.ID != project.ID {
		t.Fatalf("удалён не тот проект: %s", deleted.ID)
	}
	got := map[string]int{
		"tasks": counters.Tasks, "stage_runs": counters.StageRuns,
		"artifacts": counters.Artifacts, "repositories": counters.Repositories,
		"repo_index_runs": counters.IndexRuns, "code_files": counters.CodeFiles,
		"code_chunks": counters.CodeChunks, "project_documents": counters.Documents,
		"project_members": counters.Members, "agent_tokens": counters.Tokens,
		"roles": counters.Roles, "workflows": counters.Workflows,
		"invites": counters.Invites,
	}
	for table, want := range before {
		if want == 0 {
			t.Fatalf("проект не набит: в %s не было ни строки", table)
		}
		if got[table] != want {
			t.Fatalf("счётчик %s: в ответе %d, в базе до удаления было %d", table, got[table], want)
		}
	}
	if counters.CostUSD != 1.5 {
		t.Fatalf("расход в счётчиках: %v", counters.CostUSD)
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
	// У соседа на месте не только строка проекта, но и содержимое.
	for table, want := range map[string]int{"repositories": 1, "repo_index_runs": 1, "code_files": 1} {
		if n := countRows(t, ctx, store, fmt.Sprintf(projectScopes[table], neighbour.ID)); n != want {
			t.Fatalf("у соседнего проекта в %s осталось %d вместо %d", table, n, want)
		}
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
// проекту. Таблицы, у которых нет project_id и нет пути к нему в
// projectScopes, считаются пустыми только буквально.
func neighbourOwns(t *testing.T, ctx context.Context, store *postgres.Store, table string, projectID uuid.UUID, total int) bool {
	t.Helper()
	query, ok := projectScopes[table]
	if !ok {
		query = fmt.Sprintf(`SELECT count(*) FROM %s WHERE project_id = '%%s'`, table)
	}
	var n int
	if err := store.DB().QueryRow(ctx, fmt.Sprintf(query, projectID)).Scan(&n); err != nil {
		return false
	}
	return n == total
}

// projectScopes — как посчитать строки проекта в таблице: у большинства есть
// project_id, индекс репозиториев достаётся через сам репозиторий. Список
// покрывает всё, что попадает в счётчики ручки.
var projectScopes = map[string]string{
	"tasks":             `SELECT count(*) FROM tasks WHERE project_id = '%s'`,
	"stage_runs":        `SELECT count(*) FROM stage_runs sr JOIN tasks t ON t.id = sr.task_id WHERE t.project_id = '%s'`,
	"artifacts":         `SELECT count(*) FROM artifacts WHERE project_id = '%s'`,
	"repositories":      `SELECT count(*) FROM repositories WHERE project_id = '%s'`,
	"repo_index_runs":   `SELECT count(*) FROM repo_index_runs ir JOIN repositories r ON r.id = ir.repository_id WHERE r.project_id = '%s'`,
	"code_files":        `SELECT count(*) FROM code_files cf JOIN repositories r ON r.id = cf.repository_id WHERE r.project_id = '%s'`,
	"code_chunks":       `SELECT count(*) FROM code_chunks WHERE project_id = '%s'`,
	"project_documents": `SELECT count(*) FROM project_documents WHERE project_id = '%s'`,
	"project_members":   `SELECT count(*) FROM project_members WHERE project_id = '%s'`,
	"agent_tokens":      `SELECT count(*) FROM agent_tokens WHERE project_id = '%s'`,
	"roles":             `SELECT count(*) FROM roles WHERE project_id = '%s'`,
	"workflows":         `SELECT count(*) FROM workflows WHERE project_id = '%s'`,
	"invites":           `SELECT count(*) FROM invites WHERE project_id = '%s'`,
}

// projectContents — что лежит в проекте прямо сейчас. Запросы независимые:
// счётчики ручки не должны сверяться сами с собой.
func projectContents(t *testing.T, ctx context.Context, store *postgres.Store, projectID uuid.UUID) map[string]int {
	t.Helper()
	out := map[string]int{}
	for table, query := range projectScopes {
		out[table] = countRows(t, ctx, store, fmt.Sprintf(query, projectID))
	}
	return out
}

// Удаление идёт одной транзакцией: ошибка в середине оставляет проект целиком,
// полуудалённого состояния не возникает.
func TestDeleteProjectRollsBackWhole(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	projectWithEverything(t, ctx, store, engine, system)

	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	before := projectContents(t, ctx, store, project.ID)

	boom := errors.New("искусственная ошибка после удаления")
	err = store.InTx(ctx, func(tx pgx.Tx) error {
		if err := store.DeleteProject(ctx, tx, project.ID); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ожидалась искусственная ошибка: %v", err)
	}

	if _, err := store.GetProjectByKey(ctx, store.DB(), "SDLC"); err != nil {
		t.Fatalf("проект не вернулся после отката: %v", err)
	}
	after := projectContents(t, ctx, store, project.ID)
	for table, want := range before {
		if after[table] != want {
			t.Fatalf("после отката в %s осталось %d строк вместо %d", table, after[table], want)
		}
	}
}

// Агент, державший аренду этапа удалённого проекта, получает «не найдено», а не
// пятисотку: прогон ушёл каскадом задачи, и работать больше не с чем.
func TestAgentOfDeletedProjectIsToldNotFound(t *testing.T) {
	ctx := context.Background()
	_, engine, _, _, system := setupPhase4(t, AuthHeader)

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Живая задача"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, task.Key, "start", system.ID, "поехали"); err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.Claim(ctx, task.Key, system.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.DeleteProject(ctx, "SDLC", system.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Heartbeat(ctx, claimed.StageRunID, system.ID); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("heartbeat после удаления проекта: код %q, ошибка %v", apperr.As(err).Code, err)
	}
	if _, err := engine.SubmitArtifact(ctx, task.Key, system.ID, SubmitArtifactInput{
		Kind: "analysis.brief", Title: "Бриф", Content: []byte("# Бриф"),
	}); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("сдача артефакта после удаления проекта: код %q, ошибка %v", apperr.As(err).Code, err)
	}
	if _, err := engine.Execute(ctx, task.Key, "analysis_done", system.ID, ""); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("переход после удаления проекта: код %q, ошибка %v", apperr.As(err).Code, err)
	}
}
