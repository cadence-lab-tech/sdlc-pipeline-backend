# Команды разработки. Всё, что касается контейнеров, живёт в dev/.

BIN     := bin
VERSION ?= dev

.PHONY: build run test lint tidy up down logs psql reset image

# Три бинарника: сервис, фоновые задания и CLI для CI.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/api ./cmd/api
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/worker ./cmd/worker
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/sdlc ./cmd/sdlc

test:
	go test ./...

lint:
	go vet ./...
	gofmt -l cmd internal migrations

tidy:
	go mod tidy

# Локальный запуск всего бэка: база, приложение, инструменты.
up:
	docker compose -f dev/docker-compose.yml up -d --build

down:
	docker compose -f dev/docker-compose.yml down

# Снести вместе с данными базы: сиды применяются только на пустой базе,
# поэтому пересобрать демонстрационный проект можно только так.
reset:
	docker compose -f dev/docker-compose.yml down -v

logs:
	docker compose -f dev/docker-compose.yml logs -f api

psql:
	docker compose -f dev/docker-compose.yml exec postgres psql -U sdlc -d sdlc

image:
	docker build -t sdlc-pipeline-backend:$(VERSION) --build-arg VERSION=$(VERSION) .
