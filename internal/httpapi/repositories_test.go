package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Отвязка репозитория стоит уровнем выше привязки: привязка обратима, отвязка
// уносит знание. Проверяется на транспорте, потому что уровень назначает
// роутер, а не движок.
func TestRemoveRepositoryNeedsProjectAdmin(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(config.Config{Env: "test"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)

	if err := auth.Bootstrap(ctx, "root", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{Repository: "acme/api"}); err != nil {
		t.Fatal(err)
	}

	token := func(handle string, level domain.MemberRole) string {
		t.Helper()
		// Человек по хэндлу не заводится (решение 0033): принципал появляется
		// входом, здесь — резолвом актора вместо него.
		if _, err := store.ResolveActor(ctx, store.DB(), handle, domain.PrincipalUser); err != nil {
			t.Fatal(err)
		}
		if _, err := auth.SetMember(ctx, admin, "SDLC", handle, level, nil); err != nil {
			t.Fatal(err)
		}
		issued, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{
			Principal: handle, ProjectKey: "SDLC", Name: "test-" + handle, MemberRole: level,
		})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token
	}

	call := func(token string) int {
		req := httptest.NewRequest(http.MethodDelete, "/v1/projects/SDLC/repositories/acme/api", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call(token("keeper", domain.MemberMaintainer)); code != http.StatusForbidden {
		t.Fatalf("сопровождающий отвязал репозиторий: код %d", code)
	}
	adminToken := token("boss", domain.MemberAdmin)
	if code := call(adminToken); code != http.StatusOK {
		t.Fatalf("администратор проекта не смог отвязать: код %d", code)
	}
	// Отвязывать больше нечего: повтор — не успех, а «нет такого».
	if code := call(adminToken); code != http.StatusNotFound {
		t.Fatalf("повторная отвязка: код %d", code)
	}
}
