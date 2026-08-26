package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect поднимает пул и дожидается готовности базы: в docker-compose
// приложение стартует раньше, чем Postgres примет соединения, и healthcheck
// зависимости покрывает не все случаи (например, рестарт базы под нагрузкой).
func Connect(ctx context.Context, url string, maxConns int32, wait time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("разбор SDLC_DATABASE_URL: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("создание пула: %w", err)
	}

	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		if err := pool.Ping(ctx); err == nil {
			return pool, nil
		} else {
			lastErr = err
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("база не ответила за %s: %w", wait, lastErr)
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
