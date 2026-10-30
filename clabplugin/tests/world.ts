import { mock } from 'claude-code/testing'
import type { Engine } from 'claude-code/testing'
import type { On } from 'claude-code'

// Мир под модом для тестов: подставные clab ($.process.run) и сервер
// ($.http.fetch), часы в памяти и журнал всего, что мод сделал. Ни один
// тест не ходит в настоящую сеть и не запускает настоящий clab.

export const TOKEN = 'clab_test-secret-token-0042'
export const HOST = 'https://cadence.example.test'

export type Reply = { status: number; body?: unknown; text?: string }

export type WorldOptions = {
  // Ответ clab whoami --json; null — whoami падает (нет входа, нет clab).
  whoami?: Record<string, unknown> | null
  // Что отдаёт clab auth token по очереди вызовов; последний повторяется.
  tokens?: string[]
  // Ответ сервера на путь после /v1 (с запросом). undefined — 404.
  routes?: (path: string, auth: string | undefined) => Reply | undefined
  // Сетевая ошибка на запросы, путь которых подходит.
  fail?: RegExp
  // Где стоят часы в начале теста (мс); по умолчанию 0.
  now?: number
  // Значение ряда theme в /config; undefined — ряда нет.
  theme?: string
}

export type Journal = {
  argv: string[][]
  envs: Record<string, string>[]
  urls: string[]
  auths: (string | undefined)[]
  statuses: (string | undefined)[]
  opened: number
  commands: string[]
  toasts: string[]
  tools: Record<string, unknown>[]
  prompts: string[]
  // Всё, что мод записал в $.state, как JSON.
  states: string[]
}

export const WORKFLOW = {
  id: 'wf-1',
  name: 'Поток',
  version: 4,
  statuses: [
    { id: 's1', key: 'intake', name: 'Приём', position: 1, is_terminal: false },
    { id: 's2', key: 'design', name: 'Проектирование', position: 2, is_terminal: false },
    { id: 's3', key: 'build', name: 'Сборка', position: 3, is_terminal: false },
    { id: 's4', key: 'verify', name: 'Сверка', position: 4, is_terminal: true },
  ],
  transitions: [],
}

export const task = (key: string, title: string, status: string, workflow = 'wf-1') => ({
  task: { key, title, status_key: status, status_name: status, workflow_id: workflow, type: 'story' },
  transitions: [],
  stage_runs: [],
  children: [],
})

export const WHOAMI = { principal: 'user:ann', authenticated: true, api: `${HOST}/v1`, project: { key: 'ACME', name: 'Acme' } }

export function world(on: On, opts: WorldOptions = {}) {
  const j: Journal = { argv: [], envs: [], urls: [], auths: [], statuses: [], opened: 0, commands: [], toasts: [], tools: [], prompts: [], states: [] }
  const tokens = opts.tokens ?? [TOKEN]
  let tokenCalls = 0
  const clock = mock.clock(on, { now: opts.now ?? 0 })
  on('config.list', () => ({
    value:
      opts.theme === undefined
        ? []
        : [{ key: 'theme', label: 'Theme', kind: 'choice', value: opts.theme, options: ['auto', 'dark', 'light'], provider: { plugin: 'engine', tier: 'core' }, isLocked: false }],
  }) as never)

  on('state.set', (_, e, next) => {
    j.states.push(JSON.stringify(e))
    return next(e)
  })
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.attach', (_, e) => ({ clientId: e.clientId }))
  on('session.end', (_, e) => ({ sessionId: e.sessionId }))
  on('command.register', (_, e) => {
    j.commands.push(e.name)
    return { value: { command: e.name } }
  })
  on('ui.open', () => {
    j.opened++
    return { value: { isPlaced: true } }
  })
  on('ui.status', (_, e) => {
    j.statuses.push(e.text)
    return { value: undefined } as never
  })
  on('ui.toast', (_, e) => {
    j.toasts.push(e.text)
    return { value: undefined } as never
  })
  on('tool.call', (_, e) => {
    j.tools.push({ ...(e as unknown as Record<string, unknown>) })
    return { result: {} } as never
  })
  on('prompt.submit', (_, e) => {
    j.prompts.push(e.text)
    return { text: e.text }
  })
  on('process.run', (_, e) => {
    const argv = [...e.argv]
    j.argv.push(argv)
    j.envs.push({ ...(e.init?.env ?? {}) })
    const ok = (stdout: string) => ({
      value: { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false },
    })
    const bad = (stderr: string) => ({
      value: { exitCode: 1, stdout: '', stderr, isStdoutTruncated: false, isStderrTruncated: false },
    })
    if (argv[1] === 'whoami') {
      const who = opts.whoami === undefined ? WHOAMI : opts.whoami
      return who === null ? bad('хост не настроен') : ok(JSON.stringify(who))
    }
    if (argv[1] === 'auth' && argv[2] === 'token') {
      const t = tokens[Math.min(tokenCalls, tokens.length - 1)] ?? ''
      tokenCalls++
      return t ? ok(`${t}\n`) : bad('токена нет: clab auth login')
    }
    return bad('неизвестная команда')
  })
  on('http.fetch', (_, e) => {
    j.urls.push(e.url)
    const auth = e.init?.headers?.Authorization
    j.auths.push(auth)
    const path = e.url.startsWith(`${HOST}/v1`) ? e.url.slice(`${HOST}/v1`.length) : e.url
    if (opts.fail?.test(path)) throw new Error('ECONNREFUSED')
    const reply = opts.routes?.(path, auth) ?? defaultRoutes(path)
    if (!reply) return { value: { status: 404, ok: false, headers: {}, text: '404 page not found' } }
    const text = reply.text ?? JSON.stringify(reply.body ?? {})
    return { value: { status: reply.status, ok: reply.status >= 200 && reply.status < 300, headers: {}, text } }
  })
  return { j, clock }
}

// Сервер по умолчанию: /instance, цикл wf-1 и две задачи ACME.
export function defaultRoutes(path: string): Reply | undefined {
  if (path === '/instance') return { status: 200, body: { version: 'v9.9.9' } }
  if (path === '/workflows/wf-1') return { status: 200, body: WORKFLOW }
  if (path === '/tasks/ACME-1') return { status: 200, body: task('ACME-1', 'Первая', 'design') }
  if (path === '/tasks/ACME-2') return { status: 200, body: task('ACME-2', 'Вторая', 'build') }
  return undefined
}

export const PANE = {
  plugin: 'clab-plugin',
  component: 'Pane',
  requestId: 'clab-plugin',
  props: {
    title: 'Cadence Lab',
    isFocused: false,
    bodyColumns: 70,
    placement: 'dock',
    scroll: { offset: 0, bodyRows: 40 },
    view: {},
  },
} as const

export const SURFACES = ['terminal', 'desktop'] as const

// Команда и промпт так, как их набрал человек: поля, которые ставит движок
// (origin, presentation, wait), тест не придумывает.
export const run = ($: Engine, args: string) => $.command.run({ command: 'clab-plugin', args } as never)
export const submit = ($: Engine, text: string) => $.prompt.submit({ text } as never)
export const bash = ($: Engine, command: string) => $.tool.call({ tool: 'Bash', command, description: 'clab' } as never)
