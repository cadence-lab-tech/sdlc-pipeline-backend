package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Арендатор — команда (решение 0033): личная команда человека, тариф и
// подписка. Квота считается по этим запросам рядом с правами, а не в домене.

// PersonalTeamOf — живая личная команда принципала.
func (s *Store) PersonalTeamOf(ctx context.Context, q Querier, principalID uuid.UUID) (domain.Team, error) {
	t, err := scanTeam(q.QueryRow(ctx, `SELECT `+teamCols+` FROM teams
		WHERE kind = 'personal' AND personal_of = $1 AND archived_at IS NULL`, principalID))
	if err != nil {
		return t, notFound(err, "личной команды нет")
	}
	return t, nil
}

// ConvertTeam превращает личную команду в организацию: вид меняется,
// проекты остаются. Пустые ключ и название — оставить прежние.
func (s *Store) ConvertTeam(ctx context.Context, q Querier, id uuid.UUID, key, name string) (domain.Team, error) {
	t, err := scanTeam(q.QueryRow(ctx, `
		UPDATE teams SET
			kind = 'organization',
			personal_of = NULL,
			key = coalesce(nullif($2, ''), key),
			name = coalesce(nullif($3, ''), name),
			updated_at = now()
		WHERE id = $1 AND kind = 'personal'
		RETURNING `+teamCols, id, key, name))
	if err != nil {
		if isNoRows(err) {
			return t, apperr.Conflict("это уже организация")
		}
		return t, translate(err)
	}
	return t, nil
}

func (s *Store) CountTeamMembers(ctx context.Context, q Querier, teamID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id = $1`, teamID).Scan(&n)
	return n, translate(err)
}

// ── тарифы ──────────────────────────────────────────────────────────────────

const planCols = `code, name, members_max, created_at`

func scanPlan(row interface{ Scan(...any) error }) (domain.Plan, error) {
	var p domain.Plan
	err := row.Scan(&p.Code, &p.Name, &p.MembersMax, &p.CreatedAt)
	return p, err
}

func (s *Store) ListPlans(ctx context.Context, q Querier) ([]domain.Plan, error) {
	rows, err := q.Query(ctx, `SELECT `+planCols+` FROM plans ORDER BY members_max NULLS LAST, code`)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, p)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetPlan(ctx context.Context, q Querier, code string) (domain.Plan, error) {
	p, err := scanPlan(q.QueryRow(ctx, `SELECT `+planCols+` FROM plans WHERE code = $1`, code))
	if err != nil {
		return p, notFound(err, "тариф %q не найден", code)
	}
	return p, nil
}

// PlanOf — действующий тариф команды: активная подписка, иначе free.
func (s *Store) PlanOf(ctx context.Context, q Querier, teamID uuid.UUID) (domain.Plan, error) {
	p, err := scanPlan(q.QueryRow(ctx, `SELECT `+planCols+` FROM plans
		WHERE code = coalesce(
			(SELECT plan_code FROM subscriptions WHERE team_id = $1 AND status = 'active'),
			'free')`, teamID))
	if err != nil {
		return p, translate(err)
	}
	return p, nil
}

// SetSubscription переводит команду на тариф: прежняя активная подписка
// закрывается, новая заводится. free — это отсутствие подписки, поэтому
// перевод на free только закрывает прежнюю.
func (s *Store) SetSubscription(ctx context.Context, q Querier, teamID uuid.UUID, planCode string, by uuid.UUID) error {
	if _, err := q.Exec(ctx, `
		UPDATE subscriptions SET status = 'canceled', period_end = now(), updated_at = now()
		WHERE team_id = $1 AND status = 'active'`, teamID); err != nil {
		return translate(err)
	}
	if planCode == "free" {
		return nil
	}
	_, err := q.Exec(ctx, `
		INSERT INTO subscriptions (team_id, plan_code, status, period_start, created_by)
		VALUES ($1, $2, 'active', now(), $3)`, teamID, planCode, by)
	return translate(err)
}
