# Сборка статического бинарника. Схема и сиды встроены в него через
# migrations/embed.go, поэтому образ поднимает базу сам — отдельный шаг с
# миграциями в compose не нужен и не может разъехаться с версией сервиса.
FROM golang:1.23-alpine AS build

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
        -o /out/api ./cmd/api

# distroless без shell: внутрь нечего эксплуатировать, а проба готовности
# делается самим бинарником (api healthcheck).
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/api /api

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/api"]
