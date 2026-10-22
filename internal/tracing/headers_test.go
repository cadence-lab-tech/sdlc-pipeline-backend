package tracing

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
)

// leak — маркер секрета в строке заголовков: его не должно быть ни в
// ошибке, ни в журнале.
const leak = "Mk7otlpLeak"

func headersFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "otlp-headers")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// exportOnce — FromEnv и Start, как в cmd/api, на окружении env (и в
// процессе: адрес и прочее SDK читает сам), один спан до сборщика.
// Возвращает заголовки запроса сборщику и журнал api.
func exportOnce(t *testing.T, env map[string]string) (http.Header, string) {
	t.Helper()
	col := &collector{}
	srv := httptest.NewServer(col)
	defer srv.Close()
	env[envEndpoint] = srv.URL
	for k, v := range env {
		t.Setenv(k, v)
	}
	settings, err := FromEnv(envOf(env))
	if err != nil || !settings.Enabled {
		t.Fatalf("FromEnv: %v", err)
	}
	var buf bytes.Buffer
	tr, err := Start(context.Background(), settings, "v1.2.3", slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, span := tr.Provider().Tracer("test").Start(context.Background(), "GET /v1/instance")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.headers) != 1 {
		t.Fatalf("сборщик принял %d запросов, ждали 1", len(col.headers))
	}
	return col.headers[0], buf.String()
}

// AC-2: заголовок из файла доходит до сборщика, %-экранирование
// раскрывается, завершающий перевод строки отброшен; в журнале и в печати
// настроек значения нет.
func TestHeadersFromFileReachCollector(t *testing.T) {
	path := headersFile(t, "authorization=Bearer%20"+leak+"\n")
	got, logs := exportOnce(t, map[string]string{envHeaders + "_FILE": path})
	if v := got.Get("Authorization"); v != "Bearer "+leak {
		t.Fatalf("заголовок из %s_FILE не дошёл: %q", envHeaders, v)
	}
	if strings.Contains(logs, leak) {
		t.Fatalf("в журнале значение заголовка:\n%s", logs)
	}

	s, err := FromEnv(envOf(map[string]string{envEndpoint: "http://c:4318", envHeaders + "_FILE": path}))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("настройки", "settings", s, "headers", s.Headers)
	printed := fmt.Sprintf("%v %+v %#v %s", s, s, s, buf.String())
	if strings.Contains(printed, leak) || strings.Contains(printed, "uthorization") {
		t.Fatalf("печать настроек показывает заголовки: %s", printed)
	}
}

// AC-1 в пределах пакета: значение и файл с \n и \r\n дают одни заголовки.
func TestHeadersValueEqualsFile(t *testing.T) {
	for _, name := range []string{envHeaders, envTracesHeaders} {
		byValue, err := FromEnv(envOf(map[string]string{envEndpoint: "http://c:4318", name: "api-key=" + leak}))
		if err != nil || byValue.Headers.Get("api-key") != leak {
			t.Fatalf("%s значением: %v", name, err)
		}
		for _, tail := range []string{"\n", "\r\n", ""} {
			byFile, err := FromEnv(envOf(map[string]string{
				envEndpoint: "http://c:4318", name + "_FILE": headersFile(t, "api-key="+leak+tail),
			}))
			if err != nil {
				t.Fatal(err)
			}
			if byFile.Headers.Get("api-key") != leak || byFile.Headers.Len() != 1 {
				t.Fatalf("%s_FILE с хвостом %q: %d заголовков", name, tail, byFile.Headers.Len())
			}
		}
	}
}

// AC-3: обе формы одной переменной — отказ с обоими именами, без значения.
func TestHeadersBothFormsRefused(t *testing.T) {
	for _, name := range []string{envHeaders, envTracesHeaders} {
		_, err := FromEnv(envOf(map[string]string{
			name: "api-key=" + leak, name + "_FILE": headersFile(t, "api-key="+leak),
		}))
		if err == nil {
			t.Fatalf("%s: обе формы приняты", name)
		}
		msg := err.Error()
		if !strings.Contains(msg, name+" ") || !strings.Contains(msg, name+"_FILE") || strings.Contains(msg, leak) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// AC-4: плохой файл и плохая строка — отказ с именем и причиной, без ключа
// и значения пары; и при выключенных трассировках (адреса нет).
func TestHeadersRefusedWithoutContent(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, path string
	}{
		{"нет файла", filepath.Join(dir, "missing")},
		{"каталог", dir},
		{"пустой файл", headersFile(t, "\n")},
	} {
		_, err := FromEnv(envOf(map[string]string{envHeaders + "_FILE": tc.path}))
		if err == nil || !strings.Contains(err.Error(), envHeaders+"_FILE") || !strings.Contains(err.Error(), tc.path) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}

	bad := []struct {
		raw, pair, reason string
	}{
		{"api-key: " + leak, "pair 1", "no '='"},
		{"x-ok=1,api-key " + leak + "=v", "pair 2", "not a valid HTTP header name"},
		{"Authorization=Bearer%zz" + leak, "pair 1", "%-escape"},
		{"a=b,,Authorization=Bearer%" + leak, "pair 3", "%-escape"},
	}
	for _, name := range []string{envHeaders, envTracesHeaders} {
		for _, tc := range bad {
			for form, env := range map[string]map[string]string{
				name:           {name: tc.raw},
				name + "_FILE": {name + "_FILE": headersFile(t, tc.raw+"\n")},
			} {
				_, err := FromEnv(envOf(env))
				if err == nil {
					t.Fatalf("%s=%q принята", form, tc.raw)
				}
				msg := err.Error()
				if !strings.HasPrefix(msg, form+":") || !strings.Contains(msg, tc.pair) ||
					!strings.Contains(msg, tc.reason) {
					t.Fatalf("%s=%q: %v", form, tc.raw, err)
				}
				for _, piece := range []string{leak, "api-key", "Bearer", "%zz"} {
					if strings.Contains(msg, piece) {
						t.Fatalf("%s: в ошибке %q: %v", form, piece, err)
					}
				}
			}
		}
	}

	// Пустые сегменты — не ошибка.
	s, err := FromEnv(envOf(map[string]string{envHeaders: " a=b,, c = d%2Ce ,"}))
	if err != nil || s.Enabled {
		t.Fatalf("пустые сегменты: %+v %v", s, err)
	}
	s, err = FromEnv(envOf(map[string]string{envEndpoint: "http://c:4318", envHeaders: " a=b,, c = d%2Ce ,"}))
	if err != nil || s.Headers.Len() != 2 || s.Headers.Get("a") != "b" || s.Headers.Get("c") != "d,e" {
		t.Fatalf("пустые сегменты: %d %v", s.Headers.Len(), err)
	}
}

// AC-5: разборщик заголовков SDK печатает в глобальный журнал куски
// неверной строки (пару без '=' целиком, значение с плохим
// %-экранированием). Первая половина теста доказывает саму утечку на SDK
// без нашей правки: если она перестанет воспроизводиться (SDK исправят),
// тест скажет об этом, и проверку стоит пересмотреть. Вторая — что api с той
// же строкой отказывает до экспортёра и в журнале ничего нет.
func TestBadHeadersNeverReachSDKLog(t *testing.T) {
	for _, raw := range []string{"api-key: " + leak, "Authorization=Bearer%zz" + leak} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(envEndpoint, "http://127.0.0.1:1")
			t.Setenv(envHeaders, raw)

			var sdk bytes.Buffer
			otel.SetLogger(logr.FromSlogHandler(slog.NewTextHandler(&sdk, nil)))
			exp, err := otlptracehttp.New(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_ = exp.Shutdown(context.Background())
			if !strings.Contains(sdk.String(), leak) {
				t.Fatalf("SDK больше не печатает неверную строку заголовков — пересмотреть проверку:\n%s", sdk.String())
			}

			// Путь cmd/api: FromEnv до Start, журнал SDK — в журнал api.
			var api bytes.Buffer
			log := slog.New(slog.NewTextHandler(&api, nil))
			otel.SetLogger(logr.FromSlogHandler(log.Handler()))
			settings, err := FromEnv(os.Getenv)
			if err == nil {
				t.Fatalf("неверная строка принята: %+v", settings)
			}
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("в ошибке старта значение: %v", err)
			}
			if api.Len() != 0 {
				t.Fatalf("журнал api не пуст:\n%s", api.String())
			}
		})
	}
}

// AC-6: заголовки сигнала целиком заменяют общие, из какой бы формы ни
// пришли; задана одна — она.
func TestSignalHeadersReplaceGeneral(t *testing.T) {
	cases := []struct {
		name       string
		env        func() map[string]string
		want, none string
	}{
		{"общие файлом, сигнала значением", func() map[string]string {
			return map[string]string{
				envHeaders + "_FILE": headersFile(t, "x-general=g\n"), envTracesHeaders: "x-traces=t",
			}
		}, "X-Traces", "X-General"},
		{"общие значением, сигнала файлом", func() map[string]string {
			return map[string]string{
				envHeaders: "x-general=g", envTracesHeaders + "_FILE": headersFile(t, "x-traces=t\n"),
			}
		}, "X-Traces", "X-General"},
		{"только общие", func() map[string]string {
			return map[string]string{envHeaders + "_FILE": headersFile(t, "x-general=g\n")}
		}, "X-General", "X-Traces"},
		{"только сигнала", func() map[string]string {
			return map[string]string{envTracesHeaders + "_FILE": headersFile(t, "x-traces=t\n")}
		}, "X-Traces", "X-General"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := exportOnce(t, tc.env())
			if got.Get(tc.want) == "" || got.Get(tc.none) != "" {
				t.Fatalf("%s=%q %s=%q", tc.want, got.Get(tc.want), tc.none, got.Get(tc.none))
			}
		})
	}
}
