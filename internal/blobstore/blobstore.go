// Package blobstore — вынос крупного содержимого из базы.
//
// Мелкое лежит inline в blobs.content, крупное уходит сюда, а в базе остаётся
// строка с external_uri. Интерфейс один для файловой системы и объектного
// хранилища: вызывающий код видит только sha и поток байтов. Содержимое идёт
// через хранилище потоком (CLAB-295): запись — из файла спула, чтение —
// io.ReadCloser, и память процесса не растёт на размер содержимого.
package blobstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InlineMaxBytes — потолок содержимого, которое лежит прямо в строке
// blobs.content. Без внешнего хранилища крупнее не принимается вовсе, с ним —
// порог inline (SDLC_BLOB_INLINE_MAX_BYTES) не выше этого числа: строка таблицы
// не место для мегабайтов. Определён здесь, потому что на него ссылаются и
// проверка конфигурации, и хранилище артефактов.
const InlineMaxBytes = 4 << 20

// ErrNotFound — по адресу ничего нет: либо хранилище подменили, либо файл
// удалили руками. Для вызывающего кода это не сбой сети, а потеря данных.
var ErrNotFound = errors.New("blobstore: содержимое не найдено")

// Store — адресуемое по sha хранилище байтов.
type Store interface {
	// PutFile кладёт содержимое файла path — size байт, sha256 которых
	// вызывающий уже посчитал, — и возвращает URI, по которому его читать.
	// Повторная запись того же sha безвредна: адресация по содержимому.
	//
	// Файл реализация вправе забрать себе: FS переименовывает его в цель,
	// если он лежит на той же файловой системе (TempDir). Вызывающий удаляет
	// path после вызова, и отсутствие файла тогда не ошибка.
	PutFile(ctx context.Context, sha, path string, size int64, mediaType string) (string, error)
	// Open открывает содержимое на чтение. Пропавший объект — ErrNotFound
	// здесь же, до первого Read: отдача узнаёт о потере данных раньше, чем
	// отправит заголовки ответа.
	Open(ctx context.Context, uri string) (io.ReadCloser, error)
	// Delete убирает содержимое по адресу. Отсутствие объекта — не ошибка:
	// сборщик мусора приходит сюда после удаления строки в blobs и может
	// прийти второй раз после падения между этими шагами.
	Delete(ctx context.Context, uri string) error
	// TempDir — где спулить содержимое перед PutFile; пусто — os.TempDir().
	TempDir() string
}

// Open выбирает хранилище по настройкам: бакет важнее каталога, потому что
// каталог задан в образе по умолчанию, а бакет — осознанное решение. Без того
// и другого возвращается nil: содержимое лежит inline.
func Open(s3 S3Config, dir string) (Store, error) {
	switch {
	case s3.Bucket != "":
		return NewS3(s3)
	case dir != "":
		return NewFS(dir)
	default:
		return nil, nil
	}
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

// TempDir — сам каталог хранилища: спул на том же томе уходит в цель
// переименованием, без второй записи всего содержимого. Временный каталог
// пода для этого не годится ещё и потому, что в Helm-чарте он может быть
// только для чтения, а том блобов записываем всегда.
func (f *FS) TempDir() string { return f.Dir }

// PutFile переносит файл в цель переименованием, а если не вышло (другая
// файловая система) — копирует через временный файл рядом с целью. Читатель
// в обоих случаях не увидит половину содержимого.
func (f *FS) PutFile(_ context.Context, sha, path string, _ int64, _ string) (string, error) {
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
	if err := os.Rename(path, target); err == nil {
		return fsScheme + sha, nil
	}
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	if err := f.write(target, src); err != nil {
		return "", err
	}
	return fsScheme + sha, nil
}

// Put — запись содержимого, уже лежащего в памяти.
func (f *FS) Put(_ context.Context, sha string, content []byte, _ string) (string, error) {
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
	if err := f.write(target, bytes.NewReader(content)); err != nil {
		return "", err
	}
	return fsScheme + sha, nil
}

// write пишет во временный файл рядом с целью и переименовывает: читатель
// никогда не увидит половину содержимого.
func (f *FS) write(target string, src io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// String — для журнала: видно, куда именно уходит содержимое.
func (f *FS) String() string { return "file://" + f.Dir }

// Open открывает файл содержимого; отдача читает его потоком.
func (f *FS) Open(_ context.Context, uri string) (io.ReadCloser, error) {
	sha, ok := strings.CutPrefix(uri, fsScheme)
	if !ok || len(sha) < 3 {
		return nil, fmt.Errorf("blobstore: адрес %q не из файлового хранилища", uri)
	}
	file, err := os.Open(f.path(sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return file, nil
}

// Get читает содержимое целиком.
func (f *FS) Get(ctx context.Context, uri string) ([]byte, error) {
	return readAll(f.Open(ctx, uri))
}

func readAll(r io.ReadCloser, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Delete уносит файл. Чужой адрес отбивается так же, как в Get: он мог
// остаться от прежней установки, и удалить по нему что-нибудь своё было бы
// хуже, чем отказать. Пустой каталог после удаления не подчищается — в него
// тут же может лечь следующий блоб с тем же префиксом.
func (f *FS) Delete(_ context.Context, uri string) error {
	sha, ok := strings.CutPrefix(uri, fsScheme)
	if !ok || len(sha) < 3 {
		return fmt.Errorf("blobstore: адрес %q не из файлового хранилища", uri)
	}
	if err := os.Remove(f.path(sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
