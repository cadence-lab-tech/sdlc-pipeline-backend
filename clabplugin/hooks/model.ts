// Чистая часть слоя данных: настройки, константы цикла обновления, разбор
// ответов. Всё, что зовёт $ (clab, сеть, часы, состояние), живёт в
// register.tsx: движок прослеживает $ только внутри одного файла.

export type Options = { host: string; project: string; clab: string }

export const hostOf = (s: string) => s.trim().replace(/\/+$/, '').replace(/\/v1$/, '').replace(/\/+$/, '')

export function optionsOf(raw: Readonly<Record<string, unknown>>): Options {
  return {
    host: hostOf(String(raw.host ?? '')),
    project: String(raw.project ?? '').trim().toUpperCase(),
    clab: String(raw.clab ?? '').trim(),
  }
}

// Ожидание события на сервере — не больше 25 с: меньше SDLC_REQUEST_TIMEOUT
// (30 с), иначе ответ оборвёт WriteTimeout API (решение 0067, п. 4).
export const EVENTS_WAIT_S = 25
// Без ручки событий — опрос задач чата раз в 30 с (решение 0067, п. 6).
export const POLL_MS = 30_000
// Между запросами событий не меньше секунды: сервер, который отвечает сразу,
// не превращает цикл в плотный.
export const MIN_GAP_MS = 1_000
// Паузы после сетевых ошибок подряд.
export const BACKOFF_MS = [5_000, 10_000, 30_000, 60_000] as const
// Токена нет или он не принят: повтор не раньше чем через минуту — вдруг
// человек вошёл в другом терминале.
export const AUTH_RETRY_MS = 60_000

export const backoff = (failures: number) => BACKOFF_MS[Math.min(Math.max(failures, 1), BACKOFF_MS.length) - 1] ?? 60_000

// clab для мода — всегда личный вход человека. Токен задачи в окружении
// (решение 0049) перекрыл бы его конфиг, поэтому SDLC_TOKEN и SDLC_API_URL
// пусты: clab читает пустое значение как отсутствие переменной.
export const CLI_ENV: Readonly<Record<string, string>> = { SDLC_TOKEN: '', SDLC_API_URL: '' }

export const CLI_PATHS = ['clab', '/opt/homebrew/bin/clab', '/usr/local/bin/clab']

export const LOGIN_HINT = 'войдите: clab auth login'

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

export type Workflow = { id: string; statuses: { key: string; name: string }[] }
export type TaskDetail = { task: { key: string; title: string; status_key: string; workflow_id: string } }
export type EventsPage = { events?: unknown[]; cursor?: unknown }

// Сессия мода. Токен — только здесь, в памяти модуля: ни в $.state, ни в
// $.store, ни в тостах и строке статуса (решение 0067, п. 3).
export type Session = {
  opts: Options
  host: string | null
  project: string | null
  token: string | null
  bin: string | null
  workflows: Map<string, Promise<Workflow>>
  // null — ещё не спрашивали, false — ручки нет, мод опрашивает задачи.
  events: boolean | null
  cursor: string | null
  timer: { cancel: () => void } | null
  running: boolean
  stopped: boolean
  failures: number
  polledAt: Map<string, number>
  ready: Promise<void>
}

export function newSession(opts: Options): Session {
  return {
    opts,
    host: null,
    project: null,
    token: null,
    bin: null,
    workflows: new Map(),
    events: null,
    cursor: null,
    timer: null,
    running: false,
    stopped: false,
    failures: 0,
    polledAt: new Map(),
    ready: Promise.resolve(),
  }
}

// Текст ошибки сервера: поле error.message ответа, иначе начало тела.
export function messageOf(status: number, text: string): string {
  try {
    const body = JSON.parse(text) as { error?: { message?: unknown } | string; message?: unknown }
    const m = typeof body.error === 'string' ? body.error : (body.error?.message ?? body.message)
    if (typeof m === 'string' && m) return `${status}: ${m.slice(0, 200)}`
  } catch {}
  return `${status}: ${text.slice(0, 200) || 'ошибка сервера'}`
}

// Ключ задачи события. Событие только будит, поэтому мод берёт из него лишь
// ключ — поле task_key, task.key или key, что из них есть.
export function eventKey(ev: unknown): string | null {
  if (!ev || typeof ev !== 'object') return null
  const e = ev as { task_key?: unknown; key?: unknown; task?: { key?: unknown } }
  const k = e.task_key ?? e.task?.key ?? e.key
  return typeof k === 'string' ? k.toUpperCase() : null
}

// Что ответил clab whoami --json: адрес API и проект каталога, если есть.
export function whoamiOf(stdout: string): { host: string | null; project: string | null } {
  try {
    const who = JSON.parse(stdout) as { api?: unknown; project?: { key?: unknown } | null }
    const host = typeof who.api === 'string' && who.api ? hostOf(who.api) : null
    const project = who.project && typeof who.project.key === 'string' && who.project.key ? who.project.key : null
    return { host, project }
  } catch {
    return { host: null, project: null }
  }
}
