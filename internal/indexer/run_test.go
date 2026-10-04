package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Интеграционные проверки прогона индексации: инкрементальность — это
// свойство связки «источник + база», и на моках она не проверяется.
//
//	SDLC_TEST_DATABASE_URL=postgres://sdlc:sdlc@localhost:55432/sdlc_test go test ./...
//
// База обязана быть выделенной: тест сносит схему public целиком.

// fakeSource — репозиторий, живущий в памяти. Коммит здесь просто имя набора
// файлов, а sha объекта — sha содержимого: индексатору достаточно того, что
// одинаковое содержимое даёт одинаковый sha, а разное — разный.
type fakeSource struct {
	repo    string
	head    string
	commits map[string]map[string]string
}

func (f *fakeSource) ForRepository(context.Context, Credentials) (Source, error) { return f, nil }

func (f *fakeSource) Head(_ context.Context, repo Repo, _ string) (string, error) {
	if repo.Name != f.repo {
		return "", errors.New("репозиторий недоступен")
	}
	return f.head, nil
}

func (f *fakeSource) Tree(_ context.Context, repo Repo, commit string) ([]Entry, bool, error) {
	if repo.Name != f.repo {
		return nil, false, errors.New("репозиторий недоступен")
	}
	files, ok := f.commits[commit]
	if !ok {
		return nil, false, fmt.Errorf("коммита %s нет", commit)
	}
	out := make([]Entry, 0, len(files))
	for path, content := range files {
		out = append(out, Entry{Path: path, SHA: blobSHA(content), Size: int64(len(content))})
	}
	return out, false, nil
}

func (f *fakeSource) Blob(_ context.Context, _ Repo, sha string) ([]byte, error) {
	for _, files := range f.commits {
		for _, content := range files {
			if blobSHA(content) == sha {
				return []byte(content), nil
			}
		}
	}
	return nil, errors.New("объект не найден")
}

func blobSHA(content string) string {
	return fmt.Sprintf("%x", len(content)) + "-" + fmt.Sprintf("%x", hash32(content))
}

func hash32(s string) uint32 {
	var h uint32 = 2166136261
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// Файл нарочно с длинными комментариями: куски режутся по объявлениям, но
// мелочь склеивается в один — иначе на этом файле не видно, что кэш
// эмбеддингов закрывает неизменившиеся куски.
const engineV1 = `package workflow

// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
type Engine struct {
	store *Store
}

// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
func NewEngine(store *Store) *Engine {
	return &Engine{store: store}
}

// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
func (e *Engine) Advance(ctx context.Context) error {
	return nil
}
`

const engineV2 = `package workflow

// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
type Engine struct {
	store *Store
}

// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
func NewEngine(store *Store) *Engine {
	return &Engine{store: store}
}

// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
// Комментарий, объясняющий, почему это устроено именно так, а не иначе.
func (e *Engine) Advance(ctx context.Context) error {
	return e.store.Save(ctx)
}
`

func testStore(t *testing.T) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	return testdb.Store(t)
}

func newTestIndexer(t *testing.T, store *postgres.Store, src Source) *Indexer {
	t.Helper()
	return New(store, src, embed.Hash{}, testdb.Logger(), Options{ChunkChars: 400, EmbedBatch: 8})
}

// Полный прогон, затем инкрементальный: неизменившийся файл обязан
// наследоваться целиком, а изменившийся — переехать в новую дельту.
func TestIncrementalRunReusesUnchangedFiles(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {
				"internal/workflow/engine.go": engineV1,
				"README.md":                   "# Бэкенд\n\nСервис цикла разработки.\n",
				"internal/legacy/old.go":      "package legacy\n\nfunc Old() {}\n",
				"node_modules/react/index.js": "module.exports = {}\n",
			},
			"c2": {
				"internal/workflow/engine.go": engineV2,
				"README.md":                   "# Бэкенд\n\nСервис цикла разработки.\n",
				"internal/workflow/lease.go":  "package workflow\n\nfunc Lease() {}\n",
			},
		},
	}
	ix := newTestIndexer(t, store, src)

	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка в очередь: %v", err)
	}
	first, err := ix.RunNext(ctx)
	if err != nil || first == nil {
		t.Fatalf("первый прогон: %v (run=%v)", err, first)
	}
	if first.Kind != "full" {
		t.Fatalf("первый прогон ветки полный, а не %q", first.Kind)
	}
	// node_modules в индекс не попадает.
	if first.FilesIndexed != 3 || first.FilesReused != 0 {
		t.Fatalf("первый прогон: indexed=%d reused=%d", first.FilesIndexed, first.FilesReused)
	}
	if first.ChunksIndexed == 0 || first.ChunksEmbedded == 0 {
		t.Fatalf("первый прогон не посчитал куски: %+v", first)
	}

	src.head = "c2"
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка второго коммита: %v", err)
	}
	second, err := ix.RunNext(ctx)
	if err != nil || second == nil {
		t.Fatalf("второй прогон: %v", err)
	}

	if second.Kind != "incremental" || second.ParentID == nil || *second.ParentID != first.ID {
		t.Fatalf("второй прогон обязан наследовать первому: %+v", second)
	}
	// README.md не изменился — он наследуется без сети и без модели.
	if second.FilesReused != 1 {
		t.Fatalf("ожидали один унаследованный файл, получили %d", second.FilesReused)
	}
	if second.FilesIndexed != 2 {
		t.Fatalf("изменённый и новый файл — это два, а не %d", second.FilesIndexed)
	}
	if second.ChunksReused == 0 {
		t.Fatalf("куски унаследованного файла обязаны копироваться: %+v", second)
	}
	// Часть кусков изменённого файла осталась прежней — за них платить не надо.
	if second.ChunksEmbedded >= second.ChunksIndexed {
		t.Fatalf("кэш эмбеддингов не сработал: embedded=%d indexed=%d",
			second.ChunksEmbedded, second.ChunksIndexed)
	}

	changes := map[string]string{}
	symbols := map[string][]string{}
	rows, err := pool.Query(ctx,
		`SELECT path, change_type, changed_symbols FROM code_file_changes WHERE index_run_id = $1`, second.ID)
	if err != nil {
		t.Fatalf("чтение дельты: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path, kind string
		var syms []string
		if err := rows.Scan(&path, &kind, &syms); err != nil {
			t.Fatalf("разбор дельты: %v", err)
		}
		changes[path] = kind
		symbols[path] = syms
	}

	want := map[string]string{
		"internal/workflow/engine.go": "modified",
		"internal/workflow/lease.go":  "added",
		"internal/legacy/old.go":      "deleted",
	}
	for path, kind := range want {
		if changes[path] != kind {
			t.Fatalf("%s: ожидали %q, получили %q (вся дельта: %v)", path, kind, changes[path], changes)
		}
	}
	if changes["README.md"] != "" {
		t.Fatalf("неизменившийся файл в дельте быть не должен: %v", changes)
	}
	if got := symbols["internal/workflow/engine.go"]; len(got) != 1 || got[0] != "internal/workflow.Engine.Advance" {
		t.Fatalf("дельта символов неверна: %v", got)
	}

	// Коммит уже проиндексирован — второй раз он в очередь не встаёт.
	if n, err := ix.Enqueue(ctx); err != nil || n != 0 {
		t.Fatalf("повторная постановка того же коммита: n=%d err=%v", n, err)
	}
}

// Дрейф: код, на который ссылается артефакт задачи, уехал в main.
func TestDriftSignalFromIndexRun(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {"internal/workflow/engine.go": engineV1},
			"c2": {"internal/workflow/engine.go": engineV2},
		},
	}
	ix := newTestIndexer(t, store, src)

	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := ix.RunNext(ctx)
	if err != nil || first == nil {
		t.Fatalf("первый прогон: %v", err)
	}

	var taskID, projectID, repoID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT t.id, t.project_id, r.id
		FROM tasks t
		JOIN repositories r ON r.project_id = t.project_id AND r.name = 'sdlc-pipeline-backend'
		WHERE t.key = 'SDLC-1'`).Scan(&taskID, &projectID, &repoID); err != nil {
		t.Fatalf("демо-задача не найдена: %v", err)
	}

	sha, err := store.PutBlob(ctx, store.DB(), []byte("# Разбор\n"), "text/markdown")
	if err != nil {
		t.Fatalf("blob: %v", err)
	}
	var artifactID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, status)
		VALUES ($1, $2, 'analysis.brief', 'Разбор', $3, 'submitted') RETURNING id`,
		taskID, projectID, sha).Scan(&artifactID); err != nil {
		t.Fatalf("артефакт: %v", err)
	}
	// Задача сослалась ровно на тот символ, который во втором коммите изменится.
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifact_code_refs (artifact_id, task_id, repository_id, path, symbol_fqn, ref_kind)
		VALUES ($1, $2, $3, 'internal/workflow/engine.go', 'internal/workflow.Engine.Advance', 'reads')`,
		artifactID, taskID, repoID); err != nil {
		t.Fatalf("зона задачи: %v", err)
	}
	if _, err := store.EnsureBaselines(ctx, store.DB(), taskID, nil, uuid.Nil); err != nil {
		t.Fatalf("базовая линия: %v", err)
	}

	var baseline string
	if err := pool.QueryRow(ctx,
		`SELECT commit_sha FROM task_baselines WHERE task_id = $1 AND repository_id = $2`,
		taskID, repoID).Scan(&baseline); err != nil {
		t.Fatalf("базовая линия не поставилась: %v", err)
	}
	if baseline != first.CommitSHA {
		t.Fatalf("базовая линия должна встать на актуальный индекс: %q вместо %q", baseline, first.CommitSHA)
	}

	src.head = "c2"
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := ix.RunNext(ctx)
	if err != nil || second == nil {
		t.Fatalf("второй прогон: %v", err)
	}

	signals, err := store.DetectBaselineDrift(ctx, store.DB(), second.ID)
	if err != nil {
		t.Fatalf("детектор: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("ожидали один сигнал, получили %d", len(signals))
	}
	// Изменился именно тот символ, на который есть прямая ссылка.
	if signals[0].Severity != "high" || signals[0].Kind != "baseline_drift" {
		t.Fatalf("уровень сигнала неверен: %+v", signals[0])
	}

	// Повторный проход детектора дубликата не заводит.
	again, err := store.DetectBaselineDrift(ctx, store.DB(), second.ID)
	if err != nil || len(again) != 0 {
		t.Fatalf("детектор не идемпотентен: %d сигналов, %v", len(again), err)
	}
}

// Поиск: код из индекса и артефакты задач находятся одним запросом, а
// источник у каждого куска остаётся видимым.
func TestSearchFindsCodeAndArtifacts(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {"internal/workflow/engine.go": engineV1},
		},
	}
	ix := newTestIndexer(t, store, src)
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.RunNext(ctx); err != nil {
		t.Fatal(err)
	}

	var taskID, projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id, project_id FROM tasks WHERE key = 'SDLC-1'`).Scan(&taskID, &projectID); err != nil {
		t.Fatal(err)
	}
	content := []byte("# Разбор требований\n\nРотация токенов доступа выполняется по расписанию.\n")
	sha, err := store.PutBlob(ctx, store.DB(), content, "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, status)
		VALUES ($1, $2, 'analysis.brief', 'Разбор требований', $3, 'submitted')`,
		taskID, projectID, sha); err != nil {
		t.Fatal(err)
	}

	artifacts, _, err := ix.IndexKnowledge(ctx, 10)
	if err != nil || artifacts != 1 {
		t.Fatalf("векторизация артефактов: %d, %v", artifacts, err)
	}

	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{Embedder: embed.Hash{}})

	code, err := engine.SearchContext(ctx, "SDLC", workflow.SearchInput{Query: "Advance", TopK: 5})
	if err != nil {
		t.Fatalf("поиск по коду: %v", err)
	}
	if code.Mode != "hybrid" {
		t.Fatalf("с провайдером эмбеддингов поиск гибридный, а не %q", code.Mode)
	}
	found := false
	for _, h := range code.Hits {
		if h.Scope == "code" && h.Path == "internal/workflow/engine.go" && h.Repository != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("код не найден: %+v", code.Hits)
	}

	docs, err := engine.SearchContext(ctx, "SDLC", workflow.SearchInput{
		Query: "ротация токенов", Scopes: []string{"artifacts"}, TopK: 5,
	})
	if err != nil {
		t.Fatalf("поиск по артефактам: %v", err)
	}
	if len(docs.Hits) == 0 || docs.Hits[0].TaskKey != "SDLC-1" {
		t.Fatalf("артефакт задачи не найден: %+v", docs.Hits)
	}
}

// Без провайдера эмбеддингов система обязана работать, просто хуже: индекс
// строится, поиск остаётся полнотекстовым, а векторы досчитываются потом.
func TestWorksWithoutEmbedder(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	log := testdb.Logger()

	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {"internal/workflow/engine.go": engineV1},
		},
	}
	ix := New(store, src, nil, log, Options{ChunkChars: 400})
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := ix.RunNext(ctx)
	if err != nil || run == nil {
		t.Fatalf("прогон без эмбеддера: %v", err)
	}
	if run.ChunksIndexed == 0 || run.ChunksEmbedded != 0 {
		t.Fatalf("без провайдера куски пишутся без векторов: %+v", run)
	}

	engine := workflow.NewEngine(store, log, workflow.Options{})
	res, err := engine.SearchContext(ctx, "SDLC", workflow.SearchInput{Query: "Advance"})
	if err != nil {
		t.Fatalf("полнотекстовый поиск: %v", err)
	}
	if res.Mode != "text" || len(res.Hits) == 0 {
		t.Fatalf("ожидали полнотекстовую выдачу: mode=%q hits=%d", res.Mode, len(res.Hits))
	}

	// Досчёт векторов после того, как провайдер появился.
	withEmbedder := New(store, src, embed.Hash{}, log, Options{ChunkChars: 400})
	n, err := withEmbedder.BackfillEmbeddings(ctx, 100)
	if err != nil || n == 0 {
		t.Fatalf("досчёт эмбеддингов: %d, %v", n, err)
	}
}

// Репозиторий отвязали, пока прогон уже шёл: строка прогона ушла каскадом.
// Воркер обязан завершиться ошибкой и не воскресить ни файла — это и есть
// «удаление окончательно».
func TestRunOfDetachedRepositoryWritesNothing(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{})

	if _, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{Repository: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{
		repo: "api",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {"internal/workflow/engine.go": engineV1},
		},
	}
	ix := newTestIndexer(t, store, src)
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка в очередь: %v", err)
	}
	run, ok, err := store.ClaimIndexRun(ctx)
	if err != nil || !ok {
		t.Fatalf("забрать прогон: %v (ok=%v)", err, ok)
	}

	if _, _, err := engine.RemoveRepository(ctx, "SDLC", "acme/api", uuid.Nil); err != nil {
		t.Fatalf("отвязка: %v", err)
	}

	if _, _, err := ix.process(ctx, run); err == nil {
		t.Fatal("прогон по отвязанному репозиторию завершился успехом")
	}
	var files int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM code_files WHERE index_run_id = $1`, run.ID).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("прогон записал %d файлов по удалённому репозиторию", files)
	}
}

// То же для удаления проекта целиком: репозитории уходят вместе с ним, и
// прогон, забранный воркером до удаления, не должен ничего записать.
func TestRunOfDeletedProjectWritesNothing(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{})

	if _, _, err := engine.AddRepository(ctx, "SDLC", workflow.AddRepositoryInput{Repository: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{
		repo: "api",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {"internal/workflow/engine.go": engineV1},
		},
	}
	ix := newTestIndexer(t, store, src)
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка в очередь: %v", err)
	}
	run, ok, err := store.ClaimIndexRun(ctx)
	if err != nil || !ok {
		t.Fatalf("забрать прогон: %v (ok=%v)", err, ok)
	}

	if _, _, err := engine.DeleteProject(ctx, "SDLC", uuid.Nil); err != nil {
		t.Fatalf("удаление проекта: %v", err)
	}

	if _, _, err := ix.process(ctx, run); err == nil {
		t.Fatal("прогон по удалённому проекту завершился успехом")
	}
	var files int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM code_files WHERE index_run_id = $1`, run.ID).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("прогон записал %d файлов по удалённому проекту", files)
	}
}

// Индексатор собран с пределом файла выше того, что примет хранилище
// содержимого (CLAB-82; config.Load такую пару не пропускает). Файл крупнее
// предела блоба пропускается, как файл крупнее предела индексатора: без
// скачивания, без ошибки прогона, со строкой warn в логе.
func TestFileAboveBlobLimitSkipped(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	const blobMax = 4096
	store.UseBlobStore(nil, 1024, blobMax)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	big := "// " + strings.Repeat("x", blobMax) + "\n"
	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {
				"internal/workflow/engine.go": engineV1,
				"internal/generated/big.go":   big,
			},
		},
	}
	// Предел индексатора по умолчанию — 512 КиБ, выше предела блоба.
	ix := New(store, src, embed.Hash{}, log, Options{ChunkChars: 400, EmbedBatch: 8})
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка в очередь: %v", err)
	}
	run, err := ix.RunNext(ctx)
	if err != nil || run == nil {
		t.Fatalf("прогон упал на файле крупнее предела блоба: %v (лог: %s)", err, logBuf.String())
	}
	if run.State != "completed" {
		t.Fatalf("прогон в состоянии %q, ждали completed", run.State)
	}
	if run.FilesIndexed != 1 {
		t.Fatalf("проиндексировано %d файлов, ждали один обычный", run.FilesIndexed)
	}

	paths := map[string]bool{}
	rows, err := pool.Query(ctx, `SELECT path FROM code_files WHERE index_run_id = $1`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		paths[p] = true
	}
	if !paths["internal/workflow/engine.go"] || paths["internal/generated/big.go"] {
		t.Fatalf("в code_files ждали только обычный файл: %v", paths)
	}
	var bigBlobs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM blobs WHERE size_bytes = $1`, len(big)).Scan(&bigBlobs); err != nil {
		t.Fatal(err)
	}
	if bigBlobs != 0 {
		t.Fatal("содержимое крупного файла легло в blobs")
	}

	out := logBuf.String()
	var warn string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "internal/generated/big.go") {
			warn = line
		}
	}
	if warn == "" {
		t.Fatalf("в логе нет warn о пропущенном файле: %s", out)
	}
	for _, want := range []string{"blob_limit=" + strconv.Itoa(blobMax), "size=" + strconv.Itoa(len(big))} {
		if !strings.Contains(warn, want) {
			t.Fatalf("в строке warn нет %q: %s", want, warn)
		}
	}
}

// Предел индексатора ниже предела блоба — отсев по-прежнему по нему, молча:
// охрана хранилища не срабатывает и в лог не пишет.
func TestIndexLimitBelowBlobLimitSkipsSilently(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	store.UseBlobStore(nil, 1024, 64<<20)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {
				"internal/workflow/lease.go": "package workflow\n\nfunc Lease() {}\n",
				"internal/generated/big.go":  "// " + strings.Repeat("y", 2000) + "\n",
			},
		},
	}
	ix := New(store, src, embed.Hash{}, log, Options{MaxFileBytes: 1000, ChunkChars: 400, EmbedBatch: 8})
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := ix.RunNext(ctx)
	if err != nil || run == nil || run.FilesIndexed != 1 {
		t.Fatalf("прогон: %v (run=%+v)", err, run)
	}
	if strings.Contains(logBuf.String(), "internal/generated/big.go") {
		t.Fatalf("отсев пределом индексатора не должен писать в лог: %s", logBuf.String())
	}
}
