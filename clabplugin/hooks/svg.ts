// Схема цикла в Svg (Desktop, VS Code, mobile): узлы версии цикла по
// порядку, линия между ними, дуга фактического возврата. Число узлов не
// зашито: до 6 узлов — подпись у каждого, больше — только у текущего,
// остальные точки (номер этапа и число этапов — в строке под схемой).
//
// Ширина подписи считается по таблице символов, а не браузером: тест
// проверяет, что ни одна подпись не выходит за viewBox.

import type { CardBack, CycleNode } from '../types'
import { svgStyle } from './theme'
import type { Scheme } from './theme'

export const WIDTH = 372
const COMPACT_AFTER = 6
const FONT = 10.5

// Ширина символа в пикселях при размере шрифта 10.5: широкие (CJK, эмодзи)
// — квадрат, остальные — средняя ширина системного шрифта.
function charWidth(ch: string, bold: boolean): number {
  const cp = ch.codePointAt(0) ?? 0
  if (cp >= 0x1100 && (cp <= 0x115f || (cp >= 0x2e80 && cp <= 0xa4cf) || (cp >= 0xac00 && cp <= 0xd7a3) || (cp >= 0xf900 && cp <= 0xfaff) || (cp >= 0xfe30 && cp <= 0xfe4f) || (cp >= 0xff00 && cp <= 0xff60) || (cp >= 0xffe0 && cp <= 0xffe6) || cp >= 0x1f300)) return FONT
  return bold ? 6.4 : 5.9
}

export function textWidth(s: string, bold = false): number {
  let w = 0
  for (const ch of s) w += charWidth(ch, bold)
  return w
}

// Обрезка по ширине с «…».
export function fit(s: string, max: number, bold = false): string {
  if (textWidth(s, bold) <= max) return s
  const ell = textWidth('…', bold)
  let out = ''
  let w = 0
  for (const ch of s) {
    const cw = charWidth(ch, bold)
    if (w + cw + ell > max) break
    out += ch
    w += cw
  }
  return (out || [...s][0] || '') + '…'
}

export type Label = { text: string; x: number; anchor: 'start' | 'middle' | 'end'; width: number; bold: boolean; node: number }

export type Layout = { width: number; height: number; cy: number; r: number; xs: number[]; labels: Label[]; compact: boolean }

const active = (n: CycleNode) => n.state === 'current' || n.state === 'waiting'

export function layout(nodes: readonly CycleNode[], back: CardBack | null, width = WIDTH): Layout {
  const n = nodes.length
  const compact = n > COMPACT_AFTER
  const top = back && back.from !== null && back.to !== null ? 22 : 10
  const r = compact ? 5 : 6
  const pad = compact ? 14 : n <= 3 ? 40 : 26
  const gap = (width - pad * 2) / Math.max(1, n - 1)
  const xs = nodes.map((_, i) => (n === 1 ? width / 2 : pad + i * gap))
  const cy = top + 8
  const labels: Label[] = []
  nodes.forEach((node, i) => {
    const on = active(node)
    if (compact && !on) return
    const suffix = node.attempts > 1 ? ` ×${node.attempts}` : ''
    // Место подписи: слот между узлами (у крайних — до края), у текущего в
    // плотной схеме — вся строка.
    const slot = compact ? width - 12 : i === 0 || i === n - 1 ? pad * 2 + gap / 2 - 4 : gap - 4
    const room = Math.min(width - 4, on && !compact ? slot * 1.6 : slot)
    const text = fit(node.name, room - textWidth(suffix, on), on) + suffix
    const w = textWidth(text, on)
    let x = xs[i] ?? 0
    let anchor: Label['anchor'] = 'middle'
    if (x - w / 2 < 2) {
      x = 2
      anchor = 'start'
    } else if (x + w / 2 > width - 2) {
      x = width - 2
      anchor = 'end'
    }
    labels.push({ text, x, anchor, width: w, bold: on, node: i })
  })
  return { width, height: cy + 28, cy, r, xs, labels, compact }
}

const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')

// Разметка SVG. Цвета — переменные CSS из svgStyle: при явной теме Claude —
// её палитра, при auto — prefers-color-scheme внутри картинки.
export function cycleSvg(nodes: readonly CycleNode[], back: CardBack | null, scheme: Scheme, width = WIDTH): string {
  const L = layout(nodes, back, width)
  const { cy, r, xs } = L
  const parts: string[] = []
  for (let i = 0; i < nodes.length - 1; i++) {
    const a = nodes[i]
    const b = nodes[i + 1]
    if (!a || !b) continue
    const dash = a.state === 'skipped' || b.state === 'skipped' ? ' stroke-dasharray="3 3"' : ''
    const color = a.state === 'done' ? 'var(--ok)' : 'var(--line)'
    parts.push(`<line x1="${(xs[i] ?? 0) + r + 2}" y1="${cy}" x2="${(xs[i + 1] ?? 0) - r - 2}" y2="${cy}" style="stroke:${color}" stroke-width="1.5"${dash}/>`)
  }
  if (back && back.from !== null && back.to !== null && back.from !== back.to) {
    const a = xs[back.from] ?? 0
    const b = xs[back.to] ?? 0
    const dir = b < a ? 3 : -3
    parts.push(`<path d="M ${a} ${cy - r - 2} C ${a} ${cy - 20}, ${b} ${cy - 20}, ${b + dir} ${cy - r - 3}" fill="none" style="stroke:var(--err)" stroke-width="1.2" opacity=".8"/>`)
    parts.push(`<path d="M ${b + dir} ${cy - r - 3} l ${dir > 0 ? 4 : -4} -1 M ${b + dir} ${cy - r - 3} l ${dir > 0 ? -1 : 1} -4" style="stroke:var(--err)" stroke-width="1.2" opacity=".8"/>`)
  }
  nodes.forEach((node, i) => {
    const x = xs[i] ?? 0
    if (node.state === 'done') {
      parts.push(`<circle cx="${x}" cy="${cy}" r="${r}" style="fill:var(--ok)"/>`)
      parts.push(`<path d="M ${x - 3} ${cy} l 2 2.2 l 4 -4.4" fill="none" style="stroke:var(--bg)" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>`)
    } else if (active(node)) {
      const col = node.state === 'waiting' ? 'var(--wait)' : 'var(--cur)'
      parts.push(`<circle cx="${x}" cy="${cy}" r="${r + 2.5}" fill="none" style="stroke:${col}" stroke-width="1.2" opacity=".45"/>`)
      parts.push(`<circle cx="${x}" cy="${cy}" r="${r}" style="fill:${col}"/>`)
      if (node.state === 'waiting') {
        parts.push(`<rect x="${x - 2.4}" y="${cy - 2.6}" width="1.7" height="5.2" style="fill:var(--bg)"/><rect x="${x + 0.7}" y="${cy - 2.6}" width="1.7" height="5.2" style="fill:var(--bg)"/>`)
      }
    } else if (node.state === 'skipped') {
      parts.push(`<circle cx="${x}" cy="${cy}" r="${r - 0.5}" fill="none" style="stroke:var(--faint)" stroke-width="1.2" stroke-dasharray="2 2"/>`)
    } else {
      parts.push(`<circle cx="${x}" cy="${cy}" r="${r - 0.5}" fill="none" style="stroke:var(--faint)" stroke-width="1.4"/>`)
    }
  })
  for (const l of L.labels) {
    const node = nodes[l.node]
    const color = l.bold ? 'var(--text)' : node?.state === 'done' ? 'var(--dim)' : 'var(--faint)'
    const deco = node?.state === 'skipped' ? ' text-decoration="line-through"' : ''
    parts.push(`<text x="${l.x}" y="${cy + 21}" text-anchor="${l.anchor}" font-size="${FONT}" font-weight="${l.bold ? 600 : 400}" style="fill:${color}"${deco} font-family="-apple-system,system-ui,sans-serif">${esc(l.text)}</text>`)
  }
  return `<svg width="${L.width}" height="${L.height}" viewBox="0 0 ${L.width} ${L.height}" xmlns="http://www.w3.org/2000/svg" role="img"><style>${svgStyle(scheme)}</style>${parts.join('')}</svg>`
}

// Подпись для программ чтения с экрана: те же имена и состояния, что видно.
export function cycleAlt(nodes: readonly CycleNode[]): string {
  return nodes.map(n => `${n.state === 'done' ? '✓' : n.state === 'waiting' ? '⏸' : n.state === 'current' ? '●' : n.state === 'skipped' ? '◌' : '○'} ${n.name}${n.attempts > 1 ? ` ×${n.attempts}` : ''}`).join(' → ')
}
