package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Профиль single на транспорте (CLAB-88): коды и тела ответов, которые видят
// интерфейс и CLI. Номера — критерии приёмки задачи.
func TestSingleProfileOverHTTP(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken).Tenancy(workflow.TenancySingle).OpenProjectCreation(true)
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
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	if _, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	issued, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{Principal: "user:lead", Name: "test-lead"})
	if err != nil {
		t.Fatal(err)
	}
	leadToken := issued.Token

	// К8: команду заводит только администратор, отказ называет его.
	rec := call(http.MethodPost, "/v1/teams", leadToken, `{"key":"rogue","name":"Сами"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "администратор") {
		t.Fatalf("команду завёл не администратор: %d %s", rec.Code, rec.Body)
	}
	if rec = call(http.MethodPost, "/v1/teams", rootToken, `{"key":"acme","name":"Acme"}`); rec.Code != http.StatusCreated {
		t.Fatalf("команда администратора: %d %s", rec.Code, rec.Body)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "acme", "user:lead", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}

	// К4: проект без команды — 400 с понятным текстом, и администратору тоже.
	for who, token := range map[string]string{"лид": leadToken, "администратор": rootToken} {
		rec := call(http.MethodPost, "/v1/projects", token, `{"key":"NOTEAM","name":"Без команды"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "личных команд на этом инстансе нет") {
			t.Fatalf("%s: проект без команды: %d %s", who, rec.Code, rec.Body)
		}
	}
	// К5: в команде с правом — 201.
	if rec = call(http.MethodPost, "/v1/projects", leadToken, `{"key":"ACME","name":"Проект","team":"acme"}`); rec.Code != http.StatusCreated {
		t.Fatalf("проект лида в команде: %d %s", rec.Code, rec.Body)
	}

	// К11: тарифов нет — 404, и администратору тоже.
	if rec = call(http.MethodGet, "/v1/plans", rootToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/plans: %d %s", rec.Code, rec.Body)
	}
	rec = call(http.MethodPut, "/v1/teams/acme/plan", rootToken, `{"plan":"business"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "тарифов на этом инстансе нет") {
		t.Fatalf("PUT /v1/teams/acme/plan: %d %s", rec.Code, rec.Body)
	}

	// К12: по сырому ответу — ключа plan нет, остальные на месте.
	keysOf := func(rec *httptest.ResponseRecorder) map[string]json.RawMessage {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/teams/acme: %d %s", rec.Code, rec.Body)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
			t.Fatal(err)
		}
		return keys
	}
	keys := keysOf(call(http.MethodGet, "/v1/teams/acme", leadToken, ""))
	if _, ok := keys["plan"]; ok {
		t.Fatal("ключ plan в ответе команды на выделенном инстансе")
	}
	for _, k := range []string{"team", "members", "projects", "installations"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("нет ключа %q", k)
		}
	}

	// К16: на общем инстансе тарифы на месте.
	auth.Tenancy(workflow.TenancyShared)
	if _, ok := keysOf(call(http.MethodGet, "/v1/teams/acme", leadToken, ""))["plan"]; !ok {
		t.Fatal("на общем инстансе в команде нет plan")
	}
	if rec = call(http.MethodGet, "/v1/plans", rootToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/plans на общем инстансе: %d %s", rec.Code, rec.Body)
	}
	if rec = call(http.MethodPost, "/v1/teams", leadToken, `{"key":"self","name":"Сами"}`); rec.Code != http.StatusCreated {
		t.Fatalf("организация по кнопке на общем инстансе: %d %s", rec.Code, rec.Body)
	}
}
