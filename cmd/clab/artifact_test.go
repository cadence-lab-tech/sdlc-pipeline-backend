package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// CLAB-298: clab artifact -o <файл|каталог/|->, санитизация имени, запись
// потоком и отказы без -o.

// contentServer отдаёт одно содержимое с заданными заголовками и считает
// запросы и байты, которые клиент прочитал из тела.
type contentServer struct {
	status      int
	contentType string
	disposition string
	body        []byte
	// truncateAt > 0: заявить Content-Length всего тела, а отдать только
	// первые truncateAt байт и оборвать соединение.
	truncateAt int

	calls atomic.Int32
	read  atomic.Int64
}

// bodyCounter считает прочитанное из тела ответа.
type bodyCounter struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b bodyCounter) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

type bodyCountTransport struct {
	next http.RoundTripper
	n    *atomic.Int64
}

func (t bodyCountTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err == nil {
		resp.Body = bodyCounter{resp.Body, t.n}
	}
	return resp, err
}

func newContentClient(t *testing.T, s *contentServer) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		if s.status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"артефакта нет"}}`))
			return
		}
		if s.contentType != "" {
			w.Header().Set("Content-Type", s.contentType)
		}
		if s.disposition != "" {
			w.Header().Set("Content-Disposition", s.disposition)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(s.body)))
		if s.truncateAt > 0 {
			_, _ = w.Write(s.body[:s.truncateAt])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(s.body)
	}))
	t.Cleanup(srv.Close)
	hc := srv.Client()
	hc.Transport = bodyCountTransport{next: hc.Transport, n: &s.read}
	return &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", http: hc}
}

const artID = "0f3c9a7e-1111-4222-8333-444455556666"

func runArtifact(t *testing.T, c *Client, args ...string) (stdout string, err error) {
	t.Helper()
	out, _, err := capture(t, func() error {
		return cmdArtifact(context.Background(), c, append([]string{artID}, args...))
	})
	return out, err
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

// AC-22.1: -o <файл> — байты потоком, sha совпадает, печатаются путь,
// размер и sha256.
func TestArtifactOutputFile(t *testing.T) {
	withJSON(t, false)
	data := randomBytes(3<<20 + 17)
	s := &contentServer{contentType: "image/png", body: data}
	c := newContentClient(t, s)
	dir := t.TempDir()
	target := filepath.Join(dir, "out.png")

	out, err := runArtifact(t, c, "-o", target)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, data) {
		t.Fatal("содержимое файла не совпало")
	}
	for _, want := range []string{target, fmt.Sprint(len(data)), sha256Hex(data)} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if names := dirEntries(t, dir); len(names) != 1 {
		t.Errorf("в каталоге лишнее: %v", names)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o644 {
		t.Errorf("права: %v", info.Mode())
	}
}

// --json с -o — сведения о записанном файле.
func TestArtifactOutputFileJSON(t *testing.T) {
	withJSON(t, true)
	data := []byte("\x89PNG....")
	c := newContentClient(t, &contentServer{contentType: "image/png", body: data})
	target := filepath.Join(t.TempDir(), "x.png")
	out, err := runArtifact(t, c, "-o", target)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["path"] != target ||
		got["sha256"] != sha256Hex(data) || got["size_bytes"] != float64(len(data)) || got["id"] != artID {
		t.Fatalf("вывод: %s %v", out, err)
	}
}

// AC-22.2: -o <каталог>/ — имя из Content-Disposition (filename* с UTF-8).
func TestArtifactOutputDirUsesServerName(t *testing.T) {
	withJSON(t, false)
	data := []byte("PDF")
	s := &contentServer{contentType: "application/pdf", body: data,
		disposition: `attachment; filename="_____.pdf"; filename*=UTF-8''%D0%A1%D1%85%D0%B5%D0%BC%D0%B0%20%D0%B2%D1%85%D0%BE%D0%B4%D0%B0.pdf`}
	c := newContentClient(t, s)
	dir := t.TempDir()

	if _, err := runArtifact(t, c, "-o", dir+"/"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "Схема входа.pdf"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("файл с кириллическим именем: %v %v", err, dirEntries(t, dir))
	}
	// Повторно в тот же каталог — отказ, файл не тронут.
	if err := os.WriteFile(filepath.Join(dir, "Схема входа.pdf"), []byte("мой"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runArtifact(t, c, "-o", dir+"/"); err == nil || !strings.Contains(err.Error(), "уже есть") {
		t.Fatalf("повторная запись: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "Схема входа.pdf")); string(got) != "мой" {
		t.Fatal("существующий файл перезаписан")
	}
	// Существующий каталог без слэша — тоже каталог.
	other := t.TempDir()
	if _, err := runArtifact(t, c, "-o", other); err != nil {
		t.Fatalf("каталог без слэша: %v", err)
	}
	if names := dirEntries(t, other); len(names) != 1 || names[0] != "Схема входа.pdf" {
		t.Errorf("каталог без слэша: %v", names)
	}
}

// AC-22.2: враждебные имена не выводят запись из каталога.
func TestArtifactHostileNamesStayInDir(t *testing.T) {
	withJSON(t, false)
	longName := strings.Repeat("я", 200) + ".png" // 404 байта
	cases := map[string]string{
		`attachment; filename="../../evil.png"`:                       "evil.png",
		`attachment; filename="a/b.txt"`:                              "b.txt",
		`attachment; filename="..\\..\\win.txt"`:                      "win.txt",
		`attachment; filename*=UTF-8''..%2F..%2Fevil2.png`:            "evil2.png",
		`attachment; filename*=UTF-8''%22q%22.txt`:                    "q.txt",
		`attachment; filename*=UTF-8''line%0D%0Abreak.txt`:            "linebreak.txt",
		`attachment; filename*=UTF-8''%E2%80%AEgpj.exe`:               "gpj.exe",
		`attachment; filename*=UTF-8''` + escapeAll(longName):         "",
		`attachment; filename=".."`:                                   artID + ".png",
		`attachment; filename="..."`:                                  artID + ".png",
		`attachment`:                                                  artID + ".png",
		``:                                                            artID + ".png",
		`attachment; filename=""`:                                     artID + ".png",
		`attachment; filename="  "`:                                   artID + ".png",
		`attachment; filename="` + strings.Repeat("a", 300) + `.txt"`: "",
	}
	for disposition, want := range cases {
		outer := t.TempDir()
		dir := filepath.Join(outer, "dir")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		s := &contentServer{contentType: "image/png", body: []byte("x"), disposition: disposition}
		if _, err := runArtifact(t, newContentClient(t, s), "-o", dir+"/"); err != nil {
			t.Errorf("%q: %v", disposition, err)
			continue
		}
		names := dirEntries(t, dir)
		if len(names) != 1 {
			t.Errorf("%q: в каталоге %v", disposition, names)
			continue
		}
		if outerNames := dirEntries(t, outer); len(outerNames) != 1 {
			t.Errorf("%q: за пределами каталога появилось %v", disposition, outerNames)
		}
		name := names[0]
		if len(name) > 255 || strings.ContainsAny(name, "/\\\"\r\n") {
			t.Errorf("%q: небезопасное имя %q", disposition, name)
		}
		if want != "" && name != want {
			t.Errorf("%q: имя %q вместо %q", disposition, name, want)
		}
		if want == "" && !strings.HasSuffix(name, ".png") && !strings.HasSuffix(name, ".txt") {
			t.Errorf("%q: потеряно расширение: %q", disposition, name)
		}
	}
}

func escapeAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

func TestCleanDownloadNameKeepsExtensionOnTruncate(t *testing.T) {
	got := cleanDownloadName(strings.Repeat("ж", 200) + ".png")
	if len(got) > 255 || !strings.HasSuffix(got, ".png") {
		t.Fatalf("%d байт: %q", len(got), got)
	}
	if !strings.HasPrefix(got, "жжж") || strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("обрезано посреди руны: %q", got)
	}
}

// AC-22.3: бинарное без -o — отказ с подсказкой, тело не читается.
func TestArtifactBinaryWithoutOutputRefused(t *testing.T) {
	for _, jsonOn := range []bool{false, true} {
		withJSON(t, jsonOn)
		s := &contentServer{contentType: "image/png", body: randomBytes(1 << 20)}
		c := newContentClient(t, s)
		out, err := runArtifact(t, c)
		if err == nil || !strings.Contains(err.Error(), "-o <файл>") || !strings.Contains(err.Error(), "-o <каталог>/") {
			t.Fatalf("json=%v: ошибка %v", jsonOn, err)
		}
		if out != "" {
			t.Errorf("json=%v: в stdout напечатано %d байт", jsonOn, len(out))
		}
		if s.read.Load() != 0 {
			t.Errorf("json=%v: прочитано %d байт тела", jsonOn, s.read.Load())
		}
	}
}

// Текст без -o печатается как до Ч5; --json — {id, media_type, content}.
func TestArtifactTextPrintsAsBefore(t *testing.T) {
	withJSON(t, false)
	for _, tc := range []struct{ contentType, body, want string }{
		{"text/markdown; charset=utf-8", "# Заголовок", "# Заголовок\n"},
		{"text/markdown", "строка\n", "строка\n"},
		{"application/json", `{"a":1}`, "{\"a\":1}\n"},
		{"", "без типа", "без типа\n"},
	} {
		c := newContentClient(t, &contentServer{contentType: tc.contentType, body: []byte(tc.body)})
		out, err := runArtifact(t, c)
		if err != nil || out != tc.want {
			t.Errorf("%q: %q %v", tc.contentType, out, err)
		}
	}
	withJSON(t, true)
	c := newContentClient(t, &contentServer{contentType: "text/markdown; charset=utf-8", body: []byte("текст")})
	out, err := runArtifact(t, c)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil ||
		got["id"] != artID || got["media_type"] != "text/markdown" || got["content"] != "текст" {
		t.Fatalf("--json: %s %v", out, err)
	}
}

// AC-22.3: -o - — байты в stdout, бинарные тоже, без добавленного перевода строки.
func TestArtifactStdout(t *testing.T) {
	withJSON(t, false)
	data := []byte("\x89PNG\r\n\x1a\n\x00\x01\xff")
	c := newContentClient(t, &contentServer{contentType: "image/png", body: data})
	out, err := runArtifact(t, c, "-o", "-")
	if err != nil || out != string(data) {
		t.Fatalf("stdout: %q %v", out, err)
	}
	withJSON(t, true)
	s := &contentServer{contentType: "image/png", body: data}
	if _, err := runArtifact(t, newContentClient(t, s), "-o", "-"); err == nil || s.calls.Load() != 0 {
		t.Fatalf("-o - с --json: %v, запросов %d", err, s.calls.Load())
	}
}

// AC-22.5: цель уже есть — отказ до запроса, файл не тронут.
func TestArtifactExistingTargetRefusedBeforeRequest(t *testing.T) {
	withJSON(t, false)
	s := &contentServer{contentType: "image/png", body: []byte("новое")}
	c := newContentClient(t, s)
	target := filepath.Join(t.TempDir(), "mine.png")
	if err := os.WriteFile(target, []byte("старое"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runArtifact(t, c, "-o", target)
	if err == nil || !strings.Contains(err.Error(), "уже есть") {
		t.Fatalf("ошибка: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "старое" {
		t.Fatal("файл перезаписан")
	}
	if s.calls.Load() != 0 {
		t.Fatalf("запросов к серверу: %d", s.calls.Load())
	}
}

// AC-22.5: обрыв посередине — ошибка, ни цели, ни временного файла.
func TestArtifactTruncatedBodyLeavesNothing(t *testing.T) {
	withJSON(t, false)
	data := randomBytes(1 << 20)
	for name, args := range map[string]func(dir string) []string{
		"файл":    func(dir string) []string { return []string{"-o", filepath.Join(dir, "out.bin")} },
		"каталог": func(dir string) []string { return []string{"-o", dir + "/"} },
	} {
		s := &contentServer{contentType: "application/octet-stream", body: data, truncateAt: len(data) / 2,
			disposition: `attachment; filename="part.bin"`}
		c := newContentClient(t, s)
		dir := t.TempDir()
		_, err := runArtifact(t, c, args(dir)...)
		if err == nil || !strings.Contains(err.Error(), "оборвана") {
			t.Fatalf("%s: ошибка %v", name, err)
		}
		if names := dirEntries(t, dir); len(names) != 0 {
			t.Errorf("%s: на диске осталось %v", name, names)
		}
	}
}

// AC-22.6: каталога нет — ошибка до запроса, каталоги не создаются.
func TestArtifactMissingDirRefusedBeforeRequest(t *testing.T) {
	withJSON(t, false)
	for _, out := range []string{"нет/файл.bin", "нет/"} {
		s := &contentServer{contentType: "image/png", body: []byte("x")}
		c := newContentClient(t, s)
		dir := t.TempDir()
		_, err := runArtifact(t, c, "-o", dir+"/"+out)
		if err == nil || !strings.Contains(err.Error(), "не существует") {
			t.Errorf("%s: %v", out, err)
		}
		if s.calls.Load() != 0 {
			t.Errorf("%s: запросов %d", out, s.calls.Load())
		}
		if names := dirEntries(t, dir); len(names) != 0 {
			t.Errorf("%s: создано %v", out, names)
		}
	}
}

// Ошибка сервера показывается как пришла, и файла нет.
func TestArtifactServerError(t *testing.T) {
	withJSON(t, false)
	c := newContentClient(t, &contentServer{status: 404})
	dir := t.TempDir()
	_, err := runArtifact(t, c, "-o", dir+"/")
	if err == nil || err.Error() != "not_found: артефакта нет" {
		t.Fatalf("ошибка: %v", err)
	}
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Errorf("на диске: %v", names)
	}
}

// Скачивание идёт файловым клиентом: общего срока у него нет.
func TestArtifactUsesFileClient(t *testing.T) {
	withJSON(t, false)
	var sawDeadline atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	hc := srv.Client()
	hc.Timeout = 60 * time.Second
	hc.Transport = deadlineProbe{next: hc.Transport, saw: &sawDeadline}
	c := &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", http: hc}
	if _, err := runArtifact(t, c); err != nil {
		t.Fatal(err)
	}
	if sawDeadline.Load() {
		t.Fatal("запрос скачивания ушёл со сроком обычного клиента")
	}
}

// deadlineProbe замечает срок на контексте запроса: его ставит Client.Timeout.
type deadlineProbe struct {
	next http.RoundTripper
	saw  *atomic.Bool
}

func (p deadlineProbe) RoundTrip(r *http.Request) (*http.Response, error) {
	if _, ok := r.Context().Deadline(); ok {
		p.saw.Store(true)
	}
	return p.next.RoundTrip(r)
}

// id и -o можно писать в любом порядке.
func TestArtifactFlagsBeforeID(t *testing.T) {
	withJSON(t, false)
	c := newContentClient(t, &contentServer{contentType: "image/png", body: []byte("x"), disposition: `attachment; filename="a.png"`})
	dir := t.TempDir()
	if _, _, err := capture(t, func() error {
		return cmdArtifact(context.Background(), c, []string{"-o", dir + "/", artID})
	}); err != nil {
		t.Fatal(err)
	}
	if names := dirEntries(t, dir); len(names) != 1 || names[0] != "a.png" {
		t.Fatalf("в каталоге %v", names)
	}
}
