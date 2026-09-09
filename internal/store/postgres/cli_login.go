package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Запросы входа CLI (решение 0032). Строка живёт минуты: от «clab auth login»
// до подтверждения в браузере и первого удачного опроса.

type CreateCLILoginInput struct {
	UserCode  string
	PollHash  string
	Label     string
	TokenDays int
	ExpiresAt time.Time
}

func (s *Store) CreateCLILogin(ctx context.Context, q Querier, in CreateCLILoginInput) (domain.CLILoginRequest, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO cli_login_requests (user_code, poll_hash, label, token_days, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		in.UserCode, in.PollHash, in.Label, in.TokenDays, in.ExpiresAt).Scan(&id)
	if err != nil {
		return domain.CLILoginRequest{}, translate(err)
	}
	return s.cliLogin(ctx, q, `r.id = $1`, id)
}

// CLILoginByCode — запрос по коду, который показал CLI. Протухший запрос
// отдаётся: страница подтверждения должна сказать «код устарел», а не
// «не найден».
func (s *Store) CLILoginByCode(ctx context.Context, q Querier, code string) (domain.CLILoginRequest, error) {
	return s.cliLogin(ctx, q, `r.user_code = $1`, code)
}

// CLILoginByPollHash — запрос по хешу секрета, который держит CLI.
func (s *Store) CLILoginByPollHash(ctx context.Context, q Querier, hash string) (domain.CLILoginRequest, error) {
	return s.cliLogin(ctx, q, `r.poll_hash = $1`, hash)
}

func (s *Store) cliLogin(ctx context.Context, q Querier, where string, arg any) (domain.CLILoginRequest, error) {
	var r domain.CLILoginRequest
	err := q.QueryRow(ctx, `
		SELECT r.id, r.user_code, r.label, r.token_days, r.created_at, r.expires_at,
		       r.approved_by, p.handle, p.kind, r.approved_at, r.denied_at, r.consumed_at
		FROM cli_login_requests r
		LEFT JOIN principals p ON p.id = r.approved_by
		WHERE `+where, arg,
	).Scan(&r.ID, &r.UserCode, &r.Label, &r.TokenDays, &r.CreatedAt, &r.ExpiresAt,
		&r.ApprovedBy, &r.ApprovedHandle, &r.ApprovedKind, &r.ApprovedAt, &r.DeniedAt, &r.ConsumedAt)
	if err != nil {
		return r, notFound(err, "запрос входа не найден: код неверный или уже использован")
	}
	return r, nil
}

// ApproveCLILogin отмечает запрос подтверждённым. Условие в WHERE — защита
// от гонки двух вкладок: подтвердить можно ровно один раз.
func (s *Store) ApproveCLILogin(ctx context.Context, q Querier, id, principalID uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		UPDATE cli_login_requests SET approved_by = $2, approved_at = now()
		WHERE id = $1 AND approved_at IS NULL AND denied_at IS NULL AND expires_at > now()`,
		id, principalID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("запрос входа уже закрыт или устарел")
	}
	return nil
}

func (s *Store) DenyCLILogin(ctx context.Context, q Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		UPDATE cli_login_requests SET denied_at = now()
		WHERE id = $1 AND approved_at IS NULL AND denied_at IS NULL`, id)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("запрос входа уже закрыт")
	}
	return nil
}

// ConsumeCLILogin закрывает запрос после выдачи токена. Возвращает false,
// если его уже закрыл параллельный опрос: токен тогда выдавать нельзя.
func (s *Store) ConsumeCLILogin(ctx context.Context, q Querier, id uuid.UUID) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE cli_login_requests SET consumed_at = now()
		WHERE id = $1 AND approved_at IS NOT NULL AND consumed_at IS NULL`, id)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

// PurgeCLILogins убирает закрытые и протухшие запросы. Вызывается при
// создании нового: отдельного задания воркера такая мелочь не стоит.
func (s *Store) PurgeCLILogins(ctx context.Context, q Querier, olderThan time.Duration) error {
	_, err := q.Exec(ctx, `
		DELETE FROM cli_login_requests
		WHERE expires_at < now() - make_interval(secs => $1)
		   OR consumed_at < now() - make_interval(secs => $1)
		   OR denied_at < now() - make_interval(secs => $1)`, olderThan.Seconds())
	return translate(err)
}
