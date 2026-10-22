package tracing

import (
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/secrets"
)

// Headers — заголовки экспортёра OTLP. В них ключи APM, поэтому при любой
// печати (%v, %+v, %#v, slog) видно только их число: значения и имена
// заголовков в журнал не попадают.
type Headers struct {
	m map[string]string
	// set — переменная задана: заголовки сигнала заменяют общие целиком,
	// даже если в строке не нашлось ни одной пары (как в SDK).
	set bool
}

// Get — значение заголовка key.
func (h Headers) Get(key string) string { return h.m[key] }

// Len — число заголовков.
func (h Headers) Len() int { return len(h.m) }

func (h Headers) String() string           { return fmt.Sprintf("[%d headers]", len(h.m)) }
func (h Headers) GoString() string         { return h.String() }
func (h Headers) LogValue() slog.Value     { return slog.StringValue(h.String()) }
func (h Headers) clone() map[string]string { return maps.Clone(h.m) }

// headersFromEnv — заголовки экспортёра так, как их выберет SDK: заголовки
// сигнала (OTEL_EXPORTER_OTLP_TRACES_HEADERS), если заданы, целиком
// заменяют общие (OTEL_EXPORTER_OTLP_HEADERS). Каждая переменная — значением
// или файлом X_FILE по правилу перечня секретов (решение 0052), и разбор
// строгий: неверная пара — ошибка, а не молча выброшенный ключ.
func headersFromEnv(src secrets.Source) (Headers, error) {
	var out Headers
	for _, name := range []string{envHeaders, envTracesHeaders} {
		raw, err := src.Value(name)
		if err != nil {
			return Headers{}, err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// В ошибке разбора — имя той формы, откуда пришла строка.
		from := name
		if strings.TrimSpace(src.Getenv(name)) == "" {
			from = name + "_FILE"
		}
		m, err := parseHeaders(from, raw)
		if err != nil {
			return Headers{}, err
		}
		out = Headers{m: m, set: true}
	}
	return out, nil
}

// parseHeaders разбирает key1=value1,key2=value2 по правилам SDK
// (otlptracehttp/internal/envconfig): ключ — HTTP-токен с обрезкой пробелов,
// значение %-декодируется и обрезается. Отличие одно и намеренное: SDK
// неверную пару выбрасывает и печатает её в журнал целиком или значением, а
// здесь она — ошибка с именем переменной, номером пары и причиной, без
// ключа и значения. Пустые сегменты (a=b,) пропускаются.
func parseHeaders(name, raw string) (map[string]string, error) {
	headers := map[string]string{}
	for i, pair := range strings.Split(raw, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		n := i + 1
		key, value, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("%s: pair %d has no '=': the format is key1=value1,key2=value2", name, n)
		}
		key = strings.TrimSpace(key)
		if !validHeaderKey(key) {
			return nil, fmt.Errorf("%s: pair %d: the key is not a valid HTTP header name (letters, digits and !#$%%&'*+-.^_`|~ only)",
				name, n)
		}
		// Ошибку url.PathUnescape не заворачиваем: в ней кусок значения.
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return nil, fmt.Errorf("%s: pair %d: the value has a broken %%-escape (a literal %% is written %%25)", name, n)
		}
		headers[key] = strings.TrimSpace(decoded)
	}
	return headers, nil
}

// validHeaderKey — токен RFC 9110, как isValidHeaderKey SDK.
func validHeaderKey(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
}
