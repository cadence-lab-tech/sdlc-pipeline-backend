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
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Удаление проекта доступно только его администратору и уносит проект из всех
// выдач. Проверяется на транспорте: уровень назначает роутер, а не движок.
func TestDeleteProjectNeedsProjectAdmin(t *testing.T) {
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
	token := func(handle string, level domain.MemberRole) string {
		t.Helper()
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
	call := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	viewerToken := token("looker", domain.MemberViewer)
	if code := call(http.MethodDelete, "/v1/projects/SDLC", viewerToken).Code; code != http.StatusForbidden {
		t.Fatalf("наблюдатель удалил проект: код %d", code)
	}
	if code := call(http.MethodDelete, "/v1/projects/SDLC", token("keeper", domain.MemberMaintainer)).Code; code != http.StatusForbidden {
		t.Fatalf("сопровождающий удалил проект: код %d", code)
	}

	adminToken := token("boss", domain.MemberAdmin)
	rec := call(http.MethodDelete, "/v1/projects/SDLC", adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("администратор проекта не смог удалить: код %d, тело %s", rec.Code, rec.Body)
	}
	var out struct {
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
		Removed struct {
			Roles     int `json:"roles"`
			Workflows int `json:"workflows"`
		} `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Project.Key != "SDLC" || out.Removed.Roles == 0 || out.Removed.Workflows == 0 {
		t.Fatalf("ответ удаления: %s", rec.Body)
	}

	// Токен, привязанный к проекту, умирает вместе с ним: строка agent_tokens
	// уходит каскадом, и предъявлять больше нечего.
	if code := call(http.MethodGet, "/v1/projects", adminToken).Code; code != http.StatusUnauthorized {
		t.Fatalf("токен удалённого проекта всё ещё действует: код %d", code)
	}

	// Дальше — токеном администратора инстанса: он к проекту не привязан.
	const rootToken = "bootstrap-token-0123456789"

	// Проект исчезает отовсюду: карточка, задачи, список.
	if code := call(http.MethodGet, "/v1/projects/SDLC", rootToken).Code; code != http.StatusNotFound {
		t.Fatalf("карточка удалённого проекта: код %d", code)
	}
	if code := call(http.MethodGet, "/v1/projects/SDLC/tasks", rootToken).Code; code != http.StatusNotFound {
		t.Fatalf("задачи удалённого проекта: код %d", code)
	}
	list := call(http.MethodGet, "/v1/projects", rootToken)
	if list.Code != http.StatusOK {
		t.Fatalf("список проектов: код %d", list.Code)
	}
	var projects struct {
		Projects []struct {
			Key string `json:"key"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &projects); err != nil {
		t.Fatal(err)
	}
	for _, p := range projects.Projects {
		if p.Key == "SDLC" {
			t.Fatal("удалённый проект остался в списке")
		}
	}
	// Удалять больше нечего: повтор — не успех, а «нет такого».
	if code := call(http.MethodDelete, "/v1/projects/SDLC", rootToken).Code; code != http.StatusNotFound {
		t.Fatalf("повторное удаление: код %d", code)
	}
}
