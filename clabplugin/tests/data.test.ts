import { expect, test } from 'claude-code/testing'

import { HOST, PANE, TOKEN, WORKFLOW, bash, run, submit, task, world } from './world'

const tick = async (clock: { settle: () => Promise<void> }) => {
  for (let i = 0; i < 5; i++) await clock.settle()
}

// AC-10: хост и проект — из clab whoami; clab — с пустыми SDLC_TOKEN и
// SDLC_API_URL; токен — clab auth token --host.
test('хост и проект из clab whoami, токен через --host без токена задачи', async ($, on) => {
  const { j, clock } = world(on)
  await $.session.start({ cwd: '/work/acme', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-1')
  await tick(clock)

  expect(j.argv[0]?.slice(1)).toEqual(['whoami', '--json'])
  const tokenCall = j.argv.find(a => a[1] === 'auth')
  expect(tokenCall?.slice(1)).toEqual(['auth', 'token', '--host', HOST])
  for (const env of j.envs) {
    expect(env.SDLC_TOKEN).toBe('')
    expect(env.SDLC_API_URL).toBe('')
  }
  expect(j.urls).toContain(`${HOST}/v1/tasks/ACME-1`)
  expect(j.auths.filter(Boolean).every(a => a === `Bearer ${TOKEN}`)).toBe(true)
})

test('whoami не ответил — хост и проект из настроек плагина', { options: { host: 'https://other.example.test/', project: 'ZED' } }, async ($, on) => {
  const { j, clock } = world(on, {
    whoami: null,
    routes: p => (p === '/tasks/ZED-5' ? { status: 200, body: task('ZED-5', 'Чужая', 'intake') } : undefined),
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await bash($, 'clab show ZED-5')
  await tick(clock)
  expect(j.argv.find(a => a[1] === 'auth')?.slice(1)).toEqual(['auth', 'token', '--host', 'https://other.example.test'])
  expect(j.urls).toContain('https://other.example.test/v1/tasks/ZED-5')
})

// AC-10: 401 — токен перечитывается один раз; второй 401 — ошибка, прежняя
// строка не стёрта.
test('на 401 токен перечитывается один раз, прежнее состояние остаётся', async ($, on) => {
  let denied = false
  const { j, clock } = world(on, {
    tokens: ['old-token', 'new-token'],
    routes: (p, auth) => {
      if (p === '/tasks/ACME-1' && auth === 'Bearer old-token') return { status: 401, body: { error: { message: 'нет' } } }
      if (p === '/tasks/ACME-1' && denied) return { status: 401, body: {} }
      return undefined
    },
  })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'ACME-1')
  await tick(clock)
  expect(j.argv.filter(a => a[1] === 'auth')).toHaveLength(2)
  expect(j.auths.filter(a => a === 'Bearer new-token').length).toBeGreaterThan(0)

  let ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect((await ui.find({ type: 'Text', text: /ACME-1 Первая · Проектирование/ }))?.text).toBeDefined()
  await ui.unmount()

  // Токен отозван: оба раза 401 — просьба войти, строка задачи на месте.
  denied = true
  const before = j.argv.filter(a => a[1] === 'auth').length
  await bash($, 'clab decide ACME-1 --gate g')
  await tick(clock)
  expect(j.argv.filter(a => a[1] === 'auth').length - before).toBe(1)
  ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await ui.find({ type: 'Text', text: /ACME-1 Первая · Проектирование/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /войдите: clab auth login/ })).toBeDefined()
  await ui.unmount()
  expect(j.statuses.at(-1)).toContain('войдите')
})

// AC-10: токен нигде, кроме заголовка запроса и вывода clab auth token.
test('токен не попадает ни в состояние, ни в строку статуса, ни в аргументы', async ($, on) => {
  const { j, clock } = world(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-1 ACME-2')
  await tick(clock)
  expect(j.states.length).toBeGreaterThan(0)
  for (const w of j.states) expect(w).not.toContain(TOKEN)
  for (const s of j.statuses) expect(String(s)).not.toContain(TOKEN)
  for (const t of j.toasts) expect(t).not.toContain(TOKEN)
  for (const a of j.argv) expect(a.join(' ')).not.toContain(TOKEN)
  expect(j.urls.join(' ')).not.toContain(TOKEN)
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    const all = await ui.findAll({ type: 'Text' })
    expect(all.map(t => t.text).join('\n')).not.toContain(TOKEN)
    await ui.unmount()
  }
})

// AC-10: две задачи одной версии цикла — один запрос версии.
test('две задачи одной версии цикла дают один запрос /workflows', async ($, on) => {
  const { j, clock } = world(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-1 ACME-2')
  await tick(clock)
  await run($, 'ACME-1')
  await tick(clock)
  expect(j.urls.filter(u => u.endsWith('/workflows/wf-1'))).toHaveLength(1)
})

// AC-10, AC-11: сервер без /v1/instance и без ручки событий — мод живёт.
test('без /v1/instance и без ручки событий — опрос раз в 30 с', async ($, on) => {
  const { j, clock } = world(on, { routes: p => (p === '/instance' ? { status: 404, text: 'not found' } : undefined) })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-1')
  await tick(clock)
  const conn = j.states.filter(w => w.includes('"connection"')).at(-1) ?? ''
  expect(conn).toContain('"serverVersion":null')
  expect(conn).toContain('"events":false')
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /опрос раз в 30 с/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /сервер v/ })).toBeUndefined()
  await ui.unmount()

  const reads = () => j.urls.filter(u => u.endsWith('/tasks/ACME-1')).length
  const first = reads()
  await clock.advance(29_000)
  expect(reads()).toBe(first)
  await clock.advance(1_000)
  await tick(clock)
  expect(reads()).toBe(first + 1)

  // После меняющей команды clab строка перечитывается сразу.
  await bash($, 'clab transition ACME-1 --to next')
  await tick(clock)
  expect(reads()).toBe(first + 2)
  // Читающая — нет.
  await bash($, 'clab show ACME-1')
  await tick(clock)
  expect(reads()).toBe(first + 2)
})

test('сетевая ошибка /v1/instance не роняет мод', async ($, on) => {
  const { j, clock } = world(on, { fail: /^\/instance$/ })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-1')
  await tick(clock)
  expect(j.urls).toContain(`${HOST}/v1/tasks/ACME-1`)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /ACME-1 Первая/ })).toBeDefined()
  await ui.unmount()
})

// AC-11: долгий опрос событий проекта; событие будит ровно одно чтение
// своей задачи, чужое — ни одного; курсор передаётся как есть.
test('события проекта: курсор, пробуждение по своей задаче, ничего по чужой', async ($, on) => {
  const pages: { events: unknown[]; cursor: string }[] = [
    { events: [], cursor: 'c-1' },
    { events: [{ task_key: 'ACME-1', type: 'stage.claimed' }], cursor: 'c 2/x' },
    { events: [{ task_key: 'ACME-9', type: 'task.updated' }], cursor: 'c-3' },
  ]
  let served = 0
  const { j, clock } = world(on, {
    routes: p => {
      if (p.startsWith('/projects/ACME/events')) {
        const page = pages[Math.min(served, pages.length - 1)]
        served++
        return { status: 200, body: page }
      }
      return undefined
    },
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-1')
  await tick(clock)

  const events = () => j.urls.filter(u => u.includes('/events'))
  const reads = () => j.urls.filter(u => u.endsWith('/tasks/ACME-1')).length
  expect(events()[0]).toBe(`${HOST}/v1/projects/ACME/events?wait=0`)

  const r0 = reads()
  await clock.advance(1_000)
  await tick(clock)
  expect(events()[1]).toBe(`${HOST}/v1/projects/ACME/events?after=c-1&wait=25`)
  expect(reads()).toBe(r0 + 1)

  await clock.advance(1_000)
  await tick(clock)
  expect(events()[2]).toBe(`${HOST}/v1/projects/ACME/events?after=${encodeURIComponent('c 2/x')}&wait=25`)
  expect(reads()).toBe(r0 + 1)
  const wait = Number(new URL(events()[2] ?? '').searchParams.get('wait'))
  expect(wait).toBeLessThanOrEqual(25)
})

// AC-11: ошибка сети — пауза, без плотного цикла; состояние не стёрто;
// session.end останавливает цикл.
test('ошибка сети: пауза между повторами, состояние на месте, конец сессии гасит цикл', async ($, on) => {
  let down = true
  const { j, clock } = world(on, {
    routes: p => (p.startsWith('/projects/ACME/events') ? { status: 200, body: { events: [], cursor: 'k' } } : undefined),
    fail: { test: (p: string) => down && /^\/projects\/ACME\/events\?after/.test(p) } as RegExp,
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-1')
  await tick(clock)
  const calls = () => j.urls.filter(u => u.includes('/events?after')).length

  await clock.advance(1_000)
  await tick(clock)
  expect(calls()).toBe(1)
  // Пауза 5 с: за 4 с повторов нет.
  await clock.advance(4_000)
  expect(calls()).toBe(1)
  await clock.advance(1_000)
  await tick(clock)
  expect(calls()).toBe(2)
  // Следующая пауза длиннее.
  await clock.advance(9_000)
  expect(calls()).toBe(2)
  await clock.advance(1_000)
  await tick(clock)
  expect(calls()).toBe(3)

  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /ACME-1 Первая · Проектирование/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /нет связи с инстансом/ })).toBeDefined()
  await ui.unmount()
  expect(j.statuses.at(-1)).toContain('нет связи')

  // Связь вернулась — ошибка снята.
  down = false
  await clock.advance(30_000)
  await tick(clock)
  const back = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await back.find({ type: 'Text', text: /нет связи с инстансом/ })).toBeUndefined()
  await back.unmount()
  expect(j.statuses.at(-1)).toBe('Clab: 1 в чате')

  await $.session.end({ reason: 'prompt_input_exit', sessionId: 's', resume: { id: 's' } } as never)
  const n = j.urls.length
  await clock.advance(120_000)
  await tick(clock)
  expect(j.urls.length).toBe(n)
})

// AC-12: сессия без поверхности инертна; поверхность позже — мод включается.
test('без поверхности: ни clab, ни HTTP, ни строки статуса, ни таймеров', async ($, on) => {
  const { j, clock } = world(on)
  await $.session.start({ cwd: '/', surface: null, isInteractive: false })
  await bash($, 'clab claim ACME-1')
  await submit($, '/clab-run ACME-2')
  await clock.advance(120_000)
  await tick(clock)
  expect(j.argv).toHaveLength(0)
  expect(j.urls).toHaveLength(0)
  expect(j.statuses).toHaveLength(0)
  expect(j.opened).toBe(0)
  expect(j.commands).toHaveLength(0)

  await $.session.attach({ surface: 'desktop', clientId: 'desktop:default' })
  await tick(clock)
  expect(j.commands).toEqual(['clab-plugin'])
  expect(j.argv[0]?.slice(1)).toEqual(['whoami', '--json'])
})

// AC-14: вымышленный цикл и другие типы задач — подписи из цикла, поведение
// то же.
test('чужой цикл: подписи статусов берутся из версии цикла задачи', async ($, on) => {
  const other = {
    ...WORKFLOW,
    id: 'wf-x',
    statuses: [
      { id: 'a', key: 'intake', name: 'Intake', position: 1, is_terminal: false },
      { id: 'b', key: 'design', name: 'Design', position: 2, is_terminal: false },
      { id: 'c', key: 'build', name: 'Build', position: 3, is_terminal: false },
      { id: 'd', key: 'verify', name: 'Verify', position: 4, is_terminal: true },
    ],
  }
  const { clock } = world(on, {
    routes: p => {
      if (p === '/workflows/wf-x') return { status: 200, body: other }
      if (p === '/tasks/ACME-7') return { status: 200, body: task('ACME-7', 'Spike', 'build', 'wf-x') }
      if (p === '/tasks/ACME-8') return { status: 200, body: { ...task('ACME-8', 'Chore', 'verify', 'wf-x') } }
      return undefined
    },
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-7 ACME-8')
  await tick(clock)
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect(await ui.find({ type: 'Text', text: /ACME-7 Spike · Build/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /ACME-8 Chore · Verify/ })).toBeDefined()
    await ui.unmount()
  }
})
