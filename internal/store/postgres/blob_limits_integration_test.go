package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Пределы содержимого в хранилище (CLAB-81). config.Load противоречивую пару
// не пропускает; здесь — охрана для вызовов в обход конфигурации и режим без
// внешнего хранилища, где общий предел раньше не действовал вовсе.

func putBlob(t *testing.T, store *postgres.Store, content []byte) (string, error) {
	t.Helper()
	ctx := context.Background()
	var sha string
	err := store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		sha, err = store.PutBlob(ctx, tx, content, "text/plain")
		return err
	})
	return sha, err
}

// blobStorage — как лежит содержимое: inline, external или "" — строки нет.
func blobStorage(t *testing.T, pool *pgxpool.Pool, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	var storage string
	err := pool.QueryRow(context.Background(),
		`SELECT storage FROM blobs WHERE sha256 = $1`, hex.EncodeToString(sum[:])).Scan(&storage)
	if err == pgx.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return storage
}

func wantTooLarge(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("содержимое больше предела принято")
	}
	if apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("ждали ошибку ввода, получили %v", err)
	}
}

// Содержимое больше общего предела, которое поместилось бы inline, —
// ошибка ввода, а не строка в базе мимо предела.
func TestPutBlobMaxAppliesToInline(t *testing.T) {
	store, pool := testdb.Store(t)
	blobs := newFakeBlobs()
	store.UseBlobStore(blobs, 1<<20, 1000)

	big := bytes.Repeat([]byte("a"), 2000)
	_, err := putBlob(t, store, big)
	wantTooLarge(t, err)
	if s := blobStorage(t, pool, big); s != "" {
		t.Fatalf("содержимое больше предела легло в blobs (%s)", s)
	}
	if len(blobs.objects) != 0 {
		t.Fatal("содержимое больше предела ушло во внешнее хранилище")
	}

	small := bytes.Repeat([]byte("b"), 500)
	if _, err := putBlob(t, store, small); err != nil {
		t.Fatal(err)
	}
	if s := blobStorage(t, pool, small); s != "inline" {
		t.Fatalf("содержимое под порогом лежит как %q", s)
	}
}

// Порог inline выше 4 МиБ срезается до потолка: крупнее уходит во внешнее
// хранилище, в строку таблицы мегабайты не попадают.
func TestPutBlobInlineCappedWithStore(t *testing.T) {
	store, pool := testdb.Store(t)
	store.UseBlobStore(newFakeBlobs(), 16<<20, 64<<20)

	over := bytes.Repeat([]byte("c"), blobstore.InlineMaxBytes+1)
	if _, err := putBlob(t, store, over); err != nil {
		t.Fatal(err)
	}
	if s := blobStorage(t, pool, over); s != "external" {
		t.Fatalf("содержимое больше 4 МиБ лежит как %q, ждали external", s)
	}

	atCeiling := bytes.Repeat([]byte("d"), blobstore.InlineMaxBytes)
	if _, err := putBlob(t, store, atCeiling); err != nil {
		t.Fatal(err)
	}
	if s := blobStorage(t, pool, atCeiling); s != "inline" {
		t.Fatalf("содержимое ровно 4 МиБ лежит как %q, ждали inline", s)
	}
}

// Без внешнего хранилища общий предел действует: принимается не больше
// min(4 МиБ, max).
func TestPutBlobMaxWithoutStore(t *testing.T) {
	store, pool := testdb.Store(t)
	store.UseBlobStore(nil, 256<<10, 1000)

	big := bytes.Repeat([]byte("e"), 2000)
	_, err := putBlob(t, store, big)
	wantTooLarge(t, err)
	if s := blobStorage(t, pool, big); s != "" {
		t.Fatalf("содержимое больше предела легло в blobs (%s)", s)
	}

	small := bytes.Repeat([]byte("f"), 500)
	if _, err := putBlob(t, store, small); err != nil {
		t.Fatal(err)
	}
	if s := blobStorage(t, pool, small); s != "inline" {
		t.Fatalf("содержимое под пределом лежит как %q", s)
	}

	// Предел выше потолка: без хранилища крупнее 4 МиБ всё равно не лечь.
	store.UseBlobStore(nil, 256<<10, 64<<20)
	over := bytes.Repeat([]byte("g"), blobstore.InlineMaxBytes+1)
	_, err = putBlob(t, store, over)
	wantTooLarge(t, err)
	if s := blobStorage(t, pool, over); s != "" {
		t.Fatalf("содержимое больше 4 МиБ без хранилища легло в blobs (%s)", s)
	}
}

// BlobLimit говорит ровно то, что делает PutBlob (CLAB-82): содержимое в
// BlobLimit байт принимается, на байт больше — нет. На это опирается охрана
// индексатора, отсеивающего файлы до скачивания.
func TestBlobLimitMatchesPutBlob(t *testing.T) {
	store, _ := testdb.Store(t)
	for _, tc := range []struct {
		name      string
		external  bool
		inline    int
		max       int
		wantLimit int
	}{
		{"без хранилища, предел ниже потолка", false, 256 << 10, 1000, 1000},
		{"без хранилища, предел выше потолка", false, 256 << 10, 64 << 20, blobstore.InlineMaxBytes},
		{"с хранилищем", true, 1000, 5 << 20, 5 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bs postgres.BlobStore
			if tc.external {
				bs = newFakeBlobs()
			}
			store.UseBlobStore(bs, tc.inline, tc.max)
			limit := store.BlobLimit()
			if limit != tc.wantLimit {
				t.Fatalf("BlobLimit %d, ждали %d", limit, tc.wantLimit)
			}
			at := bytes.Repeat([]byte("h"), limit)
			at[0] = byte(len(tc.name)) // своё содержимое на каждый случай
			if _, err := putBlob(t, store, at); err != nil {
				t.Fatalf("содержимое ровно в BlobLimit не принято: %v", err)
			}
			_, err := putBlob(t, store, append(at, 'x'))
			wantTooLarge(t, err)
		})
	}
}
