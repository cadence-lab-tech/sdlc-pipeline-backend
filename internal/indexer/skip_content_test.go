package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Отсев по содержимому (CLAB-361): файл с кириллицей, разрезанной на границе
// 8000 байт, обязан попасть в индекс, а настоящий бинарник и файл с битыми
// байтами за пределами головы — нет, и прогон от них не падает.

// cyrillicSplit — валидный файл больше 8000 байт, у которого «я» занимает
// байты 7999-8000: срез по 8000 байт режет её пополам.
func cyrillicSplit() string {
	return strings.Repeat("a", 7999) + "я\n// Токен задачи гасится вместе с арендой.\n" +
		"func RevokeRunTokens() {}\n"
}

func indexedPaths(t *testing.T, pool *pgxpool.Pool, runID any) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT path FROM code_files WHERE index_run_id = $1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out[p] = true
	}
	return out
}

func TestRunIndexesSplitRuneFileAndSkipsBinaryVisibly(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()
	spy := &logSpy{}

	good := cyrillicSplit()
	badTail := strings.Repeat("a", 9000) + "\xff\n"
	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {
				"internal/workflow/engine.go": engineV1,
				"internal/store/auth.go":      good,
				"tools/blob":                  "ELF\x00\x01\x02binary",
				"tools/broken":                badTail,
			},
		},
	}
	ix := New(store, src, embed.Hash{}, spy.logger(), Options{ChunkChars: 400, EmbedBatch: 8})
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatalf("постановка в очередь: %v", err)
	}
	run, err := ix.RunNext(ctx)
	if err != nil || run == nil {
		t.Fatalf("прогон: %v (run=%v)", err, run)
	}
	if run.State != "completed" {
		t.Fatalf("прогон в состоянии %q (%v), ждали completed", run.State, run.Error)
	}
	if run.FilesIndexed != 2 {
		t.Fatalf("проиндексировано %d файлов, ждали 2", run.FilesIndexed)
	}
	paths := indexedPaths(t, pool, run.ID)
	if len(paths) != 2 || !paths["internal/workflow/engine.go"] || !paths["internal/store/auth.go"] {
		t.Fatalf("в code_files ждали ровно engine.go и auth.go: %v", paths)
	}

	// Символы файла с кириллицей на месте, и полнотекстовый поиск по имени
	// функции находит именно его.
	var symbols int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM code_symbols s JOIN code_files f ON f.id = s.code_file_id
		WHERE f.index_run_id = $1 AND f.path = 'internal/store/auth.go' AND s.name = 'RevokeRunTokens'`,
		run.ID).Scan(&symbols); err != nil {
		t.Fatal(err)
	}
	if symbols != 1 {
		t.Fatalf("символ RevokeRunTokens: %d записей, ждали 1", symbols)
	}
	engine := workflow.NewEngine(store, testdb.Logger(), workflow.Options{})
	res, err := engine.SearchContext(ctx, "SDLC", workflow.SearchInput{
		Query: "RevokeRunTokens", Scopes: []string{"code"}, TopK: 5,
	})
	if err != nil {
		t.Fatalf("поиск: %v", err)
	}
	found := false
	for _, h := range res.Hits {
		if h.Path == "internal/store/auth.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("поиск не нашёл файл с кириллицей: %+v", res.Hits)
	}

	// Отсев виден: ровно один warn на файл с полями и причиной.
	warns := map[string]logRecord{}
	for _, r := range spy.records {
		if r.message == "файл отсеян по содержимому и в индекс не попал" {
			if r.level.String() != "WARN" {
				t.Fatalf("уровень %s, ждали WARN", r.level)
			}
			path := r.attrs["path"].String()
			if _, dup := warns[path]; dup {
				t.Fatalf("две записи на %s", path)
			}
			warns[path] = r
		}
	}
	if len(warns) != 2 {
		t.Fatalf("записей об отсеве %d, ждали 2: %v", len(warns), warns)
	}
	for path, wantReason := range map[string]string{
		"tools/blob":   "нулевой байт на смещении 3",
		"tools/broken": "невалидный UTF-8 на смещении 9000",
	} {
		r, ok := warns[path]
		if !ok {
			t.Fatalf("нет записи об отсеве %s", path)
		}
		if r.attrs["reason"].String() != wantReason {
			t.Errorf("%s: причина %q, ждали %q", path, r.attrs["reason"].String(), wantReason)
		}
		for _, key := range []string{"repository", "commit", "hint"} {
			if r.attrs[key].String() == "" {
				t.Errorf("%s: нет поля %s", path, key)
			}
		}
	}

	// Сверка одной строкой: indexed + reused + skipped = considered.
	rec, ok := spy.find("репозиторий проиндексирован")
	if !ok {
		t.Fatal("сводки прогона нет в логе")
	}
	indexed, reused := rec.attrs["files_indexed"].Int64(), rec.attrs["files_reused"].Int64()
	skipped, considered := rec.attrs["files_skipped_content"].Int64(), rec.attrs["files_considered"].Int64()
	if considered != 4 || skipped != 2 || indexed+reused+skipped != considered {
		t.Fatalf("сверка: indexed=%d reused=%d skipped=%d considered=%d", indexed, reused, skipped, considered)
	}
}

// Файл, выпавший в прошлом прогоне, подхватывается следующим коммитом, хотя
// его sha не изменился: наследуется только то, что было в прошлом прогоне, а
// остальное выкачивается заново и пишется как added. Так выпавшие из-за
// CLAB-361 файлы доходят до индекса после выката без миграции данных.
func TestSkippedFileIsPickedUpByNextCommit(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	good := cyrillicSplit()
	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {"internal/workflow/engine.go": engineV1, "internal/store/auth.go": good},
			"c2": {"internal/workflow/engine.go": engineV1, "internal/store/auth.go": good,
				"README.md": "# Бэкенд\n"},
		},
	}
	ix := newTestIndexer(t, store, src)

	// До выката файл отсеян (здесь — настройкой репозитория, на проде — срезом).
	if _, err := pool.Exec(ctx, `UPDATE repositories SET exclude_globs = '{internal/store/*}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := ix.RunNext(ctx)
	if err != nil || first == nil {
		t.Fatalf("прогон A: %v", err)
	}
	if paths := indexedPaths(t, pool, first.ID); paths["internal/store/auth.go"] {
		t.Fatalf("прогон A не должен был взять файл: %v", paths)
	}

	// После выката фильтр файл не отсеивает; sha тот же.
	if _, err := pool.Exec(ctx, `UPDATE repositories SET exclude_globs = '{}'`); err != nil {
		t.Fatal(err)
	}
	src.head = "c2"
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := ix.RunNext(ctx)
	if err != nil || second == nil {
		t.Fatalf("прогон B: %v", err)
	}
	if second.Kind != "incremental" || second.ParentID == nil || *second.ParentID != first.ID {
		t.Fatalf("прогон B обязан быть инкрементальным: %+v", second)
	}
	if !indexedPaths(t, pool, second.ID)["internal/store/auth.go"] {
		t.Fatal("выпавший файл не попал в индекс следующего коммита")
	}
	var change string
	if err := pool.QueryRow(ctx, `
		SELECT change_type FROM code_file_changes
		WHERE index_run_id = $1 AND path = 'internal/store/auth.go'`, second.ID).Scan(&change); err != nil {
		t.Fatalf("дельта файла: %v", err)
	}
	if change != "added" {
		t.Fatalf("change_type = %q, ждали added", change)
	}
	// engine.go наследован (reused), auth.go и README.md выкачаны (indexed).
	if second.FilesReused != 1 || second.FilesIndexed != 2 {
		t.Fatalf("прогон B: indexed=%d reused=%d, ждали 2 и 1", second.FilesIndexed, second.FilesReused)
	}
}
