import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { Connection, Tab, TaskRow } from '../types'
import { anyKeys, promptKeys, toolKeys } from './keys'
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
  whoamiOf,
} from './model'
import type { EventsPage, Options, Session, TaskDetail, Workflow } from './model'

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
  shape: 'rows-1',
})
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
  const status = wf.statuses.find(st => st.key === detail.task.status_key)
  return {
    key: detail.task.key || key,
    title: detail.task.title,
    statusName: status?.name ?? detail.task.status_key,
    error: null,
    fetchedAt: await $.clock.now(),
  }
}

// Перечитать задачи. Ошибка не стирает последнее известное: у строки
// остаются прежние поля и появляется текст ошибки. Ошибка входа поднимается
// наверх — цикл обновления по ней делает долгую паузу, а не повторяет.
async function refresh($: $T, s: Session, keys: string[]) {
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
  await update($, rowsAtom, rows => {
    const next = { ...rows }
    for (const { key, row, error } of loaded) {
      if (!tracked.includes(key)) continue
      const prev = next[key]
      next[key] = row ?? (prev ? { ...prev, error } : { key, title: '', statusName: '', error, fetchedAt: now })
    }
    return next
  })
  await update($, connectionAtom, c => ({ ...c, error: authFailed ? LOGIN_HINT : null }))
  await showStatus($)
  if (authFailed) throw new ApiError(401, LOGIN_HINT)
}

async function showStatus($: $T) {
  const keys = await read($, keysAtom)
  if (keys.length === 0) return $.ui.status(undefined)
  const conn = await read($, connectionAtom)
  $.ui.status(`Clab: ${keys.length} в чате${conn.error ? ` · ${conn.error}` : ''}`)
}

async function track($: $T, s: Session, found: string[]) {
  if (found.length === 0) return
  const before = await read($, keysAtom)
  const fresh = found.filter(k => !before.includes(k))
  if (fresh.length === 0) return
  await update($, keysAtom, keys => [...keys, ...fresh.filter(k => !keys.includes(k))])
  await showStatus($)
  await s.ready.catch(() => undefined)
  await refresh($, s, fresh).catch(() => undefined)
  wake($, s)
}

async function untrack($: $T, keys: string[]) {
  await update($, keysAtom, list => list.filter(k => !keys.includes(k)))
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
    await refresh($, s, keys)
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
    await refresh($, s, keys)
    return s.events === false ? POLL_MS : MIN_GAP_MS
  }

  const page = await eventsPage($, s, `/projects/${project}/events?after=${encodeURIComponent(s.cursor)}&wait=${EVENTS_WAIT_S}`)
  if (page === null) return 0
  // Событие только будит: истина — перечитанная задача (решение 0067, п. 4).
  const hit = new Set((page.events ?? []).map(eventKey).filter((k): k is string => k !== null))
  const now = await read($, keysAtom)
  // Задачи другого проекта событиями этого проекта не покрыты: им — опрос
  // раз в 30 с.
  const end = await $.clock.now()
  const stale = now.filter(k => {
    if (k.startsWith(prefix ?? '')) return false
    const at = s.polledAt.get(k)
    return at === undefined || end - at >= POLL_MS
  })
  await refresh($, s, [...now.filter(k => hit.has(k)), ...stale])
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
async function activate($: $T, opts: Options): Promise<Session> {
  const s = newSession(opts)
  s.ready = (async () => {
    await resolveSession($, s)
    await readInstance($, s)
  })()
  await $.command.register({
    name: 'clab-plugin',
    description: 'Задачи Cadence Lab этого чата: /clab-plugin [КЛЮЧ|tasks|approvals|remove КЛЮЧ]',
    argumentHint: '[КЛЮЧ|tasks|approvals|remove КЛЮЧ]',
  })
  void s.ready.then(
    () => wake($, s),
    () => wake($, s),
  )
  return s
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
    if (keys.length) await track($, s, keys)
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
  await refresh($, s, hit.keys)
}

async function beforeTool($: $T, s: Session, tool: string, input: Record<string, unknown>) {
  await s.ready
  await track($, s, toolKeys(tool, input, s.project).keys)
}

async function onPrompt($: $T, s: Session, text: string) {
  await s.ready
  await track($, s, promptKeys(text, s.project, COMMAND))
}

// --- Модуль -------------------------------------------------------------------

export const register: Register = (on, options) => {
  const opts = optionsOf(options)
  let live: Session | null = null

  on('session.start', async ($, e, next) => {
    const started = await next(e)
    if (live) stop(live)
    live = null
    if (e.surface !== null || e.isInteractive) live = await activate($, opts)
    return started
  })

  // Поверхность появилась позже (Claude Desktop подключился к сессии).
  on('session.attach', async ($, e, next) => {
    const attached = await next(e)
    if (!live) live = await activate($, opts)
    return attached
  })

  on('session.end', async ($, e, next) => {
    if (live) stop(live)
    live = null
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

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const tab = await read($, tabAtom)
    const keys = await read($, keysAtom)
    const rows = await read($, rowsAtom)
    const conn = await read($, connectionAtom)
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
        <Box flexDirection="column">
          {keys.map(key => {
            const row = rows[key]
            const head = row?.title ? `${key} ${row.title}` : key
            const status = row?.statusName ? ` · ${row.statusName}` : row ? '' : ' · загружаю…'
            return (
              <Box flexDirection="column" key={`row-${key}`}>
                <Box flexDirection="row" justifyContent="space-between">
                  <Text wrap="truncate-end">
                    {head}
                    {status}
                  </Text>
                  <Button key={`remove-${key}`} plain dimColor onPress={() => void untrack($, [key])}>
                    ✕
                  </Button>
                </Box>
                {row?.error ? <Text color="error">Не обновилось: {row.error}</Text> : null}
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
