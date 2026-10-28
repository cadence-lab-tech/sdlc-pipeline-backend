package httpapi

import (
	"strings"
	"testing"
)

// Санитизация имени и типа и заголовок отдачи — без базы (CLAB-296, R-3).

func TestSanitizeFileName(t *testing.T) {
	for raw, want := range map[string]string{
		"report.pdf":             "report.pdf",
		"C:\\Users\\a\\x.png":    "x.png",
		"../../etc/passwd":       "passwd",
		" spaced name.txt ":      "spaced name.txt",
		"q\"uote.txt":            "quote.txt",
		"rtl\u202Egnp.exe":       "rtlgnp.exe",
		"":                       defaultFileName,
		"...":                    defaultFileName,
		"dir/":                   defaultFileName,
		"\xff\xfeбитое.txt":      "битое.txt",
		strings.Repeat("a", 300): strings.Repeat("a", 255),
	} {
		if got := sanitizeFileName(raw); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, ждали %q", raw, got, want)
		}
	}
	long := sanitizeFileName(strings.Repeat("ж", 200) + ".tar.gz")
	if len(long) > maxFileNameBytes || !strings.HasSuffix(long, ".gz") || !strings.HasPrefix(long, "жж") {
		t.Errorf("длинное имя: %d байт, %q", len(long), long[len(long)-8:])
	}
}

func TestContentDispositionEncoding(t *testing.T) {
	cd := contentDisposition("inline", "Отчёт 100%.pdf")
	if cd != `inline; filename="_____ 100_.pdf"; filename*=UTF-8''%D0%9E%D1%82%D1%87%D1%91%D1%82%20100%25.pdf` {
		t.Fatalf("заголовок: %s", cd)
	}
	if got := dispositionFor("image/png", jpegBytes); got != "attachment" {
		t.Fatalf("PNG с сигнатурой JPEG: %s", got)
	}
	if got := dispositionFor("text/html", []byte("<p>")); got != "attachment" {
		t.Fatalf("HTML: %s", got)
	}
	if got := dispositionFor("text/plain", []byte("<script>")); got != "inline" {
		t.Fatalf("текст: %s", got)
	}
}
