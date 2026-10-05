package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// rejectedApp — провайдер, не принимающий учётные данные приложения: любой
// поход через установку кончается отказом с меткой, как у
// App.InstallationToken на 401.
type rejectedApp struct{}

func (rejectedApp) AppConfigured() bool { return true }

func (rejectedApp) AppStatus(context.Context, bool) github.AppStatus {
	now := time.Now().UTC()
	return github.AppStatus{State: github.AppRejected, CheckedAt: &now,
		Error: `github 401 на утверждение приложения: {"message":"Bad credentials"}`}
}

func (rejectedApp) CheckRepo(_ context.Context, installationID *int64, _, _, _ string) error {
	if installationID == nil {
		return nil
	}
	return fmt.Errorf("%w: github 401 на токен установки %d: {\"message\":\"Bad credentials\"}",
		github.ErrAppRejected, *installationID)
}

// Отказ приложения на транспорте (CLAB-78): свой код, 502 и исходный текст,
// а не «внутренняя ошибка» и не 400 про репозиторий.
func TestAppRejectionOverHTTP(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true, GitHub: rejectedApp{}})
	auth := workflow.NewAuth(store, log, workflow.AuthHeader)
	srv := NewServer(config.Config{Env: "test"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)

	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	who, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	if project.OwnerTeamID == nil {
		t.Fatal("у демонстрационного проекта нет команды-владельца")
	}
	if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 506, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: *project.OwnerTeamID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ path, body string }{
		{"/v1/projects/SDLC/repositories", `{"repository":"acme/api"}`},
		{"/v1/projects/SDLC/repositories", `{"repository":"acme/api","installation_id":506}`},
		{"/v1/projects/SDLC/github/repositories",
			`{"installation_id":506,"repositories":[{"owner":"acme","name":"api"}]}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-SDLC-Actor", "user:lead")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s %s: код %d вместо 502: %s", tc.path, tc.body, rec.Code, rec.Body)
		}
		var body errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != apperr.CodeProviderMisconfigured {
			t.Fatalf("%s: код ошибки %q", tc.path, body.Error.Code)
		}
		if !strings.Contains(body.Error.Message, github.ErrAppRejected.Error()) ||
			strings.Contains(body.Error.Message, "acme/api") {
			t.Fatalf("%s: текст ошибки %q", tc.path, body.Error.Message)
		}
	}
}

func TestProviderMisconfiguredIsBadGateway(t *testing.T) {
	if got := statusFor(apperr.CodeProviderMisconfigured); got != http.StatusBadGateway {
		t.Fatalf("provider_misconfigured → %d", got)
	}
}
