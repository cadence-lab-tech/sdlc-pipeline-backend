package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// fakeBlobs — внешнее хранилище в памяти. Проверяется то, ради чего порядок
// «строка, потом объект» и выбран: объект уходит вслед за строкой, а сбой
// удаления не воскрешает строку.
type fakeBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
	failing bool
	deletes int
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{objects: map[string][]byte{}} }

func (f *fakeBlobs) Put(_ context.Context, sha string, content []byte, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uri := "fake://" + sha
	// Адресация по содержимому, как у настоящих реализаций: повторная запись
	// тех же байтов объект не перезаливает.
	if _, ok := f.objects[uri]; !ok {
		f.objects[uri] = content
	}
	return uri, nil
}

func (f *fakeBlobs) Get(_ context.Context, uri string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.objects[uri]
	if !ok {
		return nil, errors.New("нет объекта " + uri)
	}
	return content, nil
}

func (f *fakeBlobs) Delete(_ context.Context, uri string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.failing {
		return errors.New("хранилище недоступно")
	}
	delete(f.objects, uri)
	return nil
}

func (f *fakeBlobs) has(uri string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[uri]
	return ok
}

// gcFixture — база со свежей схемой и демонстрационным проектом плюс
// подставное внешнее хранилище.
type gcFixture struct {
	t     *testing.T
	ctx   context.Context
	store *postgres.Store
	pool  *pgxpool.Pool
	blobs *fakeBlobs
}

func newGCFixture(t *testing.T) *gcFixture {
	t.Helper()
	store, pool := testdb.Store(t)
	blobs := newFakeBlobs()
	// Порог выноса занижен до байта: так каждый блоб теста уходит во внешнее
	// хранилище, и путь «строка, потом объект» проверяется целиком.
	store.UseBlobStore(blobs, 1, 1<<20)
	return &gcFixture{t: t, ctx: context.Background(), store: store, pool: pool, blobs: blobs}
}

// put кладёт содержимое и возвращает sha. Транзакция своя: PutBlob берёт
// advisory-замок до конца транзакции, и держать его дольше нужного незачем.
func (f *gcFixture) put(content string) string {
	f.t.Helper()
	var sha string
	err := f.store.InTx(f.ctx, func(tx pgx.Tx) error {
		var err error
		sha, err = f.store.PutBlob(f.ctx, tx, []byte(content), "text/plain")
		return err
	})
	if err != nil {
		f.t.Fatalf("запись блоба: %v", err)
	}
	return sha
}

func (f *gcFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *gcFixture) alive(sha string) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM blobs WHERE sha256 = $1`, sha).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

// demoTask — задача демонстрационного проекта: артефакту нужны и задача, и
// проект, а заводить их руками в каждом тесте незачем.
func (f *gcFixture) demoTask() (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	var taskID, projectID uuid.UUID
	err := f.pool.QueryRow(f.ctx, `SELECT id, project_id FROM tasks ORDER BY key LIMIT 1`).Scan(&taskID, &projectID)
	if err != nil {
		f.t.Fatalf("задача демонстрационного проекта: %v", err)
	}
	return taskID, projectID
}

// artifact вешает на блоб ссылку из artifacts и возвращает id — по нему
// следующая версия ставит supersedes_id.
func (f *gcFixture) artifact(sha, kind string, supersedes *uuid.UUID) uuid.UUID {
	f.t.Helper()
	taskID, projectID := f.demoTask()
	var id uuid.UUID
	err := f.pool.QueryRow(f.ctx, `
		INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, version, supersedes_id, status)
		VALUES ($1, $2, $3, $3, $4, CASE WHEN $5::uuid IS NULL THEN 1 ELSE 2 END, $5, 'submitted')
		RETURNING id`, taskID, projectID, kind, sha, supersedes).Scan(&id)
	if err != nil {
		f.t.Fatalf("артефакт: %v", err)
	}
	return id
}

// repository заводит репозиторий проекта: под ним живут прогоны индексации.
func (f *gcFixture) repository(projectID uuid.UUID, name string) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	err := f.pool.QueryRow(f.ctx, `
		INSERT INTO repositories (project_id, provider, owner, name, default_branch, clone_url)
		VALUES ($1, 'github', 'cadence-lab-tech', $2, 'main',
		        'https://github.com/cadence-lab-tech/' || $2 || '.git')
		RETURNING id`, projectID, name).Scan(&id)
	if err != nil {
		f.t.Fatalf("репозиторий: %v", err)
	}
	return id
}

// codeFile вешает на блоб ссылку из прогона индексации.
func (f *gcFixture) codeFile(repoID uuid.UUID, sha, path string) {
	f.t.Helper()
	var runID uuid.UUID
	err := f.pool.QueryRow(f.ctx, `
		INSERT INTO repo_index_runs (repository_id, commit_sha, state)
		VALUES ($1, $2, 'completed')
		ON CONFLICT (repository_id, commit_sha) DO UPDATE SET state = 'completed'
		RETURNING id`, repoID, "commit-"+path).Scan(&runID)
	if err != nil {
		f.t.Fatalf("прогон индексации: %v", err)
	}
	f.exec(`INSERT INTO code_files (index_run_id, repository_id, path, blob_sha)
	        VALUES ($1, $2, $3, $4)`, runID, repoID, path, sha)
}

// document вешает на блоб ссылку из документа проекта.
func (f *gcFixture) document(projectID uuid.UUID, sha, title string) {
	f.t.Helper()
	f.exec(`INSERT INTO project_documents (project_id, kind, title, source, blob_sha)
	        VALUES ($1, 'guide', $2, 'inline', $3)`, projectID, title, sha)
}

// collect гоняет проход сборщика и возвращает статистику вместе со списком
// блобов, чьи объекты удалить не вышло.
func (f *gcFixture) collect(opts postgres.BlobGCOptions) (postgres.BlobGCStats, []string) {
	f.t.Helper()
	var failed []string
	stats, err := f.store.CollectBlobs(f.ctx, opts, func(sha string, _ error) {
		failed = append(failed, sha)
	})
	if err != nil {
		f.t.Fatalf("проход сборщика: %v", err)
	}
	return stats, failed
}

func gcOpts() postgres.BlobGCOptions {
	return postgres.BlobGCOptions{Limit: 100}
}

// Мусор — блоб, на который не ссылается ни артефакт, ни файл прогона
// индексации, ни документ проекта. Ссылок на blobs(sha256) в схеме ровно три,
// и каждая обязана держать блоб.
func TestBlobGCKeepsReferenced(t *testing.T) {
	f := newGCFixture(t)
	_, projectID := f.demoTask()
	repoID := f.repository(projectID, "keeps-referenced")

	byArtifact := f.put("содержимое артефакта")
	f.artifact(byArtifact, "analysis.requirements", nil)

	byIndex := f.put("содержимое файла в индексе")
	f.codeFile(repoID, byIndex, "internal/blobstore/blobstore.go")

	byDocument := f.put("содержимое документа проекта")
	f.document(projectID, byDocument, "Гайд")

	orphan := f.put("никому не нужное содержимое")

	stats, failed := f.collect(gcOpts())
	if stats.Deleted != 1 {
		t.Fatalf("удалено %d блобов, ожидали один: %+v", stats.Deleted, stats)
	}
	if len(failed) != 0 {
		t.Fatalf("сбои удаления объектов: %v", failed)
	}
	for name, sha := range map[string]string{
		"артефакт": byArtifact, "индекс": byIndex, "документ": byDocument,
	} {
		if !f.alive(sha) {
			t.Fatalf("ссылка из %s блоб не удержала", name)
		}
	}
	if f.alive(orphan) {
		t.Fatal("блоб без ссылок не удалён")
	}

	// Повтор безвреден: сборщик приходит по тикеру, и второй проход по тому
	// же состоянию обязан не находить ничего и не ошибаться.
	again, _ := f.collect(gcOpts())
	if again.Deleted != 0 || !again.Empty() {
		t.Fatalf("повторный проход что-то нашёл: %+v", again)
	}
}

// Артефакт иммутабелен: новая версия через supersedes_id не удаляет
// предыдущую строку, и блоб старой версии остаётся сосчитанным. Сборщик про
// артефакты не знает вовсе — он считает ссылки.
func TestBlobGCKeepsSupersededVersions(t *testing.T) {
	f := newGCFixture(t)

	first := f.put("первая версия разбора изменений")
	firstID := f.artifact(first, "dev.change_summary", nil)
	second := f.put("вторая версия разбора изменений")
	f.artifact(second, "dev.change_summary", &firstID)
	f.exec(`UPDATE artifacts SET status = 'superseded' WHERE id = $1`, firstID)

	stats, _ := f.collect(gcOpts())
	if stats.Deleted != 0 {
		t.Fatalf("удалено %d блобов, ожидали ноль: %+v", stats.Deleted, stats)
	}
	if !f.alive(first) || !f.alive(second) {
		t.Fatal("версия артефакта освободила блоб предыдущей")
	}
}

// Блобы разделяются между арендаторами намеренно (docs/SAAS.md §8): ссылки
// считаются по всему инстансу, и осиротевший у одного проекта блоб остаётся
// живым, пока на него ссылается другой.
func TestBlobGCCountsReferencesAcrossProjects(t *testing.T) {
	f := newGCFixture(t)
	_, demoID := f.demoTask()

	var otherID uuid.UUID
	err := f.pool.QueryRow(f.ctx, `
		INSERT INTO projects (key, name, owner_team_id)
		SELECT 'OTHER', 'Проект другой команды', owner_team_id FROM projects WHERE id = $1
		RETURNING id`, demoID).Scan(&otherID)
	if err != nil {
		t.Fatalf("второй проект: %v", err)
	}

	// Одно и то же содержимое в двух проектах — один блоб и две ссылки.
	shared := f.put("общий для двух арендаторов файл")
	f.document(demoID, shared, "Документ первого проекта")
	f.document(otherID, shared, "Документ второго проекта")

	drop := func(projectID uuid.UUID) {
		t.Helper()
		err := f.store.InTx(f.ctx, func(tx pgx.Tx) error {
			if err := f.store.LockProjectForRemoval(f.ctx, tx, projectID); err != nil {
				return err
			}
			return f.store.DeleteProject(f.ctx, tx, projectID)
		})
		if err != nil {
			t.Fatalf("удаление проекта: %v", err)
		}
	}

	drop(otherID)
	if stats, _ := f.collect(gcOpts()); stats.Deleted != 0 {
		t.Fatalf("удаление одного из двух проектов освободило блоб: %+v", stats)
	}
	if !f.alive(shared) {
		t.Fatal("ссылка из оставшегося проекта блоб не удержала")
	}

	drop(demoID)
	if stats, _ := f.collect(gcOpts()); stats.Deleted == 0 {
		t.Fatalf("после удаления обоих проектов блоб не освобождён: %+v", stats)
	}
	if f.alive(shared) {
		t.Fatal("блоб без ссылок остался")
	}
}

// Минимальный возраст — запас на гонку с записью: только что положенный блоб
// может ждать ссылки из ещё не закоммиченной транзакции.
func TestBlobGCRespectsMinAge(t *testing.T) {
	f := newGCFixture(t)
	young := f.put("только что записанное содержимое")

	opts := gcOpts()
	opts.MinAge = time.Hour
	stats, _ := f.collect(opts)
	if stats.Deleted != 0 {
		t.Fatalf("молодой блоб удалён: %+v", stats)
	}
	if !f.alive(young) {
		t.Fatal("блоб моложе минимального возраста не пережил проход")
	}

	f.exec(`UPDATE blobs SET created_at = now() - interval '2 hours' WHERE sha256 = $1`, young)
	if stats, _ := f.collect(opts); stats.Deleted != 1 {
		t.Fatalf("состарившийся блоб не удалён: %+v", stats)
	}
}

// Предел за проход соблюдается: тик не должен затягиваться, а следующий всё
// равно наступит — хвост разбирается за несколько проходов.
func TestBlobGCRespectsLimit(t *testing.T) {
	f := newGCFixture(t)
	for i := 0; i < 5; i++ {
		f.put(fmt.Sprintf("мусор номер %d", i))
	}

	opts := postgres.BlobGCOptions{Limit: 2}
	total := 0
	for pass := 0; pass < 3; pass++ {
		stats, _ := f.collect(opts)
		if pass < 2 && stats.Deleted != 2 {
			t.Fatalf("проход %d: удалено %d, ожидали два", pass, stats.Deleted)
		}
		total += stats.Deleted
	}
	if total != 5 {
		t.Fatalf("за три прохода удалено %d из пяти", total)
	}
}

// Объект в хранилище исчезает вслед за строкой — в этом порядке, а не в
// обратном: строка, пережившая своё содержимое, была бы битой ссылкой навсегда.
func TestBlobGCDeletesObjectAfterRow(t *testing.T) {
	f := newGCFixture(t)
	sha := f.put("крупное содержимое во внешнем хранилище")

	var storage, uri string
	err := f.pool.QueryRow(f.ctx, `SELECT storage, external_uri FROM blobs WHERE sha256 = $1`, sha).
		Scan(&storage, &uri)
	if err != nil {
		t.Fatal(err)
	}
	if storage != "external" {
		t.Fatalf("блоб лёг как %q, а тест про внешнее хранилище", storage)
	}
	if !f.blobs.has(uri) {
		t.Fatal("объекта нет в хранилище ещё до прохода")
	}

	stats, failed := f.collect(gcOpts())
	if stats.Deleted != 1 || stats.ExternalBytes == 0 || stats.InlineBytes != 0 {
		t.Fatalf("статистика прохода: %+v", stats)
	}
	if len(failed) != 0 {
		t.Fatalf("сбои удаления объектов: %v", failed)
	}
	if f.alive(sha) {
		t.Fatal("строка блоба осталась")
	}
	if f.blobs.has(uri) {
		t.Fatal("объект остался в хранилище")
	}
}

// Сбой хранилища строку не воскрешает: ссылок на неё уже нет, и повторное
// появление строки без содержимого было бы хуже утечки места. Проход при этом
// не падает наружу — воркер должен дожить до следующего тика.
func TestBlobGCSurvivesStorageFailure(t *testing.T) {
	f := newGCFixture(t)
	sha := f.put("содержимое, которое хранилище не отдаст удалить")
	f.blobs.failing = true

	stats, failed := f.collect(gcOpts())
	if stats.Deleted != 1 {
		t.Fatalf("строка не удалена: %+v", stats)
	}
	if stats.Failed != 1 || len(failed) != 1 || failed[0] != sha {
		t.Fatalf("сбой удаления объекта не сосчитан: %+v, %v", stats, failed)
	}
	if f.alive(sha) {
		t.Fatal("сбой хранилища воскресил строку блоба")
	}
}

// Сухой прогон считает ровно то же, что посчитал бы настоящий, и не удаляет
// ничего: оператор смотрит в лог перед тем, как включить сборку.
func TestBlobGCDryRun(t *testing.T) {
	f := newGCFixture(t)
	for i := 0; i < 3; i++ {
		f.put(fmt.Sprintf("мусор для сухого прогона %d", i))
	}

	opts := gcOpts()
	opts.DryRun = true
	dry, _ := f.collect(opts)
	if dry.Deleted != 3 || dry.Bytes() == 0 {
		t.Fatalf("сухой прогон посчитал %+v", dry)
	}
	if f.blobs.deletes != 0 {
		t.Fatalf("сухой прогон сходил в хранилище %d раз", f.blobs.deletes)
	}

	wet, _ := f.collect(gcOpts())
	if wet.Deleted != dry.Deleted || wet.Bytes() != dry.Bytes() {
		t.Fatalf("настоящий проход разошёлся с сухим: %+v против %+v", wet, dry)
	}
}

// Строка в blobs никогда не остаётся без объекта. Запись того же содержимого,
// начатая во время прохода, держит advisory-замок по sha до своего коммита —
// и сборщик такой блоб не трогает вовсе.
//
// Ждать коммита ему незачем: раз содержимое пишут, ссылка на блоб вот-вот
// появится, и удалять его не нужно. Поэтому проход не стоит на замке, занимая
// соединение пула на всё время чужой транзакции, а считает блоб занятым и
// идёт дальше — к нему вернётся следующий проход.
func TestBlobGCYieldsToConcurrentWrite(t *testing.T) {
	f := newGCFixture(t)
	content := "содержимое, которое пишут прямо сейчас"
	sha := f.put(content)

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(f.ctx))

	// Пишущая транзакция: замок взят, ссылка вставлена, коммита ещё нет.
	if _, err := f.store.PutBlob(f.ctx, tx, []byte(content), "text/plain"); err != nil {
		t.Fatalf("параллельная запись: %v", err)
	}
	taskID, projectID := f.demoTask()
	_, err = tx.Exec(f.ctx, `
		INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, status)
		VALUES ($1, $2, 'dev.change_summary', 'гонка', $3, 'submitted')`,
		taskID, projectID, sha)
	if err != nil {
		t.Fatalf("ссылка на блоб: %v", err)
	}

	done := make(chan postgres.BlobGCStats, 1)
	go func() {
		stats, err := f.store.CollectBlobs(f.ctx, gcOpts(), nil)
		if err != nil {
			t.Errorf("проход сборщика: %v", err)
		}
		done <- stats
	}()

	// Проход обязан закончиться, пока пишущая транзакция открыта: он её не
	// ждёт. Если завис — вернулось бы поведение, при котором один писатель
	// держит соединение сборщика столько, сколько длится его транзакция.
	select {
	case stats := <-done:
		if stats.Deleted != 0 || stats.Busy != 1 {
			t.Fatalf("блоб под замком не пропущен: %+v", stats)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("сборщик встал на замке вместо того, чтобы пропустить блоб")
	}

	if !f.alive(sha) {
		t.Fatal("строка блоба, который пишут прямо сейчас, удалена")
	}
	if !f.blobs.has("fake://" + sha) {
		t.Fatal("объект удалён из-под строки")
	}

	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	// Ссылка появилась — следующему проходу этот блоб уже не кандидат.
	stats, _ := f.collect(gcOpts())
	if !stats.Empty() {
		t.Fatalf("блоб со ссылкой попал в проход: %+v", stats)
	}
	if !f.alive(sha) || !f.blobs.has("fake://"+sha) {
		t.Fatal("блоб со ссылкой не пережил следующий проход")
	}
}

// Блоб, вынесенный во внешнее хранилище, в процессе без этого хранилища не
// трогается вовсе: удалить объект нечем, и удаление строки гарантировало бы
// утечку места, а не рисковало бы ею. Такой блоб дождётся процесса, у
// которого хранилище настроено.
func TestBlobGCSkipsExternalWithoutStore(t *testing.T) {
	store, pool := testdb.Store(t)
	f := &gcFixture{t: t, ctx: context.Background(), store: store, pool: pool}

	sha := "e" + fmt.Sprintf("%063d", 1)
	f.exec(`INSERT INTO blobs (sha256, size_bytes, media_type, storage, external_uri)
	        VALUES ($1, 1024, 'text/plain', 'external', $2)`, sha, "fake://"+sha)

	stats, failed := f.collect(gcOpts())
	if stats.Deleted != 0 || stats.Skipped != 1 {
		t.Fatalf("статистика прохода: %+v", stats)
	}
	if len(failed) != 1 || failed[0] != sha {
		t.Fatalf("причина пропуска не названа: %v", failed)
	}
	if !f.alive(sha) {
		t.Fatal("строка удалена, а объект удалить было нечем")
	}
}

// hookBlobs — хранилище, которое даёт заглянуть внутрь удаления объекта:
// before зовётся в тот самый момент, когда строка блоба уже удалена и
// закоммичена, а объект ещё на месте. Это окно и было дырой.
type hookBlobs struct {
	*fakeBlobs
	before func(uri string)
}

func (h *hookBlobs) Delete(ctx context.Context, uri string) error {
	if h.before != nil {
		before := h.before
		h.before = nil
		before(uri)
	}
	return h.fakeBlobs.Delete(ctx, uri)
}

// Вторая половина той же гонки: писатель приходит не до удаляющей транзакции,
// а после её коммита — когда строки уже нет, а объект ещё есть. Транзакционный
// замок к этому моменту отпущен коммитом, и без сессионного PutBlob проходил
// свободно: Put не перезаливал объект, найденный на месте, строка вставлялась
// со старым адресом, а сборщик следом уносил объект из-под неё.
//
// Проверяется ровно то, что удаление объекта идёт под тем же замком: писатель,
// пришедший в это окно, обязан ждать, а дождавшись — получить строку вместе с
// объектом.
func TestBlobGCHoldsLockUntilObjectGone(t *testing.T) {
	f := newGCFixture(t)
	hooked := &hookBlobs{fakeBlobs: f.blobs}
	f.store.UseBlobStore(hooked, 1, 1<<20)

	content := "содержимое, которое перезапишут в окне"
	sha := f.put(content)

	taskID, projectID := f.demoTask()
	written := make(chan error, 1)
	hooked.before = func(string) {
		go func() {
			written <- f.store.InTx(f.ctx, func(tx pgx.Tx) error {
				if _, err := f.store.PutBlob(f.ctx, tx, []byte(content), "text/plain"); err != nil {
					return err
				}
				_, err := tx.Exec(f.ctx, `
					INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, status)
					VALUES ($1, $2, 'dev.change_summary', 'гонка', $3, 'submitted')`,
					taskID, projectID, sha)
				return err
			})
		}()
		// Писатель стоит на замке, пока объект не удалён. Если прошёл —
		// окно открыто, и строка вот-вот останется без содержимого.
		select {
		case err := <-written:
			t.Errorf("параллельная запись прошла в окне между строкой и объектом: %v", err)
			written <- err // вернуть, чтобы проверки после прохода не ждали впустую
		case <-time.After(500 * time.Millisecond):
		}
	}

	stats, failed := f.collect(gcOpts())
	if stats.Deleted != 1 || len(failed) != 0 {
		t.Fatalf("проход сборщика: %+v, сбои %v", stats, failed)
	}

	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("параллельная запись: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("параллельная запись не дождалась снятия замка")
	}

	// Писатель дождался пустого хранилища, залил объект заново и вставил
	// строку: пара цела с обеих сторон.
	if !f.alive(sha) {
		t.Fatal("строка блоба, записанная после прохода, потеряна")
	}
	if !f.blobs.has("fake://" + sha) {
		t.Fatal("строка блоба осталась без объекта")
	}
	got, _, err := f.store.BlobContent(f.ctx, f.pool, sha)
	if err != nil {
		t.Fatalf("чтение содержимого после гонки: %v", err)
	}
	if string(got) != content {
		t.Fatalf("содержимое после гонки: %q", got)
	}
}
