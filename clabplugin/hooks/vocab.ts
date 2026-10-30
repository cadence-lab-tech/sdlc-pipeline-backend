// Словарь плагина. Слов два вида:
//   • словарь типов гейтов движка — тип гейта знает движок, а не проект;
//   • обвязка карточки — слова, которые не зависят ни от цикла, ни от проекта.
// Всё остальное, что видно в карточке, — подписи версии цикла задачи и её
// данные: названия статусов, переходов, гейтов, типов, артефактов, ключи.
// Ни одного слова под смысл какого-либо этапа здесь нет и быть не должно.

// Тип гейта → слово. Тип, которого нет в таблице (новый тип движка),
// показывается ключом как есть.
export const GATE_WORD: Readonly<Record<string, string>> = {
  artifacts_required: 'артефакты',
  schema_validation: 'схема артефакта',
  checklist: 'чек-лист',
  human_approval: 'апрув',
  agent_review: 'перепроверка',
  external_check: 'внешняя проверка',
  repo_ref_required: 'ссылка на PR или ветку',
  drift_clear: 'дрейф',
  repo_scope: 'область',
  dependencies_closed: 'зависимости',
  attempts_limit: 'предел попыток',
  stage_time_limit: 'предел времени',
}

export const gateWord = (type: string) => GATE_WORD[type] ?? type

// Тип гейта апрува человека — словарь движка (domain.GateHumanApproval).
export const HUMAN_APPROVAL = 'human_approval'

// Обвязка: каждая фраза, которую мод пишет сам. Тест подписей (AC-13) берёт
// этот список как закрытый: всё, что нарисовано и не входит в него, в
// словарь гейтов или в данные задачи, — провал.
export const WORDS = {
  awaiting: 'ждёт апрува',
  awaitingYou: 'ждёт вашего апрува',
  step: 'этап',
  of: 'из',
  attempt: 'попытка',
  holds: 'держит',
  leaseLeft: 'аренда ещё',
  nobody: 'никто не держит',
  returnedFrom: 'Вернули из',
  ago: 'назад',
  beforeTransition: 'До перехода',
  after: 'после',
  description: 'Описание',
  openInLab: 'Открыть в Cadence Lab',
  open: 'Открыть',
  children: 'Вложенные задачи',
  done: 'готово',
  inWork: 'в работе',
  notStarted: 'не начато',
  cancelled: 'отменено',
  olderServer: 'сервер старше: прогресс недоступен',
  artifacts: 'Артефакты',
  attachment: 'вложение',
  pr: 'PR',
  approval: 'апрув',
  inChat: 'в чате',
  passedN: 'пройдено',
  more: 'ещё',
  justNow: 'только что',
  yesterday: 'вчера',
  min: 'мин',
  hour: 'ч',
  day: 'дн',
  byte: 'Б',
  kb: 'КБ',
  mb: 'МБ',
  // Вкладка «Задачи»: сегменты и обвязка списка. Названия этапов и типов тут
  // не живут — они приходят с задачами.
  segWork: 'В работе',
  segQueue: 'Можно забрать',
  segBacklog: 'Бэклог',
  segDone: 'Готово',
  allTypes: 'Все типы',
  allAssignees: 'Все исполнители',
  unassigned: 'Без исполнителя',
  searchHint: 'Ключ или слова из заголовка',
  searchSubmit: 'искать',
  nothingFound: 'ничего не найдено',
  resetFilters: 'сбросить фильтры',
  queueEmpty: 'сейчас нечего забрать',
  shown: 'показано',
  sortedBy: 'сортировка: обновлены недавно',
  projectSearch: 'поиск по проекту',
  loadedOnly: 'поиск только по загруженным задачам',
  firstN: 'показаны первые',
  partialKids: 'не все вложенные загружены',
  moreKids: 'ещё',
  fewer: 'свернуть',
  prev: 'назад',
  next: 'вперёд',
  page: 'стр.',
  loading: 'загружаю…',
  holder: 'держит',
  retry: 'обновить',
  noProject: 'проект не задан — укажите его в настройках плагина',
} as const

// Значки вердикта гейта и состояния. Значки — не слова, но и они закрыты.
export const GLYPH = {
  ok: '✓',
  open: '○',
  approval: '⏸',
  fail: '✕',
  current: '●',
  skipped: '◌',
  back: '↩',
  link: '↗',
  dot: '•',
  artifact: '▤',
  file: '⌗',
  closed: '▸',
  opened: '▾',
  bar: '█',
  barEmpty: '░',
  arrow: '→',
} as const
