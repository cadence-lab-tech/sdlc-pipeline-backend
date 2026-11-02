package indexer

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// Каталоги, которых в индексе быть не должно. Они не пишутся людьми, но
// составляют основную массу файлов репозитория: без этого списка прогон
// выкачивает node_modules и платит за его эмбеддинги.
var skipDirs = map[string]bool{
	".git": true, ".idea": true, ".vscode": true, "node_modules": true,
	"vendor": true, "dist": true, "build": true, "target": true, "out": true,
	".next": true, ".nuxt": true, "__pycache__": true, ".venv": true, "venv": true,
	".terraform": true, "coverage": true, ".cache": true, ".gradle": true,
}

// Расширения без текста, который стоит искать: картинки, шрифты, архивы,
// собранные бинарники.
var skipExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".ico": true, ".svg": true, ".pdf": true, ".zip": true, ".gz": true,
	".tar": true, ".bz2": true, ".xz": true, ".7z": true, ".rar": true,
	".mp3": true, ".mp4": true, ".mov": true, ".avi": true, ".wav": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".otf": true,
	".so": true, ".dylib": true, ".dll": true, ".exe": true, ".bin": true,
	".class": true, ".jar": true, ".wasm": true, ".pyc": true, ".db": true,
}

// Сгенерированные файлы: формально текст, но искать по ним нечего, а объём
// у них как у половины репозитория.
var skipNames = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"go.sum": true, "cargo.lock": true, "composer.lock": true,
	"poetry.lock": true, "gemfile.lock": true,
}

// SkipPath решает по листингу дерева, стоит ли выкачивать файл. Решение по
// пути и размеру, до сети: смысл инкрементальной индексации в том, чтобы не
// трогать лишнее, а не в том, чтобы быстро его выбрасывать.
func SkipPath(filePath string, size int64, maxBytes int, extra []string) (bool, string) {
	for _, seg := range strings.Split(path.Dir(filePath), "/") {
		if skipDirs[seg] {
			return true, "служебный каталог " + seg
		}
	}
	name := strings.ToLower(path.Base(filePath))
	if skipNames[name] {
		return true, "сгенерированный файл"
	}
	if skipExts[strings.ToLower(path.Ext(filePath))] {
		return true, "не текстовый формат"
	}
	if strings.HasSuffix(name, ".min.js") || strings.HasSuffix(name, ".min.css") {
		return true, "минифицированный файл"
	}
	if maxBytes > 0 && size > int64(maxBytes) {
		return true, "файл больше предела индексации"
	}
	for _, pattern := range extra {
		if pattern == "" {
			continue
		}
		if ok, _ := path.Match(pattern, filePath); ok {
			return true, "исключён настройкой репозитория: " + pattern
		}
		if ok, _ := path.Match(pattern, name); ok {
			return true, "исключён настройкой репозитория: " + pattern
		}
	}
	return false, ""
}

// SkipContent отсеивает то, что по пути распознать нельзя: бинарник без
// расширения или файл в неизвестной кодировке. Индекс — это текст, кусок
// бинарника в векторе не значит ничего. Вторая причина строже вкуса: Postgres
// не примет в text ни нулевой байт, ни невалидный UTF-8, и такой файл уронил
// бы весь прогон, а не один себя.
//
// Проверяется всё содержимое, а не его голова. Срез в 8000 байт резал
// многобайтовый символ на границе, и валидный файл с кириллицей считался
// бинарным (CLAB-361); а нулевой байт за пределами среза пропускался и ронял
// запись. Размер файла и так ограничен SkipPath, проверка дешёвая.
//
// Вторым значением — причина для лога: что именно и где.
func SkipContent(content []byte) (bool, string) {
	if len(content) == 0 {
		return true, reasonEmpty
	}
	if i := bytes.IndexByte(content, 0); i >= 0 {
		return true, fmt.Sprintf("%s на смещении %d", reasonNUL, i)
	}
	if utf8.Valid(content) {
		return false, ""
	}
	// Невалидное место ищется только когда оно точно есть: обычный файл
	// проходит одной проверкой.
	for i := 0; i < len(content); {
		r, size := utf8.DecodeRune(content[i:])
		if r == utf8.RuneError && size <= 1 {
			return true, fmt.Sprintf("%s на смещении %d", reasonInvalidUTF8, i)
		}
		i += size
	}
	return true, reasonInvalidUTF8
}

// Причины отсева по содержимому. Первые две попадают в warn воркера и
// остаются префиксом строки «причина на смещении N».
const (
	reasonEmpty       = "пустой файл"
	reasonNUL         = "нулевой байт"
	reasonInvalidUTF8 = "невалидный UTF-8"
)

// Lang — язык файла для выдачи и подсветки. Не влияет на разбор: правила
// извлечения символов выбираются по расширению отдельно.
func Lang(filePath string) string {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".go":
		return "go"
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "jsx"
	case ".py":
		return "python"
	case ".sql":
		return "sql"
	case ".md", ".mdx":
		return "markdown"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".sh", ".bash":
		return "shell"
	case ".css", ".scss":
		return "css"
	case ".html":
		return "html"
	case ".toml":
		return "toml"
	case ".dockerfile":
		return "dockerfile"
	default:
		if strings.EqualFold(path.Base(filePath), "dockerfile") {
			return "dockerfile"
		}
		if strings.EqualFold(path.Base(filePath), "makefile") {
			return "make"
		}
		return ""
	}
}
