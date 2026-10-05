package indexer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Прогон репозитория на установке при непринятом приложении (CLAB-78):
// ошибка прогона начинается с описания отказа приложения, а не с «github 401
// на токен установки», и прогон уходит на повтор — причина бывает временной
// (часы машины), постоянной её не делают.
func TestRunFailsWithAppRejectionAndRetries(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})

	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	who, err := store.ResolveActor(ctx, store.DB(), "lead", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 507, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: *project.OwnerTeamID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{Repository: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repositories SET installation_id = 507
		WHERE project_id = $1 AND owner = 'acme' AND name = 'api'`, project.ID); err != nil {
		t.Fatal(err)
	}

	// Коммит в очередь ставит рабочий источник, исполняет прогон — источник
	// с приложением, которое провайдер не принимает.
	ix := newTestIndexer(t, store, &fakeSource{
		repo: "api", head: "c1",
		commits: map[string]map[string]string{"c1": {"internal/workflow/engine.go": engineV1}},
	})
	if n, err := ix.Enqueue(ctx); err != nil || n != 1 {
		t.Fatalf("постановка в очередь: %d, %v", n, err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			t.Errorf("поход мимо токена установки: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := github.NewApp(srv.URL, "12345", pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	if err != nil {
		t.Fatal(err)
	}
	ix.source = NewGitHubSource(github.NewProvider(github.New(srv.URL, ""), app, nil))

	if run, err := ix.RunNext(ctx); err != nil || run != nil {
		t.Fatalf("прогон: %+v, %v", run, err)
	}
	var state, cause string
	if err := pool.QueryRow(ctx, `SELECT state, coalesce(error, '') FROM repo_index_runs
		ORDER BY created_at DESC LIMIT 1`).Scan(&state, &cause); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cause, github.ErrAppRejected.Error()) {
		t.Fatalf("ошибка прогона начинается не с отказа приложения: %q", cause)
	}
	if !strings.Contains(cause, "Bad credentials") {
		t.Fatalf("в ошибке прогона нет ответа провайдера: %q", cause)
	}
	if state != "queued" {
		t.Fatalf("прогон в состоянии %q: отказ приложения сделан постоянным", state)
	}
}
