package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Схема из output_contract роли ревьюера в сиде: именно её проверяет гейт
// schema_validation, когда своей схемы у гейта нет.
const verdictSchema = `{"type":"object","required":["verdict","findings"],
	"properties":{"verdict":{"enum":["pass","fail"]},"findings":{"type":"array"}}}`

func TestSchemaValidation(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantOK   bool
		contains string
	}{
		{
			name:   "вердикт по схеме",
			doc:    `{"verdict":"pass","findings":[]}`,
			wantOK: true,
		},
		{
			name:     "нет обязательного поля",
			doc:      `{"verdict":"pass"}`,
			contains: "findings",
		},
		{
			name:     "значение вне перечисления",
			doc:      `{"verdict":"maybe","findings":[]}`,
			contains: "verdict",
		},
		{
			name:     "поле не того типа",
			doc:      `{"verdict":"pass","findings":"нет"}`,
			contains: "findings",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc any
			if err := json.Unmarshal([]byte(tc.doc), &doc); err != nil {
				t.Fatalf("подготовка теста: %v", err)
			}
			problems, err := validateAgainstSchema(json.RawMessage(verdictSchema), doc)
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if tc.wantOK {
				if len(problems) != 0 {
					t.Fatalf("ожидалось, что схема пройдёт: %v", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("схема обязана была отбить документ")
			}
			found := false
			for _, p := range problems {
				if strings.Contains(p, tc.contains) {
					found = true
				}
			}
			if !found {
				t.Fatalf("в ошибках нет упоминания %q: %v", tc.contains, problems)
			}
		})
	}
}

// Ожидания сверяются как вложенное подмножество: сверх перечисленного в
// артефакте может быть что угодно.
func TestExpectMismatches(t *testing.T) {
	doc := map[string]any{
		"verdict":  "pass",
		"findings": []any{},
		"meta":     map[string]any{"reviewer": "reviewer-bot", "attempt": 2.0},
	}

	cases := []struct {
		name   string
		expect map[string]any
		want   int
	}{
		{"совпало", map[string]any{"verdict": "pass"}, 0},
		{"не совпало значение", map[string]any{"verdict": "fail"}, 1},
		{"нет поля", map[string]any{"score": 5}, 1},
		{"вложенное совпало", map[string]any{"meta": map[string]any{"attempt": 2}}, 0},
		{"вложенное не совпало", map[string]any{"meta": map[string]any{"attempt": 1}}, 1},
		{"пустые ожидания ничего не проверяют", nil, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expectMismatches(tc.expect, doc, "")
			if len(got) != tc.want {
				t.Fatalf("расхождений %d, ожидалось %d: %v", len(got), tc.want, got)
			}
		})
	}
}

// Вердикт ревьюера действует, пока исполнитель не сдал новую версию
// результата: иначе задача проехала бы гейт с проверкой прошлой работы.
func TestReviewStale(t *testing.T) {
	finished := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	review := domain.StageRun{FinishedAt: &finished}

	before := domain.Artifact{CreatedAt: finished.Add(-time.Minute)}
	after := domain.Artifact{CreatedAt: finished.Add(time.Minute)}

	if reviewStale(review, []domain.Artifact{before}) {
		t.Fatal("артефакт старше вердикта не делает его устаревшим")
	}
	if !reviewStale(review, []domain.Artifact{before, after}) {
		t.Fatal("артефакт, сданный после вердикта, обязан его обесценить")
	}
	if reviewStale(domain.StageRun{}, []domain.Artifact{after}) {
		t.Fatal("незакрытая перепроверка устареть не может")
	}
}

func TestIsProblemReport(t *testing.T) {
	for _, kind := range []string{"problem.report", "problem.blocking"} {
		if !isProblemReport(kind) {
			t.Fatalf("%q — описание проблемы", kind)
		}
	}
	for _, kind := range []string{"analysis.brief", "problem", "reproblem.report"} {
		if isProblemReport(kind) {
			t.Fatalf("%q не описание проблемы", kind)
		}
	}
}

// Захват объясняет отказ кодом из контракта агента: по нему локальный агент
// решает, ждать освобождения или идти к другой задаче.
func TestClaimable(t *testing.T) {
	me, other := uuid.New(), uuid.New()
	role := uuid.New()
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name string
		run  domain.StageRun
		want apperr.Code
	}{
		{
			name: "свободный этап",
			run:  domain.StageRun{State: domain.StagePending, RoleID: &role},
		},
		{
			name: "свой же захват продлевается",
			run: domain.StageRun{State: domain.StageClaimed, RoleID: &role,
				ClaimedBy: &me, LeaseExpires: &future},
		},
		{
			name: "аренда протухла — можно забирать",
			run: domain.StageRun{State: domain.StageClaimed, RoleID: &role,
				ClaimedBy: &other, LeaseExpires: &past},
		},
		{
			name: "сдал артефакт, но задачу не отпустил",
			run: domain.StageRun{State: domain.StageSubmitted, RoleID: &role,
				ClaimedBy: &other, LeaseExpires: &future},
			want: apperr.CodeLeaseHeldByOther,
		},
		{
			name: "держит другой агент",
			run: domain.StageRun{State: domain.StageInProgress, RoleID: &role,
				ClaimedBy: &other, LeaseExpires: &future},
			want: apperr.CodeLeaseHeldByOther,
		},
		{
			name: "на статусе нет роли",
			run:  domain.StageRun{State: domain.StagePending},
			want: apperr.CodeTaskNotClaimable,
		},
		{
			name: "этап уже закрыт",
			run:  domain.StageRun{State: domain.StagePassed, RoleID: &role},
			want: apperr.CodeTaskNotClaimable,
		},
		{
			name: "этап ушёл в gating и ждёт перепроверки",
			run:  domain.StageRun{State: domain.StageGating, RoleID: &role},
			want: apperr.CodeTaskNotClaimable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := claimable(tc.run, me)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("захват должен был пройти: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ожидался отказ %q", tc.want)
			}
			if got := apperr.As(err).Code; got != tc.want {
				t.Fatalf("код %q, ожидался %q", got, tc.want)
			}
		})
	}
}
