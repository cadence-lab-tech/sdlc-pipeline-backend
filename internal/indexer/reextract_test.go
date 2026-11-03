package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Доводка индекса до нового извлечения символов (CLAB-369, AC-16): файл, не
// менявшийся между коммитами, но проиндексированный прежним извлечением,
// ближайший прогон пересчитывает из blobs — без дельты, без дрейфа и без
// эмбеддингов для прежнего текста; следующий прогон снова его копирует.

// countingEmbedder — Hash, который помнит каждый текст, ушедший в модель.
type countingEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (c *countingEmbedder) Model() string { return embed.Hash{}.Model() }

func (c *countingEmbedder) Embed(ctx context.Context, texts []string, kind embed.Kind) ([][]float32, error) {
	c.mu.Lock()
	c.texts = append(c.texts, texts...)
	c.mu.Unlock()
	return embed.Hash{}.Embed(ctx, texts, kind)
}

func (c *countingEmbedder) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.texts
	c.texts = nil
	return out
}

const reextractPadding = searchPadding

// Фикстура в raw-строке: прежнее извлечение берёт из неё tokenHash и
// Store.RevokeRunTokens и режет по ним куски. Последний кусок (TestReal)
// одинаков при обоих извлечениях — его вектор обязан переехать из родителя.
const reextractFile = "package indexer\n\n" + reextractPadding +
	"const authFixture = `package postgres\n\n" +
	"func tokenHash(token string) string { return token }\n\n" + reextractPadding +
	"func (s *Store) RevokeRunTokens(ctx context.Context) error { return nil }\n`\n\n" +
	reextractPadding + reextractPadding + reextractPadding +
	"func TestReal(t *testing.T) {}\n"

const reextractPath = "internal/indexer/fixture_test.go"

type fileIndex struct {
	version int
	symbols []string
	fqns    []string
	chunks  map[string]bool
	vectors int
}

func readFileIndex(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, path string) fileIndex {
	t.Helper()
	ctx := context.Background()
	var fi fileIndex
	var fileID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id, symbols_version FROM code_files WHERE index_run_id = $1 AND path = $2`,
		runID, path).Scan(&fileID, &fi.version); err != nil {
		t.Fatalf("файл %s в прогоне: %v", path, err)
	}
	if err := pool.QueryRow(ctx, `SELECT coalesce(array_agg(fqn ORDER BY start_line), '{}') FROM code_symbols WHERE code_file_id = $1`,
		fileID).Scan(&fi.symbols); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT coalesce(symbol_fqn, ''), text_sha, embedding IS NOT NULL FROM code_chunks WHERE code_file_id = $1 ORDER BY ord`, fileID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	fi.chunks = map[string]bool{}
	for rows.Next() {
		var fqn, sha string
		var vec bool
		if err := rows.Scan(&fqn, &sha, &vec); err != nil {
			t.Fatal(err)
		}
		fi.fqns = append(fi.fqns, fqn)
		fi.chunks[sha] = true
		if vec {
			fi.vectors++
		}
	}
	return fi
}

func sha(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func TestReextractUnchangedFileWithoutDriftOrEmbeddings(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {reextractPath: reextractFile, "internal/workflow/engine.go": engineV1},
			"c2": {reextractPath: reextractFile, "internal/workflow/engine.go": engineV2},
			"c3": {reextractPath: reextractFile, "internal/workflow/engine.go": engineV1},
		},
	}
	emb := &countingEmbedder{}

	// Прогон 1 — прежним извлечением, как индекс, построенный до выкладки.
	legacy := New(store, src, emb, testdb.Logger(), Options{ChunkChars: 1600, EmbedBatch: 8})
	legacy.legacySymbols = true
	first := runCommit(t, legacy, src, "c1")
	old := readFileIndex(t, pool, first.ID, reextractPath)
	if old.version != 0 || !slices.Contains(old.symbols, "internal/indexer.tokenHash") ||
		!slices.Contains(old.fqns, "internal/indexer.Store.RevokeRunTokens") {
		t.Fatalf("прогон 1 не похож на прежнее извлечение: %+v", old)
	}

	// Задача сослалась на файл и на ложный символ: доводка не должна поднять
	// по ним дрейф.
	var taskID, projectID, repoID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT t.id, t.project_id, r.id FROM tasks t
		JOIN repositories r ON r.project_id = t.project_id AND r.name = 'sdlc-pipeline-backend'
		WHERE t.key = 'SDLC-1'`).Scan(&taskID, &projectID, &repoID); err != nil {
		t.Fatalf("демо-задача: %v", err)
	}
	blob, err := store.PutBlob(ctx, store.DB(), []byte("# Разбор\n"), "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	var artifactID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, status)
		VALUES ($1, $2, 'analysis.brief', 'Разбор', $3, 'submitted') RETURNING id`,
		taskID, projectID, blob).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	for _, fqn := range []any{nil, "internal/indexer.tokenHash", "internal/indexer.TestReal"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO artifact_code_refs (artifact_id, task_id, repository_id, path, symbol_fqn, ref_kind)
			VALUES ($1, $2, $3, $4, $5, 'reads')`, artifactID, taskID, repoID, reextractPath, fqn); err != nil {
			t.Fatalf("зона задачи: %v", err)
		}
	}
	if _, err := store.EnsureBaselines(ctx, store.DB(), taskID, nil, uuid.Nil); err != nil {
		t.Fatal(err)
	}

	// Кэш эмбеддингов пуст: прежний текст обязан взять вектор у родителя, а
	// не у кэша.
	if _, err := pool.Exec(ctx, `DELETE FROM embedding_cache`); err != nil {
		t.Fatal(err)
	}
	emb.take()

	// Прогон 2 — новый воркер, файл фикстуры не менялся.
	ix := New(store, src, emb, testdb.Logger(), Options{ChunkChars: 1600, EmbedBatch: 8})
	second := runCommit(t, ix, src, "c2")
	got := readFileIndex(t, pool, second.ID, reextractPath)
	if got.version != SymbolsVersion {
		t.Fatalf("версия извлечения файла %d, ждали %d", got.version, SymbolsVersion)
	}
	if !slices.Equal(got.symbols, []string{"internal/indexer.authFixture", "internal/indexer.TestReal"}) {
		t.Fatalf("символы после доводки: %v", got.symbols)
	}
	for _, fqn := range got.fqns {
		if fqn == "internal/indexer.tokenHash" || fqn == "internal/indexer.Store.RevokeRunTokens" {
			t.Fatalf("кусок несёт ложный symbol_fqn %q: %v", fqn, got.fqns)
		}
	}
	if got.vectors != len(got.fqns) {
		t.Fatalf("у пересчитанных кусков векторы %d из %d", got.vectors, len(got.fqns))
	}
	if second.FilesIndexed != 2 || second.FilesReused != 0 {
		t.Fatalf("прогон 2: indexed=%d reused=%d, ждали 2 и 0 (изменённый и доведённый)",
			second.FilesIndexed, second.FilesReused)
	}

	// Модель видела только новый текст фикстуры, прежний переехал.
	var carried, fresh int
	for s := range got.chunks {
		if old.chunks[s] {
			carried++
		} else {
			fresh++
		}
	}
	if carried == 0 || fresh == 0 {
		t.Fatalf("набор не проверяет перенос векторов: прежних кусков %d, новых %d", carried, fresh)
	}
	for _, text := range emb.take() {
		if s := sha(text); old.chunks[s] {
			t.Fatalf("модель заново посчитала прежний текст фикстуры:\n%s", text)
		}
	}

	var changes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM code_file_changes WHERE index_run_id = $1 AND path = $2`,
		second.ID, reextractPath).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if changes != 0 {
		t.Fatalf("доводка извлечения записала %d изменений файла", changes)
	}
	signals, err := store.DetectBaselineDrift(ctx, store.DB(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 0 {
		t.Fatalf("доводка подняла дрейф: %+v", signals)
	}

	// Прогон 3 — файл снова копируется из родителя.
	third := runCommit(t, ix, src, "c3")
	if third.FilesReused != 1 || third.FilesIndexed != 1 {
		t.Fatalf("прогон 3: reused=%d indexed=%d, ждали копию фикстуры", third.FilesReused, third.FilesIndexed)
	}
	if again := readFileIndex(t, pool, third.ID, reextractPath); again.version != SymbolsVersion ||
		!slices.Equal(again.symbols, got.symbols) {
		t.Fatalf("копия потеряла версию или символы: %+v", again)
	}
	if texts := emb.take(); slices.ContainsFunc(texts, func(text string) bool { return got.chunks[sha(text)] }) {
		t.Fatal("копия файла в прогоне 3 ходила в модель")
	}
}

// Содержимого в blobs нет — файл читается у провайдера и всё равно не даёт
// дельты (UC-7, 2б).
func TestReextractWithoutStoredContent(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {reextractPath: reextractFile, "internal/workflow/engine.go": engineV1},
			"c2": {reextractPath: reextractFile, "internal/workflow/engine.go": engineV2},
		},
	}
	legacy := New(store, src, embed.Hash{}, testdb.Logger(), Options{ChunkChars: 1600, EmbedBatch: 8})
	legacy.legacySymbols = true
	first := runCommit(t, legacy, src, "c1")
	// Ссылку на содержимое снимаем: так выглядит вычищенный blob.
	if _, err := pool.Exec(ctx, `UPDATE code_files SET blob_sha = NULL WHERE index_run_id = $1 AND path = $2`,
		first.ID, reextractPath); err != nil {
		t.Fatal(err)
	}

	ix := New(store, src, embed.Hash{}, testdb.Logger(), Options{ChunkChars: 1600, EmbedBatch: 8})
	second := runCommit(t, ix, src, "c2")
	got := readFileIndex(t, pool, second.ID, reextractPath)
	if got.version != SymbolsVersion || slices.Contains(got.symbols, "internal/indexer.tokenHash") {
		t.Fatalf("файл без содержимого в blobs не доведён: %+v", got)
	}
	var changes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM code_file_changes WHERE index_run_id = $1 AND path = $2`,
		second.ID, reextractPath).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if changes != 0 {
		t.Fatalf("доводка без содержимого записала %d изменений", changes)
	}
}
