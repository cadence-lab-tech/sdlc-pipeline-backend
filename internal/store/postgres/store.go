// Package postgres — доступ к данным поверх pgx без ORM.
//
// Методы принимают Querier, а не работают со скрытым внутри пулом: движок
// переходов обязан прогонять гейты и переводить задачу одной транзакцией, и
// те же самые запросы должны уметь исполняться внутри неё.
package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Querier покрывает и пул, и транзакцию.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool *pgxpool.Pool
	// Внешнее хранилище крупного содержимого; nil — всё лежит inline.
	blobs     BlobStore
	inlineMax int
	blobMax   int
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) DB() Querier { return s.pool }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// InTx прогоняет fn в транзакции.
func (s *Store) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return runInTx(ctx, s.pool, fn)
}

// beginner — то, на чём можно начать транзакцию: и пул, и отдельное
// соединение из него. Соединение нужно там, где транзакция обязана идти в той
// же сессии, что и взятый до неё сессионный замок (см. withBlobLock).
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// runInTx прогоняет fn в транзакции. Откат при панике обязателен: без него
// соединение вернётся в пул с открытой транзакцией и отравит следующего.
func runInTx(ctx context.Context, b beginner, fn func(tx pgx.Tx) error) error {
	tx, err := b.Begin(ctx)
	if err != nil {
		return apperr.Internal(err, "начало транзакции")
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return translate(err)
	}
	return nil
}

// ResolveActor находит принципала по хэндлу или заводит нового.
//
// Аутентификации пока нет (решение 0010), но аудит уже ссылается на principals,
// а не на строки — поэтому актор материализуется в таблице с первого обращения,
// и включение auth в фазе 2 не потребует миграции данных.
func (s *Store) ResolveActor(ctx context.Context, q Querier, handle string, kind domain.PrincipalKind) (domain.Principal, error) {
	var p domain.Principal
	err := q.QueryRow(ctx, `
		INSERT INTO principals (kind, handle, display_name)
		VALUES ($1, $2, $2)
		ON CONFLICT (handle) DO UPDATE SET handle = EXCLUDED.handle
		RETURNING id, kind, handle, display_name, email, is_admin, created_at`,
		kind, handle,
	).Scan(&p.ID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.IsAdmin, &p.CreatedAt)
	if err != nil {
		return p, apperr.Internal(err, "резолв актора %q", handle)
	}
	return p, nil
}

// translate превращает ошибки драйвера в ошибки домена, чтобы наружу не
// уезжали формулировки Postgres, а нарушения инвариантов схемы читались.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound("не найдено")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return apperr.Internal(err, "ошибка базы")
	}

	switch pgErr.Code {
	case "23505": // unique_violation
		return apperr.Conflict("нарушена уникальность: %s", pgErr.ConstraintName)
	case "23503": // foreign_key_violation
		return apperr.InvalidInput("ссылка на несуществующую запись: %s", pgErr.ConstraintName)
	case "23514": // check_violation
		// Сюда же попадает enforce_task_repo_scope(): отложенный триггер
		// поднимает check_violation с человекочитаемым текстом.
		return apperr.InvalidInput("%s", pgErr.Message)
	default:
		return apperr.Internal(err, "ошибка базы (%s)", pgErr.Code)
	}
}

func notFound(err error, format string, args ...any) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound(format, args...)
	}
	return translate(err)
}

// splitTaskKey разбирает SDLC-42 на ключ проекта и ключ задачи. Ключ задачи
// уникален только внутри проекта, а в URL приезжает один — префикс и есть
// проект, потому что ключи выдаются как <PROJECT>-<seq>.
func splitTaskKey(key string) (projectKey string, err error) {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '-' {
			if i == 0 || i == len(key)-1 {
				break
			}
			return key[:i], nil
		}
	}
	return "", apperr.InvalidInput("ключ задачи %q не в формате PROJECT-42", key)
}

func uuidPtr(u uuid.UUID) *uuid.UUID { return &u }

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
