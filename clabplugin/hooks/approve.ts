// Модель действий человека по гейтам: плашки «ждёт вашего
// решения», переходы-возвраты, пункты чек-листа, снимок для сверки и план
// записи. Чистые функции над ответом GET /v1/tasks/:key и версией цикла: сеть,
// часы и $ — в register.tsx.
//
// Какие гейты закрывает человек — словарь движка (human_approval, checklist),
// а не данные проекта. Имена гейтов, переходов, статусов и виды артефактов —
// только из версии цикла задачи и ответа сервера.

import type { Plate, PlateItem, ReturnOption, Seen } from '../types'
import type { ApiGate, ApiTransition, TaskDetail, Workflow } from './card'
import { CHECKLIST, HUMAN_APPROVAL, WORDS } from './vocab'

const RESOLVED = new Set(['passed', 'waived', 'skipped'])
const isHuman = (type: unknown) => type === HUMAN_APPROVAL || type === CHECKLIST
const ARTIFACTS_REQUIRED = 'artifacts_required'

type WfGate = { key?: string; name?: string; type?: string; config?: Record<string, unknown> | null; blocking?: boolean }
type WfTransition = { key?: string; gates?: WfGate[] | null }

const statusName = (wf: Workflow, key: string | null | undefined) =>
  key ? ((wf.statuses ?? []).find(s => s.key === key)?.name ?? key) : ''

// Куда встанет задача по переходу: lands_in с выбором этапов, иначе to_status.
const landing = (t: ApiTransition) => (t.lands_in === undefined ? t.to_status : t.lands_in)

function configOf(wf: Workflow, transition: string, gate: string): Record<string, unknown> {
  const t = ((wf as { transitions?: WfTransition[] | null }).transitions ?? []).find(x => x.key === transition)
  return t?.gates?.find(g => g.key === gate)?.config ?? {}
}

// Пункты чек-листа из версии цикла; отмечен — обязательный пункт, которого
// нет среди неотмеченных в вердикте гейта.
function itemsOf(wf: Workflow, transition: string, gate: ApiGate): PlateItem[] {
  const raw = configOf(wf, transition, String(gate.gate_key ?? '')).items
  const unchecked = new Set(
    ((gate.details?.unchecked as { id?: unknown }[] | undefined) ?? []).map(u => String(u.id ?? '')),
  )
  const passed = RESOLVED.has(String(gate.status))
  return (Array.isArray(raw) ? raw : []).map(it => {
    const item = it as { id?: unknown; text?: unknown; required?: unknown }
    const id = String(item.id ?? '')
    const required = item.required === true
    return { id, text: String(item.text ?? id), required, checked: passed || (required && !unchecked.has(id)) }
  })
}

function failedReason(g: ApiGate): string | null {
  const d = g.details?.decision as { reason?: unknown } | undefined
  return typeof d?.reason === 'string' && d.reason ? d.reason : null
}

// Плашки: по одной на человеческий гейт перехода вперёд, который ждёт решения
// (pending) или отклонён (failed: человек может передумать), при том что все
// остальные блокирующие гейты прошли или тоже человеческие. Переход в
// выключенный этап плашки не даёт.
export function platesOf(detail: TaskDetail, wf: Workflow): Plate[] {
  const out: Plate[] = []
  for (const t of detail.transitions ?? []) {
    if (t.kind !== 'advance' || t.unavailable) continue
    const blocking = (t.gates ?? []).filter(g => g.blocking !== false)
    if (blocking.some(g => !isHuman(g.type) && !RESOLVED.has(String(g.status)))) continue
    for (const g of blocking) {
      if (!isHuman(g.type)) continue
      const status = String(g.status)
      const failed = status === 'failed' && g.type === HUMAN_APPROVAL
      if (status !== 'pending' && !failed) continue
      const key = String(t.key ?? '')
      out.push({
        transition: key,
        transitionName: String(t.name ?? key),
        toName: statusName(wf, landing(t)),
        gate: String(g.gate_key ?? ''),
        gateName: String(g.gate_name ?? g.gate_key ?? ''),
        type: String(g.type),
        state: failed ? 'failed' : 'pending',
        reason: failed ? failedReason(g) : g.reason ? String(g.reason) : null,
        items: g.type === CHECKLIST ? itemsOf(wf, key, g) : [],
      })
    }
  }
  return out
}

// Переходы-возвраты из текущего статуса версии цикла задачи. Недоступный
// (в выключенный этап) не предлагается. Гейт artifacts_required возврата
// закрывает сам плагин сдачей причины — виды из вердикта (missing) или из
// версии цикла; любой другой непройденный блокирующий гейт человеку из
// плагина не закрыть — он назван в held.
export function returnsOf(detail: TaskDetail, wf: Workflow): ReturnOption[] {
  const out: ReturnOption[] = []
  for (const t of detail.transitions ?? []) {
    if (t.kind !== 'reject' || t.unavailable || landing(t) === null) continue
    const key = String(t.key ?? '')
    const kinds: string[] = []
    let held: string | null = null
    for (const g of t.gates ?? []) {
      if (g.blocking === false || RESOLVED.has(String(g.status))) continue
      if (g.type === ARTIFACTS_REQUIRED) {
        const missing = g.details?.missing
        const cfg = configOf(wf, key, String(g.gate_key ?? '')).kinds
        const list = Array.isArray(missing) && missing.length ? missing : Array.isArray(cfg) ? cfg : []
        for (const k of list) if (typeof k === 'string' && !kinds.includes(k)) kinds.push(k)
        continue
      }
      if (held === null) held = [g.gate_name ?? g.gate_key, g.reason].filter(Boolean).join(' · ')
    }
    out.push({ transition: key, name: String(t.name ?? key), toName: statusName(wf, landing(t)), kinds, held })
  }
  return out
}

// Текущий прогон этапа: верхнего уровня на статусе задачи, последний.
function currentRunId(detail: TaskDetail): string | null {
  let best: { id?: string; attempt?: number } | null = null
  for (const r of detail.stage_runs ?? []) {
    if (r.parent_run_id || (r.kind !== undefined && r.kind !== 'stage') || r.status_key !== detail.task.status_key) continue
    if (!best || (r.attempt ?? 0) >= (best.attempt ?? 0)) best = r
  }
  return best?.id ?? null
}

// Артефакты текущего этапа: по ним сервер считает свежесть апрува (0042).
export function stageArtifacts(detail: TaskDetail) {
  const run = currentRunId(detail)
  return (detail.artifacts ?? []).filter(a => run !== null && a.stage_run_id === run)
}

// Снимок того, что человек видел: статус и последний артефакт этапа ("" —
// на этапе ничего не сдано). Его же сервер сверяет условием seen_artifact_id.
export function seenOf(detail: TaskDetail): Seen {
  let latest: { id?: string; created_at?: string } | null = null
  for (const a of stageArtifacts(detail)) {
    if (!latest || Date.parse(a.created_at ?? '') > Date.parse(latest.created_at ?? '')) latest = a
  }
  return { status: detail.task.status_key, artifact: latest?.id ?? '' }
}

export type Drift = 'same' | 'changed' | 'moved'

// Перечитанная задача против снимка: другой статус — задача ушла; другой
// последний артефакт этапа — результат изменился. Решение не пишется ни в
// одном из двух случаев.
export function driftOf(seen: Seen, fresh: TaskDetail): Drift {
  if (fresh.task.status_key !== seen.status) return 'moved'
  return seenOf(fresh).artifact === seen.artifact ? 'same' : 'changed'
}

// --- План записи --------------------------------------------------------------

export type Step =
  | { kind: 'artifact'; path: string; body: { kind: string; title: string; format: 'markdown'; content: string } }
  | { kind: 'decision'; path: string; body: { status: 'passed' | 'failed'; details: Record<string, unknown>; seen_artifact_id: string } }
  | { kind: 'transition'; path: string; body: { comment?: string } }

const enc = encodeURIComponent
const decisionPath = (key: string, transition: string, gate: string) =>
  `/tasks/${enc(key)}/transitions/${enc(transition)}/gates/${enc(gate)}/decision`
export const transitionPath = (key: string, transition: string) => `/tasks/${enc(key)}/transitions/${enc(transition)}`

// Апрув: решение passed; переход — отдельным шагом, только если после решения
// сервер говорит allowed (min_approvals, другой гейт).
export function approveSteps(key: string, plate: Plate, seen: Seen): { decision: Step; transition: Step } {
  return {
    decision: { kind: 'decision', path: decisionPath(key, plate.transition, plate.gate), body: { status: 'passed', details: {}, seen_artifact_id: seen.artifact } },
    transition: { kind: 'transition', path: transitionPath(key, plate.transition), body: {} },
  }
}

// Возврат: (а) артефакты, которых требуют гейты возврата, с текстом причины;
// (б) решение failed с причиной по гейту, ради которого пришли; (в) переход-
// возврат с комментарием-причиной. Условие сверки (б) — последний сданный
// здесь артефакт, его подставляет исполнитель плана.
export function returnSteps(key: string, plate: Plate, option: ReturnOption, reason: string, seen: Seen): Step[] {
  const text = reason.trim()
  const steps: Step[] = option.kinds.map(kind => ({
    kind: 'artifact',
    path: `/tasks/${enc(key)}/artifacts`,
    body: { kind, title: WORDS.returnReason, format: 'markdown', content: text },
  }))
  steps.push({ kind: 'decision', path: decisionPath(key, plate.transition, plate.gate), body: { status: 'failed', details: { reason: text }, seen_artifact_id: seen.artifact } })
  steps.push({ kind: 'transition', path: transitionPath(key, option.transition), body: { comment: text } })
  return steps
}

// Чек-лист: пройден — passed с отмеченными пунктами; не пройден — failed с
// причиной. Переход чек-лист не делает: его запрашивает тот, кто ведёт этап.
export function checklistStep(key: string, plate: Plate, seen: Seen, pass: { checked: string[] } | { reason: string }): Step {
  const body = 'checked' in pass ? { status: 'passed' as const, details: { checked: pass.checked } } : { status: 'failed' as const, details: { reason: pass.reason.trim() } }
  return { kind: 'decision', path: decisionPath(key, plate.transition, plate.gate), body: { ...body, seen_artifact_id: seen.artifact } }
}

// Можно ли отметить чек-лист пройденным: все обязательные пункты отмечены.
export const requiredChecked = (items: PlateItem[], checked: string[]) => items.every(i => !i.required || checked.includes(i.id))

// Что держит переход после записанного решения: непройденные блокирующие
// гейты перехода с их причинами («апрувов 1 из 2»).
export function holdsOf(t: ApiTransition | undefined): string {
  return (t?.gates ?? [])
    .filter(g => g.blocking !== false && !RESOLVED.has(String(g.status)))
    .map(g => [g.gate_name ?? g.gate_key, g.reason].filter(Boolean).join(' · '))
    .join('; ')
}

// Может ли участник решать гейты человека: уровень из GET /projects/:key.
// viewer и agent — нет; пусто (режим без прав) и неизвестно — да, а отказ
// сервера (403) всё равно обрабатывается.
export const canDecideAt = (level: string | null | undefined) => level !== 'viewer' && level !== 'agent'

// Элемент выборки «Ждут меня» в том виде, в каком его рисует вкладка.
export type ApiApproval = {
  task?: { key?: string; title?: string; status_name?: string }
  transition?: { key?: string; name?: string; lands_in?: string; lands_in_name?: string }
  gate?: { gate_key?: string; gate_name?: string; type?: string }
}

export function approvalRowsOf(list: ApiApproval[] | null | undefined) {
  return (list ?? []).map(a => ({
    key: String(a.task?.key ?? ''),
    title: String(a.task?.title ?? ''),
    transition: String(a.transition?.key ?? ''),
    transitionName: String(a.transition?.name ?? a.transition?.key ?? ''),
    toName: String(a.transition?.lands_in_name ?? a.transition?.lands_in ?? ''),
    gate: String(a.gate?.gate_key ?? ''),
    gateName: String(a.gate?.gate_name ?? a.gate?.gate_key ?? ''),
    type: String(a.gate?.type ?? ''),
  }))
}
