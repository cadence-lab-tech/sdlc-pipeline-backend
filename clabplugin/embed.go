// Package clabplugin — мод Claude clab-plugin (решение 0067): каталог плагина
// с манифестом, хуками и контрактом состояния, встроенный в бинарь clab.
//
// Ставит его clab setup --plugin: мод раскладывается на диск локальным
// каталогом-маркетплейсом и устанавливается в Claude. С инстанса мод не
// берётся (п. 6 решения 0050 в силе): исполняемый код на машине человека
// приходит только подписанным выпуском clab, и версия мода — версия clab.
//
// Тесты мода (tests/, *.test.ts) в бинарь не вшиваются: их гоняет
// claude plugin test по исходному каталогу.
package clabplugin

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// Name — имя плагина в манифесте и в Claude; команда в чате — /clab-plugin.
const Name = "clab-plugin"

// Манифест назван файлом: каталог .claude-plugin (с точки) шаблоном не
// вшивается, а целиком он и не нужен — движок кладёт в .claude-plugin/types
// свои типы, когда грузит мод из этого каталога при разработке.
//
//go:embed .claude-plugin/plugin.json hooks types
var source embed.FS

// manifest — путь манифеста внутри каталога плагина.
const manifest = ".claude-plugin/plugin.json"

// sourceVersion — версия в исходном манифесте: валидная семвер-версия, чтобы
// claude plugin validate проходил по каталогу в репозитории; при раскладке
// её заменяет версия сборки clab.
const sourceVersion = `"version": "0.0.0-dev"`

// File — файл плагина: путь внутри каталога плагина (через /) и содержимое.
type File struct {
	Path    string
	Content []byte
}

var releaseVersion = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$`)

// Version — версия плагина для версии сборки clab. Выпуск vX.Y.Z — X.Y.Z.
// Сборка без версии (dev, ветка) получает 0.0.0-dev.<8 знаков sha256 мода>:
// семвер ниже любого выпуска и другая при каждом изменении мода, чтобы Claude,
// который кэширует плагин по версии, его обновил.
func Version(clabVersion string) string {
	if m := releaseVersion.FindStringSubmatch(strings.TrimSpace(clabVersion)); m != nil {
		return m[1]
	}
	return "0.0.0-dev." + Digest()[:8]
}

// Files — файлы плагина в порядке путей, с версией в манифесте.
func Files(clabVersion string) ([]File, error) {
	files, err := read()
	if err != nil {
		return nil, err
	}
	v := Version(clabVersion)
	for i, f := range files {
		if f.Path != manifest {
			continue
		}
		if bytes.Count(f.Content, []byte(sourceVersion)) != 1 {
			return nil, fmt.Errorf("clabplugin: в %s нет строки %s", manifest, sourceVersion)
		}
		files[i].Content = bytes.Replace(f.Content, []byte(sourceVersion), []byte(`"version": "`+v+`"`), 1)
	}
	return files, nil
}

// Digest — sha256 вшитого мода (пути и содержимое, без подстановки версии).
func Digest() string {
	files, err := read()
	if err != nil {
		return strings.Repeat("0", 64)
	}
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00", f.Path, len(f.Content))
		h.Write(f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func read() ([]File, error) {
	var files []File
	err := fs.WalkDir(source, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := source.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, File{Path: p, Content: raw})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
