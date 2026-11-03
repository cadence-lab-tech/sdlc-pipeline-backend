package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// memberToken заводит человека уровня level с ролями специалистов roles
// (nil — любые) и выдаёт ему личный токен проекта SDLC.
func (f *auditFixture) memberToken(handle string, level domain.MemberRole, roles []string) string {
	f.t.Helper()
	ctx := context.Background()
	store := postgres.New(f.pool)
	if _, err := store.ResolveActor(ctx, store.DB(), strings.TrimPrefix(handle, "user:"), domain.PrincipalUser); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.auth.SetMember(ctx, f.admin, "SDLC", handle, level, roles); err != nil {
		f.t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{Name: handle, Principal: handle, ProjectKey: "SDLC"})
	if err != nil {
		f.t.Fatal(err)
	}
	return issued.Token
}

// Характеризация до правки (CLAB-379, AC-10): так личный токен уровня agent
// ведёт себя сегодня — двигает задачу из любого статуса, в том числе из
// статуса без исполнителя (start), снимает её (cancel) и проходит в чужой
// статус до гейтов; решение по human_approval ему закрыто уровнем
// maintainer. После правки права этапа тест заменяется тестами AC-3, AC-4,
// AC-6.
func TestAgentTokenTransitionsToday(t *testing.T) {
	f := newAuditFixture(t)
	dev := f.memberToken("user:dev", domain.MemberAgent, []string{"developer"})

	// start из new — статуса без исполнителя.
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-1/transitions/start", dev, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	// analysis — этап роли analyst, а у участника только developer: переход
	// доходит до гейтов (409), права роли не проверяются.
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-1/transitions/analysis_done", dev, nil); code != http.StatusConflict {
		t.Fatalf("analysis_done чужого этапа: %d %v", code, body)
	}
	// cancel — глобальный переход.
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-1/transitions/cancel", dev, nil); code != http.StatusOK {
		t.Fatalf("cancel: %d %v", code, body)
	}
	// start эпика из planning — статуса без исполнителя другого цикла.
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-4/transitions/start", f.bot, nil); code != http.StatusOK {
		t.Fatalf("start эпика: %d %v", code, body)
	}

	// MCP — то же правило: start и cancel проходят.
	if out, isErr := f.mcp(dev, "sdlc_request_transition", map[string]any{"task_key": "SDLC-2", "transition": "start"}); isErr {
		t.Fatalf("MCP start: %v", out)
	}
	if out, isErr := f.mcp(dev, "sdlc_request_transition", map[string]any{"task_key": "SDLC-2", "transition": "cancel"}); isErr {
		t.Fatalf("MCP cancel: %v", out)
	}

	// Решение по human_approval — только maintainer.
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-3/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start SDLC-3: %d %v", code, body)
	}
	ana := f.memberToken("user:ana", domain.MemberAgent, []string{"analyst"})
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-3/transitions/analysis_done/gates/human_approval/decision",
		ana, map[string]any{"status": "passed"}); code != http.StatusForbidden {
		t.Fatalf("human_approval участником agent: %d %v", code, body)
	}
}
