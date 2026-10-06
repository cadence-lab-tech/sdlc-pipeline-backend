// Команда worker — фоновые задания, которые никто не дёргает запросом.
//
// Заданий пять: возврат задач с протухшей арендой, инкрементальная
// индексация репозиториев с детектором дрейфа сразу после неё, синхронизация
// состояния привязанных PR, векторизация артефактов и сборка мусора в блобах.
//
// Все они идут последовательно в одном цикле: воркер один, и параллелить
// внутри него нечего — узкое место у всех заданий одно и то же, чужой API.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/indexer"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// systemActor — принципал, от имени которого воркер пишет в ленту задачи.
// Аудит ссылается на principals, а не на строки, поэтому у фонового процесса
// тоже есть своё лицо.
const systemActor = "sdlc-worker"

func main() {
	if err := run(); err != nil {
		slog.Error("воркер остановлен с ошибкой", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, cfg.MaxConns, cfg.ConnectTimeout)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := postgres.New(pool)
	// То же хранилище, что у api: документы и артефакты, вынесенные сервисом,
	// воркер читает при векторизации.
	blobs, err := blobstore.Open(cfg.BlobS3, cfg.BlobDir)
	if err != nil {
		return err
	}
	// Пределы передаются и без хранилища: SDLC_BLOB_MAX_BYTES действует на
	// любом пути, в том числе когда всё лежит inline.
	if blobs != nil {
		store.UseBlobStore(blobs, cfg.BlobInlineMaxBytes, cfg.BlobMaxBytes)
	} else {
		store.UseBlobStore(nil, cfg.BlobInlineMaxBytes, cfg.BlobMaxBytes)
	}
	// Миграции катит api: два процесса, накатывающие схему, разошлись бы в
	// порядке старта. Воркер работает по уже готовой схеме.
	embedder := newEmbedder(cfg, log)
	engine := workflow.NewEngine(store, log, workflow.Options{
		StrictGates:       cfg.StrictGates,
		LeaseTTL:          cfg.LeaseTTL,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Embedder:          embedder,
	})

	system, err := store.ResolveActor(ctx, store.DB(), systemActor, domain.PrincipalSystem)
	if err != nil {
		return err
	}

	// Учётные данные репозитория выбираются от частного к общему: секрет и
	// установка репозитория, установка и секрет проекта, общий токен
	// (решение 0034, github.CredentialRefs.Resolve).
	app, err := github.LoadApp(cfg.GitHubAPIURL, cfg.GitHubAppID, cfg.GitHubAppKeyFile)
	if err != nil {
		return err
	}
	provider := github.NewProvider(github.New(cfg.GitHubAPIURL, cfg.GitHubToken), app, github.SecretFromEnv)

	ix := indexer.New(store,
		indexer.NewGitHubSource(provider),
		embedder, log, indexer.Options{
			MaxFileBytes:    cfg.IndexMaxFileBytes,
			MaxFiles:        cfg.IndexMaxFiles,
			ChunkChars:      cfg.ChunkMaxChars,
			EmbedBatch:      cfg.EmbeddingBatch,
			CacheTouchAfter: cfg.EmbeddingCacheTouchAfter,
			StuckAfter:      cfg.IndexStuckAfter,
		})

	log.Info("воркер запущен",
		"lease_sweep_interval", cfg.LeaseSweepInterval, "lease_ttl", cfg.LeaseTTL,
		"index_enabled", cfg.IndexEnabled, "index_interval", cfg.IndexInterval,
		"embeddings", cfg.EmbeddingProvider,
		"blob_gc_enabled", cfg.BlobGCEnabled, "blob_gc_interval", cfg.BlobGCInterval,
		"blob_gc_dry_run", cfg.BlobGCDryRun,
		"embedding_cache_gc_enabled", cfg.EmbeddingCacheGCEnabled,
		"embedding_cache_gc_interval", cfg.EmbeddingCacheGCInterval,
		"embedding_cache_gc_dry_run", cfg.EmbeddingCacheGCDryRun)

	leaseTicker := time.NewTicker(cfg.LeaseSweepInterval)
	defer leaseTicker.Stop()
	indexTicker := time.NewTicker(cfg.IndexInterval)
	defer indexTicker.Stop()
	// Очередь прогонов опрашивается чаще, чем головы веток: вебхук кладёт
	// коммит в очередь сразу, и ждать полного интервала до него незачем.
	queueTicker := time.NewTicker(cfg.IndexPollInterval)
	defer queueTicker.Stop()
	// Сборка мусора идёт реже всех: место освобождается не срочно, а каждый
	// проход — это поход в объектное хранилище на каждый удалённый блоб.
	// Выключенная не заводит тикера вовсе: nil-канал в select молчит всегда,
	// и до базы дело не доходит ни разу.
	var blobGC <-chan time.Time
	if cfg.BlobGCEnabled {
		blobGCTicker := time.NewTicker(cfg.BlobGCInterval)
		defer blobGCTicker.Stop()
		blobGC = blobGCTicker.C
	}
	// Вытеснение кэша эмбеддингов идёт своим периодом, а не вместе с блобами:
	// это чистый SQL по одной таблице, а сборщик блобов ходит во внешнее
	// хранилище на каждый блоб — общий период был бы компромиссом без причины.
	// Выключенное тикера не заводит: nil-канал в select молчит всегда.
	var embedGC <-chan time.Time
	if cfg.EmbeddingCacheGCEnabled {
		embedGCTicker := time.NewTicker(cfg.EmbeddingCacheGCInterval)
		defer embedGCTicker.Stop()
		embedGC = embedGCTicker.C
	}

	sweepLeases(ctx, engine, log, system.ID)
	if cfg.IndexEnabled {
		indexPass(ctx, ix, engine, log, system.ID, cfg.IndexRetainRuns)
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("получен сигнал, останавливаемся")
			return nil
		case <-leaseTicker.C:
			sweepLeases(ctx, engine, log, system.ID)
		case <-indexTicker.C:
			if cfg.IndexEnabled {
				indexPass(ctx, ix, engine, log, system.ID, cfg.IndexRetainRuns)
			}
		case <-queueTicker.C:
			if cfg.IndexEnabled {
				drainQueue(ctx, ix, engine, log, system.ID)
			}
		case <-blobGC:
			collectBlobs(ctx, store, log, cfg)
		case <-embedGC:
			evictEmbeddings(ctx, store, log, cfg)
		}
	}
}

// drainQueue исполняет то, что уже стоит в очереди, не опрашивая провайдера:
// это путь вебхука к индексу и детектору дрейфа между полными проходами.
func drainQueue(ctx context.Context, ix *indexer.Indexer, engine *workflow.Engine, log *slog.Logger, system uuid.UUID) {
	if runIndexRuns(ctx, ix, engine, log, system) > 0 {
		if n, err := ix.SyncRepoRefs(ctx, 50); err != nil {
			logJobError(ctx, log, "синхронизация PR", err)
		} else if n > 0 {
			log.Info("состояние PR обновлено", "count", n)
		}
	}
}

// runIndexRuns исполняет прогоны из очереди и сразу за каждым гоняет
// детектор дрейфа: задача, проехавшая мимо расхождения, узнает о нём на
// следующем этапе, когда чинить дороже.
func runIndexRuns(ctx context.Context, ix *indexer.Indexer, engine *workflow.Engine, log *slog.Logger, system uuid.UUID) int {
	done := 0
	// Предел прогонов за проход: репозиториев может быть много, а следующий
	// тик всё равно наступит — лучше вернуться к арендам, чем зависнуть.
	for i := 0; i < maxRunsPerPass; i++ {
		run, err := ix.RunNext(ctx)
		if err != nil {
			logJobError(ctx, log, "прогон индексации", err)
			break
		}
		if run == nil {
			break
		}
		done++
		signals, err := engine.DetectDrift(ctx, *run, system)
		if err != nil {
			logJobError(ctx, log, "детектор дрейфа", err)
			continue
		}
		if len(signals) > 0 {
			log.Info("заведены сигналы дрейфа",
				"repository", run.Repository, "commit", run.CommitSHA[:min(8, len(run.CommitSHA))],
				"count", len(signals))
		}
	}
	return done
}

// collectBlobs освобождает место под блобами, на которые не осталось ссылок.
//
// Первого прохода на старте нет намеренно, в отличие от аренд и индексации:
// освобождение места не срочно, а сразу после выкладки лучше дать инстансу
// подняться. Ошибка прохода воркер не роняет — база могла быть недоступна
// секунду, а следующий тик всё равно наступит.
func collectBlobs(ctx context.Context, store *postgres.Store, log *slog.Logger, cfg config.Config) {
	stats, err := store.CollectBlobs(ctx, postgres.BlobGCOptions{
		Limit:  cfg.BlobGCLimit,
		MinAge: cfg.BlobGCMinAge,
		DryRun: cfg.BlobGCDryRun,
	}, func(sha string, err error) {
		// Строка уже удалена, а объект остался: это утечка места, но не
		// потеря данных — ссылок на него нет. В логе sha, чтобы было по чему
		// разбирать хранилище руками.
		log.Warn("объект остался в хранилище после удаления строки блоба",
			"sha256", sha, "error", err)
	})
	if err != nil {
		logJobError(ctx, log, "сборка мусора в блобах", err)
		return
	}
	if stats.Empty() {
		return
	}
	log.Info("сборка мусора в блобах",
		"blobs", stats.Deleted, "bytes", stats.Bytes(),
		"inline_bytes", stats.InlineBytes, "external_bytes", stats.ExternalBytes,
		"skipped", stats.Skipped, "busy", stats.Busy,
		"failed", stats.Failed, "dry_run", cfg.BlobGCDryRun)
}

// evictEmbeddings вытесняет из кэша эмбеддингов то, чем давно не
// пользовались, и то, что не помещается в границу размера.
//
// Кэш ключуется содержимым и владельца не имеет намеренно: ни удаление
// проекта, ни отвязка репозитория его не уменьшают, и без этого прохода он
// растёт монотонно. Первого прохода на старте нет — тот же довод, что у
// блобов: место освобождается не срочно. Ошибка прохода воркер не роняет.
func evictEmbeddings(ctx context.Context, store *postgres.Store, log *slog.Logger, cfg config.Config) {
	stats, err := store.EvictEmbeddingCache(ctx, postgres.EmbeddingCacheGCOptions{
		TTL:     cfg.EmbeddingCacheTTL,
		MaxRows: cfg.EmbeddingCacheMaxRows,
		Limit:   cfg.EmbeddingCacheGCLimit,
		DryRun:  cfg.EmbeddingCacheGCDryRun,
	})
	if err != nil {
		logJobError(ctx, log, "вытеснение кэша эмбеддингов", err)
		return
	}
	if stats.Empty() {
		return
	}
	log.Info("вытеснение кэша эмбеддингов",
		"rows_before", stats.RowsBefore, "bytes_before", stats.BytesBefore,
		"stale", stats.DeletedStale, "overflow", stats.DeletedOverflow,
		"dry_run", cfg.EmbeddingCacheGCDryRun)
}

// newEmbedder выбирает, кто считает векторы. Провайдера может не быть вовсе:
// тогда индекс строится без векторов, поиск живёт на полнотекстовой половине,
// а досчитать векторы можно потом — backfill для того и написан.
func newEmbedder(cfg config.Config, log *slog.Logger) embed.Embedder {
	switch cfg.EmbeddingProvider {
	case "voyage":
		return embed.NewVoyage(cfg.VoyageAPIURL, cfg.VoyageAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBatch)
	case "hash":
		log.Warn("эмбеддинги считает заглушка: это мешок слов, а не семантика")
		return embed.Hash{}
	default:
		return nil
	}
}

// indexPass — полный проход по всему, что приезжает из провайдера и уезжает
// в индекс.
//
// Порядок неслучаен: сперва ставим в очередь новые коммиты, потом исполняем
// прогоны и сразу за каждым гоняем детектор дрейфа (задача, проехавшая мимо
// расхождения, узнает о нём на следующем этапе, когда чинить дороже), затем
// состояние PR для гейта external_check и в конце — векторизация артефактов.
func indexPass(ctx context.Context, ix *indexer.Indexer, engine *workflow.Engine, log *slog.Logger, system uuid.UUID, retain int) {
	if _, err := ix.Enqueue(ctx); err != nil {
		logJobError(ctx, log, "постановка прогонов индексации", err)
	}

	runIndexRuns(ctx, ix, engine, log, system)

	// Индекс на коммит неизменяем и растёт линейно по коммитам; старые
	// прогоны без ссылающихся базовых линий вычищаются.
	if n, err := ix.Prune(ctx, retain); err != nil {
		logJobError(ctx, log, "вычистка старых прогонов", err)
	} else if n > 0 {
		log.Info("старые прогоны индексации удалены", "count", n, "retain_per_branch", retain)
	}

	if n, err := ix.SyncRepoRefs(ctx, 50); err != nil {
		logJobError(ctx, log, "синхронизация PR", err)
	} else if n > 0 {
		log.Info("состояние PR обновлено", "count", n)
	}

	if artifacts, documents, err := ix.IndexKnowledge(ctx, 50); err != nil {
		logJobError(ctx, log, "векторизация артефактов", err)
	} else if artifacts+documents > 0 {
		log.Info("артефакты и документы разложены на куски",
			"artifacts", artifacts, "documents", documents)
	}

	if n, err := ix.BackfillEmbeddings(ctx, 100); err != nil {
		logJobError(ctx, log, "досчёт эмбеддингов", err)
	} else if n > 0 {
		log.Info("векторы досчитаны кускам, записанным без них", "count", n)
	}

	// Пересечение задач возникает не от движения main, а от того, что вторая
	// задача объявила ту же зону: ждать индексации для него незачем.
	if signals, err := engine.DetectOverlaps(ctx, system); err != nil {
		logJobError(ctx, log, "поиск пересечений задач", err)
	} else if len(signals) > 0 {
		log.Info("найдены пересечения зон задач", "count", len(signals))
	}
}

// maxRunsPerPass — сколько прогонов индексации исполняется за один проход.
const maxRunsPerPass = 20

// logJobError не роняет воркер: база или чужой API могли отвалиться на
// секунду, а следующий тик всё равно наступит.
func logJobError(ctx context.Context, log *slog.Logger, job string, err error) {
	if ctx.Err() != nil {
		return
	}
	log.Error(job+" не удалась", "error", err)
}

// sweepLeases возвращает в pending всё, чья аренда истекла.
//
// Ошибка прохода не роняет воркер: база могла быть недоступна секунду, а
// следующий тик всё равно наступит.
func sweepLeases(ctx context.Context, engine *workflow.Engine, log *slog.Logger, system uuid.UUID) {
	n, err := engine.ExpireLeases(ctx, system, 100)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error("проход по протухшим арендам не удался", "error", err)
		return
	}
	if n > 0 {
		log.Info("аренды истекли, задачи возвращены в общий доступ", "count", n)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
