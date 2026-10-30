// Модель карточки задачи из двух ответов: GET /v1/tasks/:key и версии цикла
// GET /v1/workflows/:id. Чистые функции: часы, сеть и $ — в register.tsx.
//
// Карточка одна на любую задачу любого цикла. Особые случаи — по данным:
// задача с детьми получает строки детей, апрув — гейт типа human_approval на
// любом переходе вперёд. Ни ключей статусов, ни ролей, ни типов задач код не
// сравнивает: узлы, подписи и порядок приходят из версии цикла.

import type { CardData, CardGate, CardGates, CycleNode, GateMark } from '../types'
import { childrenOf } from './children'
import type { ApiChild } from './children'
import { HUMAN_APPROVAL } from './vocab'

export type ApiStatus = {
  key: string
  name: string
  category?: string
  position?: number
  is_initial?: boolean
  is_terminal?: boolean
  is_optional?: boolean
  enabled_by_default?: boolean
  roles?: unknown[] | null
}

export type Workflow = { id: string; name?: string; version?: number; statuses: ApiStatus[] }

export type ApiGate = { gate_key?: string; gate_name?: string; type?: string; status?: string; blocking?: boolean; reason?: string | null }

export type ApiTransition = { key?: string; name?: string; kind?: string; to_status?: string; gates?: ApiGate[] }

export type ApiRun = {
  id?: string
  status_key?: string
  kind?: string
  attempt?: number
  parent_run_id?: string | null
  claimed_by_handle?: string | null
  lease_expires_at?: string | null
  rejected_from_run_id?: string | null
  started_at?: string | null
  finished_at?: string | null
  exit?: { comment?: string | null; at?: string | null } | null
}

export type TaskDetail = {
  task: {
    key: string
    title: string
    status_key: string
    status_name?: string
    workflow_id: string
    description?: string | null
    priority?: string | null
    project_key?: string
    properties?: { type?: unknown } | null
    optional_stages?: Record<string, unknown> | null
  }
  stage_runs?: ApiRun[] | null
  transitions?: ApiTransition[] | null
  artifacts?: { title?: string; kind?: string; version?: number; stage_run_id?: string | null; created_at?: string }[] | null
  attachments?: { title?: string; file_name?: string; size_bytes?: number; added_at?: string }[] | null
  children?: ApiChild[] | null
  usage?: { cost_usd?: number | null } | null
  repo_refs?: { ref_type?: string; ref?: string; url?: string | null }[] | null
}

const RESOLVED = new Set(['passed', 'waived', 'skipped'])

const isTop = (r: ApiRun) => !r.parent_run_id && (r.kind === undefined || r.kind === 'stage')

// «Ждёт апрува» у одного перехода: блокирующий human_approval не прошёл, все
// остальные блокирующие прошли. То же правило, что у сервера
// (workflow.AwaitsApproval) и карточки интерфейса.
export function gatesAwaitApproval(gates: ApiGate[]): boolean {
  const blocking = gates.filter(g => g.blocking)
  const approvals = blocking.filter(g => g.type === HUMAN_APPROVAL)
  const others = blocking.filter(g => g.type !== HUMAN_APPROVAL)
  return approvals.some(g => !RESOLVED.has(String(g.status))) && others.every(g => RESOLVED.has(String(g.status)))
}

export function awaitsApproval(transitions: ApiTransition[] | null | undefined): boolean {
  return (transitions ?? []).some(t => t.kind === 'advance' && gatesAwaitApproval(t.gates ?? []))
}

// Переход, чьи гейты показывает карточка: вперёд и ждущий апрува, иначе
// первый вперёд с гейтами, иначе первый вперёд.
function forward(transitions: ApiTransition[]): ApiTransition | null {
  const adv = transitions.filter(t => t.kind === 'advance')
  return adv.find(t => gatesAwaitApproval(t.gates ?? [])) ?? adv.find(t => (t.gates ?? []).length > 0) ?? adv[0] ?? null
}

function markOf(g: ApiGate, awaiting: boolean): GateMark {
  const s = String(g.status)
  if (s === 'passed' || s === 'waived') return 'ok'
  if (s === 'skipped') return 'skip'
  if (s === 'failed') return 'fail'
  if (g.type === HUMAN_APPROVAL && awaiting) return 'approval'
  return 'open'
}

function gatesOf(t: ApiTransition | null): CardGates | null {
  if (!t) return null
  const raw = t.gates ?? []
  const awaiting = gatesAwaitApproval(raw)
  const items: CardGate[] = raw.map(g => ({
    name: String(g.gate_name ?? g.gate_key ?? ''),
    type: String(g.type ?? ''),
    status: String(g.status ?? ''),
    reason: g.reason ? String(g.reason) : null,
    blocking: g.blocking !== false,
    mark: markOf(g, awaiting),
  }))
  const blocking = items.filter(g => g.blocking)
  return {
    transition: String(t.name ?? t.key ?? ''),
    items,
    passed: blocking.filter(g => RESOLVED.has(g.status)).length,
    total: blocking.length,
  }
}

// Включён ли необязательный этап у задачи: выбор задачи, иначе умолчание
// версии цикла (решение 0066). Обязательный — всегда включён.
function enabled(s: ApiStatus, choice: Record<string, unknown> | null | undefined): boolean {
  if (!s.is_optional) return true
  const own = choice?.[s.key]
  return typeof own === 'boolean' ? own : s.enabled_by_default !== false
}

// Узлы схемы: статусы с ролью и терминальные, по position. Начальный без
// роли не рисуется; терминальный категории cancelled — только если задача в
// нём.
export function nodesOf(detail: TaskDetail, wf: Workflow, awaiting: boolean): CycleNode[] {
  const current = detail.task.status_key
  const statuses = [...(wf.statuses ?? [])]
    .filter(s => (s.roles?.length ?? 0) > 0 || s.is_terminal === true)
    .filter(s => s.category !== 'cancelled' || s.key === current)
    .sort((a, b) => (a.position ?? 0) - (b.position ?? 0))
  const runs = (detail.stage_runs ?? []).filter(isTop)
  const cur = statuses.findIndex(s => s.key === current)
  return statuses.map((s, i) => {
    const own = runs.filter(r => r.status_key === s.key)
    const finished = own.some(r => !!r.finished_at)
    let state: CycleNode['state']
    if (i === cur) state = awaiting ? 'waiting' : 'current'
    else if (finished && (cur < 0 || i < cur)) state = 'done'
    else if (!enabled(s, detail.task.optional_stages)) state = 'skipped'
    else state = 'future'
    return { key: s.key, name: s.name, state, attempts: own.length }
  })
}

// Текущий прогон: верхнего уровня на статусе задачи, последний по попытке.
function currentRun(detail: TaskDetail): ApiRun | null {
  const own = (detail.stage_runs ?? []).filter(r => isTop(r) && r.status_key === detail.task.status_key)
  return own.reduce<ApiRun | null>((a, r) => (!a || (r.attempt ?? 0) >= (a.attempt ?? 0) ? r : a), null)
}

export const typeOf = (detail: { properties?: { type?: unknown } | null }) => {
  const t = detail.properties?.type
  return typeof t === 'string' && t ? t : null
}

export function cardOf(detail: TaskDetail, wf: Workflow, host: string | null): CardData {
  const t = detail.task
  const transitions = detail.transitions ?? []
  const awaiting = awaitsApproval(transitions)
  const nodes = nodesOf(detail, wf, awaiting)
  const names = new Map((wf.statuses ?? []).map(s => [s.key, s.name]))
  const run = currentRun(detail)
  const runs = detail.stage_runs ?? []

  let back: CardData['back'] = null
  if (run?.rejected_from_run_id) {
    const src = runs.find(r => r.id === run.rejected_from_run_id)
    if (src) {
      const from = nodes.findIndex(n => n.key === src.status_key)
      const to = nodes.findIndex(n => n.key === t.status_key)
      back = {
        from: from < 0 ? null : from,
        to: to < 0 ? null : to,
        fromName: names.get(String(src.status_key)) ?? String(src.status_key ?? ''),
        reason: src.exit?.comment ? String(src.exit.comment) : null,
        at: src.exit?.at ?? src.finished_at ?? null,
      }
    }
  }

  const gates = gatesOf(forward(transitions))
  const approvalGate = awaiting
    ? (transitions
        .filter(x => x.kind === 'advance' && gatesAwaitApproval(x.gates ?? []))
        .flatMap(x => x.gates ?? [])
        .find(g => g.type === HUMAN_APPROVAL && !RESOLVED.has(String(g.status)))?.gate_name ?? null)
    : null

  const stageOf = new Map(runs.map(r => [r.id, r.status_key]))
  const live = run && !run.finished_at ? run : null
  const step = nodes.findIndex(n => n.key === t.status_key) + 1
  const cost = typeof detail.usage?.cost_usd === 'number' && detail.usage.cost_usd > 0 ? detail.usage.cost_usd : null
  const project = t.project_key || t.key.replace(/-\d+$/, '')

  return {
    key: t.key,
    title: t.title,
    type: typeOf(t),
    priority: t.priority ?? null,
    statusName: names.get(t.status_key) ?? t.status_name ?? t.status_key,
    workflowName: wf.name ?? null,
    workflowVersion: typeof wf.version === 'number' ? wf.version : null,
    nodes,
    step,
    attempt: run?.attempt ?? 1,
    holder: live?.claimed_by_handle ?? null,
    leaseUntil: live?.lease_expires_at ?? null,
    back,
    gates,
    awaiting,
    approvalGate: approvalGate ? String(approvalGate) : null,
    artifacts: (detail.artifacts ?? []).map(a => {
      const st = a.stage_run_id ? stageOf.get(a.stage_run_id) : undefined
      return {
        title: String(a.title ?? a.kind ?? ''),
        version: typeof a.version === 'number' ? a.version : null,
        stage: st && st !== t.status_key ? (names.get(String(st)) ?? String(st)) : null,
        at: a.created_at ?? null,
      }
    }),
    attachments: (detail.attachments ?? []).map(a => ({
      name: String(a.file_name ?? a.title ?? ''),
      size: typeof a.size_bytes === 'number' ? a.size_bytes : null,
      at: a.added_at ?? null,
    })),
    cost,
    prs: (detail.repo_refs ?? [])
      .filter(r => r.ref_type === 'pull_request' && r.url)
      .map(r => ({ label: `#${r.ref}`, url: String(r.url) })),
    description: t.description ? String(t.description) : null,
    children: (detail.children ?? []).length > 0 ? childrenOf(detail.children ?? []) : null,
    url: host ? `${host}/p/${encodeURIComponent(project)}/tasks/${encodeURIComponent(t.key)}` : null,
  }
}

// --- Слова, зависящие от часов ------------------------------------------------

// Давность: «только что», «N мин», «N ч», «вчера», «N дн».
export function ago(at: string | null, now: number): string | null {
  if (!at) return null
  const t = Date.parse(at)
  if (Number.isNaN(t)) return null
  const min = Math.floor((now - t) / 60_000)
  if (min < 1) return 'только что'
  if (min < 60) return `${min} мин`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h} ч`
  const d = Math.floor(h / 24)
  return d === 1 ? 'вчера' : `${d} дн`
}

// Остаток аренды в минутах; null — аренды нет или она истекла.
export function leaseLeft(until: string | null, now: number): number | null {
  if (!until) return null
  const t = Date.parse(until)
  if (Number.isNaN(t) || t <= now) return null
  return Math.max(1, Math.ceil((t - now) / 60_000))
}

export function sizeOf(bytes: number | null): string | null {
  if (bytes === null) return null
  if (bytes < 1024) return `${bytes} Б`
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} КБ`
  return `${(bytes / 1024 / 1024).toFixed(1)} МБ`
}

export const money = (usd: number) => `$${usd.toFixed(2)}`

// Строка под схемой: «<цикл> v<N> · этап n из m · попытка k · держит … ·
// аренда ещё N мин» или «никто не держит».
export function metaLine(c: CardData, now: number): string {
  const bits: string[] = []
  if (c.workflowName) bits.push(c.workflowVersion !== null ? `${c.workflowName} v${c.workflowVersion}` : c.workflowName)
  if (c.nodes.length > 0) bits.push(`этап ${c.step} из ${c.nodes.length}`)
  if (c.attempt > 1) bits.push(`попытка ${c.attempt}`)
  const left = leaseLeft(c.leaseUntil, now)
  if (c.holder && left !== null) bits.push(`держит ${c.holder} · аренда ещё ${left} мин`)
  else bits.push('никто не держит')
  return bits.join(' · ')
}

export function backLine(c: CardData, now: number): string | null {
  if (!c.back) return null
  const when = ago(c.back.at, now)
  const head = when && when !== 'только что' && when !== 'вчера' ? `Вернули из ${c.back.fromName} ${when} назад` : `Вернули из ${c.back.fromName}${when ? ` ${when}` : ''}`
  return c.back.reason ? `${head}: «${c.back.reason}»` : head
}
