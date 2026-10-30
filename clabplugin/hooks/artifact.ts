// Просмотр артефактов этапа: вкладки по прогонам, чипы по kind, версии по
// supersedes_id, предел отрисовки, вложения. Чистые функции: сеть, часы и $ —
// в register.tsx.
//
// Ни ключей статусов, ни ролей, ни видов артефактов код не сравнивает:
// вкладки — статусы версии цикла задачи, на которых были прогоны, чипы — kind
// из ответа сервера, подписи — данные.

import type { CardData, RunRow, StageTab } from '../types'
import type { ApiRun, TaskDetail, Workflow } from './card'
import { MONTHS } from './vocab'

// Ответ GET /v1/tasks/:key/artifacts: артефакты этапов, без вложений.
export type ApiArtifact = {
  id: string
  kind: string
  title?: string
  format?: string
  version?: number
  status?: string
  supersedes_id?: string | null
  stage_run_id?: string | null
  size_bytes?: number
  file_name?: string | null
  media_type?: string | null
  created_at?: string
}

// Ответ GET /v1/tasks/:key/attachments.
export type ApiAttachment = {
  id: string
  title?: string
  file_name?: string
  media_type?: string
  size_bytes?: number
  version?: number
  added_by?: string
  added_at?: string
  content_path?: string
}

export type TaskFiles = { artifacts: ApiArtifact[]; attachments: ApiAttachment[] }

// Предел отрисовки длинного артефакта: что наступит раньше, 60 строк или
// 8 КБ. «В чат» и «Копировать» берут полный текст.
export const MAX_LINES = 60
export const MAX_BYTES = 8 * 1024

const top = (r: ApiRun) => !r.parent_run_id && (r.kind === undefined || r.kind === 'stage')

// --- Прогоны и вкладки ----------------------------------------------------------

function runRow(r: ApiRun, runs: readonly ApiRun[], names: Map<string, string>): RunRow {
  let back: RunRow['back'] = null
  if (r.rejected_from_run_id) {
    const src = runs.find(x => x.id === r.rejected_from_run_id)
    if (src) {
      back = {
        fromName: names.get(String(src.status_key)) ?? String(src.status_key ?? ''),
        reason: src.exit?.comment ? String(src.exit.comment) : null,
      }
    }
  }
  const reviews = runs
    .filter(x => x.parent_run_id === r.id)
    .sort((a, b) => Date.parse(a.entered_at ?? '') - Date.parse(b.entered_at ?? '') || (a.attempt ?? 0) - (b.attempt ?? 0))
    .map(x => runRow(x, runs, names))
  return {
    id: String(r.id ?? ''),
    attempt: r.attempt ?? 1,
    role: r.role_key ? String(r.role_key) : null,
    holder: r.claimed_by_handle ? String(r.claimed_by_handle) : null,
    from: r.entered_at ?? null,
    to: r.finished_at ?? null,
    running: !r.finished_at,
    back,
    reviews,
  }
}

// Вкладки этапов: статусы версии цикла с ролью, на которых были прогоны
// верхнего уровня. Начальный статус без роли, непройденные и пропущенные
// необязательные этапы вкладок не получают.
export function stageTabs(detail: TaskDetail, wf: Workflow): StageTab[] {
  const runs = detail.stage_runs ?? []
  const names = new Map((wf.statuses ?? []).map(s => [s.key, s.name]))
  const tops = runs.filter(top)
  return [...(wf.statuses ?? [])]
    .filter(s => (s.roles?.length ?? 0) > 0)
    .sort((a, b) => (a.position ?? 0) - (b.position ?? 0))
    .flatMap(s => {
      const own = tops.filter(r => r.status_key === s.key).sort((a, b) => (a.attempt ?? 0) - (b.attempt ?? 0))
      return own.length === 0 ? [] : [{ key: s.key, name: s.name, attempts: own.length, runs: own.map(r => runRow(r, runs, names)) }]
    })
}

export function runStagesOf(detail: TaskDetail): Record<string, string> {
  const out: Record<string, string> = {}
  for (const r of detail.stage_runs ?? []) if (r.id && r.status_key) out[r.id] = r.status_key
  return out
}

const two = (n: number) => String(n).padStart(2, '0')
const day = (d: Date) => `${d.getDate()} ${MONTHS[d.getMonth()] ?? ''}`
const hm = (d: Date) => `${two(d.getHours())}:${two(d.getMinutes())}`

// «29 окт, 18:40–19:05»; на разных сутках — обе даты; без конца — только
// начало (идёт ли прогон, подписывает вызывающий).
export function whenRange(from: string | null, to: string | null): string | null {
  const a = from ? new Date(from) : null
  if (!a || Number.isNaN(a.getTime())) return null
  const head = `${day(a)}, ${hm(a)}`
  const b = to ? new Date(to) : null
  if (!b || Number.isNaN(b.getTime())) return head
  return day(a) === day(b) ? `${head}–${hm(b)}` : `${head} – ${day(b)}, ${hm(b)}`
}

// Вкладка по умолчанию: этап, на котором задача сейчас, иначе последний
// пройденный.
export function defaultStage(c: CardData): StageTab | null {
  const now = c.nodes.find(n => n.state === 'current' || n.state === 'waiting')
  return c.stages.find(s => s.key === now?.key) ?? c.stages[c.stages.length - 1] ?? null
}

// --- Артефакты этапа и версии -----------------------------------------------------

export type VersionRow = { art: ApiArtifact; current: boolean }

// Чип этапа: kind и цепочка его версий по supersedes_id, от старой к новой.
export type KindChip = { kind: string; title: string; versions: VersionRow[]; current: VersionRow }

const byVersion = (a: ApiArtifact, b: ApiArtifact) =>
  (a.version ?? 0) - (b.version ?? 0) || Date.parse(a.created_at ?? '') - Date.parse(b.created_at ?? '')

// Артефакты этапа чипами по kind. В чип входит весь путь версий каждого
// артефакта этапа: прошлые версии могли быть сданы на другой попытке.
export function chipsOf(list: readonly ApiArtifact[], runStages: Readonly<Record<string, string>>, stage: string | null): KindChip[] {
  if (!stage) return []
  const byId = new Map(list.map(a => [a.id, a]))
  const next = new Map<string, ApiArtifact[]>()
  for (const a of list) {
    if (a.supersedes_id) next.set(a.supersedes_id, [...(next.get(a.supersedes_id) ?? []), a])
  }
  const chain = (a: ApiArtifact): ApiArtifact[] => {
    let root = a
    const seen = new Set<string>([a.id])
    while (root.supersedes_id) {
      const up = byId.get(root.supersedes_id)
      if (!up || seen.has(up.id)) break
      seen.add(up.id)
      root = up
    }
    const out: ApiArtifact[] = []
    const walk = (x: ApiArtifact) => {
      if (out.some(o => o.id === x.id)) return
      out.push(x)
      for (const n of next.get(x.id) ?? []) walk(n)
    }
    walk(root)
    return out
  }
  const own = list.filter(a => a.stage_run_id && runStages[a.stage_run_id] === stage)
  const kinds: string[] = []
  for (const a of own) if (!kinds.includes(a.kind)) kinds.push(a.kind)
  return kinds.map(kind => {
    const found = new Map<string, ApiArtifact>()
    for (const a of own.filter(x => x.kind === kind)) for (const v of chain(a)) found.set(v.id, v)
    const sorted = [...found.values()].sort(byVersion)
    const live = sorted.filter(v => v.status !== 'superseded')
    const cur = live[live.length - 1] ?? sorted[sorted.length - 1]
    const versions = sorted.map(art => ({ art, current: art.id === cur?.id }))
    const current = versions.find(v => v.current) ?? { art: sorted[0] as ApiArtifact, current: true }
    return { kind, title: String(current.art.title ?? kind), versions, current }
  })
}

export type Selection = { stage: string | null; kind: string | null; version: string | null }

export type Resolved = { stage: StageTab | null; chips: KindChip[]; chip: KindChip | null; version: VersionRow | null }

// Выбор человека → что показано: вкладка (по умолчанию — текущий этап),
// чип (первый), версия (текущая). Выбор, которого уже нет в данных, уступает
// умолчанию.
export function resolve(card: CardData, files: TaskFiles | undefined, sel: Selection): Resolved {
  const stage = card.stages.find(s => s.key === sel.stage) ?? defaultStage(card)
  const chips = chipsOf(files?.artifacts ?? [], card.runStages, stage?.key ?? null)
  const chip = chips.find(c => c.kind === sel.kind) ?? chips[0] ?? null
  const version = chip ? (chip.versions.find(v => v.art.id === sel.version) ?? chip.current) : null
  return { stage, chips, chip, version }
}

// --- Вид содержимого ----------------------------------------------------------------

// Как показывается артефакт: markdown — элементом Markdown, текстовые формата
// — кодом, файл по контракту, html и бинарное — только скачиванием (решение
// 0064: HTML и SVG в панели не открываются).
export type Shown = 'markdown' | 'code' | 'diff' | 'file'

export function shownOf(a: ApiArtifact): Shown {
  if (a.file_name || a.format === 'html' || a.format === 'binary') return 'file'
  if (a.format === 'diff') return 'diff'
  if (a.format === 'json' || a.format === 'yaml' || a.format === 'text') return 'code'
  return 'markdown'
}

export const languageOf = (a: ApiArtifact) => (a.format === 'json' || a.format === 'yaml' ? a.format : undefined)

export const contentPath = (id: string) => `/v1/artifacts/${encodeURIComponent(id)}/content`

function utf8(s: string): number {
  let n = 0
  for (const ch of s) {
    const c = ch.codePointAt(0) ?? 0
    n += c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4
  }
  return n
}

export type Clipped = { text: string; hidden: number }

// Начало текста до предела: строки целиком, пока их не больше MAX_LINES и они
// влезают в MAX_BYTES; hidden — сколько строк не показано. Первая строка
// длиннее предела режется по знакам. Оборванный посреди ограждения код
// закрывается, чтобы Markdown не съел остаток панели.
export function clip(text: string): Clipped {
  const lines = (text.endsWith('\n') ? text.slice(0, -1) : text).split('\n')
  let bytes = 0
  let n = 0
  while (n < lines.length && n < MAX_LINES) {
    const b = utf8(lines[n] ?? '') + 1
    if (bytes + b > MAX_BYTES && n > 0) break
    bytes += b
    n++
    if (bytes > MAX_BYTES) break
  }
  let head = lines.slice(0, n).join('\n')
  if (n >= lines.length && utf8(head) <= MAX_BYTES) return { text, hidden: 0 }
  if (utf8(head) > MAX_BYTES) {
    let cut = ''
    let size = 0
    for (const ch of head) {
      const w = utf8(ch)
      if (size + w > MAX_BYTES) break
      cut += ch
      size += w
    }
    head = cut
  }
  const fences = head.split('\n').filter(l => l.trimStart().startsWith('```')).length
  return { text: fences % 2 === 1 ? `${head}\n\`\`\`` : head, hidden: lines.length - n }
}

// Заголовок «КЛЮЧ · kind · vN» и полный текст: то, что «В чат» кладёт в поле
// ввода.
export const chatText = (key: string, kind: string, version: number | undefined, text: string) =>
  `${key} · ${kind} · v${version ?? 1}\n\n${text}`

// --- Вложения -----------------------------------------------------------------------

// «image/png» → «PNG», «image/svg+xml» → «SVG».
export function typeLabel(mediaType: string | undefined, name: string | undefined): string | null {
  const sub = (mediaType ?? '').split(';')[0]?.split('/')[1]?.split('+')[0]
  if (sub) return sub.toUpperCase()
  const ext = /\.([A-Za-z0-9]+)$/.exec(name ?? '')?.[1]
  return ext ? ext.toUpperCase() : null
}

// HTML и SVG: исполняемая разметка, панель про неё говорит отдельно.
export function isMarkup(a: { media_type?: string | null; file_name?: string | null; format?: string }): boolean {
  const t = `${a.media_type ?? ''} ${a.file_name ?? ''} ${a.format ?? ''}`.toLowerCase()
  return /html|svg/.test(t)
}
