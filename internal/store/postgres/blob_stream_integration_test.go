package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Запись и чтение содержимого потоком (CLAB-295, AC-2.2–AC-2.6).

// Потоковый интерфейс хранилища у подставного fakeBlobs: содержимое
// по-прежнему в памяти, проверки сборщика смотрят туда же.
func (f *fakeBlobs) PutFile(ctx context.Context, sha, path string, _ int64, mediaType string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return f.Put(ctx, sha, content, mediaType)
}

func (f *fakeBlobs) Open(ctx context.Context, uri string) (io.ReadCloser, error) {
	content, err := f.Get(ctx, uri)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (f *fakeBlobs) TempDir() string { return "" }

// onlyReader прячет у читателя всё, кроме Read: длину содержимого запись
// узнаёт только по потоку, как у тела запроса без Content-Length.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func putFrom(t *testing.T, store *postgres.Store, r io.Reader) (postgres.BlobInfo, error) {
	t.Helper()
	ctx := context.Background()
	var info postgres.BlobInfo
	err := store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		info, err = store.PutBlobFrom(ctx, tx, r, "application/octet-stream")
		return err
	})
	return info, err
}

// files — обычные файлы под каталогом, относительными путями: и объекты
// хранилища, и спул.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func spoolLeft(t *testing.T, dir string) []string {
	t.Helper()
	var left []string
	for _, f := range files(t, dir) {
		if strings.Contains(filepath.Base(f), ".spool-") || strings.HasPrefix(filepath.Base(f), ".tmp-") {
			left = append(left, f)
		}
	}
	return left
}

func fsStore(t *testing.T, inline, maxBytes int) (*postgres.Store, string) {
	t.Helper()
	store, _ := testdb.Store(t)
	dir := t.TempDir()
	fs, err := blobstore.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.UseBlobStore(fs, inline, maxBytes)
	return store, dir
}

// AC-2.2: sha и размер совпадают с посчитанными по самим байтам на трёх
// размерах — inline, чуть больше порога и значительно больше; читается то
// же, что записано, и спула не остаётся.
func TestPutBlobFromSizes(t *testing.T) {
	const inline = 256 << 10
	store, dir := fsStore(t, inline, 64<<20)
	pool := store.DB()
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		size    int
		storage string
	}{
		{"меньше порога inline", inline - 1, "inline"},
		{"ровно порог inline", inline, "inline"},
		{"чуть больше порога", inline + 1, "external"},
		{"8 МиБ", 8 << 20, "external"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := randomBytes(tc.size, int64(tc.size))
			sum := sha256.Sum256(content)
			want := hex.EncodeToString(sum[:])

			info, err := putFrom(t, store, onlyReader{bytes.NewReader(content)})
			if err != nil {
				t.Fatal(err)
			}
			if info.SHA != want || info.Size != int64(tc.size) {
				t.Fatalf("записано %s/%d, ждали %s/%d", info.SHA, info.Size, want, tc.size)
			}
			var storage string
			var size int64
			if err := pool.QueryRow(ctx, `SELECT storage, size_bytes FROM blobs WHERE sha256 = $1`, want).
				Scan(&storage, &size); err != nil {
				t.Fatal(err)
			}
			if storage != tc.storage || size != int64(tc.size) {
				t.Fatalf("строка blobs: %s/%d, ждали %s/%d", storage, size, tc.storage, tc.size)
			}
			if tc.storage == "external" {
				if _, err := os.Stat(filepath.Join(dir, want[:2], want)); err != nil {
					t.Fatalf("объекта в хранилище нет: %v", err)
				}
			}

			blob, err := store.OpenBlob(ctx, pool, want)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(blob)
			blob.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) || blob.Size != int64(tc.size) {
				t.Fatalf("прочитано %d байт (size %d), ждали %d", len(got), blob.Size, tc.size)
			}
			text, _, err := store.BlobContent(ctx, pool, want)
			if err != nil || !bytes.Equal(text, content) {
				t.Fatalf("BlobContent расходится с записанным: %v", err)
			}
			if left := spoolLeft(t, dir); len(left) > 0 {
				t.Fatalf("спул остался: %v", left)
			}
		})
	}
}

// AC-2.3: повторная запись тех же байтов потоком — одна строка и один
// объект, спул второй записи убран.
func TestPutBlobFromDedup(t *testing.T) {
	store, dir := fsStore(t, 1024, 1<<20)
	content := randomBytes(64<<10, 7)
	first, err := putFrom(t, store, onlyReader{bytes.NewReader(content)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := putFrom(t, store, onlyReader{bytes.NewReader(content)})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("одинаковые байты дали разное: %+v и %+v", first, second)
	}
	var n int
	if err := store.DB().QueryRow(context.Background(), `SELECT count(*) FROM blobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("строк blobs: %d", n)
	}
	if got := files(t, dir); len(got) != 1 {
		t.Fatalf("файлы хранилища: %v", got)
	}
}

func wantPayloadTooLarge(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("содержимое больше предела принято")
	}
	if e := apperr.As(err); e.Code != apperr.CodePayloadTooLarge {
		t.Fatalf("ждали payload_too_large, получили %v", err)
	}
}

func blobRows(t *testing.T, store *postgres.Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(context.Background(), `SELECT count(*) FROM blobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AC-2.4: предел считается по потоку, без заявленной длины. Ровно предел
// принимается, байт сверх него — payload_too_large, и после отказа нет ни
// строки, ни объекта, ни спула.
func TestPutBlobFromOverLimit(t *testing.T) {
	const limit = 1 << 20
	store, dir := fsStore(t, 1024, limit)

	over := randomBytes(limit+1, 1)
	_, err := putFrom(t, store, onlyReader{bytes.NewReader(over)})
	wantPayloadTooLarge(t, err)
	if !strings.Contains(err.Error(), "1048576") {
		t.Fatalf("в отказе нет предела: %v", err)
	}
	if n := blobRows(t, store); n != 0 {
		t.Fatalf("после отказа строк blobs: %d", n)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("после отказа в каталоге хранилища: %v", got)
	}

	// Поток длиннее предела в разы читается не дальше предела: отказ
	// приходит, не дожидаясь конца.
	endless := &countingReader{r: rand.New(rand.NewSource(2))}
	_, err = putFrom(t, store, endless)
	wantPayloadTooLarge(t, err)
	if endless.n > limit+64<<10 {
		t.Fatalf("прочитано %d байт при пределе %d", endless.n, limit)
	}

	if _, err := putFrom(t, store, onlyReader{bytes.NewReader(over[:limit])}); err != nil {
		t.Fatalf("ровно предел отвергнут: %v", err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// Без внешнего хранилища крупнее потолка inline поток не принимается —
// как PutBlob, только отказ — payload_too_large.
func TestPutBlobFromWithoutStore(t *testing.T) {
	store, _ := testdb.Store(t)
	store.UseBlobStore(nil, 256<<10, 64<<20)
	_, err := putFrom(t, store, onlyReader{bytes.NewReader(randomBytes(blobstore.InlineMaxBytes+1, 3))})
	wantPayloadTooLarge(t, err)
	if n := blobRows(t, store); n != 0 {
		t.Fatalf("после отказа строк blobs: %d", n)
	}
	if _, err := putFrom(t, store, onlyReader{bytes.NewReader(randomBytes(blobstore.InlineMaxBytes, 3))}); err != nil {
		t.Fatalf("потолок inline без хранилища отвергнут: %v", err)
	}
}

// failingReader отдаёт n байт и ломается — клиент ушёл посреди тела.
type failingReader struct {
	n   int
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, f.err
	}
	k := min(len(p), f.n)
	for i := range p[:k] {
		p[i] = byte(i)
	}
	f.n -= k
	return k, nil
}

// AC-2.4: оборванный поток — ошибка с причиной в цепочке, и ничего не
// записано. Причина нужна транспорту: свой предел тела он узнаёт по ней.
func TestPutBlobFromReaderError(t *testing.T) {
	store, dir := fsStore(t, 1024, 64<<20)
	broken := errors.New("клиент ушёл")
	for _, tc := range []struct {
		name string
		at   int
		err  error
	}{
		{"до порога inline", 100, broken},
		{"посреди спула", 2 << 20, broken},
		{"предел тела запроса", 3 << 20, &http.MaxBytesError{Limit: 3 << 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := putFrom(t, store, &failingReader{n: tc.at, err: tc.err})
			if err == nil {
				t.Fatal("оборванный поток принят")
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("причины нет в цепочке: %v", err)
			}
			if n := blobRows(t, store); n != 0 {
				t.Fatalf("строк blobs: %d", n)
			}
			if got := files(t, dir); len(got) != 0 {
				t.Fatalf("в каталоге хранилища: %v", got)
			}
		})
	}
	var mbe *http.MaxBytesError
	_, err := putFrom(t, store, &failingReader{n: 10, err: &http.MaxBytesError{Limit: 5}})
	if !errors.As(err, &mbe) || mbe.Limit != 5 {
		t.Fatalf("ошибка предела тела потеряна: %v", err)
	}
}

// AC-2.5: запись 32 МиБ потоком не держит содержимое в памяти. Источник —
// генератор без своего буфера, поэтому всё, что выделено за запись, —
// расход самой записи; он обязан быть на порядок меньше содержимого.
func TestPutBlobFromMemory(t *testing.T) {
	const size = 32 << 20
	store, _ := fsStore(t, 256<<10, 64<<20)

	src := io.LimitReader(rand.New(rand.NewSource(4)), size)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	info, err := putFrom(t, store, src)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != size {
		t.Fatalf("записано %d байт", info.Size)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("выделено за запись %d байт на %d байт содержимого", allocated, size)
	if allocated > size/10 {
		t.Fatalf("выделено %d байт на %d байт содержимого: содержимое держится в памяти", allocated, size)
	}
}

// AC-2.6 на уровне хранилища: пропавший объект — ошибка при открытии, а
// не посреди чтения.
func TestOpenBlobMissingObject(t *testing.T) {
	store, dir := fsStore(t, 1024, 1<<20)
	info, err := putFrom(t, store, onlyReader{bytes.NewReader(randomBytes(4096, 5))})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, info.SHA[:2], info.SHA)); err != nil {
		t.Fatal(err)
	}
	_, err = store.OpenBlob(context.Background(), store.DB(), info.SHA)
	if err == nil || !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("пропавший объект не замечен при открытии: %v", err)
	}
	if apperr.As(err).Code != apperr.CodeInternal {
		t.Fatalf("пропавший объект — не внутренняя ошибка: %v", err)
	}
}

// lockProbe — хранилище, которое в момент записи объекта проверяет с
// другого соединения, занят ли замок по sha.
type lockProbe struct {
	*fakeBlobs
	t      *testing.T
	store  *postgres.Store
	probed bool
}

func (p *lockProbe) PutFile(ctx context.Context, sha, path string, size int64, mediaType string) (string, error) {
	var free bool
	if err := p.store.DB().QueryRow(ctx,
		`SELECT pg_try_advisory_lock(('x' || substr($1::text, 1, 16))::bit(64)::bigint)`, sha).Scan(&free); err != nil {
		p.t.Errorf("проба замка: %v", err)
	}
	if free {
		p.store.DB().Exec(ctx, `SELECT pg_advisory_unlock(('x' || substr($1::text, 1, 16))::bit(64)::bigint)`, sha)
		p.t.Error("объект пишется во внешнее хранилище вне замка по sha")
	}
	p.probed = true
	return p.fakeBlobs.PutFile(ctx, sha, path, size, mediaType)
}

// AC-2.3: замок по sha взят до обращения к внешнему хранилищу — и в
// потоковом пути, и в PutBlob, который теперь идёт через него же.
func TestPutBlobFromLocksBeforeStore(t *testing.T) {
	store, _ := testdb.Store(t)
	probe := &lockProbe{fakeBlobs: newFakeBlobs(), t: t, store: store}
	store.UseBlobStore(probe, 1, 1<<20)

	if _, err := putFrom(t, store, onlyReader{strings.NewReader("потоком мимо inline")}); err != nil {
		t.Fatal(err)
	}
	if _, err := putBlob(t, store, []byte("байтами мимо inline")); err != nil {
		t.Fatal(err)
	}
	if !probe.probed {
		t.Fatal("внешнее хранилище не звалось")
	}
}

// AC-2.3: гонка со сборщиком (TestBlobGCHoldsLockUntilObjectGone) на
// потоковом пути — писатель, пришедший в окно между удалением строки и
// удалением объекта, ждёт замка и получает строку вместе с объектом.
func TestBlobGCHoldsLockAgainstStreamWriter(t *testing.T) {
	f := newGCFixture(t)
	hooked := &hookBlobs{fakeBlobs: f.blobs}
	f.store.UseBlobStore(hooked, 1, 1<<20)

	content := "содержимое, которое перезапишут потоком в окне"
	sha := f.put(content)
	taskID, projectID := f.demoTask()

	written := make(chan error, 1)
	hooked.before = func(string) {
		go func() {
			written <- f.store.InTx(f.ctx, func(tx pgx.Tx) error {
				if _, err := f.store.PutBlobFrom(f.ctx, tx, onlyReader{strings.NewReader(content)}, "text/plain"); err != nil {
					return err
				}
				_, err := tx.Exec(f.ctx, `
					INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, status)
					VALUES ($1, $2, 'dev.change_summary', 'гонка', $3, 'submitted')`,
					taskID, projectID, sha)
				return err
			})
		}()
		select {
		case err := <-written:
			t.Errorf("потоковая запись прошла в окне между строкой и объектом: %v", err)
			written <- err
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
			t.Fatalf("потоковая запись: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("потоковая запись не дождалась снятия замка")
	}
	if !f.alive(sha) || !f.blobs.has("fake://"+sha) {
		t.Fatal("после гонки строка или объект потеряны")
	}
}
