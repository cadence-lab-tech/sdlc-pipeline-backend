// Package blobstore — вынос крупного содержимого из базы.
//
// Мелкое лежит inline в blobs.content, крупное уходит сюда, а в базе остаётся
// строка с external_uri. Интерфейс один для файловой системы и объектного
// хранилища: вызывающий код видит только sha и байты.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound — по адресу ничего нет: либо хранилище подменили, либо файл
// удалили руками. Для вызывающего кода это не сбой сети, а потеря данных.
var ErrNotFound = errors.New("blobstore: содержимое не найдено")

// Store — адресуемое по sha хранилище байтов.
type Store interface {
	// Put кладёт содержимое и возвращает URI, по которому его читать.
	// Повторная запись того же sha безвредна: адресация по содержимому.
	Put(ctx context.Context, sha string, content []byte, mediaType string) (string, error)
	Get(ctx context.Context, uri string) ([]byte, error)
}

// FS — каталог на диске. Годится для одного инстанса и для тома в
// контейнере; для нескольких реплик нужен общий том или объектное хранилище.
type FS struct {
	Dir string
}

const fsScheme = "file://"

func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("blobstore: каталог не задан")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blobstore: каталог %s: %w", dir, err)
	}
	return &FS{Dir: dir}, nil
}

// path раскладывает по первым двум символам sha: миллион файлов в одном
// каталоге тормозит любую файловую систему.
func (f *FS) path(sha string) string {
	return filepath.Join(f.Dir, sha[:2], sha)
}

func (f *FS) Put(ctx context.Context, sha string, content []byte, _ string) (string, error) {
	if len(sha) < 3 {
		return "", errors.New("blobstore: некорректный sha")
	}
	target := f.path(sha)
	if _, err := os.Stat(target); err == nil {
		return fsScheme + sha, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", err
	}
	// Запись во временный файл и переименование: читатель никогда не увидит
	// половину содержимого.
	tmp, err := os.CreateTemp(filepath.Dir(target), ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return fsScheme + sha, nil
}

func (f *FS) Get(_ context.Context, uri string) ([]byte, error) {
	sha, ok := strings.CutPrefix(uri, fsScheme)
	if !ok || len(sha) < 3 {
		return nil, fmt.Errorf("blobstore: адрес %q не из файлового хранилища", uri)
	}
	content, err := os.ReadFile(f.path(sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return content, err
}
