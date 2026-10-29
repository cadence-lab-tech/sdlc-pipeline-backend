package testdb

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Данные для проверок видимости токена задачи (CLAB-305): задача другого
// проекта и корпус поиска по всем репозиториям проекта. Пакетам workflow,
// httpapi и mcp они нужны одинаковые, а движок отсюда не позвать — workflow
// сам зависит от testdb, — поэтому всё через хранилище.

// ForeignTask заводит рядом с демонстрационным проектом проект OTHER той же
// команды и в нём задачу OTHER-1 с артефактом — для проверок границы проекта
// (токен, привязанный к SDLC, задачу OTHER не видит). Цикл задачи — тот же,
// что у SDLC-1: связку в OTHER ради одной строки задачи импортировать
// незачем, а схема требует только пару цикл–статус.
func ForeignTask(t *testing.T, store *postgres.Store) (domain.Task, domain.Artifact) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO projects (key, name, owner_team_id)
		SELECT 'OTHER', 'Другой проект', owner_team_id FROM projects WHERE key = 'SDLC'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO tasks (project_id, key, seq, title, description, workflow_id, status_id)
		SELECT p.id, 'OTHER-1', 1, 'Чужая', 'описание OTHER-1', t.workflow_id, t.status_id
		FROM projects p, tasks t WHERE p.key = 'OTHER' AND t.key = 'SDLC-1'`); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTaskByKey(ctx, store.DB(), "OTHER-1")
	if err != nil {
		t.Fatal(err)
	}
	return task, Artifact(t, store, task, "секрет OTHER-1")
}

// Artifact кладёт задаче текстовый артефакт без прогона — как сдал бы его
// человек; содержимое — content. Вид свой, test.note: сдача этапа того же
// вида перекрыла бы артефакт (supersedes_id), и поиск его бы не нашёл.
func Artifact(t *testing.T, store *postgres.Store, task domain.Task, content string) domain.Artifact {
	t.Helper()
	ctx := context.Background()
	author, err := store.ResolveActor(ctx, store.DB(), "test-system", domain.PrincipalSystem)
	if err != nil {
		t.Fatal(err)
	}
	var out domain.Artifact
	err = store.InTx(ctx, func(tx pgx.Tx) error {
		a, err := store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task: task, Kind: "test.note", Title: "заметка " + task.Key,
			Format: "markdown", Content: []byte(content), MediaType: "text/markdown",
			Status: domain.ArtifactSubmitted, ProducedBy: author.ID,
		})
		out = a
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// SearchCorpus индексирует слово word в каждом репозитории проекта SDLC
// (один кусок кода на репозиторий) и в артефакте каждой из задач keys — по
// куску на задачу. Векторов нет: поиск идёт полнотекстом.
func SearchCorpus(t *testing.T, store *postgres.Store, word string, keys ...string) {
	t.Helper()
	ctx, q := context.Background(), store.DB()
	project, err := store.GetProjectByKey(ctx, q, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	repos, err := store.ListRepositories(ctx, q, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		run, _, err := store.EnqueueIndexRun(ctx, q, postgres.EnqueueIndexRunInput{
			RepositoryID: r.ID, Branch: "main", CommitSHA: strings.Repeat("c", 40), Trigger: "manual",
		})
		if err != nil {
			t.Fatal(err)
		}
		var fileID uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO code_files (index_run_id, repository_id, path, lang, size_bytes)
			VALUES ($1, $2, $3, 'go', 10) RETURNING id`, run.ID, r.ID, word+".go").Scan(&fileID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO code_chunks (code_file_id, project_id, ord, start_line, end_line, text)
			VALUES ($1, $2, 0, 1, 10, $3)`, fileID, project.ID, "func "+word+"() {}"); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE repo_index_runs SET state = 'completed', finished_at = now() WHERE id = $1`, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range keys {
		task, err := store.GetTaskByKey(ctx, q, key)
		if err != nil {
			t.Fatal(err)
		}
		a := Artifact(t, store, task, word+" "+key)
		if err := store.ReplaceArtifactChunks(ctx, q, a.ID, project.ID, task.ID,
			[]postgres.TextChunkInput{{Text: word + " " + key, TextSHA: "sha-" + word + "-" + key}}); err != nil {
			t.Fatal(err)
		}
	}
}
