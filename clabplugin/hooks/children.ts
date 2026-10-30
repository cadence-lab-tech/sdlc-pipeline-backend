// Дети в карточке родителя: полоса прогресса и строки. Задача с детьми — та
// же карточка любой задачи; состояние ребёнка — по категории его статуса и
// признаку «ждёт апрува» из полей children[] ответа сервера, подпись —
// status_name из цикла самого ребёнка. Версии циклов детей не читаются.

import type { CardChildren, ChildProgress, ChildRow, ChildState } from '../types'

export type ApiChild = {
  key: string
  title: string
  status_key?: string
  status_name?: string
  properties?: { type?: unknown } | null
  status_category?: string
  awaiting_approval?: boolean
  blocked_by?: string[] | null
}

// «после A, B +N»: до двух ключей, остальные числом.
export function afterOf(keys: readonly string[]): string | null {
  if (keys.length === 0) return null
  const shown = keys.slice(0, 2).join(', ')
  return keys.length > 2 ? `${shown} +${keys.length - 2}` : shown
}

function stateOf(c: ApiChild): ChildState {
  if (c.status_category === 'cancelled') return 'cancelled'
  if (c.status_category === 'done') return 'done'
  if (c.awaiting_approval === true) return 'waiting'
  if (c.status_category === 'backlog') return 'backlog'
  return 'active'
}

const typeOf = (c: ApiChild) => (typeof c.properties?.type === 'string' && c.properties.type ? c.properties.type : null)

export function childrenOf(list: readonly ApiChild[]): CardChildren {
  // Сервер прошлой версии полей не отдаёт: строки по status_name, без
  // полосы и без «после».
  const fresh = list.every(c => typeof c.status_category === 'string')
  const counts = new Map<string, number>()
  for (const c of list) {
    const t = typeOf(c)
    if (t) counts.set(t, (counts.get(t) ?? 0) + 1)
  }
  const major = [...counts.entries()].sort((a, b) => b[1] - a[1])[0]?.[0] ?? null

  const rows: ChildRow[] = list.map(c => {
    const type = typeOf(c)
    const state: ChildState = fresh ? stateOf(c) : 'active'
    return {
      key: c.key,
      title: c.title,
      type,
      typeChip: type !== null && type !== major,
      statusName: String(c.status_name ?? c.status_key ?? ''),
      state,
      awaiting: fresh && c.awaiting_approval === true,
      after: fresh ? afterOf(c.blocked_by ?? []) : null,
    }
  })

  let progress: ChildProgress | null = null
  if (fresh) {
    progress = { done: 0, waiting: 0, active: 0, backlog: 0, cancelled: 0, total: 0 }
    for (const r of rows) {
      progress[r.state]++
      if (r.state !== 'cancelled') progress.total++
    }
  }
  return { progress, rows }
}
