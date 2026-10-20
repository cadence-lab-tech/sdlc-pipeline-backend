# Команды разработки. Всё, что касается контейнеров, живёт в dev/.

BIN     := bin
VERSION ?= dev

.PHONY: build run test test-integration lint tidy up down logs psql reset image task-up task-down task-ls

# Четыре бинарника: сервис, фоновые задания, CLI для CI и раннер саб-агентов.
# Раннер живёт не в образе, а там, где есть CLI агента и код (решение 0025).
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/api ./cmd/api
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/worker ./cmd/worker
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/clab ./cmd/clab
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN)/clab-runner ./cmd/clab-runner

test:
	go test ./...

# Интеграционные проверки идут по настоящей схеме: инкрементальность прогона
# индексации и гибридный поиск живут в связке с базой и на моках не ловятся.
# База выделенная — тест сносит в ней схему целиком.
#
# Какую базу брать: заданный SDLC_TEST_DATABASE_URL как есть (так работает
# верстак стека — внутри него нет Docker, а адрес базы стека ему выдан при
# старте); иначе решает dev/stack.sh test-db: стек задачи KEY, иначе стек,
# поднятый из этой рабочей копии, иначе (пустой ответ) — общий стек make up.
# Стек есть, но стоит, — ошибка, а не тихий уход в общий: там чужие тесты.
#
# -p 1: база одна на все пакеты, а каждый начинает с чистой схемы —
# параллельные пакеты снесли бы её друг у друга посреди прогона.
test-integration:
	@url="$${SDLC_TEST_DATABASE_URL:-}"; \
	if [ -z "$$url" ]; then url=$$(dev/stack.sh test-db $(KEY)) || exit 1; fi; \
	if [ -z "$$url" ]; then \
		docker compose -f dev/docker-compose.yml exec -T postgres \
			psql -U sdlc -d postgres -c 'CREATE DATABASE sdlc_test' >/dev/null 2>&1 || true; \
		url='postgres://sdlc:sdlc@localhost:55432/sdlc_test?sslmode=disable'; \
		echo 'общий стек (make up)'; \
	fi; \
	echo "интеграционные тесты идут в $$url"; \
	SDLC_TEST_DATABASE_URL="$$url" go test ./... -count=1 -p 1

# gofmt -l печатает неформатированные файлы, но выходит с 0; код ненулевой
# у него только при ошибке разбора. Поэтому вывод проверяется явно, а
# `|| exit 1` не даёт подстановке проглотить код самого gofmt.
lint:
	go vet ./...
	@echo 'gofmt -l cmd internal migrations'; \
	out=$$(gofmt -l cmd internal migrations) || exit 1; \
	if [ -n "$$out" ]; then \
		echo 'неформатированные файлы, исправить: gofmt -w <файл>' >&2; \
		echo "$$out" >&2; \
		exit 1; \
	fi

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

# Стек на задачу: своя база, свои порты, свои данные. Нужен, когда параллельно
# идут несколько задач — общий стек они делят вместе с тестовыми данными.
# Подробности и остальные команды — dev/stack.sh и dev/README.md.
task-up:
	dev/stack.sh up $(KEY)

task-down:
	dev/stack.sh down $(KEY)

task-ls:
	dev/stack.sh ls

psql:
	docker compose -f dev/docker-compose.yml exec postgres psql -U sdlc -d sdlc

image:
	docker build -t sdlc-pipeline-backend:$(VERSION) --build-arg VERSION=$(VERSION) .
