// Политика учёта проекта «захват без учёта запрещён» (CLAB-219, дополнение к
// решению 0026). Администратор проекта включает её, чтобы агент, который не
// отчитывается о стоимости, не получал новую работу. По умолчанию выключена:
// упор сейчас на локальные раннеры сотрудников на подписке, им она мешать не
// должна.
//
// Политика действует только на принципалов вида agent. Человек под своей
// сессией и раннер сотрудника на личном токене под неё не попадают: так
// человек сам забирает этап, и это его работа, а не исполнителя.
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// UsageReportRequired едет в ответе захвата вместо expected, когда политика
// проекта включена и захватывает агентский принципал: без отчёта о прогоне
// следующего захвата не будет.
const UsageReportRequired = "required"

// usageDebtsShown — сколько долгов перечисляет отказ; полное число — total.
const usageDebtsShown = 20

// UsagePolicyView — политика учёта проекта для показа: как записана и что
// из неё действует.
type UsagePolicyView struct {
	Project string `json:"project"`
	Mode    string `json:"mode"`
	// Окно как задано; null — не задано, действует default_grace_minutes.
	GraceMinutes          *int       `json:"grace_minutes"`
	EffectiveGraceMinutes int        `json:"effective_grace_minutes"`
	DefaultGraceMinutes   int        `json:"default_grace_minutes"`
	EnabledAt             *time.Time `json:"enabled_at"`
}

func usagePolicyView(project string, p domain.UsagePolicy) UsagePolicyView {
	return UsagePolicyView{
		Project: project, Mode: p.Mode, GraceMinutes: p.GraceMinutes,
		EffectiveGraceMinutes: p.Grace(), DefaultGraceMinutes: domain.DefaultUsageGraceMinutes,
		EnabledAt: p.EnabledAt,
	}
}

// UsagePolicyInput — правка политики: меняется то, что задано.
type UsagePolicyInput struct {
	// off или required; nil — режим не меняется.
	Mode *string
	// SetGrace — менять ли окно; Grace nil при SetGrace — снять окно, снова
	// действует значение по умолчанию.
	SetGrace bool
	Grace    *int
}

// UsagePolicyChange — было и стало: журнал действий пишет оба.
type UsagePolicyChange struct {
	Before UsagePolicyView `json:"before"`
	After  UsagePolicyView `json:"after"`
}

const usageGraceHint = "окно — целое число минут от 0 до %d (0 — без окна) или default — по умолчанию %d"

// ParseUsageGrace разбирает окно из тела запроса: целое число минут, null или
// "default" — снять окно. Отсутствие поля решает транспорт (SetGrace).
func ParseUsageGrace(raw json.RawMessage) (*int, error) {
	text := strings.TrimSpace(string(raw))
	if text == "null" || text == `"default"` {
		return nil, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return nil, apperr.InvalidInput("grace_minutes: %s — "+usageGraceHint,
			text, domain.MaxUsageGraceMinutes, domain.DefaultUsageGraceMinutes)
	}
	return &n, nil
}

// UsagePolicy — политика учёта проекта. Читают её все участники: она же
// лежит в project.settings.
func (e *Engine) UsagePolicy(ctx context.Context, projectKey string) (UsagePolicyView, error) {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return UsagePolicyView{}, err
	}
	p, err := e.store.ProjectUsagePolicy(ctx, q, project.ID, false)
	if err != nil {
		return UsagePolicyView{}, err
	}
	return usagePolicyView(project.Key, p), nil
}

// SetUsagePolicy меняет политику учёта проекта. Уровень администратора
// проверяет транспорт guard'ом (администратор проекта или системы), токен
// задачи отбит белым списком исполнителя; здесь — вид принципала: политику
// меняет человек, агент её не меняет даже с уровнем admin — иначе агент,
// которого она держит, снял бы её сам. Правило вида — в любом режиме, как у
// апрува человека.
//
// Момент включения ставит сервер при переходе off → required; required
// поверх required его не сдвигает, выключение его снимает, и следующее
// включение ставит новый: прогоны, закрытые до включения, долгом не
// становятся.
func (e *Engine) SetUsagePolicy(ctx context.Context, projectKey string, actor uuid.UUID, in UsagePolicyInput) (UsagePolicyChange, error) {
	var out UsagePolicyChange
	if in.Mode == nil && !in.SetGrace {
		return out, apperr.InvalidInput("нечего менять: укажите mode (off, required) или grace_minutes")
	}
	if in.Mode != nil && *in.Mode != domain.UsagePolicyOff && *in.Mode != domain.UsagePolicyRequired {
		return out, apperr.InvalidInput("mode: %q — допустимо off или required", *in.Mode)
	}
	if in.SetGrace && in.Grace != nil && (*in.Grace < 0 || *in.Grace > domain.MaxUsageGraceMinutes) {
		return out, apperr.InvalidInput("grace_minutes: %d — "+usageGraceHint,
			*in.Grace, domain.MaxUsageGraceMinutes, domain.DefaultUsageGraceMinutes)
	}
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		who, err := e.store.GetPrincipal(ctx, tx, actor)
		if err != nil {
			return err
		}
		if who.Kind != domain.PrincipalUser {
			return apperr.Forbidden("политику учёта проекта меняет человек — администратор проекта или системы; "+
				"принципал вида %s её не меняет", who.Kind)
		}
		project, err := e.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		before, err := e.store.ProjectUsagePolicy(ctx, tx, project.ID, true)
		if err != nil {
			return err
		}
		next := before
		if in.Mode != nil {
			next.Mode = *in.Mode
		}
		if in.SetGrace {
			next.GraceMinutes = in.Grace
		}
		stamp := false
		switch {
		case !next.Required():
			next.EnabledAt = nil
		case !before.Required():
			stamp = true
		}
		after, err := e.store.SetProjectUsagePolicy(ctx, tx, project.ID, next, stamp)
		if err != nil {
			return err
		}
		out = UsagePolicyChange{
			Before: usagePolicyView(project.Key, before),
			After:  usagePolicyView(project.Key, after),
		}
		return nil
	})
	return out, err
}

// usagePolicyFor — политика проекта для захвата и признак, что она
// действует на этого принципала (вид agent).
func (e *Engine) usagePolicyFor(ctx context.Context, tx pgx.Tx, projectID, actor uuid.UUID) (domain.UsagePolicy, bool, error) {
	policy, err := e.store.ProjectUsagePolicy(ctx, tx, projectID, false)
	if err != nil || !policy.Required() {
		return policy, false, err
	}
	who, err := e.store.GetPrincipal(ctx, tx, actor)
	if err != nil {
		return policy, false, err
	}
	return policy, who.Kind == domain.PrincipalAgent, nil
}

// usageAllows отбивает новую аренду агентскому принципалу с долгом по учёту:
// прогоном, который он забирал, закрытым после включения политики и раньше
// окна, без единой строки учёта. Подтверждение живой аренды сюда не доходит
// — начатое не обрывается, как и при исчерпанном бюджете.
func (e *Engine) usageAllows(ctx context.Context, tx pgx.Tx, task domain.Task, actor uuid.UUID, policy domain.UsagePolicy) error {
	if policy.EnabledAt == nil {
		return nil
	}
	debts, total, err := e.store.UsageDebts(ctx, tx, task.ProjectID, actor, *policy.EnabledAt, policy.Grace(), usageDebtsShown)
	if err != nil || total == 0 {
		return err
	}
	return usageRefusal(task.Key, debts, total, policy.Grace())
}

func usageRefusal(taskKey string, debts []postgres.UsageDebt, total, grace int) error {
	var list bytes.Buffer
	for i, d := range debts {
		if i > 0 {
			list.WriteString(", ")
		}
		fmt.Fprintf(&list, "%s (прогон %s)", d.TaskKey, d.StageRunID)
	}
	if total > len(debts) {
		fmt.Fprintf(&list, " и ещё %d", total-len(debts))
	}
	first := debts[0]
	return apperr.New(apperr.CodeUsageReportRequired,
		"захват %s отбит политикой учёта проекта: у этого агентского принципала %d закрытых прогонов без отчёта о стоимости — %s. "+
			"Отчитайтесь о каждом API-токеном того же принципала: clab usage %s --run %s --model <полный id модели> [--cost <доллары>]; "+
			"строка без --cost тоже гасит долг. Политика действует только на токены агентов; снять её может администратор проекта",
		taskKey, total, list.String(), first.TaskKey, first.StageRunID).
		WithDetails(map[string]any{
			"debts":         debts,
			"total":         total,
			"grace_minutes": grace,
		})
}
