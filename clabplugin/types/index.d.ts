// Контракт состояния мода clab-plugin в $.state сессии. Токена здесь нет и
// быть не может: он живёт только в памяти модуля (решение 0067, п. 3).

// Состояние узла схемы цикла: пройден, текущий, текущий и ждёт апрува,
// впереди, выключенный необязательный этап (решение 0066).
export type NodeState = 'done' | 'current' | 'waiting' | 'future' | 'skipped'

// Узел схемы — статус версии цикла с ролью или терминальный. name — подпись
// из версии цикла; attempts — прогоны верхнего уровня на этом статусе.
export type CycleNode = { key: string; name: string; state: NodeState; attempts: number }

// Фактический возврат: из какого узла (индекс в nodes, null — узла на схеме
// нет) в какой, подпись статуса-источника, комментарий перехода и время.
export type CardBack = { from: number | null; to: number | null; fromName: string; reason: string | null; at: string | null }

// Вердикт гейта так, как его рисует карточка: ✓ прошёл или снят, ✓ серым —
// не относится, ○ не прошёл, ⏸ ждёт апрува человека, ✕ отклонён.
export type GateMark = 'ok' | 'skip' | 'open' | 'approval' | 'fail'

export type CardGate = { name: string; type: string; status: string; reason: string | null; blocking: boolean; mark: GateMark }

export type CardGates = { transition: string; items: CardGate[]; passed: number; total: number }

// Человеческий гейт перехода вперёд, который ждёт решения:
// плашка в карточке. type — human_approval или checklist (словарь движка);
// state failed — апрув или чек-лист отклонён, человек может передумать.
// reason — причина отказа или пояснение сервера («апрувов 0 из 2»). items —
// пункты чек-листа. since — с какого момента решение ждёт: последняя сдача на
// этапе, без сдач — начало прогона. others — остальные блокирующие гейты
// перехода, уже пройденные (подписи из версии цикла).
export type PlateItem = { id: string; text: string; required: boolean; checked: boolean }
export type Plate = {
  transition: string
  transitionName: string
  toName: string
  gate: string
  gateName: string
  type: string
  state: 'pending' | 'failed'
  reason: string | null
  items: PlateItem[]
  since: string | null
  others: string[]
}

// Артефакт текущего этапа — чип плашки: по одному на вид, последняя версия.
export type StageChip = { kind: string; title: string; version: number | null }

// Переход-возврат из текущего статуса: виды артефактов, которых требуют его
// гейты (их плагин сдаёт с причиной), и гейт, который человеку из плагина не
// закрыть (held, «имя · причина»), если есть.
export type ReturnOption = { transition: string; name: string; toName: string; kinds: string[]; held: string | null }

// Что человек видел: статус задачи и последний артефакт этапа ("" — нет).
export type Seen = { status: string; artifact: string }

// Открытое действие — одно на сессию (R7): подтверждение апрува, форма
// возврата или чек-листа. Токена здесь нет.
export type Action = {
  key: string
  mode: 'approve' | 'return' | 'checklist'
  transition: string
  gate: string
  seen: Seen
  // Форма возврата: выбранный переход и причина.
  target: string | null
  reason: string
  // Чек-лист: отмеченные пункты.
  checked: string[]
}

// Итог последнего действия по задаче — строка под плашкой.
export type Outcome = {
  tone: 'ok' | 'warning' | 'error'
  text: string
  // «журнал» — ссылка на карточку в Cadence Lab; «Посмотреть артефакты».
  journal: boolean
  artifacts: boolean
}

// Строка вкладки «Ждут меня»: элемент серверной выборки.
export type ApprovalRow = {
  key: string
  title: string
  transition: string
  transitionName: string
  toName: string
  gate: string
  gateName: string
  type: string
}

// supported: null — ещё не спрашивали, false — у сервера нет выборки.
// loadedAt — когда выборка прочитана (часы сессии, мс); null — ещё не читали.
export type ApprovalsState = {
  supported: boolean | null
  canDecide: boolean
  rows: ApprovalRow[]
  truncated: boolean
  error: string | null
  loadedAt: number | null
}

// Прогон этапа в строке вкладки: попытка, роль и исполнитель из прогона,
// время начала и конца (ISO), возврат, из которого прогон начат, и
// вложенные прогоны перепроверки под ним.
export type RunRow = {
  id: string
  attempt: number
  role: string | null
  holder: string | null
  from: string | null
  to: string | null
  running: boolean
  back: { fromName: string; reason: string | null } | null
  reviews: RunRow[]
}

// Вкладка этапа: статус версии цикла задачи, на котором были прогоны.
// attempts — прогоны верхнего уровня.
export type StageTab = { key: string; name: string; attempts: number; runs: RunRow[] }

// Что открыто в панели вместо списка: артефакты этапа задачи. stage, kind и
// version — выбор человека (version — id артефакта, null — текущая версия);
// null — по умолчанию. rev меняется, когда в памяти мода появились данные.
export type ArtifactView = {
  key: string
  stage: string | null
  kind: string | null
  version: string | null
  error: string | null
  busy: boolean
  rev: number
}

export type CardArtifact = { title: string; version: number | null; stage: string | null; at: string | null }

export type CardAttachment = { name: string; size: number | null; at: string | null }

export type CardLink = { label: string; url: string }

// Состояние ребёнка в карточке родителя: по категории статуса и признаку
// «ждёт апрува», которые отдаёт сервер в children[].
export type ChildState = 'done' | 'waiting' | 'active' | 'backlog' | 'cancelled'

export type ChildRow = {
  key: string
  title: string
  type: string | null
  // Чип типа — только у ребёнка, чей тип отличается от самого частого.
  typeChip: boolean
  statusName: string
  state: ChildState
  awaiting: boolean
  // Входящие blocks: «после КЛЮЧ, КЛЮЧ +N»; null — связей нет или сервер их
  // не отдаёт.
  after: string | null
}

export type ChildProgress = { done: number; waiting: number; active: number; backlog: number; cancelled: number; total: number }

// progress null — сервер старше полей children[] (status_category и др.):
// строки детей есть, полосы и «после» нет.
export type CardChildren = { progress: ChildProgress | null; rows: ChildRow[] }

// Модель карточки — всё, что рисует раскрытая строка, без слов, зависящих от
// часов: давность и остаток аренды считаются при рисовании.
export type CardData = {
  key: string
  title: string
  type: string | null
  priority: string | null
  statusName: string
  workflowName: string | null
  workflowVersion: number | null
  nodes: CycleNode[]
  // Вкладки этапов: статусы с ролью, на которых были прогоны, по порядку цикла.
  stages: StageTab[]
  // Прогон (любой, и перепроверка) → статус, на котором он шёл.
  runStages: Record<string, string>
  // Номер текущего узла среди узлов схемы (с 1); 0 — статуса задачи на схеме нет.
  step: number
  attempt: number
  holder: string | null
  leaseUntil: string | null
  back: CardBack | null
  gates: CardGates | null
  awaiting: boolean
  approvalGate: string | null
  artifacts: CardArtifact[]
  attachments: CardAttachment[]
  cost: number | null
  prs: CardLink[]
  description: string | null
  children: CardChildren | null
  url: string | null
  plates: Plate[]
  returns: ReturnOption[]
  seen: Seen
  stageChips: StageChip[]
}

// Строка задачи во вкладке «Этот чат»: ключ, заголовок и подпись статуса из
// версии цикла задачи, признак «ждёт апрува» и модель карточки. error —
// последняя ошибка чтения; прежние поля при ошибке остаются, как были.
export type TaskRow = {
  key: string
  title: string
  statusName: string
  awaiting: boolean
  // Может ли человек решать гейты в проекте задачи (уровень maintainer+).
  canDecide: boolean
  error: string | null
  fetchedAt: number
  card: CardData | null
}

export type Tab = 'chat' | 'approvals' | 'tasks'

// Вкладка «Задачи» (CLAB-327). Сегменты делит данные цикла: «в работе» — не
// конечные статусы с ролью, «можно забрать» — очередь проекта, «бэклог» — не
// конечные без роли, «готово» — конечные категории done.
export type Segment = 'work' | 'queue' | 'backlog' | 'done'

// Выбор человека на вкладке: сегмент, тип (из циклов проекта), исполнитель
// ('' — любой, '-' — без исполнителя), поиск, страница с нуля и эпики, у
// которых раскрыты все дети. Токена здесь нет.
export type TasksView = { segment: Segment; type: string; assignee: string; q: string; page: number; more: string[] }

export type ListKid = { key: string; title: string; statusName: string; state: ChildState; awaiting: boolean }

// Строка списка. nested есть у задачи с детьми: полоса по категориям детей и
// первые дети; all — все дети, включая отменённых; partial — не все дети
// загружены.
export type ListRow = {
  key: string
  title: string
  type: string | null
  statusName: string
  state: ChildState
  awaiting: boolean
  holder: string | null
  assignee: string | null
  updatedAt: string | null
  nested: { progress: ChildProgress; kids: ListKid[]; all: number; partial: boolean } | null
}

// Что вкладка знает о выборке. mode: paged — сервер листает, legacy — сервер
// прошлого выпуска (выборка целиком, сегменты по версиям циклов), null — ещё
// не спрашивали. counts: null — счёт неизвестен. truncated — выборка прошлого
// сервера обрезана лимитом.
export type TasksData = {
  loaded: boolean
  error: string | null
  mode: 'paged' | 'legacy' | null
  types: string[]
  handles: string[]
  counts: Record<Segment, number | null>
  rows: ListRow[]
  total: number
  truncated: boolean
  // Есть ли у сервера признак «ждёт апрува» (его вводит CLAB-326).
  approvals: boolean
}

// Что мод знает об инстансе: адрес и проект (clab whoami или настройки
// плагина), версия сервера из /v1/instance и есть ли у него ручка событий
// проекта (null — ещё не спрашивали). error — почему данные не обновляются.
export type Connection = {
  host: string | null
  project: string | null
  serverVersion: string | null
  events: boolean | null
  error: string | null
}

declare module 'claude-code' {
  interface PluginState {
    'clab-plugin': {
      keys: Shaped<string[]>
      rows: Shaped<Record<string, TaskRow>>
      tab: Shaped<Tab>
      connection: Shaped<Connection>
      // Раскрытые строки и открытые описания — ключи задач.
      open: Shaped<string[]>
      described: Shaped<string[]>
      // Значение ряда theme из /config: палитра схемы цикла (Svg).
      theme: Shaped<string>
      // Решения по гейтам: открытое действие, итоги и «Ждут меня».
      action: Shaped<Action | null>
      outcomes: Shaped<Record<string, Outcome>>
      approvals: Shaped<ApprovalsState>
      tasksView: Shaped<TasksView>
      tasksData: Shaped<TasksData>
      // Открытый просмотр артефактов этапа; null — панель показывает список.
      view: Shaped<ArtifactView | null>
    }
  }
}
