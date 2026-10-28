package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// cmdArtifact печатает или сохраняет содержимое артефакта (CLAB-222,
// CLAB-298). Исполнитель этапа работает без MCP, только `clab` с токеном
// задачи, и входные артефакты и вложения читает этой командой: id — из
// prior_artifacts и вложений ответа `clab claim` и `clab show`. Видимость
// решает сервер: токену задачи открыты артефакты своей задачи, её предков,
// детей и связанных задач (решение 0049).
//
// Формы:
//   - без -o текст печатается как есть, чтобы его можно было перенаправить в
//     файл; бинарное содержимое в терминал не печатается — отказ с подсказкой,
//     тело при этом не читается;
//   - -o <файл> пишет байты потоком; существующий файл не перезаписывается;
//   - -o <каталог>/ пишет файл с именем из Content-Disposition, очищенным
//     повторно: сервер имя санитизирует, а CLI пишет на диск и заголовку не
//     доверяет;
//   - -o - пишет байты в stdout, в том числе бинарные;
//   - --json без -o — {id, media_type, content} и только для текста; с -o
//     --json печатает сведения о записанном файле.
//
// Запись идёт во временный файл рядом с целью и переименованием по
// завершении: оборванная загрузка не оставляет на диске частичного файла.
func cmdArtifact(ctx context.Context, c *Client, args []string) error {
	id, rest := popTask(args)
	var output string
	if _, err := flags("artifact", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&output, "o", "", "куда писать: файл, каталог/ (имя берётся с сервера) или - (stdout)")
	}); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("clab artifact <id> — id из prior_artifacts и вложений ответа clab claim или clab show")
	}
	dest, err := planDownload(output)
	if err != nil {
		return err
	}

	resp, err := c.openContent(ctx, "/artifacts/"+url.PathEscape(id)+"/content")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	mediaType := responseMediaType(resp.Header)

	switch dest.kind {
	case destNone:
		if isBinaryMedia(mediaType) {
			return fmt.Errorf("артефакт %s — бинарный (%s): в терминал он не печатается. "+
				"Сохраните его: clab artifact %s -o <файл> или clab artifact %s -o <каталог>/ (в stdout — -o -)",
				id, mediaType, id, id)
		}
		if jsonOut {
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			return print(map[string]any{"id": id, "media_type": mediaType, "content": string(raw)})
		}
		return printText(resp.Body)
	case destStdout:
		_, err := io.Copy(os.Stdout, resp.Body)
		return err
	}

	saved, err := dest.save(id, mediaType, resp)
	if err != nil {
		return err
	}
	return emit(map[string]any{"id": id, "media_type": mediaType, "path": saved.path,
		"size_bytes": saved.size, "sha256": saved.sha256}, func() error {
		fmt.Printf("Сохранено %s  %s  sha256 %s\n", saved.path, byteSize(saved.size), saved.sha256)
		return nil
	})
}

// printText печатает текст в stdout потоком и дописывает перевод строки, если
// содержимое им не заканчивается.
func printText(r io.Reader) error {
	last := byte('\n')
	n, err := io.Copy(os.Stdout, readerFunc(func(p []byte) (int, error) {
		n, err := r.Read(p)
		if n > 0 {
			last = p[n-1]
		}
		return n, err
	}))
	if err != nil {
		return err
	}
	if n > 0 && last != '\n' {
		fmt.Println()
	}
	return nil
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// isBinaryMedia — тип содержимого, который не печатается в терминал: всё,
// кроме text/* и application/json. Тексты артефактов этапов всегда из
// первых двух.
func isBinaryMedia(mediaType string) bool {
	return !strings.HasPrefix(mediaType, "text/") && mediaType != "application/json"
}

// responseMediaType — тип содержимого из ответа без параметров; отсутствие
// заголовка — как у сервера прежней версии: текст.
func responseMediaType(h http.Header) string {
	raw := h.Get("Content-Type")
	if raw == "" {
		return "text/plain"
	}
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.SplitN(raw, ";", 2)[0]))
	}
	return mt
}

// openContent открывает содержимое для чтения потоком. Клиент файловый — без
// общего срока: срок скачивания держит сервер (SDLC_DOWNLOAD_TIMEOUT).
// Ошибка домена приходит конвертом, как у do.
func (c *Client) openContent(ctx context.Context, path string) (*http.Response, error) {
	if err := c.requireHost(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.fileClient().Do(req)
	if err != nil {
		return nil, err
	}
	c.checkServerVersion(resp)
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		return nil, responseError(resp.StatusCode, raw)
	}
	return resp, nil
}

// ── куда писать ─────────────────────────────────────────────────────────────

type destKind int

const (
	destNone   destKind = iota // без -o
	destStdout                 // -o -
	destFile                   // -o <файл>
	destDir                    // -o <каталог>/
)

type destination struct {
	kind destKind
	// Для destFile — путь файла; для destDir — каталог.
	path string
}

// planDownload разбирает -o и проверяет всё, что проверяется без сети:
// каталог существует (clab каталогов не создаёт), файл не занят.
func planDownload(output string) (destination, error) {
	switch {
	case output == "":
		return destination{}, nil
	case output == "-":
		if jsonOut {
			return destination{}, errors.New("-o - пишет байты в stdout и несовместим с --json")
		}
		return destination{kind: destStdout}, nil
	}
	if strings.HasSuffix(output, "/") || strings.HasSuffix(output, string(os.PathSeparator)) {
		return dirDestination(output)
	}
	if st, err := os.Stat(output); err == nil && st.IsDir() {
		return dirDestination(output)
	}
	parent := filepath.Dir(output)
	if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		return destination{}, fmt.Errorf("каталог %s не существует: clab каталоги не создаёт", parent)
	}
	if err := refuseExisting(output); err != nil {
		return destination{}, err
	}
	return destination{kind: destFile, path: output}, nil
}

func dirDestination(dir string) (destination, error) {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return destination{}, fmt.Errorf("каталог %s не существует: clab каталоги не создаёт", dir)
	}
	return destination{kind: destDir, path: dir}, nil
}

// refuseExisting — отказ, если путь занят: загрузка ничего не перезаписывает.
func refuseExisting(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s уже есть: clab не перезаписывает файлы. Удалите его или выберите другое имя", path)
	}
	return nil
}

type savedFile struct {
	path   string
	size   int64
	sha256 string
}

// save пишет тело ответа потоком во временный файл рядом с целью и
// переименовывает по завершении. Длина принятого тела сверяется с
// Content-Length; при любой ошибке временный файл удаляется, цели на диске
// нет.
func (d destination) save(id, mediaType string, resp *http.Response) (savedFile, error) {
	dir, target := d.path, ""
	if d.kind == destFile {
		dir, target = filepath.Dir(d.path), d.path
	} else {
		target = filepath.Join(dir, downloadName(resp.Header, id, mediaType))
		if err := refuseExisting(target); err != nil {
			return savedFile{}, err
		}
	}

	tmp, err := os.CreateTemp(dir, ".clab-download-*")
	if err != nil {
		return savedFile{}, err
	}
	// Временный файл убирается на любом выходе, кроме успешной публикации:
	// после неё его уже нет.
	defer os.Remove(tmp.Name())

	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), resp.Body)
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = fmt.Errorf("принято %d байт из %d", n, resp.ContentLength)
	}
	if err != nil {
		_ = tmp.Close()
		return savedFile{}, fmt.Errorf("загрузка оборвана: %w; на диске ничего не осталось", err)
	}
	if err := tmp.Chmod(0o644); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return savedFile{}, err
	}
	if err := tmp.Close(); err != nil {
		return savedFile{}, err
	}
	if err := publishNew(tmp.Name(), target); err != nil {
		return savedFile{}, err
	}
	return savedFile{path: target, size: n, sha256: hex.EncodeToString(sum.Sum(nil))}, nil
}

// publishNew ставит временный файл на место цели, не перезаписывая занятую:
// жёсткая ссылка отказывает атомарно, а на файловой системе без них — проверка
// и переименование.
func publishNew(tmp, target string) error {
	err := os.Link(tmp, target)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return refuseExisting(target)
	}
	if err := refuseExisting(target); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// ── имя файла ───────────────────────────────────────────────────────────────

const maxDownloadNameBytes = 255

// downloadName — имя файла для сохранения в каталог: из Content-Disposition,
// очищенное; пустое или из одних точек — id с расширением по типу.
func downloadName(h http.Header, id, mediaType string) string {
	name := ""
	if _, params, err := mime.ParseMediaType(h.Get("Content-Disposition")); err == nil {
		name = params["filename"]
	}
	if name = cleanDownloadName(name); name != "" {
		return name
	}
	return cleanDownloadName(id + extensionFor(mediaType))
}

// extensionFor — расширение по типу содержимого; неизвестный тип — без него.
func extensionFor(mediaType string) string {
	switch mediaType {
	case "text/markdown":
		return ".md"
	case "text/plain":
		return ".txt"
	case "application/json":
		return ".json"
	case "application/pdf":
		return ".pdf"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}

// cleanDownloadName оставляет от имени безопасное: без каталогов, `..`,
// управляющих и невидимых символов, кавычек, не длиннее 255 байт с
// сохранением расширения. Пусто — если очищать нечего.
func cleanDownloadName(raw string) string {
	name := strings.ToValidUTF8(raw, "")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == '"':
			return -1
		case runtime.GOOS == "windows" && strings.ContainsRune(`<>:|?*`, r):
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if strings.Trim(name, ". ") == "" {
		return ""
	}
	if len(name) <= maxDownloadNameBytes {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) > 32 || ext == name {
		ext = ""
	}
	stem := cutUTF8(name[:len(name)-len(ext)], maxDownloadNameBytes-len(ext))
	if stem = strings.TrimSpace(stem); stem == "" {
		return cutUTF8(name, maxDownloadNameBytes)
	}
	return stem + ext
}

// cutUTF8 обрезает строку до n байт по границе руны.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
