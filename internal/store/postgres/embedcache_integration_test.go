package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Вытеснение кэша эмбеддингов проверяется на настоящей базе: весь проход —
// это три запроса, и мок проверял бы собственный текст, а не их поведение.

const cacheModel = "voyage-code-3"

// cacheFixture — база со свежей схемой и пустым кэшем эмбеддингов.
type cacheFixture struct {
	t     *testing.T
	ctx   context.Context
	store *postgres.Store
	pool  *pgxpool.Pool
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	store, pool := testdb.Store(t)
	return &cacheFixture{t: t, ctx: context.Background(), store: store, pool: pool}
}

// zeroVector — литерал вектора нужной размерности. Содержимое вектора
// вытеснению безразлично: оно смотрит только на отметку использования.
func zeroVector() string {
	return "[" + strings.Repeat("0,", 1023) + "0]"
}

// put кладёт строку кэша с заданным возрастом отметки использования.
// created_at ставится тем же временем: строку, которой не пользовались,
// нельзя выдавать за свежую.
func (f *cacheFixture) put(sha string, usedAgo time.Duration) {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx, `
		INSERT INTO embedding_cache (model, text_sha, embedding, created_at, last_used_at)
		VALUES ($1, $2, $3::vector, now() - $4::interval, now() - $4::interval)`,
		cacheModel, sha, zeroVector(), usedAgo)
	if err != nil {
		f.t.Fatalf("запись в кэш: %v", err)
	}
}

func (f *cacheFixture) rows() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM embedding_cache`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *cacheFixture) alive(sha string) bool {
	f.t.Helper()
	var n int
	err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM embedding_cache WHERE text_sha = $1`, sha).Scan(&n)
	if err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

// sha — ключ строки кэша: char(64), поэтому дополняется до длины.
func sha(name string) string {
	return name + strings.Repeat("0", 64-len(name))
}

// Строка, которой не пользовались дольше срока, уходит; та, которой
// пользовались, остаётся. Смотрит проход на last_used_at, а не на created_at:
// кэш, которым пользуются год, ценнее вчерашнего, к которому не обращались.
func TestEvictEmbeddingCacheByTTL(t *testing.T) {
	f := newCacheFixture(t)
	f.put(sha("stale"), 48*time.Hour)
	f.put(sha("fresh"), time.Minute)

	stats, err := f.store.EvictEmbeddingCache(f.ctx, postgres.EmbeddingCacheGCOptions{
		TTL: 24 * time.Hour, Limit: 100,
	})
	if err != nil {
		t.Fatalf("проход: %v", err)
	}
	switch {
	case stats.RowsBefore != 2:
		t.Fatalf("размер до прохода %d", stats.RowsBefore)
	case stats.Bytes <= 0:
		t.Fatalf("размер в байтах %d", stats.Bytes)
	case stats.DeletedStale != 1 || stats.DeletedOverflow != 0:
		t.Fatalf("вытеснено stale=%d overflow=%d", stats.DeletedStale, stats.DeletedOverflow)
	case f.alive(sha("stale")):
		t.Fatal("просроченная строка осталась")
	case !f.alive(sha("fresh")):
		t.Fatal("недавно использованная строка вытеснена")
	}
}

// Граница размера оставляет ровно столько строк, сколько разрешено, и именно
// самые свежие по отметке использования.
func TestEvictEmbeddingCacheByMaxRows(t *testing.T) {
	f := newCacheFixture(t)
	// Все строки свежие: по сроку вытеснять нечего, работает только граница.
	for i, age := range []time.Duration{5 * time.Minute, 4 * time.Minute, 3 * time.Minute, 2 * time.Minute, time.Minute} {
		f.put(sha(string(rune('a'+i))), age)
	}

	stats, err := f.store.EvictEmbeddingCache(f.ctx, postgres.EmbeddingCacheGCOptions{
		TTL: time.Hour, MaxRows: 2, Limit: 100,
	})
	if err != nil {
		t.Fatalf("проход: %v", err)
	}
	if stats.DeletedStale != 0 || stats.DeletedOverflow != 3 {
		t.Fatalf("вытеснено stale=%d overflow=%d", stats.DeletedStale, stats.DeletedOverflow)
	}
	if n := f.rows(); n != 2 {
		t.Fatalf("после прохода осталось %d строк при границе 2", n)
	}
	// Остаться обязаны самые свежие: 'd' (две минуты) и 'e' (минута).
	if !f.alive(sha("d")) || !f.alive(sha("e")) {
		t.Fatal("проход оставил не самые свежие строки")
	}
}

// Предел за проход соблюдается: хвост разбирается следующими проходами, а не
// одной транзакцией на всю таблицу.
func TestEvictEmbeddingCacheRespectsLimit(t *testing.T) {
	f := newCacheFixture(t)
	for i := range 5 {
		f.put(sha(string(rune('a'+i))), 48*time.Hour)
	}

	opts := postgres.EmbeddingCacheGCOptions{TTL: time.Hour, Limit: 2}
	stats, err := f.store.EvictEmbeddingCache(f.ctx, opts)
	if err != nil {
		t.Fatalf("первый проход: %v", err)
	}
	if stats.Deleted() != 2 {
		t.Fatalf("первый проход вытеснил %d строк при пределе 2", stats.Deleted())
	}
	if n := f.rows(); n != 3 {
		t.Fatalf("после первого прохода осталось %d строк", n)
	}
	if stats, err = f.store.EvictEmbeddingCache(f.ctx, opts); err != nil {
		t.Fatalf("второй проход: %v", err)
	}
	if stats.Deleted() != 2 || f.rows() != 1 {
		t.Fatalf("второй проход: вытеснено %d, осталось %d", stats.Deleted(), f.rows())
	}
}

// Сухой прогон считает ровно то же, что посчитал бы настоящий, и ничего не
// удаляет: по этой цифре оператор решает, включать ли вытеснение на
// засиженной базе.
func TestEvictEmbeddingCacheDryRun(t *testing.T) {
	f := newCacheFixture(t)
	// Три просроченных и три свежих; граница в две строки снимет ещё одну
	// сверх просроченных — так проверяется и совместная работа двух причин.
	for i, age := range []time.Duration{72 * time.Hour, 71 * time.Hour, 70 * time.Hour, 3 * time.Minute, 2 * time.Minute, time.Minute} {
		f.put(sha(string(rune('a'+i))), age)
	}

	opts := postgres.EmbeddingCacheGCOptions{TTL: 24 * time.Hour, MaxRows: 2, Limit: 100}
	dry := opts
	dry.DryRun = true

	before := f.rows()
	dryStats, err := f.store.EvictEmbeddingCache(f.ctx, dry)
	if err != nil {
		t.Fatalf("сухой прогон: %v", err)
	}
	if n := f.rows(); n != before {
		t.Fatalf("сухой прогон удалил строки: было %d, стало %d", before, n)
	}

	wet, err := f.store.EvictEmbeddingCache(f.ctx, opts)
	if err != nil {
		t.Fatalf("настоящий проход: %v", err)
	}
	if dryStats.DeletedStale != wet.DeletedStale || dryStats.DeletedOverflow != wet.DeletedOverflow {
		t.Fatalf("сухой прогон обещал stale=%d overflow=%d, настоящий дал stale=%d overflow=%d",
			dryStats.DeletedStale, dryStats.DeletedOverflow, wet.DeletedStale, wet.DeletedOverflow)
	}
	if wet.DeletedStale != 3 || wet.DeletedOverflow != 1 {
		t.Fatalf("вытеснено stale=%d overflow=%d", wet.DeletedStale, wet.DeletedOverflow)
	}
	if n := f.rows(); n != 2 {
		t.Fatalf("после прохода осталось %d строк при границе 2", n)
	}
}

// Проходу нечего делать — статистика пустая, и воркеру нечего писать в лог:
// строка «вытеснено 0» раз в шесть часов навсегда никому не нужна.
func TestEvictEmbeddingCacheQuietWhenNothingToDo(t *testing.T) {
	f := newCacheFixture(t)
	opts := postgres.EmbeddingCacheGCOptions{TTL: 24 * time.Hour, MaxRows: 10, Limit: 100}

	stats, err := f.store.EvictEmbeddingCache(f.ctx, opts)
	if err != nil || !stats.Empty() || stats.RowsBefore != 0 {
		t.Fatalf("пустой кэш: %+v (%v)", stats, err)
	}

	f.put(sha("fresh"), time.Minute)
	if stats, err = f.store.EvictEmbeddingCache(f.ctx, opts); err != nil || !stats.Empty() {
		t.Fatalf("свежий кэш: %+v (%v)", stats, err)
	}
	if !f.alive(sha("fresh")) {
		t.Fatal("свежая строка вытеснена")
	}
}

// countingQuerier считает обращения к базе: отметка попаданий обязана быть
// одним запросом на пачку, иначе прогон индексации встанет на сотнях sha.
type countingQuerier struct {
	postgres.Querier
	execs int
}

func (c *countingQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.execs++
	return c.Querier.Exec(ctx, sql, args...)
}

func TestTouchEmbeddingsMarksBatchInOneQuery(t *testing.T) {
	f := newCacheFixture(t)
	shas := make([]string, 0, 4)
	for i := range 4 {
		s := sha(string(rune('a' + i)))
		f.put(s, 48*time.Hour)
		shas = append(shas, s)
	}

	q := &countingQuerier{Querier: f.pool}
	n, err := f.store.TouchEmbeddings(f.ctx, q, cacheModel, shas, time.Hour)
	if err != nil {
		t.Fatalf("отметка: %v", err)
	}
	if n != len(shas) {
		t.Fatalf("отмечено %d строк из %d", n, len(shas))
	}
	if q.execs != 1 {
		t.Fatalf("отметка пачки из %d sha стоила %d запросов", len(shas), q.execs)
	}

	var newer int
	err = f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM embedding_cache WHERE last_used_at > created_at`).Scan(&newer)
	if err != nil {
		t.Fatal(err)
	}
	if newer != len(shas) {
		t.Fatalf("отметка не обновила время у %d строк", len(shas)-newer)
	}
}

// Порог отметки: строку, отмеченную только что, второй раз не переписывают.
// Без него каждая пачка каждого прогона переписывала бы четырёхкилобайтные
// строки и индекс по last_used_at ради времени, которое не изменилось.
func TestTouchEmbeddingsHonoursThreshold(t *testing.T) {
	f := newCacheFixture(t)
	s := sha("hit")
	f.put(s, 30*time.Minute)

	n, err := f.store.TouchEmbeddings(f.ctx, f.pool, cacheModel, []string{s}, time.Hour)
	if err != nil {
		t.Fatalf("отметка с порогом: %v", err)
	}
	if n != 0 {
		t.Fatalf("строка моложе порога переписана (%d)", n)
	}

	if n, err = f.store.TouchEmbeddings(f.ctx, f.pool, cacheModel, []string{s}, 0); err != nil {
		t.Fatalf("отметка без порога: %v", err)
	}
	if n != 1 {
		t.Fatalf("отметка без порога обновила %d строк", n)
	}

	// Чужая модель мимо: вектор от другой модели несравним с этими, и его
	// отметка не имеет отношения к этой строке.
	if n, err = f.store.TouchEmbeddings(f.ctx, f.pool, "other-model", []string{s}, 0); err != nil || n != 0 {
		t.Fatalf("отметка по чужой модели обновила %d строк (%v)", n, err)
	}
}

// Отметка сбоит — вызывающий об этом узнаёт, но попадания у него уже на
// руках: ронять прогон индексации из-за неё нельзя, и наверху ошибка уходит
// в лог предупреждением.
func TestTouchEmbeddingsReportsFailure(t *testing.T) {
	f := newCacheFixture(t)
	if _, err := f.store.TouchEmbeddings(f.ctx, failingQuerier{}, cacheModel, []string{sha("hit")}, 0); err == nil {
		t.Fatal("сбой отметки не вернулся ошибкой")
	}
	// Пустая пачка до базы не доходит вовсе.
	if _, err := f.store.TouchEmbeddings(f.ctx, failingQuerier{}, cacheModel, nil, 0); err != nil {
		t.Fatalf("пустая пачка сходила в базу: %v", err)
	}
}

// failingQuerier — база, которой нет.
type failingQuerier struct{}

var errNoDB = errors.New("база недоступна")

func (failingQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errNoDB
}

func (failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errNoDB
}

func (failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

type errRow struct{}

func (errRow) Scan(...any) error { return errNoDB }
