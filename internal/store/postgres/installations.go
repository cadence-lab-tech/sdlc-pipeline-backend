package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Установки GitHub App на арендаторе (решение 0034). Секретов здесь нет:
// таблица помнит, чья установка и жива ли она, а токен по ней выписывает
// ключ приложения в памяти процесса.

const installationCols = `id, installation_id, account_login, account_id, account_type,
	owner_team_id,
	(SELECT t.key FROM teams t WHERE t.id = github_installations.owner_team_id) AS owner_team_key,
	connected_by, created_at, updated_at, suspended_at, removed_at`

func scanInstallation(row interface{ Scan(...any) error }) (domain.GitHubInstallation, error) {
	var i domain.GitHubInstallation
	err := row.Scan(&i.ID, &i.InstallationID, &i.AccountLogin, &i.AccountID, &i.AccountType,
		&i.OwnerTeamID, &i.OwnerTeamKey, &i.ConnectedBy, &i.CreatedAt, &i.UpdatedAt,
		&i.SuspendedAt, &i.RemovedAt)
	return i, err
}

type InstallationInput struct {
	InstallationID int64
	AccountLogin   string
	AccountID      int64
	AccountType    string
	OwnerTeamID    uuid.UUID
	ConnectedBy    uuid.UUID
}

// UpsertInstallation записывает установку на команду. Повторное подключение
// той же установки той же командой обновляет аккаунт и оживляет снятую;
// установка, уже записанная на другую команду, не переписывается —
// идентификаторы установок идут подряд, и приписать себе чужую было бы
// способом читать чужие репозитории.
func (s *Store) UpsertInstallation(ctx context.Context, q Querier, in InstallationInput) (domain.GitHubInstallation, error) {
	i, err := scanInstallation(q.QueryRow(ctx, `
		INSERT INTO github_installations
		    (installation_id, account_login, account_id, account_type, owner_team_id, connected_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (installation_id) DO UPDATE SET
		    account_login = EXCLUDED.account_login,
		    account_id    = EXCLUDED.account_id,
		    account_type  = EXCLUDED.account_type,
		    connected_by  = EXCLUDED.connected_by,
		    suspended_at  = NULL,
		    removed_at    = NULL,
		    updated_at    = now()
		WHERE github_installations.owner_team_id = EXCLUDED.owner_team_id
		RETURNING `+installationCols,
		in.InstallationID, in.AccountLogin, in.AccountID, in.AccountType, in.OwnerTeamID, in.ConnectedBy))
	if err != nil {
		if isNoRows(err) {
			return i, apperr.Conflict("установка %d уже подключена к другой команде", in.InstallationID)
		}
		return i, translate(err)
	}
	return i, nil
}

func (s *Store) ListInstallations(ctx context.Context, q Querier, teamID uuid.UUID) ([]domain.GitHubInstallation, error) {
	rows, err := q.Query(ctx, `SELECT `+installationCols+` FROM github_installations
		WHERE owner_team_id = $1 ORDER BY account_login`, teamID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.GitHubInstallation{}
	for rows.Next() {
		i, err := scanInstallation(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, i)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetInstallation(ctx context.Context, q Querier, installationID int64) (domain.GitHubInstallation, error) {
	i, err := scanInstallation(q.QueryRow(ctx, `SELECT `+installationCols+`
		FROM github_installations WHERE installation_id = $1`, installationID))
	if err != nil {
		return i, notFound(err, "установка %d не подключена", installationID)
	}
	return i, nil
}

// Состояния установки, о которых сообщает GitHub.
const (
	InstallationSuspended   = "suspended"
	InstallationUnsuspended = "unsuspended"
	InstallationRemoved     = "removed"
)

// SetInstallationState отмечает событие GitHub на установке. Строка не
// удаляется: отзыв по ошибке обычен, а переиндексация с нуля стоит денег.
// Неизвестная установка — ноль строк, не ошибка: приложение приносит события
// всех своих установок, в том числе тех, что никто не подключал.
func (s *Store) SetInstallationState(ctx context.Context, q Querier, installationID int64, state string) (int, error) {
	var set string
	switch state {
	case InstallationSuspended:
		set = `suspended_at = coalesce(suspended_at, now())`
	case InstallationUnsuspended:
		set = `suspended_at = NULL`
	case InstallationRemoved:
		set = `removed_at = coalesce(removed_at, now())`
	default:
		return 0, apperr.InvalidInput("состояние установки %q неизвестно", state)
	}
	tag, err := q.Exec(ctx, `UPDATE github_installations SET `+set+`, updated_at = now()
		WHERE installation_id = $1`, installationID)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

// SetRepositoryAvailability помечает репозиторий установки недоступным или
// снова доступным: GitHub сообщает, что его убрали из установки или вернули.
func (s *Store) SetRepositoryAvailability(ctx context.Context, q Querier, installationID int64, owner, name string, available bool) (int, error) {
	var at *time.Time
	if !available {
		now := time.Now()
		at = &now
	}
	tag, err := q.Exec(ctx, `UPDATE repositories SET unavailable_at = $4
		WHERE installation_id = $1 AND lower(owner) = lower($2) AND lower(name) = lower($3)`,
		installationID, owner, name, at)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}
