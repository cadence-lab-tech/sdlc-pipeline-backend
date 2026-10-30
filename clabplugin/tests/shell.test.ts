import { expect, test } from 'claude-code/testing'

import { PANE, SURFACES, bash, run, submit, world } from './world'

const tick = async (clock: { settle: () => Promise<void> }) => {
  for (let i = 0; i < 5; i++) await clock.settle()
}

const keysOf = (states: string[]) => {
  const last = states.filter(w => w.includes('"keys"')).at(-1)
  if (!last) return []
  const value = (JSON.parse(last) as { value?: { value?: string[] } | string[] }).value
  return Array.isArray(value) ? value : (value?.value ?? [])
}

// AC-9: поимка ключа из /clab-run, Bash clab и тулов MCP sdlc; ключ вне
// префикса проекта не ловится; вход хуков не меняется и не запрещается.
test('ключи ловятся из промпта, Bash и MCP, чужой префикс — нет, вход не тронут', async ($, on) => {
  const { j, clock } = world(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await tick(clock)

  const prompt = await submit($, '/clab-run ACME-1')
  expect(prompt).toMatchObject({ text: '/clab-run ACME-1' })
  const gates = { tool: 'Bash', command: 'SDLC_PROJECT=ACME clab gates ACME-2', description: 'гейты' }
  const ran = await $.tool.call(gates as never)
  expect((ran as { deny?: unknown }).deny).toBeUndefined()
  const mcp = { tool: 'mcp__sdlc__sdlc_get_task', task_key: 'ACME-3' }
  await $.tool.call(mcp as never)
  await bash($, 'clab show OTHER-4 XACME-5 ACME-6x')
  await submit($, 'посмотри ACME-7 просто так')
  await submit($, '/help ACME-8')
  await tick(clock)

  expect(keysOf(j.states)).toEqual(['ACME-1', 'ACME-2', 'ACME-3'])
  // То, что дошло до движка, — ровно то, что было вызвано.
  expect(j.prompts[0]).toBe('/clab-run ACME-1')
  expect(j.tools[0]).toMatchObject(gates)
  expect(j.tools[1]).toMatchObject(mcp)
})

test('скил clab-* через тул Skill и команда в пространстве имён ловят ключ', async ($, on) => {
  const { j, clock } = world(on)
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await tick(clock)
  await $.tool.call({ tool: 'Skill', skill: 'clab-work', args: 'ACME-1 developer' } as never)
  await submit($, '/someplugin:clab-status ACME-2\n\nразвёрнутый текст скила с ACME-99')
  await tick(clock)
  expect(keysOf(j.states)).toEqual(['ACME-1', 'ACME-2'])
})

test('без проекта ключи сами не ловятся, вручную — любые', async ($, on) => {
  const { j, clock } = world(on, { whoami: { api: 'https://cadence.example.test/v1', project: null } })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await tick(clock)
  await submit($, '/clab-run ACME-1')
  await tick(clock)
  expect(keysOf(j.states)).toEqual([])
  await run($, 'acme-2')
  await tick(clock)
  expect(keysOf(j.states)).toEqual(['ACME-2'])
})

// AC-13: оболочка панели на terminal и desktop.
test('панель: пустое состояние, три вкладки, команда добавляет и убирает, строка статуса', async ($, on) => {
  const { j, clock } = world(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await tick(clock)

  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect(await ui.find({ type: 'Text', text: /В этом чате задач пока нет/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\/clab-plugin ACME-12/ })).toBeDefined()
    for (const label of ['Этот чат', 'Задачи']) {
      expect(await ui.find({ type: 'Button', text: label })).toBeDefined()
    }
    // У сервера мира нет выборки «Ждут меня» (404): вкладка скрыта (CLAB-326).
    expect(await ui.find({ type: 'Button', text: 'Ждут меня' })).toBeUndefined()
    await ui.unmount()
  }
  expect(j.statuses.filter(s => s !== undefined)).toHaveLength(0)

  const added = await run($, 'ACME-1')
  await tick(clock)
  expect(added.text).toContain('ACME-1')
  expect(j.opened).toBeGreaterThan(0)
  expect(j.statuses.at(-1)).toBe('Clab: 1 в чате')

  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect(await ui.find({ type: 'Text', text: /ACME-1 Первая · Проектирование/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /сервер v9\.9\.9/ })).toBeDefined()
    await ui.press({ key: 'tab-tasks' })
    expect(await ui.find({ type: 'Button', text: /В работе/ })).toBeDefined()
    await ui.press({ key: 'tab-chat' })
    expect(await ui.find({ type: 'Text', text: /ACME-1 Первая/ })).toBeDefined()
    await ui.unmount()
  }

  await run($, 'tasks')
  const onDesktop = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await onDesktop.find({ type: 'Button', text: /В работе/ })).toBeDefined()
  await onDesktop.unmount()
  // Командой вкладку открыть можно и без выборки: она говорит, почему пусто.
  await run($, 'approvals')
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /сервер старше: «Ждут меня»/ })).toBeDefined()
  await ui.unmount()

  await run($, 'remove ACME-1')
  await tick(clock)
  expect(j.statuses.at(-1)).toBeUndefined()
  const empty = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await empty.find({ type: 'Text', text: /В этом чате задач пока нет/ })).toBeDefined()
  await empty.unmount()
})

test('кнопка ✕ убирает задачу; задача с ошибкой показывает её и не мешает другим', async ($, on) => {
  const { j, clock } = world(on, {
    routes: p => (p === '/tasks/ACME-404' ? { status: 404, body: { error: { code: 'not_found', message: 'задача не найдена' } } } : undefined),
  })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'ACME-404 ACME-2')
  await tick(clock)
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect(await ui.find({ type: 'Text', text: /Не обновилось: 404: задача не найдена/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /ACME-2 Вторая · Сборка/ })).toBeDefined()
    await ui.unmount()
  }
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  await ui.press({ key: 'remove-ACME-404' })
  expect(await ui.find({ type: 'Text', text: /ACME-404/ })).toBeUndefined()
  await ui.unmount()
  expect(j.statuses.at(-1)).toBe('Clab: 1 в чате')
})

// AC-13: мод не пишет в разговор и не даёт модели тулов.
test('мод не пишет в сессию, не отправляет промптов и не регистрирует тулов', async ($, on) => {
  const appended: unknown[] = []
  const tools: unknown[] = []
  on('session.append', (_, e) => {
    appended.push(e)
    return { message: e.message, uuid: 'u' } as never
  })
  on('tool.register', (_, e) => {
    tools.push(e)
    return { value: { tool: 'x' } } as never
  })
  const { j, clock } = world(on)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'ACME-1')
  await bash($, 'clab transition ACME-1 --to x')
  await clock.advance(60_000)
  await tick(clock)
  expect(appended).toHaveLength(0)
  expect(j.prompts).toHaveLength(0)
  expect(tools).toHaveLength(0)
})
