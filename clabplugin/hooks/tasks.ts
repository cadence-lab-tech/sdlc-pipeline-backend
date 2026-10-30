// Модель вкладки «Задачи»: чистые функции над ответами сервера. Часы, сеть и
// $ — в register.tsx.
//
// Что в списке и какая страница — решает сервер, когда он умеет листать
// (GET /v1/projects/:key/tasks с offset, total, q и полями статуса); сегменты
// — это параметры запроса, а не ключи статусов. Сервер прошлого выпуска
// листать не умеет: тогда мод читает выборку целиком и делит её сам по версиям
// циклов (category, is_terminal, roles) — те же правила, тот же вид строк.
//
// Названий статусов, ролей и типов в коде нет: подписи приходят с задачами.

import type { ChildProgress, ChildState, ListKid, ListRow, Segment, TasksData, TasksView } from '../types'
import type { Workflow } from './card'

export const SEGMENTS: readonly Segment[] = ['work', 'queue', 'backlog', 'done']

// Страница списка — десять строк (ответ человека).
export const PAGE = 10
// Очередь читается одним запросом: у ручки лимит 200 и нет смещения.
export const QUEUE_LIMIT = 200
// Сервер без листания отдаёт не больше 500 задач за запрос.
export const LEGACY_LIMIT = 500
// Детей эпика на странице читаем одним запросом на все эпики страницы.
export const KIDS_LIMIT = 500
// Дети под строкой эпика свёрнуты до трёх.
export const KIDS_SHOWN = 3

export const DEFAULT_VIEW: TasksView = { segment: 'work', type: '', assignee: '', q: '', page: 0, more: [] }

export const EMPTY_DATA: TasksData = {
  loaded: false,
  error: null,
  mode: null,
  types: [],
  handles: [],
  counts: { work: null, queue: null, backlog: null, done: null },
  rows: [],
  total: 0,
  truncated: false,
  approvals: false,
}

export type ApiListTask = {
  key: string
  title: string
  status_key?: string
  status_name?: string
  workflow_id?: string
  parent_key?: string | null
  assignee_handle?: string | null
  properties?: { type?: unknown } | null
  updated_at?: string | null
  status_category?: string
  child_counts?: Record<string, number> | null
  // Признак «ждёт апрува» вводит соседняя задача про апрувы; пока сервер его не отдаёт, мод
  // не показывает ни сегмента, ни части полосы.
  awaiting_approval?: boolean
}

export type ApiList = { tasks?: ApiListTask[] | null; total?: number }

export type ApiQueueItem = {
  task: ApiListTask
  stage_run?: { claimed_by_handle?: string | null } | null
  claimable?: boolean
}

export type ApiWorkflowRow = { id?: string; state?: string; task_types?: string[] | null }

// --- Запрос ------------------------------------------------------------------

export type ListQuery = {
  segment: Segment
  type: string
  assignee: string
  q: string
  page: number
}

// Значение фильтра «исполнитель» для задач без исполнителя. Хэндл не
// начинается с «-», так что с настоящим не спутать.
export const UNASSIGNED = '-'

const SEGMENT_PARAMS: Readonly<Record<Exclude<Segment, 'queue'>, Record<string, string>>> = {
  work: { terminal: 'false', in_work: 'true' },
  backlog: { terminal: 'false', in_work: 'false' },
  done: { terminal: 'true', category: 'done' },
}

// Строка запроса страницы списка у сервера, который листает. Поиск идёт по
// всему проекту, поэтому сегмент при нём не действует. Сегмент считает все
// задачи проекта, как список задач в CLI: дети эпика — строки своих сегментов
// и ещё свёрнуты под строкой эпика (top_level не передаётся).
export function listPath(v: ListQuery, limit: number, offset: number): string {
  const p = new URLSearchParams()
  p.set('limit', String(limit))
  p.set('offset', String(offset))
  p.set('order', 'updated')
  const q = v.q.trim()
  if (q) p.set('q', q)
  else if (v.segment !== 'queue') {
    for (const [k, val] of Object.entries(SEGMENT_PARAMS[v.segment])) p.set(k, val)
  }
  if (v.type) p.set('type', v.type)
  if (v.assignee === UNASSIGNED) p.set('unassigned', 'true')
  else if (v.assignee) p.set('assignee', v.assignee)
  return `/tasks?${p.toString()}`
}

// Счётчик сегмента — тот же запрос на одну строку: нужен только total.
export const countPath = (v: ListQuery, segment: Segment) => listPath({ ...v, segment, q: '' }, 1, 0)

export const kidsPath = (parents: readonly string[]) =>
  `/tasks?parent=${encodeURIComponent(parents.join(','))}&limit=${KIDS_LIMIT}`

// --- Типы и версии циклов ----------------------------------------------------

// Типы фильтра — объединение task_types версий циклов проекта; черновик типа
// не заводит: задач у него нет.
export function typesOf(rows: readonly ApiWorkflowRow[]): string[] {
  const out = new Set<string>()
  for (const w of rows) {
    if (w.state === 'draft') continue
    for (const t of w.task_types ?? []) if (typeof t === 'string' && t) out.add(t)
  }
  return [...out].sort()
}

export type Classified = { category: string | null; terminal: boolean; hasRole: boolean }

// Статус задачи по версии её цикла. null — версии нет или статуса в ней нет.
export function classify(wf: Workflow | undefined, statusKey: string | undefined): Classified | null {
  const st = wf?.statuses?.find(s => s.key === statusKey)
  if (!st) return null
  return {
    category: typeof st.category === 'string' ? st.category : null,
    terminal: st.is_terminal === true,
    hasRole: Array.isArray(st.roles) && st.roles.length > 0,
  }
}

// Дети, с которых работа начата: в работе, ждут апрува или готовы. Неначатые
// (бэклог) и отменённые работой не считаются.
export const STARTED_CATEGORIES: readonly string[] = ['active', 'waiting', 'done']

export const startedKids = (counts: Readonly<Record<string, number>> | null | undefined) =>
  STARTED_CATEGORIES.some(c => Number(counts?.[c] ?? 0) > 0)

// Сегмент по полям статуса: «в работе» — не конечный и с ролью, либо не
// конечный с начатыми детьми (эпик, чей статус ролей не имеет, но работа по
// детям идёт); «бэклог» — не конечный, без роли и без начатых детей; «готово»
// — конечный категории done. Отменённые не входят никуда: их находит поиск.
// Те же правила у сервера (in_work).
export function segmentOf(c: Classified | null, started = false): Exclude<Segment, 'queue'> | null {
  if (!c) return null
  if (!c.terminal) return c.hasRole || started ? 'work' : 'backlog'
  return c.category === 'done' ? 'done' : null
}

// --- Строки ------------------------------------------------------------------

const typeOf = (t: ApiListTask) => (typeof t.properties?.type === 'string' && t.properties.type ? t.properties.type : null)

function stateOf(category: string | null | undefined, awaiting: boolean): ChildState {
  if (category === 'cancelled') return 'cancelled'
  if (category === 'done') return 'done'
  if (awaiting) return 'waiting'
  if (category === 'backlog') return 'backlog'
  return 'active'
}

const seqOf = (key: string) => Number(/-(\d+)$/.exec(key)?.[1] ?? 0)

export function kidOf(t: ApiListTask, category: string | null | undefined): ListKid {
  const awaiting = t.awaiting_approval === true
  return {
    key: t.key,
    title: t.title,
    statusName: String(t.status_name ?? t.status_key ?? ''),
    state: stateOf(category ?? t.status_category, awaiting),
    awaiting,
  }
}

// Полоса эпика: готово, ждёт апрува, в работе, не начато; отменённые в
// знаменатель не входят. Ждущих апрува видно только среди загруженных детей —
// и только если сервер отдаёт признак.
export function progressOf(counts: Readonly<Record<string, number>>, kids: readonly ListKid[]): ChildProgress {
  const n = (c: string) => Math.max(0, Number(counts[c] ?? 0))
  const waiting = Math.min(kids.filter(k => k.awaiting && k.state === 'waiting').length, n('active') + n('waiting'))
  const done = n('done')
  const active = n('active') + n('waiting') - waiting
  const backlog = n('backlog')
  return { done, waiting, active, backlog, cancelled: n('cancelled'), total: done + waiting + active + backlog }
}

export function nestedOf(counts: Readonly<Record<string, number>> | null | undefined, kids: readonly ListKid[]): ListRow['nested'] {
  if (!counts) return null
  const all = Object.values(counts).reduce((a, b) => a + Math.max(0, Number(b) || 0), 0)
  if (all === 0) return null
  const sorted = [...kids].sort((a, b) => seqOf(a.key) - seqOf(b.key))
  return { progress: progressOf(counts, sorted), kids: sorted, all, partial: sorted.length < all }
}

export function rowOf(
  t: ApiListTask,
  o: { category: string | null | undefined; holder: string | null; kids?: readonly ApiListTask[]; categoryOf?: (k: ApiListTask) => string | null | undefined },
): ListRow {
  const awaiting = t.awaiting_approval === true
  const kids = (o.kids ?? []).map(k => kidOf(k, o.categoryOf ? o.categoryOf(k) : k.status_category))
  return {
    key: t.key,
    title: t.title,
    type: typeOf(t),
    statusName: String(t.status_name ?? t.status_key ?? ''),
    state: stateOf(o.category, awaiting),
    awaiting,
    holder: o.holder,
    assignee: t.assignee_handle ?? null,
    updatedAt: t.updated_at ?? null,
    nested: nestedOf(t.child_counts, kids),
  }
}

// Ключ → хэндл того, кто держит аренду. Берётся из очереди: открытый прогон
// есть у каждой задачи в работе.
export function holders(queue: readonly ApiQueueItem[]): Map<string, string> {
  const out = new Map<string, string>()
  for (const it of queue) {
    const h = it.stage_run?.claimed_by_handle
    if (typeof h === 'string' && h && it.task?.key) out.set(it.task.key, h)
  }
  return out
}

// Детей по родителям: ключ родителя → дети страницы.
export function groupKids(tasks: readonly ApiListTask[]): Map<string, ApiListTask[]> {
  const out = new Map<string, ApiListTask[]>()
  for (const t of tasks) {
    if (!t.parent_key) continue
    const list = out.get(t.parent_key) ?? []
    list.push(t)
    out.set(t.parent_key, list)
  }
  return out
}

// Чип типа — только если типы в списке разные.
export const mixedTypes = (rows: readonly ListRow[]) => new Set(rows.map(r => r.type ?? '')).size > 1

// Новые сверху; без времени — в конец, по номеру.
export function byUpdated<T extends { updated_at?: string | null; key: string }>(list: readonly T[]): T[] {
  return [...list].sort((a, b) => {
    const x = a.updated_at ? Date.parse(a.updated_at) : 0
    const y = b.updated_at ? Date.parse(b.updated_at) : 0
    return y - x || seqOf(b.key) - seqOf(a.key)
  })
}

// Поиск по части ключа и по словам заголовка, без учёта регистра; текст
// короче одного символа не применяется.
export function matches(t: { key: string; title: string }, q: string): boolean {
  const needle = q.trim().toLowerCase()
  if (needle.length < 1) return true
  return t.key.toLowerCase().includes(needle) || t.title.toLowerCase().includes(needle)
}

// Ключ задачи целиком — такой запрос открывает карточку напрямую.
export const exactKey = (q: string) => /^[A-Za-z][A-Za-z0-9_]*-\d+$/.test(q.trim())

// --- Прошлый сервер ----------------------------------------------------------

export type Legacy = {
  rows: ListRow[]
  counts: Record<Exclude<Segment, 'queue'>, number>
  // Сколько строк выбрано (до страницы).
  total: number
}

// Сервер без листания: выборка целиком, версии циклов — по workflow_id.
// Сегменты, эпики и поиск считает мод по тем же правилам, что сервер.
export function legacyList(
  tasks: readonly ApiListTask[],
  workflows: ReadonlyMap<string, Workflow>,
  v: ListQuery,
  holder: ReadonlyMap<string, string>,
): Legacy {
  const info = (t: ApiListTask) => classify(workflows.get(String(t.workflow_id)), t.status_key)
  const catOf = (t: ApiListTask) => info(t)?.category ?? null
  const kids = groupKids(tasks)
  const kidCounts = (own: readonly ApiListTask[]) => {
    const out: Record<string, number> = {}
    for (const k of own) {
      const c = catOf(k) ?? 'active'
      out[c] = (out[c] ?? 0) + 1
    }
    return out
  }
  const counts = { work: 0, backlog: 0, done: 0 }
  const q = v.q.trim()
  const picked: ApiListTask[] = []
  for (const t of tasks) {
    const seg = segmentOf(info(t), startedKids(kidCounts(kids.get(t.key) ?? [])))
    if (v.type && typeOf(t) !== v.type) continue
    if (v.assignee === UNASSIGNED ? !!t.assignee_handle : v.assignee ? t.assignee_handle !== v.assignee : false) continue
    if (!q && seg) counts[seg]++
    if (q ? !matches(t, q) : seg !== v.segment) continue
    picked.push(t)
  }
  const sorted = byUpdated(picked)
  const offset = v.page * PAGE
  const rows = sorted.slice(offset, offset + PAGE).map(t => {
    const own = kids.get(t.key) ?? []
    return rowOf({ ...t, child_counts: own.length ? kidCounts(own) : null }, {
      category: catOf(t),
      holder: holder.get(t.key) ?? null,
      kids: own,
      categoryOf: catOf,
    })
  })
  return { rows, counts, total: sorted.length }
}
