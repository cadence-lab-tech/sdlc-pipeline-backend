package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// BlobRef — строка blobs глазами сборщика мусора: что удалять и сколько места
// это освободит.
type BlobRef struct {
	SHA256      string
	SizeBytes   int64
	Storage     string
	ExternalURI string
}

// blobRefsExist — три ссылки на blobs(sha256), какие есть в схеме, и других
// нет. Считаются по всему инстансу, без project_id и team_id: блобы
// разделяются между арендаторами намеренно (docs/SAAS.md §8), и осиротевший у
// одного может быть живым у другого. Появится четвёртая ссылка — её сюда, и
// проверить, что под неё есть индекс по blob_sha (миграция 000022).
const blobRefsExist = `
	    NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_sha = b.sha256)
	AND NOT EXISTS (SELECT 1 FROM code_files f WHERE f.blob_sha = b.sha256)
	AND NOT EXISTS (SELECT 1 FROM project_documents d WHERE d.blob_sha = b.sha256)`

// blobLockKey — ключ advisory-замка по sha, подставляется в запрос выражением
// над $1: sha обязан быть первым параметром. Первые 16 шестнадцатеричных цифр
// содержимого — sha равномерен, поэтому столкновений не больше, чем у любого
// другого усечения.
const blobLockKey = `('x' || substr($1::text, 1, 16))::bit(64)::bigint`

// UnreferencedBlobs отдаёт кандидатов на удаление: блобы старше minAge, на
// которые не ссылается ни артефакт, ни файл прогона индексации, ни документ
// проекта.
//
// Минимальный возраст — не косметика: только что записанный блоб может ждать
// ссылки из ещё не закоммиченной транзакции, и сутки запаса стоят дешевле
// потерянного содержимого. Старые идут первыми: место освобождает та часть
// хвоста, которая уже точно никому не нужна.
func (s *Store) UnreferencedBlobs(ctx context.Context, q Querier, limit int, minAge time.Duration) ([]BlobRef, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT b.sha256, b.size_bytes, b.storage, coalesce(b.external_uri, '')
		FROM blobs b
		WHERE b.created_at < now() - $2::interval
		  AND`+blobRefsExist+`
		ORDER BY b.created_at
		LIMIT $1`, limit, minAge)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []BlobRef
	for rows.Next() {
		var b BlobRef
		if err := rows.Scan(&b.SHA256, &b.SizeBytes, &b.Storage, &b.ExternalURI); err != nil {
			return nil, translate(err)
		}
		out = append(out, b)
	}
	return out, translate(rows.Err())
}

// DeleteBlobIfUnreferenced удаляет строку блоба, ещё раз проверив отсутствие
// всех трёх ссылок в том же запросе: между выборкой кандидата и удалением
// ссылка могла появиться, и без перепроверки внешний ключ уронил бы проход
// целиком. false без ошибки — ссылка появилась или строки уже нет.
//
// Перед удалением берётся advisory-замок по sha, тот же, что берёт PutBlob:
// он держится до конца транзакции и закрывает окно между удалением строки и
// удалением объекта из хранилища. Без него параллельная запись того же
// содержимого увидела бы объект на месте (S3.Put пропускает заливку по head,
// FS.Put — по os.Stat), не залила бы его заново, вставила бы строку — и эту
// строку сборщик оставил бы без содержимого.
func (s *Store) DeleteBlobIfUnreferenced(ctx context.Context, q Querier, sha string, minAge time.Duration) (BlobRef, bool, error) {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(`+blobLockKey+`)`, sha); err != nil {
		return BlobRef{}, false, translate(err)
	}
	b := BlobRef{SHA256: sha}
	err := q.QueryRow(ctx, `
		DELETE FROM blobs b
		WHERE b.sha256 = $1
		  AND b.created_at < now() - $2::interval
		  AND`+blobRefsExist+`
		RETURNING b.size_bytes, b.storage, coalesce(b.external_uri, '')`,
		sha, minAge,
	).Scan(&b.SizeBytes, &b.Storage, &b.ExternalURI)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return BlobRef{}, false, nil
	case err != nil:
		return BlobRef{}, false, translate(err)
	}
	return b, true, nil
}

// lockBlob берёт тот же advisory-замок, что и сборщик мусора, до конца
// транзакции. Зовёт его запись содержимого: пока пишущая транзакция не
// закоммичена, сборщик не тронет ни строку, ни объект.
func (s *Store) lockBlob(ctx context.Context, q Querier, sha string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(`+blobLockKey+`)`, sha); err != nil {
		return translate(err)
	}
	return nil
}

// BlobGCOptions — настройки одного прохода сборщика.
type BlobGCOptions struct {
	// Limit — сколько блобов удалять за проход. Проход не должен затягиваться:
	// следующий тик всё равно наступит.
	Limit int
	// MinAge — насколько старым должен быть кандидат.
	MinAge time.Duration
	// DryRun — считать и не удалять. Оператор смотрит в лог перед включением.
	DryRun bool
}

// BlobGCStats — что дал проход. Отдельного слоя метрик в сервисе нет, поэтому
// цифры возвращаются вызывающему и уходят полями структурированного лога.
type BlobGCStats struct {
	// Deleted — удалено строк blobs (в сухом прогоне — сколько удалилось бы).
	Deleted int
	// InlineBytes и ExternalBytes — освобождённое место по видам хранения:
	// первое разгружает базу, второе — хранилище, и это разные счета.
	InlineBytes   int64
	ExternalBytes int64
	// Skipped — кандидат обзавёлся ссылкой между выборкой и удалением.
	Skipped int
	// Failed — строка удалена, а объект в хранилище убрать не вышло. Это
	// утечка места, но не потеря данных: ссылок на объект уже нет.
	Failed int
}

// Empty — проходу нечего сказать в лог.
func (s BlobGCStats) Empty() bool {
	return s.Deleted == 0 && s.Skipped == 0 && s.Failed == 0
}

// Bytes — сколько места освободил проход всего.
func (s BlobGCStats) Bytes() int64 { return s.InlineBytes + s.ExternalBytes }

// errNoBlobStore — внешнее хранилище не настроено в этом процессе, а блоб
// вынесен в него: удалять объект нечем.
var errNoBlobStore = errors.New("внешнее хранилище не настроено")

// CollectBlobs — проход сборщика мусора по блобам без ссылок.
//
// Порядок принципиален: сперва строка, потом объект в хранилище. Обратный
// порядок при падении между шагами оставил бы строку, пережившую своё
// содержимое, — битую ссылку навсегда: повторная запись того же содержимого
// не перезаливает объект (PutBlob делает ON CONFLICT DO NOTHING, S3.Put
// пропускает заливку по head). При выбранном порядке падение между шагами
// оставляет осиротевший объект: это утечка места на один блоб, и следующая
// запись того же содержимого объект переиспользует.
//
// Каждый блоб идёт своей транзакцией: проход на пятьсот блобов не должен
// держать одну транзакцию открытой на всё время походов в хранилище, а сбой
// на одном блобе не должен откатывать удалённые до него.
//
// onFail зовётся на каждый блоб, чей объект убрать не вышло: sha обязан
// доехать до лога вместе с причиной — только по нему хранилище разбирают
// руками. Может быть nil.
func (s *Store) CollectBlobs(ctx context.Context, opts BlobGCOptions, onFail func(sha string, err error)) (BlobGCStats, error) {
	var stats BlobGCStats
	if opts.Limit < 1 {
		return stats, nil
	}
	candidates, err := s.UnreferencedBlobs(ctx, s.DB(), opts.Limit, opts.MinAge)
	if err != nil {
		return stats, err
	}

	for _, c := range candidates {
		if ctx.Err() != nil {
			return stats, nil
		}
		if c.Storage == "external" && s.blobs == nil {
			// Удалить объект нечем: строка ушла бы, а содержимое осталось в
			// хранилище навсегда. Утечку, которую видно заранее, не делают —
			// блоб дождётся процесса с настроенным хранилищем.
			stats.Skipped++
			if onFail != nil {
				onFail(c.SHA256, errNoBlobStore)
			}
			continue
		}
		if opts.DryRun {
			// Сухой прогон считает ровно то же, что посчитал бы настоящий:
			// оператору нужна цифра, по которой он решает включать или нет.
			stats.count(c)
			continue
		}

		var deleted BlobRef
		var ok bool
		err := s.InTx(ctx, func(tx pgx.Tx) error {
			var err error
			deleted, ok, err = s.DeleteBlobIfUnreferenced(ctx, tx, c.SHA256, opts.MinAge)
			return err
		})
		if err != nil {
			return stats, err
		}
		if !ok {
			stats.Skipped++
			continue
		}
		stats.count(deleted)

		if deleted.Storage != "external" || deleted.ExternalURI == "" {
			continue
		}
		// Сбой удаления объекта строку не воскрешает: ссылок на неё уже нет,
		// а повторное появление строки без содержимого было бы хуже утечки.
		if err := s.blobs.Delete(ctx, deleted.ExternalURI); err != nil {
			stats.Failed++
			if onFail != nil {
				onFail(deleted.SHA256, err)
			}
		}
	}
	return stats, nil
}

func (s *BlobGCStats) count(b BlobRef) {
	s.Deleted++
	if b.Storage == "external" {
		s.ExternalBytes += b.SizeBytes
		return
	}
	s.InlineBytes += b.SizeBytes
}
