package blobstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
)

// Запись из файла и чтение потоком (CLAB-295, AC-2.1).

// spoolFile кладёт случайное содержимое в файл каталога dir и отдаёт путь,
// байты и их sha256 — так, как их отдаёт спул хранилища артефактов.
func spoolFile(t *testing.T, dir string, size int) (string, []byte, string) {
	t.Helper()
	content := make([]byte, size)
	rand.Read(content)
	f, err := os.CreateTemp(dir, ".spool-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return f.Name(), content, hex.EncodeToString(sum[:])
}

func readOpened(t *testing.T, s blobstore.Store, uri string) []byte {
	t.Helper()
	r, err := s.Open(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestS3PutFileAndOpen(t *testing.T) {
	s3, fake := newObjectS3(t)
	ctx := context.Background()
	path, content, sha := spoolFile(t, t.TempDir(), 3<<20+17)

	uri, err := s3.PutFile(ctx, sha, path, int64(len(content)), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := fake.Get(sha[:2] + "/" + sha)
	if !ok || !bytes.Equal(stored, content) {
		t.Fatalf("в бакете не то содержимое (%d байт, есть: %v)", len(stored), ok)
	}
	if got := readOpened(t, s3, uri); !bytes.Equal(got, content) {
		t.Fatalf("прочитано %d байт, ждали %d", len(got), len(content))
	}

	// Повторная запись того же sha объект не перезаливает.
	puts := 0
	for _, r := range fake.Requests {
		if len(r) > 4 && r[:4] == "PUT " {
			puts++
		}
	}
	if _, err := s3.PutFile(ctx, sha, path, int64(len(content)), ""); err != nil {
		t.Fatal(err)
	}
	again := 0
	for _, r := range fake.Requests {
		if len(r) > 4 && r[:4] == "PUT " {
			again++
		}
	}
	if puts != 1 || again != 1 {
		t.Fatalf("PUT до и после повторной записи: %d и %d", puts, again)
	}

	missing := "s3://backups/" + "ff/ff" + sha[4:]
	if _, err := s3.Open(ctx, missing); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("пропавший объект: %v", err)
	}
	if s3.TempDir() != "" {
		t.Fatalf("спул S3 не во временном каталоге процесса: %q", s3.TempDir())
	}
}

func TestFSPutFileAndOpen(t *testing.T) {
	dir := t.TempDir()
	fs, err := blobstore.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if fs.TempDir() != dir {
		t.Fatalf("спул FS не в каталоге хранилища: %q", fs.TempDir())
	}

	// Спул в каталоге хранилища уходит в цель переименованием.
	path, content, sha := spoolFile(t, fs.TempDir(), 1<<20)
	uri, err := fs.PutFile(ctx, sha, path, int64(len(content)), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("спул не переименован в цель: %v", err)
	}
	if got := readOpened(t, fs, uri); !bytes.Equal(got, content) {
		t.Fatal("прочитано не то, что записано")
	}

	// Файл из другого каталога — тоже, а существующая цель не трогается.
	other, content2, sha2 := spoolFile(t, t.TempDir(), 4096)
	uri2, err := fs.PutFile(ctx, sha2, other, int64(len(content2)), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := readOpened(t, fs, uri2); !bytes.Equal(got, content2) {
		t.Fatal("прочитано не то, что записано")
	}
	again, _, _ := spoolFile(t, t.TempDir(), 10)
	if _, err := fs.PutFile(ctx, sha2, again, 10, ""); err != nil {
		t.Fatal(err)
	}
	if got := readOpened(t, fs, uri2); !bytes.Equal(got, content2) {
		t.Fatal("существующий объект перезаписан")
	}

	if err := os.Remove(filepath.Join(dir, sha[:2], sha)); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Open(ctx, uri); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("пропавший файл: %v", err)
	}
}
