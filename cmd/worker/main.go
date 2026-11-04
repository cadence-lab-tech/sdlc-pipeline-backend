// Команда worker — фоновые задания, которые никто не дёргает запросом.
//
// Задания: возврат задач с протухшей арендой, инкрементальная индексация
// репозиториев с детектором дрейфа сразу после неё, синхронизация состояния
// привязанных PR, опрос установок GitHub App, векторизация артефактов и
// уборка — блобы, кэш эмбеддингов, журнал действий.
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
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// systemActor — принципал, от имени которого воркер пишет в ленту задачи.
// Аудит ссылается на principals, а не на строки, поэтому у фонового процесса
// тоже есть своё лицо.
const systemActor = "sdlc-worker"

// version подставляется при сборке: -ldflags "-X main.version=…". Пишется в
// лог при старте — та же строка, что у api в /v1/instance.
var version = "dev"

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

	// Метрики эксплуатации (CLAB-126): у воркера это первый и единственный
	// слушатель, и только при заданном SDLC_METRICS_ADDR. Без него m == nil,
	// и учёт в заданиях ничего не делает.
	var m *metrics.Worker
	if cfg.MetricsAddr != "" {
		reg := metrics.New("worker", version)
		reg.Pool(pool)
		m = metrics.NewWorker(reg, workerJobs...)
		srv, err := metrics.Start(cfg.MetricsAddr, reg, log)
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}

	store := postgres.New(pool)
	// То же хранилище, что у api: документы и артефакты, вынесенные сервисом,
	// воркер читает при векторизации.
	blobs, err := blobstore.Open(ctx, cfg.BlobS3, cfg.BlobDir, log)
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
	// Модель не той размерности останавливает старт: векторы, которые
	// воркер посчитал бы ею, в схему vector(1024) всё равно не лягут.
	embedder, err := embed.New(ctx, cfg, log)
	if err != nil {
		return err
	}
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
			OnRunDone:       m.IndexRunFinished,
		})

	log.Info("воркер запущен", "version", version,
		"lease_sweep_interval", cfg.LeaseSweepInterval, "lease_ttl", cfg.LeaseTTL,
		"index_enabled", cfg.IndexEnabled, "index_interval", cfg.IndexInterval,
		"index_poll_interval", cfg.IndexPollInterval,
		"repo_ref_sync_interval", cfg.RepoRefSyncInterval,
		"installation_sync_enabled", provider.HasApp(),
		"installation_sync_interval", cfg.InstallationSyncInterval,
		"embeddings", cfg.EmbeddingProvider, "embedding_model", embeddingModel(embedder),
		"blob_gc_enabled", cfg.BlobGCEnabled, "blob_gc_interval", cfg.BlobGCInterval,
		"blob_gc_dry_run", cfg.BlobGCDryRun,
		"embedding_cache_gc_enabled", cfg.EmbeddingCacheGCEnabled,
		"embedding_cache_gc_interval", cfg.EmbeddingCacheGCInterval,
		"embedding_cache_gc_dry_run", cfg.EmbeddingCacheGCDryRun,
		"audit_retention_days", cfg.AuditRetentionDays)

	plan := newSchedule(cfg, provider.HasApp())
	leaseTick, stopLease := plan.ticker(plan.LeaseSweep)
	defer stopLease()
	indexTick, stopIndex := plan.ticker(plan.Index)
	defer stopIndex()
	// Очередь прогонов опрашивается чаще, чем головы веток: вебхук кладёт
	// коммит в очередь сразу, и ждать полного интервала до него незачем.
	queueTick, stopQueue := plan.ticker(plan.Queue)
	defer stopQueue()
	// Синхронизация PR — своим периодом (CLAB-395): задержку гейта
	// external_check без вебхука задаёт именно она, а полный проход
	// индексации тянет за собой Prune и досчёт эмбеддингов.
	refsTick, stopRefs := plan.ticker(plan.RepoRefSync)
	defer stopRefs()
	// Опрос установок — подстраховка вебхуков installation и
	// installation_repositories (CLAB-395); без приложения его нет.
	installTick, stopInstall := plan.ticker(plan.InstallationSync)
	defer stopInstall()
	// Сборка мусора идёт реже всех: место освобождается не срочно, а каждый
	// проход — это поход в объектное хранилище на каждый удалённый блоб.
	blobGC, stopBlobGC := plan.ticker(plan.BlobGC)
	defer stopBlobGC()
	// Вытеснение кэша эмбеддингов идёт своим периодом, а не вместе с блобами:
	// это чистый SQL по одной таблице, а сборщик блобов ходит во внешнее
	// хранилище на каждый блоб — общий период был бы компромиссом без причины.
	embedGC, stopEmbedGC := plan.ticker(plan.EmbedCacheGC)
	defer stopEmbedGC()
	// Уборка журнала действий по сроку (CLAB-210).
	auditGC, stopAuditGC := plan.ticker(plan.AuditGC)
	defer stopAuditGC()

	sweepLeases(ctx, engine, log, system.ID, m)
	refreshState(ctx, store, log, m)
	if plan.InstallationSync > 0 {
		// До индексации: снятая установка не должна получить ещё один проход
		// отказов на старте.
		syncInstallations(ctx, engine, provider, log, m)
	}
	if cfg.IndexEnabled {
		indexPass(ctx, ix, engine, log, system.ID, cfg.IndexRetainRuns, m)
		syncRepoRefs(ctx, ix, log, m)
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("получен сигнал, останавливаемся")
			return nil
		case <-leaseTick:
			sweepLeases(ctx, engine, log, system.ID, m)
			refreshState(ctx, store, log, m)
		case <-indexTick:
			indexPass(ctx, ix, engine, log, system.ID, cfg.IndexRetainRuns, m)
		case <-queueTick:
			drainQueue(ctx, ix, engine, log, system.ID, m)
		case <-refsTick:
			syncRepoRefs(ctx, ix, log, m)
		case <-installTick:
			syncInstallations(ctx, engine, provider, log, m)
		case <-blobGC:
			collectBlobs(ctx, store, log, cfg, m)
		case <-embedGC:
			evictEmbeddings(ctx, store, log, cfg, m)
		case <-auditGC:
			purgeAudit(ctx, store, log, cfg.AuditRetentionDays, m)
		}
	}
}

// schedule — периоды тикеров воркера. Ноль — задание не заводит тикера:
// nil-канал в select молчит всегда, и до базы и провайдера дело не доходит
// ни разу.
type schedule struct {
	LeaseSweep       time.Duration
	Index            time.Duration
	Queue            time.Duration
	RepoRefSync      time.Duration
	InstallationSync time.Duration
	BlobGC           time.Duration
	EmbedCacheGC     time.Duration
	AuditGC          time.Duration
}

// newSchedule решает, какие задания идут и с каким периодом. Выключенная
// индексация выключает и очередь, и синхронизацию PR: обе читают провайдера
// теми же учётными данными, что индексация (так было и до CLAB-395). Опрос
// установок от индексации не зависит — его включает приложение.
func newSchedule(cfg config.Config, appConfigured bool) schedule {
	s := schedule{LeaseSweep: cfg.LeaseSweepInterval}
	if cfg.IndexEnabled {
		s.Index, s.Queue, s.RepoRefSync = cfg.IndexInterval, cfg.IndexPollInterval, cfg.RepoRefSyncInterval
	}
	if appConfigured {
		s.InstallationSync = cfg.InstallationSyncInterval
	}
	if cfg.BlobGCEnabled {
		s.BlobGC = cfg.BlobGCInterval
	}
	if cfg.EmbeddingCacheGCEnabled {
		s.EmbedCacheGC = cfg.EmbeddingCacheGCInterval
	}
	// Срок 0 — хранить журнал вечно.
	if cfg.AuditRetentionDays > 0 {
		s.AuditGC = auditGCInterval
	}
	return s
}

// ticker заводит тикер периода d; при нуле отдаёт nil-канал.
func (schedule) ticker(d time.Duration) (<-chan time.Time, func()) {
	if d <= 0 {
		return nil, func() {}
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// syncInstallations сверяет установки GitHub App с провайдером. В лог на
// info — только изменения: согласованное состояние — не событие.
func syncInstallations(ctx context.Context, engine *workflow.Engine, src workflow.InstallationSource, log *slog.Logger, m *metrics.Worker) {
	var changes []workflow.InstallationChange
	err := m.Job(ctx, jobInstallationSync, func() (err error) {
		changes, err = engine.SyncInstallations(ctx, src)
		return err
	})
	for _, c := range changes {
		log.Info("состояние установки GitHub обновлено опросом",
			"installation", c.InstallationID, "change", c.Change, "repository", c.Repository)
	}
	if err != nil {
		logJobError(ctx, log, "опрос установок GitHub", err)
	}
}

// drainQueue исполняет то, что уже стоит в очереди, не опрашивая провайдера:
// это путь вебхука к индексу и детектору дрейфа между полными проходами.
func drainQueue(ctx context.Context, ix *indexer.Indexer, engine *workflow.Engine, log *slog.Logger, system uuid.UUID, m *metrics.Worker) {
	if runIndexRuns(ctx, ix, engine, log, system, m) > 0 {
		syncRepoRefs(ctx, ix, log, m)
	}
}

func syncRepoRefs(ctx context.Context, ix *indexer.Indexer, log *slog.Logger, m *metrics.Worker) {
	var n int
	err := m.Job(ctx, jobRepoRefSync, func() (err error) {
		n, err = ix.SyncRepoRefs(ctx, 50)
		return err
	})
	if err != nil {
		logJobError(ctx, log, "синхронизация PR", err)
	} else if n > 0 {
		log.Info("состояние PR обновлено", "count", n)
	}
}

// runIndexRuns исполняет прогоны из очереди и сразу за каждым гоняет
// детектор дрейфа: задача, проехавшая мимо расхождения, узнает о нём на
// следующем этапе, когда чинить дороже.
func runIndexRuns(ctx context.Context, ix *indexer.Indexer, engine *workflow.Engine, log *slog.Logger, system uuid.UUID, m *metrics.Worker) int {
	done := 0
	// Предел прогонов за проход: репозиториев может быть много, а следующий
	// тик всё равно наступит — лучше вернуться к арендам, чем зависнуть.
	for i := 0; i < maxRunsPerPass; i++ {
		var run *postgres.IndexRun
		err := m.Job(ctx, jobIndexRun, func() (err error) {
			run, err = ix.RunNext(ctx)
			return err
		})
		if err != nil {
			logJobError(ctx, log, "прогон индексации", err)
			break
		}
		if run == nil {
			break
		}
		done++
		var signals []postgres.NewDriftSignal
		err = m.Job(ctx, jobDrift, func() (err error) {
			signals, err = engine.DetectDrift(ctx, *run, system)
			return err
		})
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
func collectBlobs(ctx context.Context, store *postgres.Store, log *slog.Logger, cfg config.Config, m *metrics.Worker) {
	var stats postgres.BlobGCStats
	err := m.Job(ctx, jobBlobGC, func() (err error) {
		stats, err = store.CollectBlobs(ctx, postgres.BlobGCOptions{
			Limit:  cfg.BlobGCLimit,
			MinAge: cfg.BlobGCMinAge,
			DryRun: cfg.BlobGCDryRun,
		}, func(sha string, err error) {
			// Строка уже удалена, а объект остался: это утечка места, но не
			// потеря данных — ссылок на него нет. В логе sha, чтобы было по
			// чему разбирать хранилище руками.
			log.Warn("объект остался в хранилище после удаления строки блоба",
				"sha256", sha, "error", err)
		})
		return err
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

// Уборка журнала: раз в час, пачками по auditGCBatch, не больше
// auditGCMaxBatches пачек за проход. Хвост после долгого простоя или после
// уменьшения срока разбирается за несколько проходов, а тик не затягивается.
const (
	auditGCInterval   = time.Hour
	auditGCBatch      = 10_000
	auditGCMaxBatches = 20
)

// purgeAudit удаляет записи журнала действий старше срока хранения.
// Первого прохода на старте нет — тот же довод, что у блобов: место
// освобождается не срочно. Ошибка прохода воркер не роняет.
func purgeAudit(ctx context.Context, store *postgres.Store, log *slog.Logger, days int, m *metrics.Worker) {
	var total int64
	err := m.Job(ctx, jobAuditGC, func() error {
		total = 0
		before := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		for range auditGCMaxBatches {
			n, err := store.PurgeAudit(ctx, store.DB(), before, auditGCBatch)
			if err != nil {
				return err
			}
			total += n
			if n < auditGCBatch {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		logJobError(ctx, log, "уборка журнала действий", err)
		return
	}
	if total > 0 {
		log.Info("уборка журнала действий", "deleted", total, "retention_days", days)
	}
}

// evictEmbeddings вытесняет из кэша эмбеддингов то, чем давно не
// пользовались, и то, что не помещается в границу размера.
//
// Кэш ключуется содержимым и владельца не имеет намеренно: ни удаление
// проекта, ни отвязка репозитория его не уменьшают, и без этого прохода он
// растёт монотонно. Первого прохода на старте нет — тот же довод, что у
// блобов: место освобождается не срочно. Ошибка прохода воркер не роняет.
func evictEmbeddings(ctx context.Context, store *postgres.Store, log *slog.Logger, cfg config.Config, m *metrics.Worker) {
	var stats postgres.EmbeddingCacheGCStats
	err := m.Job(ctx, jobEmbedCacheGC, func() (err error) {
		stats, err = store.EvictEmbeddingCache(ctx, postgres.EmbeddingCacheGCOptions{
			TTL:     cfg.EmbeddingCacheTTL,
			MaxRows: cfg.EmbeddingCacheMaxRows,
			Limit:   cfg.EmbeddingCacheGCLimit,
			DryRun:  cfg.EmbeddingCacheGCDryRun,
		})
		return err
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

// Пачка и бюджет досчёта векторов за проход воркера. Пачка — кусков на
// источник за один заход (код, артефакты, документы), бюджет — сколько проход
// готов отдать модели: минута из пятиминутного интервала индексации. Репозиторий
// на 20 тыс. кусков переходит на новую модель за несколько проходов, а не за
// сутки.
const (
	embedRefreshBatch  = 100
	embedRefreshBudget = time.Minute
)

func embeddingModel(e embed.Embedder) string {
	if e == nil {
		return ""
	}
	return e.Model()
}

// indexPass — полный проход по всему, что приезжает из провайдера и уезжает
// в индекс.
//
// Порядок неслучаен: сперва ставим в очередь новые коммиты, потом исполняем
// прогоны и сразу за каждым гоняем детектор дрейфа (задача, проехавшая мимо
// расхождения, узнает о нём на следующем этапе, когда чинить дороже), затем
// векторизация артефактов. Состояние PR для гейта external_check идёт своим
// тикером (SDLC_REPO_REF_SYNC_INTERVAL, CLAB-395) и после исполненных
// прогонов очереди (drainQueue).
func indexPass(ctx context.Context, ix *indexer.Indexer, engine *workflow.Engine, log *slog.Logger, system uuid.UUID, retain int, m *metrics.Worker) {
	if err := m.Job(ctx, jobIndexEnqueue, func() error {
		_, err := ix.Enqueue(ctx)
		return err
	}); err != nil {
		logJobError(ctx, log, "постановка прогонов индексации", err)
	}

	runIndexRuns(ctx, ix, engine, log, system, m)

	// Индекс на коммит неизменяем и растёт линейно по коммитам; старые
	// прогоны без ссылающихся базовых линий вычищаются.
	var pruned int
	if err := m.Job(ctx, jobIndexPrune, func() (err error) {
		pruned, err = ix.Prune(ctx, retain)
		return err
	}); err != nil {
		logJobError(ctx, log, "вычистка старых прогонов", err)
	} else if pruned > 0 {
		log.Info("старые прогоны индексации удалены", "count", pruned, "retain_per_branch", retain)
	}

	var artifacts, documents int
	if err := m.Job(ctx, jobKnowledge, func() (err error) {
		artifacts, documents, err = ix.IndexKnowledge(ctx, 50)
		return err
	}); err != nil {
		logJobError(ctx, log, "векторизация артефактов", err)
	} else if artifacts+documents > 0 {
		log.Info("артефакты и документы разложены на куски",
			"artifacts", artifacts, "documents", documents)
	}

	// Досчёт и пересчёт на текущую модель идут пачками, пока есть кандидаты,
	// но не дольше бюджета: после смены модели кусков десятки тысяч, а
	// детектор пересечений и следующий прогон ждать их не должны.
	var stats indexer.RefreshStats
	if err := m.Job(ctx, jobEmbedRefresh, func() (err error) {
		stats, err = ix.RefreshEmbeddings(ctx, embedRefreshBatch, embedRefreshBudget)
		return err
	}); err != nil {
		logJobError(ctx, log, "досчёт эмбеддингов", err)
	}
	if stats.Embedded > 0 || stats.Cleared > 0 {
		log.Info("векторы досчитаны на текущую модель",
			"count", stats.Embedded, "cleared_stale_runs", stats.Cleared)
	}

	// Пересечение задач возникает не от движения main, а от того, что вторая
	// задача объявила ту же зону: ждать индексации для него незачем.
	var overlaps []postgres.NewDriftSignal
	if err := m.Job(ctx, jobOverlaps, func() (err error) {
		overlaps, err = engine.DetectOverlaps(ctx, system)
		return err
	}); err != nil {
		logJobError(ctx, log, "поиск пересечений задач", err)
	} else if len(overlaps) > 0 {
		log.Info("найдены пересечения зон задач", "count", len(overlaps))
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
//
// Тем же проходом отмечаются этапы, открытые дольше предела stage_time_limit
// (CLAB-221): своей переменной интервала у этого нет, а имя задания в
// метриках прежнее — оно часть правил оповещения пакета self-hosted.
// Просрочка аренду не трогает: этап остаётся открытым, решение за человеком.
func sweepLeases(ctx context.Context, engine *workflow.Engine, log *slog.Logger, system uuid.UUID, m *metrics.Worker) {
	var n, overdue int
	err := m.Job(ctx, jobLeaseSweep, func() error {
		expired, err := engine.ExpireLeases(ctx, system, 100)
		if err != nil {
			return err
		}
		// Считаем только после удачного прохода: при ошибке транзакция
		// откатилась, и число не значит ничего. Аренды и просрочка — разные
		// транзакции: сбой отметки просрочки вернувшихся аренд не откатывает.
		n = expired
		overdue, err = engine.MarkOverdueStages(ctx, system, 100)
		return err
	})
	m.LeasesExpired(n)
	if n > 0 {
		log.Info("аренды истекли, задачи возвращены в общий доступ", "count", n)
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error("проход по протухшим арендам и просроченным этапам не удался", "error", err)
		return
	}
	if overdue > 0 {
		log.Info("этапы открыты дольше предела, ждут решения человека", "count", overdue)
	}
}

// refreshState снимает состояние базы для метрик: очередь индексации,
// аренды, нечитаемые головы веток. Идёт на тике аренд, а не на scrape:
// сколько бы раз Prometheus ни опрашивал воркер, база получает один запрос
// за период. Без метрик в базу не ходит вовсе.
func refreshState(ctx context.Context, store *postgres.Store, log *slog.Logger, m *metrics.Worker) {
	if m == nil {
		return
	}
	var st postgres.OpsState
	err := m.Job(ctx, jobOpsState, func() (err error) {
		st, err = store.OpsState(ctx, store.DB())
		return err
	})
	if err != nil {
		logJobError(ctx, log, "снимок состояния для метрик", err)
		return
	}
	m.SetState(metrics.State{
		QueuedRuns: st.QueuedRuns, RunningRuns: st.RunningRuns,
		OldestQueued: st.OldestQueued,
		LiveLeases:   st.LiveLeases, ExpiredLeases: st.ExpiredLeases,
		HeadFailures: st.HeadFailures,
	}, time.Now())
}

// Имена заданий цикла — значения метки job в метриках воркера. Меняются
// только вместе с правилами оповещения в docs/admin.md пакета self-hosted.
const (
	jobLeaseSweep   = "lease_sweep"
	jobIndexEnqueue = "index_enqueue"
	jobIndexRun     = "index_run"
	jobDrift        = "drift"
	jobIndexPrune   = "index_prune"
	jobRepoRefSync  = "repo_ref_sync"
	jobKnowledge    = "knowledge"
	jobEmbedRefresh = "embed_refresh"
	jobOverlaps     = "overlaps"
	jobBlobGC       = "blob_gc"
	jobEmbedCacheGC = "embed_cache_gc"
	jobOpsState     = "ops_state"
	jobAuditGC      = "audit_gc"
	// jobInstallationSync — опрос установок GitHub App (CLAB-395).
	jobInstallationSync = "installation_sync"
)

var workerJobs = []string{jobLeaseSweep, jobIndexEnqueue, jobIndexRun, jobDrift,
	jobIndexPrune, jobRepoRefSync, jobKnowledge, jobEmbedRefresh, jobOverlaps,
	jobBlobGC, jobEmbedCacheGC, jobOpsState, jobAuditGC, jobInstallationSync}

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
