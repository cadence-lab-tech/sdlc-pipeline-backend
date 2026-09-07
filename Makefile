# Команды разработки. Всё, что касается контейнеров, живёт в dev/.

BIN     := bin
VERSION ?= dev

.PHONY: build run test test-integration lint tidy up down logs psql reset image

# Три бинарника: сервис, фоновые задания и CLI для CI.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/api ./cmd/api
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/worker ./cmd/worker
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/sdlc ./cmd/sdlc

test:
	go test ./...

# Интеграционные проверки идут по настоящей схеме: инкрементальность прогона
# индексации и гибридный поиск живут в связке с базой и на моках не ловятся.
# База выделенная — тест сносит в ней схему целиком.
test-integration:
	docker compose -f dev/docker-compose.yml exec -T postgres \
		psql -U sdlc -d postgres -c 'CREATE DATABASE sdlc_test' 2>/dev/null || true
	# -p 1: база одна на все пакеты, а каждый начинает с чистой схемы —
	# параллельные пакеты снесли бы её друг у друга посреди прогона.
	SDLC_TEST_DATABASE_URL='postgres://sdlc:sdlc@localhost:55432/sdlc_test?sslmode=disable' \
		go test ./... -count=1 -p 1

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
