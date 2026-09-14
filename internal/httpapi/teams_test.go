package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Удаление арендатора — ручка DELETE /v1/teams/{key} (CLAB-13): её закрывает
// не уровень в проекте, а роль в команде, и ответ называет цену действия.
// Проверяется на транспорте: метод и путь прежние, изменились право и ответ.
func TestDeleteTeamOverHTTP(t *testing.T) {
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
	const rootToken = "bootstrap-token-0123456789"
	admin, err := auth.Authenticate(ctx, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	// Токен человека без привязки к проекту: право на команду берётся из
	// состава команды, а не из участия в проекте.
	token := func(handle string) string {
		t.Helper()
		if _, err := store.ResolveActor(ctx, store.DB(), handle, domain.PrincipalUser); err != nil {
			t.Fatal(err)
		}
		issued, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{
			Principal: "user:" + handle, Name: "test-" + handle,
		})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token
	}

	if _, err := auth.CreateTeam(ctx, admin, workflow.CreateTeamInput{Key: "acme", Name: "Acme"}); err != nil {
		t.Fatal(err)
	}
	leadToken, memberToken := token("lead"), token("member")
	if _, err := auth.SetTeamMember(ctx, admin, "acme", "user:lead", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "acme", "user:member", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	// Проект команды: удаление обязано унести его вместе с ней, и это видно в
	// ответе, а не только в базе.
	team, err := store.GetTeamByKey(ctx, store.DB(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateProject(ctx, admin.Principal.ID, postgres.CreateProjectInput{
		Key: "ACME", Name: "Проект команды", OwnerTeamID: team.ID}); err != nil {
		t.Fatal(err)
	}

	if code := call(http.MethodDelete, "/v1/teams/acme", memberToken).Code; code != http.StatusForbidden {
		t.Fatalf("рядовой участник удалил команду: код %d", code)
	}
	if code := call(http.MethodDelete, "/v1/teams/acme", token("stranger")).Code; code != http.StatusForbidden {
		t.Fatalf("посторонний удалил команду: код %d", code)
	}

	// Личная команда не удаляется этой ручкой никогда — 409, а не 403 и не 204.
	personal, err := auth.PersonalTeam(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if code := call(http.MethodDelete, "/v1/teams/"+personal.Key, rootToken).Code; code != http.StatusConflict {
		t.Fatalf("удаление личной команды: код %d", code)
	}

	rec := call(http.MethodDelete, "/v1/teams/acme", leadToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("лид не смог удалить свою команду: код %d, тело %s", rec.Code, rec.Body)
	}
	var out struct {
		Team struct {
			Key string `json:"key"`
		} `json:"team"`
		Removed struct {
			Projects    int      `json:"projects"`
			ProjectKeys []string `json:"project_keys"`
			Members     int      `json:"members"`
		} `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Team.Key != "acme" || out.Removed.Projects != 1 ||
		len(out.Removed.ProjectKeys) != 1 || out.Removed.ProjectKeys[0] != "ACME" ||
		out.Removed.Members != 3 {
		t.Fatalf("ответ удаления: %s", rec.Body)
	}

	// Команда и её проект исчезают отовсюду, повтор — «нет такого».
	if code := call(http.MethodGet, "/v1/teams/acme", rootToken).Code; code != http.StatusNotFound {
		t.Fatalf("карточка удалённой команды: код %d", code)
	}
	if code := call(http.MethodGet, "/v1/projects/ACME", rootToken).Code; code != http.StatusNotFound {
		t.Fatalf("проект удалённой команды: код %d", code)
	}
	if code := call(http.MethodDelete, "/v1/teams/acme", rootToken).Code; code != http.StatusNotFound {
		t.Fatalf("повторное удаление: код %d", code)
	}
}
