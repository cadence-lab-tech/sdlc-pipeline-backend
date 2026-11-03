package indexer

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github/githubtest"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// AC-17 CLAB-395: установка снята без вебхука. До опроса индексация ходит в
// провайдера, получает отказ выписки токена и пишет провал головы — жалоба
// в лог одна, а не на каждом проходе. После того как опрос отметил снятие,
// Enqueue за репозиториями установки не ходит вовсе: ни токена, ни головы,
// ни новых провалов.
func TestEnqueueStopsAfterSyncMarksInstallationRemoved(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})

	// Сид демонстрационного проекта не мешает считать запросы.
	if _, err := pool.Exec(ctx, `UPDATE repositories SET index_enabled = false`); err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	who, err := store.ResolveActor(ctx, store.DB(), "lead", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 641, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: *project.OwnerTeamID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (project_id, owner, name, clone_url, installation_id)
		VALUES ($1, 'acme', 'api', 'https://github.com/acme/api.git', 641)`, project.ID); err != nil {
		t.Fatal(err)
	}

	gh := githubtest.New(t)
	gh.Installations[641] = &githubtest.Installation{Repos: []string{"acme/api"}}
	provider := gh.Provider(t)
	var logs bytes.Buffer
	ix := New(store, NewGitHubSource(provider), embed.Hash{},
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Options{ChunkChars: 400, EmbedBatch: 8})

	if n, err := ix.Enqueue(ctx); err != nil || n != 1 {
		t.Fatalf("живая установка: %d, %v", n, err)
	}
	if gh.Requests(githubtest.KindBranch) != 1 {
		t.Fatalf("голова живой установки не прочитана: %d", gh.Requests(githubtest.KindBranch))
	}

	// Установку сняли на GitHub, вебхук не дошёл. Токен из кэша выброшен —
	// как у процесса, который его ещё не выписывал.
	delete(gh.Installations, 641)
	provider.ForgetInstallationToken(641)
	for range 3 {
		if _, err := ix.Enqueue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "учётные данные репозитория не разрешились"); n != 1 {
		t.Fatalf("жалоба на учётные данные в логе %d раз, ждали одну:\n%s", n, logs.String())
	}
	failures := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT coalesce(sum(failures), 0) FROM repository_head_failures`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if failures() != 3 {
		t.Fatalf("провалов головы %d, ждали три прохода", failures())
	}

	changes, err := engine.SyncInstallations(ctx, provider)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Change != workflow.ChangeInstallationRemoved {
		t.Fatalf("опрос: %+v", changes)
	}

	gh.ResetRequests()
	logs.Reset()
	if n, err := ix.Enqueue(ctx); err != nil || n != 0 {
		t.Fatalf("снятая установка: %d, %v", n, err)
	}
	if gh.Requests(githubtest.KindToken)+gh.Requests(githubtest.KindBranch) != 0 {
		t.Fatalf("за снятой установкой ходили: token=%d branch=%d",
			gh.Requests(githubtest.KindToken), gh.Requests(githubtest.KindBranch))
	}
	if failures() != 3 {
		t.Fatalf("провал головы записан после снятия: %d", failures())
	}
	if logs.Len() != 0 {
		t.Fatalf("лог после снятия:\n%s", logs.String())
	}
}
