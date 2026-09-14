package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestFSRoundTrip(t *testing.T) {
	fs, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("большой артефакт")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	uri, err := fs.Put(context.Background(), sha, content, "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	// Повторная запись того же sha безвредна: адресация по содержимому.
	again, err := fs.Put(context.Background(), sha, content, "text/markdown")
	if err != nil || again != uri {
		t.Fatalf("повторная запись: %v, %q против %q", err, again, uri)
	}

	got, err := fs.Get(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("прочитано не то: %q", got)
	}

	if _, err := fs.Get(context.Background(), "file://"+hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("отсутствующее содержимое: ожидали ErrNotFound, получили %v", err)
	}
	if _, err := fs.Get(context.Background(), "s3://bucket/key"); err == nil {
		t.Fatal("чужой адрес должен отбиваться")
	}
}

// Сборка мусора приходит сюда после удаления строки в blobs и может прийти
// второй раз после падения между шагами: отсутствие объекта — не ошибка.
func TestFSDelete(t *testing.T) {
	fs, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	content := []byte("осиротевший артефакт")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	uri, err := fs.Put(ctx, sha, content, "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, uri); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Get(ctx, uri); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после удаления: ожидали ErrNotFound, получили %v", err)
	}
	// Повтор безвреден: иначе проход сборщика после падения валился бы.
	if err := fs.Delete(ctx, uri); err != nil {
		t.Fatalf("повторное удаление: %v", err)
	}
	if err := fs.Delete(ctx, "file://"+hex.EncodeToString(make([]byte, 32))); err != nil {
		t.Fatalf("удаление того, чего не было: %v", err)
	}
	// Чужой адрес мог остаться от прежней установки: удалить по нему
	// что-нибудь своё хуже, чем отказать.
	if err := fs.Delete(ctx, "s3://bucket/key"); err == nil {
		t.Fatal("чужой адрес должен отбиваться")
	}
}
