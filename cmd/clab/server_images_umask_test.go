//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// CLAB-257: архив save получает права обычного нового файла, 0666 &^
// umask, а не 0600 от os.CreateTemp. Umask общий на процесс, поэтому
// тест не параллельный и возвращает прежний.
func TestImagesSaveArchiveMode(t *testing.T) {
	for _, c := range []struct {
		umask int
		want  os.FileMode
	}{
		{0o022, 0o644}, // AC-1: обычный umask root под sudo
		{0o077, 0o600}, // AC-2: строгий umask не ослабляется
		{0o027, 0o640},
		{0o002, 0o664},
	} {
		t.Run(fmt.Sprintf("umask%03o", c.umask), func(t *testing.T) {
			old := syscall.Umask(c.umask)
			defer syscall.Umask(old)
			f := airgapSetup(t, nil)
			p := f.save()
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != c.want {
				t.Errorf("umask %03o: archive %04o, want %04o", c.umask, got, c.want)
			}
			// AC-3: временного .partial после успешного save нет.
			if left := f.leftovers("a.tar"); len(left) > 0 {
				t.Errorf("left: %v", left)
			}
		})
	}
}

// createPartial: имя .<output>.<случайное>.partial рядом с output, файл
// новый (O_EXCL) и с правами по umask.
func TestCreatePartial(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	dir := t.TempDir()
	out := filepath.Join(dir, "a.tar")
	a, err := createPartial(out)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := createPartial(out)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.Name() == b.Name() {
		t.Errorf("same name twice: %s", a.Name())
	}
	for _, f := range []*os.File{a, b} {
		name := filepath.Base(f.Name())
		if filepath.Dir(f.Name()) != dir || !strings.HasPrefix(name, ".a.tar.") || !strings.HasSuffix(name, ".partial") {
			t.Errorf("name: %s", f.Name())
		}
		fi, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("%s: %04o", name, fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("output exists: %v", err)
	}
	if _, err := createPartial(filepath.Join(dir, "missing", "a.tar")); err == nil {
		t.Error("no error for a missing directory")
	}
}
