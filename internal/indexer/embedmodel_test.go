package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Смена модели эмбеддингов (CLAB-90): поиск не смешивает векторы разных
// моделей, а досчёт переводит индекс на текущую модель целиком.

// namedHash — заглушка с другим именем модели: векторы те же, что у Hash, но
// для системы это другая модель, и смешивать их с прежней нельзя.
type namedHash struct{ name string }

func (n namedHash) Model() string { return n.name }

func (n namedHash) Embed(ctx context.Context, texts []string, kind embed.Kind) ([][]float32, error) {
	return embed.Hash{}.Embed(ctx, texts, kind)
}

const (
	modelA = "model-a"
	modelB = "model-b"
)

func modelSource() *fakeSource {
	readme := "# Бэкенд\n\nСервис цикла разработки.\n"
	return &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {
				"internal/workflow/engine.go": engineV1,
				"internal/workflow/lease.go":  "package workflow\n\nfunc Lease() {}\n",
				"README.md":                   readme,
			},
			"c2": {
				"internal/workflow/engine.go": engineV2,
				"internal/workflow/lease.go":  "package workflow\n\nfunc Lease() {}\n",
				"README.md":                   readme,
			},
			"c3": {
				"internal/workflow/engine.go": engineV2,
				"internal/workflow/lease.go":  "package workflow\n\nfunc Lease() error { return nil }\n",
				"README.md":                   readme,
			},
		},
	}
}

// runCommit индексирует голову источника и возвращает прогон.
func runCommit(t *testing.T, ix *Indexer, src *fakeSource, commit string) *postgres.IndexRun {
	t.Helper()
	ctx := context.Background()
	src.head = commit
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка %s: %v", commit, err)
	}
	run, err := ix.RunNext(ctx)
	if err != nil || run == nil {
		t.Fatalf("прогон %s: %v (run=%v)", commit, err, run)
	}
	return run
}

// addKnowledge кладёт артефакт задачи и проектный документ и раскладывает их
// на куски индексатором ix.
func addKnowledge(t *testing.T, store *postgres.Store, pool *pgxpool.Pool, ix *Indexer) {
	t.Helper()
	ctx := context.Background()
	var taskID, projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id, project_id FROM tasks WHERE key = 'SDLC-1'`).Scan(&taskID, &projectID); err != nil {
		t.Fatal(err)
	}
	art, err := store.PutBlob(ctx, store.DB(),
		[]byte("# Разбор требований\n\nРотация токенов доступа выполняется по расписанию.\n"), "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, status)
		VALUES ($1, $2, 'analysis.brief', 'Разбор требований', $3, 'submitted')`,
		taskID, projectID, art); err != nil {
		t.Fatal(err)
	}
	doc, err := store.PutBlob(ctx, store.DB(),
		[]byte("# Регламент выкладки\n\nВыкладка идёт по тегу после зелёных проверок.\n"), "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO project_documents (project_id, kind, title, source, blob_sha)
		VALUES ($1, 'guide', 'Регламент выкладки', 'inline', $2)`, projectID, doc); err != nil {
		t.Fatal(err)
	}
	artifacts, documents, err := ix.IndexKnowledge(ctx, 10)
	if err != nil || artifacts != 1 || documents != 1 {
		t.Fatalf("раскладка знаний: %d артефактов, %d документов, %v", artifacts, documents, err)
	}
}

// foreignVectors — запрос из критерия К4.1: строки с непустым вектором не
// текущей модели во всех трёх таблицах.
func foreignVectors(t *testing.T, pool *pgxpool.Pool, model string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM (
		  SELECT embedding_model FROM code_chunks     WHERE embedding IS NOT NULL
		  UNION ALL
		  SELECT embedding_model FROM artifact_chunks WHERE embedding IS NOT NULL
		  UNION ALL
		  SELECT embedding_model FROM document_chunks WHERE embedding IS NOT NULL
		) t WHERE embedding_model IS DISTINCT FROM $1`, model).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// runVectors — сколько кусков прогона с вектором модели model и с вектором
// любой другой модели.
func runVectors(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, model string) (own, foreign int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE cc.embedding_model = $2),
		       count(*) FILTER (WHERE cc.embedding_model IS DISTINCT FROM $2)
		FROM code_chunks cc JOIN code_files cf ON cf.id = cc.code_file_id
		WHERE cf.index_run_id = $1 AND cc.embedding IS NOT NULL`, runID, model).Scan(&own, &foreign); err != nil {
		t.Fatal(err)
	}
	return own, foreign
}

// searchAll ищет по трём областям и отдаёт выдачу по каждой.
func searchAll(t *testing.T, engine *workflow.Engine) map[string]workflow.SearchResult {
	t.Helper()
	queries := map[string]string{
		"code":      "Advance",
		"artifacts": "ротация токенов",
		"documents": "регламент выкладки",
	}
	out := map[string]workflow.SearchResult{}
	for scope, q := range queries {
		res, err := engine.SearchContext(context.Background(), "SDLC",
			workflow.SearchInput{Query: q, Scopes: []string{scope}, TopK: 10})
		if err != nil {
			t.Fatalf("поиск %s: %v", scope, err)
		}
		out[scope] = res
	}
	return out
}

func hasMatch(hits []postgres.SearchHit, source string) bool {
	for _, h := range hits {
		if strings.Contains(h.Match, source) {
			return true
		}
	}
	return false
}

// К3.1, К3.2: после смены модели векторная половина не видит кусков прежней
// модели ни в одной области, полнотекстовая — видит; после досчёта векторные
// попадания возвращаются.
func TestSearchIgnoresVectorsOfAnotherModel(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	log := testdb.Logger()
	src := modelSource()

	ixA := New(store, src, namedHash{modelA}, log, Options{ChunkChars: 400, EmbedBatch: 8})
	runCommit(t, ixA, src, "c1")
	addKnowledge(t, store, pool, ixA)

	// Контроль: на своей модели векторные попадания есть во всех областях.
	for scope, res := range searchAll(t, workflow.NewEngine(store, log, workflow.Options{Embedder: namedHash{modelA}})) {
		if res.Mode != "hybrid" || !hasMatch(res.Hits, "vector") {
			t.Fatalf("%s на модели A: mode=%q, векторных попаданий нет: %+v", scope, res.Mode, res.Hits)
		}
	}

	engineB := workflow.NewEngine(store, log, workflow.Options{Embedder: namedHash{modelB}})
	for scope, res := range searchAll(t, engineB) {
		if hasMatch(res.Hits, "vector") {
			t.Fatalf("%s: модель B нашла векторами куски модели A: %+v", scope, res.Hits)
		}
		if !hasMatch(res.Hits, "text") {
			t.Fatalf("%s: полнотекстовых попаданий нет: %+v", scope, res.Hits)
		}
	}

	ixB := New(store, src, namedHash{modelB}, log, Options{ChunkChars: 400, EmbedBatch: 8})
	if _, err := ixB.RefreshEmbeddings(ctx, 100, time.Minute); err != nil {
		t.Fatal(err)
	}
	for scope, res := range searchAll(t, engineB) {
		if !hasMatch(res.Hits, "vector") {
			t.Fatalf("%s: после досчёта на модель B векторных попаданий нет: %+v", scope, res.Hits)
		}
	}
}

// К4.1, К4.2, К4.3: досчёт переводит актуальный индекс, артефакты и документы
// на новую модель одним проходом, хотя кусков больше пачки, снимает векторы
// прежней модели с неактуального прогона, а следующий инкрементальный прогон
// векторов прежней модели не содержит.
func TestRefreshMovesIndexToNewModel(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	log := testdb.Logger()
	src := modelSource()

	ixA := New(store, src, namedHash{modelA}, log, Options{ChunkChars: 400, EmbedBatch: 8})
	stale := runCommit(t, ixA, src, "c1")
	current := runCommit(t, ixA, src, "c2")
	addKnowledge(t, store, pool, ixA)

	if own, _ := runVectors(t, pool, stale.ID, modelA); own == 0 {
		t.Fatal("неактуальный прогон без векторов модели A — проверять нечего")
	}
	if n := foreignVectors(t, pool, modelB); n < 4 {
		t.Fatalf("векторов модели A всего %d — пачка в 1 кусок не покажет цикл", n)
	}

	// Пачка в один кусок на источник: пересчёт обязан пройти циклом, а не
	// остановиться на первой пачке (К4.2).
	ixB := New(store, src, namedHash{modelB}, log, Options{ChunkChars: 400, EmbedBatch: 8})
	stats, err := ixB.RefreshEmbeddings(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("досчёт: %v", err)
	}
	if stats.Embedded <= 3 || stats.Cleared == 0 {
		t.Fatalf("досчёт одним проходом: %+v", stats)
	}

	if n := foreignVectors(t, pool, modelB); n != 0 {
		t.Fatalf("после досчёта осталось %d векторов не модели B", n)
	}
	if own, foreign := runVectors(t, pool, current.ID, modelB); own == 0 || foreign != 0 {
		t.Fatalf("актуальный прогон: модели B %d, прочих %d", own, foreign)
	}
	var missing int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM artifact_chunks WHERE embedding IS NULL OR embedding_model <> $1)
		     + (SELECT count(*) FROM document_chunks WHERE embedding IS NULL OR embedding_model <> $1)
		     + (SELECT count(*) FROM code_chunks cc JOIN code_files cf ON cf.id = cc.code_file_id
		         WHERE cf.index_run_id = $2 AND (cc.embedding IS NULL OR cc.embedding_model <> $1))`,
		modelB, current.ID).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("%d кусков актуального индекса, артефакта и документа без вектора модели B", missing)
	}
	// Неактуальный прогон не пересчитывается, а очищается.
	if own, foreign := runVectors(t, pool, stale.ID, modelB); own != 0 || foreign != 0 {
		t.Fatalf("неактуальный прогон: модели B %d, прочих %d — его векторы обязаны сняться", own, foreign)
	}

	// Повторный проход — пустой.
	if again, err := ixB.RefreshEmbeddings(ctx, 1, time.Minute); err != nil || again != (RefreshStats{}) {
		t.Fatalf("повторный досчёт не пустой: %+v, %v", again, err)
	}

	// К4.3: следующий инкрементальный прогон на модели B.
	next := runCommit(t, ixB, src, "c3")
	if next.Kind != "incremental" || next.FilesReused == 0 {
		t.Fatalf("третий прогон обязан наследовать файлы: %+v", next)
	}
	if _, foreign := runVectors(t, pool, next.ID, modelB); foreign != 0 {
		t.Fatalf("в новом прогоне %d векторов прежней модели", foreign)
	}
}

// К4.3, С5: прогон сразу после смены модели, до досчёта, векторы прежней
// модели не наследует — неизменившиеся файлы уходят на досчёт без вектора.
func TestIncrementalRunDropsVectorsOfAnotherModel(t *testing.T) {
	store, pool := testStore(t)
	log := testdb.Logger()
	src := modelSource()

	ixA := New(store, src, namedHash{modelA}, log, Options{ChunkChars: 400, EmbedBatch: 8})
	runCommit(t, ixA, src, "c1")

	ixB := New(store, src, namedHash{modelB}, log, Options{ChunkChars: 400, EmbedBatch: 8})
	run := runCommit(t, ixB, src, "c2")
	if run.ChunksReused == 0 {
		t.Fatalf("прогон ничего не унаследовал: %+v", run)
	}
	if _, foreign := runVectors(t, pool, run.ID, modelB); foreign != 0 {
		t.Fatalf("унаследовано %d векторов модели A", foreign)
	}
	var unembedded int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM code_chunks cc JOIN code_files cf ON cf.id = cc.code_file_id
		WHERE cf.index_run_id = $1 AND cc.embedding IS NULL`, run.ID).Scan(&unembedded); err != nil {
		t.Fatal(err)
	}
	if unembedded != run.ChunksReused {
		t.Fatalf("без вектора %d кусков, унаследовано %d", unembedded, run.ChunksReused)
	}
}

// Без провайдера наследование векторов прежнее: выключенные эмбеддинги не
// стирают того, что посчитано раньше (сценарий 5).
func TestIncrementalRunWithoutEmbedderKeepsVectors(t *testing.T) {
	store, pool := testStore(t)
	log := testdb.Logger()
	src := modelSource()

	runCommit(t, New(store, src, namedHash{modelA}, log, Options{ChunkChars: 400, EmbedBatch: 8}), src, "c1")
	ix := New(store, src, nil, log, Options{ChunkChars: 400})
	run := runCommit(t, ix, src, "c2")
	if own, _ := runVectors(t, pool, run.ID, modelA); own == 0 {
		t.Fatal("без провайдера унаследованные векторы потеряны")
	}
	if stats, err := ix.RefreshEmbeddings(context.Background(), 100, time.Minute); err != nil || stats != (RefreshStats{}) {
		t.Fatalf("досчёт без провайдера что-то сделал: %+v, %v", stats, err)
	}
}

// fakeOpenAIServer — OpenAI-совместимый сервер: векторы — мешок слов Hash от
// входных текстов, чтобы гибридный поиск находил осмысленное. Запросы
// записываются.
type fakeOpenAIServer struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	paths  []string
}

func (f *fakeOpenAIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	raw := map[string]any{}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	buf, _ := json.Marshal(raw)
	_ = json.Unmarshal(buf, &body)

	f.mu.Lock()
	f.bodies = append(f.bodies, raw)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	vecs, _ := embed.Hash{}.Embed(r.Context(), body.Input, embed.KindDocument)
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]item, len(vecs))
	for i, v := range vecs {
		data[i] = item{Index: i, Embedding: v}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": body.Model})
}

// К1.1, К1.2: OpenAI-совместимый провайдер, собранный из окружения той же
// функцией, что в api и worker, пишет векторы в индекс, и поиск по ним
// гибридный.
func TestOpenAIProviderIndexesAndSearches(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	log := testdb.Logger()

	fake := &fakeOpenAIServer{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	t.Setenv("SDLC_EMBEDDINGS_PROVIDER", "openai")
	t.Setenv("SDLC_EMBEDDINGS_URL", srv.URL+"/v1")
	t.Setenv("SDLC_EMBEDDINGS_API_KEY", "sk-test")
	t.Setenv("SDLC_EMBEDDINGS_MODEL", "text-embedding-3-small")
	t.Setenv("SDLC_VOYAGE_API_KEY", "")
	t.Setenv("SDLC_EMBEDDINGS_SEND_DIMENSIONS", "")
	// Load требует адрес базы; подключается к ней тест сам, через testdb.
	t.Setenv("SDLC_DATABASE_URL", "postgres://unused")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	embedder, err := embed.New(ctx, cfg, log)
	if err != nil || embedder == nil {
		t.Fatalf("сборка эмбеддера: %v, %v", embedder, err)
	}

	src := modelSource()
	ix := New(store, src, embedder, log, Options{ChunkChars: 400, EmbedBatch: cfg.EmbeddingBatch})
	run := runCommit(t, ix, src, "c1")
	if own, foreign := runVectors(t, pool, run.ID, "text-embedding-3-small"); own == 0 || foreign != 0 {
		t.Fatalf("индекс: модели text-embedding-3-small %d, прочих %d", own, foreign)
	}

	engine := workflow.NewEngine(store, log, workflow.Options{Embedder: embedder})
	res, err := engine.SearchContext(ctx, "SDLC", workflow.SearchInput{Query: "Advance", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "hybrid" || !hasMatch(res.Hits, "vector") {
		t.Fatalf("поиск: mode=%q, векторных попаданий нет: %+v", res.Mode, res.Hits)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.bodies) < 3 {
		t.Fatalf("ожидали пробу, индексацию и запрос — было %d запросов", len(fake.bodies))
	}
	for i, body := range fake.bodies {
		if fake.paths[i] != "POST /v1/embeddings" || fake.auth[i] != "Bearer sk-test" {
			t.Fatalf("запрос %d: %s, Authorization %q", i, fake.paths[i], fake.auth[i])
		}
		if body["model"] != "text-embedding-3-small" {
			t.Fatalf("запрос %d: model %v", i, body["model"])
		}
		if _, ok := body["input"].([]any); !ok {
			t.Fatalf("запрос %d: input не массив: %v", i, body["input"])
		}
		if d, ok := body["dimensions"].(float64); !ok || int(d) != embed.Dim {
			t.Fatalf("запрос %d: dimensions %v", i, body["dimensions"])
		}
	}
}
