package indexer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Полнотекст кода (CLAB-362): имя находится внутри записи через точку, а
// кусок, где оно объявлено, идёт выше кусков, где его зовут.

// Набивка ≥ 400 байт: следующее объявление начинает новый кусок (chunk.go,
// minChunkChars), и у куска с объявлением свой symbol_fqn.
const searchPadding = `// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
`

// Объявление в начале куска: symbol_fqn куска — сам ListAttachments.
const searchAttachmentsDecl = "package postgres\n\n" + searchPadding +
	`func (s *Store) ListAttachments(ctx context.Context, taskID string) ([]string, error) {
	return nil, nil
}
`

// Объявление не в начале куска: мелкие объявления слиплись, и symbol_fqn
// куска — IssueTaskToken, а RevokeRunTokens начинается ниже.
const searchAuthDecl = "package postgres\n\n" + searchPadding +
	`func (s *Store) IssueTaskToken(ctx context.Context) (string, error) {
	return "", nil
}

func tokenHash(token string) string { return token }

// RevokeRunTokens revokes the tokens of a run.
func (s *Store) RevokeRunTokens(ctx context.Context, runID string) error {
	return nil
}
`

// Только вызовы через точку, и их больше, чем упоминаний в объявлении:
// по одному ts_rank_cd этот кусок обгонял бы определение. Комментарий внутри
// функции — чтобы кусок был один и вектор по мешку слов не тянул его вверх.
const searchCalls = `package workflow

func (e *Engine) releaseLease(ctx context.Context, s *Store, runID string) error {
	// Аренда снимается явно: освобождение, протухание, повторный захват и
	// уход этапа в ожидание гейтов гасят учётные данные исполнителя одной
	// транзакцией с событием аренды, чтобы он не пережил свой прогон.
	if err := e.store.RevokeRunTokens(ctx, runID); err != nil {
		return err
	}
	_ = s.RevokeRunTokens(ctx, runID)
	_ = e.store.RevokeRunTokens(ctx, runID)
	return e.store.RevokeRunTokens(ctx, runID)
}

func (e *Engine) attachments(ctx context.Context, s *Store, taskID string) error {
	// Вложения читаются отдельным списком, а не вместе с артефактами этапа.
	_, _ = s.store.ListAttachments(ctx, taskID)
	_, _ = e.store.ListAttachments(ctx, taskID)
	_, err := e.store.ListAttachments(ctx, taskID)
	return err
}
`

// Обычные слова фразы — много раз и рядом с объявлением func task: по
// слову task этот кусок поднимать нельзя.
const searchHelpers = `package workflow

func task(name string) string { return "revoke task tokens " + name }

func revokeTaskTokens() string { return task("revoke") + task("tokens") + task("task") }
`

const searchMixed = `package workflow

func mixed() {
	e.store.RevokeRunTokens(ctx) api.listAttachments(k) ctx context.Context pkg.X a.b.c.D internal/workflow.Engine.Advance
	v := obj?.items
	user_id := 1
	my_thing := "my-thing"
}
`

// Для гибрида: объявление — весь файл, и по мешку слов embed.Hash оно ближе
// к запросу, чем кусок с тремя вызовами среди прочих слов. Иначе векторный
// ранг 1 у вызовов и полнотекстовый ранг 1 у объявления дали бы равные
// суммы RRF, и порядок решала бы случайность обхода map.
const searchBlobDecl = `package postgres

func PutBlob() {}
`

const searchBlobCalls = `package workflow

func (e *Engine) save(ctx context.Context) error {
	// Содержимое уходит в хранилище до строки артефакта: объект без строки
	// подберёт сборщик мусора, а строка без объекта сломала бы чтение.
	_ = e.store.PutBlob(ctx)
	_ = e.store.PutBlob(ctx)
	return e.store.PutBlob(ctx)
}
`

const searchJSX = `export function Page() {
  return <Button variant="outline">ok</Button>
}
`

func indexSearchFixture(t *testing.T) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	store, pool := testStore(t)
	ctx := context.Background()
	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {
				"internal/store/postgres/attachments.go": searchAttachmentsDecl,
				"internal/store/postgres/auth.go":        searchAuthDecl,
				"internal/workflow/agents.go":            searchCalls,
				"internal/workflow/helpers_test.go":      searchHelpers,
				"internal/workflow/mixed.go":             searchMixed,
				"web/src/Page.tsx":                       searchJSX,
				"internal/store/postgres/blobs.go":       searchBlobDecl,
				"internal/workflow/save.go":              searchBlobCalls,
			},
		},
	}
	// Размер куска как у индексатора по умолчанию: файл вызовов — один кусок.
	ix := New(store, src, embed.Hash{}, testdb.Logger(), Options{ChunkChars: 1600, EmbedBatch: 8})
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	if run, err := ix.RunNext(ctx); err != nil || run == nil {
		t.Fatalf("прогон индексации: %v", err)
	}
	return store, pool
}

func searchCode(t *testing.T, engine *workflow.Engine, query, wantMode string) []postgres.SearchHit {
	t.Helper()
	res, err := engine.SearchContext(context.Background(), "SDLC", workflow.SearchInput{
		Query: query, Scopes: []string{"code"}, TopK: 20,
	})
	if err != nil {
		t.Fatalf("поиск %q: %v", query, err)
	}
	if res.Mode != wantMode {
		t.Fatalf("поиск %q: режим %q, ждали %q", query, res.Mode, wantMode)
	}
	return res.Hits
}

// hitIndex — место первого куска файла, текст которого содержит needle; -1 —
// нет в выдаче.
func hitIndex(hits []postgres.SearchHit, path, needle string) int {
	return slices.IndexFunc(hits, func(h postgres.SearchHit) bool {
		return h.Path == path && strings.Contains(h.Text, needle)
	})
}

func describeHits(hits []postgres.SearchHit) string {
	var b strings.Builder
	for _, h := range hits {
		b.WriteString("\n  " + h.Path + " " + h.SymbolFQN + " " + h.Match)
	}
	return b.String()
}

// AC-1, AC-8: части записей через точку и имя JSX-компонента — в tsvector
// куска, записи целиком — тоже.
func TestCodeTsvectorHasDottedParts(t *testing.T) {
	_, pool := indexSearchFixture(t)
	ctx := context.Background()

	lexemes := func(path string) []string {
		t.Helper()
		var out []string
		if err := pool.QueryRow(ctx, `
			SELECT tsvector_to_array(cc.tsv) FROM code_chunks cc
			JOIN code_files cf ON cf.id = cc.code_file_id
			WHERE cf.path = $1`, path).Scan(&out); err != nil {
			t.Fatalf("tsvector %s: %v", path, err)
		}
		return out
	}

	got := lexemes("internal/workflow/mixed.go")
	for _, want := range []string{
		"revokeruntokens", "listattachments", "context", "x", "d", "advance",
		"e.store.revokeruntokens", "api.listattachments", "context.context",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("в tsvector нет %q: %v", want, got)
		}
	}
	// camelCase не дробится (R4).
	for _, unwanted := range []string{"revoke", "run", "tokens"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("camelCase раздроблен: в tsvector есть %q", unwanted)
		}
	}

	if jsx := lexemes("web/src/Page.tsx"); !slices.Contains(jsx, "button") {
		t.Errorf("имя компонента из тега со строковым атрибутом не в tsvector: %v", jsx)
	}
}

// AC-2, AC-3: вызов через точку находится по имени метода, запись целиком —
// по записи; obj?.x, user_id и прочее находят всё, что находили до 000041.
func TestCodeSearchFindsDottedCalls(t *testing.T) {
	store, pool := indexSearchFixture(t)
	ctx := context.Background()
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})

	hits := searchCode(t, engine, "RevokeRunTokens", "text")
	if hitIndex(hits, "internal/workflow/agents.go", "e.store.RevokeRunTokens") < 0 {
		t.Fatalf("вызов через точку не найден по имени метода:%s", describeHits(hits))
	}

	hits = searchCode(t, engine, "e.store.RevokeRunTokens", "text")
	if hitIndex(hits, "internal/workflow/agents.go", "e.store.RevokeRunTokens") < 0 {
		t.Fatalf("запись целиком не найдена:%s", describeHits(hits))
	}

	hits = searchCode(t, engine, "Button", "text")
	if hitIndex(hits, "web/src/Page.tsx", "<Button") < 0 {
		t.Fatalf("JSX-тег не найден по имени компонента:%s", describeHits(hits))
	}

	// Старое выражение колонки — подмножество нового: кусок, который
	// находился прежде, находится и теперь.
	for _, q := range []string{"obj?.items", "user_id", "my-thing", "e.store.RevokeRunTokens", "context.Context", "Advance"} {
		var lost int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM code_chunks
			WHERE to_tsvector('simple', text) @@ sdlc_tsquery('simple', $1)
			  AND NOT tsv @@ sdlc_tsquery('simple', $1)`, q).Scan(&lost); err != nil {
			t.Fatal(err)
		}
		if lost != 0 {
			t.Errorf("запрос %q потерял %d кусков против to_tsvector('simple')", q, lost)
		}
	}
	hits = searchCode(t, engine, "user_id", "text")
	if hitIndex(hits, "internal/workflow/mixed.go", "user_id") < 0 {
		t.Fatalf("user_id не найден:%s", describeHits(hits))
	}
	hits = searchCode(t, engine, "obj?.items", "text")
	if hitIndex(hits, "internal/workflow/mixed.go", "obj?.items") < 0 {
		t.Fatalf("obj?.items не найден:%s", describeHits(hits))
	}
}

// rankOf — ts_rank_cd куска файла по запросу: проверка, что без порядка по
// объявлению вызовы и правда стояли бы выше.
func rankOf(t *testing.T, pool *pgxpool.Pool, path, needle, query string) float64 {
	t.Helper()
	var rank float64
	if err := pool.QueryRow(context.Background(), `
		SELECT ts_rank_cd(cc.tsv, sdlc_tsquery('simple', $3)) FROM code_chunks cc
		JOIN code_files cf ON cf.id = cc.code_file_id
		WHERE cf.path = $1 AND strpos(cc.text, $2) > 0`, path, needle, query).Scan(&rank); err != nil {
		t.Fatalf("ранг %s: %v", path, err)
	}
	return rank
}

// AC-4, AC-5, AC-6: определение — первым, использования — ниже; обычное слово
// фразы не поднимает объявление с этим именем.
func TestCodeSearchPutsDefinitionFirst(t *testing.T) {
	store, pool := indexSearchFixture(t)
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})

	const (
		attDecl   = "internal/store/postgres/attachments.go"
		authDecl  = "internal/store/postgres/auth.go"
		calls     = "internal/workflow/agents.go"
		helpers   = "internal/workflow/helpers_test.go"
		declAtt   = "func (s *Store) ListAttachments("
		declToken = "func (s *Store) RevokeRunTokens("
	)

	t.Run("объявление в начале куска", func(t *testing.T) {
		if rankOf(t, pool, calls, "ListAttachments", "ListAttachments") <= rankOf(t, pool, attDecl, declAtt, "ListAttachments") {
			t.Fatal("набор не проверяет порядок: вызовы и без него ниже объявления")
		}
		hits := searchCode(t, engine, "ListAttachments", "text")
		d, c := hitIndex(hits, attDecl, declAtt), hitIndex(hits, calls, "ListAttachments")
		if d != 0 || c < 0 {
			t.Fatalf("ждали объявление первым и вызовы ниже: объявление %d, вызовы %d:%s", d, c, describeHits(hits))
		}
		if hits[d].SymbolFQN != "internal/store/postgres.Store.ListAttachments" {
			t.Fatalf("кусок с объявлением несёт symbol_fqn %q", hits[d].SymbolFQN)
		}
	})

	t.Run("объявление не в начале куска", func(t *testing.T) {
		if rankOf(t, pool, calls, "RevokeRunTokens", "RevokeRunTokens") <= rankOf(t, pool, authDecl, declToken, "RevokeRunTokens") {
			t.Fatal("набор не проверяет порядок: вызовы и без него ниже объявления")
		}
		hits := searchCode(t, engine, "RevokeRunTokens", "text")
		d, c := hitIndex(hits, authDecl, declToken), hitIndex(hits, calls, "RevokeRunTokens")
		if d != 0 || c < 0 {
			t.Fatalf("ждали объявление первым и вызовы ниже: объявление %d, вызовы %d:%s", d, c, describeHits(hits))
		}
		if fqn := hits[d].SymbolFQN; strings.HasSuffix(fqn, ".RevokeRunTokens") {
			t.Fatalf("кусок начинается с RevokeRunTokens (%q): набор не проверяет слипшиеся объявления", fqn)
		}
	})

	t.Run("обычное слово фразы не поднимает", func(t *testing.T) {
		const q = "revoke task tokens RevokeRunTokens"
		if rankOf(t, pool, helpers, "func task(", q) <= rankOf(t, pool, authDecl, declToken, q) {
			t.Fatal("набор не проверяет слова-имена: помощники и так ниже объявления")
		}
		hits := searchCode(t, engine, q, "text")
		d, h := hitIndex(hits, authDecl, declToken), hitIndex(hits, helpers, "func task(")
		if d != 0 || h < 0 {
			t.Fatalf("ждали RevokeRunTokens первым, func task ниже: объявление %d, помощники %d:%s", d, h, describeHits(hits))
		}
	})
}

// AC-7: в гибридном режиме полнотекстовый список упорядочен так же, слияние
// по рангу прежнее — определение в выдаче выше вызовов.
func TestCodeSearchHybridKeepsDefinitionFirst(t *testing.T) {
	store, pool := indexSearchFixture(t)
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{Embedder: embed.Hash{}})

	const (
		decl  = "internal/store/postgres/blobs.go"
		calls = "internal/workflow/save.go"
	)
	if rankOf(t, pool, calls, "PutBlob", "PutBlob") <= rankOf(t, pool, decl, "func PutBlob(", "PutBlob") {
		t.Fatal("набор не проверяет порядок: вызовы и без него ниже объявления")
	}
	hits := searchCode(t, engine, "PutBlob", "hybrid")
	d, c := hitIndex(hits, decl, "func PutBlob("), hitIndex(hits, calls, "PutBlob")
	if d != 0 || c < 0 || hits[d].Score <= hits[c].Score {
		t.Fatalf("ждали объявление первым и вызовы ниже: объявление %d, вызовы %d:%s", d, c, describeHits(hits))
	}
	if hits[d].Match != "vector+text" && hits[d].Match != "text+vector" {
		t.Fatalf("объявление пришло не из обоих списков: %q", hits[d].Match)
	}

	// Объявление не в начале куска и вызовы, которые ближе по вектору: в
	// выдаче есть оба, полнотекстовая половина участвует.
	hits = searchCode(t, engine, "RevokeRunTokens", "hybrid")
	d = hitIndex(hits, "internal/store/postgres/auth.go", "func (s *Store) RevokeRunTokens(")
	c = hitIndex(hits, "internal/workflow/agents.go", "RevokeRunTokens")
	if d < 0 || c < 0 || !strings.Contains(hits[d].Match, "text") {
		t.Fatalf("ждали объявление и вызовы в гибридной выдаче: объявление %d, вызовы %d:%s", d, c, describeHits(hits))
	}
}

// AC-12: при пустом search_path (так грузится дамп) вставка считает ту же
// tsvector, что при обычном пути.
func TestCodeTsvectorWithEmptySearchPath(t *testing.T) {
	_, pool := indexSearchFixture(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var want string
	if err := tx.QueryRow(ctx, `SELECT sdlc_code_tsvector($1)::text`, searchMixed).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL search_path = ''`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := tx.QueryRow(ctx, `
		INSERT INTO public.code_chunks (code_file_id, project_id, ord, start_line, end_line, text)
		SELECT code_file_id, project_id, 1000, 1, 1, $1 FROM public.code_chunks LIMIT 1
		RETURNING tsv::text`, searchMixed).Scan(&got); err != nil {
		t.Fatalf("вставка при пустом search_path: %v", err)
	}
	if got != want {
		t.Fatalf("tsvector при пустом search_path другая:\n%s\nпротив\n%s", got, want)
	}
}
