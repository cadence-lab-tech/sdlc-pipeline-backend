// Схема цикла в терминале — одна строка Text (в терминале нет Svg). До 6
// узлов — все; больше — «✓ пройдено n», текущий, следующий, «… ещё n»,
// последний. Цвет — ключ темы Claude, терминал красит его сам.

import type { CycleNode } from '../types'

export type Segment = { text: string; color?: 'success' | 'warning' | 'claude'; dim?: boolean; strike?: boolean }

const SEP: Segment = { text: ' ─ ', dim: true }

function one(n: CycleNode): Segment {
  const x = n.attempts > 1 ? ` ×${n.attempts}` : ''
  switch (n.state) {
    case 'done':
      return { text: `✓ ${n.name}${x}`, color: 'success' }
    case 'waiting':
      return { text: `⏸ ${n.name}${x}`, color: 'warning' }
    case 'current':
      return { text: `● ${n.name}${x}`, color: 'claude' }
    case 'skipped':
      return { text: `◌ ${n.name}`, dim: true, strike: true }
    default:
      return { text: `○ ${n.name}${x}`, dim: true }
  }
}

export function cycleLine(nodes: readonly CycleNode[]): Segment[] {
  const parts: Segment[] = []
  if (nodes.length <= 6) {
    nodes.forEach(n => parts.push(one(n)))
  } else {
    const found = nodes.findIndex(n => n.state === 'current' || n.state === 'waiting')
    // Статуса задачи на схеме нет (начальный без роли) — счёт с начала.
    const cur = found < 0 ? -1 : found
    const last = nodes.length - 1
    if (cur > 1) parts.push({ text: `✓ пройдено ${cur}`, color: 'success' })
    else if (cur === 1 && nodes[0]) parts.push(one(nodes[0]))
    const shown: number[] = []
    if (cur >= 0) shown.push(cur)
    if (cur + 1 < last) shown.push(cur + 1)
    for (const i of shown) {
      const node = nodes[i]
      if (node) parts.push(one(node))
    }
    const tailFrom = (shown.at(-1) ?? -1) + 1
    const rest = last - tailFrom
    if (rest > 0) parts.push({ text: `… ещё ${rest}`, dim: true })
    if (cur !== last && nodes[last]) parts.push(one(nodes[last]))
  }
  const out: Segment[] = []
  parts.forEach((p, i) => {
    if (i > 0) out.push(SEP)
    out.push(p)
  })
  return out
}

export const lineText = (segments: readonly Segment[]) => segments.map(s => s.text).join('')
