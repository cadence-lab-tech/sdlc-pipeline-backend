# SDLC Pipeline — Backend

Go-сервис, ведущий задачи по настраиваемому циклу разработки: REST API для UI,
MCP-сервер для локальных агентов, воркеры индексации репозиториев и детектора
дрейфа.

- [Архитектура](docs/ARCHITECTURE.md) — модель домена, принятые решения, схема БД, план по фазам
- [Контракт агента](docs/MCP_CONTRACT.md) — MCP-тулы для локального агента
- [Детектор дрейфа](docs/drift_detection.sql) — референсные запросы

Фронтенд живёт отдельно: [sdlc-pipeline-frontend](https://github.com/stepaKaziatko/sdlc-pipeline-frontend).

## Стек

Go 1.23, Gin, pgx, PostgreSQL 17 + pgvector, эмбеддинги Voyage (`voyage-code-3`, 1024).

## Локальный запуск базы

```bash
docker compose up -d
for f in migrations/*.up.sql; do
  docker exec -i sdlc-postgres psql -v ON_ERROR_STOP=1 -U sdlc -d sdlc < "$f"
done
docker exec -i sdlc-postgres psql -U sdlc -d sdlc < migrations/seed/default_workflow.sql
```

Postgres поднимается на `localhost:55432`, база `sdlc`, пользователь/пароль `sdlc`.
Сид разворачивает демонстрационный проект: полный цикл из семи статусов, пять
ролей с инструкциями и контрактами, гейты на каждом переходе и эпик с двумя
связанными задачами.

Дополнительно `migrations/seed/drift_demo.sql` разворачивает сценарий для
проверки детектора дрейфа: две задачи в полёте, движение main и ожидаемые сигналы.

## Статус

Фаза 0: зафиксированы модель домена, схема БД и контракт агента. Кода ещё нет.
