# Сборка статического бинарника. Схема и сиды встроены в него через
# migrations/embed.go, поэтому образ поднимает базу сам — отдельный шаг с
# миграциями в compose не нужен и не может разъехаться с версией сервиса.
# Go 1.26: не ниже 1.25 требует MCP SDK (github.com/modelcontextprotocol/go-sdk),
# 1.26 — golang.org/x/net и x/crypto с исправлениями безопасности (CLAB-169),
# патч не ниже go1.26.10 — crypto/x509, GO-2026-6809 (CLAB-224). Строка
# toolchain в go.mod здесь не действует (образ ставит GOTOOLCHAIN=local): Go
# берётся из плавающего тега golang:1.26-alpine, поэтому локально — --pull.
#
# Образ версии выходит под linux/amd64 и linux/arm64 (release.yml). Этап
# сборки закреплён за платформой сборщика ($BUILDPLATFORM), а целевую
# архитектуру получает кросс-компиляцией через GOOS/GOARCH: без этого
# arm64-половина сборки на amd64-раннере шла бы через QEMU, которого в CI
# нет, и падала. Обычный `docker build` (make image, deploy.yml) ничего не
# замечает: BuildKit подставляет в TARGETOS/TARGETARCH платформу хоста.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Зависимости отдельным слоем: правка кода не тянет за собой повторную
# выкачку модулей.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/clab ./cmd/clab && \
    mkdir -p /out/blobs

# LICENSE из корня репозитория ложится в корень образа, /LICENSE. Файла нет —
# каталог пуст и сборка по SHA для нашего инстанса не падает; образ версии
# без лицензии не выпускает release.yml, он проверяет файл до сборки.
RUN mkdir -p /out/license && if [ -f LICENSE ]; then cp LICENSE /out/license/; fi

# distroless без shell: внутрь нечего эксплуатировать, а проба готовности
# делается самим бинарником (api healthcheck). В этом этапе только COPY —
# никаких RUN: он выполняется под целевой платформой, и команда внутри него
# потребовала бы эмуляции.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/api /api
# Воркер и CLI лежат в том же образе: одна сборка, одна версия схемы.
# Воркер запускается тем же образом с другим entrypoint.
COPY --from=build /out/worker /worker
COPY --from=build /out/clab /clab
# Каталог для вынесенного содержимого (SDLC_BLOB_DIR). Заводится в образе с
# владельцем nonroot: именованный том наследует права каталога образа, а
# пустой том создался бы под root, и сервис не смог бы в него писать.
COPY --from=build --chown=nonroot:nonroot /out/blobs /blobs
COPY --from=build /out/license/ /

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/api"]
