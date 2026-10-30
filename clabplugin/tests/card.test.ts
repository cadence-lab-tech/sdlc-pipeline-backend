import { expect, test } from 'claude-code/testing'

import type { CycleNode } from '../types'
import { awaitsApproval, cardOf, metaLine, backLine } from '../hooks/card'
import type { TaskDetail, Workflow } from '../hooks/card'
import { layout, cycleSvg, WIDTH } from '../hooks/svg'
import { cycleLine, lineText } from '../hooks/term'
import { contrast, DARK, LIGHT, schemeOf, svgStyle } from '../hooks/theme'
import { GATE_WORD, GLYPH, WORDS } from '../hooks/vocab'
import { CAPTURED_AT, TASKS, WORKFLOWS } from './stack'
import { HOST, PANE, run, world } from './world'
import type { Reply } from './world'

// Карточка задачи CLAB-324 на снимке стека задачи: проект NOVA (форма
// нашего цикла: Simple, Epic, Release) и FORGE (вымышленный: Story с
// необязательным Design, Incident на 9 узлов, Initiative на 3 узла).

const NOW = Date.parse(CAPTURED_AT) + 60_000
// true — тест перечня печатает строки карточек (для разбора изменений).
const DUMP = false
const tick = async (clock: { settle: () => Promise<void> }) => {
  for (let i = 0; i < 6; i++) await clock.settle()
}
const clone = <T>(x: T): T => JSON.parse(JSON.stringify(x)) as T

// Сервер по снимку: /tasks/:key и /workflows/:id; tasks/workflows можно
// подменить (обезличенные копии, сервер старше).
function stackRoutes(tasks: Record<string, any> = TASKS, workflows: Record<string, any> = WORKFLOWS, extra?: (p: string) => Reply | undefined) {
  return (path: string): Reply | undefined => {
    const own = extra?.(path)
    if (own) return own
    const t = /^\/tasks\/([A-Z]+-\d+)$/.exec(path)
    if (t && tasks[t[1] ?? '']) return { status: 200, body: tasks[t[1] ?? ''] }
    const w = /^\/workflows\/(.+)$/.exec(path)
    if (w && workflows[w[1] ?? '']) return { status: 200, body: workflows[w[1] ?? ''] }
    if (path === '/instance') return { status: 200, body: { version: 'v9.9.9' } }
    return undefined
  }
}

const detail = (key: string) => TASKS[key] as TaskDetail
const wfOf = (key: string) => WORKFLOWS[detail(key).task.workflow_id] as Workflow
const card = (key: string) => cardOf(detail(key), wfOf(key), HOST)

// Все строки, которые нарисовал мод в поддереве: строковые дети, подписи
// кнопок и ссылок, alt и подписи узлов Svg, текст Markdown.
function strings(node: unknown, out: string[] = []): string[] {
  if (typeof node === 'string') {
    if (node) out.push(node)
    return out
  }
  if (!node || typeof node !== 'object') return out
  const el = node as { type?: string; props?: Record<string, unknown>; children?: unknown[] }
  const p = el.props ?? {}
  for (const k of ['label', 'alt', 'text']) if (typeof p[k] === 'string') out.push(p[k] as string)
  if (el.type === 'Svg' && typeof p.source === 'string') {
    for (const m of (p.source as string).matchAll(/<text[^>]*>([^<]*)<\/text>/g)) out.push((m[1] ?? '').replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"'))
  }
  for (const c of el.children ?? (p.children as unknown[]) ?? []) strings(c, out)
  return out
}

// Строки так, как их читает человек: Text и Button целиком, Link — подписью,
// Svg — подписями узлов.
function lines(node: unknown, out: string[] = []): string[] {
  if (!node || typeof node !== 'object') return out
  const el = node as { type?: string; props?: Record<string, unknown>; children?: unknown[] }
  if (el.type === 'Button') {
    out.push(`[${typeof el.props?.label === 'string' ? el.props.label : strings(el).join('')}]`)
    return out
  }
  if (el.type === 'Text') {
    out.push(strings(el).join(''))
    return out
  }
  if (el.type === 'Link' || el.type === 'Markdown') {
    out.push(strings(el).join(''))
    return out
  }
  if (el.type === 'Svg') {
    out.push(`<Svg: ${strings(el).slice(1).join(' | ')}>`)
    return out
  }
  for (const c of el.children ?? (el.props?.children as unknown[]) ?? []) lines(c, out)
  return out
}

// --- AC-1: правило «ждёт апрува» и строка ------------------------------------

test('правило «ждёт апрува» на пяти таблицах гейтов', () => {
  const g = (type: string, status: string, blocking = true) => ({ type, status, blocking })
  const adv = (...gates: ReturnType<typeof g>[]) => ({ kind: 'advance', gates })
  expect(awaitsApproval([adv(g('artifacts_required', 'passed'), g('human_approval', 'passed'))])).toBe(false)
  expect(awaitsApproval([adv(g('artifacts_required', 'passed'), g('checklist', 'waived'), g('drift_clear', 'skipped'), g('human_approval', 'pending'))])).toBe(true)
  expect(awaitsApproval([adv(g('artifacts_required', 'failed'), g('human_approval', 'pending'))])).toBe(false)
  expect(awaitsApproval([adv(g('artifacts_required', 'pending'), g('human_approval', 'pending')), adv(g('checklist', 'passed'), g('human_approval', 'pending'))])).toBe(true)
  expect(awaitsApproval([])).toBe(false)
  expect(awaitsApproval([{ kind: 'reject', gates: [g('human_approval', 'pending')] }])).toBe(false)
})

test('строка: подпись статуса из версии цикла, признак апрува, два запроса на задачу', async ($, on) => {
  const { j, clock } = world(on, { now: NOW, routes: stackRoutes() })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'FORGE-2 FORGE-4')
  await tick(clock)
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await ui.find({ type: 'Text', text: /FORGE-2 Платёжный шлюз .* · Deploy window/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /FORGE-4 Токены темы · Verify/ })).toBeDefined()
  // Ждёт апрува только FORGE-2.
  const marks = await ui.findAll({ type: 'Text', text: /⏸ ждёт апрува/ })
  expect(marks.map(m => m.text)).toEqual(['⏸ ждёт апрува · Go-live sign-off'])
  expect(marks[0]?.props.color).toBe('warning')
  await ui.unmount()
  const reads = (re: RegExp) => j.urls.filter(u => re.test(u)).length
  expect(reads(/\/tasks\/FORGE-2$/)).toBe(1)
  expect(reads(/\/tasks\/FORGE-4$/)).toBe(1)
  // Две задачи разных версий цикла — по одному запросу версии; повторов нет.
  expect(j.urls.filter(u => u.includes('/workflows/'))).toHaveLength(2)
})

// --- AC-2…AC-4: схема, возврат, гейты, строка под схемой ------------------------

test('узлы схемы по версии цикла: 6, 5, 9 и 3 узла; начальный и отменённый не рисуются', () => {
  const expectNodes = (key: string, statuses: number, names: string[]) => {
    expect(wfOf(key).statuses).toHaveLength(statuses)
    expect(card(key).nodes.map(n => n.name)).toEqual(names)
  }
  expectNodes('NOVA-4', 8, ['Analysis', 'Development', 'Testing', 'Merge', 'Release', 'Done'])
  expectNodes('FORGE-4', 7, ['Intake', 'Design', 'Build', 'Verify', 'Closed'])
  expectNodes('FORGE-2', 11, ['Triage', 'Contain', 'Diagnose', 'Fix', 'Peer review', 'Deploy window', 'Monitor', 'Postmortem write-up', 'Resolved'])
  expectNodes('FORGE-1', 5, ['Scoping', 'Delivery', 'Closed'])
  expectNodes('NOVA-1', 6, ['Planning', 'Analysis', 'In progress', 'Done'])
  expectNodes('NOVA-11', 6, ['Prepare', 'Publish', 'Verify', 'Done'])

  const states = (key: string) => card(key).nodes.map(n => `${n.state}${n.attempts > 1 ? `×${n.attempts}` : ''}`)
  expect(states('NOVA-4')).toEqual(['done', 'current×2', 'future', 'future', 'future', 'future'])
  expect(states('FORGE-2')).toEqual(['done', 'done', 'done', 'done', 'done', 'waiting×2', 'future', 'future', 'future'])
  // Design выключен у задачи (решение 0066) — пунктиром.
  expect(states('FORGE-4')).toEqual(['done', 'skipped', 'done', 'current', 'future'])
  // Design включён и ждёт апрува.
  expect(states('FORGE-3')).toEqual(['done', 'waiting', 'future', 'future', 'future'])

  // Отменённый статус рисуется, только если задача в нём.
  const cancelled = clone(detail('NOVA-4'))
  cancelled.task.status_key = 'cancelled'
  cancelled.transitions = []
  expect(cardOf(cancelled, wfOf('NOVA-4'), HOST).nodes.at(-1)).toMatchObject({ name: 'Cancelled', state: 'current' })
  // Статус без роли (начальный) — ни один узел не текущий, «этап 0 из m».
  const initial = clone(detail('NOVA-4'))
  initial.task.status_key = 'new'
  initial.stage_runs = []
  const c = cardOf(initial, wfOf('NOVA-4'), HOST)
  expect(c.step).toBe(0)
  expect(metaLine(c, NOW)).toContain('этап 0 из 6')
})

test('возврат: одна дуга и одна строка с причиной, хотя reject-переходов в цикле три', () => {
  const c = card('NOVA-4')
  expect(c.back).toMatchObject({ from: 2, to: 1, fromName: 'Testing', reason: 'нет теста на повторную выгрузку' })
  expect(backLine(c, NOW)).toMatch(/^Вернули из Testing \d+ мин назад: «нет теста на повторную выгрузку»$/)
  const svg = cycleSvg(c.nodes, c.back, 'light')
  expect(svg.match(/<path d="M [^"]* C /g)).toHaveLength(1)
  // Без возврата — ни дуги, ни строки.
  const plain = card('FORGE-4')
  expect(plain.back).toBeNull()
  expect(backLine(plain, NOW)).toBeNull()
  expect(cycleSvg(plain.nodes, plain.back, 'light')).not.toContain(' C ')
  // Возврат на 9 узлах: из Monitor в Deploy window.
  expect(card('FORGE-2').back).toMatchObject({ from: 6, to: 5, fromName: 'Monitor', reason: 'доля ошибок выросла до 4% после выкладки' })
})

test('гейты перехода вперёд: имя перехода и гейтов из версии, слово типа, вердикт, счётчик', () => {
  const c = card('FORGE-2')
  expect(c.gates?.transition).toBe('Approve production deploy')
  expect(c.gates?.items.map(g => `${g.mark} ${g.name} ${g.type} ${g.reason ?? ''}`.trim())).toEqual([
    'ok Fix verified agent_review снят вручную',
    'ok Rollback plan attached artifacts_required',
    'ok Linked tickets closed dependencies_closed',
    'approval Go-live sign-off human_approval апрувов 0 из 1',
  ])
  expect([c.gates?.passed, c.gates?.total]).toEqual([3, 4])
  expect(c.awaiting).toBe(true)
  expect(c.approvalGate).toBe('Go-live sign-off')
  // Не ждёт апрува: ✓, ○ и ✕ без ⏸.
  const nova = card('NOVA-4')
  expect(nova.gates?.transition).toBe('Разработка готова')
  expect(nova.gates?.items.map(g => g.mark)).toEqual(['ok', 'fail', 'open'])
  expect(nova.awaiting).toBe(false)
  // Терминальный статус — блока гейтов нет.
  const done = clone(detail('NOVA-4'))
  done.transitions = []
  expect(cardOf(done, wfOf('NOVA-4'), HOST).gates).toBeNull()
  // Тип гейта вне словаря — ключом как есть.
  const odd = clone(detail('NOVA-4'))
  odd.transitions = [{ key: 'x', name: 'X', kind: 'advance', gates: [{ gate_name: 'Новый', type: 'quantum_check', status: 'pending', blocking: true }] }]
  expect(cardOf(odd, wfOf('NOVA-4'), HOST).gates?.items[0]?.type).toBe('quantum_check')
})

test('строка под схемой: цикл и версия, этап n из m, попытка, кто держит и аренда', () => {
  const c = card('FORGE-2')
  expect(metaLine(c, NOW)).toMatch(/^Incident v1 · этап 6 из 9 · попытка 2 · держит ops-agent · аренда ещё \d+ мин$/)
  // Аренда истекла — никто не держит.
  expect(metaLine(c, Date.parse(c.leaseUntil ?? '') + 1)).toBe('Incident v1 · этап 6 из 9 · попытка 2 · никто не держит')
  expect(metaLine(card('FORGE-1'), NOW)).toBe('Initiative v1 · этап 2 из 3 · никто не держит')
  expect(c.cost).toBe(1.2)
  expect(c.url).toBe(`${HOST}/p/FORGE/tasks/FORGE-2`)
  expect(c.attachments).toEqual([{ name: 'gateway-502-rate.gif', size: 14, at: expect.any(String) }])
  expect(c.artifacts.map(a => a.title)).toEqual(['Rollback plan'])
})

test('карточка раскрывается, описание и ссылки вторым планом, PR из repo_refs', async ($, on) => {
  const tasks = clone(TASKS)
  tasks['FORGE-2'].task.description = '## Что случилось\n\nШлюз отвечает 502.'
  tasks['FORGE-2'].repo_refs = [{ ref_type: 'pull_request', ref: '77', url: 'https://github.example/acme/gw/pull/77' }, { ref_type: 'branch', ref: 'x' }]
  const { clock } = world(on, { now: NOW, routes: stackRoutes(tasks) })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'FORGE-2')
  await tick(clock)
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect(await ui.find({ type: 'Link', text: 'Открыть в Cadence Lab ↗' })).toMatchObject({ props: { href: `${HOST}/p/FORGE/tasks/FORGE-2` } })
    expect(await ui.find({ type: 'Link', text: 'PR #77 ↗' })).toMatchObject({ props: { href: 'https://github.example/acme/gw/pull/77' } })
    expect(await ui.findAll({ type: 'Link', text: /^PR / })).toHaveLength(1)
    expect(await ui.find({ type: 'Text', text: '$1.20' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /До перехода «Approve production deploy»/ })).toBeDefined()
    expect(await ui.find({ type: 'Markdown' })).toBeUndefined()
    await ui.press({ key: 'desc-FORGE-2' })
    expect((await ui.find({ type: 'Markdown' }))?.text).toContain('Шлюз отвечает 502')
    await ui.press({ key: 'desc-FORGE-2' })
    // Svg — не в терминале; в терминале схема строкой.
    if (surface === 'terminal') {
      expect(await ui.find({ type: 'Svg' })).toBeUndefined()
      expect(await ui.find({ type: 'Text', text: /✓ пройдено 5 ─ ⏸ Deploy window ×2 ─ ○ Monitor ─ … ещё 1 ─ ○ Resolved/ })).toBeDefined()
    } else {
      const svg = await ui.find({ type: 'Svg' })
      expect(String(svg?.props.alt)).toContain('⏸ Deploy window ×2')
    }
    // Свернуть и раскрыть.
    await ui.press({ key: 'open-FORGE-2' })
    expect(await ui.find({ type: 'Text', text: /До перехода/ })).toBeUndefined()
    await ui.press({ key: 'open-FORGE-2' })
    await ui.unmount()
  }
})

// --- AC-5, AC-6, AC-12: дети ---------------------------------------------------

test('задача с детьми: полоса прогресса, строки детей, «после», отменённый, чип типа, ≤3 запроса', async ($, on) => {
  const { j, clock } = world(on, { now: NOW, routes: stackRoutes() })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'FORGE-1')
  await tick(clock)
  const ch = card('FORGE-1').children
  expect(ch?.progress).toEqual({ done: 2, waiting: 2, active: 1, backlog: 1, cancelled: 1, total: 6 })
  expect(ch?.rows.map(r => [r.key, r.state, r.statusName, r.typeChip, r.after])).toEqual([
    ['FORGE-2', 'waiting', 'Deploy window', true, null],
    ['FORGE-3', 'waiting', 'Design', false, null],
    ['FORGE-4', 'active', 'Verify', false, null],
    ['FORGE-5', 'done', 'Closed', false, null],
    ['FORGE-6', 'done', 'Closed', false, null],
    ['FORGE-7', 'cancelled', 'Dropped', false, null],
    ['FORGE-8', 'backlog', 'Backlog', false, 'FORGE-6'],
  ])
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect(await ui.find({ type: 'Text', text: '2 из 6 готово · отменено 1' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: '• готово 2  • ждёт апрува 2  • в работе 1  • не начато 1' })).toBeDefined()
    const dropped = await ui.find({ type: 'Button', key: 'child-FORGE-7' })
    expect(JSON.stringify(dropped?.children)).toContain('"strikethrough":true')
    expect((await ui.find({ type: 'Button', key: 'child-FORGE-8' }))?.text).toContain('после FORGE-6')
    expect((await ui.find({ type: 'Button', key: 'child-FORGE-2' }))?.text).toContain('incident')
    expect((await ui.find({ type: 'Button', key: 'child-FORGE-2' }))?.text).toContain('⏸ Deploy window')
    expect((await ui.find({ type: 'Button', key: 'child-FORGE-4' }))?.text).not.toContain('story')
    await ui.unmount()
  }
  // Карточка с детьми — те же два запроса: задача и версия её цикла.
  expect(j.urls.filter(u => u.includes('/tasks/') || u.includes('/workflows/'))).toHaveLength(2)

  // Нажатие на ребёнка добавляет его карточку в чат.
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'child-FORGE-8' })
  await tick(clock)
  // Ребёнок в чате и раскрыт; в снимке его карточки нет — сервер ответил 404.
  expect(await ui.find({ key: 'row-FORGE-8' })).toBeDefined()
  expect(j.urls.filter(u => u.endsWith('/tasks/FORGE-8'))).toHaveLength(1)
  await ui.unmount()
})

test('«после» — до двух ключей, остальные числом', () => {
  const d = clone(detail('NOVA-1'))
  const kid = d.children?.find(c => c.key === 'NOVA-7')
  expect(kid?.blocked_by).toEqual(['NOVA-4'])
  if (kid) kid.blocked_by = ['NOVA-2', 'NOVA-3', 'NOVA-4', 'NOVA-5']
  const rows = cardOf(d, wfOf('NOVA-1'), HOST).children?.rows ?? []
  expect(rows.find(r => r.key === 'NOVA-7')?.after).toBe('NOVA-2, NOVA-3 +2')
})

test('сервер старше: строки детей по status_name, без полосы и «после», лишних запросов нет', async ($, on) => {
  const tasks = clone(TASKS)
  for (const c of tasks['FORGE-1'].children) {
    delete c.status_category
    delete c.awaiting_approval
    delete c.blocked_by
  }
  delete tasks['FORGE-1'].repo_refs
  const { j, clock } = world(on, { now: NOW, routes: stackRoutes(tasks) })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'FORGE-1')
  await tick(clock)
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect(await ui.find({ type: 'Text', text: 'сервер старше: прогресс недоступен' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /из 6 готово/ })).toBeUndefined()
    expect((await ui.find({ type: 'Button', key: 'child-FORGE-8' }))?.text).toContain('Backlog')
    expect((await ui.find({ type: 'Button', key: 'child-FORGE-8' }))?.text).not.toContain('после')
    // Карточка самой задачи — целиком.
    expect(await ui.find({ type: 'Text', text: /Initiative v1 · этап 2 из 3/ })).toBeDefined()
    await ui.unmount()
  }
  expect(j.urls.filter(u => u.includes('/tasks/') || u.includes('/workflows/'))).toHaveLength(2)
})

// --- AC-8, AC-9: события, полоса над вводом, строка статуса, тост -----------------

function liveWorld(on: Parameters<typeof world>[0], tasks: Record<string, any>, pages: { events: unknown[]; cursor: string }[]) {
  let served = 0
  return world(on, {
    now: NOW,
    whoami: { api: `${HOST}/v1`, project: { key: 'FORGE' } },
    routes: stackRoutes(tasks, WORKFLOWS, p => {
      if (!p.startsWith('/projects/FORGE/events')) return undefined
      const page = pages[Math.min(served, pages.length - 1)]
      served++
      return { status: 200, body: page }
    }),
  })
}

test('апрув извне: событие → карточка ≤ 30 с, полоса пропадает, тост «ключ · статус → статус»', async ($, on) => {
  const tasks = clone(TASKS)
  const pages = [{ events: [] as unknown[], cursor: 'c0' }]
  const { j, clock } = liveWorld(on, tasks, pages)
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'FORGE-2 FORGE-3')
  await tick(clock)

  // Две ждущие задачи — полоса одна и называет первую; строка статуса — 2.
  let band = await $.ui.mount({ plugin: 'clab-plugin', surface: 'desktop', component: 'AbovePrompt', props: { hasSurvey: false, isWorking: false, maxRows: 10, bodyColumns: 80, scroll: { offset: 0, bodyRows: 10 } } } as never)
  const bandText = (await band.findAll({ type: 'Text' })).map(t => t.text).join(' ')
  expect(bandText).toContain('FORGE-2 ждёт вашего апрува · Go-live sign-off')
  expect(bandText).not.toContain('FORGE-3')
  expect(await band.find({ type: 'Button', text: 'Открыть' })).toBeDefined()
  expect(await band.find({ type: 'Button', text: /Утвердить|Вернуть/ })).toBeUndefined()
  await band.unmount()
  expect(j.statuses.at(-1)).toBe('Clab: 2 в чате · ⏸ 2 апрув')
  expect(j.toasts).toHaveLength(0)

  // В интерфейсе поставили апрув и перевели задачу: событие по FORGE-2.
  const moved = clone(TASKS['FORGE-2'])
  moved.task.status_key = 'monitor'
  moved.task.status_name = 'Monitor'
  moved.transitions = [{ key: 'monitor_done', name: 'Monitor complete', kind: 'advance', gates: [] }]
  moved.stage_runs = [...moved.stage_runs.map((r: any) => (r.finished_at ? r : { ...r, finished_at: CAPTURED_AT })), { id: 'r-new', status_key: 'monitor', kind: 'stage', attempt: 2 }]
  tasks['FORGE-2'] = moved
  pages.push({ events: [{ task_key: 'FORGE-2', type: 'task.advanced' }], cursor: 'c1' }, { events: [], cursor: 'c2' })
  await clock.advance(1_000)
  await tick(clock)
  await clock.advance(1_000)
  await tick(clock)

  expect(j.toasts).toEqual(['FORGE-2 · Deploy window → Monitor'])
  band = await $.ui.mount({ plugin: 'clab-plugin', surface: 'terminal', component: 'AbovePrompt', props: { hasSurvey: false, isWorking: false, maxRows: 10, bodyColumns: 80, scroll: { offset: 0, bodyRows: 10 } } } as never)
  expect((await band.findAll({ type: 'Text' })).map(t => t.text).join(' ')).toContain('FORGE-3 ждёт вашего апрува · Design sign-off')
  await band.unmount()
  expect(j.statuses.at(-1)).toBe('Clab: 2 в чате · ⏸ 1 апрув')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await ui.find({ type: 'Text', text: /FORGE-2 .* · Monitor/ })).toBeDefined()
  await ui.unmount()
})

test('своё действие в чате тоста не даёт; событие ребёнка будит родителя', async ($, on) => {
  const tasks = clone(TASKS)
  const pages = [{ events: [] as unknown[], cursor: 'c0' }]
  // Bash идёт секунду: за это время приходит событие о его же переходе.
  let during: (() => Promise<void>) | null = null
  on('tool.call', { tool: 'Bash' }, async () => {
    await during?.()
    return { result: {} } as never
  })
  const { j, clock } = liveWorld(on, tasks, pages)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await run($, 'FORGE-1 FORGE-4')
  await tick(clock)
  const reads = (key: string) => j.urls.filter(u => u.endsWith(`/tasks/${key}`)).length

  // Ребёнок FORGE-3 не в чате, событие по нему перечитывает FORGE-1.
  const before = reads('FORGE-1')
  pages.push({ events: [{ task_key: 'FORGE-3', type: 'gate.decided' }], cursor: 'c1' })
  await clock.advance(1_000)
  await tick(clock)
  expect(reads('FORGE-1')).toBe(before + 1)
  expect(reads('FORGE-3')).toBe(0)

  // clab transition в этом чате, событие о нём пришло, пока тул шёл.
  const moved = clone(TASKS['FORGE-4'])
  moved.task.status_key = 'closed'
  moved.task.status_name = 'Closed'
  tasks['FORGE-4'] = moved
  pages.push({ events: [{ task_key: 'FORGE-4', type: 'task.advanced' }], cursor: 'c2' })
  during = async () => {
    await clock.advance(1_000)
    for (let i = 0; i < 4; i++) await clock.settle()
  }
  await $.tool.call({ tool: 'Bash', command: 'clab transition FORGE-4 --to accept', description: 'x' } as never)
  await tick(clock)
  expect(j.toasts).toEqual([])
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /FORGE-4 Токены темы · Closed/ })).toBeDefined()
  await ui.unmount()
})

// --- AC-10, AC-11: ширина, тема -------------------------------------------------

const nodesOfNames = (names: string[], cur: number): CycleNode[] =>
  names.map((name, i) => ({ key: `s${i}`, name, state: i < cur ? 'done' : i === cur ? 'current' : 'future', attempts: i === cur ? 3 : 1 }))

test('подписи узлов не выходят за viewBox на 3, 5, 6, 9 узлах и длинных, русских, китайских названиях', () => {
  const long = 'Очень длинное название этапа '.repeat(7).slice(0, 200)
  const cases: string[][] = [
    ['Scoping', 'Delivery', 'Closed'],
    ['Intake', 'Design', 'Build', 'Verify', 'Closed'],
    ['Analysis', 'Development', 'Testing', 'Merge', 'Release', 'Done'],
    ['Triage', 'Contain', 'Diagnose', 'Fix', 'Peer review', 'Deploy window', 'Monitor', 'Postmortem write-up', 'Resolved'],
    [long, long, long],
    Array.from({ length: 6 }, () => long),
    Array.from({ length: 9 }, (_, i) => `需求分析与评审阶段${i}`),
    ['Приём заявки', 'Проектирование решения', 'Сборка', 'Сверка с заказчиком', 'Закрыта'],
  ]
  for (const names of cases) {
    for (let cur = 0; cur < names.length; cur++) {
      const L = layout(nodesOfNames(names, cur), { from: names.length - 1, to: 0, fromName: 'x', reason: null, at: null })
      for (const l of L.labels) {
        const left = l.anchor === 'start' ? l.x : l.anchor === 'end' ? l.x - l.width : l.x - l.width / 2
        expect(left).toBeGreaterThanOrEqual(0)
        expect(left + l.width).toBeLessThanOrEqual(WIDTH)
      }
      // До 6 узлов — подпись у каждого; больше — только у текущего.
      expect(L.labels).toHaveLength(names.length > 6 ? 1 : names.length)
      if (names.length <= 6) expect(L.labels.find(l => l.node === cur)?.text.endsWith(' ×3')).toBe(true)
    }
  }
})

test('терминал: до 6 узлов — все, больше — «✓ пройдено n», текущий, следующий, «… ещё n», последний', () => {
  const nine = card('FORGE-2').nodes
  expect(lineText(cycleLine(nine))).toBe('✓ пройдено 5 ─ ⏸ Deploy window ×2 ─ ○ Monitor ─ … ещё 1 ─ ○ Resolved')
  expect(lineText(cycleLine(card('FORGE-4').nodes))).toBe('✓ Intake ─ ◌ Design ─ ✓ Build ─ ● Verify ─ ○ Closed')
  expect(lineText(cycleLine(card('FORGE-1').nodes))).toBe('✓ Scoping ─ ● Delivery ─ ○ Closed')
  const first = nodesOfNames(['A1', 'A2', 'A3', 'A4', 'A5', 'A6', 'A7', 'A8', 'A9'], 0)
  expect(lineText(cycleLine(first))).toBe('● A1 ×3 ─ ○ A2 ─ … ещё 6 ─ ○ A9')
  const last = nodesOfNames(['A1', 'A2', 'A3', 'A4', 'A5', 'A6', 'A7', 'A8', 'A9'], 8)
  expect(lineText(cycleLine(last))).toBe('✓ пройдено 8 ─ ● A9 ×3')
})

test('палитры: контраст подписей ≥ 4.5:1, «текущий» и «ждёт апрува» различаются, выбор по ряду theme', () => {
  for (const p of [LIGHT, DARK]) {
    for (const c of [p.text, p.dim, p.faint]) expect(contrast(c, p.bg)).toBeGreaterThanOrEqual(4.5)
    expect(p.cur).not.toBe(p.wait)
  }
  expect(['dark', 'dark-daltonized', 'dark-ansi', 'light', 'light-ansi', 'auto', '', 'solarized'].map(schemeOf)).toEqual(['dark', 'dark', 'dark', 'light', 'light', 'auto', 'auto', 'auto'])
  expect(svgStyle('dark')).toContain(DARK.bg)
  expect(svgStyle('dark')).not.toContain('prefers-color-scheme')
  expect(svgStyle('auto')).toContain('@media (prefers-color-scheme: dark)')
})

test('тема Claude: палитра Svg из ряда theme, смена в /config перерисовывает схему', async ($, on) => {
  on('config.set', (_, e) => ({ value: e.value }) as never)
  const { clock } = world(on, { now: NOW, routes: stackRoutes(), theme: 'dark' })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'FORGE-4')
  await tick(clock)
  let ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(String((await ui.find({ type: 'Svg' }))?.props.source)).toContain(`--bg:${DARK.bg}`)
  await ui.unmount()
  await $.config.set({ key: 'theme', value: 'light' } as never)
  ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  const source = String((await ui.find({ type: 'Svg' }))?.props.source)
  expect(source).toContain(`--bg:${LIGHT.bg}`)
  expect(source).not.toContain(DARK.bg)
  await ui.unmount()
  await $.config.set({ key: 'theme', value: 'auto' } as never)
  ui = await $.ui.mount({ ...PANE, surface: 'vscode' })
  expect(String((await ui.find({ type: 'Svg' }))?.props.source)).toContain('prefers-color-scheme')
  await ui.unmount()
})

// --- AC-13, AC-16: ни одной подписи вне версии цикла и словаря -------------------

// Обезличить снимок: каждое название статуса, перехода, гейта, вида
// артефакта, роли, типа, заголовок, причина, хэндл — свой токен §N.
function tokenize(rename: (n: number) => string) {
  const tasks = clone(TASKS)
  const wfs = clone(WORKFLOWS)
  const map = new Map<string, string>()
  let n = 0
  const tok = (s: unknown) => {
    if (typeof s !== 'string' || !s) return s
    if (!map.has(s)) map.set(s, rename(++n))
    return map.get(s)
  }
  for (const w of Object.values(wfs) as any[]) {
    w.name = tok(w.name)
    for (const st of w.statuses) {
      st.name = tok(st.name)
      for (const r of st.roles ?? []) r.role_name = tok(r.role_name)
    }
  }
  for (const d of Object.values(tasks) as any[]) {
    d.task.title = tok(d.task.title)
    d.task.status_name = tok(d.task.status_name)
    d.task.priority = tok(d.task.priority)
    d.task.description = tok('описание задачи')
    d.task.properties = { type: tok(d.task.properties?.type) }
    for (const t of d.transitions) {
      t.name = tok(t.name)
      for (const g of t.gates) {
        g.gate_name = tok(g.gate_name)
        g.reason = tok(g.reason)
      }
    }
    for (const r of d.stage_runs) {
      r.claimed_by_handle = tok(r.claimed_by_handle)
      if (r.exit) r.exit.comment = tok(r.exit.comment)
    }
    for (const a of d.artifacts) a.title = tok(a.title)
    for (const a of d.attachments) a.file_name = tok(a.file_name)
    for (const c of d.children) {
      c.title = tok(c.title)
      c.status_name = tok(c.status_name)
      c.properties = { type: tok(c.properties?.type) }
    }
    d.usage = { cost_usd: 3.5 }
    d.repo_refs = [{ ref_type: 'pull_request', ref: '41', url: 'https://github.example/x/y/pull/41' }]
  }
  return { tasks, wfs }
}

const ALLOWED = new Set(
  [...Object.values(WORDS), ...Object.values(GATE_WORD), 'Cadence Lab', 'Не обновилось']
    .flatMap(w => w.toLowerCase().split(/[\s:·,]+/))
    .filter(Boolean),
)
const GLYPHS = new Set([...Object.values(GLYPH), '·', '─', '…', '«', '»', '×', '$', '#', '+', ',', ':', '(', ')', '—', '✕', '⏸', 'v', '.'])

function leftovers(all: string[], token: RegExp): string[] {
  const bad: string[] = []
  for (const s of all) {
    const rest = s.replace(token, ' ').replace(/[A-Z][A-Z0-9_]*-\d+/g, ' ').replace(/\d+([.,]\d+)?/g, ' ')
    for (const w of rest.split(/[\s·:,«»()]+/).filter(Boolean)) {
      if (ALLOWED.has(w.toLowerCase())) continue
      if ([...w].every(ch => GLYPHS.has(ch))) continue
      bad.push(`${w} ← «${s}»`)
    }
  }
  return bad
}

for (const [label, rename, token] of [
  ['токены', (n: number) => `§${n}`, /§\d+/g],
  ['китайские', (n: number) => `需求${n}阶段`, /需求\d+阶段/g],
  ['длинные', (n: number) => `L${n}${'ж'.repeat(196)}`, /L\d+ж*/g],
] as const) {
  test(`подписи карточек — только данные цикла, словарь гейтов и слова плагина (${label})`, async ($, on) => {
    const { tasks, wfs } = tokenize(rename)
    const keys = Object.keys(tasks)
    const { j, clock } = world(on, { now: NOW, routes: stackRoutes(tasks, wfs), theme: 'auto' })
    await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
    await run($, keys.join(' '))
    await tick(clock)
    const seen: string[] = []
    for (const surface of ['terminal', 'desktop', 'vscode'] as const) {
      const ui = await $.ui.mount({ ...PANE, surface })
      for (const key of keys) {
        const row = await ui.find({ key: `row-${key}` })
        expect(row).toBeDefined()
        seen.push(...strings(row))
      }
      // Описание раскрыто у всех.
      for (const key of keys) await ui.press({ key: `desc-${key}` })
      for (const key of keys) seen.push(...strings(await ui.find({ key: `row-${key}` })))
      for (const key of keys) await ui.press({ key: `desc-${key}` })
      await ui.unmount()
      if (surface !== 'vscode') {
        const band = await $.ui.mount({ plugin: 'clab-plugin', surface, component: 'AbovePrompt', props: { hasSurvey: false, isWorking: false, maxRows: 10, bodyColumns: 80, scroll: { offset: 0, bodyRows: 10 } } } as never)
        seen.push(...strings(await band.drawn()))
        await band.unmount()
      }
    }
    seen.push(...j.statuses.filter((s): s is string => typeof s === 'string').map(s => s.replace(/^Clab: /, '')), ...j.toasts)
    expect(seen.length).toBeGreaterThan(100)
    expect(leftovers(seen, token)).toEqual([])
    // Ничего из исходного снимка в обезличенной карточке не осталось.
    for (const name of ['Deploy window', 'Approve production deploy', 'Go-live sign-off', 'Development', 'incident', 'Rollback plan', 'ops-agent']) {
      expect(seen.join('\n')).not.toContain(name)
    }
  })
}

// AC-16: перечень строк карточек обоих проектов на снимке стека — для
// разбора изменений и проверки глазами.
test('перечень строк карточек NOVA и FORGE (терминал и Desktop)', async ($, on) => {
  const { clock } = world(on, { now: NOW, routes: stackRoutes(), theme: 'light' })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  const keys = Object.keys(TASKS)
  await run($, keys.join(' '))
  await tick(clock)
  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    for (const key of keys) {
      expect(strings(await ui.find({ key: `row-${key}` })).length).toBeGreaterThan(3)
      if (DUMP) console.log(`--- ${surface} ${key}\n${lines(await ui.find({ key: `row-${key}` })).join('\n')}`)
      if (DUMP && surface === 'desktop') {
        const c = card(key)
        for (const scheme of ['light', 'dark'] as const) console.log(`=== svg ${key} ${scheme}\n${cycleSvg(c.nodes, c.back, scheme)}`)
      }
    }
    await ui.unmount()
  }
})
