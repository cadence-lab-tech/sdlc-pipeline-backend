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

// Ручной чек-лист — второй гейт, который закрывает решение человека.
export const CHECKLIST = 'checklist'

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
  // Решения человека по гейтам. Подписи общие для любого цикла:
  // имя гейта, перехода и статуса — из версии цикла задачи.
  approve: 'Апрув',
  returnWithReason: 'Вернуть с причиной',
  yesApprove: 'Да, апрув',
  doReturn: 'Вернуть',
  cancel: 'Отмена',
  where: 'Куда',
  reason: 'Причина',
  markPassed: 'Отметить пройденным',
  notPassed: 'Не пройден',
  mark: 'Отметить…',
  viewArtifacts: 'Посмотреть артефакты',
  notWritten: 'Решение не записано',
  changed: 'результат этапа изменился',
  lookAgain: 'посмотрите заново',
  moved: 'задача уже на другом этапе',
  approved: 'апрув поставлен',
  recorded: 'Апрув записан',
  returned: 'возвращена',
  checkedOff: 'отмечен',
  rejected: 'Отклонено',
  inThisChat: 'в этом чате',
  needMaintainer: 'нужен уровень сопровождающего',
  transition: 'Переход',
  confirmApprove: 'Утвердить',
  andMoveTo: 'и перевести в',
  onBehalf: 'от имени',
  journal: 'журнал',
  returnReason: 'Причина возврата',
  awaitingMe: 'Ждут меня',
  nothingAwaits: 'Вашего решения ничего не ждёт',
  loadingApprovals: 'загружаю «Ждут меня»…',
  olderServerApprovals: 'сервер старше: «Ждут меня» появятся с его обновлением',
  // Пояснения плашки, подтверждения, форм и итогов (макеты 02, 04, 04b, 06).
  othersPassed: 'Остальные гейты пройдены',
  gate: 'Гейт',
  transitionLower: 'переход',
  writtenAs: 'Решение запишется от вашего имени',
  agentLearns: 'Ждущий агент узнает исход по состоянию задачи.',
  returnTitle: 'Вернуть',
  whereHint: 'Куда — переходы-возвраты текущего статуса',
  reasonHint: 'Причина — её прочитает агент следующего этапа',
  withReturn: 'Вместе с возвратом от вашего имени сдаётся артефакт',
  withReturnTail: 'с этой причиной — его требует гейт перехода.',
  returnPlan: 'гейт: failed + переход',
  checkRequired: 'отметьте все обязательные пункты',
  notPassedHint: '«Не пройден» просит причину, как возврат: гейт закрывается как failed, пока чек-лист не отметят заново.',
  underYourName: 'Решение под вашим именем',
  silentChat: 'В разговор плагин ничего не пишет.',
  heldHint: 'Что держит переход — в списке гейтов карточки.',
  staleHint: 'Апрув до новой сдачи не считается.',
  approvalsIntro: 'Гейты проекта, которые вы можете закрыть по уровню доступа.',
  updatedByEvent: 'обновлено по событию',
  updatedByPoll: 'обновлено опросом',
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
  box: '☐',
  boxChecked: '☑',
  // Чип артефакта этапа и подсказка клавиши «Enter» у кнопки в фокусе.
  chip: '›',
  enter: '↩',
} as const
