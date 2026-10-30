// Палитры схемы цикла. Box и Text красятся ключами темы Claude и следуют ей
// сами; Svg рисуется изображением, и его цвета — забота мода: палитра
// выбирается по ряду theme из /config (решение 0067).

export type Palette = {
  bg: string
  text: string
  dim: string
  faint: string
  ok: string
  cur: string
  wait: string
  err: string
  line: string
}

// Подписи узлов — text, dim и faint: у каждого контраст к фону карточки не
// ниже 4.5:1 (проверяет тест). Текущий узел и «ждёт апрува» — разные цвета.
export const LIGHT: Palette = {
  bg: '#ffffff',
  text: '#1f1e1d',
  dim: '#5f5d57',
  faint: '#73716a',
  ok: '#3d7a4c',
  cur: '#c96442',
  wait: '#a8670f',
  err: '#b4423a',
  line: '#d6d3c8',
}

export const DARK: Palette = {
  bg: '#262624',
  text: '#ece9e1',
  dim: '#b3b0a7',
  faint: '#9a978e',
  ok: '#82b78c',
  cur: '#d97757',
  wait: '#e3a64f',
  err: '#e27b70',
  line: '#4a4844',
}

export type Scheme = 'light' | 'dark' | 'auto'

// Ряд theme: dark, dark-daltonized, dark-ansi — тёмная; light* — светлая;
// auto и всё незнакомое — по prefers-color-scheme внутри самого SVG.
export function schemeOf(theme: string | null | undefined): Scheme {
  const t = String(theme ?? '').toLowerCase()
  if (t.startsWith('dark')) return 'dark'
  if (t.startsWith('light')) return 'light'
  return 'auto'
}

const vars = (p: Palette) =>
  Object.entries(p)
    .map(([k, v]) => `--${k}:${v}`)
    .join(';')

// Стиль внутри SVG: цвета — переменные CSS. При явной теме — одна палитра;
// при auto — светлая и тёмная по prefers-color-scheme.
export function svgStyle(scheme: Scheme): string {
  if (scheme === 'dark') return `svg{${vars(DARK)}}`
  if (scheme === 'light') return `svg{${vars(LIGHT)}}`
  return `svg{${vars(LIGHT)}}@media (prefers-color-scheme: dark){svg{${vars(DARK)}}}`
}

// Относительная яркость и контраст по WCAG 2.
function luminance(hex: string): number {
  const h = hex.replace('#', '')
  const c = [0, 2, 4].map(i => parseInt(h.slice(i, i + 2), 16) / 255).map(x => (x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4))
  return 0.2126 * (c[0] ?? 0) + 0.7152 * (c[1] ?? 0) + 0.0722 * (c[2] ?? 0)
}

export function contrast(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)]
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)
}
