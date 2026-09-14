package postgres

import (
	"context"
	"time"
)

// EmbeddingCacheGCOptions — настройки одного прохода вытеснения кэша
// эмбеддингов.
type EmbeddingCacheGCOptions struct {
	// TTL — сколько строка живёт без использования. Ноль выключает
	// вытеснение по сроку.
	TTL time.Duration
	// MaxRows — верхняя граница размера кэша в строках. Ноль выключает
	// вытеснение по границе.
	//
	// Граница в строках, а не в байтах: размер строки фиксирован
	// размерностью вектора, а байты на диске после DELETE не уменьшаются до
	// автовакуума — проход, гоняющийся за цифрой, которая не двигается,
	// вынес бы кэш целиком.
	MaxRows int
	// Limit — сколько строк удалять за проход. Проход не должен затягиваться:
	// следующий тик всё равно наступит.
	Limit int
	// DryRun — считать и не удалять. Оператор смотрит в лог, прежде чем
	// включить вытеснение на засиженной базе.
	DryRun bool
}

// EmbeddingCacheGCStats — что дал проход. Отдельного слоя метрик в сервисе
// нет, поэтому цифры возвращаются вызывающему и уходят полями
// структурированного лога.
type EmbeddingCacheGCStats struct {
	// RowsBefore и BytesBefore — размер кэша до прохода. Байты считаются с
	// TOAST и индексами (pg_total_relation_size): оператору важно место на
	// диске, а не размер одной кучи. Именно до прохода, а не освобождённое:
	// DELETE места на диске не возвращает до автовакуума, и цифра
	// «освобождено», как у сборщика блобов, здесь была бы выдумкой.
	RowsBefore  int
	BytesBefore int64
	// DeletedStale и DeletedOverflow — вытеснено по сроку и сверх границы (в
	// сухом прогоне — сколько вытеснилось бы). Причины разные: первая говорит,
	// что кэш остыл, вторая — что он не помещается.
	DeletedStale    int
	DeletedOverflow int
}

// Deleted — сколько строк ушло за проход всего.
func (s EmbeddingCacheGCStats) Deleted() int { return s.DeletedStale + s.DeletedOverflow }

// Empty — проходу нечего сказать в лог.
func (s EmbeddingCacheGCStats) Empty() bool { return s.Deleted() == 0 }

// EvictEmbeddingCache — проход вытеснения кэша эмбеддингов.
//
// Сначала по сроку, потом по границе: строка, которой давно не пользовались,
// уходит независимо от того, помещается кэш или нет, а граница разбирает уже
// то, что осталось. Оба запроса идут по индексу embedding_cache_last_used_at_idx
// от самых давних.
//
// Замков не берётся, в отличие от сборщика блобов: гонка «вытесняем строку,
// которую прямо сейчас читает прогон индексации» безопасна. Потеря строки
// кэша стоит одного вызова модели, а не битой ссылки, — advisory-замок здесь
// был бы платой ни за что.
func (s *Store) EvictEmbeddingCache(ctx context.Context, opts EmbeddingCacheGCOptions) (EmbeddingCacheGCStats, error) {
	var stats EmbeddingCacheGCStats
	if opts.Limit < 1 {
		return stats, nil
	}

	// count(*), а не reltuples: по этой цифре принимается решение о границе, а
	// оценка планировщика отстаёт ровно тогда, когда кэш быстро растёт. Раз в
	// несколько часов полный проход по таблице допустим.
	err := s.DB().QueryRow(ctx, `
		SELECT count(*), pg_total_relation_size('embedding_cache')
		FROM embedding_cache`).Scan(&stats.RowsBefore, &stats.BytesBefore)
	if err != nil {
		return stats, translate(err)
	}
	if stats.RowsBefore == 0 {
		return stats, nil
	}

	if opts.TTL > 0 {
		stats.DeletedStale, err = s.evictStale(ctx, opts, opts.Limit)
		if err != nil {
			return stats, err
		}
	}

	budget := opts.Limit - stats.DeletedStale
	excess := stats.RowsBefore - stats.DeletedStale - opts.MaxRows
	if opts.MaxRows <= 0 || budget <= 0 || excess <= 0 {
		return stats, nil
	}
	if excess < budget {
		budget = excess
	}
	stats.DeletedOverflow, err = s.evictOverflow(ctx, opts, budget, stats.DeletedStale)
	return stats, err
}

// evictStale уносит строки, которыми не пользовались дольше TTL.
func (s *Store) evictStale(ctx context.Context, opts EmbeddingCacheGCOptions, limit int) (int, error) {
	if opts.DryRun {
		var n int
		err := s.DB().QueryRow(ctx, `
			SELECT count(*) FROM (
			    SELECT 1 FROM embedding_cache
			    WHERE last_used_at < now() - $1::interval
			    ORDER BY last_used_at
			    LIMIT $2
			) t`, opts.TTL, limit).Scan(&n)
		return n, translate(err)
	}
	tag, err := s.DB().Exec(ctx, `
		DELETE FROM embedding_cache WHERE (model, text_sha) IN (
		    SELECT model, text_sha FROM embedding_cache
		    WHERE last_used_at < now() - $1::interval
		    ORDER BY last_used_at
		    LIMIT $2
		)`, opts.TTL, limit)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

// evictOverflow уносит самые давно не используемые строки сверх границы.
//
// skip — сколько строк унёс проход по сроку. В настоящем прогоне их уже нет и
// пропускать нечего, а в сухом они на месте, и без пропуска те же самые
// строки посчитались бы дважды: по сроку уходят ровно самые давние, то есть
// начало того же порядка.
func (s *Store) evictOverflow(ctx context.Context, opts EmbeddingCacheGCOptions, limit, skip int) (int, error) {
	if opts.DryRun {
		var n int
		err := s.DB().QueryRow(ctx, `
			SELECT count(*) FROM (
			    SELECT 1 FROM embedding_cache
			    ORDER BY last_used_at
			    OFFSET $2
			    LIMIT $1
			) t`, limit, skip).Scan(&n)
		return n, translate(err)
	}
	tag, err := s.DB().Exec(ctx, `
		DELETE FROM embedding_cache WHERE (model, text_sha) IN (
		    SELECT model, text_sha FROM embedding_cache
		    ORDER BY last_used_at
		    LIMIT $1
		)`, limit)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}
