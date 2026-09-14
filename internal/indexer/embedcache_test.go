package indexer

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
)

// Отметка попаданий и счётчики кэша проверяются на настоящей базе: и то и
// другое живёт в связке прогона с кэшем, а на моках проверялся бы сам мок.

// logSpy — логгер, запоминающий записи: доля попаданий в кэш уходит полями
// структурированного лога, отдельного слоя метрик в сервисе нет, и другого
// способа увидеть цифру снаружи не существует.
type logSpy struct {
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	level   slog.Level
	message string
	attrs   map[string]slog.Value
}

func (s *logSpy) Enabled(context.Context, slog.Level) bool { return true }

func (s *logSpy) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, message: r.Message, attrs: map[string]slog.Value{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value
		return true
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
	return nil
}

func (s *logSpy) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *logSpy) WithGroup(string) slog.Handler      { return s }

// find отдаёт последнюю запись с таким сообщением: сводок прогона в логе
// столько же, сколько прогонов, а интересна та, что относится ко второму.
func (s *logSpy) find(message string) (logRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].message == message {
			return s.records[i], true
		}
	}
	return logRecord{}, false
}

func (s *logSpy) logger() *slog.Logger { return slog.New(s) }

// twoRuns индексирует два коммита одного репозитория: второй меняет один файл
// и добавляет другой, поэтому часть кусков у него та же — это и есть
// попадание в кэш.
func twoRuns(t *testing.T, ix *Indexer) {
	t.Helper()
	ctx := context.Background()
	src := ix.source.(*fakeSource)
	for _, head := range []string{"c1", "c2"} {
		src.head = head
		if _, err := ix.Enqueue(ctx); err != nil {
			t.Fatalf("постановка %s: %v", head, err)
		}
		run, err := ix.RunNext(ctx)
		if err != nil || run == nil {
			t.Fatalf("прогон %s: %v (run=%v)", head, err, run)
		}
		if run.State != "completed" {
			t.Fatalf("прогон %s завершился состоянием %q: %v", head, run.State, run.Error)
		}
	}
}

func cacheSource() *fakeSource {
	return &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {
				"internal/workflow/engine.go": engineV1,
				"README.md":                   "# Бэкенд\n\nСервис цикла разработки.\n",
			},
			"c2": {
				"internal/workflow/engine.go": engineV2,
				"README.md":                   "# Бэкенд\n\nСервис цикла разработки.\n",
				"internal/workflow/lease.go":  "package workflow\n\nfunc Lease() {}\n",
			},
		},
	}
}

// Попадание в кэш обязано отмечаться: без отметки вытеснение считало бы
// строку, которой пользуются каждый прогон, такой же остывшей, как забытую.
// Заодно проверяется, что доля попаданий доезжает до сводки прогона.
func TestSecondRunMarksCacheHits(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	spy := &logSpy{}
	// Порог отметки нулевой: строки заведены секунды назад, а проверяется
	// сама отметка, а не то, что она бережёт записи.
	ix := New(store, cacheSource(), embed.Hash{}, spy.logger(),
		Options{ChunkChars: 400, EmbedBatch: 8})

	twoRuns(t, ix)

	var touched int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM embedding_cache WHERE last_used_at > created_at`).Scan(&touched)
	if err != nil {
		t.Fatal(err)
	}
	if touched == 0 {
		t.Fatal("ни одно попадание в кэш не отмечено: вытеснение унесёт используемые строки")
	}

	rec, ok := spy.find("репозиторий проиндексирован")
	if !ok {
		t.Fatal("сводки прогона нет в логе")
	}
	lookups := rec.attrs["cache_lookups"].Int64()
	hits := rec.attrs["cache_hits"].Int64()
	if hits <= 0 || hits > lookups {
		t.Fatalf("доля попаданий в сводке: hits=%d lookups=%d", hits, lookups)
	}
}

// Отметка попаданий прогон не роняет: векторы уже отданы вызывающему, и
// платить за них второй раз из-за недоступной на секунду базы незачем.
// Сбой изображается триггером на UPDATE — так падает ровно тот запрос,
// который проверяется, а не соседний.
func TestTouchFailureDoesNotBreakRun(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	spy := &logSpy{}
	ix := New(store, cacheSource(), embed.Hash{}, spy.logger(),
		Options{ChunkChars: 400, EmbedBatch: 8})

	// Первый прогон наполняет кэш, второму есть во что попадать.
	src := ix.source.(*fakeSource)
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка c1: %v", err)
	}
	if _, err := ix.RunNext(ctx); err != nil {
		t.Fatalf("прогон c1: %v", err)
	}

	_, err := pool.Exec(ctx, `
		CREATE FUNCTION reject_touch() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'кэш не отмечается'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_touch BEFORE UPDATE ON embedding_cache
		FOR EACH ROW EXECUTE FUNCTION reject_touch();`)
	if err != nil {
		t.Fatalf("триггер: %v", err)
	}

	src.head = "c2"
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка c2: %v", err)
	}
	run, err := ix.RunNext(ctx)
	if err != nil || run == nil {
		t.Fatalf("прогон c2: %v (run=%v)", err, run)
	}
	if run.State != "completed" {
		t.Fatalf("сбой отметки уронил прогон: %s (%v)", run.State, run.Error)
	}
	if run.ChunksEmbedded >= run.ChunksIndexed {
		t.Fatalf("кэш перестал работать: embedded=%d indexed=%d",
			run.ChunksEmbedded, run.ChunksIndexed)
	}

	rec, ok := spy.find("попадания в кэш эмбеддингов не отмечены")
	if !ok {
		t.Fatal("сбой отметки не попал в лог")
	}
	if rec.level != slog.LevelWarn {
		t.Fatalf("сбой отметки записан уровнем %s", rec.level)
	}

	// Векторы у кусков на месте: отметка не имеет к ним отношения.
	var withVectors int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM code_chunks c
		JOIN code_files f ON f.id = c.code_file_id
		WHERE f.index_run_id = $1 AND c.embedding IS NOT NULL`, run.ID).Scan(&withVectors)
	if err != nil {
		t.Fatal(err)
	}
	if withVectors == 0 {
		t.Fatal("куски прогона остались без векторов")
	}
}
