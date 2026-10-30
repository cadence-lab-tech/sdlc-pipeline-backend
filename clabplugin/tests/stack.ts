// Снимок ответов стека задачи CLAB-324 (backend/dev/stack.sh): проекты NOVA
// (форма нашего цикла) и FORGE (вымышленный цикл). Только поля, которые
// читает карточка; пересобирается скриптом из разбора изменений.

export const CAPTURED_AT = "2026-10-30T11:15:25Z"

export const TASKS: Record<string, any> = {
 "NOVA-1": {
  "task": {
   "key": "NOVA-1",
   "title": "Импорт и экспорт данных: форматы, ограничения и история операций",
   "project_key": "NOVA",
   "status_key": "in_progress",
   "status_name": "In progress",
   "workflow_id": "0f03864f-c6e0-494c-b0c1-bd0f3eaca55f",
   "priority": "high",
   "properties": {
    "type": "epic"
   },
   "optional_stages": {}
  },
  "stage_runs": [
   {
    "id": "bb615636-55dc-43fe-bc69-e1504c076c9c",
    "status_key": "new",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:21.655645Z",
    "exit": {
     "kind": "advanced",
     "transition": "start",
     "to_status": "planning",
     "comment": null,
     "at": "2026-10-30T11:12:21.665478Z"
    }
   },
   {
    "id": "227fbfa5-c0f7-4e45-ba8c-c5153cb1850d",
    "status_key": "planning",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:21.677484Z",
    "exit": {
     "kind": "advanced",
     "transition": "planned",
     "to_status": "analysis",
     "comment": null,
     "at": "2026-10-30T11:12:21.679708Z"
    }
   },
   {
    "id": "fd643ed9-6fbb-4b67-8a23-15f7a0723b47",
    "status_key": "analysis",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:21.703826Z",
    "exit": {
     "kind": "advanced",
     "transition": "breakdown_done",
     "to_status": "in_progress",
     "comment": null,
     "at": "2026-10-30T11:12:21.708436Z"
    }
   },
   {
    "id": "e9c53435-61f4-4d4b-8464-e6d1f3102521",
    "status_key": "in_progress",
    "kind": "stage",
    "attempt": 1,
    "exit": null
   }
  ],
  "transitions": [
   {
    "key": "children_done",
    "name": "Дети закрыты",
    "kind": "advance",
    "to_status": "done",
    "gates": [
     {
      "gate_key": "children",
      "gate_name": "Дети закрыты",
      "type": "dependencies_closed",
      "status": "failed",
      "blocking": true,
      "reason": "не закрыты задачи: NOVA-4, NOVA-5, NOVA-6, NOVA-7, NOVA-8"
     }
    ]
   },
   {
    "key": "replan",
    "name": "Перепланировать",
    "kind": "reject",
    "to_status": "analysis",
    "gates": []
   },
   {
    "key": "cancel",
    "name": "Отменить",
    "kind": "cancel",
    "to_status": "cancelled",
    "gates": []
   }
  ],
  "artifacts": [],
  "attachments": [],
  "children": [
   {
    "key": "NOVA-2",
    "title": "Решение: формат журнала операций",
    "status_key": "done",
    "status_name": "Done",
    "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
    "properties": {
     "type": "task"
    },
    "status_category": "done",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "NOVA-3",
    "title": "Ограничения размера файла",
    "status_key": "done",
    "status_name": "Done",
    "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
    "properties": {
     "type": "task"
    },
    "status_category": "done",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "NOVA-4",
    "title": "Выгрузка отчёта в CSV: выбор столбцов, кодировки и разделителя для больших таблиц",
    "status_key": "development",
    "status_name": "Development",
    "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
    "properties": {
     "type": "task"
    },
    "status_category": "active",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "NOVA-5",
    "title": "История импортов в интерфейсе",
    "status_key": "testing",
    "status_name": "Testing",
    "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
    "properties": {
     "type": "task"
    },
    "status_category": "active",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "NOVA-6",
    "title": "Кодировки и разделитель",
    "status_key": "development",
    "status_name": "Development",
    "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
    "properties": {
     "type": "task"
    },
    "status_category": "active",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "NOVA-7",
    "title": "Повтор неудачного импорта",
    "status_key": "new",
    "status_name": "New",
    "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
    "properties": {
     "type": "task"
    },
    "status_category": "backlog",
    "awaiting_approval": false,
    "blocked_by": [
     "NOVA-4"
    ]
   },
   {
    "key": "NOVA-8",
    "title": "Экспорт в XLSX",
    "status_key": "new",
    "status_name": "New",
    "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
    "properties": {
     "type": "task"
    },
    "status_category": "backlog",
    "awaiting_approval": false,
    "blocked_by": [
     "NOVA-4"
    ]
   },
   {
    "key": "NOVA-9",
    "title": "Устаревший экран импорта",
    "status_key": "cancelled",
    "status_name": "Cancelled",
    "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
    "properties": {
     "type": "task"
    },
    "status_category": "cancelled",
    "awaiting_approval": false,
    "blocked_by": []
   }
  ],
  "usage": {
   "cost_usd": 0
  },
  "repo_refs": [],
  "links": []
 },
 "NOVA-4": {
  "task": {
   "key": "NOVA-4",
   "title": "Выгрузка отчёта в CSV: выбор столбцов, кодировки и разделителя для больших таблиц",
   "project_key": "NOVA",
   "status_key": "development",
   "status_name": "Development",
   "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
   "priority": "high",
   "properties": {
    "type": "task"
   },
   "optional_stages": {},
   "parent_key": "NOVA-1"
  },
  "stage_runs": [
   {
    "id": "3153ef3f-fc8f-4428-a341-73549292532f",
    "status_key": "new",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.276887Z",
    "exit": {
     "kind": "advanced",
     "transition": "start",
     "to_status": "analysis",
     "comment": null,
     "at": "2026-10-30T11:12:22.27902Z"
    }
   },
   {
    "id": "a01b4da9-52c1-4803-ac8d-39bd4ddf0468",
    "status_key": "analysis",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.317271Z",
    "exit": {
     "kind": "advanced",
     "transition": "analysis_done",
     "to_status": "development",
     "comment": null,
     "at": "2026-10-30T11:12:22.328221Z"
    }
   },
   {
    "id": "8930b5c2-0f1e-45d0-998e-fb32954b1dbf",
    "status_key": "development",
    "kind": "stage",
    "attempt": 1,
    "claimed_by_handle": "dev-agent",
    "finished_at": "2026-10-30T11:12:22.5109Z",
    "exit": {
     "kind": "advanced",
     "transition": "dev_done",
     "to_status": "testing",
     "comment": null,
     "at": "2026-10-30T11:12:22.514416Z"
    }
   },
   {
    "id": "fb66f002-62a2-466b-b3cc-57d9dbc02023",
    "status_key": "testing",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.527227Z",
    "exit": {
     "kind": "rejected",
     "transition": "testing_failed",
     "to_status": "development",
     "comment": "нет теста на повторную выгрузку",
     "at": "2026-10-30T11:12:22.536997Z"
    }
   },
   {
    "id": "793deb2a-7623-4cb6-b34c-7ecdf5a8cce3",
    "status_key": "development",
    "kind": "stage",
    "attempt": 2,
    "claimed_by_handle": "dev-agent",
    "lease_expires_at": "2026-10-30T11:42:22.548363Z",
    "rejected_from_run_id": "fb66f002-62a2-466b-b3cc-57d9dbc02023",
    "exit": null
   }
  ],
  "transitions": [
   {
    "key": "dev_done",
    "name": "Разработка готова",
    "kind": "advance",
    "to_status": "testing",
    "gates": [
     {
      "gate_key": "artifacts",
      "gate_name": "Артефакты сданы",
      "type": "artifacts_required",
      "status": "passed",
      "blocking": true
     },
     {
      "gate_key": "pull_request",
      "gate_name": "PR привязан",
      "type": "repo_ref_required",
      "status": "failed",
      "blocking": true,
      "reason": "у задачи не объявлена область репозиториев"
     },
     {
      "gate_key": "selfhosted",
      "gate_name": "Миграции проверены",
      "type": "checklist",
      "status": "pending",
      "blocking": true,
      "reason": "не отмечено пунктов чек-листа: 1"
     }
    ]
   },
   {
    "key": "reject_to_analysis",
    "name": "Вернуть аналитику",
    "kind": "reject",
    "to_status": "analysis",
    "gates": []
   },
   {
    "key": "cancel",
    "name": "Отменить",
    "kind": "cancel",
    "to_status": "cancelled",
    "gates": []
   }
  ],
  "artifacts": [
   {
    "id": "f3d3a8e5-6903-457d-8c18-53fae4b2c677",
    "kind": "dev.change_summary",
    "title": "Разбор изменений",
    "version": 2,
    "stage_run_id": "793deb2a-7623-4cb6-b34c-7ecdf5a8cce3",
    "created_at": "2026-10-30T11:12:22.554965Z"
   }
  ],
  "attachments": [],
  "children": [],
  "usage": {
   "cost_usd": 4.1
  },
  "repo_refs": [],
  "links": [
   {
    "rel": "blocks",
    "direction": "outgoing",
    "task_id": "be56e395-cd74-4fad-8489-fc56eb37f706",
    "task_key": "NOVA-7",
    "task_title": "Повтор неудачного импорта",
    "status_key": "new"
   },
   {
    "rel": "blocks",
    "direction": "outgoing",
    "task_id": "e3b15818-f5d4-4b5d-8fa0-5ba90eeca56a",
    "task_key": "NOVA-8",
    "task_title": "Экспорт в XLSX",
    "status_key": "new"
   }
  ]
 },
 "NOVA-10": {
  "task": {
   "key": "NOVA-10",
   "title": "Счётчик повторных попыток в карточке заказа",
   "project_key": "NOVA",
   "status_key": "merge",
   "status_name": "Merge",
   "workflow_id": "28134227-c0db-4fad-94a4-25fd49362549",
   "priority": "normal",
   "properties": {
    "type": "task"
   },
   "optional_stages": {}
  },
  "stage_runs": [
   {
    "id": "b4d3fe43-aaf9-493d-af9c-b7e19c433e52",
    "status_key": "new",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.689012Z",
    "exit": {
     "kind": "advanced",
     "transition": "start",
     "to_status": "analysis",
     "comment": null,
     "at": "2026-10-30T11:12:22.691089Z"
    }
   },
   {
    "id": "21ff16db-d96e-4962-ac73-9f67d7bbfae9",
    "status_key": "analysis",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.710115Z",
    "exit": {
     "kind": "advanced",
     "transition": "analysis_done",
     "to_status": "development",
     "comment": null,
     "at": "2026-10-30T11:12:22.712438Z"
    }
   },
   {
    "id": "8b259188-3e40-4e0e-8e5a-1e702995f675",
    "status_key": "development",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.865584Z",
    "exit": {
     "kind": "advanced",
     "transition": "dev_done",
     "to_status": "testing",
     "comment": null,
     "at": "2026-10-30T11:12:22.868444Z"
    }
   },
   {
    "id": "dc07d1b1-86a6-4005-9523-e0e39efda4af",
    "status_key": "testing",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.882009Z",
    "exit": {
     "kind": "advanced",
     "transition": "testing_passed",
     "to_status": "merge",
     "comment": null,
     "at": "2026-10-30T11:12:22.885359Z"
    }
   },
   {
    "id": "34516650-63ac-43b3-857a-ae78d6ed589c",
    "status_key": "merge",
    "kind": "stage",
    "attempt": 1,
    "exit": null
   }
  ],
  "transitions": [
   {
    "key": "merge_ready",
    "name": "Готово к слиянию",
    "kind": "advance",
    "to_status": "release",
    "gates": [
     {
      "gate_key": "approval",
      "gate_name": "Разрешение слить",
      "type": "human_approval",
      "status": "pending",
      "blocking": true,
      "reason": "апрувов 0 из 1"
     }
    ]
   },
   {
    "key": "merge_conflict",
    "name": "Конфликт слияния",
    "kind": "reject",
    "to_status": "development",
    "gates": []
   },
   {
    "key": "cancel",
    "name": "Отменить",
    "kind": "cancel",
    "to_status": "cancelled",
    "gates": []
   }
  ],
  "artifacts": [],
  "attachments": [],
  "children": [],
  "usage": {
   "cost_usd": 0
  },
  "repo_refs": [],
  "links": []
 },
 "NOVA-11": {
  "task": {
   "key": "NOVA-11",
   "title": "v1.4.0",
   "project_key": "NOVA",
   "status_key": "prepare",
   "status_name": "Prepare",
   "workflow_id": "f595b585-95bf-4436-802a-2884aee4231b",
   "priority": "high",
   "properties": {
    "type": "release"
   },
   "optional_stages": {}
  },
  "stage_runs": [
   {
    "id": "c2b60bb4-2bc9-45f6-912a-4e619081bb26",
    "status_key": "new",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.916067Z",
    "exit": {
     "kind": "advanced",
     "transition": "start",
     "to_status": "prepare",
     "comment": null,
     "at": "2026-10-30T11:12:22.917339Z"
    }
   },
   {
    "id": "fe23dc0d-79ab-4653-bfbe-aed35e76ebdd",
    "status_key": "prepare",
    "kind": "stage",
    "attempt": 1,
    "exit": null
   }
  ],
  "transitions": [
   {
    "key": "prepared",
    "name": "Подготовлено",
    "kind": "advance",
    "to_status": "publish",
    "gates": [
     {
      "gate_key": "approval",
      "gate_name": "Разрешение ставить тег",
      "type": "human_approval",
      "status": "pending",
      "blocking": true,
      "reason": "апрувов 0 из 1"
     }
    ]
   },
   {
    "key": "cancel",
    "name": "Отменить",
    "kind": "cancel",
    "to_status": "cancelled",
    "gates": []
   }
  ],
  "artifacts": [],
  "attachments": [],
  "children": [],
  "usage": {
   "cost_usd": 0
  },
  "repo_refs": [],
  "links": []
 },
 "FORGE-1": {
  "task": {
   "key": "FORGE-1",
   "title": "Новая панель администратора",
   "project_key": "FORGE",
   "status_key": "delivery",
   "status_name": "Delivery",
   "workflow_id": "781a7b5c-6af6-46b7-931e-8881b5d185cb",
   "priority": "high",
   "properties": {
    "type": "initiative"
   },
   "optional_stages": {}
  },
  "stage_runs": [
   {
    "id": "016901bd-52ab-4511-b15e-d4db44aac6ad",
    "status_key": "proposed",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.946616Z",
    "exit": {
     "kind": "advanced",
     "transition": "kickoff",
     "to_status": "scoping",
     "comment": null,
     "at": "2026-10-30T11:12:22.948696Z"
    }
   },
   {
    "id": "c30203dd-61e7-43d0-8108-43e930b58e62",
    "status_key": "scoping",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:22.977171Z",
    "exit": {
     "kind": "advanced",
     "transition": "scoped",
     "to_status": "delivery",
     "comment": null,
     "at": "2026-10-30T11:12:22.982841Z"
    }
   },
   {
    "id": "9928226d-9ee6-4a5a-b9b7-842f7ab68308",
    "status_key": "delivery",
    "kind": "stage",
    "attempt": 1,
    "exit": null
   }
  ],
  "transitions": [
   {
    "key": "delivered",
    "name": "Delivered",
    "kind": "advance",
    "to_status": "closed",
    "gates": [
     {
      "gate_key": "children",
      "gate_name": "All work closed",
      "type": "dependencies_closed",
      "status": "failed",
      "blocking": true,
      "reason": "не закрыты задачи: FORGE-2, FORGE-3, FORGE-4, FORGE-8"
     }
    ]
   },
   {
    "key": "shelve",
    "name": "Shelve",
    "kind": "cancel",
    "to_status": "shelved",
    "gates": []
   }
  ],
  "artifacts": [],
  "attachments": [],
  "children": [
   {
    "key": "FORGE-2",
    "title": "Платёжный шлюз отвечает 502 на повторную авторизацию карты при частичном отказе провайдера в одном регионе",
    "status_key": "deploy_window",
    "status_name": "Deploy window",
    "workflow_id": "4486cbfd-6695-45b6-928c-5074fae7e743",
    "properties": {
     "type": "incident"
    },
    "status_category": "active",
    "awaiting_approval": true,
    "blocked_by": []
   },
   {
    "key": "FORGE-3",
    "title": "Онбординг: чек-лист первого запуска для администраторов и приглашённых гостей",
    "status_key": "design",
    "status_name": "Design",
    "workflow_id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
    "properties": {
     "type": "story"
    },
    "status_category": "active",
    "awaiting_approval": true,
    "blocked_by": []
   },
   {
    "key": "FORGE-4",
    "title": "Токены темы",
    "status_key": "verify",
    "status_name": "Verify",
    "workflow_id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
    "properties": {
     "type": "story"
    },
    "status_category": "active",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "FORGE-5",
    "title": "Исследование: что админы делают чаще всего",
    "status_key": "closed",
    "status_name": "Closed",
    "workflow_id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
    "properties": {
     "type": "story"
    },
    "status_category": "done",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "FORGE-6",
    "title": "Роли и права: матрица доступа",
    "status_key": "closed",
    "status_name": "Closed",
    "workflow_id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
    "properties": {
     "type": "story"
    },
    "status_category": "done",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "FORGE-7",
    "title": "Устаревший экран импорта",
    "status_key": "dropped",
    "status_name": "Dropped",
    "workflow_id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
    "properties": {
     "type": "story"
    },
    "status_category": "cancelled",
    "awaiting_approval": false,
    "blocked_by": []
   },
   {
    "key": "FORGE-8",
    "title": "Журнал действий администратора",
    "status_key": "backlog",
    "status_name": "Backlog",
    "workflow_id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
    "properties": {
     "type": "story"
    },
    "status_category": "backlog",
    "awaiting_approval": false,
    "blocked_by": [
     "FORGE-6"
    ]
   }
  ],
  "usage": {
   "cost_usd": 0
  },
  "repo_refs": [],
  "links": []
 },
 "FORGE-2": {
  "task": {
   "key": "FORGE-2",
   "title": "Платёжный шлюз отвечает 502 на повторную авторизацию карты при частичном отказе провайдера в одном регионе",
   "project_key": "FORGE",
   "status_key": "deploy_window",
   "status_name": "Deploy window",
   "workflow_id": "4486cbfd-6695-45b6-928c-5074fae7e743",
   "priority": "urgent",
   "properties": {
    "type": "incident"
   },
   "optional_stages": {},
   "parent_key": "FORGE-1"
  },
  "stage_runs": [
   {
    "id": "8f9fb377-35c0-4109-b000-4d07377a14d6",
    "status_key": "reported",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.019809Z",
    "exit": {
     "kind": "advanced",
     "transition": "report",
     "to_status": "triage",
     "comment": null,
     "at": "2026-10-30T11:12:23.021393Z"
    }
   },
   {
    "id": "1f44d01c-8181-4fba-a12e-32a35c4979a3",
    "status_key": "triage",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.034122Z",
    "exit": {
     "kind": "advanced",
     "transition": "triage_done",
     "to_status": "contain",
     "comment": null,
     "at": "2026-10-30T11:12:23.037271Z"
    }
   },
   {
    "id": "2095d7fc-2b0d-4ff9-9372-8f0384aa764b",
    "status_key": "contain",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.054055Z",
    "exit": {
     "kind": "advanced",
     "transition": "contain_done",
     "to_status": "diagnose",
     "comment": null,
     "at": "2026-10-30T11:12:23.057059Z"
    }
   },
   {
    "id": "656afc0d-8843-4624-b37b-49f711cdc79b",
    "status_key": "diagnose",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.072333Z",
    "exit": {
     "kind": "advanced",
     "transition": "diagnose_done",
     "to_status": "fix",
     "comment": null,
     "at": "2026-10-30T11:12:23.074636Z"
    }
   },
   {
    "id": "589bde25-0d4f-4eb9-8b8b-0cb30edfa663",
    "status_key": "fix",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.086233Z",
    "exit": {
     "kind": "advanced",
     "transition": "fix_done",
     "to_status": "peer_review",
     "comment": null,
     "at": "2026-10-30T11:12:23.089749Z"
    }
   },
   {
    "id": "976a208f-802c-4de4-89f4-a48f077c6f0b",
    "status_key": "peer_review",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.110946Z",
    "exit": {
     "kind": "advanced",
     "transition": "peer_review_done",
     "to_status": "deploy_window",
     "comment": null,
     "at": "2026-10-30T11:12:23.111979Z"
    }
   },
   {
    "id": "102c6e13-4e95-4e1d-ad89-060b140b4a71",
    "status_key": "deploy_window",
    "kind": "stage",
    "attempt": 1,
    "claimed_by_handle": "ops-agent",
    "finished_at": "2026-10-30T11:12:23.177257Z",
    "exit": {
     "kind": "advanced",
     "transition": "deploy_window_done",
     "to_status": "monitor",
     "comment": null,
     "at": "2026-10-30T11:12:23.182622Z"
    }
   },
   {
    "id": "96cdf7ca-f83d-4d85-9f0e-f40bcb9e2dc5",
    "status_key": "monitor",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.191875Z",
    "exit": {
     "kind": "rejected",
     "transition": "roll_back",
     "to_status": "deploy_window",
     "comment": "доля ошибок выросла до 4% после выкладки",
     "at": "2026-10-30T11:12:23.193007Z"
    }
   },
   {
    "id": "608e44f9-58aa-441b-970f-3c07a0493870",
    "status_key": "deploy_window",
    "kind": "stage",
    "attempt": 2,
    "claimed_by_handle": "ops-agent",
    "lease_expires_at": "2026-10-30T11:42:23.204605Z",
    "rejected_from_run_id": "96cdf7ca-f83d-4d85-9f0e-f40bcb9e2dc5",
    "exit": null
   }
  ],
  "transitions": [
   {
    "key": "deploy_window_done",
    "name": "Approve production deploy",
    "kind": "advance",
    "to_status": "monitor",
    "gates": [
     {
      "gate_key": "fix_verified",
      "gate_name": "Fix verified",
      "type": "agent_review",
      "status": "waived",
      "blocking": true,
      "reason": "снят вручную"
     },
     {
      "gate_key": "rollback",
      "gate_name": "Rollback plan attached",
      "type": "artifacts_required",
      "status": "passed",
      "blocking": true
     },
     {
      "gate_key": "linked",
      "gate_name": "Linked tickets closed",
      "type": "dependencies_closed",
      "status": "passed",
      "blocking": true
     },
     {
      "gate_key": "signoff",
      "gate_name": "Go-live sign-off",
      "type": "human_approval",
      "status": "pending",
      "blocking": true,
      "reason": "апрувов 0 из 1"
     }
    ]
   },
   {
    "key": "withdraw",
    "name": "Withdraw",
    "kind": "cancel",
    "to_status": "withdrawn",
    "gates": []
   }
  ],
  "artifacts": [
   {
    "id": "aaf27f2c-74f0-442b-94be-fd3044f7548e",
    "kind": "incident.rollback_plan",
    "title": "Rollback plan",
    "version": 2,
    "stage_run_id": "608e44f9-58aa-441b-970f-3c07a0493870",
    "created_at": "2026-10-30T11:12:23.211701Z"
   }
  ],
  "attachments": [
   {
    "id": "9b4b917e-c936-4cc4-9d27-e31de503aef1",
    "title": "gateway-502-rate.gif",
    "file_name": "gateway-502-rate.gif",
    "size_bytes": 14,
    "added_at": "2026-10-30T11:15:10.576003Z"
   }
  ],
  "children": [],
  "usage": {
   "cost_usd": 1.2
  },
  "repo_refs": [],
  "links": []
 },
 "FORGE-3": {
  "task": {
   "key": "FORGE-3",
   "title": "Онбординг: чек-лист первого запуска для администраторов и приглашённых гостей",
   "project_key": "FORGE",
   "status_key": "design",
   "status_name": "Design",
   "workflow_id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
   "priority": "normal",
   "properties": {
    "type": "story"
   },
   "optional_stages": {
    "design": true
   },
   "parent_key": "FORGE-1"
  },
  "stage_runs": [
   {
    "id": "3c7c79e7-2e7e-4bc5-9951-478ffe351a19",
    "status_key": "backlog",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.25501Z",
    "exit": {
     "kind": "advanced",
     "transition": "pick",
     "to_status": "intake",
     "comment": null,
     "at": "2026-10-30T11:12:23.259089Z"
    }
   },
   {
    "id": "e350af63-78a9-4914-a822-0a7b8b36b6eb",
    "status_key": "intake",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.279002Z",
    "exit": {
     "kind": "advanced",
     "transition": "intake_done",
     "to_status": "design",
     "comment": null,
     "at": "2026-10-30T11:12:23.283427Z"
    }
   },
   {
    "id": "01f6479f-108e-4ea0-a7d1-660e19510718",
    "status_key": "design",
    "kind": "stage",
    "attempt": 1,
    "exit": null
   }
  ],
  "transitions": [
   {
    "key": "design_done",
    "name": "Design accepted",
    "kind": "advance",
    "to_status": "build",
    "gates": [
     {
      "gate_key": "approval",
      "gate_name": "Design sign-off",
      "type": "human_approval",
      "status": "pending",
      "blocking": true,
      "reason": "апрувов 0 из 1"
     }
    ]
   },
   {
    "key": "drop",
    "name": "Drop",
    "kind": "cancel",
    "to_status": "dropped",
    "gates": []
   }
  ],
  "artifacts": [],
  "attachments": [],
  "children": [],
  "usage": {
   "cost_usd": 0
  },
  "repo_refs": [],
  "links": []
 },
 "FORGE-4": {
  "task": {
   "key": "FORGE-4",
   "title": "Токены темы",
   "project_key": "FORGE",
   "status_key": "verify",
   "status_name": "Verify",
   "workflow_id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
   "priority": "normal",
   "properties": {
    "type": "story"
   },
   "optional_stages": {
    "design": false
   },
   "parent_key": "FORGE-1"
  },
  "stage_runs": [
   {
    "id": "0bf2bab6-8355-44d8-9dc2-26d32e0d1c03",
    "status_key": "backlog",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.316823Z",
    "exit": {
     "kind": "advanced",
     "transition": "pick",
     "to_status": "intake",
     "comment": null,
     "at": "2026-10-30T11:12:23.317986Z"
    }
   },
   {
    "id": "6853c001-07ba-4b4c-b565-4a2e4cce327e",
    "status_key": "intake",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.338319Z",
    "exit": {
     "kind": "advanced",
     "transition": "intake_done",
     "to_status": "build",
     "comment": null,
     "at": "2026-10-30T11:12:23.340772Z"
    }
   },
   {
    "id": "e40be463-d2af-4a7a-b6b2-c413fa83a79e",
    "status_key": "build",
    "kind": "stage",
    "attempt": 1,
    "finished_at": "2026-10-30T11:12:23.422302Z",
    "exit": {
     "kind": "advanced",
     "transition": "build_done",
     "to_status": "verify",
     "comment": null,
     "at": "2026-10-30T11:12:23.42375Z"
    }
   },
   {
    "id": "18c2a24f-fe28-453a-910b-f00496cfaa6d",
    "status_key": "verify",
    "kind": "stage",
    "attempt": 1,
    "claimed_by_handle": "qa-agent",
    "lease_expires_at": "2026-10-30T11:42:23.502298Z",
    "exit": null
   }
  ],
  "transitions": [
   {
    "key": "accept",
    "name": "Accept build",
    "kind": "advance",
    "to_status": "closed",
    "gates": [
     {
      "gate_key": "report",
      "gate_name": "Verification report filed",
      "type": "artifacts_required",
      "status": "failed",
      "blocking": true,
      "reason": "не сданы артефакты: verify.report"
     },
     {
      "gate_key": "approval",
      "gate_name": "Ship sign-off",
      "type": "human_approval",
      "status": "pending",
      "blocking": true,
      "reason": "апрувов 0 из 1"
     }
    ]
   },
   {
    "key": "bounce",
    "name": "Back to build",
    "kind": "reject",
    "to_status": "build",
    "gates": []
   },
   {
    "key": "drop",
    "name": "Drop",
    "kind": "cancel",
    "to_status": "dropped",
    "gates": []
   }
  ],
  "artifacts": [],
  "attachments": [],
  "children": [],
  "usage": {
   "cost_usd": 0
  },
  "repo_refs": [],
  "links": []
 }
}

export const WORKFLOWS: Record<string, any> = {
 "0f03864f-c6e0-494c-b0c1-bd0f3eaca55f": {
  "id": "0f03864f-c6e0-494c-b0c1-bd0f3eaca55f",
  "name": "Epic",
  "version": 1,
  "task_types": [
   "epic"
  ],
  "statuses": [
   {
    "key": "new",
    "name": "New",
    "category": "backlog",
    "position": 1,
    "is_initial": true,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "planning",
    "name": "Planning",
    "category": "active",
    "position": 2,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "orchestrator",
      "role_name": "Оркестратор"
     }
    ]
   },
   {
    "key": "analysis",
    "name": "Analysis",
    "category": "active",
    "position": 3,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "analytics",
      "role_name": "Аналитик"
     }
    ]
   },
   {
    "key": "in_progress",
    "name": "In progress",
    "category": "waiting",
    "position": 4,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "orchestrator",
      "role_name": "Оркестратор"
     }
    ]
   },
   {
    "key": "done",
    "name": "Done",
    "category": "done",
    "position": 5,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "cancelled",
    "name": "Cancelled",
    "category": "cancelled",
    "position": 6,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   }
  ]
 },
 "28134227-c0db-4fad-94a4-25fd49362549": {
  "id": "28134227-c0db-4fad-94a4-25fd49362549",
  "name": "Simple",
  "version": 1,
  "task_types": [
   "task"
  ],
  "statuses": [
   {
    "key": "new",
    "name": "New",
    "category": "backlog",
    "position": 1,
    "is_initial": true,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "analysis",
    "name": "Analysis",
    "category": "active",
    "position": 2,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "analytics",
      "role_name": "Аналитик"
     }
    ]
   },
   {
    "key": "development",
    "name": "Development",
    "category": "active",
    "position": 3,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "developer",
      "role_name": "Разработчик"
     }
    ]
   },
   {
    "key": "testing",
    "name": "Testing",
    "category": "active",
    "position": 4,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "test",
      "role_name": "Тестировщик"
     }
    ]
   },
   {
    "key": "merge",
    "name": "Merge",
    "category": "active",
    "position": 5,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "developer",
      "role_name": "Разработчик"
     }
    ]
   },
   {
    "key": "release",
    "name": "Release",
    "category": "active",
    "position": 6,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "developer",
      "role_name": "Разработчик"
     }
    ]
   },
   {
    "key": "done",
    "name": "Done",
    "category": "done",
    "position": 7,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "cancelled",
    "name": "Cancelled",
    "category": "cancelled",
    "position": 8,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   }
  ]
 },
 "f595b585-95bf-4436-802a-2884aee4231b": {
  "id": "f595b585-95bf-4436-802a-2884aee4231b",
  "name": "Release",
  "version": 1,
  "task_types": [
   "release"
  ],
  "statuses": [
   {
    "key": "new",
    "name": "New",
    "category": "backlog",
    "position": 1,
    "is_initial": true,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "prepare",
    "name": "Prepare",
    "category": "active",
    "position": 2,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "developer",
      "role_name": "Разработчик"
     }
    ]
   },
   {
    "key": "publish",
    "name": "Publish",
    "category": "active",
    "position": 3,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "developer",
      "role_name": "Разработчик"
     }
    ]
   },
   {
    "key": "verify",
    "name": "Verify",
    "category": "active",
    "position": 4,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "test",
      "role_name": "Тестировщик"
     }
    ]
   },
   {
    "key": "done",
    "name": "Done",
    "category": "done",
    "position": 5,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "cancelled",
    "name": "Cancelled",
    "category": "cancelled",
    "position": 6,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   }
  ]
 },
 "781a7b5c-6af6-46b7-931e-8881b5d185cb": {
  "id": "781a7b5c-6af6-46b7-931e-8881b5d185cb",
  "name": "Initiative",
  "version": 1,
  "task_types": [
   "initiative"
  ],
  "statuses": [
   {
    "key": "proposed",
    "name": "Proposed",
    "category": "backlog",
    "position": 1,
    "is_initial": true,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "scoping",
    "name": "Scoping",
    "category": "active",
    "position": 2,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "lead",
      "role_name": "Initiative lead"
     }
    ]
   },
   {
    "key": "delivery",
    "name": "Delivery",
    "category": "waiting",
    "position": 3,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "lead",
      "role_name": "Initiative lead"
     }
    ]
   },
   {
    "key": "closed",
    "name": "Closed",
    "category": "done",
    "position": 4,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "shelved",
    "name": "Shelved",
    "category": "cancelled",
    "position": 5,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   }
  ]
 },
 "4486cbfd-6695-45b6-928c-5074fae7e743": {
  "id": "4486cbfd-6695-45b6-928c-5074fae7e743",
  "name": "Incident",
  "version": 1,
  "task_types": [
   "incident"
  ],
  "statuses": [
   {
    "key": "reported",
    "name": "Reported",
    "category": "backlog",
    "position": 1,
    "is_initial": true,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "triage",
    "name": "Triage",
    "category": "active",
    "position": 2,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "responder",
      "role_name": "Responder"
     }
    ]
   },
   {
    "key": "contain",
    "name": "Contain",
    "category": "active",
    "position": 3,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "responder",
      "role_name": "Responder"
     }
    ]
   },
   {
    "key": "diagnose",
    "name": "Diagnose",
    "category": "active",
    "position": 4,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "responder",
      "role_name": "Responder"
     }
    ]
   },
   {
    "key": "fix",
    "name": "Fix",
    "category": "active",
    "position": 5,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "responder",
      "role_name": "Responder"
     }
    ]
   },
   {
    "key": "peer_review",
    "name": "Peer review",
    "category": "active",
    "position": 6,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "responder",
      "role_name": "Responder"
     }
    ]
   },
   {
    "key": "deploy_window",
    "name": "Deploy window",
    "category": "active",
    "position": 7,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "responder",
      "role_name": "Responder"
     }
    ]
   },
   {
    "key": "monitor",
    "name": "Monitor",
    "category": "active",
    "position": 8,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "responder",
      "role_name": "Responder"
     }
    ]
   },
   {
    "key": "postmortem",
    "name": "Postmortem write-up",
    "category": "active",
    "position": 9,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "responder",
      "role_name": "Responder"
     }
    ]
   },
   {
    "key": "resolved",
    "name": "Resolved",
    "category": "done",
    "position": 10,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "withdrawn",
    "name": "Withdrawn",
    "category": "cancelled",
    "position": 11,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   }
  ]
 },
 "f6a7edd4-a29a-4274-9e17-97c5cae42bd8": {
  "id": "f6a7edd4-a29a-4274-9e17-97c5cae42bd8",
  "name": "Story",
  "version": 1,
  "task_types": [
   "story"
  ],
  "statuses": [
   {
    "key": "backlog",
    "name": "Backlog",
    "category": "backlog",
    "position": 1,
    "is_initial": true,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "intake",
    "name": "Intake",
    "category": "active",
    "position": 2,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "triager",
      "role_name": "Triager"
     }
    ]
   },
   {
    "key": "design",
    "name": "Design",
    "category": "active",
    "position": 3,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": true,
    "enabled_by_default": false,
    "roles": [
     {
      "role_key": "designer",
      "role_name": "Designer"
     }
    ]
   },
   {
    "key": "build",
    "name": "Build",
    "category": "active",
    "position": 4,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "builder",
      "role_name": "Builder"
     }
    ]
   },
   {
    "key": "verify",
    "name": "Verify",
    "category": "active",
    "position": 5,
    "is_initial": false,
    "is_terminal": false,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": [
     {
      "role_key": "verifier",
      "role_name": "Verifier"
     }
    ]
   },
   {
    "key": "closed",
    "name": "Closed",
    "category": "done",
    "position": 6,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   },
   {
    "key": "dropped",
    "name": "Dropped",
    "category": "cancelled",
    "position": 7,
    "is_initial": false,
    "is_terminal": true,
    "is_optional": false,
    "enabled_by_default": true,
    "roles": []
   }
  ]
 }
}
