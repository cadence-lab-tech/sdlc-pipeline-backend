# Сборка статического бинарника. Схема и сиды встроены в него через
# migrations/embed.go, поэтому образ поднимает базу сам — отдельный шаг с
# миграциями в compose не нужен и не может разъехаться с версией сервиса.
# Go 1.25: этой версии требует MCP SDK (github.com/modelcontextprotocol/go-sdk).
FROM golang:1.25-alpine AS build

WORKDIR /src

# Зависимости отдельным слоем: правка кода не тянет за собой повторную
# выкачку модулей.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/sdlc ./cmd/sdlc

# distroless без shell: внутрь нечего эксплуатировать, а проба готовности
# делается самим бинарником (api healthcheck).
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/api /api
# Воркер и CLI лежат в том же образе: одна сборка, одна версия схемы.
# Воркер запускается тем же образом с другим entrypoint.
COPY --from=build /out/worker /worker
COPY --from=build /out/sdlc /sdlc

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/api"]
