import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { CardData, Connection, Tab, TaskRow } from '../types'
import { ago, backLine, cardOf, metaLine, money, sizeOf } from './card'
import type { TaskDetail, Workflow } from './card'
import { anyKeys, promptKeys, toolKeys } from './keys'
import { cycleLine } from './term'
import { cycleAlt, cycleSvg } from './svg'
import { schemeOf } from './theme'
import { GLYPH, gateWord, WORDS } from './vocab'
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
  whoamiOf,
} from './model'
import type { EventsPage, Options, Session } from './model'

// clab-plugin — Cadence Lab в Claude (решение 0067). Каркас: поимка ключей
// задач в чате, строка задачи с подписью статуса из её версии цикла, вкладки
// панели, команда /clab-plugin, строка статуса и слой данных под ними.
//
// Мод ничего не меняет ни в задачах, ни в разговоре: хуки prompt.submit и
// tool.call только смотрят, тулов модели нет, в сессию он не пишет. Смысла
// этапов он не знает — подписи берёт из цикла задачи, ключи ловит по
// префиксу проекта из данных.
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
  shape: 'rows-2',
})
export const openAtom = atom({ plugin: 'clab-plugin', key: 'open' } as const, [] as string[], { shape: 'open-1' })
export const describedAtom = atom({ plugin: 'clab-plugin', key: 'described' } as const, [] as string[], { shape: 'described-1' })
export const themeAtom = atom({ plugin: 'clab-plugin', key: 'theme' } as const, '', { shape: 'theme-1' })
export const tabAtom = atom({ plugin: 'clab-plugin', key: 'tab' } as const, 'chat' as Tab, { shape: 'tab-1' })
export const connectionAtom = atom(
  { plugin: 'clab-plugin', key: 'connection' } as const,
  { host: null, project: null, serverVersion: null, events: null, error: null } as Connection,
  { shape: 'connection-1' },
)

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
  let found: { host: string | null; project: string | null } = { host: null, project: null }
  try {
    const res = await runClab($, s, ['whoami', '--json'])
    if (res.exitCode === 0) found = whoamiOf(res.stdout)
  } catch {
    // Нет clab, нет входа — остаются настройки плагина.
  }
  s.host = found.host ?? (s.opts.host || null)
  s.project = found.project ?? (s.opts.project || null)
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
async function api<T>($: $T, s: Session, path: string): Promise<T> {
  for (let attempt = 0; attempt < 2; attempt++) {
    const auth = await token($, s)
    const res = await $.http.fetch(`${s.host}/v1${path}`, {
      headers: { Authorization: `Bearer ${auth}`, Accept: 'application/json' },
    })
    if (res.status === 401) {
      s.token = null
      continue
    }
    if (!res.ok) throw new ApiError(res.status, messageOf(res.status, res.text))
    return JSON.parse(res.text) as T
  }
  throw new ApiError(401, LOGIN_HINT)
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
  return {
    key: detail.task.key || key,
    title: detail.task.title,
    statusName: card.statusName,
    awaiting: card.awaiting,
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
      next[key] = row ?? (prev ? { ...prev, error } : { key, title: '', statusName: '', awaiting: false, error, fetchedAt: now, card: null })
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

async function showStatus($: $T) {
  const keys = await read($, keysAtom)
  if (keys.length === 0) return $.ui.status(undefined)
  const conn = await read($, connectionAtom)
  const rows = await read($, rowsAtom)
  const waiting = keys.filter(k => rows[k]?.awaiting).length
  const approvals = waiting > 0 ? ` · ${GLYPH.approval} ${waiting} ${WORDS.approval}` : ''
  $.ui.status(`Clab: ${keys.length} ${WORDS.inChat}${approvals}${conn.error ? ` · ${conn.error}` : ''}`)
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
  if (keys.length === 0 || !s.host) return null
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
  await $.ui.open({ id: PANE, title: TITLE })
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

// --- Карточка ------------------------------------------------------------------

// Элементы поверхности. Svg есть везде, кроме терминала: там схема — строка.
type Els = { Box: any; Text: any; Button: any; Link: any; Markdown: any; Svg?: any }

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

const MARK_COLOR = { ok: 'success', skip: undefined, open: undefined, approval: 'warning', fail: 'error' } as const
const MARK_GLYPH = { ok: GLYPH.ok, skip: GLYPH.ok, open: GLYPH.open, approval: GLYPH.approval, fail: GLYPH.fail } as const
const CHILD_DOT = { done: 'success', waiting: 'warning', active: 'claude', backlog: undefined, cancelled: undefined } as const

type CardOpts = { scheme: ReturnType<typeof schemeOf>; now: number; described: boolean }

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
      {kids}
      {files}
      {footer}
      {o.described && c.description ? <Markdown text={c.description} /> : null}
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
    const ran = await next(e)
    if (s) void afterTool($, s, tool, input).catch(() => undefined)
    return ran
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
    return (
      <Box flexDirection="row" gap={1}>
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
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const els = $.ui.resolve(e) as unknown as Els
    const { Box, Text, Button } = els
    const tab = await read($, tabAtom)
    const keys = await read($, keysAtom)
    const rows = await read($, rowsAtom)
    const conn = await read($, connectionAtom)
    const open = await read($, openAtom)
    const described = await read($, describedAtom)
    const scheme = schemeOf(await read($, themeAtom))
    const now = await $.clock.now()
    const example = `${conn.project ?? 'КЛЮЧ'}-12`

    const tabs = (
      <Box flexDirection="row" gap={1} flexWrap="wrap">
        {TABS.map(t => (
          <Button
            key={`tab-${t.id}`}
            variant={t.id === tab ? 'primary' : 'secondary'}
            onPress={() => void update($, tabAtom, () => t.id)}>
            {t.label}
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
                  ? cardTree($, els, e.surface, row.card, { scheme, now, described: described.includes(key) })
                  : null}
              </Box>
            )
          })}
        </Box>
      )

    // Вкладки «Ждут меня» и «Задачи» в каркасе — оболочка: содержимое
    // приходит следующими версиями плагина.
    const later = (what: string) => (
      <Box flexDirection="column">
        <Text dimColor>{what}</Text>
        <Text dimColor>Раздел появится в следующей версии плагина; пока — /clab-plugin {example}.</Text>
      </Box>
    )

    const body =
      tab === 'chat'
        ? chat
        : tab === 'approvals'
          ? later('Здесь будут задачи, которые ждут вашего решения.')
          : later('Здесь будет список задач проекта.')

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
        <Text dimColor>{where}</Text>
      </Box>
    )
  })
}
