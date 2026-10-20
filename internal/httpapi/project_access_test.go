package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// projectAccess — то, что clab whoami читает из GET /v1/projects/:key.
// allowed_roles указателем: null (любые) и [] (ни одной) — разные ответы.
type projectAccess struct {
	Project struct {
		Key string `json:"key"`
	} `json:"project"`
	Repositories []json.RawMessage `json:"repositories"`
	Access       *struct {
		Level        string    `json:"level"`
		AllowedRoles *[]string `json:"allowed_roles"`
	} `json:"access"`
}

func getAccess(t *testing.T, srv *Server, header, value string) projectAccess {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/projects/SDLC", nil)
	req.Header.Set(header, value)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/projects/SDLC: код %d, тело %s", rec.Code, rec.Body)
	}
	var out projectAccess
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Project.Key != "SDLC" || out.Repositories == nil {
		t.Fatalf("прежние поля ответа пропали: %s", rec.Body)
	}
	if out.Access == nil {
		t.Fatalf("в ответе нет access: %s", rec.Body)
	}
	return out
}

func rolesOf(a projectAccess) []string {
	if a.Access.AllowedRoles == nil {
		return nil
	}
	roles := append([]string{}, *a.Access.AllowedRoles...)
	sort.Strings(roles)
	return roles
}

// CLAB-146, критерий 16: карточка проекта отдаёт доступ актора тем же
// Auth.ProjectRole и Auth.AllowedRoles, что держат захват этапа.
func TestGetProjectReportsActorAccess(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(config.Config{Env: "test"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)

	const rootToken = "bootstrap-token-0123456789"
	if err := auth.Bootstrap(ctx, "root", rootToken); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(handle string, level domain.MemberRole, memberRoles, tokenRoles []string) string {
		t.Helper()
		if _, err := store.ResolveActor(ctx, store.DB(), handle, domain.PrincipalUser); err != nil {
			t.Fatal(err)
		}
		if _, err := auth.SetMember(ctx, admin, "SDLC", handle, level, memberRoles); err != nil {
			t.Fatal(err)
		}
		issued, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{
			Principal: handle, ProjectKey: "SDLC", Name: "test-" + handle, RoleKeys: tokenRoles,
		})
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + issued.Token
	}

	// Участник с ролями developer и reviewer, токен сужен до developer.
	narrowed := getAccess(t, srv, "Authorization", issue("dev", domain.MemberAgent, []string{"developer", "reviewer"}, []string{"developer"}))
	if narrowed.Access.Level != "agent" || !reflect.DeepEqual(rolesOf(narrowed), []string{"developer"}) {
		t.Fatalf("участник с сужающим токеном: %+v %v", narrowed.Access, rolesOf(narrowed))
	}

	// Участник с ролями, токен без ограничений — роли участия.
	member := getAccess(t, srv, "Authorization", issue("rev", domain.MemberMaintainer, []string{"reviewer", "qa"}, nil))
	if member.Access.Level != "maintainer" || !reflect.DeepEqual(rolesOf(member), []string{"qa", "reviewer"}) {
		t.Fatalf("участник с ролями: %+v %v", member.Access, rolesOf(member))
	}

	// Участник без ограничения ролей — null, то есть любые.
	open := getAccess(t, srv, "Authorization", issue("any", domain.MemberAgent, nil, nil))
	if open.Access.Level != "agent" || open.Access.AllowedRoles != nil {
		t.Fatalf("участник без ограничений: %+v", open.Access)
	}

	// Администратор системы: уровень admin, роли держит только токен.
	root := getAccess(t, srv, "Authorization", "Bearer "+rootToken)
	if root.Access.Level != "admin" || root.Access.AllowedRoles != nil {
		t.Fatalf("администратор системы: %+v", root.Access)
	}

	// Режим заголовка: участия нет, прав нет — роли не ограничены.
	headerAuth := workflow.NewAuth(store, log, workflow.AuthHeader)
	headerSrv := NewServer(config.Config{Env: "test"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), headerAuth, nil)
	hdr := getAccess(t, headerSrv, "X-SDLC-Actor", "agent:somebody")
	if hdr.Access.AllowedRoles != nil {
		t.Fatalf("режим заголовка: %+v", hdr.Access)
	}
}
