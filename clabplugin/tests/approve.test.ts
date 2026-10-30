import { expect, test } from 'claude-code/testing'

import type { TaskDetail, Workflow } from '../hooks/card'
import { platesOf, returnsOf, seenOf, driftOf, returnSteps, approveSteps, stageChipsOf } from '../hooks/approve'
import { GATE_WORD, GLYPH, WORDS } from '../hooks/vocab'
import { contrast, DARK, LIGHT, TINT } from '../hooks/theme'
import { lab } from './lab'
import type { Lab } from './lab'
import { PANE, TOKEN, run, world } from './world'
import type { WorldOptions } from './world'

// Решения человека по гейтам из панели (CLAB-326) на подставном сервере с
// записью: NOVA (форма нашего цикла) и FORGE (чужой цикл).

const NOW = Date.parse('2026-10-30T12:30:00Z')
const tick = async (clock: { settle: () => Promise<void> }) => {
  for (let i = 0; i < 8; i++) await clock.settle()
}
const NOVA_WHO = { principal: 'user:ann', authenticated: true, api: 'https://cadence.example.test/v1', project: { key: 'NOVA', name: 'Nova' } }
const BAND = { plugin: 'clab-plugin', component: 'AbovePrompt', props: { hasSurvey: false, isWorking: false, maxRows: 10, bodyColumns: 80, scroll: { offset: 0, bodyRows: 10 } } } as never

async function start($: any, on: any, L: Lab, keys: string, opts: WorldOptions = {}, surface = 'desktop') {
  const w = world(on, { now: NOW, whoami: NOVA_WHO, serve: L.serve, ...opts })
  await $.session.start({ cwd: '/', surface, isInteractive: true })
  await tick(w.clock)
  if (keys) await run($, keys)
  await tick(w.clock)
  return w
}

const decisions = (L: Lab) => L.posts.filter(p => p.path.endsWith('/decision'))
const moves = (L: Lab) => L.posts.filter(p => /\/transitions\/[a-z_]+$/.test(p.path))
const texts = async (ui: any, re: RegExp) => (await ui.findAll({ type: 'Text', text: re })).map((t: any) => t.text)

// --- Чистая модель -------------------------------------------------------------

test('модель: плашки, возвраты и снимок по ответу сервера и версии цикла', () => {
  const L = lab()
  const d = (key: string) => (L.serve('GET', `/tasks/${key}`, undefined)!.body as TaskDetail)
  const wf = (id: string) => (L.serve('GET', `/workflows/${id}`, undefined)!.body as Workflow)

  const p1 = platesOf(d('NOVA-1'), wf('wf-nova'))
  expect(p1).toEqual([
    {
      transition: 'analysis_done',
      transitionName: 'Аналитика готова',
      toName: 'Разработка',
      gate: 'human_approval',
      gateName: 'Апрув аналитики',
      type: 'human_approval',
      state: 'pending',
      reason: 'апрувов 0 из 1',
      items: [],
      since: L.tasks['NOVA-1']!.artifacts[0]!.created_at,
      others: ['Артефакты сданы'],
    },
  ])
  // Чек-лист: пункты из версии цикла, обязательный не отмечен.
  const p2 = platesOf(d('NOVA-2'), wf('wf-nova'))
  expect(p2.map(p => [p.gateName, p.type, p.items.map(i => [i.id, i.required, i.checked])])).toEqual([
    ['Self-hosted учтён', 'checklist', [['selfhosted', true, false], ['notes', false, false]]],
  ])
  // FORGE-1: чек-лист пройден, плашка одна — апрув на двух людей.
  expect(platesOf(d('FORGE-1'), wf('wf-forge')).map(p => p.gate)).toEqual(['two_people'])
  // «Не пройден» у чек-листа: плашка отклонённого с причиной, пункты — с нуля.
  L.serve('POST', '/tasks/NOVA-2/transitions/dev_done/gates/selfhosted/decision', { status: 'failed', details: { reason: 'нет раздела' } })
  expect(platesOf(d('NOVA-2'), wf('wf-nova')).map(p => [p.state, p.reason, p.items.map(i => i.checked)])).toEqual([['failed', 'нет раздела', [false, false]]])
  // Чипы этапа — по виду, последняя версия, в порядке первой сдачи.
  L.submit('NOVA-3', 'test.report')
  L.submit('NOVA-3', 'test.report')
  expect(stageChipsOf(d('NOVA-3')).map(c => c.kind)).toEqual(['result', 'test.report'])

  // Возвраты: только reject, без недоступного; вид артефакта — свой у цикла;
  // предел попыток — держит.
  expect(returnsOf(d('NOVA-3'), wf('wf-nova'))).toEqual([{ transition: 'merge_conflict', name: 'Конфликт слияния', toName: 'Разработка', kinds: ['problem.report'], held: null }])
  expect(returnsOf(d('FORGE-2'), wf('wf-forge'))).toEqual([
    { transition: 'send_back', name: 'Send back', toName: 'Build', kinds: ['forge.rework'], held: null },
    { transition: 'escalate', name: 'Escalate', toName: 'Backlog', kinds: [], held: 'Attempts limit · попыток 3 из 3' },
  ])
  expect(returnsOf(d('NOVA-1'), wf('wf-nova'))).toEqual([])

  // Снимок и сверка: новый артефакт этапа — changed, другой статус — moved.
  const seen = seenOf(d('NOVA-1'))
  expect(seen.status).toBe('analysis')
  expect(seen.artifact).not.toBe('')
  expect(driftOf(seen, d('NOVA-1'))).toBe('same')
  L.submit('NOVA-1', 'analysis.requirements')
  expect(driftOf(seen, d('NOVA-1'))).toBe('changed')
  expect(driftOf({ ...seen, status: 'development' }, d('NOVA-1'))).toBe('moved')

  // План записи: возврат — артефакт, failed, переход; апрув — решение и переход.
  const plate = platesOf(d('NOVA-3'), wf('wf-nova'))[0]!
  const option = returnsOf(d('NOVA-3'), wf('wf-nova'))[0]!
  expect(returnSteps('NOVA-3', plate, option, '  нет плана отката ', seenOf(d('NOVA-3'))).map(s => [s.kind, s.path, s.body])).toEqual([
    ['artifact', '/tasks/NOVA-3/artifacts', { kind: 'problem.report', title: WORDS.returnReason, format: 'markdown', content: 'нет плана отката' }],
    ['decision', '/tasks/NOVA-3/transitions/merge_ready/gates/human_approval/decision', { status: 'failed', details: { reason: 'нет плана отката' }, seen_artifact_id: seenOf(d('NOVA-3')).artifact }],
    ['transition', '/tasks/NOVA-3/transitions/merge_conflict', { comment: 'нет плана отката' }],
  ])
  const a = approveSteps('NOVA-1', p1[0]!, seen)
  expect([a.decision.path, a.decision.body.status, a.transition.path]).toEqual(['/tasks/NOVA-1/transitions/analysis_done/gates/human_approval/decision', 'passed', '/tasks/NOVA-1/transitions/analysis_done'])
})

// --- AC-1, AC-2: апрув -------------------------------------------------------------

test('апрув: плашка в карточке, первое нажатие ничего не пишет, «Да, апрув» — решение и переход', async ($, on) => {
  const L = lab()
  const { j, clock } = await start($, on, L, 'NOVA-1')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await texts(ui, /Ждёт вашего апрува/)).toEqual(['Ждёт вашего апрува · Апрув аналитики'])
  expect(await texts(ui, /^Переход «/)).toEqual(['Переход «Аналитика готова» → Разработка · апрувов 0 из 1'])
  const id = 'NOVA-1-analysis_done-human_approval'
  expect(await ui.find({ key: `approve-${id}` })).toBeDefined()
  // У статуса нет возвратов — и кнопки нет.
  expect(await ui.find({ key: `return-${id}` })).toBeUndefined()

  // Первое нажатие и отмена — ни одной записи.
  await ui.press({ key: `approve-${id}` })
  await tick(clock)
  expect(await texts(ui, /^Утвердить/)).toEqual(['Утвердить NOVA-1 и перевести в Разработка?'])
  expect(await texts(ui, /от вашего имени/)).toEqual([
    'Гейт «Апрув аналитики» · переход «Аналитика готова». Решение запишется от вашего имени (user:ann). Ждущий агент узнает исход по состоянию задачи.',
  ])
  await ui.press({ key: 'cancel-NOVA-1' })
  await tick(clock)
  expect(await ui.find({ key: 'yes-NOVA-1' })).toBeUndefined()
  expect(L.posts).toEqual([])

  // Подтверждение: два нажатия «Да, апрув» подряд — одна запись.
  await ui.press({ key: `approve-${id}` })
  await tick(clock)
  const first = ui.press({ key: 'yes-NOVA-1' })
  const second = ui.press({ key: 'yes-NOVA-1' }).catch(() => undefined)
  await Promise.all([first, second])
  await tick(clock)
  expect(L.posts.map(p => p.path)).toEqual(['/tasks/NOVA-1/transitions/analysis_done/gates/human_approval/decision', '/tasks/NOVA-1/transitions/analysis_done'])
  expect(L.posts[0]!.body).toMatchObject({ status: 'passed', seen_artifact_id: L.tasks['NOVA-1']!.artifacts[0]!.id })
  expect(j.toasts).toContain('NOVA-1 · апрув поставлен → Разработка')
  expect(await texts(ui, /Апрув поставлен/)).toEqual(['Апрув поставлен · NOVA-1 → Разработка'])
  expect(await ui.find({ type: 'Link', text: /журнал/ })).toBeDefined()
  // Записи — личным токеном; в разговор ничего, тулов нет.
  const writes = j.methods.map((m, i) => [m, j.auths[i]]).filter(([m]) => m === 'POST')
  expect(writes).toEqual([['POST', `Bearer ${TOKEN}`], ['POST', `Bearer ${TOKEN}`]])
  expect(j.prompts).toEqual([])
  expect(j.tools).toEqual([])
  await ui.unmount()
})

// --- AC-3: результат изменился --------------------------------------------------

test('новая сдача после показа: решение не пишется, плашка просит посмотреть заново', async ($, on) => {
  const L = lab()
  const { clock } = await start($, on, L, 'NOVA-1')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  L.submit('NOVA-1', 'analysis.requirements')
  await ui.press({ key: 'yes-NOVA-1' })
  await tick(clock)
  expect(decisions(L)).toEqual([])
  expect(await texts(ui, /Решение не записано/)).toEqual(['Решение не записано: результат этапа изменился — посмотрите заново'])
  expect(await ui.find({ type: 'Link', text: /Посмотреть артефакты/ })).toBeDefined()
  // Плашка снова с «Апрув»: карточка перечитана.
  expect(await ui.find({ key: 'approve-NOVA-1-analysis_done-human_approval' })).toBeDefined()

  // Окно между перечитыванием и записью закрывает сервер: 409 сверки — тот же
  // итог, перехода нет.
  L.refuse = p => (p.path.endsWith('/decision') ? { status: 409, body: { error: { code: 'conflict', message: 'результат этапа изменился', details: { reason: 'stage_result_changed' } } } } : undefined)
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  await ui.press({ key: 'yes-NOVA-1' })
  await tick(clock)
  expect(decisions(L)).toHaveLength(1)
  expect(moves(L)).toEqual([])
  expect(await texts(ui, /Решение не записано/)).toEqual(['Решение не записано: результат этапа изменился — посмотрите заново'])
  await ui.unmount()
})

// --- AC-5: апрув записан, переход не выполнен -----------------------------------

test('апрув на двух людей: решение записано, перехода нет, карточка говорит, что держит', async ($, on) => {
  const L = lab()
  const { clock } = await start($, on, L, 'FORGE-1')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'approve-FORGE-1-ship-two_people' })
  await tick(clock)
  await ui.press({ key: 'yes-FORGE-1' })
  await tick(clock)
  expect(decisions(L)).toHaveLength(1)
  expect(moves(L)).toEqual([])
  expect(await texts(ui, /Апрув записан/)).toEqual(['Апрув записан · Two sign-offs · апрувов 1 из 2'])
  // Записан, но переход держит — нейтральный блок с пояснением, где смотреть.
  expect(await texts(ui, /^Что держит переход/)).toHaveLength(1)
  expect(await texts(ui, /^Переход «Ship/)).toEqual(['Переход «Ship to board» → Review board · апрувов 1 из 2'])
  await ui.unmount()
})

// --- AC-6, AC-7, AC-8: возврат -----------------------------------------------------

test('возврат NOVA: только reject из версии, без причины кнопки нет, порядок записи — артефакт, failed, переход', async ($, on) => {
  const L = lab()
  const { j, clock } = await start($, on, L, 'NOVA-3')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'return-NOVA-3-merge_ready-human_approval' })
  await tick(clock)
  const select = await ui.find({ key: 'target-NOVA-3' })
  expect((select?.props as any).options).toEqual([{ value: 'merge_conflict', label: 'Конфликт слияния → Разработка' }])
  expect(await ui.find({ key: 'do-return-NOVA-3' })).toBeUndefined()
  await ui.input({ key: 'reason-NOVA-3', text: '   ', kind: 'change' })
  await tick(clock)
  expect(await ui.find({ key: 'do-return-NOVA-3' })).toBeUndefined()
  await ui.input({ key: 'reason-NOVA-3', text: 'Нет плана отката', kind: 'change' })
  await tick(clock)
  expect(L.posts).toEqual([])
  await ui.press({ key: 'do-return-NOVA-3' })
  await tick(clock)
  const art = L.tasks['NOVA-3']!.artifacts.find(a => a.kind === 'problem.report')!
  expect(L.posts.map(p => [p.path, p.body])).toEqual([
    ['/tasks/NOVA-3/artifacts', { kind: 'problem.report', title: 'Причина возврата', format: 'markdown', content: 'Нет плана отката' }],
    ['/tasks/NOVA-3/transitions/merge_ready/gates/human_approval/decision', { status: 'failed', details: { reason: 'Нет плана отката' }, seen_artifact_id: art.id }],
    ['/tasks/NOVA-3/transitions/merge_conflict', { comment: 'Нет плана отката' }],
  ])
  expect(L.tasks['NOVA-3']!.status).toBe('development')
  expect(j.toasts).toContain('NOVA-3 · возвращена → Разработка')
  await ui.unmount()
})

test('возврат FORGE: свой вид артефакта, переход в выключенный этап не предлагается, предел попыток держит', async ($, on) => {
  const L = lab()
  const { clock } = await start($, on, L, 'FORGE-2')
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  await ui.press({ key: 'return-FORGE-2-accept-sign_off' })
  await tick(clock)
  const select = await ui.find({ key: 'target-FORGE-2' })
  expect((select?.props as any).options.map((o: any) => o.value)).toEqual(['send_back', 'escalate'])
  // Под пределом: гейт назван, кнопки нет, записи нет.
  await ui.select({ key: 'target-FORGE-2', value: 'escalate' })
  await ui.input({ key: 'reason-FORGE-2', text: 'Ещё раз', kind: 'change' })
  await tick(clock)
  expect(await texts(ui, /^Держит/)).toEqual(['Держит: Attempts limit · попыток 3 из 3'])
  expect(await ui.find({ key: 'do-return-FORGE-2' })).toBeUndefined()
  expect(L.posts).toEqual([])
  // Шаг (в) упал: артефакт и failed остались, карточка перечитана, повтор
  // возможен.
  await ui.select({ key: 'target-FORGE-2', value: 'send_back' })
  await tick(clock)
  L.refuse = p => (/\/transitions\/send_back$/.test(p.path) ? { status: 500, body: { error: { code: 'internal', message: 'внутренняя ошибка' } } } : undefined)
  await ui.press({ key: 'do-return-FORGE-2' })
  await tick(clock)
  expect(L.posts.map(p => p.path)).toEqual(['/tasks/FORGE-2/artifacts', '/tasks/FORGE-2/transitions/accept/gates/sign_off/decision', '/tasks/FORGE-2/transitions/send_back'])
  expect(L.posts[0]!.body.kind).toBe('forge.rework')
  expect(await texts(ui, /Решение не записано: 500/)).toHaveLength(1)
  expect(await texts(ui, /Отклонено · Go-live sign-off · Ещё раз/)).toHaveLength(1)
  expect(await ui.find({ key: 'return-FORGE-2-accept-sign_off' })).toBeDefined()
  // Сетевой сбой не повторяется сам.
  expect(L.posts).toHaveLength(3)
  await ui.unmount()
})

// --- AC-9: чек-лист ------------------------------------------------------------------

test('чек-лист: пункты из версии цикла, «Отметить пройденным» — после обязательных, «Не пройден» — с причиной', async ($, on) => {
  const L = lab()
  const { clock } = await start($, on, L, 'NOVA-2')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'mark-NOVA-2-dev_done-selfhosted' })
  await tick(clock)
  expect((await ui.findAll({ type: 'Button', text: /☐/ })).map((b: any) => b.text)).toEqual(['☐ Раздел «Self-hosted» написан', '☐ Заметки релиза'])
  expect(await ui.find({ key: 'pass-NOVA-2' })).toBeUndefined()
  expect(await ui.find({ key: 'fail-NOVA-2' })).toBeUndefined()
  await ui.press({ key: 'item-NOVA-2-selfhosted' })
  await tick(clock)
  expect(L.posts).toEqual([])
  await ui.press({ key: 'pass-NOVA-2' })
  await tick(clock)
  expect(L.posts.map(p => [p.path, p.body])).toEqual([
    ['/tasks/NOVA-2/transitions/dev_done/gates/selfhosted/decision', { status: 'passed', details: { checked: ['selfhosted'] }, seen_artifact_id: L.tasks['NOVA-2']!.artifacts[0]!.id }],
  ])
  expect(await texts(ui, /Self-hosted учтён ✓ отмечен/)).toHaveLength(1)

  // «Не пройден»: без причины кнопки нет; с причиной — failed.
  L.decide('NOVA-2', 'dev_done', 'selfhosted', 'pending')
  // Опрос раз в 30 с (у сервера нет ручки событий) перечитывает карточку.
  await clock.advance(30_000)
  await tick(clock)
  await ui.press({ key: 'mark-NOVA-2-dev_done-selfhosted' })
  await tick(clock)
  await ui.input({ key: 'reason-NOVA-2', text: 'раздела нет', kind: 'change' })
  await tick(clock)
  await ui.press({ key: 'fail-NOVA-2' })
  await tick(clock)
  expect(L.posts.at(-1)?.body).toMatchObject({ status: 'failed', details: { reason: 'раздела нет' } })
  expect(moves(L)).toEqual([])

  // Гейт закрыт failed: плашка отклонённого с причиной, из «Ждут меня»
  // строка ушла; «Отметить…» снова открывает чек-лист с пустыми пунктами.
  expect(await texts(ui, /Отклонено/)).toEqual(['✕ Отклонено · Self-hosted учтён · раздела нет'])
  expect(await texts(ui, /^Что держит переход/)).toEqual([])
  await ui.press({ key: 'tab-approvals' })
  await tick(clock)
  expect(await ui.find({ key: 'appr-NOVA-2-dev_done-selfhosted' })).toBeUndefined()
  await ui.press({ key: 'tab-chat' })
  await ui.press({ key: 'mark-NOVA-2-dev_done-selfhosted' })
  await tick(clock)
  expect((await ui.findAll({ type: 'Button', text: /☐|☑/ })).map((b: any) => b.text)).toEqual(['☐ Раздел «Self-hosted» написан', '☐ Заметки релиза'])
  await ui.press({ key: 'item-NOVA-2-selfhosted' })
  await tick(clock)
  await ui.press({ key: 'pass-NOVA-2' })
  await tick(clock)
  expect(L.posts.at(-1)?.body).toMatchObject({ status: 'passed', details: { checked: ['selfhosted'] } })
  expect(L.tasks['NOVA-2']!.transitions.development![0]!.gates[1]!.status).toBe('passed')
  expect(await texts(ui, /Отклонено/)).toEqual([])
  await ui.unmount()
})

// --- AC-10, AC-11: права, ошибки, граница ---------------------------------------

test('viewer видит плашку без кнопок, и в полосе «Апрув» нет', async ($, on) => {
  const L = lab()
  L.levels.NOVA = 'viewer'
  await start($, on, L, 'NOVA-1 NOVA-3')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await texts(ui, /Ждёт вашего апрува/)).toHaveLength(2)
  expect(await ui.findAll({ type: 'Button', text: /Апрув|Вернуть/ })).toEqual([])
  await ui.unmount()
  const band = await $.ui.mount({ ...BAND, surface: 'desktop' } as never)
  expect(await band.find({ key: 'band-approve' })).toBeUndefined()
  await band.unmount()
})

test('403 на записи — тост с текстом сервера, перехода нет; сеть упала — без повтора, карточка перечитана', async ($, on) => {
  const L = lab()
  L.refuse = p => (p.path.endsWith('/decision') ? { status: 403, body: { error: { code: 'forbidden', message: 'нужен уровень maintainer, а есть viewer' } } } : undefined)
  const { j, clock } = await start($, on, L, 'NOVA-1')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  await ui.press({ key: 'yes-NOVA-1' })
  await tick(clock)
  expect(j.toasts).toContain('NOVA-1 · 403: нужен уровень maintainer, а есть viewer')
  expect(moves(L)).toEqual([])
  expect(L.tasks['NOVA-1']!.status).toBe('analysis')
  await ui.unmount()
})

test('401 на записи: токен перечитывается один раз, второй 401 — «войдите», записи нет', async ($, on) => {
  const L = lab()
  L.refuse = p => (p.path.endsWith('/decision') ? { status: 401, body: { error: { code: 'unauthenticated', message: 'нет' } } } : undefined)
  const { j, clock } = await start($, on, L, 'NOVA-1')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  const before = j.argv.filter(a => a[1] === 'auth').length
  await ui.press({ key: 'yes-NOVA-1' })
  await tick(clock)
  expect(decisions(L)).toHaveLength(2)
  expect(j.argv.filter(a => a[1] === 'auth').length - before).toBeGreaterThanOrEqual(1)
  expect(moves(L)).toEqual([])
  expect(await texts(ui, /войдите: clab auth login/)).not.toEqual([])
  await ui.unmount()
})

test('сеть упала на записи: запрос не повторяется, карточка перечитывается', async ($, on) => {
  const L = lab()
  const { j, clock } = await start($, on, L, 'NOVA-1', { fail: /\/decision$/ })
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  const reads = j.urls.filter(u => u.endsWith('/tasks/NOVA-1')).length
  await ui.press({ key: 'yes-NOVA-1' })
  await tick(clock)
  expect(j.urls.filter(u => u.endsWith('/decision'))).toHaveLength(1)
  expect(j.urls.filter(u => u.endsWith('/tasks/NOVA-1')).length).toBeGreaterThan(reads + 1)
  expect(await texts(ui, /нет связи с инстансом/)).toHaveLength(1)
  await ui.unmount()
})

test('граница: токен — только в заголовке записи, в разговор ничего, без поверхности — ни запросов, ни кнопок', async ($, on) => {
  const L = lab()
  const { j, clock } = await start($, on, L, 'NOVA-1')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  await ui.press({ key: 'yes-NOVA-1' })
  await tick(clock)
  await ui.unmount()
  for (const where of [j.states, j.toasts, j.statuses.map(String), j.prompts]) {
    expect(where.join('\n')).not.toContain(TOKEN)
  }
  expect(j.argv.filter(a => a.join(' ').includes(TOKEN))).toEqual([])
  expect(j.prompts).toEqual([])
  expect(j.tools).toEqual([])
})

test('без поверхности (claude -p): ни clab, ни запросов, ни записи', async ($, on) => {
  const L = lab()
  const { j, clock } = world(on, { now: NOW, whoami: NOVA_WHO, serve: L.serve })
  await $.session.start({ cwd: '/', surface: null, isInteractive: false } as never)
  await tick(clock)
  await run($, 'NOVA-1')
  await tick(clock)
  expect(j.urls).toEqual([])
  expect(j.argv).toEqual([])
  expect(L.posts).toEqual([])
})

// --- AC-14, AC-18: «Ждут меня» ------------------------------------------------

test('«Ждут меня»: ровно ответ выборки, число во вкладке и строке статуса, «в этом чате», действие — то же подтверждение', async ($, on) => {
  const L = lab()
  const { j, clock } = await start($, on, L, 'NOVA-1')
  await run($, 'approvals')
  await tick(clock)
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await ui.find({ key: 'tab-approvals' })).toMatchObject({ text: 'Ждут меня 3' })
  expect(j.statuses.at(-1)).toBe('Clab: 1 в чате · ⏸ 3 апрув')
  expect(await texts(ui, /^[⏸○] .*→/)).toEqual([
    '⏸ Апрув аналитики Аналитика готова → Разработка',
    '○ Self-hosted учтён Разработка готова → Тестирование',
    '⏸ Разрешение слить Готово к слиянию → Готово',
  ])
  // «в этом чате» — только у задачи чата; вводная и подвал выборки.
  expect(await texts(ui, /^в этом чате$/)).toHaveLength(1)
  expect(await texts(ui, /^Гейты проекта/)).toEqual(['Гейты проекта, которые вы можете закрыть по уровню доступа.'])
  expect(await texts(ui, /^обновлено/)).toEqual(['обновлено опросом · только что'])
  expect(await ui.find({ key: 'appr-act-NOVA-2-dev_done-selfhosted' })).toMatchObject({ text: 'Отметить…' })
  // «Апрув» из строки: карточка в «Этом чате» с тем же подтверждением.
  await ui.press({ key: 'appr-act-NOVA-3-merge_ready-human_approval' })
  await tick(clock)
  expect(await texts(ui, /^Утвердить NOVA-3/)).toEqual(['Утвердить NOVA-3 и перевести в Готово?'])
  await ui.press({ key: 'yes-NOVA-3' })
  await tick(clock)
  expect(moves(L).map(p => p.path)).toEqual(['/tasks/NOVA-3/transitions/merge_ready'])
  // Решение извне (веб-интерфейс): строка уходит при следующем опросе ≤ 30 с.
  L.decide('NOVA-2', 'dev_done', 'selfhosted', 'passed')
  await clock.advance(30_000)
  await tick(clock)
  await ui.press({ key: 'tab-approvals' })
  expect(await texts(ui, /^[⏸○] .*→/)).toEqual(['⏸ Апрув аналитики Аналитика готова → Разработка'])
  expect(j.statuses.at(-1)).toBe('Clab: 2 в чате · ⏸ 1 апрув')
  await ui.unmount()
})

test('«Ждут меня» у viewer — словами, а не пустотой', async ($, on) => {
  const L = lab()
  L.levels.NOVA = 'viewer'
  const { clock } = await start($, on, L, '')
  await run($, 'approvals')
  await tick(clock)
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await texts(ui, /нужен уровень сопровождающего/)).toEqual(['Ждут меня: нужен уровень сопровождающего'])
  await ui.unmount()
})

test('сервер без выборки: вкладки «Ждут меня» нет, апрув из карточки и сверка клиента работают', async ($, on) => {
  const L = lab()
  L.noApprovals = true
  const { j, clock } = await start($, on, L, 'NOVA-1')
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  expect(await ui.find({ key: 'tab-approvals' })).toBeUndefined()
  expect(j.statuses.at(-1)).toBe('Clab: 1 в чате · ⏸ 1 апрув')
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  await ui.press({ key: 'yes-NOVA-1' })
  await tick(clock)
  expect(moves(L).map(p => p.path)).toEqual(['/tasks/NOVA-1/transitions/analysis_done'])
  // Выборку спросили один раз и больше не спрашивают.
  expect(j.urls.filter(u => u.endsWith('/approvals'))).toHaveLength(1)
  await ui.unmount()
})

// --- AC-16: поверхности ---------------------------------------------------------

test('поверхности: полоса с «Апрув», терминал с hotkey 1 и 2, mobile — только ссылка', async ($, on) => {
  const L = lab()
  const { clock } = await start($, on, L, 'NOVA-3', {}, 'terminal')
  const band = await $.ui.mount({ ...BAND, surface: 'desktop' } as never)
  expect(await band.find({ key: 'band-approve' })).toMatchObject({ text: 'Апрув' })
  await band.press({ key: 'band-approve' })
  await tick(clock)
  await band.unmount()
  let ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ key: 'yes-NOVA-3' })).toBeDefined()
  await ui.press({ key: 'cancel-NOVA-3' })
  await tick(clock)
  expect((await ui.find({ key: 'approve-NOVA-3-merge_ready-human_approval' }))?.props).toMatchObject({ hotkey: '1' })
  expect((await ui.find({ key: 'return-NOVA-3-merge_ready-human_approval' }))?.props).toMatchObject({ hotkey: '2' })
  await ui.unmount()
  ui = await $.ui.mount({ ...PANE, surface: 'mobile' })
  expect(await ui.find({ key: 'approve-NOVA-3-merge_ready-human_approval' })).toBeUndefined()
  expect(await ui.find({ type: 'Link', text: /Открыть в Cadence Lab/ })).toBeDefined()
  await ui.unmount()
  expect(L.posts).toEqual([])
})

// --- Макеты 02, 04, 04b, 06: блоки, пояснения, возраст, чипы -------------------

test('макет: заливка по явной теме и рамка при auto, возраст, остальные гейты, чипы по виду, пояснения форм и итогов', async ($, on) => {
  const L = lab()
  L.submit('NOVA-1', 'analysis.requirements')
  const { clock } = await start($, on, L, 'NOVA-1 NOVA-2 NOVA-3', { theme: 'light' })
  let ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  const plate = await ui.find({ key: 'plate-NOVA-1-analysis_done-human_approval' })
  expect(plate?.props).toMatchObject({ borderStyle: 'round', borderColor: 'warning', backgroundColor: TINT.light.warning })
  // Возраст ожидания — от последней сдачи на этапе (12:00:0x, сейчас 12:30);
  // остальные гейты перехода.
  expect(await texts(ui, /^29 мин$/)).toHaveLength(2)
  expect(await texts(ui, /^Остальные гейты пройдены/)).toEqual(['Остальные гейты пройдены: Артефакты сданы ✓', 'Остальные гейты пройдены: Разбор сдан ✓'])
  // Чипы — по одному на вид артефакта этапа, ведут в карточку.
  expect((await ui.findAll({ type: 'Link', text: /›$/ })).map((l: any) => l.text).slice(0, 2)).toEqual(['Результат этапа ›', 'analysis.requirements ›'])
  // Чек-лист — рамка без заливки, глиф ○ и слово типа гейта.
  expect((await ui.find({ key: 'plate-NOVA-2-dev_done-selfhosted' }))?.props.backgroundColor).toBeUndefined()
  expect(await texts(ui, /^○ Self-hosted учтён$/)).toHaveLength(1)
  expect(await texts(ui, /^До перехода «Разработка готова» → Тестирование/)).toHaveLength(1)

  // Подтверждение стоит на месте плашки, с заливкой и подсказкой Enter.
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  expect(await ui.find({ key: 'plate-NOVA-1-analysis_done-human_approval' })).toBeUndefined()
  expect((await ui.find({ key: 'confirm-NOVA-1' }))?.props).toMatchObject({ backgroundColor: TINT.light.warning })
  expect((await ui.find({ key: 'yes-NOVA-1' }))?.text).toBe('Да, апрув ↩')
  // Результат изменился — блок с заливкой error и пояснением.
  L.submit('NOVA-1', 'analysis.requirements')
  await ui.press({ key: 'yes-NOVA-1' })
  await tick(clock)
  expect((await ui.find({ key: 'outcome-NOVA-1' }))?.props).toMatchObject({ backgroundColor: TINT.light.error, borderColor: 'error' })
  expect(await texts(ui, /^Апрув до новой сдачи не считается/)).toHaveLength(1)

  // Форма возврата: заголовок, подсказки полей, артефакт, который сдаётся.
  await ui.press({ key: 'return-NOVA-3-merge_ready-human_approval' })
  await tick(clock)
  expect(await texts(ui, /^Вернуть NOVA-3$/)).toHaveLength(1)
  expect((await ui.find({ key: 'target-NOVA-3' }))?.props).toMatchObject({ label: 'Куда — переходы-возвраты текущего статуса' })
  expect((await ui.find({ key: 'reason-NOVA-3' }))?.props).toMatchObject({ label: 'Причина — её прочитает агент следующего этапа' })
  expect(await texts(ui, /^Вместе с возвратом/)).toEqual(['Вместе с возвратом от вашего имени сдаётся артефакт «problem.report» с этой причиной — его требует гейт перехода.'])
  expect(await texts(ui, /^гейт: failed \+ переход$/)).toHaveLength(1)
  await ui.press({ key: 'cancel-NOVA-3' })
  await tick(clock)

  // Чек-лист: подсказка про обязательные пункты и про «Не пройден».
  await ui.press({ key: 'mark-NOVA-2-dev_done-selfhosted' })
  await tick(clock)
  expect(await texts(ui, /^отметьте все обязательные пункты$/)).toHaveLength(1)
  expect(await texts(ui, /^«Не пройден» просит причину/)).toHaveLength(1)
  await ui.press({ key: 'item-NOVA-2-selfhosted' })
  await tick(clock)
  expect(await texts(ui, /^отметьте все обязательные пункты$/)).toEqual([])
  await ui.press({ key: 'cancel-NOVA-2' })
  await tick(clock)

  // Итог апрува — рамка, «под вашим именем», журнал и «в разговор не пишет».
  await ui.press({ key: 'approve-NOVA-3-merge_ready-human_approval' })
  await tick(clock)
  await ui.press({ key: 'yes-NOVA-3' })
  await tick(clock)
  expect((await ui.find({ key: 'outcome-NOVA-3' }))?.props.backgroundColor).toBeUndefined()
  expect(await texts(ui, /В разговор плагин ничего не пишет/)).toHaveLength(1)
  await ui.unmount()

  // Полоса над вводом (макет 06): рамка и заливка warning.
  const band = await $.ui.mount({ ...BAND, surface: 'desktop' } as never)
  expect((await band.findAll({ type: 'Box' }))[0]?.props).toMatchObject({ borderStyle: 'round', borderColor: 'warning', backgroundColor: TINT.light.warning })
  await band.unmount()
})

test('заливки блоков: текст и приглушённый текст темы читаются на каждой (≥ 4.5:1)', () => {
  for (const [p, tints] of [[LIGHT, TINT.light], [DARK, TINT.dark]] as const) {
    for (const bg of Object.values(tints)) for (const fg of [p.text, p.dim]) expect(contrast(fg, bg)).toBeGreaterThanOrEqual(4.5)
  }
})

test('макет: тема auto — без заливки, блоки держит рамка цвета тона, заголовок — цветом тона', async ($, on) => {
  const L = lab()
  await start($, on, L, 'NOVA-1', { theme: 'auto' })
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const plate = await ui.find({ key: 'plate-NOVA-1-analysis_done-human_approval' })
  expect(plate?.props.backgroundColor).toBeUndefined()
  expect(plate?.props.borderColor).toBe('warning')
  expect((await ui.find({ type: 'Text', text: /^Ждёт вашего апрува/ }))?.props.color).toBe('warning')
  await ui.unmount()
})

// --- AC-12: подписи -------------------------------------------------------------

function strings(node: unknown, out: string[] = []): string[] {
  if (typeof node === 'string') {
    if (node) out.push(node)
    return out
  }
  if (!node || typeof node !== 'object') return out
  const el = node as { props?: Record<string, unknown>; children?: unknown[] }
  const p = el.props ?? {}
  for (const k of ['label', 'placeholder', 'text']) if (typeof p[k] === 'string') out.push(p[k] as string)
  if (Array.isArray(p.options)) for (const o of p.options as { label?: string }[]) if (o.label) out.push(o.label)
  for (const c of el.children ?? (p.children as unknown[]) ?? []) strings(c, out)
  return out
}

const ALLOWED = new Set(
  [...Object.values(WORDS), ...Object.values(GATE_WORD), 'Cadence Lab', 'Не обновилось', 'clab', 'auth', 'login']
    .flatMap(w => w.toLowerCase().split(/[\s:·,«»—…]+/))
    .filter(Boolean),
)
const GLYPHS = new Set([...Object.values(GLYPH), '·', '…', '«', '»', '—', '?', ':', '$', '.', 'v'])

test('подписи плашек и форм — только данные цикла, словарь гейтов и слова плагина (NOVA и FORGE токенами)', async ($, on) => {
  let k = 0
  const names = new Map<string, string>()
  const tok = (s: string) => {
    if (!names.has(s)) names.set(s, `§${++k}`)
    return names.get(s)!
  }
  const L = lab(tok)
  const { j, clock } = await start($, on, L, 'NOVA-1 NOVA-2 NOVA-3 FORGE-1 FORGE-2', { whoami: { ...NOVA_WHO, principal: '§0' } })
  const seen: string[] = []
  const grab = async (ui: any, key: string) => seen.push(...strings(await ui.find({ key: `row-${key}` })))
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' })
  for (const key of ['NOVA-1', 'NOVA-2', 'NOVA-3', 'FORGE-1', 'FORGE-2']) await grab(ui, key)
  await ui.press({ key: 'approve-NOVA-1-analysis_done-human_approval' })
  await tick(clock)
  await grab(ui, 'NOVA-1')
  await ui.press({ key: 'return-FORGE-2-accept-sign_off' })
  await tick(clock)
  await ui.select({ key: 'target-FORGE-2', value: 'escalate' })
  await tick(clock)
  await grab(ui, 'FORGE-2')
  await ui.press({ key: 'mark-NOVA-2-dev_done-selfhosted' })
  await tick(clock)
  await grab(ui, 'NOVA-2')
  await ui.press({ key: 'cancel-NOVA-2' })
  await ui.press({ key: 'tab-approvals' })
  await tick(clock)
  const rows = await ui.findAll({ type: 'Box' })
  for (const r of rows) if (typeof r.key === 'string' && r.key.startsWith('appr-')) seen.push(...strings(await ui.find({ key: r.key })))
  seen.push(...strings(await ui.find({ key: 'tab-approvals' })))
  await ui.unmount()
  seen.push(...j.toasts)
  const bad: string[] = []
  for (const s of seen) {
    const rest = s.replace(/§\d+/g, ' ').replace(/[A-Z][A-Z0-9_]*-\d+/g, ' ').replace(/\d+/g, ' ').replace(/https?:\/\/\S+/g, ' ')
    for (const w of rest.split(/[\s·:,«»()—…?]+/).filter(Boolean)) {
      if (ALLOWED.has(w.toLowerCase()) || [...w].every(ch => GLYPHS.has(ch))) continue
      bad.push(`${w} ← «${s}»`)
    }
  }
  // Ни одного слова под смысл этапа: подписи — данные (токены) или словарь.
  expect(bad).toEqual([])
  expect(seen.length).toBeGreaterThan(60)
})
