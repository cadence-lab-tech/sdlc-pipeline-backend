import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { Action, ApprovalsState, ArtifactView, CardData, Connection, ListRow, Outcome, Plate, RunRow, Segment, Tab, TaskRow, TasksData, TasksView } from '../types'
import { chatText, clip, isMarkup, languageOf, resolve, shownOf, typeLabel, whenRange } from './artifact'
import type { ApiArtifact, ApiAttachment, Selection, TaskFiles } from './artifact'
import {
  approvalRowsOf,
  approveSteps,
  canDecideAt,
  checklistStep,
  driftOf,
  holdsOf,
  requiredChecked,
  returnSteps,
  returnsOf,
} from './approve'
import type { ApiApproval, Drift, Step } from './approve'
import { ago, backLine, cardOf, metaLine, money, sizeOf } from './card'
import type { TaskDetail, Workflow } from './card'
import { anyKeys, promptKeys, toolKeys } from './keys'
import type { ApiList, ApiQueueItem, ApiWorkflowRow } from './tasks'
import {
  byUpdated,
  countPath,
  DEFAULT_VIEW,
  EMPTY_DATA,
  exactKey,
  groupKids,
  holders,
  kidsPath,
  KIDS_SHOWN,
  LEGACY_LIMIT,
  legacyList,
  listPath,
  matches,
  mixedTypes,
  PAGE,
  QUEUE_LIMIT,
  rowOf,
  SEGMENTS,
  typesOf,
  UNASSIGNED,
} from './tasks'
import { cycleLine } from './term'
import { cycleAlt, cycleSvg } from './svg'
import { schemeOf, tintOf } from './theme'
import type { Scheme, Tone } from './theme'
import { CHECKLIST, GLYPH, gateWord, HUMAN_APPROVAL, WORDS } from './vocab'
import {
  ApiError,
  AUTH_RETRY_MS,
  backoff,
  CLI_PATHS,
  CLI_ENV,
  EVENTS_WAIT_S,
  eventKey,
  LOGIN_HINT,
  messageOf,
  MIN_GAP_MS,
  newSession,
  optionsOf,
  POLL_MS,
  QUIET_MS,
  reasonOf,
  STAGE_CHANGED,
  whoamiOf,
} from './model'
import type { EventsPage, Options, Session } from './model'

// clab-plugin — Cadence Lab в Claude (решение 0067). Каркас: поимка ключей
// задач в чате, строка задачи с подписью статуса из её версии цикла, вкладки
// панели, команда /clab-plugin, строка статуса и слой данных под ними.
//
// В разговор мод не пишет: хуки prompt.submit и tool.call только смотрят,
// тулов модели нет, в сессию он ничего не добавляет. Задачу он меняет только
// по нажатию человека во втором шаге подтверждения: решение по
// гейту и переход личным токеном, REST-ручками сервера. Смысла этапов он не
// знает — подписи берёт из цикла задачи, ключи ловит по префиксу проекта из
// данных.
//
// Всё, что зовёт $, — в этом файле: движок прослеживает $ только внутри
// одного модуля. Чистые части — model.ts и keys.ts.

type $T = EngineInterface

const PANE = 'clab-plugin'
const TITLE = 'Cadence Lab'
const COMMAND = 'clab-plugin'

// Значения сессии, которые читает панель. Учётных данных среди них нет.
export const keysAtom = atom({ plugin: 'clab-plugin', key: 'keys' } as const, [] as string[], { shape: 'keys-1' })
export const rowsAtom = atom({ plugin: 'clab-plugin', key: 'rows' } as const, {} as Record<string, TaskRow>, {
  shape: 'rows-5',
})
export const openAtom = atom({ plugin: 'clab-plugin', key: 'open' } as const, [] as string[], { shape: 'open-1' })
export const describedAtom = atom({ plugin: 'clab-plugin', key: 'described' } as const, [] as string[], { shape: 'described-1' })
export const themeAtom = atom({ plugin: 'clab-plugin', key: 'theme' } as const, '', { shape: 'theme-1' })
export const viewAtom = atom({ plugin: 'clab-plugin', key: 'view' } as const, null as ArtifactView | null, { shape: 'view-1' })
export const tabAtom = atom({ plugin: 'clab-plugin', key: 'tab' } as const, 'chat' as Tab, { shape: 'tab-1' })
export const connectionAtom = atom(
  { plugin: 'clab-plugin', key: 'connection' } as const,
  { host: null, project: null, serverVersion: null, events: null, error: null } as Connection,
  { shape: 'connection-1' },
)

// Решения по гейтам: одно открытое действие на сессию, итоги
// последних действий по задачам и выборка «Ждут меня».
export const actionAtom = atom({ plugin: 'clab-plugin', key: 'action' } as const, null as Action | null, { shape: 'action-1' })
export const outcomesAtom = atom({ plugin: 'clab-plugin', key: 'outcomes' } as const, {} as Record<string, Outcome>, {
  shape: 'outcomes-1',
})
export const approvalsAtom = atom(
  { plugin: 'clab-plugin', key: 'approvals' } as const,
  { supported: null, canDecide: false, rows: [], truncated: false, error: null, loadedAt: null } as ApprovalsState,
  { shape: 'approvals-2' },
)
export const tasksViewAtom = atom({ plugin: 'clab-plugin', key: 'tasksView' } as const, DEFAULT_VIEW, {
  shape: 'tasksView-1',
})
export const tasksDataAtom = atom({ plugin: 'clab-plugin', key: 'tasksData' } as const, EMPTY_DATA, {
  shape: 'tasksData-1',
})

const TABS: { id: Tab; label: string }[] = [
  { id: 'chat', label: 'Этот чат' },
  { id: 'approvals', label: 'Ждут меня' },
  { id: 'tasks', label: 'Задачи' },
]

// --- clab -------------------------------------------------------------------

async function runClab($: $T, s: Session, args: string[]) {
  const candidates = s.bin ? [s.bin] : [s.opts.clab, ...CLI_PATHS].filter(Boolean)
  let last = ''
  for (const bin of candidates) {
    try {
      const res = await $.process.run([bin, ...args], { env: { ...CLI_ENV }, timeoutMs: 20_000 })
      s.bin = bin
      return res
    } catch (err) {
      last = err instanceof Error ? err.message : String(err)
    }
  }
  throw new Error(`clab не запускается (${last || 'не найден'}); путь — в настройках плагина`)
}

// Хост и проект: clab whoami --json в каталоге сессии, иначе настройки
// плагина. Один хост на сессию.
async function resolveSession($: $T, s: Session) {
  let found: { host: string | null; project: string | null; principal?: string | null } = { host: null, project: null }
  try {
    const res = await runClab($, s, ['whoami', '--json'])
    if (res.exitCode === 0) found = whoamiOf(res.stdout)
  } catch {
    // Нет clab, нет входа — остаются настройки плагина.
  }
  s.host = found.host ?? (s.opts.host || null)
  s.project = found.project ?? (s.opts.project || null)
  s.principal = 'principal' in found ? (found.principal as string | null) : null
}

async function token($: $T, s: Session): Promise<string> {
  if (s.token) return s.token
  if (!s.host) throw new ApiError(401, `инстанс не задан — ${LOGIN_HINT} или адрес в настройках плагина`)
  const res = await runClab($, s, ['auth', 'token', '--host', s.host])
  const out = res.stdout.trim()
  if (res.exitCode !== 0 || !out) throw new ApiError(401, LOGIN_HINT)
  s.token = out
  return out
}

// --- API ----------------------------------------------------------------------

// Запрос к /v1 личным токеном. На 401 токен перечитывается один раз (его мог
// обновить clab auth login в другом терминале); второй 401 — просьба войти.
async function request($: $T, s: Session, path: string, accept = 'application/json'): Promise<string> {
  for (let attempt = 0; attempt < 2; attempt++) {
    const auth = await token($, s)
    const res = await $.http.fetch(`${s.host}/v1${path}`, {
      headers: { Authorization: `Bearer ${auth}`, Accept: accept },
    })
    if (res.status === 401) {
      s.token = null
      continue
    }
    if (!res.ok) throw new ApiError(res.status, messageOf(res.status, res.text))
    return res.text
  }
  throw new ApiError(401, LOGIN_HINT)
}

async function api<T>($: $T, s: Session, path: string): Promise<T> {
  return JSON.parse(await request($, s, path)) as T
}

// Запись личным токеном. 401 — запрос не дошёл до ручки: токен
// перечитывается и запрос повторяется один раз, второй 401 — просьба войти.
// Любой другой отказ и сетевая ошибка не повторяются: запись не идемпотентна,
// и после неё карточка перечитывается и показывает, что вышло на самом деле.
async function post<T>($: $T, s: Session, path: string, body: unknown): Promise<T> {
  for (let attempt = 0; attempt < 2; attempt++) {
    const auth = await token($, s)
    const res = await $.http.fetch(`${s.host}/v1${path}`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${auth}`, Accept: 'application/json', 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    if (res.status === 401) {
      s.token = null
      continue
    }
    if (!res.ok) throw new ApiError(res.status, messageOf(res.status, res.text), reasonOf(res.text))
    return (res.text ? JSON.parse(res.text) : {}) as T
  }
  throw new ApiError(401, LOGIN_HINT)
}

// Уровень вызывающего в проекте (access.level в GET /projects/:key) — раз
// на проект за сессию. Не прочитался — null: кнопки есть, а отказ сервера
// (403) карточка покажет.
function levelOf($: $T, s: Session, project: string): Promise<string | null> {
  let p = s.levels.get(project)
  if (!p) {
    p = api<{ access?: { level?: unknown } | null }>($, s, `/projects/${encodeURIComponent(project)}`).then(
      r => (typeof r.access?.level === 'string' ? r.access.level : null),
      () => null,
    )
    s.levels.set(project, p)
  }
  return p
}

// Версия цикла иммутабельна (решение 0016): за сессию каждая читается один
// раз, и две задачи одной версии делят один запрос.
function workflow($: $T, s: Session, id: string): Promise<Workflow> {
  let p = s.workflows.get(id)
  if (!p) {
    p = api<Workflow>($, s, `/workflows/${encodeURIComponent(id)}`)
    s.workflows.set(id, p)
    p.catch(() => s.workflows.delete(id))
  }
  return p
}

// Сведения об инстансе. Ручка открытая, токен ей не нужен; сервер без неё
// (404) или недоступный мод не роняют — версия просто не известна.
async function readInstance($: $T, s: Session) {
  let serverVersion: string | null = null
  if (s.host) {
    try {
      const res = await $.http.fetch(`${s.host}/v1/instance`, { headers: { Accept: 'application/json' } })
      if (res.ok) {
        const info = JSON.parse(res.text) as { version?: unknown }
        if (typeof info.version === 'string' && info.version) serverVersion = info.version
      }
    } catch {}
  }
  await update($, connectionAtom, c => ({ ...c, host: s.host, project: s.project, serverVersion }))
}

// --- Строки задач -------------------------------------------------------------

async function loadRow($: $T, s: Session, key: string): Promise<TaskRow> {
  const detail = await api<TaskDetail>($, s, `/tasks/${encodeURIComponent(key)}`)
  const wf = await workflow($, s, detail.task.workflow_id)
  const card = cardOf(detail, wf, s.host)
  // Уровень нужен, только когда есть что решать: у задачи без плашек запроса
  // проекта нет.
  const project = detail.task.project_key || (detail.task.key || key).replace(/-\d+$/, '')
  const canDecide = card.plates.length > 0 ? canDecideAt(await levelOf($, s, project)) : false
  return {
    key: detail.task.key || key,
    title: detail.task.title,
    statusName: card.statusName,
    awaiting: card.awaiting,
    canDecide,
    error: null,
    fetchedAt: await $.clock.now(),
    card,
  }
}

// Почему перечитываем: своё действие в чате, событие или опрос (могло
// измениться извне — тогда тост), или задача только появилась.
type Reason = 'own' | 'external' | 'track'

// Тост — только на изменение извне, и только статуса или апрува.
function toastOf(prev: TaskRow | undefined, next: TaskRow): string | null {
  if (!prev || !prev.statusName || !next.statusName) return null
  if (prev.statusName !== next.statusName) return `${next.key} · ${prev.statusName} ${GLYPH.arrow} ${next.statusName}`
  if (prev.awaiting !== next.awaiting) {
    if (next.awaiting) return `${next.key} · ${GLYPH.approval} ${WORDS.awaiting}`
    const gate = prev.card?.approvalGate
    return gate ? `${next.key} · ${gate} ${GLYPH.ok}` : null
  }
  return null
}

// Перечитать задачи. Ошибка не стирает последнее известное: у строки
// остаются прежние поля и появляется текст ошибки. Ошибка входа поднимается
// наверх — цикл обновления по ней делает долгую паузу, а не повторяет.
async function refresh($: $T, s: Session, keys: string[], reason: Reason = 'own') {
  if (keys.length === 0) return
  const now = await $.clock.now()
  let authFailed = false
  const loaded = await Promise.all(
    keys.map(async key => {
      s.polledAt.set(key, now)
      try {
        return { key, row: await loadRow($, s, key), error: null as string | null }
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) authFailed = true
        return { key, row: null as TaskRow | null, error: err instanceof Error ? err.message : String(err) }
      }
    }),
  )
  const tracked = await read($, keysAtom)
  const before = await read($, rowsAtom)
  await update($, rowsAtom, rows => {
    const next = { ...rows }
    for (const { key, row, error } of loaded) {
      if (!tracked.includes(key)) continue
      const prev = next[key]
      next[key] = row ?? (prev ? { ...prev, error } : { key, title: '', statusName: '', awaiting: false, canDecide: false, error, fetchedAt: now, card: null })
    }
    return next
  })
  if (reason === 'external') {
    for (const { key, row } of loaded) {
      if (!row || !tracked.includes(key) || (s.quiet.get(key) ?? 0) > now) continue
      const text = toastOf(before[key], row)
      if (text) $.ui.toast(text)
    }
  }
  await update($, connectionAtom, c => ({ ...c, error: authFailed ? LOGIN_HINT : null }))
  await showStatus($)
  if (authFailed) throw new ApiError(401, LOGIN_HINT)
}

// Строка статуса: задачи чата и сколько ждут решения. Есть выборка «Ждут
// меня» — число из неё (то же, что в заголовке вкладки), иначе — задачи чата,
// ждущие апрува.
async function showStatus($: $T) {
  const keys = await read($, keysAtom)
  const appr = await read($, approvalsAtom)
  const rows = await read($, rowsAtom)
  const waiting = appr.supported && appr.canDecide ? appr.rows.length : keys.filter(k => rows[k]?.awaiting).length
  if (keys.length === 0 && waiting === 0) return $.ui.status(undefined)
  const conn = await read($, connectionAtom)
  const bits = [
    keys.length > 0 ? `${keys.length} ${WORDS.inChat}` : null,
    waiting > 0 ? `${GLYPH.approval} ${waiting} ${WORDS.approval}` : null,
    conn.error,
  ].filter(Boolean)
  $.ui.status(`Clab: ${bits.join(' · ')}`)
}

// «Ждут меня» с сервера. 404 — сервер старше выборки: вкладка скрыта, больше
// не спрашиваем. Ошибка не стирает прошлый список.
async function loadApprovals($: $T, s: Session) {
  if (!s.host || !s.project) return
  if ((await read($, approvalsAtom)).supported === false) return
  try {
    const r = await api<{ approvals?: ApiApproval[]; can_decide?: unknown; truncated?: unknown }>(
      $,
      s,
      `/projects/${encodeURIComponent(s.project)}/approvals`,
    )
    const loadedAt = await $.clock.now()
    await update($, approvalsAtom, () => ({
      supported: true,
      canDecide: r.can_decide === true,
      rows: approvalRowsOf(r.approvals),
      truncated: r.truncated === true,
      error: null,
      loadedAt,
    }))
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      await update($, approvalsAtom, a => ({ ...a, supported: false, rows: [], error: null }))
    } else {
      const message = err instanceof Error ? err.message : String(err)
      await update($, approvalsAtom, a => ({ ...a, error: message }))
    }
  }
  await showStatus($)
}

async function track($: $T, s: Session, found: string[]) {
  if (found.length === 0) return
  const before = await read($, keysAtom)
  const fresh = found.filter(k => !before.includes(k))
  if (fresh.length === 0) return
  await update($, keysAtom, keys => [...keys, ...fresh.filter(k => !keys.includes(k))])
  await showStatus($)
  await s.ready.catch(() => undefined)
  await refresh($, s, fresh, 'track').catch(() => undefined)
  wake($, s)
}

async function expand($: $T, keys: string[]) {
  if (keys.length === 0) return
  await update($, openAtom, list => [...list, ...keys.filter(k => !list.includes(k))])
}

const toggle = (key: string) => (list: string[]) => (list.includes(key) ? list.filter(k => k !== key) : [...list, key])

async function untrack($: $T, keys: string[]) {
  await update($, viewAtom, v => (v && keys.includes(v.key) ? null : v))
  for (const k of keys) sessionOf.current?.files.delete(k)
  await update($, keysAtom, list => list.filter(k => !keys.includes(k)))
  await update($, openAtom, list => list.filter(k => !keys.includes(k)))
  await update($, rowsAtom, rows => {
    const next = { ...rows }
    for (const k of keys) delete next[k]
    return next
  })
  await showStatus($)
}

// --- Цикл обновления ----------------------------------------------------------

function schedule($: $T, s: Session, ms: number) {
  s.timer?.cancel()
  s.timer = null
  if (s.stopped) return
  s.timer = $.clock.after(ms, () => {
    s.timer = null
    void tick($, s)
  })
}

// Разбудить цикл: появилась задача в чате или сессия только началась. Пока
// идёт запрос, второй не начинается.
function wake($: $T, s: Session) {
  if (s.stopped || s.running || s.timer) return
  schedule($, s, 0)
}

function stop(s: Session) {
  s.stopped = true
  s.timer?.cancel()
  s.timer = null
}

async function tick($: $T, s: Session) {
  if (s.stopped || s.running) return
  s.running = true
  let delay: number | null
  try {
    delay = await cycle($, s)
    if (s.failures > 0) {
      // Связь вернулась: ошибка прошлых оборотов больше не правда.
      await update($, connectionAtom, c => ({ ...c, error: null }))
      await showStatus($)
    }
    s.failures = 0
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      delay = AUTH_RETRY_MS
      await update($, connectionAtom, c => ({ ...c, error: LOGIN_HINT })).catch(() => undefined)
      await showStatus($).catch(() => undefined)
    } else {
      s.failures++
      delay = backoff(s.failures)
      const message = err instanceof Error ? err.message : String(err)
      await update($, connectionAtom, c => ({ ...c, error: `нет связи с инстансом: ${message}` })).catch(() => undefined)
      await showStatus($).catch(() => undefined)
    }
  } finally {
    s.running = false
  }
  if (delay !== null) schedule($, s, delay)
}

// Один оборот. Возвращает паузу до следующего; null — задач в чате нет, и
// цикл спит, пока его не разбудят.
async function cycle($: $T, s: Session): Promise<number | null> {
  const keys = await read($, keysAtom)
  if (!s.host) return null
  if (keys.length === 0) {
    // Задач в чате нет — «Ждут меня» всё равно обновляются раз в 30 с, пока
    // у сервера есть выборка.
    if (!s.project || (await read($, approvalsAtom)).supported === false) return null
    await loadApprovals($, s)
    return (await read($, approvalsAtom)).supported === false ? null : POLL_MS
  }
  const prefix = s.project ? `${s.project}-` : null
  const own = prefix ? keys.filter(k => k.startsWith(prefix)) : []
  const started = await $.clock.now()

  if (s.events === false || own.length === 0) {
    // Опрос: только задачи, которые не читались весь период, — только что
    // добавленная в чат прочитана при добавлении.
    const due = keys.filter(k => {
      const at = s.polledAt.get(k)
      return at === undefined || started - at >= POLL_MS
    })
    await refresh($, s, due, 'external')
    await loadApprovals($, s)
    return POLL_MS
  }

  const project = encodeURIComponent(s.project ?? '')
  if (s.cursor === null) {
    // Первый запрос — без ожидания: взять курсор. Задачи читаются целиком,
    // прошлые события не нужны.
    const page = await eventsPage($, s, `/projects/${project}/events?wait=0`)
    if (page === null) return 0
    if (s.cursor === null) {
      // Ответ без курсора — не та ручка, что в решении 0067: опрос.
      s.events = false
      await update($, connectionAtom, c => ({ ...c, events: false }))
    }
    await refresh($, s, keys, 'external')
    await loadApprovals($, s)
    return s.events === false ? POLL_MS : MIN_GAP_MS
  }

  const page = await eventsPage($, s, `/projects/${project}/events?after=${encodeURIComponent(s.cursor)}&wait=${EVENTS_WAIT_S}`)
  if (page === null) return 0
  // Событие только будит: истина — перечитанная задача (решение 0067, п. 4).
  const hit = new Set((page.events ?? []).map(eventKey).filter((k): k is string => k !== null))
  const now = await read($, keysAtom)
  // Событие ребёнка будит родителя из чата: строки детей — часть его карточки.
  const rows = await read($, rowsAtom)
  const parents = now.filter(k => !hit.has(k) && (rows[k]?.card?.children?.rows ?? []).some(c => hit.has(c.key)))
  // Задачи другого проекта событиями этого проекта не покрыты: им — опрос
  // раз в 30 с.
  const end = await $.clock.now()
  const stale = now.filter(k => {
    if (k.startsWith(prefix ?? '')) return false
    const at = s.polledAt.get(k)
    return at === undefined || end - at >= POLL_MS
  })
  await refresh($, s, [...now.filter(k => hit.has(k)), ...parents, ...stale], 'external')
  // Любое событие проекта может закрыть или открыть чей-то гейт: выборка
  // перечитывается целиком, она одна на проект.
  if ((page.events ?? []).length > 0) await loadApprovals($, s)
  return Math.max(0, MIN_GAP_MS - (end - started))
}

// Страница событий проекта. 404 — у сервера ручки нет (он старше её): мод
// переходит на опрос, null.
async function eventsPage($: $T, s: Session, path: string): Promise<EventsPage | null> {
  try {
    const page = await api<EventsPage>($, s, path)
    if (typeof page.cursor === 'string' || typeof page.cursor === 'number') s.cursor = String(page.cursor)
    if (s.events !== true && s.cursor !== null) {
      s.events = true
      await update($, connectionAtom, c => ({ ...c, events: true }))
    }
    return page
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      s.events = false
      s.cursor = null
      await update($, connectionAtom, c => ({ ...c, events: false }))
      return null
    }
    throw err
  }
}

// --- Сессия мода --------------------------------------------------------------

// Включить мод: только у сессии с поверхностью. Без неё (claude -p, раннер,
// саб-агент без UI) мод не зовёт clab, не ходит в сеть, не ставит строку
// статуса и таймеров (решение 0067, п. 3).
function activate($: $T, opts: Options): Session {
  const s = newSession(opts)
  s.ready = (async () => {
    await readTheme($)
    await resolveSession($, s)
    await readInstance($, s)
  })()
  void s.ready.then(
    () => wake($, s),
    () => wake($, s),
  )
  return s
}

// Тема Claude — ряд theme из /config: по нему выбирается палитра Svg.
// Ряда нет или он не читается — auto, prefers-color-scheme внутри картинки.
async function readTheme($: $T) {
  try {
    const row = (await $.config.list()).find(r => r.key === 'theme')
    await update($, themeAtom, () => (typeof row?.value === 'string' ? row.value : ''))
  } catch {}
}

// Команда регистрируется уже включённым модом и до первого промпта: хук
// session.start её дожидается.
async function registerCommand($: $T) {
  await $.command.register({
    name: 'clab-plugin',
    description: 'Задачи Cadence Lab этого чата: /clab-plugin [КЛЮЧ|tasks|approvals|remove КЛЮЧ]',
    argumentHint: '[КЛЮЧ|tasks|approvals|remove КЛЮЧ]',
  })
}

async function runCommand($: $T, s: Session, args: string): Promise<string> {
  const word = (args.trim().split(/\s+/)[0] ?? '').toLowerCase()
  const keys = anyKeys(args)
  let tab: Tab = 'chat'
  let text: string
  if (word === 'remove' || word === 'rm') {
    await untrack($, keys)
    text = keys.length ? `Убраны из чата: ${keys.join(', ')}.` : 'Какую задачу убрать: /clab-plugin remove КЛЮЧ.'
  } else if (word === 'tasks' || word === 'approvals') {
    tab = word
    text = word === 'tasks' ? 'Открыта вкладка «Задачи».' : 'Открыта вкладка «Ждут меня».'
  } else {
    if (keys.length) {
      await track($, s, keys)
      await expand($, keys)
    }
    const now = await read($, keysAtom)
    text = now.length ? `Задачи чата: ${now.join(', ')}.` : 'Панель открыта; задач в этом чате пока нет.'
  }
  await update($, tabAtom, () => tab)
  if (tab === 'tasks') void openTasks($, s).catch(() => undefined)
  await $.ui.open({ id: PANE, title: TITLE })
  if (tab === 'approvals') await loadApprovals($, s)
  return text
}

async function afterTool($: $T, s: Session, tool: string, input: Record<string, unknown>) {
  await s.ready
  const hit = toolKeys(tool, input, s.project)
  if (!hit.mutates || hit.keys.length === 0) return
  const until = (await $.clock.now()) + QUIET_MS
  for (const k of hit.keys) s.quiet.set(k, until)
  await refresh($, s, hit.keys, 'own')
}

async function beforeTool($: $T, s: Session, tool: string, input: Record<string, unknown>) {
  await s.ready
  const hit = toolKeys(tool, input, s.project)
  // Пока меняющий тул идёт, события по его задачам — свои: тоста нет.
  if (hit.mutates) for (const k of hit.keys) s.quiet.set(k, Number.MAX_SAFE_INTEGER)
  await track($, s, hit.keys)
}

async function onPrompt($: $T, s: Session, text: string) {
  await s.ready
  await track($, s, promptKeys(text, s.project, COMMAND))
}

// --- Решения по гейтам -----------------------------------------------
//
// Каждое действие — два нажатия: первое открывает подтверждение или форму и
// ничего не пишет, второе перечитывает задачу и пишет. В разговор не пишется
// ничего: ждущий агент узнаёт исход по событиям сервера (решение 0067, п. 4).

const enc = encodeURIComponent
const cap = (w: string) => w.charAt(0).toUpperCase() + w.slice(1)

const plateOf = (row: TaskRow | undefined, a: { transition: string; gate: string }): Plate | null =>
  row?.card?.plates.find(p => p.transition === a.transition && p.gate === a.gate) ?? null

async function startAction($: $T, mode: Action['mode'], key: string, plate: Plate) {
  const card = (await read($, rowsAtom))[key]?.card
  if (!card) return
  // Новое действие закрывает прежнее: подтверждение одно на сессию.
  await update($, actionAtom, () => ({
    key,
    mode,
    transition: plate.transition,
    gate: plate.gate,
    seen: card.seen,
    target: card.returns.find(o => !o.held)?.transition ?? card.returns[0]?.transition ?? null,
    reason: '',
    checked: plate.items.filter(i => i.checked).map(i => i.id),
  }))
  await update($, outcomesAtom, all => {
    const next = { ...all }
    delete next[key]
    return next
  })
}

const cancelAction = ($: $T) => update($, actionAtom, () => null)
const setReason = ($: $T, reason: string) => update($, actionAtom, a => (a ? { ...a, reason } : a))
const setTarget = ($: $T, target: string) => update($, actionAtom, a => (a ? { ...a, target } : a))
const toggleItem = ($: $T, id: string) => update($, actionAtom, a => (a ? { ...a, checked: toggle(id)(a.checked) } : a))

const setOutcome = ($: $T, key: string, o: Outcome) => update($, outcomesAtom, all => ({ ...all, [key]: o }))

function staleOutcome(drift: Exclude<Drift, 'same'>): Outcome {
  return drift === 'moved'
    ? { tone: 'warning', text: `${WORDS.notWritten}: ${WORDS.moved}`, journal: false, artifacts: false }
    : { tone: 'warning', text: `${WORDS.notWritten}: ${WORDS.changed} — ${WORDS.lookAgain}`, journal: false, artifacts: true }
}

function failureOf(err: unknown): Outcome {
  if (err instanceof ApiError && err.status === 409 && err.reason === STAGE_CHANGED) return staleOutcome('changed')
  if (err instanceof ApiError) {
    return { tone: 'error', text: err.status === 401 ? LOGIN_HINT : `${WORDS.notWritten}: ${err.message}`, journal: false, artifacts: false }
  }
  const message = err instanceof Error ? err.message : String(err)
  return { tone: 'error', text: `нет связи с инстансом: ${message}`, journal: false, artifacts: false }
}

const write = <T,>($: $T, s: Session, step: Step) => post<T>($, s, step.path, step.body)

// Общий ход записи: закрыть подтверждение, перечитать задачу, сверить с тем,
// что человек видел, и только тогда писать. Пока запись идёт, второе нажатие
// ничего не делает. После — карточка и «Ждут меня» перечитываются в любом
// случае: что бы ни случилось с сетью, человек видит состояние сервера.
async function perform($: $T, act: (s: Session, a: Action, plate: Plate, fresh: TaskDetail) => Promise<void>) {
  const s = sessionOf.current
  const a = await read($, actionAtom)
  if (!s || !a || s.writing) return
  s.writing = true
  await update($, actionAtom, () => null)
  const key = a.key
  // Изменения задачи от своего действия — без тоста «извне».
  s.quiet.set(key, Number.MAX_SAFE_INTEGER)
  try {
    const plate = plateOf((await read($, rowsAtom))[key], a)
    if (!plate) return
    const fresh = await api<TaskDetail>($, s, `/tasks/${enc(key)}`)
    const drift = driftOf(a.seen, fresh)
    if (drift !== 'same') {
      await setOutcome($, key, staleOutcome(drift))
      return
    }
    await act(s, a, plate, fresh)
  } catch (err) {
    await setOutcome($, key, failureOf(err))
    if (err instanceof ApiError && err.status === 403) $.ui.toast(`${key} · ${err.message}`)
  } finally {
    s.writing = false
    s.quiet.set(key, (await $.clock.now()) + QUIET_MS)
    await refresh($, s, [key], 'own').catch(() => undefined)
    await loadApprovals($, s).catch(() => undefined)
  }
}

// «Да, апрув»: решение passed, затем переход — только если после решения он
// allowed (min_approvals, другой гейт). Иначе апрув остаётся записанным, а
// карточка говорит, что держит переход.
const confirmApprove = ($: $T) =>
  perform($, async (s, a, plate) => {
    const steps = approveSteps(a.key, plate, a.seen)
    await write($, s, steps.decision)
    const after = await api<TaskDetail>($, s, `/tasks/${enc(a.key)}`)
    const t = (after.transitions ?? []).find(x => x.key === plate.transition)
    if (!t?.allowed) {
      await setOutcome($, a.key, { tone: 'warning', text: [WORDS.recorded, holdsOf(t)].filter(Boolean).join(' · '), journal: true, artifacts: false })
      return
    }
    const res = await write<{ task?: { status_name?: string } }>($, s, steps.transition)
    const to = res.task?.status_name || plate.toName
    await setOutcome($, a.key, { tone: 'ok', text: `${cap(WORDS.approved)} · ${a.key} ${GLYPH.arrow} ${to}`, journal: true, artifacts: false })
    $.ui.toast(`${a.key} · ${WORDS.approved} ${GLYPH.arrow} ${to}`)
  })

// «Вернуть»: артефакты, которых требуют гейты возврата (вид — из версии
// цикла), затем failed с причиной по гейту плашки, затем переход-возврат с
// комментарием. Атомарности нет: упавший шаг оставляет сделанное, карточка
// это показывает, возврат можно повторить.
const confirmReturn = ($: $T) =>
  perform($, async (s, a, plate, fresh) => {
    const wf = await workflow($, s, fresh.task.workflow_id)
    const option = returnsOf(fresh, wf).find(o => o.transition === a.target)
    if (!option || option.held || !a.reason.trim()) {
      const why = option?.held ? `: ${WORDS.holds} ${option.held}` : ''
      await setOutcome($, a.key, { tone: 'error', text: `${WORDS.notWritten}${why}`, journal: false, artifacts: false })
      return
    }
    let seen = a.seen.artifact
    for (const step of returnSteps(a.key, plate, option, a.reason, a.seen)) {
      if (step.kind === 'decision') step.body.seen_artifact_id = seen
      const res = await write<{ id?: string }>($, s, step)
      if (step.kind === 'artifact' && typeof res.id === 'string') seen = res.id
    }
    await setOutcome($, a.key, { tone: 'ok', text: `${a.key} ${WORDS.returned} ${GLYPH.arrow} ${option.toName}`, journal: true, artifacts: false })
    $.ui.toast(`${a.key} · ${WORDS.returned} ${GLYPH.arrow} ${option.toName}`)
  })

// Чек-лист: «Отметить пройденным» — passed с пунктами, «Не пройден» — failed
// с причиной. Переход чек-лист не делает.
const confirmChecklist = ($: $T, pass: boolean) =>
  perform($, async (s, a, plate) => {
    if (pass ? !requiredChecked(plate.items, a.checked) : !a.reason.trim()) return
    await write($, s, checklistStep(a.key, plate, a.seen, pass ? { checked: a.checked } : { reason: a.reason }))
    const text = pass ? `${plate.gateName} ${GLYPH.ok} ${WORDS.checkedOff}` : `${plate.gateName} ${GLYPH.fail} ${WORDS.notPassed}`
    await setOutcome($, a.key, { tone: pass ? 'ok' : 'warning', text, journal: true, artifacts: false })
  })

// Действие из полосы над вводом или из «Ждут меня»: карточка задачи
// раскрывается в «Этом чате», и в ней открывается то же подтверждение.
async function openAction($: $T, key: string, transition: string, gate: string) {
  await openCard($, key)
  const row = (await read($, rowsAtom))[key]
  const plate = plateOf(row, { transition, gate })
  if (!plate || !row?.canDecide) return
  await startAction($, plate.type === CHECKLIST ? 'checklist' : 'approve', key, plate)
}

// --- Вкладка «Задачи» ---------------------------------------------------------

// Сколько держим прочитанную выборку, прежде чем перечитать при возврате на
// вкладку.
const TASKS_FRESH_MS = 10_000

// Что перечитать: base — типы проекта и очередь, counts — счётчики сегментов.
// Страница читается всегда.
type TasksLoad = { base?: boolean; counts?: boolean }

const rowsOfQueue = (s: Session, v: TasksView, holder: ReadonlyMap<string, string>): { rows: ListRow[]; total: number } => {
  const items = byUpdated(
    s.queue
      .filter(it => it.claimable === true && it.task)
      .map(it => it.task)
      .filter(t => (!v.type || (typeof t.properties?.type === 'string' ? t.properties.type : '') === v.type))
      .filter(t => (v.assignee === UNASSIGNED ? !t.assignee_handle : v.assignee ? t.assignee_handle === v.assignee : true))
      .filter(t => matches(t, v.q)),
  )
  const page = items.slice(v.page * PAGE, (v.page + 1) * PAGE)
  return { rows: page.map(t => rowOf(t, { category: t.status_category, holder: holder.get(t.key) ?? null })), total: items.length }
}

// Читает выборку по выбору человека и кладёт её в состояние. Устаревший ответ
// (человек успел нажать другое) не пишется; ошибка не стирает прежний список.
async function loadTasks($: $T, s: Session, what: TasksLoad = {}) {
  const seq = ++s.tasksSeq
  await s.ready
  const fresh = () => seq === s.tasksSeq
  if (!s.host || !s.project) {
    await update($, tasksDataAtom, d => ({ ...d, loaded: true, error: s.host ? WORDS.noProject : `инстанс не задан — ${LOGIN_HINT}` }))
    return
  }
  const v = await read($, tasksViewAtom)
  const prev = await read($, tasksDataAtom)
  const base = what.base === true || !prev.loaded
  const project = `/projects/${encodeURIComponent(s.project)}`
  try {
    let types = prev.types
    if (base) {
      const [wfs, queue] = await Promise.all([
        api<{ workflows?: ApiWorkflowRow[] }>($, s, `${project}/workflows`),
        api<{ queue?: ApiQueueItem[] }>($, s, `${project}/queue?limit=${QUEUE_LIMIT}`),
      ])
      types = typesOf(wfs.workflows ?? [])
      s.queue = queue.queue ?? []
    }
    const holder = holders(s.queue)
    const counts = { ...prev.counts }
    let rows: ListRow[] = []
    let total = 0
    let truncated = false
    let approvals = false
    let mode = s.listMode

    const readPaged = async (): Promise<boolean> => {
      // Страница, счётчики остальных сегментов и дети эпиков страницы.
      const wanted = what.counts === true || base ? SEGMENTS.filter(x => x !== 'queue' && (v.q !== '' || x !== v.segment)) : []
      const showsQueue = v.q === '' && v.segment === 'queue'
      const [page, ...other] = await Promise.all([
        showsQueue ? Promise.resolve(null) : api<ApiList>($, s, `${project}${listPath(v, PAGE, v.page * PAGE)}`),
        ...(v.q === '' ? wanted.map(x => api<ApiList>($, s, `${project}${countPath(v, x)}`)) : []),
      ])
      if (page !== null && typeof page.total !== 'number') return false
      if (v.q === '') wanted.forEach((x, i) => (counts[x] = other[i]?.total ?? null))
      if (page === null) {
        const q = rowsOfQueue(s, v, holder)
        rows = q.rows
        total = q.total
      } else {
        const tasks = page.tasks ?? []
        if (v.q === '') counts[v.segment] = page.total ?? null
        const parents = tasks.filter(t => t.child_counts && Object.keys(t.child_counts).length > 0).map(t => t.key)
        const kids = parents.length ? groupKids((await api<ApiList>($, s, `${project}${kidsPath(parents)}`)).tasks ?? []) : new Map()
        rows = tasks.map(t => rowOf(t, { category: t.status_category, holder: holder.get(t.key) ?? null, kids: kids.get(t.key) }))
        total = page.total ?? tasks.length
        approvals = tasks.some(t => typeof t.awaiting_approval === 'boolean')
      }
      counts.queue = rowsOfQueue(s, { ...v, q: '', page: 0 }, holder).total
      return true
    }

    const readLegacy = async () => {
      // Сервер без листания: выборка целиком и версии циклов по workflow_id.
      if (s.legacy === null || base) {
        s.legacy = (await api<ApiList>($, s, `${project}/tasks?limit=${LEGACY_LIMIT}`)).tasks ?? []
      }
      const all = s.legacy
      const found = new Map<string, Workflow>()
      await Promise.all(
        [...new Set(all.map(t => String(t.workflow_id)))].map(async id => {
          try {
            found.set(id, await workflow($, s, id))
          } catch {}
        }),
      )
      if (v.segment === 'queue' && v.q === '') {
        const q = rowsOfQueue(s, v, holder)
        rows = q.rows
        total = q.total
      }
      const res = legacyList(all, found, v, holder)
      if (!(v.segment === 'queue' && v.q === '')) {
        rows = res.rows
        total = res.total
      }
      Object.assign(counts, res.counts, { queue: rowsOfQueue(s, { ...v, q: '', page: 0 }, holder).total })
      truncated = all.length >= LEGACY_LIMIT
    }

    if (mode !== 'legacy') {
      const ok = await readPaged()
      if (ok) mode = s.listMode = 'paged'
      else mode = s.listMode = 'legacy'
    }
    if (mode === 'legacy') await readLegacy()

    if (!fresh()) return
    s.tasksAt = await $.clock.now()
    const seen = new Set<string>()
    for (const r of rows) if (r.assignee) seen.add(r.assignee)
    for (const it of s.queue) if (it.task?.assignee_handle) seen.add(it.task.assignee_handle)
    if (v.assignee && v.assignee !== UNASSIGNED) seen.add(v.assignee)
    await update($, tasksDataAtom, () => ({
      loaded: true,
      error: null,
      mode,
      types,
      handles: [...seen].sort(),
      counts,
      rows,
      total,
      truncated,
      approvals,
    }))
    await update($, connectionAtom, c => (c.error === LOGIN_HINT ? { ...c, error: null } : c))
  } catch (err) {
    if (!fresh()) return
    const message = err instanceof Error ? err.message : String(err)
    await update($, tasksDataAtom, d => ({ ...d, loaded: true, error: message }))
    if (err instanceof ApiError && err.status === 401) await update($, connectionAtom, c => ({ ...c, error: LOGIN_HINT }))
  }
}

// Открыта вкладка (кнопка или команда): выборка перечитывается, если она
// старше нескольких секунд.
async function openTasks($: $T, s: Session | null) {
  if (!s) return
  const data = await read($, tasksDataAtom)
  if (data.loaded && (await $.clock.now()) - s.tasksAt < TASKS_FRESH_MS) return
  await loadTasks($, s, { base: true, counts: true })
}

// Выбор человека: view меняется, страница читается заново; счётчики — когда
// они зависят от выбора (тип, исполнитель).
async function chooseTasks($: $T, patch: Partial<TasksView>, what: TasksLoad = {}) {
  await update($, tasksViewAtom, v => ({ ...v, page: 0, more: [], ...patch }))
  const s = sessionOf.current
  if (s) await loadTasks($, s, what)
}

async function pageTasks($: $T, delta: number) {
  await update($, tasksViewAtom, v => ({ ...v, page: Math.max(0, v.page + delta), more: [] }))
  const s = sessionOf.current
  if (s) await loadTasks($, s)
}

// Клик по строке: карточка раскрывается под строкой, ключ попадает во вкладку
// «Этот чат». Повторный клик по открытой строке её сворачивает.
async function openFromList($: $T, key: string) {
  const s = sessionOf.current
  if (!s) return
  const keys = await read($, keysAtom)
  const open = await read($, openAtom)
  if (keys.includes(key) && open.includes(key)) {
    await update($, openAtom, toggle(key))
    return
  }
  await track($, s, [key])
  await expand($, [key])
}

// Поиск: Enter. Ключ задачи целиком открывает её карточку сразу (UC-9.3).
async function searchTasks($: $T, text: string) {
  const q = text.trim()
  await chooseTasks($, { q })
  if (q && exactKey(q)) await openFromList($, q.toUpperCase())
}

// --- Карточка ------------------------------------------------------------------

// Элементы поверхности. Svg есть везде, кроме терминала: там схема — строка.
// Input и Select есть везде, кроме mobile: там их вызывают с проверкой.
type Els = { Box: any; Text: any; Button: any; Link: any; Markdown: any; Code: any; Svg?: any; Input?: any; Select?: any }

// Раскрыть карточку задачи из полосы над вводом или из строки ребёнка:
// задача попадает в чат, строка раскрывается, панель открывается.
async function openCard($: $T, key: string) {
  const s = sessionOf.current
  if (s) await track($, s, [key])
  await expand($, [key])
  await update($, tabAtom, () => 'chat' as Tab)
  await $.ui.open({ id: PANE, title: TITLE })
}

// Сессия для обработчиков нажатий, у которых нет своей ссылки на неё.
const sessionOf: { current: Session | null } = { current: null }

// --- Просмотр артефактов этапа --------------------------------------------------
//
// Только чтение: GET списка, содержимого и вложений личным токеном. Данные —
// в памяти сессии мода (файлы и тексты), в состоянии — только выбор человека.
// Состояние меняется после того, как данные выбранного пришли: при ошибке
// остаётся прежнее показанное, а ошибка — строкой под ним.

async function loadFiles($: $T, s: Session, key: string) {
  const [a, f] = await Promise.all([
    api<{ artifacts?: ApiArtifact[] | null }>($, s, `/tasks/${enc(key)}/artifacts?include_superseded=true`),
    api<{ attachments?: ApiAttachment[] | null }>($, s, `/tasks/${enc(key)}/attachments`),
  ])
  s.files.set(key, { artifacts: a.artifacts ?? [], attachments: f.attachments ?? [] } satisfies TaskFiles)
}

async function loadContent($: $T, s: Session, art: ApiArtifact) {
  if (s.content.has(art.id)) return
  s.content.set(art.id, await request($, s, `/artifacts/${enc(art.id)}/content`, '*/*'))
}

const patchView = (key: string, p: Partial<ArtifactView>) => (v: ArtifactView | null) =>
  v && v.key === key ? { ...v, ...p, rev: v.rev + 1 } : v

// Показать выбор: сначала данные, потом состояние. reload — перечитать список
// (открытие просмотра), иначе список из памяти и версия без запроса, если её
// текст уже получен.
async function showView($: $T, s: Session, key: string, sel: Selection, reload = false) {
  await update($, viewAtom, patchView(key, { busy: true }))
  try {
    if (reload || !s.files.has(key)) await loadFiles($, s, key)
    const card = (await read($, rowsAtom))[key]?.card
    if (!card) throw new Error('карточка задачи ещё не загружена')
    const r = resolve(card, s.files.get(key), sel)
    if (r.version && shownOf(r.version.art) !== 'file') await loadContent($, s, r.version.art)
    await update(
      $,
      viewAtom,
      patchView(key, {
        stage: r.stage?.key ?? null,
        kind: r.chip?.kind ?? null,
        version: r.version && !r.version.current ? r.version.art.id : null,
        error: null,
        busy: false,
      }),
    )
  } catch (err) {
    await update($, viewAtom, patchView(key, { error: err instanceof Error ? err.message : String(err), busy: false }))
  }
}

// Открыть просмотр из карточки: этап (null — по умолчанию) и панель.
async function openView($: $T, key: string, stage: string | null) {
  const s = sessionOf.current
  if (!s) return
  await update($, viewAtom, () => ({ key, stage, kind: null, version: null, error: null, busy: true, rev: 0 }))
  await update($, tabAtom, () => 'chat' as Tab)
  await $.ui.open({ id: PANE, title: TITLE })
  await showView($, s, key, { stage, kind: null, version: null }, true)
}

// Выбор вкладки, чипа или версии. Выбор выше по иерархии сбрасывает ниже.
async function choose($: $T, key: string, pick: Partial<Selection>) {
  const s = sessionOf.current
  const v = await read($, viewAtom)
  if (!s || !v || v.key !== key) return
  const sel: Selection = { stage: v.stage, kind: v.kind, version: v.version, ...pick }
  await showView($, s, key, sel)
}

async function closeView($: $T) {
  await update($, viewAtom, () => null)
}

// «В чат»: заголовок и полный текст — в поле ввода. Ход не начинается и в
// разговор плагин ничего не пишет: отправляет человек.
async function toChat($: $T, key: string, art: ApiArtifact, text: string) {
  await $.prompt.fill({ text: chatText(key, art.kind, art.version, text), mode: 'insert' })
}

async function copyText($: $T, text: string, surface: string) {
  const done = await $.ui.copy({ text, surface: surface as never })
  $.ui.toast(done.isCopied ? WORDS.copied : WORDS.notCopied)
}

const MARK_COLOR = { ok: 'success', skip: undefined, open: undefined, approval: 'warning', fail: 'error' } as const
const MARK_GLYPH = { ok: GLYPH.ok, skip: GLYPH.ok, open: GLYPH.open, approval: GLYPH.approval, fail: GLYPH.fail } as const
const CHILD_DOT = { done: 'success', waiting: 'warning', active: 'claude', backlog: undefined, cancelled: undefined } as const

type CardOpts = { scheme: ReturnType<typeof schemeOf>; now: number; described: boolean; decide: Decide }

function cardTree($: $T, els: Els, surface: string, c: CardData, o: CardOpts) {
  const { Box, Text, Button, Link, Markdown, Svg } = els
  const chips = [c.type, c.priority].filter(Boolean).join(' · ')

  const cycle =
    c.nodes.length === 0 ? null : surface !== 'terminal' && Svg ? (
      <Svg source={cycleSvg(c.nodes, c.back, o.scheme)} alt={cycleAlt(c.nodes)} />
    ) : (
      <Text wrap="wrap">
        {cycleLine(c.nodes).map((seg, i) => (
          <Text key={`seg-${i}`} color={seg.color} dimColor={seg.dim} strikethrough={seg.strike}>
            {seg.text}
          </Text>
        ))}
      </Text>
    )

  const back = backLine(c, o.now)

  const gates = c.gates ? (
    <Box flexDirection="column">
      <Box flexDirection="row" justifyContent="space-between" gap={1}>
        <Text bold wrap="truncate-end">
          {WORDS.beforeTransition} «{c.gates.transition}»
        </Text>
        {c.gates.total > 0 ? (
          <Text dimColor>
            {c.gates.passed} {WORDS.of} {c.gates.total}
          </Text>
        ) : null}
      </Box>
      {c.gates.items.map((g, i) => (
        <Text key={`gate-${i}`} wrap="wrap">
          <Text color={MARK_COLOR[g.mark]} dimColor={g.mark === 'skip' || g.mark === 'open'}>
            {MARK_GLYPH[g.mark]}{' '}
          </Text>
          {g.name}
          <Text dimColor>
            {' · '}
            {gateWord(g.type)}
            {g.reason ? ` · ${g.reason}` : ''}
          </Text>
        </Text>
      ))}
    </Box>
  ) : null

  const kids = c.children ? childrenTree($, els, c.children) : null

  const files =
    c.artifacts.length + c.attachments.length === 0 ? null : (
      <Box flexDirection="column">
        <Text bold>{WORDS.artifacts}</Text>
        {c.artifacts.map((a, i) => (
          <Box key={`art-${i}`} flexDirection="row" justifyContent="space-between" gap={1}>
            <Box flexGrow={1} flexShrink={1}>
              <Text wrap="truncate-end">
                <Text dimColor>{GLYPH.artifact} </Text>
                {a.title}
                {a.stage ? <Text dimColor> · {a.stage}</Text> : ''}
              </Text>
            </Box>
            <Text dimColor>{[a.version !== null ? `v${a.version}` : null, ago(a.at, o.now)].filter(Boolean).join(' · ')}</Text>
          </Box>
        ))}
        {c.attachments.map((a, i) => (
          <Box key={`file-${i}`} flexDirection="row" justifyContent="space-between" gap={1}>
            <Box flexGrow={1} flexShrink={1}>
              <Text wrap="truncate-end">
                <Text dimColor>{GLYPH.file} </Text>
                {a.name}
                <Text dimColor> · {WORDS.attachment}</Text>
              </Text>
            </Box>
            <Text dimColor>{sizeOf(a.size) ?? ''}</Text>
          </Box>
        ))}
      </Box>
    )

  const stages =
    c.stages.length === 0 ? null : (
      <Box flexDirection="column">
        <Text bold>{WORDS.stages}</Text>
        <Box flexDirection="row" gap={1} flexWrap="wrap">
          {c.stages.map(st => (
            <Button key={`stage-open-${c.key}-${st.key}`} onPress={() => void openView($, c.key, st.key)}>
              {st.attempts > 1 ? `${st.name} ×${st.attempts}` : st.name}
            </Button>
          ))}
        </Box>
      </Box>
    )

  const footer = (
    <Box flexDirection="row" gap={2} flexWrap="wrap">
      {c.description ? (
        <Button key={`desc-${c.key}`} plain onPress={() => void update($, describedAtom, toggle(c.key))}>
          {WORDS.description}
        </Button>
      ) : null}
      {c.cost !== null ? <Text dimColor>{money(c.cost)}</Text> : null}
      {c.prs.map((p, i) => (
        <Link key={`pr-${i}`} href={p.url} label={`${WORDS.pr} ${p.label} ${GLYPH.link}`} />
      ))}
      {c.url ? <Link key={`lab-${c.key}`} href={c.url} label={`${WORDS.openInLab} ${GLYPH.link}`} /> : null}
    </Box>
  )

  return (
    <Box flexDirection="column" gap={1} paddingLeft={2} key={`card-${c.key}`}>
      {chips ? <Text dimColor>{chips}</Text> : null}
      <Text bold wrap="wrap">
        {c.title}
      </Text>
      {cycle}
      <Box flexDirection="column">
        <Text dimColor wrap="wrap">
          {metaLine(c, o.now)}
        </Text>
        {back ? (
          <Text wrap="wrap">
            <Text color="error">{GLYPH.back} </Text>
            {back}
          </Text>
        ) : null}
      </Box>
      {gates}
      {decideTree($, els, c, o.decide)}
      {kids}
      {files}
      {stages}
      {footer}
      {o.described && c.description ? <Markdown text={c.description} /> : null}
    </Box>
  )
}

const runLine = (r: RunRow) => [r.role, r.holder, [whenRange(r.from, r.to), r.running ? WORDS.running : null].filter(Boolean).join(' · ') || null].filter(Boolean).join(' · ')

function viewTree($: $T, els: Els, row: TaskRow, v: ArtifactView) {
  const { Box, Text, Button, Link, Markdown, Code, Select } = els
  const s = sessionOf.current
  const c = row.card as CardData
  const files = s?.files.get(v.key)
  const r = resolve(c, files, v)
  const art = r.version?.art
  const shown = art ? shownOf(art) : null
  const text = art ? s?.content.get(art.id) : undefined
  const cut = text !== undefined ? clip(text) : null

  const head = (
    <Box flexDirection="row" justifyContent="space-between" gap={1}>
      <Box flexDirection="row" gap={1} flexGrow={1} flexShrink={1}>
        <Button key="view-back" plain onPress={() => void closeView($)}>
          {`${GLYPH.prev} ${c.key}`}
        </Button>
        {r.stage ? (
          <Text bold wrap="truncate-end">
            {GLYPH.crumb} {r.stage.name}
          </Text>
        ) : null}
      </Box>
      <Button key="view-close" plain dimColor onPress={() => void closeView($)}>
        {GLYPH.close}
      </Button>
    </Box>
  )

  const tabs = (
    <Box flexDirection="row" gap={1} flexWrap="wrap">
      {c.stages.map(st => (
        <Button
          key={`stage-${st.key}`}
          variant={st.key === r.stage?.key ? 'primary' : 'secondary'}
          onPress={() => void choose($, v.key, { stage: st.key, kind: null, version: null })}>
          {st.attempts > 1 ? `${st.name} ×${st.attempts}` : st.name}
        </Button>
      ))}
    </Box>
  )

  const runs = r.stage ? (
    <Box flexDirection="column">
      {r.stage.runs.map(run => (
        <Box flexDirection="column" key={`run-${run.id}`}>
          <Text wrap="wrap" dimColor>
            {WORDS.run} {run.attempt} · {runLine(run)}
          </Text>
          {run.back ? (
            <Text wrap="wrap">
              <Text color="error">{GLYPH.back} </Text>
              {WORDS.returnedFrom} {run.back.fromName}
              {run.back.reason ? `: «${run.back.reason}»` : ''}
            </Text>
          ) : null}
          {run.reviews.map(rv => (
            <Text key={`review-${rv.id}`} wrap="wrap" dimColor>
              {GLYPH.nested} {gateWord('agent_review')} · {runLine(rv)}
            </Text>
          ))}
        </Box>
      ))}
    </Box>
  ) : null

  const chips =
    r.chips.length === 0 ? (
      <Text dimColor>{files ? WORDS.noArtifacts : WORDS.loading}</Text>
    ) : (
      <Box flexDirection="row" gap={1} flexWrap="wrap">
        {r.chips.map(ch => (
          <Button
            key={`kind-${ch.kind}`}
            variant={ch.kind === r.chip?.kind ? 'primary' : 'secondary'}
            onPress={() => void choose($, v.key, { kind: ch.kind, version: null })}>
            {ch.title}
          </Button>
        ))}
      </Box>
    )

  // Прямой ссылки на содержимое нет: браузер не приложит токен. Скачивание — строка для
  // терминала, а ссылка ведёт в карточку задачи в веб-интерфейсе (ниже, у вложений и у артефакта).
  const download = (id: string) => (
    <Box flexDirection="column">
      <Text dimColor wrap="wrap">
        {WORDS.download} {WORDS.viaCli}: clab artifact {id} -o ./
      </Text>
    </Box>
  )

  let body = null
  if (r.chip && art) {
    const versions = r.chip.versions
    const picker =
      versions.length > 1 ? (
        <Select
          key="version"
          options={[...versions].reverse().map(x => ({ value: x.art.id, label: `v${x.art.version ?? 1}${x.current ? ` · ${WORDS.currentVersion}` : ''}` }))}
          value={art.id}
          onSelect={(id: string) => void choose($, v.key, { version: id })}
        />
      ) : (
        <Text dimColor>v{art.version ?? 1}</Text>
      )
    let content = null
    if (shown === 'file') {
      content = (
        <Box flexDirection="column">
          <Text dimColor wrap="wrap">
            {[typeLabel(art.media_type ?? undefined, art.file_name ?? undefined), sizeOf(typeof art.size_bytes === 'number' ? art.size_bytes : null), art.file_name].filter(Boolean).join(' · ')}
            {isMarkup(art) ? ` · ${WORDS.download}, ${WORDS.noInline}` : ''}
          </Text>
          {download(art.id)}
        </Box>
      )
    } else if (cut) {
      content = (
        <Box flexDirection="column">
          {shown === 'markdown' ? <Markdown text={cut.text} /> : <Code source={cut.text} language={languageOf(art)} format={shown === 'diff' ? 'diff' : undefined} />}
          {/* Срез и без скрытых строк: одна длинная строка режется по знакам (UC-1, 4a). */}
          {cut.text !== text ? (
            <Box flexDirection="row" gap={2} flexWrap="wrap">
              {cut.hidden > 0 ? (
                <Text dimColor>
                  {GLYPH.more} {WORDS.more} {cut.hidden} {WORDS.lines}
                </Text>
              ) : null}
              {c.url ? <Link href={c.url} label={`${WORDS.openFull} ${GLYPH.link}`} /> : null}
            </Box>
          ) : null}
        </Box>
      )
    } else {
      content = <Text dimColor>{v.busy ? WORDS.loading : ''}</Text>
    }
    body = (
      <Box flexDirection="column" gap={1}>
        <Box flexDirection="row" justifyContent="space-between" gap={1}>
          <Box flexDirection="column" flexGrow={1} flexShrink={1}>
            <Text bold wrap="wrap">
              {art.title ?? art.kind}
            </Text>
            <Text dimColor wrap="truncate-end">
              {art.kind}
            </Text>
          </Box>
          {picker}
        </Box>
        {content}
        <Box flexDirection="row" gap={1} flexWrap="wrap">
          {text !== undefined ? (
            <Button key="to-chat" variant="primary" onPress={() => void toChat($, c.key, art, text)}>
              {WORDS.toChat}
            </Button>
          ) : null}
          {text !== undefined ? (
            <Button key="copy" variant="secondary" onPress={e => void copyText($, text, e.surface)}>
              {WORDS.copy}
            </Button>
          ) : null}
          {c.url ? <Link href={c.url} label={`${WORDS.openInLab} ${GLYPH.link}`} /> : null}
        </Box>
      </Box>
    )
  }

  const attachments = (files?.attachments ?? []).length === 0 ? null : (
    <Box flexDirection="column">
      <Text bold>{WORDS.attachments}</Text>
      {(files?.attachments ?? []).map(a => (
        <Box flexDirection="column" key={`att-${a.id}`}>
          <Text wrap="wrap">
            <Text dimColor>{GLYPH.file} </Text>
            {a.file_name ?? a.title}
          </Text>
          <Text dimColor wrap="wrap">
            {[
              typeLabel(a.media_type, a.file_name),
              sizeOf(typeof a.size_bytes === 'number' ? a.size_bytes : null),
              a.added_by,
              isMarkup({ media_type: a.media_type, file_name: a.file_name }) ? `${WORDS.download}, ${WORDS.noInline}` : null,
            ]
              .filter(Boolean)
              .join(' · ')}
          </Text>
          {download(a.id)}
        </Box>
      ))}
      {c.url ? <Link key="att-open" href={c.url} label={`${WORDS.openInLab} ${GLYPH.link}`} /> : null}
    </Box>
  )

  return (
    <Box flexDirection="column" gap={1} key={`view-${c.key}`}>
      {head}
      {tabs}
      {runs}
      {chips}
      {body}
      {v.error ? (
        <Text color="error" wrap="wrap">
          {WORDS.notLoaded}: {v.error}
        </Text>
      ) : null}
      {attachments}
    </Box>
  )
}

const BAR = 24

function childrenTree($: $T, els: Els, ch: NonNullable<CardData['children']>) {
  const { Box, Text, Button } = els
  const p = ch.progress
  let bar = null
  if (p && p.total > 0) {
    const cells = (n: number) => Math.round((n / p.total) * BAR)
    const done = cells(p.done)
    const waiting = cells(p.waiting)
    const active = cells(p.active)
    const rest = Math.max(0, BAR - done - waiting - active)
    bar = (
      <Box flexDirection="column">
        <Text>
          <Text color="success">{GLYPH.bar.repeat(done)}</Text>
          <Text color="warning">{GLYPH.bar.repeat(waiting)}</Text>
          <Text color="claude">{GLYPH.bar.repeat(active)}</Text>
          <Text dimColor>{GLYPH.barEmpty.repeat(rest)}</Text>
        </Text>
        <Text dimColor wrap="wrap">
          {[
            `${GLYPH.dot} ${WORDS.done} ${p.done}`,
            `${GLYPH.dot} ${WORDS.awaiting} ${p.waiting}`,
            `${GLYPH.dot} ${WORDS.inWork} ${p.active}`,
            `${GLYPH.dot} ${WORDS.notStarted} ${p.backlog}`,
          ].join('  ')}
        </Text>
      </Box>
    )
  }
  return (
    <Box flexDirection="column">
      <Box flexDirection="row" justifyContent="space-between" gap={1}>
        <Text bold>{WORDS.children}</Text>
        {p ? (
          <Text dimColor>
            {p.done} {WORDS.of} {p.total} {WORDS.done}
            {p.cancelled > 0 ? ` · ${WORDS.cancelled} ${p.cancelled}` : ''}
          </Text>
        ) : null}
      </Box>
      {bar}
      {ch.rows.map(r => (
        <Button key={`child-${r.key}`} plain onPress={() => void openCard($, r.key)}>
          <Text color={CHILD_DOT[r.state]} dimColor={r.state === 'backlog' || r.state === 'cancelled'}>
            {r.state === 'backlog' || r.state === 'cancelled' ? GLYPH.open : GLYPH.current}{' '}
          </Text>
          <Text dimColor>{r.key} </Text>
          <Text strikethrough={r.state === 'cancelled'}>{r.title}</Text>
          {r.after ? (
            <Text dimColor>
              {' · '}
              {WORDS.after} {r.after}
            </Text>
          ) : (
            ''
          )}
          {r.typeChip && r.type ? <Text dimColor> · {r.type}</Text> : ''}
          <Text color={r.awaiting ? 'warning' : undefined} dimColor={!r.awaiting}>
            {' · '}
            {r.awaiting ? `${GLYPH.approval} ` : ''}
            {r.statusName}
          </Text>
        </Button>
      ))}
      {p ? null : <Text dimColor>{WORDS.olderServer}</Text>}
    </Box>
  )
}

// Плашки «ждёт вашего решения» в карточке, формы действий и итог последнего
// действия. Кнопок нет у участника ниже maintainer; на mobile нет
// полей ввода — вместо действий ссылка в веб-интерфейс.
//
// Каждое состояние — блок (макет 04): плашка и подтверждение — заливка
// warning, отклонённое и «Решение не записано» — error, «записан, но переход
// держит» — нейтральная, формы и итог апрува — рамка. Заливка — свой цвет под
// явную тему Claude (tintOf); при теме auto её нет, и блок держит рамка цвета
// тона.
type Decide = {
  surface: string
  canDecide: boolean
  action: Action | null
  outcome: Outcome | undefined
  principal: string | null
  // Цифры 1 и 2 в терминале — у первой плашки панели: две одинаковые
  // цифры перебивали бы друг друга.
  hotkeys: { used: boolean }
  scheme: Scheme
  now: number
}

// Блок состояния: рамка всегда, заливка — по тону и явной теме.
function block(els: Els, key: string, tone: Tone | null, scheme: Scheme, children: unknown) {
  const { Box } = els
  const fill = tone ? tintOf(scheme, tone) : undefined
  const edge = tone && tone !== 'neutral' ? tone : undefined
  return (
    <Box key={key} flexDirection="column" borderStyle="round" borderColor={edge} borderDimColor={!edge} backgroundColor={fill} paddingX={1}>
      {children}
    </Box>
  )
}

// --- Тело вкладки «Задачи» -------------------------------------------------------

const SEGMENT_LABEL: Record<Segment, string> = {
  work: WORDS.segWork,
  queue: WORDS.segQueue,
  backlog: WORDS.segBacklog,
  done: WORDS.segDone,
}

type TasksCtx = {
  surface: string
  view: TasksView
  data: TasksData
  rows: Record<string, TaskRow>
  keys: string[]
  open: string[]
  described: string[]
  scheme: ReturnType<typeof schemeOf>
  now: number
  // Решения по гейтам (326): открытое действие, итоги и выбор цифр в терминале
  // — те же, что в карточке в «Этом чате».
  action: Action | null
  outcomes: Record<string, Outcome>
  principal: string | null
  hotkeys: { used: boolean }
}

function listBar(els: Els, p: ListRow['nested'] & object) {
  const { Box, Text } = els
  const total = p.progress.total
  if (total === 0) return null
  const cells = (n: number) => Math.round((n / total) * BAR)
  const done = cells(p.progress.done)
  const waiting = cells(p.progress.waiting)
  const active = cells(p.progress.active)
  const rest = Math.max(0, BAR - done - waiting - active)
  return (
    <Box flexDirection="column" paddingLeft={2}>
      <Text>
        <Text color="success">{GLYPH.bar.repeat(done)}</Text>
        <Text color="warning">{GLYPH.bar.repeat(waiting)}</Text>
        <Text color="claude">{GLYPH.bar.repeat(active)}</Text>
        <Text dimColor>{GLYPH.barEmpty.repeat(rest)}</Text>
      </Text>
    </Box>
  )
}

// Заголовок на заливке — цветом текста темы; без заливки — цветом тона.
const headColor = (scheme: Scheme, tone: Tone) => (tintOf(scheme, tone) ? undefined : tone)

function decideTree($: $T, els: Els, c: CardData, d: Decide) {
  const { Box } = els
  if (c.plates.length === 0 && !d.outcome) return null
  return (
    <Box flexDirection="column" gap={1} key={`plates-${c.key}`}>
      {c.plates.map(p => plateTree($, els, c, p, d))}
      {d.outcome ? outcomeTree(els, c, d.outcome, d) : null}
    </Box>
  )
}

function listRowTree($: $T, els: Els, r: ListRow, c: TasksCtx, mixed: boolean) {
  const { Box, Text, Button } = els
  const isOpen = c.open.includes(r.key) && !!c.rows[r.key]?.card
  const dim = r.state === 'backlog' || r.state === 'cancelled'
  const n = r.nested
  const expanded = c.view.more.includes(r.key)
  const kids = n ? (expanded ? n.kids : n.kids.slice(0, KIDS_SHOWN)) : []
  const hidden = n ? n.all - KIDS_SHOWN : 0
  const meta = [
    r.awaiting ? `${GLYPH.approval} ${WORDS.awaiting} · ${r.statusName}` : r.statusName,
    ago(r.updatedAt, c.now),
    r.holder ? `${WORDS.holder} ${r.holder}` : null,
  ].filter(Boolean)
  const stored = c.rows[r.key]
  return (
    <Box flexDirection="column" key={`lrow-${r.key}`}>
      <Button key={`task-${r.key}`} plain onPress={() => void openFromList($, r.key)}>
        <Text color={CHILD_DOT[r.state]} dimColor={dim}>
          {n ? '◈' : dim ? GLYPH.open : GLYPH.current}{' '}
        </Text>
        <Text dimColor>{r.key} </Text>
        <Text bold={isOpen || !!n}>{r.title}</Text>
        {mixed && r.type ? <Text dimColor> · {r.type}</Text> : ''}
        {n ? (
          <Text dimColor>
            {' · '}
            {n.progress.done} {WORDS.of} {n.progress.total}
          </Text>
        ) : (
          ''
        )}
      </Button>
      <Box paddingLeft={2}>
        <Text color={r.awaiting ? 'warning' : undefined} dimColor={!r.awaiting} wrap="truncate-end">
          {meta.join(' · ')}
        </Text>
      </Box>
      {n ? listBar(els, n) : null}
      {n
        ? kids.map(k => (
            <Box key={`lkid-${r.key}-${k.key}`} paddingLeft={2}>
              <Button key={`kid-${r.key}-${k.key}`} plain onPress={() => void openFromList($, k.key)}>
                <Text color={CHILD_DOT[k.state]} dimColor={k.state === 'backlog' || k.state === 'cancelled'}>
                  {k.state === 'backlog' || k.state === 'cancelled' ? GLYPH.open : GLYPH.current}{' '}
                </Text>
                <Text dimColor>{k.key} </Text>
                <Text strikethrough={k.state === 'cancelled'}>{k.title}</Text>
                <Text color={k.awaiting ? 'warning' : undefined} dimColor={!k.awaiting}>
                  {' · '}
                  {k.awaiting ? `${GLYPH.approval} ` : ''}
                  {k.statusName}
                </Text>
              </Button>
            </Box>
          ))
        : null}
      {n && hidden > 0 ? (
        <Box paddingLeft={2}>
          <Button
            key={`more-${r.key}`}
            plain
            dimColor
            onPress={() => void update($, tasksViewAtom, v => ({ ...v, more: toggle(r.key)(v.more) }))}>
            {expanded ? WORDS.fewer : `… ${WORDS.moreKids} ${hidden}`}
          </Button>
        </Box>
      ) : null}
      {n?.partial && expanded ? (
        <Box paddingLeft={2}>
          <Text dimColor>{WORDS.partialKids}</Text>
        </Box>
      ) : null}
      {stored?.error ? (
        <Box paddingLeft={2}>
          <Text color="error">{stored.error}</Text>
        </Box>
      ) : null}
      {isOpen && stored?.card
        ? cardTree($, els, c.surface, stored.card, {
            scheme: c.scheme,
            now: c.now,
            described: c.described.includes(r.key),
            decide: {
              surface: c.surface,
              canDecide: stored.canDecide,
              action: c.action,
              outcome: c.outcomes[r.key],
              principal: c.principal,
              hotkeys: c.hotkeys,
              scheme: c.scheme,
              now: c.now,
            },
          })
        : null}
    </Box>
  )
}

function plateTree($: $T, els: Els, c: CardData, p: Plate, d: Decide) {
  const { Box, Text, Button, Link } = els
  const id = `${c.key}-${p.transition}-${p.gate}`
  const open = d.action && d.action.key === c.key && d.action.transition === p.transition && d.action.gate === p.gate ? d.action : null
  const mobile = d.surface === 'mobile' || !els.Input || !els.Select
  const hot = d.surface === 'terminal' && !d.hotkeys.used
  if (hot && d.canDecide && !mobile) d.hotkeys.used = true

  // Второе состояние того же блока: подтверждение, форма возврата или
  // чек-листа стоят на месте плашки.
  if (d.canDecide && open && !mobile) {
    if (open.mode === 'approve') return confirmTree($, els, c, p, d, hot)
    if (open.mode === 'return') return returnTree($, els, c, open, d)
    return checklistTree($, els, c, p, open, d)
  }

  const checklist = p.type === CHECKLIST
  const failed = p.state === 'failed'
  const tone: Tone | null = failed ? 'error' : checklist ? null : 'warning'
  const age = p.state === 'pending' ? ago(p.since, d.now) : null

  let actions = null
  if (mobile) {
    actions = c.url ? <Link key={`plate-lab-${id}`} href={c.url} label={`${WORDS.openInLab} ${GLYPH.link}`} /> : null
  } else if (d.canDecide) {
    actions =
      p.type === HUMAN_APPROVAL ? (
        <Box flexDirection="row" gap={1} flexWrap="wrap">
          <Button key={`approve-${id}`} variant="primary" hotkey={hot ? '1' : undefined} onPress={() => void startAction($, 'approve', c.key, p)}>
            {WORDS.approve}
          </Button>
          {c.returns.length > 0 ? (
            <Button key={`return-${id}`} hotkey={hot ? '2' : undefined} onPress={() => void startAction($, 'return', c.key, p)}>
              {WORDS.returnWithReason}
            </Button>
          ) : null}
        </Box>
      ) : (
        <Box flexDirection="row" gap={1}>
          <Button key={`mark-${id}`} variant="primary" hotkey={hot ? '1' : undefined} onPress={() => void startAction($, 'checklist', c.key, p)}>
            {WORDS.mark}
          </Button>
        </Box>
      )
  }

  const head = failed ? (
    <Text color={headColor(d.scheme, 'error') ?? undefined} bold wrap="wrap">
      {GLYPH.fail} {WORDS.rejected} · {p.gateName}
      {p.reason ? ` · ${p.reason}` : ''}
    </Text>
  ) : checklist ? (
    <Box flexDirection="row" justifyContent="space-between" gap={1}>
      <Text bold wrap="wrap">
        {GLYPH.open} {p.gateName}
      </Text>
      <Text dimColor>{gateWord(p.type)}</Text>
    </Box>
  ) : (
    <Box flexDirection="row" justifyContent="space-between" gap={1}>
      <Box flexGrow={1} flexShrink={1}>
        <Text color={headColor(d.scheme, 'warning')} bold wrap="wrap">
          {cap(WORDS.awaitingYou)} · {p.gateName}
        </Text>
      </Box>
      {age ? <Text dimColor>{age}</Text> : null}
    </Box>
  )

  const where = checklist ? (
    <Text dimColor wrap="wrap">
      {WORDS.beforeTransition} «{p.transitionName}» {GLYPH.arrow} {p.toName}
      {p.state === 'pending' && p.reason ? ` · ${p.reason}` : ''}
    </Text>
  ) : (
    <Text dimColor wrap="wrap">
      {WORDS.transition} «{p.transitionName}» {GLYPH.arrow} {p.toName}
      {p.state === 'pending' && p.reason ? ` · ${p.reason}` : ''}
    </Text>
  )

  const others =
    p.others.length > 0 && !failed ? (
      <Text dimColor wrap="wrap">
        {WORDS.othersPassed}: {p.others.map(o => `${o} ${GLYPH.ok}`).join(' · ')}
      </Text>
    ) : null

  // Чипы — артефакты этапа по виду; до просмотра в самом плагине ведут в
  // карточку веб-интерфейса.
  const chips =
    c.stageChips.length > 0 ? (
      <Box flexDirection="row" gap={2} flexWrap="wrap">
        {c.stageChips.map(a =>
          c.url ? (
            <Link key={`chip-${id}-${a.kind}`} href={c.url} label={`${a.title} ${GLYPH.chip}`} />
          ) : (
            <Text key={`chip-${id}-${a.kind}`} dimColor>
              {a.title}
            </Text>
          ),
        )}
      </Box>
    ) : null

  return block(
    els,
    `plate-${id}`,
    tone,
    d.scheme,
    <Box flexDirection="column">
      {head}
      {where}
      {others}
      {chips}
      {actions}
    </Box>,
  )
}

function confirmTree($: $T, els: Els, c: CardData, p: Plate, d: Decide, hot: boolean) {
  const { Box, Text, Button } = els
  const who = d.principal ? ` (${d.principal})` : ''
  return block(
    els,
    `confirm-${c.key}`,
    'warning',
    d.scheme,
    <Box flexDirection="column">
      <Text color={headColor(d.scheme, 'warning')} bold wrap="wrap">
        {WORDS.confirmApprove} {c.key} {WORDS.andMoveTo} {p.toName}?
      </Text>
      <Text dimColor wrap="wrap">
        {WORDS.gate} «{p.gateName}» · {WORDS.transitionLower} «{p.transitionName}». {WORDS.writtenAs}
        {who}. {WORDS.agentLearns}
      </Text>
      <Box flexDirection="row" gap={1}>
        <Button key={`yes-${c.key}`} variant="primary" autoFocus hotkey={hot ? '1' : undefined} onPress={() => void confirmApprove($)}>
          {WORDS.yesApprove} <Text dimColor>{GLYPH.enter}</Text>
        </Button>
        <Button key={`cancel-${c.key}`} hotkey={hot ? '2' : undefined} onPress={() => void cancelAction($)}>
          {WORDS.cancel}
        </Button>
      </Box>
    </Box>,
  )
}

function returnTree($: $T, els: Els, c: CardData, a: Action, d: Decide) {
  const { Box, Text, Button, Input, Select } = els
  const option = c.returns.find(o => o.transition === a.target) ?? null
  const ready = !!option && !option.held && a.reason.trim() !== ''
  return block(
    els,
    `return-form-${c.key}`,
    null,
    d.scheme,
    <Box flexDirection="column">
      <Text bold>
        {WORDS.returnTitle} {c.key}
      </Text>
      <Select
        key={`target-${c.key}`}
        label={WORDS.whereHint}
        options={c.returns.map(o => ({ value: o.transition, label: `${o.name} ${GLYPH.arrow} ${o.toName}` }))}
        value={a.target ?? undefined}
        onSelect={(v: string) => void setTarget($, v)}
      />
      <Input
        key={`reason-${c.key}`}
        label={WORDS.reasonHint}
        placeholder={WORDS.reason}
        value={a.reason}
        autoFocus
        onInput={(v: string) => void setReason($, v)}
        onSubmit={(v: string) => void setReason($, v)}
      />
      {option && option.kinds.length > 0 ? (
        <Text dimColor wrap="wrap">
          {WORDS.withReturn} {option.kinds.map(k => `«${k}»`).join(', ')} {WORDS.withReturnTail}
        </Text>
      ) : null}
      {option?.held ? (
        <Text color="error" wrap="wrap">
          {cap(WORDS.holds)}: {option.held}
        </Text>
      ) : null}
      <Box flexDirection="row" gap={1} flexWrap="wrap">
        {ready ? (
          <Button key={`do-return-${c.key}`} variant="primary" onPress={() => void confirmReturn($)}>
            {WORDS.doReturn}
          </Button>
        ) : (
          <Text dimColor>{WORDS.doReturn}</Text>
        )}
        <Button key={`cancel-${c.key}`} onPress={() => void cancelAction($)}>
          {WORDS.cancel}
        </Button>
        <Text dimColor>{WORDS.returnPlan}</Text>
      </Box>
    </Box>,
  )
}

function checklistTree($: $T, els: Els, c: CardData, p: Plate, a: Action, d: Decide) {
  const { Box, Text, Button, Input } = els
  const canPass = requiredChecked(p.items, a.checked)
  const canFail = a.reason.trim() !== ''
  return block(
    els,
    `checklist-${c.key}`,
    null,
    d.scheme,
    <Box flexDirection="column">
      <Box flexDirection="row" justifyContent="space-between" gap={1}>
        <Text bold wrap="wrap">
          <Text dimColor>{c.key} </Text>
          {p.gateName}
        </Text>
        <Text dimColor>{gateWord(p.type)}</Text>
      </Box>
      <Text dimColor wrap="wrap">
        {WORDS.beforeTransition} «{p.transitionName}» {GLYPH.arrow} {p.toName}
      </Text>
      {p.items.map(it => (
        <Button key={`item-${c.key}-${it.id}`} plain onPress={() => void toggleItem($, it.id)}>
          {a.checked.includes(it.id) ? GLYPH.boxChecked : GLYPH.box} {it.text}
        </Button>
      ))}
      <Input
        key={`reason-${c.key}`}
        label={WORDS.reason}
        placeholder={WORDS.reason}
        value={a.reason}
        onInput={(v: string) => void setReason($, v)}
        onSubmit={(v: string) => void setReason($, v)}
      />
      <Box flexDirection="row" gap={1} flexWrap="wrap">
        {canPass ? (
          <Button key={`pass-${c.key}`} variant="primary" onPress={() => void confirmChecklist($, true)}>
            {WORDS.markPassed}
          </Button>
        ) : (
          <Text dimColor>{WORDS.markPassed}</Text>
        )}
        {canFail ? (
          <Button key={`fail-${c.key}`} onPress={() => void confirmChecklist($, false)}>
            {WORDS.notPassed}
          </Button>
        ) : (
          <Text dimColor>{WORDS.notPassed}</Text>
        )}
        <Button key={`cancel-${c.key}`} onPress={() => void cancelAction($)}>
          {WORDS.cancel}
        </Button>
      </Box>
      {canPass ? null : <Text dimColor>{WORDS.checkRequired}</Text>}
      <Text dimColor wrap="wrap">
        {WORDS.notPassedHint}
      </Text>
    </Box>,
  )
}

const TONE = { ok: 'success', warning: 'warning', error: 'error' } as const

// Итог: апрув поставлен — рамка; записан, но переход держит — нейтральная
// заливка; не записано — заливка error.
function outcomeTree(els: Els, c: CardData, o: Outcome, d: Decide) {
  const { Box, Text, Link } = els
  const tone: Tone | null = o.tone === 'error' ? 'error' : o.tone === 'warning' ? (o.artifacts ? 'error' : 'neutral') : null
  const color = tone && tintOf(d.scheme, tone) ? undefined : TONE[o.tone]
  return block(
    els,
    `outcome-${c.key}`,
    tone,
    d.scheme,
    <Box flexDirection="column">
      <Text color={color ?? TONE[o.tone]} bold={o.artifacts} wrap="wrap">
        {o.text}
      </Text>
      {o.artifacts ? (
        <Text dimColor wrap="wrap">
          {WORDS.staleHint}
        </Text>
      ) : null}
      {o.journal && o.tone === 'warning' && o.text.startsWith(WORDS.recorded) ? (
        <Text dimColor wrap="wrap">
          {WORDS.heldHint}
        </Text>
      ) : null}
      {o.journal ? (
        <Box flexDirection="row" flexWrap="wrap">
          <Text dimColor>{WORDS.underYourName} · </Text>
          {c.url ? <Link key={`journal-${c.key}`} href={c.url} label={`${WORDS.journal} ${GLYPH.link}`} /> : <Text dimColor>{WORDS.journal}</Text>}
          <Text dimColor>. {WORDS.silentChat}</Text>
        </Box>
      ) : null}
      {o.artifacts && c.url ? <Link key={`artifacts-${c.key}`} href={c.url} label={`${WORDS.viewArtifacts} ${GLYPH.link}`} /> : null}
    </Box>,
  )
}

function tasksBody($: $T, els: Els, c: TasksCtx) {
  const { Box, Text, Button, Input, Select } = els
  const { view, data } = c
  const searching = view.q.trim() !== ''
  const filtered = view.type !== '' || view.assignee !== '' || searching
  const mixed = mixedTypes(data.rows)

  const typeOptions = [{ value: '', label: WORDS.allTypes }, ...data.types.map(t => ({ value: t, label: t }))]
  const assigneeOptions = [
    { value: '', label: WORDS.allAssignees },
    { value: UNASSIGNED, label: WORDS.unassigned },
    ...data.handles.map(h => ({ value: h, label: h })),
  ]
  const cycle = (opts: { value: string }[], now: string) => opts[(opts.findIndex(o => o.value === now) + 1) % opts.length]?.value ?? ''

  const search = Input ? (
    <Input key="tasks-search" placeholder={WORDS.searchHint} value={view.q} submitLabel={WORDS.searchSubmit} onSubmit={(t: string) => void searchTasks($, t)} />
  ) : null

  const pick = (key: string, opts: { value: string; label: string }[], value: string, set: (v: string) => void) =>
    Select ? (
      <Select key={key} options={opts} value={value} onSelect={(v: string) => set(v)} />
    ) : (
      <Button key={key} plain onPress={() => set(cycle(opts, value))}>
        {opts.find(o => o.value === value)?.label ?? value}
      </Button>
    )

  const filters = (
    <Box flexDirection="column" gap={1}>
      {search}
      <Box flexDirection="row" gap={1} flexWrap="wrap">
        {data.types.length > 0
          ? pick('tasks-type', typeOptions, view.type, v => void chooseTasks($, { type: v }, { counts: true }))
          : null}
        {pick('tasks-assignee', assigneeOptions, view.assignee, v => void chooseTasks($, { assignee: v }, { counts: true }))}
      </Box>
      <Box flexDirection="row" gap={1} flexWrap="wrap">
        {SEGMENTS.map(id => {
          const active = !searching && view.segment === id
          const n = data.counts[id]
          return (
            <Button
              key={`seg-${id}`}
              variant={active ? 'primary' : 'secondary'}
              onPress={() => void chooseTasks($, { segment: id, q: '' })}>
              {SEGMENT_LABEL[id]}
              {n !== null ? ` ${n}` : ''}
            </Button>
          )
        })}
      </Box>
    </Box>
  )

  const from = data.total === 0 ? 0 : view.page * PAGE + 1
  const to = Math.min(data.total, (view.page + 1) * PAGE)
  const last = to >= data.total
  const caption = [
    data.total > 0 ? `${WORDS.shown} ${from}–${to} ${WORDS.of} ${data.total}` : null,
    searching ? (data.mode === 'legacy' ? WORDS.loadedOnly : WORDS.projectSearch) : null,
    data.truncated ? `${WORDS.firstN} 500` : null,
    WORDS.sortedBy,
  ].filter(Boolean)

  let body
  if (!data.loaded) {
    body = <Text dimColor>{WORDS.loading}</Text>
  } else if (data.rows.length === 0) {
    body = (
      <Box flexDirection="column">
        <Text dimColor>{view.segment === 'queue' && !filtered && !data.error ? WORDS.queueEmpty : WORDS.nothingFound}</Text>
        {filtered ? (
          <Button key="tasks-reset" plain onPress={() => void chooseTasks($, { type: '', assignee: '', q: '' }, { counts: true })}>
            {WORDS.resetFilters}
          </Button>
        ) : null}
      </Box>
    )
  } else {
    body = (
      <Box flexDirection="column" gap={1}>
        {data.rows.map(r => listRowTree($, els, r, c, mixed))}
      </Box>
    )
  }

  return (
    <Box flexDirection="column" gap={1}>
      {filters}
      {body}
      {data.error ? <Text color="error">{data.error}</Text> : null}
      {data.loaded ? (
        <Box flexDirection="row" gap={1} flexWrap="wrap">
          {view.page > 0 ? (
            <Button key="tasks-prev" plain onPress={() => void pageTasks($, -1)}>
              ‹ {WORDS.prev}
            </Button>
          ) : null}
          <Text dimColor>{caption.join(' · ')}</Text>
          {!last ? (
            <Button key="tasks-next" plain onPress={() => void pageTasks($, 1)}>
              {WORDS.next} ›
            </Button>
          ) : null}
        </Box>
      ) : null}
    </Box>
  )
}

// --- Модуль -------------------------------------------------------------------

export const register: Register = (on, options) => {
  const opts = optionsOf(options)
  let live: Session | null = null

  on('session.start', async ($, e, next) => {
    const started = await next(e)
    if (live) stop(live)
    live = null
    if (e.surface !== null || e.isInteractive) {
      live = activate($, opts)
      await registerCommand($)
    }
    sessionOf.current = live
    return started
  })

  // Поверхность появилась позже (Claude Desktop подключился к сессии).
  on('session.attach', async ($, e, next) => {
    const attached = await next(e)
    if (!live) {
      live = activate($, opts)
      await registerCommand($)
    }
    sessionOf.current = live
    return attached
  })

  on('session.end', async ($, e, next) => {
    if (live) stop(live)
    live = null
    sessionOf.current = null
    return next(e)
  })

  on('command.run', { command: 'clab-plugin' }, async ($, e) => {
    const s = live
    if (!s) return { text: 'Cadence Lab: в этой сессии мод выключен — у неё нет окна.' }
    return { text: await runCommand($, s, e.args) }
  })

  // /clab-run KEY, /clab-work KEY … — ключ ловится, промпт идёт как был.
  on('prompt.submit', async ($, e, next) => {
    const s = live
    if (s) void onPrompt($, s, e.text).catch(() => undefined)
    return next(e)
  }).catch(($, e, next) => next(e))

  // Скил clab-*, Bash с clab, тулы MCP sdlc — ключ ловится, вызов идёт как был.
  on('tool.call', async ($, e, next) => {
    const s = live
    const input = e as unknown as Record<string, unknown>
    const tool = String(e.tool)
    if (s) void beforeTool($, s, tool, input).catch(() => undefined)
    try {
      return await next(e)
    } finally {
      // И после упавшего вызова: иначе задача осталась бы «своей» навсегда.
      if (s) void afterTool($, s, tool, input).catch(() => undefined)
    }
  }).catch(($, e, next) => next(e))

  // Тема поменялась в /config — палитра схемы перерисовывается без
  // перезапуска сессии: запись в themeAtom будит читателей.
  on('config.set', { key: 'theme' }, async ($, e, next) => {
    const set = await next(e)
    if (set.deny === undefined) await update($, themeAtom, () => (typeof set.value === 'string' ? set.value : ''))
    return set
  }).catch(($, e, next) => next(e))

  // Полоса над вводом — только для задачи этого чата, ждущей апрува; одна,
  // о первой такой задаче.
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    if (!live || e.props.hasSurvey) return next(e)
    const keys = await read($, keysAtom)
    const rows = await read($, rowsAtom)
    const key = keys.find(k => rows[k]?.awaiting)
    const row = key ? rows[key] : undefined
    if (!key || !row) return next(e)
    const { Box, Text, Button } = $.ui.resolve(e)
    const gate = row.card?.approvalGate
    // Макет 06: полоса — рамка и заливка warning; в терминале — без рамки,
    // полоса там в одну строку.
    const framed = e.surface !== 'terminal'
    const scheme = schemeOf(await read($, themeAtom))
    // «Апрув» в полосе — только у того, кто вправе решать, и не на mobile:
    // нажатие раскрывает карточку с тем же подтверждением.
    const plate = row.canDecide && e.surface !== 'mobile' ? row.card?.plates.find(p => p.type === HUMAN_APPROVAL && p.state === 'pending') : undefined
    return (
      <Box
        flexDirection="row"
        gap={1}
        borderStyle={framed ? 'round' : undefined}
        borderColor={framed ? 'warning' : undefined}
        backgroundColor={framed ? tintOf(scheme, 'warning') : undefined}
        paddingX={framed ? 1 : 0}>
        <Text color="warning">{GLYPH.approval}</Text>
        <Box flexGrow={1} flexShrink={1}>
          <Text wrap="truncate-end">
            {key} {WORDS.awaitingYou}
            {gate ? ` · ${gate}` : ''}
          </Text>
        </Box>
        <Button key="band-open" onPress={() => void openCard($, key)}>
          {WORDS.open}
        </Button>
        {plate ? (
          <Button key="band-approve" variant="primary" onPress={() => void openAction($, key, plate.transition, plate.gate)}>
            {WORDS.approve}
          </Button>
        ) : null}
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const els = $.ui.resolve(e) as unknown as Els
    const { Box, Text, Button, Link } = els
    const tab = await read($, tabAtom)
    const keys = await read($, keysAtom)
    const rows = await read($, rowsAtom)
    const conn = await read($, connectionAtom)
    const open = await read($, openAtom)
    const described = await read($, describedAtom)
    const view = await read($, viewAtom)
    const scheme = schemeOf(await read($, themeAtom))
    const now = await $.clock.now()
    const example = `${conn.project ?? 'КЛЮЧ'}-12`
    const action = await read($, actionAtom)
    const outcomes = await read($, outcomesAtom)
    const appr = await read($, approvalsAtom)
    const hotkeys = { used: false }
    const principal = sessionOf.current?.principal ?? null
    const waitingMe = appr.supported && appr.canDecide ? appr.rows.length : 0

    const tabs = (
      <Box flexDirection="row" gap={1} flexWrap="wrap">
        {TABS.filter(t => t.id !== 'approvals' || appr.supported !== false).map(t => (
          <Button
            key={`tab-${t.id}`}
            variant={t.id === tab ? 'primary' : 'secondary'}
            onPress={() => {
              void update($, tabAtom, () => t.id)
              if (t.id === 'tasks') void openTasks($, sessionOf.current).catch(() => undefined)
            }}>
            {t.id === 'approvals' && waitingMe > 0 ? `${t.label} ${waitingMe}` : t.label}
          </Button>
        ))}
      </Box>
    )

    const chat =
      keys.length === 0 ? (
        <Box flexDirection="column">
          <Text dimColor>В этом чате задач пока нет.</Text>
          <Text dimColor>
            Задача появится здесь, когда в чате прозвучит её ключ: /clab-run {example}, clab show {example} — или
            вручную: /clab-plugin {example}.
          </Text>
        </Box>
      ) : (
        <Box flexDirection="column" gap={1}>
          {keys.map(key => {
            const row = rows[key]
            const head = row?.title ? `${key} ${row.title}` : key
            const status = row?.statusName ? ` · ${row.statusName}` : row ? '' : ' · загружаю…'
            const isOpen = open.includes(key) && !!row?.card
            return (
              <Box flexDirection="column" key={`row-${key}`}>
                <Box flexDirection="row" justifyContent="space-between" gap={1}>
                  <Button key={`open-${key}`} plain dimColor onPress={() => void update($, openAtom, toggle(key))}>
                    {isOpen ? GLYPH.opened : GLYPH.closed}
                  </Button>
                  <Box flexGrow={1} flexShrink={1}>
                    <Text wrap="truncate-end" bold={isOpen}>
                      {head}
                      {status}
                    </Text>
                  </Box>
                  <Button key={`remove-${key}`} plain dimColor onPress={() => void untrack($, [key])}>
                    ✕
                  </Button>
                </Box>
                {row?.awaiting ? (
                  <Text color="warning">
                    {GLYPH.approval} {WORDS.awaiting}
                    {row.card?.approvalGate ? ` · ${row.card.approvalGate}` : ''}
                  </Text>
                ) : null}
                {row?.error ? <Text color="error">Не обновилось: {row.error}</Text> : null}
                {isOpen && row?.card
                  ? cardTree($, els, e.surface, row.card, {
                      scheme,
                      now,
                      described: described.includes(key),
                      decide: { surface: e.surface, canDecide: row.canDecide, action, outcome: outcomes[key], principal, hotkeys, scheme, now },
                    })
                  : null}
              </Box>
            )
          })}
        </Box>
      )

    // «Ждут меня»: ровно ответ серверной выборки. Действие из
    // строки раскрывает карточку задачи с тем же подтверждением.
    const projectUrl = conn.host && conn.project ? `${conn.host}/p/${encodeURIComponent(conn.project)}` : null
    // Подвал «Ждут меня»: чем и когда выборка перечитана последний раз.
    const loaded = appr.loadedAt !== null ? ago(new Date(appr.loadedAt).toISOString(), now) : null
    const updated = loaded ? `${conn.events === true ? WORDS.updatedByEvent : WORDS.updatedByPoll} · ${loaded}` : null
    const approvals =
      appr.supported === false ? (
        <Text dimColor>{WORDS.olderServerApprovals}</Text>
      ) : appr.supported === null ? (
        <Text dimColor>{WORDS.loadingApprovals}</Text>
      ) : !appr.canDecide ? (
        <Text dimColor>
          {WORDS.awaitingMe}: {WORDS.needMaintainer}
        </Text>
      ) : appr.rows.length === 0 ? (
        <Text dimColor>{WORDS.nothingAwaits}</Text>
      ) : (
        <Box flexDirection="column" gap={1}>
          <Text dimColor wrap="wrap">
            {WORDS.approvalsIntro}
          </Text>
          {appr.rows.map(r => {
            const id = `${r.key}-${r.transition}-${r.gate}`
            const checklist = r.type === CHECKLIST
            return (
              <Box key={`appr-${id}`} flexDirection="column" borderStyle="round" borderDimColor paddingX={1}>
                <Box flexDirection="row" justifyContent="space-between" gap={1}>
                  <Box flexGrow={1} flexShrink={1}>
                    <Text wrap="truncate-end">
                      <Text color="warning">{GLYPH.current} </Text>
                      <Text dimColor>{r.key} </Text>
                      {r.title}
                    </Text>
                  </Box>
                  {keys.includes(r.key) ? <Text color="claude">{WORDS.inThisChat}</Text> : null}
                </Box>
                <Text wrap="wrap">
                  <Text color="warning" backgroundColor={tintOf(scheme, 'warning')}>
                    {checklist ? GLYPH.open : GLYPH.approval} {r.gateName}
                  </Text>
                  <Text dimColor>
                    {' '}
                    {r.transitionName} {GLYPH.arrow} {r.toName}
                  </Text>
                </Text>
                <Box flexDirection="row" gap={1}>
                  {e.surface !== 'mobile' ? (
                    <Button key={`appr-act-${id}`} variant="primary" onPress={() => void openAction($, r.key, r.transition, r.gate)}>
                      {checklist ? WORDS.mark : WORDS.approve}
                    </Button>
                  ) : null}
                  <Button key={`appr-open-${id}`} onPress={() => void openCard($, r.key)}>
                    {WORDS.open}
                  </Button>
                </Box>
              </Box>
            )
          })}
          {appr.truncated && projectUrl ? <Link key="appr-more" href={projectUrl} label={`${WORDS.more}… ${GLYPH.link}`} /> : null}
          {updated ? <Text dimColor>{updated}</Text> : null}
        </Box>
      )

    const viewRow = view ? rows[view.key] : undefined
    const body =
      tab === 'chat'
        ? view && viewRow?.card
          ? viewTree($, els, viewRow, view)
          : chat
        : tab === 'approvals'
          ? approvals
          : tasksBody($, els, {
              surface: e.surface,
              view: await read($, tasksViewAtom),
              data: await read($, tasksDataAtom),
              rows,
              keys,
              open,
              described,
              scheme,
              now,
              action,
              outcomes,
              principal,
              hotkeys,
            })

    const where = [
      conn.host ?? `инстанс не задан — ${LOGIN_HINT}`,
      conn.project ? `проект ${conn.project}` : null,
      conn.serverVersion ? `сервер ${conn.serverVersion}` : null,
      conn.events === true ? 'обновления по событиям' : conn.events === false ? 'опрос раз в 30 с' : null,
    ]
      .filter(Boolean)
      .join(' · ')

    return (
      <Box flexDirection="column" gap={1}>
        {tabs}
        {body}
        {conn.error ? <Text color="error">{conn.error}</Text> : null}
        {appr.error && tab === 'approvals' ? <Text color="error">{appr.error}</Text> : null}
        <Text dimColor>{where}</Text>
      </Box>
    )
  })
}
