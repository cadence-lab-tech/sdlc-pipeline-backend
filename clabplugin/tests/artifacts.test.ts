import { expect, test } from 'claude-code/testing'

import { chipsOf, clip, MAX_BYTES, MAX_LINES, stageTabs, whenRange } from '../hooks/artifact'
import type { ApiArtifact } from '../hooks/artifact'
import { cardOf } from '../hooks/card'
import type { TaskDetail, Workflow } from '../hooks/card'
import { CAPTURED_AT, TASKS, WORKFLOWS } from './stack'
import { GATE_WORD, GLYPH, MONTHS, WORDS } from '../hooks/vocab'
import { HOST, PANE, run, TOKEN, world } from './world'
import type { Reply } from './world'

// Артефакты этапов CLAB-325 на снимке стека CLAB-324: проект NOVA (форма
// нашего цикла) и FORGE (вымышленный, с необязательным этапом). Сервер —
// подставной: списки, тексты и вложения собраны здесь, ничего не ходит в сеть.

const NOW = Date.parse(CAPTURED_AT) + 60_000
const tick = async (clock: { settle: () => Promise<void> }) => {
  for (let i = 0; i < 6; i++) await clock.settle()
}
const clone = <T>(x: T): T => JSON.parse(JSON.stringify(x)) as T

const sha256 = async (s: string) =>
  [...new Uint8Array(await (globalThis as any).crypto.subtle.digest('SHA-256', new TextEncoder().encode(s)))].map(b => b.toString(16).padStart(2, '0')).join('')

// Все строки в поддереве: дети, подписи, текст Markdown и Code.
function strings(node: unknown, out: string[] = []): string[] {
  if (typeof node === 'string') {
    if (node) out.push(node)
    return out
  }
  if (!node || typeof node !== 'object') return out
  const el = node as { type?: string; props?: Record<string, unknown>; children?: unknown[] }
  const p = el.props ?? {}
  for (const k of ['label', 'alt', 'text', 'source']) if (typeof p[k] === 'string') out.push(p[k] as string)
  for (const c of el.children ?? (p.children as unknown[]) ?? []) strings(c, out)
  return out
}

// --- Данные: NOVA-4 с вложенной перепроверкой, версиями и вложениями ---------------

const R = {
  analysis: 'a01b4da9-52c1-4803-ac8d-39bd4ddf0468',
  dev1: '8930b5c2-0f1e-45d0-998e-fb32954b1dbf',
  test: 'fb66f002-62a2-466b-b3cc-57d9dbc02023',
  dev2: '793deb2a-7623-4cb6-b34c-7ecdf5a8cce3',
  review: 'c7d1e0aa-0000-4000-8000-000000000001',
}

function nova4(): TaskDetail & Record<string, any> {
  const d = clone(TASKS['NOVA-4']) as TaskDetail & Record<string, any>
  const at: Record<string, [string, string, string]> = {
    [R.analysis]: ['analytics', '2026-10-29T18:40:00Z', '2026-10-29T19:05:00Z'],
    [R.dev1]: ['developer', '2026-10-29T19:10:00Z', '2026-10-29T20:00:00Z'],
    [R.test]: ['tester', '2026-10-29T20:05:00Z', '2026-10-29T20:30:00Z'],
    [R.dev2]: ['developer', '2026-10-29T20:35:00Z', ''],
  }
  for (const r of d.stage_runs ?? []) {
    const x = at[String(r.id)]
    if (!x) continue
    r.role_key = x[0]
    r.entered_at = x[1]
    if (x[2]) r.finished_at = x[2]
    else delete r.finished_at
  }
  d.stage_runs.push({
    id: R.review,
    status_key: 'development',
    kind: 'review',
    parent_run_id: R.dev1,
    attempt: 1,
    role_key: 'reviewer',
    claimed_by_handle: 'review-agent',
    entered_at: '2026-10-29T19:50:00Z',
    finished_at: '2026-10-29T19:58:00Z',
  })
  return d
}

const art = (id: string, kind: string, title: string, version: number, runId: string, extra: Partial<ApiArtifact> = {}): ApiArtifact => ({
  id,
  kind,
  title,
  format: 'markdown',
  version,
  status: 'submitted',
  stage_run_id: runId,
  created_at: `2026-10-29T1${version}:00:00Z`,
  ...extra,
})

const ID = {
  reqV1: 'aaaaaaaa-0000-4000-8000-000000000001',
  reqV2: 'aaaaaaaa-0000-4000-8000-000000000002',
  cases: 'aaaaaaaa-0000-4000-8000-000000000003',
  sumV1: 'aaaaaaaa-0000-4000-8000-000000000004',
  sumV2: 'aaaaaaaa-0000-4000-8000-000000000005',
  long: 'aaaaaaaa-0000-4000-8000-000000000006',
  report: 'aaaaaaaa-0000-4000-8000-000000000007',
  json: 'aaaaaaaa-0000-4000-8000-000000000008',
  html: 'bbbbbbbb-0000-4000-8000-000000000001',
  svg: 'bbbbbbbb-0000-4000-8000-000000000002',
  png: 'bbbbbbbb-0000-4000-8000-000000000003',
}

const LONG = Array.from({ length: 300 }, (_, i) => `Строка ${i + 1} ревью`).join('\n')

const BODIES: Record<string, string> = {
  [ID.reqV1]: '# Требования\n\nПервая версия.',
  [ID.reqV2]: '# Требования\n\nВторая версия: **уточнено** `GET /v1/x`.\n\n- один\n- два',
  [ID.cases]: '# Сценарии\n\nUC-1',
  [ID.sumV1]: '# Разбор\n\nпервый заход',
  [ID.sumV2]: '# Разбор\n\nвторой заход после возврата',
  [ID.long]: LONG,
  [ID.report]: '# Отчёт тестов\n\nвсё зелёное',
  [ID.json]: '{"pass": 12, "fail": 0}',
}

const LIST: ApiArtifact[] = [
  art(ID.reqV1, 'analysis.requirements', 'Бизнес-требования', 1, R.analysis, { status: 'superseded' }),
  art(ID.reqV2, 'analysis.requirements', 'Бизнес-требования', 2, R.analysis, { supersedes_id: ID.reqV1 }),
  art(ID.cases, 'analysis.use_cases', 'Сценарии', 1, R.analysis),
  art(ID.sumV1, 'dev.change_summary', 'Разбор изменений', 1, R.dev1, { status: 'superseded' }),
  art(ID.sumV2, 'dev.change_summary', 'Разбор изменений', 2, R.dev2, { supersedes_id: ID.sumV1 }),
  art(ID.long, 'dev.code_review', 'Ревью кода', 1, R.dev2),
  art(ID.json, 'dev.test_report', 'Результаты прогона тестов', 1, R.dev2, { format: 'json' }),
  art(ID.report, 'test.report', 'Отчёт тестировщика', 1, R.test),
]

const ATTACHMENTS = [
  { id: ID.html, title: 'mockups.html', file_name: 'mockups.html', media_type: 'text/html', size_bytes: 12 * 1024, version: 1, added_by: 'user-a', content_path: `/v1/artifacts/${ID.html}/content` },
  { id: ID.svg, title: 'flow.svg', file_name: 'flow.svg', media_type: 'image/svg+xml', size_bytes: 3 * 1024, version: 1, added_by: 'user-b', content_path: `/v1/artifacts/${ID.svg}/content` },
  { id: ID.png, title: 'shot.png', file_name: 'shot.png', media_type: 'image/png', size_bytes: 568 * 1024, version: 1, added_by: 'user-a', content_path: `/v1/artifacts/${ID.png}/content` },
]

// Сервер по снимку плюс ручки артефактов; over подменяет ответы.
function server(over?: (path: string) => Reply | undefined, tasks: Record<string, any> = { ...TASKS, 'NOVA-4': nova4() }) {
  return (path: string): Reply | undefined => {
    const own = over?.(path)
    if (own) return own
    const t = /^\/tasks\/([A-Z]+-\d+)$/.exec(path)
    if (t && tasks[t[1] ?? '']) return { status: 200, body: tasks[t[1] ?? ''] }
    const w = /^\/workflows\/(.+)$/.exec(path)
    if (w && WORKFLOWS[w[1] ?? '']) return { status: 200, body: WORKFLOWS[w[1] ?? ''] }
    if (path === '/instance') return { status: 200, body: { version: 'v9.9.9' } }
    if (path === '/tasks/NOVA-4/artifacts?include_superseded=true') return { status: 200, body: { artifacts: LIST } }
    if (path === '/tasks/NOVA-4/attachments') return { status: 200, body: { attachments: ATTACHMENTS } }
    const c = /^\/artifacts\/([^/]+)\/content$/.exec(path)
    if (c && BODIES[c[1] ?? '']) return { status: 200, text: BODIES[c[1] ?? ''] }
    return undefined
  }
}

// Кнопки по началу ключа: текст каждой.
const buttons = async (ui: { findAll: (q: { type: string }) => Promise<any[]> }, prefix: string) =>
  (await ui.findAll({ type: 'Button' })).filter(b => String(b.key ?? b.props?.key ?? '').startsWith(prefix)).map(b => String(b.text))

const contentUrls = (urls: string[]) => urls.filter(u => /\/artifacts\/[^/]+\/content$/.test(u))
const countOf = (urls: string[], id: string) => urls.filter(u => u.endsWith(`/artifacts/${id}/content`)).length

// Всё, что мод пишет в разговор и ввод: перехват вызовов $.
function spies(on: any) {
  const fills: any[] = []
  const copies: any[] = []
  const appended: unknown[] = []
  const tools: unknown[] = []
  on('prompt.fill', (_: unknown, e: any) => {
    fills.push(e)
    return { isFilled: true }
  })
  on('ui.copy', (_: unknown, e: any) => {
    copies.push(e)
    return { value: { isCopied: true } }
  })
  on('session.append', (_: unknown, e: unknown) => {
    appended.push(e)
    return { message: (e as any).message, uuid: 'u' }
  })
  on('tool.register', (_: unknown, e: unknown) => {
    tools.push(e)
    return { value: { tool: 'x' } }
  })
  return { fills, copies, appended, tools }
}

async function opened(on: any, $: any, opts: Parameters<typeof world>[1] = {}, surface: 'desktop' | 'terminal' = 'desktop', key = 'NOVA-4', stage = 'development') {
  const w = world(on, { now: NOW, routes: server(), ...opts })
  await $.session.start({ cwd: '/', surface, isInteractive: true })
  await run($, key)
  await tick(w.clock)
  const ui = await $.ui.mount({ ...PANE, surface })
  await ui.press({ key: `stage-open-${key}-${stage}` })
  await tick(w.clock)
  return { ...w, ui }
}

// --- AC-8.1, AC-8.7, AC-8.8: вкладки и прогоны -------------------------------------

test('вкладки NOVA-4: статусы цикла с прогонами, попытки, вложенная перепроверка, возврат', () => {
  const d = nova4()
  const wf = WORKFLOWS[d.task.workflow_id] as Workflow
  const tabs = stageTabs(d, wf)
  // Начальный статус без роли и непройденные этапы вкладок не получают.
  expect(tabs.map(t => [t.name, t.attempts])).toEqual([
    ['Analysis', 1],
    ['Development', 2],
    ['Testing', 1],
  ])
  const dev = tabs[1]!
  expect(dev.runs.map(r => [r.attempt, r.role, r.holder, r.running])).toEqual([
    [1, 'developer', 'dev-agent', false],
    [2, 'developer', 'dev-agent', true],
  ])
  // Перепроверка — под родительским прогоном, а не отдельной вкладкой.
  expect(dev.runs[0]?.reviews.map(r => [r.role, r.holder])).toEqual([['reviewer', 'review-agent']])
  expect(dev.runs[1]?.reviews).toEqual([])
  // Возврат: из какого этапа и по какой причине.
  expect(dev.runs[1]?.back).toEqual({ fromName: 'Testing', reason: 'нет теста на повторную выгрузку' })
  expect(dev.runs[0]?.back).toBeNull()
  // Карточка несёт те же вкладки и привязку прогонов к статусам.
  const c = cardOf(d, wf, HOST)
  expect(c.stages).toEqual(tabs)
  expect(c.runStages[R.review]).toBe('development')
})

test('второй проект: у пропущенного необязательного этапа вкладки нет, у включённого с прогоном — есть', () => {
  const d = clone(TASKS['FORGE-4']) as TaskDetail
  const wf = WORKFLOWS[d.task.workflow_id] as Workflow
  expect(stageTabs(d, wf).map(t => t.name)).toEqual(['Intake', 'Build', 'Verify'])
  // Design включён задачей и пройден — вкладка появляется между Intake и Build.
  d.task.optional_stages = { design: true }
  d.stage_runs = [
    ...(d.stage_runs ?? []),
    { id: 'd1', status_key: 'design', kind: 'stage', attempt: 1, finished_at: '2026-10-30T11:12:23.400Z' },
  ]
  expect(stageTabs(d, wf).map(t => t.name)).toEqual(['Intake', 'Design', 'Build', 'Verify'])
})

test('время прогона: дата и интервал, на разных сутках — обе даты', () => {
  expect(whenRange('2026-10-29T18:40:00Z', '2026-10-29T19:05:00Z')).toMatch(/^29 окт, \d\d:\d\d–\d\d:\d\d$/)
  expect(whenRange('2026-10-29T18:40:00Z', null)).toMatch(/^29 окт, \d\d:\d\d$/)
  expect(whenRange('2026-10-01T12:00:00Z', '2026-10-05T12:00:00Z')).toMatch(/^\d+ окт, \d\d:\d\d – \d+ окт, \d\d:\d\d$/)
  expect(whenRange(null, null)).toBeNull()
})

// --- AC-8.1, AC-8.3: вкладка, чипы, версии --------------------------------------------

test('клик по этапу: вкладки, прогоны со вложенной перепроверкой и возвратом, чипы, текст текущей версии', async ($, on) => {
  const { j, ui } = await (async () => {
    const out = await opened(on, $)
    return out
  })()
  expect(await ui.find({ type: 'Button', key: 'view-back' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /^› Development$/ })).toBeDefined()
  const tabs = (await buttons(ui, 'stage-'))
  expect(tabs).toEqual(['Analysis', 'Development ×2', 'Testing'])
  const runs = (await ui.findAll({ type: 'Text', text: /^Прогон / })).map(t => t.text)
  expect(runs).toHaveLength(2)
  expect(runs[0]).toMatch(/^Прогон 1 · developer · dev-agent · 29 окт, \d\d:\d\d–\d\d:\d\d$/)
  expect(runs[1]).toMatch(/^Прогон 2 · developer · dev-agent · 29 окт, \d\d:\d\d · идёт$/)
  expect(await ui.find({ type: 'Text', text: /^↳ перепроверка · reviewer · review-agent · 29 окт, \d\d:\d\d–\d\d:\d\d$/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /Вернули из Testing: «нет теста на повторную выгрузку»/ })).toBeDefined()
  // Чипы — артефакты этапа по kind, подпись — заголовок; чужих этапов нет.
  const chips = (await buttons(ui, 'kind-'))
  expect(chips).toEqual(['Разбор изменений', 'Ревью кода', 'Результаты прогона тестов'])
  // Первый чип открыт: текущая версия v2 из двух.
  expect((await ui.find({ type: 'Markdown' }))?.text).toBe(BODIES[ID.sumV2])
  expect(await ui.find({ type: 'Select', key: 'version' })).toBeDefined()
  // Один запрос списка, один вложений, один содержимого.
  expect(j.urls.filter(u => u.includes('/artifacts?include_superseded=true'))).toHaveLength(1)
  expect(j.urls.filter(u => u.endsWith('/tasks/NOVA-4/attachments'))).toHaveLength(1)
  expect(contentUrls(j.urls)).toHaveLength(1)
  await ui.unmount()
})

test('версии по supersedes_id: Select, текст совпадает с серверным по sha256, вернуться — без запроса', async ($, on) => {
  const { j, ui } = await opened(on, $)
  const select = await ui.find({ type: 'Select', key: 'version' })
  expect((select?.props.options as { value: string; label: string }[]).map(o => o.label)).toEqual(['v2 · текущая', 'v1'])
  expect(select?.props.value).toBe(ID.sumV2)
  expect(await sha256(String((await ui.find({ type: 'Markdown' }))?.text))).toBe(await sha256(BODIES[ID.sumV2] ?? ''))

  await ui.select({ key: 'version', value: ID.sumV1 })
  expect(await sha256(String((await ui.find({ type: 'Markdown' }))?.text))).toBe(await sha256(BODIES[ID.sumV1] ?? ''))
  expect((await ui.find({ type: 'Select', key: 'version' }))?.props.value).toBe(ID.sumV1)
  const after = contentUrls(j.urls).length
  expect(countOf(j.urls, ID.sumV1)).toBe(1)

  // Версия уже получена — сервер второй раз не спрашивается.
  await ui.select({ key: 'version', value: ID.sumV2 })
  await ui.select({ key: 'version', value: ID.sumV1 })
  await ui.select({ key: 'version', value: ID.sumV2 })
  expect(contentUrls(j.urls)).toHaveLength(after)
  expect(countOf(j.urls, ID.sumV2)).toBe(1)

  // Цепочка собрана через этапы: v1 сдана на первой попытке, v2 — на второй.
  const c = chipsOf(LIST, nova4().stage_runs!.reduce((m: Record<string, string>, r) => ({ ...m, [String(r.id)]: String(r.status_key) }), {}), 'development')
  expect(c.find(x => x.kind === 'dev.change_summary')?.versions.map(v => [v.art.version, v.current])).toEqual([
    [1, false],
    [2, true],
  ])
  await ui.unmount()
})

test('чип другого вида и вкладка другого этапа: свои артефакты, формат json — кодом', async ($, on) => {
  const { j, ui } = await opened(on, $)
  await ui.press({ key: 'kind-dev.test_report' })
  expect((await ui.find({ type: 'Code' }))?.text).toBe(BODIES[ID.json])
  expect(await ui.find({ type: 'Code' })).toMatchObject({ props: { language: 'json' } })
  expect(await ui.find({ type: 'Markdown' })).toBeUndefined()
  // Версии одна — Select нет, номер текстом.
  expect(await ui.find({ type: 'Select', key: 'version' })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: 'v1' })).toBeDefined()

  await ui.press({ key: 'stage-analysis' })
  expect((await buttons(ui, 'kind-'))).toEqual(['Бизнес-требования', 'Сценарии'])
  expect((await ui.find({ type: 'Markdown' }))?.text).toBe(BODIES[ID.reqV2])
  expect(await ui.find({ type: 'Text', text: /^Прогон 1 · analytics · / })).toBeDefined()
  await ui.press({ key: 'stage-testing' })
  expect((await ui.find({ type: 'Markdown' }))?.text).toBe(BODIES[ID.report])
  // Список читался один раз: вкладки и чипы — из памяти.
  expect(j.urls.filter(u => u.includes('/artifacts?include_superseded=true'))).toHaveLength(1)
  await ui.unmount()
})

// --- AC-8.2: предел отрисовки ------------------------------------------------------------

test('предел отрисовки: 60 строк или 8 КБ, что раньше; границы и ограждение кода', () => {
  const lines = (n: number, w = 10) => Array.from({ length: n }, (_, i) => `${i}`.padEnd(w, 'x')).join('\n')
  expect(clip(lines(MAX_LINES))).toEqual({ text: lines(MAX_LINES), hidden: 0 })
  const over = clip(lines(MAX_LINES + 1))
  expect(over.hidden).toBe(1)
  expect(over.text.split('\n')).toHaveLength(MAX_LINES)
  // 8 КБ раньше 60 строк: строки по 1000 знаков.
  const wide = clip(lines(40, 1000))
  expect(wide.text.split('\n')).toHaveLength(8)
  expect(wide.hidden).toBe(32)
  expect(new TextEncoder().encode(wide.text).length).toBeLessThanOrEqual(MAX_BYTES)
  // Перенос строки в конце не считается лишней строкой.
  expect(clip(`${lines(MAX_LINES)}\n`).hidden).toBe(0)
  // Первая строка длиннее предела режется по знакам, прочие строки скрыты.
  const huge = clip(`${'я'.repeat(10_000)}\nвторая`)
  expect(new TextEncoder().encode(huge.text).length).toBeLessThanOrEqual(MAX_BYTES)
  expect(huge.hidden).toBe(1)
  // Одна-единственная длинная строка тоже режется, скрытых строк нет.
  const single = clip('я'.repeat(10_000))
  expect(new TextEncoder().encode(single.text).length).toBeLessThanOrEqual(MAX_BYTES)
  expect(single.hidden).toBe(0)
  expect(single.text.length).toBeLessThan(10_000)
  // Обрыв посреди ограждения закрывает его.
  const fenced = clip(['```', ...Array.from({ length: 80 }, (_, i) => `код ${i}`), '```'].join('\n'))
  expect(fenced.text.endsWith('\n```')).toBe(true)
  expect(fenced.text.split('\n').filter(l => l.startsWith('```'))).toHaveLength(2)
})

test('артефакт в 300 строк: 60 строк в Markdown, «ещё 240 строк», ссылка в веб-интерфейс', async ($, on) => {
  const { ui } = await opened(on, $)
  await ui.press({ key: 'kind-dev.code_review' })
  const md = String((await ui.find({ type: 'Markdown' }))?.text)
  expect(md.split('\n')).toHaveLength(MAX_LINES)
  expect(md).toBe(LONG.split('\n').slice(0, MAX_LINES).join('\n'))
  expect(await ui.find({ type: 'Text', text: '… ещё 240 строк' })).toBeDefined()
  const link = await ui.find({ type: 'Link', text: /^Открыть полностью/ })
  expect(link).toMatchObject({ props: { href: `${HOST}/p/NOVA/tasks/NOVA-4` } })
  await ui.unmount()
})

// --- AC-9.1, AC-9.2: «В чат» и «Копировать» --------------------------------------------

test('«В чат»: заголовок и полный текст в поле ввода; ни хода, ни строки в разговор, ни запроса', async ($, on) => {
  const spy = spies(on)
  const { j, ui } = await opened(on, $)
  await ui.press({ key: 'kind-dev.code_review' })
  const urls = j.urls.length
  await ui.press({ key: 'to-chat' })
  expect(spy.fills).toHaveLength(1)
  // Полный текст, а не 60 строк отрисовки.
  expect(spy.fills[0]).toMatchObject({ text: `NOVA-4 · dev.code_review · v1\n\n${LONG}`, mode: 'insert' })
  expect(String(spy.fills[0].text).split('\n')).toHaveLength(302)
  expect(j.prompts).toHaveLength(0)
  expect(spy.appended).toHaveLength(0)
  expect(spy.tools).toHaveLength(0)
  expect(j.urls).toHaveLength(urls)
  // Текущая версия двухверсионного вида: vN из версии.
  await ui.press({ key: 'kind-dev.change_summary' })
  await ui.press({ key: 'to-chat' })
  expect(spy.fills[1]).toMatchObject({ text: `NOVA-4 · dev.change_summary · v2\n\n${BODIES[ID.sumV2]}` })
  await ui.select({ key: 'version', value: ID.sumV1 })
  await ui.press({ key: 'to-chat' })
  expect(spy.fills[2]).toMatchObject({ text: `NOVA-4 · dev.change_summary · v1\n\n${BODIES[ID.sumV1]}` })
  expect(spy.fills).toHaveLength(3)
  expect(j.prompts).toHaveLength(0)
  await ui.unmount()
})

test('«Копировать»: полный текст версии в буфер, без запросов к серверу', async ($, on) => {
  const spy = spies(on)
  const { j, ui } = await opened(on, $)
  await ui.press({ key: 'kind-dev.code_review' })
  const urls = j.urls.length
  await ui.press({ key: 'copy' })
  expect(spy.copies).toHaveLength(1)
  expect(spy.copies[0].text).toBe(LONG)
  expect(j.toasts).toContain('Скопировано')
  expect(spy.fills).toHaveLength(0)
  expect(j.urls).toHaveLength(urls)
  await ui.unmount()
})

// --- AC-8.4: вложения ---------------------------------------------------------------------

test('вложения: имя, тип, размер, автор и ссылка; HTML и SVG не открываются, картинка не встраивается', async ($, on) => {
  const { j, ui } = await opened(on, $)
  expect(await ui.find({ type: 'Text', text: 'Вложения' })).toBeDefined()
  const names = (await ui.findAll({ type: 'Text', text: /^⌗ \S/ })).map(t => t.text)
  expect(names).toEqual(['⌗ mockups.html', '⌗ flow.svg', '⌗ shot.png'])
  const meta = (await ui.findAll({ type: 'Text', text: /· user-/ })).map(t => t.text)
  expect(meta).toEqual(['HTML · 12 КБ · user-a · скачать, не открывать inline', 'SVG · 3 КБ · user-b · скачать, не открывать inline', 'PNG · 568 КБ · user-a'])
  // Каждому — своя ссылка «скачать»; плагин её не выполняет.
  const links = await ui.findAll({ type: 'Link', text: /^скачать/ })
  expect(links.map(l => l.props.href)).toEqual([ID.html, ID.svg, ID.png].map(id => `${HOST}/v1/artifacts/${id}/content`))
  for (const id of [ID.html, ID.svg, ID.png]) expect(countOf(j.urls, id)).toBe(0)
  // Ни Svg, ни картинки, ни разметки вложений в дереве.
  expect(await ui.find({ type: 'Svg' })).toBeUndefined()
  expect(await ui.find({ type: 'Image' })).toBeUndefined()
  const all = strings(await ui.drawn()).join('\n')
  expect(all).not.toContain('<html')
  expect(all).not.toContain('<svg')
  await ui.unmount()
})

test('вложения в терминале: те же строки, Select и Code на месте', async ($, on) => {
  const { ui } = await opened(on, $, {}, 'terminal')
  expect(await ui.find({ type: 'Text', text: '⌗ flow.svg' })).toBeDefined()
  expect(await ui.find({ type: 'Select', key: 'version' })).toBeDefined()
  expect(await ui.find({ type: 'Markdown' })).toBeDefined()
  await ui.unmount()
})

// --- AC-8.5: только чтение -----------------------------------------------------------------

test('чтение не меняет задачу: только GET, ни одного тула, ни хода, ни строки разговора', async ($, on) => {
  const spy = spies(on)
  const { j, ui } = await opened(on, $)
  await ui.press({ key: 'kind-dev.code_review' })
  await ui.press({ key: 'stage-analysis' })
  await ui.select({ key: 'version', value: ID.reqV1 })
  await ui.press({ key: 'stage-testing' })
  await ui.press({ key: 'view-back' })
  await ui.unmount()
  expect(j.urls.length).toBeGreaterThan(5)
  expect(new Set(j.methods)).toEqual(new Set(['GET']))
  expect(spy.tools).toHaveLength(0)
  expect(spy.appended).toHaveLength(0)
  expect(spy.fills).toHaveLength(0)
  expect(j.prompts).toHaveLength(0)
  expect(j.tools).toHaveLength(0)
})

// --- AC-8.6: сбой не стирает показанное, токен не просачивается ------------------------

test('ошибка сервера и сети: показанное остаётся, ошибка — строкой, токен нигде не виден', async ($, on) => {
  let broken: 'none' | 'server' | 'network' | 'auth' = 'none'
  const fail = (path: string): Reply | undefined => {
    if (broken === 'none' || !path.endsWith(`/artifacts/${ID.sumV1}/content`)) return undefined
    return broken === 'server'
      ? { status: 500, body: { error: { message: 'хранилище недоступно' } } }
      : broken === 'auth'
        ? { status: 401, body: { error: { message: 'токен не принят' } } }
        : undefined
  }
  const { j, ui } = await opened(on, $, { routes: server(fail), fail: /NOPE/ })
  const shown = async () => String((await ui.find({ type: 'Markdown' }))?.text)
  expect(await shown()).toBe(BODIES[ID.sumV2])

  broken = 'server'
  await ui.select({ key: 'version', value: ID.sumV1 })
  expect(await shown()).toBe(BODIES[ID.sumV2])
  expect((await ui.find({ type: 'Select', key: 'version' }))?.props.value).toBe(ID.sumV2)
  const err = await ui.find({ type: 'Text', text: /^Не загрузилось: / })
  expect(err?.text).toBe('Не загрузилось: 500: хранилище недоступно')
  expect(err?.props.color).toBe('error')

  broken = 'auth'
  await ui.select({ key: 'version', value: ID.sumV1 })
  expect(await shown()).toBe(BODIES[ID.sumV2])
  expect((await ui.find({ type: 'Text', text: /^Не загрузилось: / }))?.text).toBe('Не загрузилось: войдите: clab auth login')

  // Успех снимает ошибку.
  broken = 'none'
  await ui.select({ key: 'version', value: ID.sumV1 })
  expect(await shown()).toBe(BODIES[ID.sumV1])
  expect(await ui.find({ type: 'Text', text: /^Не загрузилось/ })).toBeUndefined()

  // Токен-заглушка не встречается ни в чём, что записал мод.
  const everything = [...strings(await ui.drawn()), ...j.states, ...j.toasts, ...j.statuses.filter((s): s is string => !!s), ...j.argv.map(a => a.join(' '))]
  // Токен читался через clab auth token — в аргументах его самого нет.
  expect(everything.filter(s => s.includes(TOKEN))).toEqual([])
  await ui.unmount()
})

test('сетевая ошибка при открытии: карточка остаётся, на экране — строка ошибки', async ($, on) => {
  const w = world(on, { now: NOW, routes: server(), fail: /\/artifacts\?include_superseded/ })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'NOVA-4')
  await tick(w.clock)
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'stage-open-NOVA-4-development' })
  await tick(w.clock)
  expect((await ui.find({ type: 'Text', text: /^Не загрузилось: ./ }))).toBeDefined()
  // Вкладки и прогоны — из карточки, список ещё не пришёл.
  expect(await ui.find({ type: 'Button', key: 'stage-testing' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /^Прогон 1 / })).toBeDefined()
  // Назад — к карточке.
  await ui.press({ key: 'view-back' })
  expect(await ui.find({ type: 'Button', key: 'stage-open-NOVA-4-development' })).toBeDefined()
  expect(await ui.find({ type: 'Button', key: 'view-back' })).toBeUndefined()
  await ui.unmount()
})

// --- AC-8.7: второй проект с другим циклом --------------------------------------------------

test('FORGE: вкладки и чипы из данных цикла, пропущенный Design без вкладки, этап без артефактов', async ($, on) => {
  const forge = (path: string): Reply | undefined => {
    if (path === '/tasks/FORGE-4/artifacts?include_superseded=true') {
      return {
        status: 200,
        body: {
          artifacts: [
            { id: 'f0000000-0000-4000-8000-000000000001', kind: 'qa.checklist', title: 'Verification checklist', format: 'markdown', version: 1, status: 'submitted', stage_run_id: '18c2a24f-fe28-453a-910b-f00496cfaa6d', created_at: '2026-10-30T11:00:00Z' },
          ],
        },
      }
    }
    if (path === '/tasks/FORGE-4/attachments') return { status: 200, body: { attachments: [] } }
    if (path === '/artifacts/f0000000-0000-4000-8000-000000000001/content') return { status: 200, text: '- [x] contrast\n- [ ] focus ring' }
    return undefined
  }
  const { j, ui } = await opened(on, $, { routes: server(forge) }, 'desktop', 'FORGE-4', 'verify')
  expect((await buttons(ui, 'stage-'))).toEqual(['Intake', 'Build', 'Verify'])
  expect(await ui.find({ type: 'Text', text: /^› Verify$/ })).toBeDefined()
  expect((await buttons(ui, 'kind-'))).toEqual(['Verification checklist'])
  expect((await ui.find({ type: 'Markdown' }))?.text).toBe('- [x] contrast\n- [ ] focus ring')
  // Роли и времени в снимке нет: строка прогона — исполнитель и «идёт».
  expect(await ui.find({ type: 'Text', text: 'Прогон 1 · qa-agent · идёт' })).toBeDefined()
  await ui.press({ key: 'stage-intake' })
  expect(await ui.find({ type: 'Text', text: 'Артефактов нет' })).toBeDefined()
  expect(await ui.find({ type: 'Markdown' })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: 'Вложения' })).toBeUndefined()
  expect(new Set(j.methods)).toEqual(new Set(['GET']))
  await ui.unmount()
})

test('убрать задачу из чата: просмотр закрывается, память о ней сбрасывается', async ($, on) => {
  const { ui } = await opened(on, $)
  expect(await ui.find({ type: 'Button', key: 'view-back' })).toBeDefined()
  await run($, 'remove NOVA-4')
  expect(await ui.find({ type: 'Button', key: 'view-back' })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /В этом чате задач пока нет/ })).toBeDefined()
  await ui.unmount()
})

// --- AC-8.8: в просмотре нет ни одной подписи этапа, роли или вида -------------------

test('подписи просмотра — только данные цикла и слова плагина (обезличенные названия)', async ($, on) => {
  let n = 0
  const tok = (x: unknown) => (x === undefined || x === null ? x : `§${++n}`)
  const d = nova4()
  const wf = clone(WORKFLOWS[d.task.workflow_id]) as Workflow & Record<string, any>
  for (const st of wf.statuses) st.name = tok(st.name) as string
  for (const r of d.stage_runs ?? []) {
    r.role_key = tok(r.role_key)
    r.claimed_by_handle = tok(r.claimed_by_handle)
    if (r.exit) r.exit.comment = tok(r.exit.comment)
  }
  const list = LIST.map(a => ({ ...a, kind: `§k${a.kind.length}${a.id.slice(-1)}`, title: tok(a.title) as string }))
  const files = ATTACHMENTS.map(a => ({ ...a, file_name: tok(a.file_name), media_type: `application/§${a.id.slice(-1)}`, added_by: tok(a.added_by) }))
  const route = (path: string): Reply | undefined => {
    if (path === `/workflows/${d.task.workflow_id}`) return { status: 200, body: wf }
    if (path === '/tasks/NOVA-4/artifacts?include_superseded=true') return { status: 200, body: { artifacts: list } }
    if (path === '/tasks/NOVA-4/attachments') return { status: 200, body: { attachments: files } }
    return undefined
  }
  const w = world(on, { now: NOW, routes: server(route, { ...TASKS, 'NOVA-4': d }) })
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })
  await run($, 'NOVA-4')
  await tick(w.clock)
  const seen: string[] = []
  for (const surface of ['terminal', 'desktop', 'vscode'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    await ui.press({ key: 'stage-open-NOVA-4-development' })
    await tick(w.clock)
    for (const key of ['kind-' + list[3]!.kind, 'kind-' + list[5]!.kind]) {
      await ui.press({ key })
      seen.push(...strings(await ui.find({ key: 'view-NOVA-4' })).filter(x => !x.startsWith('Строка ') && !x.startsWith('# ')))
    }
    await ui.press({ key: 'view-back' })
    await ui.unmount()
  }
  const allowed = new Set(
    [...Object.values(WORDS), ...Object.values(GATE_WORD), ...MONTHS, 'clab', 'artifact', '-o', './', 'Cadence Lab']
      .flatMap(x => x.toLowerCase().split(/[\s:·,]+/))
      .filter(Boolean),
  )
  const glyphs = new Set([...Object.values(GLYPH), '·', '─', '…', '«', '»', '×', '$', '#', '+', ',', ':', '(', ')', '—', '–', '-', '.', '/', 'v', '✕', '⏸'])
  const bad: string[] = []
  for (const line of seen) {
    const rest = line
      .replace(/§[a-z]*\d*/g, ' ')
      .replace(/[0-9a-f]{8}-[0-9a-f-]{27}/g, ' ')
      .replace(/https?:\/\/\S+/g, ' ')
      .replace(/[A-Z][A-Z0-9_]*-\d+/g, ' ')
      .replace(/\d+([.,]\d+)?/g, ' ')
    for (const word of rest.split(/[\s·:,«»()]+/).filter(Boolean)) {
      if (allowed.has(word.toLowerCase()) || [...word].every(ch => glyphs.has(ch))) continue
      bad.push(`${word} ← «${line}»`)
    }
  }
  expect(seen.length).toBeGreaterThan(60)
  expect(bad).toEqual([])
  // Ничего из исходных названий в обезличенном просмотре не осталось.
  for (const name of ['Analysis', 'Development', 'Testing', 'Разбор изменений', 'dev-agent', 'developer', 'analytics', 'mockups.html']) {
    expect(seen.join('\n')).not.toContain(name)
  }
})
