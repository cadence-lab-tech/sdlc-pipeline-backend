# Контракт агента: MCP-тулы

Локальный агент подключается к MCP-серверу бэкенда (streamable HTTP), привязанному
к проекту. Инструкция роли и контекст этапа приезжают в ответе на `claim` —
в локальном репозитории ничего синхронизировать не нужно.

Подключение в Claude Code:

```bash
claude mcp add --transport http sdlc https://sdlc.internal/mcp/projects/SDLC
```

Те же операции доступны через CLI `sdlc` для CI и не-MCP клиентов.

## Обзор

| Тул | Назначение |
|---|---|
| `sdlc_whoami` | Кто я, к какому проекту подключён, какие роли доступны |
| `sdlc_list_tasks` | Задачи проекта с фильтром по статусу и доступности для захвата |
| `sdlc_claim_task` | Забрать задачу: получить инструкцию роли, контекст и аренду |
| `sdlc_get_task` | Полное состояние задачи с артефактами и таймлайном |
| `sdlc_get_artifact` | Содержимое конкретного артефакта |
| `sdlc_search_context` | Семантический поиск по артефактам проекта и коду |
| `sdlc_submit_artifact` | Сдать результат работы вместе с зоной в коде |
| `sdlc_link_repo_ref` | Привязать ветку, PR или коммит к задаче |
| `sdlc_get_drift` | Открытые расхождения с main и пересечения с другими задачами |
| `sdlc_resolve_drift` | Закрыть сигнал разбором или отклонить его |
| `sdlc_check_gates` | Что мешает переходу прямо сейчас |
| `sdlc_request_transition` | Запросить переход вперёд или откат назад |
| `sdlc_heartbeat` | Продлить аренду |
| `sdlc_release` | Отпустить задачу, не завершив этап |

## Основные вызовы

### `sdlc_claim_task`

```jsonc
// запрос
{"task_key": "SDLC-42", "session_ref": "macbook/claude-code/8f21"}

// ответ
{
  "stage_run_id": "…",
  "task": {"key": "SDLC-42", "title": "…", "description": "…",
           "status": "analysis", "attempt": 2,
           "external_ref": {"jira": {"key": "ABC-1", "url": "…"}}},
  "role": {
    "key": "analyst", "name": "Аналитик", "version": 1,
    "instructions": "Ты аналитик. Разбери задачу до уровня…",
    "output_contract": [
      {"kind": "analysis.brief", "title": "Разбор требований",
       "format": "markdown", "required": true},
      {"kind": "analysis.acceptance_criteria", "title": "Критерии приёмки",
       "format": "markdown", "required": true}
    ],
    "tool_policy": {"allow": ["Read", "Grep"], "deny": []}
  },
  "context": {
    "repositories": [{"owner": "acme", "name": "api", "default_branch": "main",
                      "role_hint": "backend", "clone_url": "…"}],
    "project_documents": [{"id": "…", "kind": "api_registry", "title": "API Registry"}],
    "prior_artifacts": [{"id": "…", "kind": "arch.adr", "title": "…",
                         "stage": "architecture", "status": "approved"}],
    // заполнено, если этап начался из-за отката
    "rejection": {"from_stage": "architecture", "reason": "…",
                  "problem_report_artifact_id": "…"},
    // коммит, на котором построены текущие артефакты задачи
    "baselines": [{"repository": "acme/api", "branch": "main", "commit": "aaa…"}],
    // main уехал или соседняя задача метит в те же файлы
    "drift": [
      {"id": "…", "kind": "baseline_drift", "severity": "high",
       "from_commit": "aaa…", "to_commit": "bbb…",
       "paths": ["internal/auth/token.go"],
       "symbols": ["internal/auth.Rotate"]},
      {"id": "…", "kind": "task_overlap", "severity": "medium",
       "related_task": "SDLC-2", "paths": ["internal/http/router.go"]}
    ]
  },
  "gates_ahead": [
    {"transition": "analysis_done", "gates": [
      {"key": "artifacts", "type": "artifacts_required",
       "config": {"kinds": ["analysis.brief", "analysis.acceptance_criteria"]}},
      {"key": "peer_review", "type": "agent_review", "config": {"reviewer_role_key": "reviewer"}},
      {"key": "human_approval", "type": "human_approval", "config": {"min_approvals": 1}}
    ]}
  ],
  "lease": {"expires_at": "2026-08-27T12:30:00Z", "heartbeat_interval_seconds": 300}
}
```

Захват атомарен: `pending → claimed` под `SELECT … FOR UPDATE`. Второй агент на
занятой задаче получает ошибку с указанием, кто держит аренду и до какого момента.

`gates_ahead` отдаётся сразу, чтобы агент знал требования до начала работы, а не
узнавал о них на попытке перехода.

### `sdlc_search_context`

```jsonc
{"query": "как сейчас устроена ротация токенов",
 "scope": ["artifacts", "code", "documents"],
 "repositories": ["acme/api"], "top_k": 12}
```

Возвращает куски с источником: артефакт какой задачи и этапа, либо файл и
диапазон строк на конкретном коммите. Гибридно — вектор + полнотекстовый поиск.

### `sdlc_submit_artifact`

```jsonc
{"stage_run_id": "…", "kind": "analysis.brief",
 "title": "Разбор требований", "format": "markdown",
 "content": "## Требования\n…",
 "links": [{"rel": "references_file", "target_uri": "repo://acme/api@abc123/internal/auth/token.go#L40-L88"}],
 // зона задачи в коде — вход детектора дрейфа
 "code_refs": [
   {"repository": "acme/api", "path": "internal/auth/token.go",
    "symbol_fqn": "internal/auth.Rotate", "ref_kind": "depends_on", "commit": "abc123"},
   {"repository": "acme/api", "path": "internal/http/router.go",
    "ref_kind": "modifies", "commit": "abc123"}
 ]}
```

Повторная сдача того же `kind` создаёт новую версию с `supersedes_id`, предыдущая
переходит в `superseded`. Ответ содержит `sha256` и `size_bytes`; если контент
совпал с уже существующим блобом, он не дублируется.

`code_refs` — не украшение: `modifies` объявляет, что задача собирается менять
этот файл, и именно по нему ловятся пересечения с задачами, идущими параллельно.
`depends_on` даёт самый точный сигнал дрейфа — изменение указанного символа в main
поднимает расхождение до `high`. Роль, у которой в `output_contract` есть
`arch.impact_map` или `dev.plan`, обязана заполнять `code_refs`.

### `sdlc_link_repo_ref`

Сам код изменений живёт в PR, а не в артефакте. Гейт `repo_ref_required` на
переходе из разработки проверяет, что PR привязан и открыт.

```jsonc
{"task_key": "SDLC-42", "repository": "acme/api",
 "ref_type": "pull_request", "ref": "128",
 "url": "https://github.com/acme/api/pull/128", "state": "open"}
```

### `sdlc_get_drift`

```jsonc
// запрос
{"task_key": "SDLC-42", "state": ["open", "acknowledged"]}

// ответ
{"baselines": [{"repository": "acme/api", "branch": "main", "commit": "aaa…",
                "head_commit": "bbb…", "behind_by": 14}],
 "signals": [
   {"id": "…", "kind": "baseline_drift", "severity": "high",
    "paths": ["internal/auth/token.go"], "symbols": ["internal/auth.Rotate"],
    "artifact_ids": ["…"], "detected_at": "2026-08-27T09:12:00Z"}
 ]}
```

`behind_by` — насколько базовая линия задачи отстала от головы отслеживаемой
ветки. Ненулевое значение без сигналов означает, что main двигался, но не
задевал зону задачи.

### `sdlc_resolve_drift`

```jsonc
// разобрано: выводы этапа остаются в силе
{"signal_id": "…", "resolution": "resolved",
 "note": "Rotate сменил сигнатуру, но задача использует его через интерфейс TokenSource",
 "artifact": {"kind": "drift.assessment", "format": "markdown", "content": "…"}}

// сигнал нерелевантен
{"signal_id": "…", "resolution": "dismissed", "note": "Затронут только тестовый хелпер"}
```

При `resolved` базовая линия задачи переставляется на коммит, относительно
которого дрейф разбирался. Если разбор показал, что выводы этапа сломались,
резолвить сигнал не нужно — задача уходит назад обычным `reject`-переходом,
и сигнал закроется вместе с новым проходом этапа.

### `sdlc_check_gates`

```jsonc
// ответ
{"transition": "analysis_done", "can_transition": false,
 "gates": [
   {"key": "artifacts", "status": "passed"},
   {"key": "peer_review", "status": "pending",
    "detail": "ревью не запускалось"},
   {"key": "human_approval", "status": "pending",
    "detail": "ожидает апрува в UI"}
 ],
 "missing": ["analysis.open_questions не сдан (необязательный)"]}
```

Побочных эффектов нет — это чистая проверка. Позволяет агенту доработать
результат до запроса перехода.

### `sdlc_request_transition`

```jsonc
// вперёд
{"stage_run_id": "…", "transition": "analysis_done", "summary": "Требования разобраны…"}

// назад: гейт обязывает приложить problem.report
{"stage_run_id": "…", "transition": "reject_to_analysis",
 "reason": "Критерий 3 противоречит текущей модели прав",
 "artifacts": [{"kind": "problem.report", "format": "markdown", "content": "…"}]}
```

Движок прогоняет гейты перехода. Если все блокирующие пройдены — задача
переходит, `stage_run` закрывается с `outcome`, на новом статусе создаётся
`stage_run` в `pending`. Если нет — возвращается тот же ответ, что у
`sdlc_check_gates`, и этап остаётся в `gating`.

Асинхронные гейты (`human_approval`, `external_check`, `agent_review`) переводят
этап в `gating` и не блокируют агента: он отпускает задачу, а переход
довыполняется, когда результат гейта придёт.

## Ошибки

| Код | Когда |
|---|---|
| `task_not_claimable` | Задача не в `pending`, либо статус без роли |
| `lease_held_by_other` | Задачу держит другой агент; в деталях — кто и до какого момента |
| `lease_expired` | Аренда протухла, задача возвращена в `pending`; нужен повторный `claim` |
| `contract_violation` | `kind` артефакта не входит в `output_contract` роли |
| `transition_not_allowed` | Перехода нет в цикле из текущего статуса |
| `gates_not_satisfied` | Блокирующие гейты не пройдены; в деталях — какие |
| `drift_unresolved` | Есть открытые расхождения выше порога гейта |
| `unknown_repository` | Репозиторий не привязан к проекту |
