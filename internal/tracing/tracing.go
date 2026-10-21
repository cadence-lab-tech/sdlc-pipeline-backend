// Package tracing — экспорт трассировок api по OTLP/HTTP (CLAB-204).
//
// Включается только явно: непустым адресом сборщика или
// OTEL_TRACES_EXPORTER=otlp. Пакет self-hosted пробрасывает переменные OTEL_*
// пустыми строками, и пустое значение здесь равно отсутствию. Поэтому
// contrib/exporters/autoexport не годится: пустой OTEL_TRACES_EXPORTER он
// считает otlp и пошёл бы на localhost:4318 у каждой установки, а заодно
// тянет gRPC- и Prometheus-экспортёры.
//
// Адрес, TLS, таймаут, сжатие, выборку и пакетную отправку SDK читает из
// стандартных OTEL_* сам; здесь — решение «включать ли», отказ на том, чего
// api не умеет, ресурс с версией сборки и журнал. Заголовки экспортёра — секрет
// перечня secrets.List (CLAB-235): они читаются здесь, значением или файлом
// X_FILE, разбираются своим строгим разбором и уходят в экспортёр опцией.
//
// Пакет импортирует только cmd/api: SDK и экспортёр не должны попадать в
// воркер и clab (их SBOM и govulncheck).
package tracing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/secrets"
)

// DefaultServiceName — service.name, если OTEL_SERVICE_NAME не задан.
const DefaultServiceName = "cadence-api"

// Переменные, которые читает само решение о включении, и заголовки.
// Остальные OTEL_* (TLS, выборка, ресурс) читает SDK.
const (
	envSDKDisabled     = "OTEL_SDK_DISABLED"
	envExporter        = "OTEL_TRACES_EXPORTER"
	envEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envProtocol        = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envTracesProtocol  = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	envHeaders         = "OTEL_EXPORTER_OTLP_HEADERS"
	envTracesHeaders   = "OTEL_EXPORTER_OTLP_TRACES_HEADERS"
	protocolHTTPProto  = "http/protobuf"
	defaultTracesPath  = "/v1/traces"
	defaultSDKEndpoint = "https://localhost:4318" + defaultTracesPath
)

// Settings — итог разбора окружения.
type Settings struct {
	Enabled bool
	// Endpoint — адрес приёма спанов для строки журнала: без userinfo,
	// строки запроса и фрагмента. Заголовков здесь нет и быть не должно.
	Endpoint string
	// Headers — заголовки экспортёра; при печати — только число.
	Headers Headers
}

// FromEnv решает, включены ли трассировки. Ошибка — значение, которого api
// не поддерживает: сервис с ней не стартует, как с другими неверными
// настройками, — молча работать без трассировок, когда их просили, хуже.
func FromEnv(getenv func(string) string) (Settings, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }

	// Заголовки — тоже и при выключенных трассировках, и первыми: неверная
	// строка не должна дойти до разбора SDK, который печатает её куски.
	headers, err := headersFromEnv(secrets.Source{Getenv: getenv})
	if err != nil {
		return Settings{}, err
	}

	exporter := strings.ToLower(get(envExporter))
	switch exporter {
	case "", "otlp", "none":
	default:
		return Settings{}, fmt.Errorf("%s=%q не поддерживается: api экспортирует трассировки только по OTLP (otlp) или не экспортирует (none)",
			envExporter, get(envExporter))
	}
	for _, k := range []string{envProtocol, envTracesProtocol} {
		if v := get(k); v != "" && !strings.EqualFold(v, protocolHTTPProto) {
			return Settings{}, fmt.Errorf("%s=%q не поддерживается: api отправляет трассировки только по OTLP/HTTP (%s), gRPC и http/json — нет",
				k, v, protocolHTTPProto)
		}
	}
	// Адрес проверяется и при выключенных трассировках: опечатка в .env
	// всплывёт сразу, а не в тот день, когда трассировки включат.
	base, traces := get(envEndpoint), get(envTracesEndpoint)
	for k, v := range map[string]string{envEndpoint: base, envTracesEndpoint: traces} {
		if v == "" {
			continue
		}
		if err := checkEndpoint(v); err != nil {
			return Settings{}, fmt.Errorf("%s: %w", k, err)
		}
	}

	if strings.EqualFold(get(envSDKDisabled), "true") || exporter == "none" {
		return Settings{}, nil
	}
	if base == "" && traces == "" && exporter != "otlp" {
		return Settings{}, nil
	}
	return Settings{Enabled: true, Endpoint: logEndpoint(base, traces), Headers: headers}, nil
}

// checkEndpoint отбивает адрес, который SDK понял бы не так: без схемы
// («otel-collector:4318») url.Parse видит схему otel-collector и пустой
// хост, и экспортёр молча ушёл бы на адрес по умолчанию.
func checkEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("адрес сборщика не разбирается как URL")
	}
	if s := strings.ToLower(u.Scheme); (s != "http" && s != "https") || u.Host == "" {
		return fmt.Errorf("адрес сборщика должен быть URL вида http://host:4318 или https://host:4318")
	}
	return nil
}

// logEndpoint повторяет выбор адреса SDK: адрес сигнала — как есть, общий —
// база, к которой добавляется /v1/traces.
func logEndpoint(base, traces string) string {
	var u *url.URL
	switch {
	case traces != "":
		u, _ = url.Parse(traces)
		if u.Path == "" {
			u.Path = "/"
		}
	case base != "":
		u, _ = url.Parse(base)
		u.Path = path.Join("/", u.Path, defaultTracesPath)
	default:
		return defaultSDKEndpoint
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// Tracing — провайдер трассировок api. nil — выключено; методы nil-безопасны.
type Tracing struct {
	tp *sdktrace.TracerProvider
}

// Start создаёт экспортёр и провайдер. Сети не трогает: OTLP/HTTP
// соединяется при первой отправке, и недоступный сборщик старт не роняет.
func Start(ctx context.Context, s Settings, version string, log *slog.Logger) (*Tracing, error) {
	if !s.Enabled {
		return nil, nil
	}
	// Ошибки экспорта SDK отдаёт глобальному обработчику, свой журнал —
	// глобальному logr: оба — в slog сервиса, ошибки с ограничением частоты.
	otel.SetErrorHandler(newErrorHandler(log, errorLogInterval))
	otel.SetLogger(logr.FromSlogHandler(log.Handler()))

	var opts []otlptracehttp.Option
	if s.Headers.set {
		opts = append(opts, otlptracehttp.WithHeaders(s.Headers.clone()))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("экспортёр OTLP: %w", err)
	}
	res, err := newResource(ctx, version, log)
	if err != nil {
		return nil, err
	}
	// Пакетная отправка в фоне: запрос не ждёт сборщика, а переполненная
	// очередь отбрасывает спаны, а не держит обработчики.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	log.Info("трассировки включены", "endpoint", s.Endpoint, "protocol", protocolHTTPProto)
	return &Tracing{tp: tp}, nil
}

// newResource: service.name по умолчанию cadence-api, OTEL_SERVICE_NAME и
// OTEL_RESOURCE_ATTRIBUTES поверх, service.version — всегда версия сборки:
// та же строка, что в /v1/instance, и переопределить её из окружения нельзя.
func newResource(ctx context.Context, version string, log *slog.Logger) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", DefaultServiceName)),
		resource.WithFromEnv(),
		resource.WithAttributes(attribute.String("service.version", version)),
	)
	// Неразборчивый OTEL_RESOURCE_ATTRIBUTES по спецификации не повод
	// падать: SDK отдаёт то, что разобрал, и частичную ошибку.
	if errors.Is(err, resource.ErrPartialResource) {
		log.Warn("трассировки: OTEL_RESOURCE_ATTRIBUTES разобран не целиком", "error", err)
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ресурс трассировок: %w", err)
	}
	return res, nil
}

// Provider — провайдер для middleware; nil, если трассировки выключены.
func (t *Tracing) Provider() trace.TracerProvider {
	if t == nil {
		return nil
	}
	return t.tp
}

// Shutdown отправляет накопленные спаны в пределах ctx: недоступный
// сборщик не продлевает остановку дальше SDLC_SHUTDOWN_TIMEOUT.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return t.tp.Shutdown(ctx)
}
