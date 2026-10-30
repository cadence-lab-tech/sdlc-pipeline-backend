import { expect, test } from 'claude-code/testing'

import { PAGE } from '../hooks/tasks'
import { WORDS } from '../hooks/vocab'
import { CAPTURED_AT, TASKS, WORKFLOWS } from './stack'
import { HOST, PANE, TOKEN, run, world } from './world'
import type { Reply } from './world'

// Вкладка «Задачи» (CLAB-327) на обезличенных снимках двух проектов: NOVA
// (форма нашего цикла: Simple, Epic, Release) и FORGE (вымышленный: Story,
// Incident, Initiative на 5, 9 и 3 узла). Циклы — снимок стека задачи
// CLAB-324 (tests/stack.ts), список — подставной сервер, который отвечает на
// параметры списка так же, как ручка: terminal, has_role, category,
// in_work, top_level, q, type, assignee, unassigned, order, limit, offset,
// parent.

const NOW = Date.parse(CAPTURED_AT) + 3_600_000
const MIN = 60_000
const tick = async (clock: { settle: () => Promise<void> }) => {
  for (let i = 0; i < 8; i++) await clock.settle()
}

type Status = { key: string; name: string; category: string; is_terminal: boolean; roles: unknown[] }
type Wf = { id: string; name: string; task_types: string[]; statuses: Status[] }
const WFS = Object.values(WORKFLOWS) as Wf[]
const wfNamed = (name: string) => WFS.find(w => w.name === name) as Wf

// Строка снимка: цикл по имени, статус по ключу, возраст в минутах.
type Fx = { key: string; title: string; wf: string; status: string; parent?: string; assignee?: string; age: number; holder?: string; claimable?: boolean }

const statusOf = (f: Fx) => wfNamed(f.wf).statuses.find(s => s.key === f.status) as Status

// Сегмент по правилам из бизнес-требований и решения человека после возврата
// с тестирования — считает тест, независимо от мода и по всем задачам
// проекта, а не по задачам без родителя. Нетерминальная задача, у которой есть
// дети в работе, ждут апрува или готовые, — «в работе», хотя у её статуса нет
// роли; в «бэклоге» остаются неначатые.
const STARTED = ['active', 'waiting', 'done']
function segmentOf(f: Fx, all: readonly Fx[]): string | null {
  const st = statusOf(f)
  if (!st.is_terminal) {
    const started = all.some(k => k.parent === f.key && STARTED.includes(statusOf(k).category))
    return st.roles.length > 0 || started ? 'work' : 'backlog'
  }
  return st.category === 'done' ? 'done' : null
}
const inSegment = (all: readonly Fx[], seg: string) => all.filter(f => segmentOf(f, all) === seg).sort((a, b) => a.age - b.age)

function itemOf(f: Fx, all: Fx[], withNew = true) {
  const wf = wfNamed(f.wf)
  const st = statusOf(f)
  const base = {
    key: f.key,
    title: f.title,
    status_key: st.key,
    status_name: st.name,
    workflow_id: wf.id,
    parent_key: f.parent ?? null,
    assignee_handle: f.assignee ?? null,
    properties: { type: wf.task_types[0] },
    updated_at: new Date(NOW - f.age * MIN).toISOString(),
  }
  if (!withNew) return base
  const kids = all.filter(k => k.parent === f.key)
  const counts: Record<string, number> = {}
  for (const k of kids) counts[statusOf(k).category] = (counts[statusOf(k).category] ?? 0) + 1
  return { ...base, status_category: st.category, ...(kids.length ? { child_counts: counts } : {}) }
}

// NOVA: эпик NOVA-1 с восемью детьми (один отменён), шесть в работе, четырнадцать
// в бэклоге, девять готовых, две отменённые, три можно забрать.
function nova(): Fx[] {
  const out: Fx[] = [
    { key: 'NOVA-1', title: 'Импорт и экспорт данных: форматы, ограничения и история операций', wf: 'Epic', status: 'in_progress', age: 5 },
    { key: 'NOVA-2', title: 'Выгрузка отчёта в CSV: выбор столбцов', wf: 'Simple', status: 'done', parent: 'NOVA-1', age: 700 },
    { key: 'NOVA-3', title: 'Кодировки и разделитель', wf: 'Simple', status: 'done', parent: 'NOVA-1', age: 650 },
    { key: 'NOVA-4', title: 'История импортов в интерфейсе', wf: 'Simple', status: 'development', parent: 'NOVA-1', age: 20 },
    { key: 'NOVA-5', title: 'Лимиты на размер файла', wf: 'Simple', status: 'testing', parent: 'NOVA-1', age: 30 },
    { key: 'NOVA-6', title: 'Повторная отправка при ошибке', wf: 'Simple', status: 'development', parent: 'NOVA-1', age: 40 },
    { key: 'NOVA-7', title: 'Шаблоны сопоставления столбцов', wf: 'Simple', status: 'new', parent: 'NOVA-1', age: 900 },
    { key: 'NOVA-8', title: 'Журнал правок', wf: 'Simple', status: 'new', parent: 'NOVA-1', age: 910 },
    { key: 'NOVA-9', title: 'Экспорт в XML', wf: 'Simple', status: 'cancelled', parent: 'NOVA-1', age: 920 },
    { key: 'NOVA-10', title: 'Бэкенд: необязательные поля в схеме импорта', wf: 'Simple', status: 'merge', age: 300, holder: 'a.petrova' },
    { key: 'NOVA-11', title: 'Выпуск v3.0.0', wf: 'Release', status: 'prepare', age: 1440, claimable: true },
    { key: 'NOVA-12', title: 'Учётные данные попадают в журнал экспорта', wf: 'Simple', status: 'analysis', age: 14, claimable: true },
    { key: 'NOVA-13', title: 'Счётчик повторных попыток в карточке заказа', wf: 'Simple', status: 'release', age: 120, assignee: 'a.petrova' },
    { key: 'NOVA-14', title: 'Проверка миграций на копии базы', wf: 'Simple', status: 'testing', age: 200, claimable: true },
  ]
  for (let i = 0; i < 14; i++) out.push({ key: `NOVA-${20 + i}`, title: `Идея ${i + 1} в бэклоге`, wf: 'Simple', status: 'new', age: 2000 + i * 10 })
  for (let i = 0; i < 9; i++) out.push({ key: `NOVA-${40 + i}`, title: `Сделано ${i + 1}`, wf: 'Simple', status: 'done', age: 4000 + i * 10 })
  out.push({ key: 'NOVA-50', title: 'Снятая задача', wf: 'Simple', status: 'cancelled', age: 5000 })
  out.push({ key: 'NOVA-51', title: 'Снятый выпуск', wf: 'Release', status: 'cancelled', age: 5100 })
  return out
}

// FORGE: Initiative на трёх узлах с семью детьми разных циклов (один снят),
// Story с необязательным Design, Incident на девяти узлах.
function forge(): Fx[] {
  return [
    { key: 'FORGE-1', title: 'Платёжный шлюз второго поколения', wf: 'Initiative', status: 'delivery', age: 10 },
    { key: 'FORGE-2', title: 'Откат релиза при ошибке оплаты', wf: 'Incident', status: 'deploy_window', parent: 'FORGE-1', age: 25 },
    { key: 'FORGE-3', title: 'Страница статуса платежа', wf: 'Story', status: 'design', parent: 'FORGE-1', age: 35 },
    { key: 'FORGE-4', title: 'Повтор запроса к банку', wf: 'Story', status: 'verify', parent: 'FORGE-1', age: 45 },
    { key: 'FORGE-5', title: 'Тестовый контур', wf: 'Story', status: 'closed', parent: 'FORGE-1', age: 800 },
    { key: 'FORGE-6', title: 'Сверка выписок', wf: 'Story', status: 'closed', parent: 'FORGE-1', age: 810 },
    { key: 'FORGE-7', title: 'Старый режим токенов', wf: 'Story', status: 'dropped', parent: 'FORGE-1', age: 820 },
    { key: 'FORGE-8', title: 'Отчёт по возвратам', wf: 'Story', status: 'backlog', parent: 'FORGE-1', age: 830 },
    { key: 'FORGE-9', title: 'Сбой очереди уведомлений', wf: 'Incident', status: 'triage', age: 15, claimable: true },
    { key: 'FORGE-10', title: 'Мобильный экран баланса', wf: 'Story', status: 'build', age: 60 },
    { key: 'FORGE-11', title: 'Идея: подписки', wf: 'Story', status: 'backlog', age: 1000 },
    { key: 'FORGE-12', title: 'Разбор инцидента 41', wf: 'Incident', status: 'resolved', age: 2000 },
  ]
}

type Server = { fixtures: Fx[]; legacy?: boolean; hits?: string[] }

// Ручки проекта и карточек. legacy — сервер прошлого выпуска: параметры
// списка он не знает, total и новых полей не отдаёт.
function routes(sv: Server, project: string) {
  return (path: string): Reply | undefined => {
    sv.hits?.push(path)
    const url = new URL(path, 'http://x')
    const fx = sv.fixtures
    if (url.pathname === `/projects/${project}/workflows`) {
      return {
        status: 200,
        body: {
          workflows: [
            ...WFS.map(w => ({ id: w.id, name: w.name, state: 'active', task_types: w.task_types })),
            { id: 'draft-1', name: 'Черновик', state: 'draft', task_types: ['ghost'] },
          ],
        },
      }
    }
    if (url.pathname === `/projects/${project}/queue`) {
      return {
        status: 200,
        body: {
          queue: fx
            .filter(f => (!statusOf(f).is_terminal && statusOf(f).roles.length > 0) || f.claimable)
            .map(f => ({ task: itemOf(f, fx), stage_run: { claimed_by_handle: f.holder ?? null }, claimable: f.claimable === true })),
        },
      }
    }
    if (url.pathname === `/projects/${project}/tasks`) {
      const p = url.searchParams
      const limit = Number(p.get('limit') ?? 200)
      if (sv.legacy) return { status: 200, body: { tasks: fx.slice(0, limit).map(f => itemOf(f, fx, false)) } }
      const q = (p.get('q') ?? '').toLowerCase()
      const parents = (p.get('parent') ?? '').split(',').filter(Boolean)
      let list = fx.filter(f => {
        const st = statusOf(f)
        if (p.get('top_level') === 'true' && f.parent) return false
        if (p.has('terminal') && st.is_terminal !== (p.get('terminal') === 'true')) return false
        if (p.has('has_role') && st.roles.length > 0 !== (p.get('has_role') === 'true')) return false
        if (p.has('in_work') && (segmentOf(f, fx) === 'work') !== (p.get('in_work') === 'true')) return false
        if (p.has('category') && !(p.get('category') ?? '').split(',').includes(st.category)) return false
        if (parents.length && !parents.includes(f.parent ?? '')) return false
        if (p.get('type') && wfNamed(f.wf).task_types[0] !== p.get('type')) return false
        if (p.get('assignee') && f.assignee !== p.get('assignee')) return false
        if (p.get('unassigned') === 'true' && f.assignee) return false
        if (q && !f.key.toLowerCase().includes(q) && !f.title.toLowerCase().includes(q)) return false
        return true
      })
      list = p.get('order') === 'updated' ? [...list].sort((a, b) => a.age - b.age) : list
      const off = Number(p.get('offset') ?? 0)
      return { status: 200, body: { tasks: list.slice(off, off + limit).map(f => itemOf(f, fx)), total: list.length } }
    }
    const w = /^\/workflows\/(.+)$/.exec(url.pathname)
    if (w) {
      const wf = WFS.find(x => x.id === w[1])
      return wf ? { status: 200, body: wf } : undefined
    }
    const t = /^\/tasks\/([A-Z]+-\d+)$/.exec(url.pathname)
    if (t) return TASKS[t[1] ?? ''] ? { status: 200, body: TASKS[t[1] ?? ''] } : { status: 404, body: { error: { message: 'задача не найдена' } } }
    return undefined
  }
}

const who = (project: string) => ({ principal: 'user:ann', authenticated: true, api: `${HOST}/v1`, project: { key: project, name: project } })

type Tree = { type?: string; props?: Record<string, unknown>; children?: unknown[] }

function strings(node: unknown, out: string[] = []): string[] {
  if (typeof node === 'string') {
    if (node) out.push(node)
    return out
  }
  if (!node || typeof node !== 'object') return out
  const el = node as Tree
  for (const k of ['label', 'text']) if (typeof el.props?.[k] === 'string') out.push(el.props[k] as string)
  for (const c of el.children ?? (el.props?.children as unknown[]) ?? []) strings(c, out)
  return out
}

function walk(node: unknown, fn: (el: Tree) => void) {
  if (!node || typeof node !== 'object') return
  const el = node as Tree
  fn(el)
  for (const c of el.children ?? (el.props?.children as unknown[]) ?? []) walk(c, fn)
}

// Ключи строк списка по порядку: кнопки «task-КЛЮЧ» верхнего уровня.
function rowKeys(tree: unknown): string[] {
  const out: string[] = []
  walk(tree, el => {
    const k = String(el.props?.key ?? '')
    if (el.type === 'Button' && k.startsWith('task-')) out.push(k.slice(5))
  })
  return out
}

function kidKeys(tree: unknown, parent: string): string[] {
  const out: string[] = []
  walk(tree, el => {
    const k = String(el.props?.key ?? '')
    if (el.type === 'Button' && k.startsWith(`kid-${parent}-`)) out.push(k.slice(`kid-${parent}-`.length))
  })
  return out
}

const text = (tree: unknown) => strings(tree).join('\n')

async function openTab(
  $: any,
  clock: { settle: () => Promise<void> },
  surface: 'terminal' | 'desktop' = 'terminal',
) {
  await $.session.start({ cwd: '/', surface, isInteractive: true })
  await tick(clock)
  await run($, 'tasks')
  await tick(clock)
  return $.ui.mount({ ...PANE, surface })
}

const forSurfaces = ['terminal', 'desktop'] as const

// AC-17a, AC-17c, AC-17o: сегменты по полям статуса из версий циклов, строки
// верхнего уровня, порядок по времени обновления; счётчики со всех сегментов.
test('NOVA: сегменты «в работе», «бэклог», «готово» по данным цикла, новые сверху', async ($, on) => {
  const sv: Server = { fixtures: nova() }
  const { clock } = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
  const ui = await openTab($, clock)

  // Сегменты считают все задачи проекта, дети эпика тоже (AC-17a, AC-17c).
  const expected = (seg: string) => inSegment(sv.fixtures, seg)
  const work = expected('work')
  expect(work.map(f => f.key)).toEqual(
    expect.arrayContaining(['NOVA-1', 'NOVA-4', 'NOVA-5', 'NOVA-6', 'NOVA-10', 'NOVA-13']),
  )

  // «В работе» открыт по умолчанию; кнопки несут счётчики.
  expect(rowKeys(await ui.drawn())).toEqual(work.map(f => f.key))
  // Эпик в «В работе» (дети идут), его неначатые дети — в «Бэклоге» (дети эпика
  // — строки своих сегментов), готовые — в «Готово».
  expect(expected('backlog').map(f => f.key)).toEqual(expect.arrayContaining(['NOVA-7', 'NOVA-8']))
  expect(expected('backlog').map(f => f.key)).not.toContain('NOVA-1')
  expect(expected('done').map(f => f.key)).toEqual(expect.arrayContaining(['NOVA-2', 'NOVA-3']))
  const sizes = { work: work.length, backlog: expected('backlog').length, done: expected('done').length }
  expect(sizes).toEqual({ work: 9, backlog: 16, done: 11 })
  for (const [label, n] of [[WORDS.segWork, sizes.work], [WORDS.segQueue, 3], [WORDS.segBacklog, sizes.backlog], [WORDS.segDone, sizes.done]] as const) {
    expect(await ui.find({ type: 'Button', text: `${label} ${n}` })).toBeDefined()
  }

  await ui.press({ key: 'seg-backlog' })
  await tick(clock)
  const backlog = expected('backlog')
  expect(rowKeys(await ui.drawn())).toEqual(backlog.slice(0, PAGE).map(f => f.key))
  // Отменённые в сегменты не попадают.
  await ui.press({ key: 'seg-done' })
  await tick(clock)
  const done = rowKeys(await ui.drawn())
  expect(done).toEqual(expected('done').slice(0, PAGE).map(f => f.key))
  expect(done).not.toContain('NOVA-50')
  expect(done).not.toContain('NOVA-9')
  await ui.unmount()

  // Тот же выбор на Desktop.
  const onDesktop = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(rowKeys(await onDesktop.drawn())).toEqual(done)
  await onDesktop.unmount()
})

// Страница по десять и листание: «показано 1–10 из 16», дальше — вторая.
test('NOVA: страницы по десять, вперёд и назад', async ($, on) => {
  const sv: Server = { fixtures: nova(), hits: [] }
  const { clock } = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
  const ui = await openTab($, clock)
  await ui.press({ key: 'seg-backlog' })
  await tick(clock)
  const backlog = inSegment(sv.fixtures, 'backlog')
  expect(backlog).toHaveLength(16)
  expect(text(await ui.drawn())).toContain(`${WORDS.shown} 1–10 ${WORDS.of} 16`)
  expect(await ui.find({ type: 'Button', text: /вперёд/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /назад/ })).toBeUndefined()

  await ui.press({ key: 'tasks-next' })
  await tick(clock)
  expect(rowKeys(await ui.drawn())).toEqual(backlog.slice(10).map(f => f.key))
  expect(text(await ui.drawn())).toContain(`${WORDS.shown} 11–16 ${WORDS.of} 16`)
  expect(await ui.find({ type: 'Button', text: /вперёд/ })).toBeUndefined()
  expect(sv.hits?.some(h => h.includes('offset=10'))).toBe(true)

  await ui.press({ key: 'tasks-prev' })
  await tick(clock)
  expect(rowKeys(await ui.drawn())).toEqual(backlog.slice(0, 10).map(f => f.key))
  await ui.unmount()
})

// AC-17b: «Можно забрать» — ровно claimable из очереди.
test('NOVA: «Можно забрать» — ключи claimable из очереди проекта', async ($, on) => {
  const sv: Server = { fixtures: nova() }
  const { clock } = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
  const ui = await openTab($, clock)
  await ui.press({ key: 'seg-queue' })
  await tick(clock)
  const want = sv.fixtures.filter(f => f.claimable).sort((a, b) => a.age - b.age).map(f => f.key)
  expect(want).toHaveLength(3)
  expect(rowKeys(await ui.drawn())).toEqual(want)
  await ui.unmount()
})

// Пустая очередь — подпись, а не пустота.
test('NOVA: пустая очередь — «сейчас нечего забрать»', async ($, on) => {
  const empty: Server = { fixtures: nova().map(f => ({ ...f, claimable: false })) }
  const w = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(empty, 'NOVA') })
  const ui = await openTab($, w.clock, 'desktop')
  await ui.press({ key: 'seg-queue' })
  await tick(w.clock)
  expect(text(await ui.drawn())).toContain(WORDS.queueEmpty)
  await ui.unmount()
})

// AC-17d: типы — объединение task_types версий проекта, черновик не в счёт;
// выбор типа сужает выборку на сервере.
test('NOVA: типы из версий циклов, выбор типа', async ($, on) => {
  const sv: Server = { fixtures: nova(), hits: [] }
  const { clock } = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
  const ui = await openTab($, clock)
  let select: Tree | undefined
  walk(await ui.drawn(), el => {
    if (el.type === 'Select' && el.props?.key === 'tasks-type') select = el
  })
  const options = ((select?.props?.options as { value: string; label: string }[]) ?? []).map(o => o.value)
  const declared = [...new Set(WFS.flatMap(w => w.task_types))].sort()
  expect(options).toEqual(['', ...declared])
  expect(options).not.toContain('ghost')

  await ui.select({ key: 'tasks-type', value: 'release' })
  await tick(clock)
  expect(rowKeys(await ui.drawn())).toEqual(['NOVA-11'])
  expect(sv.hits?.some(h => h.includes('type=release'))).toBe(true)
  await ui.unmount()
})

// AC-17e: поиск идёт на сервере по всему проекту, без сегмента; точный ключ
// открывает карточку.
test('NOVA: поиск по слову заголовка на сервере, точный ключ открывает карточку', async ($, on) => {
  const sv: Server = { fixtures: nova(), hits: [] }
  const w = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
  const ui = await openTab($, w.clock)

  await ui.input({ key: 'tasks-search', text: 'КОДИРОВКИ' })
  await tick(w.clock)
  expect(rowKeys(await ui.drawn())).toEqual(['NOVA-3'])
  const hit = sv.hits?.filter(h => h.includes('/tasks?')).at(-1) ?? ''
  expect(hit).toContain('q=')
  expect(hit).not.toContain('terminal=')
  expect(hit).not.toContain('top_level=')
  expect(text(await ui.drawn())).toContain(WORDS.projectSearch)

  await ui.input({ key: 'tasks-search', text: 'nova-4' })
  await tick(w.clock)
  // Часть ключа находит всё, что начинается с NOVA-4 (NOVA-4 и NOVA-40…48).
  expect(rowKeys(await ui.drawn()).slice(0, 2).sort()).toEqual(['NOVA-4', 'NOVA-40'].sort())

  await ui.input({ key: 'tasks-search', text: 'NOVA-10' })
  await tick(w.clock)
  expect(w.j.urls).toContain(`${HOST}/v1/tasks/NOVA-10`)
  expect(rowKeys(await ui.drawn())).toContain('NOVA-10')

  await ui.input({ key: 'tasks-search', text: 'нет такой задачи' })
  await tick(w.clock)
  expect(text(await ui.drawn())).toContain(WORDS.nothingFound)
  await ui.press({ key: 'tasks-reset' })
  await tick(w.clock)
  expect(rowKeys(await ui.drawn()).length).toBeGreaterThan(0)
  await ui.unmount()
})

// AC-17f и ответ человека Q4: клик открывает карточку под строкой и
// добавляет ключ в «Этот чат»; записи на сервер нет.
test('NOVA: клик по строке — карточка под строкой и ключ в «Этом чате»', async ($, on) => {
  const sv: Server = { fixtures: nova() }
  const w = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
  const ui = await openTab($, w.clock)
  await ui.press({ key: 'task-NOVA-10' })
  await tick(w.clock)
  expect(w.j.urls).toContain(`${HOST}/v1/tasks/NOVA-10`)
  const states = w.j.states.filter(s => s.includes('"keys"')).at(-1) ?? ''
  expect(states).toContain('NOVA-10')
  expect(text(await ui.drawn())).toContain(WORDS.beforeTransition)
  // Повторный клик сворачивает карточку, ключ остаётся в чате.
  await ui.press({ key: 'task-NOVA-10' })
  await tick(w.clock)
  expect(text(await ui.drawn())).not.toContain(WORDS.beforeTransition)
  // Ничего не записано на сервер: только GET.
  expect(w.j.urls.every(u => u.startsWith(HOST))).toBe(true)
  await ui.press({ key: 'tab-chat' })
  expect(await ui.find({ type: 'Text', text: /NOVA-10/ })).toBeDefined()
  await ui.unmount()
})

// AC-17g: строка с вложенными — полоса из четырёх частей, «готово из живых»,
// дети свёрнуты до трёх, «… ещё N» = все дети минус три.
for (const [project, fixtures, parent, all, live, done] of [
  ['NOVA', nova(), 'NOVA-1', 8, 7, 2],
  ['FORGE', forge(), 'FORGE-1', 7, 6, 2],
] as const) {
  test(`${project}: вложенные — полоса, три ребёнка, «… ещё N» раскрывает остальных`, async ($, on) => {
    const sv: Server = { fixtures: [...fixtures] }
    const w = world(on, { whoami: who(project), now: NOW, routes: routes(sv, project) })
    const ui = await openTab($, w.clock)
    const tree = await ui.drawn()
    expect(rowKeys(tree)).toContain(parent)
    expect(text(tree)).toContain(`${done} ${WORDS.of} ${live}`)
    // Полоса — одна строка блоков.
    expect(text(tree)).toMatch(/█+/)
    expect(kidKeys(tree, parent)).toHaveLength(3)
    expect(await ui.find({ type: 'Button', text: `… ${WORDS.moreKids} ${all - 3}` })).toBeDefined()

    await ui.press({ key: `more-${parent}` })
    await tick(w.clock)
    expect(kidKeys(await ui.drawn(), parent)).toHaveLength(all)
    expect(await ui.find({ type: 'Button', text: WORDS.fewer })).toBeDefined()
    await ui.press({ key: `more-${parent}` })
    await tick(w.clock)
    expect(kidKeys(await ui.drawn(), parent)).toHaveLength(3)
    await ui.unmount()
  })
}

// AC-17m: второй проект с другим циклом — те же сегменты, типы и подписи
// статусов, без правок кода.
test('FORGE: сегменты, типы и подписи статусов из своих циклов', async ($, on) => {
  const sv: Server = { fixtures: forge() }
  const w = world(on, { whoami: who('FORGE'), now: NOW, routes: routes(sv, 'FORGE') })
  const ui = await openTab($, w.clock)
  const want = (seg: string) => inSegment(sv.fixtures, seg).map(f => f.key)
  expect(want('work')).toEqual(expect.arrayContaining(['FORGE-1', 'FORGE-2', 'FORGE-3', 'FORGE-4']))
  expect(want('done')).toEqual(expect.arrayContaining(['FORGE-5', 'FORGE-6']))
  expect(rowKeys(await ui.drawn())).toEqual(want('work'))
  expect(text(await ui.drawn())).toContain(statusOf(sv.fixtures[0] as Fx).name)
  for (const seg of ['backlog', 'done'] as const) {
    await ui.press({ key: `seg-${seg}` })
    await tick(w.clock)
    expect(rowKeys(await ui.drawn())).toEqual(want(seg))
  }
  // Подписи — из циклов задач: в строке Incident стоит статус Incident.
  await ui.press({ key: 'seg-work' })
  await tick(w.clock)
  expect(text(await ui.drawn())).toContain(statusOf(sv.fixtures.find(f => f.key === 'FORGE-9') as Fx).name)
  // Типы — из версий циклов проекта.
  let select: Tree | undefined
  walk(await ui.drawn(), el => {
    if (el.type === 'Select' && el.props?.key === 'tasks-type') select = el
  })
  expect(((select?.props?.options as { value: string }[]) ?? []).map(o => o.value)).toContain('incident')
  await ui.unmount()
})

// AC-17j: сервер прошлого выпуска. Листания и полей нет: мод читает выборку
// целиком, делит её по версиям циклов, сегмента «ждут апрува» не показывает,
// поиск — по загруженным, с подписью.
for (const [project, fixtures] of [['NOVA', nova()], ['FORGE', forge()]] as const) {
  test(`${project}: сервер прошлого выпуска — сегменты по версиям циклов, поиск по загруженным`, async ($, on) => {
    const sv: Server = { fixtures, legacy: true, hits: [] }
    const w = world(on, { whoami: who(project), now: NOW, routes: routes(sv, project) })
    const ui = await openTab($, w.clock)
    const want = (seg: string) => inSegment(fixtures, seg).map(f => f.key)
    expect(rowKeys(await ui.drawn())).toEqual(want('work').slice(0, PAGE))
    const n = (seg: string) => want(seg).length
    expect(await ui.find({ type: 'Button', text: `${WORDS.segBacklog} ${n('backlog')}` })).toBeDefined()
    // Версии циклов читаются по одной на цикл.
    const versions = (sv.hits ?? []).filter(h => h.startsWith('/workflows/'))
    expect(new Set(versions).size).toBe(versions.length)
    expect(versions.length).toBeGreaterThan(0)

    await ui.press({ key: 'seg-backlog' })
    await tick(w.clock)
    expect(rowKeys(await ui.drawn())).toEqual(want('backlog').slice(0, PAGE))

    await ui.input({ key: 'tasks-search', text: fixtures[1]?.title.slice(0, 6) ?? '' })
    await tick(w.clock)
    expect(text(await ui.drawn())).toContain(WORDS.loadedOnly)
    expect(text(await ui.drawn())).not.toContain(WORDS.awaiting)
    // Эпик и его дети читаются из той же выборки.
    await ui.input({ key: 'tasks-search', text: '' })
    await tick(w.clock)
    await ui.press({ key: 'seg-work' })
    await tick(w.clock)
    const parent = project === 'NOVA' ? 'NOVA-1' : 'FORGE-1'
    expect(rowKeys(await ui.drawn())).toContain(parent)
    expect(kidKeys(await ui.drawn(), parent)).toHaveLength(3)
    await ui.unmount()
  })
}

// AC-17i: запросов на открытие вкладки не больше шести и их число не растёт
// с числом задач.
const opened: number[] = []
const bigNova = () => {
  const big = nova()
  for (let i = 0; i < 200; i++) big.push({ key: `NOVA-${100 + i}`, title: `Массовая ${i}`, wf: 'Simple', status: i % 2 ? 'new' : 'development', age: 9000 + i })
  return big
}
for (const [name, make] of [['14 задач', nova], ['214 задач', bigNova]] as const) {
  test(`запросов на открытие вкладки не больше шести (${name})`, async ($, on) => {
    const sv: Server = { fixtures: make(), hits: [] }
    const w = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
    const ui = await openTab($, w.clock)
    await ui.unmount()
    // /instance — сведения о сервере при старте сессии, не вкладка; фоновая
    // выборка «Ждут меня» при старте — тоже не открытие вкладки (решение
    // человека 2026-10-30).
    const tab = (sv.hits ?? []).filter(h => h !== '/instance' && !h.endsWith('/approvals'))
    opened.push(tab.length)
    expect(tab.length).toBeLessThanOrEqual(6)
  })
}
test('число запросов не зависит от числа задач', () => {
  expect(opened).toHaveLength(2)
  expect(opened[1]).toBe(opened[0])
})

// AC-17n: токен не попадает ни в состояние, ни в тосты, ни в строки вкладки.
test('токен не попадает в состояние, тосты и строки вкладки', async ($, on) => {
  const sv: Server = { fixtures: nova() }
  const w = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
  const ui = await openTab($, w.clock)
  await ui.press({ key: 'task-NOVA-10' })
  await ui.input({ key: 'tasks-search', text: 'Идея' })
  await tick(w.clock)
  const everything = [...w.j.states, ...w.j.toasts, ...w.j.statuses.map(String), JSON.stringify(await ui.drawn())].join('\n')
  expect(everything).not.toContain(TOKEN)
  expect(w.j.auths.filter(Boolean).every(a => a === `Bearer ${TOKEN}`)).toBe(true)
  await ui.unmount()
})

// Ошибка сервера не стирает прежний список.
test('ошибка чтения списка: прежние строки на месте, текст ошибки виден', async ($, on) => {
  const sv: Server = { fixtures: nova() }
  let broken = false
  const inner = routes(sv, 'NOVA')
  const w = world(on, {
    whoami: who('NOVA'),
    now: NOW,
    routes: p => (broken && p.includes('/tasks?') ? { status: 500, body: { error: { message: 'сломалось' } } } : inner(p)),
  })
  const ui = await openTab($, w.clock)
  const before = rowKeys(await ui.drawn())
  expect(before.length).toBeGreaterThan(0)
  broken = true
  await ui.press({ key: 'seg-done' })
  await tick(w.clock)
  expect(text(await ui.drawn())).toContain('сломалось')
  expect(rowKeys(await ui.drawn())).toEqual(before)
  await ui.unmount()
})

// Слова вкладки — словарь плагина, данные задач и цикла; других нет.
test('подписи вкладки: словарь, ключи, заголовки и подписи циклов', async ($, on) => {
  const sv: Server = { fixtures: nova() }
  const w = world(on, { whoami: who('NOVA'), now: NOW, routes: routes(sv, 'NOVA') })
  const ui = await openTab($, w.clock)
  const own = new Set<string>([...Object.values(WORDS), 'Этот чат', 'Ждут меня', 'Задачи'])
  const data = new Set<string>()
  for (const f of sv.fixtures) {
    data.add(f.key)
    data.add(f.title)
    data.add(statusOf(f).name)
    if (f.holder) data.add(f.holder)
  }
  const wfTypes = new Set(WFS.flatMap(x => x.task_types))
  const known = (s: string): boolean => {
    let rest = s
    for (const w2 of [...own, ...data, ...wfTypes].sort((a, b) => b.length - a.length)) rest = rest.split(w2).join('')
    // Остаются разделители, значки, числа, время и подписи сегментов с числом.
    return /^[\s·–—…‹›█░●○◈⏸:,0-9/-]*(мин|ч|дн|вчера|только что|[\s·–—…‹›█░●○◈⏸:,0-9/-])*$/.test(rest)
  }
  const bad = strings(await ui.drawn()).filter(s => !known(s) && !s.startsWith(HOST))
  expect(bad).toEqual([])
  await ui.unmount()
})


// AC-17a, AC-17c: ключи всех страниц трёх сегментов вместе с отменёнными дают
// весь проект, как список задач в CLI: ни одна задача, в том числе ребёнок
// эпика, не выпадает, и ни одна не стоит в двух сегментах. Ожидание — по всем
// задачам проекта, а не по задачам без родителя.
for (const [project, fixtures] of [['NOVA', nova()], ['FORGE', forge()]] as const) {
  test(`${project}: сегменты вместе покрывают все задачи проекта, дети эпика тоже`, async ($, on) => {
    const sv: Server = { fixtures: [...fixtures] }
    const w = world(on, { whoami: who(project), now: NOW, routes: routes(sv, project) })
    const ui = await openTab($, w.clock)
    const seen: string[] = []
    for (const seg of ['work', 'backlog', 'done'] as const) {
      await ui.press({ key: `seg-${seg}` })
      await tick(w.clock)
      const keys: string[] = []
      for (;;) {
        keys.push(...rowKeys(await ui.drawn()))
        if (!(await ui.find({ type: 'Button', text: /вперёд/ }))) break
        await ui.press({ key: 'tasks-next' })
        await tick(w.clock)
      }
      expect(keys).toEqual(inSegment(fixtures, seg).map(f => f.key))
      seen.push(...keys)
    }
    const cancelled = fixtures.filter(f => statusOf(f).category === 'cancelled').map(f => f.key)
    expect([...seen, ...cancelled].sort()).toEqual(fixtures.map(f => f.key).sort())
    expect(new Set(seen).size).toBe(seen.length)
    // Ребёнок эпика — строка своего сегмента, а не только вложенный.
    const kid = fixtures.find(f => f.parent && segmentOf(f, fixtures) === 'work') as Fx
    expect(seen).toContain(kid.key)
    await ui.unmount()
  })
}
