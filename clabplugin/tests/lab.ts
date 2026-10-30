// Подставной сервер с записью для тестов решений по гейтам (CLAB-326): два
// проекта с непохожими циклами — NOVA (форма нашего цикла) и FORGE (чужой:
// апрув на двух людей рядом с чек-листом, приёмка на статусе ожидания,
// возврат с артефактом своего вида, возврат в выключенный этап, возврат под
// исчерпанным пределом попыток). Сервер держит задачи в памяти, отвечает на
// GET карточки, версии цикла, проекта и «Ждут меня» и исполняет POST решения,
// перехода и сдачи так, как это делает движок, в той мере, в какой это видит
// мод. Все подписи проходят через name(): тест подписей подменяет их токенами.

import type { Reply } from './world'

type Gate = {
  gate_key: string
  gate_name: string
  type: string
  status: string
  blocking: boolean
  reason: string | null
  details: Record<string, unknown> | null
}

type Tr = { key: string; name: string; kind: string; to_status: string; lands_in: string | null; unavailable?: string; allowed: boolean; gates: Gate[] }

type Artifact = { id: string; kind: string; title: string; version: number; stage_run_id: string; created_at: string }

type LabTask = {
  key: string
  title: string
  project: string
  workflow: string
  status: string
  run: string
  artifacts: Artifact[]
  // Переходы по статусам: у задачи свой набор, чтобы вердикты гейтов жили
  // вместе с ней.
  transitions: Record<string, Tr[]>
  approvals: Record<string, string[]>
}

export type Post = { path: string; body: any }

export type Lab = {
  tasks: Record<string, LabTask>
  levels: Record<string, string>
  posts: Post[]
  // Ответ на запись вместо исполнения: код и тело (403, 500, 409 сверки).
  refuse: ((post: Post) => Reply | undefined) | null
  // Ручки «Ждут меня» нет (сервер старше).
  noApprovals: boolean
  serve: (method: string, path: string, body: unknown) => Reply | undefined
  // Сдача артефакта на этап «со стороны» — как агент между показом и решением.
  submit: (key: string, kind: string) => void
  decide: (key: string, transition: string, gate: string, status: string) => void
  writes: () => Post[]
}

const STAMP = Date.parse('2026-10-30T12:00:00Z')

export function lab(name: (s: string) => string = s => s): Lab {
  let clock = 0
  let ids = 0
  const at = () => new Date(STAMP + ++clock * 1000).toISOString()
  const n = name

  const g = (key: string, gname: string, type: string, extra: Partial<Gate> = {}): Gate => ({
    gate_key: key,
    gate_name: n(gname),
    type,
    status: 'pending',
    blocking: true,
    reason: null,
    details: null,
    ...extra,
  })

  const workflows: Record<string, any> = {
    'wf-nova': {
      id: 'wf-nova',
      name: n('Simple'),
      version: 6,
      statuses: [
        { key: 'new', name: n('Новая'), category: 'backlog', position: 1, is_initial: true, roles: [] },
        { key: 'analysis', name: n('Аналитика'), category: 'active', position: 2, roles: [{}] },
        { key: 'development', name: n('Разработка'), category: 'active', position: 3, roles: [{}] },
        { key: 'testing', name: n('Тестирование'), category: 'active', position: 4, roles: [{}] },
        { key: 'merge', name: n('Слияние'), category: 'active', position: 5, roles: [{}] },
        { key: 'done', name: n('Готово'), category: 'done', position: 6, is_terminal: true, roles: [] },
      ],
      transitions: [
        { key: 'dev_done', gates: [{ key: 'selfhosted', type: 'checklist', config: { items: [{ id: 'selfhosted', text: n('Раздел «Self-hosted» написан'), required: true }, { id: 'notes', text: n('Заметки релиза'), required: false }] } }] },
        { key: 'merge_conflict', gates: [{ key: 'problem_report', type: 'artifacts_required', config: { kinds: ['problem.report'] } }] },
      ],
    },
    'wf-forge': {
      id: 'wf-forge',
      name: n('Story'),
      version: 3,
      statuses: [
        { key: 'backlog', name: n('Backlog'), category: 'backlog', position: 1, is_initial: true, roles: [] },
        { key: 'build', name: n('Build'), category: 'active', position: 2, roles: [{}] },
        { key: 'design', name: n('Design'), category: 'active', position: 3, roles: [{}], is_optional: true, enabled_by_default: false },
        { key: 'review', name: n('Review board'), category: 'waiting', position: 4, roles: [{}] },
        { key: 'shipped', name: n('Shipped'), category: 'done', position: 5, is_terminal: true, roles: [] },
      ],
      transitions: [
        { key: 'ship', gates: [{ key: 'rollback', type: 'checklist', config: { items: [{ id: 'plan', text: n('Plan written'), required: true }] } }] },
        { key: 'send_back', gates: [{ key: 'rework', type: 'artifacts_required', config: { kinds: ['forge.rework'] } }] },
      ],
    },
  }

  const tr = (key: string, name: string, kind: string, to: string, gates: Gate[], extra: Partial<Tr> = {}): Tr => ({
    key,
    name: n(name),
    kind,
    to_status: to,
    lands_in: to,
    allowed: false,
    gates,
    ...extra,
  })

  const tasks: Record<string, LabTask> = {}
  const add = (key: string, title: string, workflow: string, status: string, transitions: Record<string, Tr[]>) => {
    tasks[key] = { key, title: n(title), project: key.replace(/-\d+$/, ''), workflow, status, run: `run-${key}-${status}`, artifacts: [], transitions, approvals: {} }
  }

  // NOVA-1: аналитика сдана, ждёт апрува; возвратов из статуса нет.
  add('NOVA-1', 'Апрув из панели', 'wf-nova', 'analysis', {
    analysis: [tr('analysis_done', 'Аналитика готова', 'advance', 'development', [g('artifacts', 'Артефакты сданы', 'artifacts_required', { status: 'passed' }), g('human_approval', 'Апрув аналитики', 'human_approval', { reason: n('апрувов 0 из 1'), details: { approvals: 0, min_approvals: 1 } })])],
    development: [],
  })
  // NOVA-2: разработка сдана, ручной чек-лист ждёт отметки.
  add('NOVA-2', 'Чек-лист', 'wf-nova', 'development', {
    development: [
      tr('dev_done', 'Разработка готова', 'advance', 'testing', [g('artifacts', 'Разбор сдан', 'artifacts_required', { status: 'passed' }), g('selfhosted', 'Self-hosted учтён', 'checklist', { reason: n('не отмечено пунктов чек-листа: 1'), details: { unchecked: [{ id: 'selfhosted', text: 'x' }] } })]),
      tr('reject_to_analysis', 'Вернуть аналитику', 'reject', 'analysis', [g('problem_report', 'Описание проблемы', 'artifacts_required', { status: 'failed', details: { missing: ['problem.report'] } })]),
    ],
  })
  // NOVA-3: на слиянии, ждёт апрува; возврат требует описания проблемы.
  add('NOVA-3', 'Слить', 'wf-nova', 'merge', {
    merge: [
      tr('merge_ready', 'Готово к слиянию', 'advance', 'done', [g('human_approval', 'Разрешение слить', 'human_approval', { reason: n('апрувов 0 из 1'), details: { approvals: 0, min_approvals: 1 } })]),
      tr('merge_conflict', 'Конфликт слияния', 'reject', 'development', [g('problem_report', 'Описание проблемы', 'artifacts_required', { status: 'failed', details: { missing: ['problem.report'] } })]),
    ],
    development: [],
  })
  // FORGE-1: чек-лист отмечен, апрув на двух людей.
  add('FORGE-1', 'Платёжный шлюз', 'wf-forge', 'build', {
    build: [tr('ship', 'Ship to board', 'advance', 'review', [g('rollback', 'Rollback plan', 'checklist', { status: 'passed' }), g('two_people', 'Two sign-offs', 'human_approval', { reason: n('апрувов 0 из 2'), details: { approvals: 0, min_approvals: 2 } })])],
  })
  // FORGE-2: на статусе ожидания, приёмка; три возврата — свой вид
  // артефакта, в выключенный этап, под исчерпанным пределом.
  add('FORGE-2', 'Токены темы', 'wf-forge', 'review', {
    review: [
      tr('accept', 'Accept', 'advance', 'shipped', [g('sign_off', 'Go-live sign-off', 'human_approval', { reason: n('апрувов 0 из 1'), details: { approvals: 0, min_approvals: 1 } })]),
      tr('send_back', 'Send back', 'reject', 'build', [g('rework', 'Rework note', 'artifacts_required', { status: 'failed', details: { missing: ['forge.rework'] } })]),
      tr('redesign', 'Redesign', 'reject', 'design', [], { lands_in: null, unavailable: 'stage_disabled' }),
      tr('escalate', 'Escalate', 'reject', 'backlog', [g('attempts', 'Attempts limit', 'attempts_limit', { status: 'failed', reason: n('попыток 3 из 3') })]),
    ],
    build: [],
  })

  const RESOLVED = new Set(['passed', 'waived', 'skipped'])
  const settle = (t: LabTask) => {
    for (const x of t.transitions[t.status] ?? []) x.allowed = !x.unavailable && x.gates.every(y => !y.blocking || RESOLVED.has(y.status))
  }
  for (const t of Object.values(tasks)) {
    settle(t)
    t.artifacts.push({ id: `a-${++ids}`, kind: 'result', title: n('Результат этапа'), version: 1, stage_run_id: t.run, created_at: at() })
  }

  const latest = (t: LabTask) => {
    const own = t.artifacts.filter(a => a.stage_run_id === t.run)
    return own.length ? own[own.length - 1]!.id : ''
  }

  const detail = (t: LabTask) => ({
    task: { key: t.key, title: t.title, status_key: t.status, status_name: t.status, workflow_id: t.workflow, project_key: t.project, properties: { type: n('task') } },
    stage_runs: [{ id: t.run, status_key: t.status, kind: 'stage', attempt: 1, finished_at: null }],
    transitions: t.transitions[t.status] ?? [],
    artifacts: t.artifacts,
    attachments: [],
    children: [],
    repo_refs: [],
  })

  const approvalsOf = () => {
    const out: unknown[] = []
    for (const t of Object.values(tasks)) {
      const wf = workflows[t.workflow]
      for (const x of t.transitions[t.status] ?? []) {
        if (x.kind !== 'advance') continue
        const human = (y: Gate) => y.type === 'human_approval' || y.type === 'checklist'
        if (x.gates.some(y => y.blocking && !human(y) && !RESOLVED.has(y.status))) continue
        for (const y of x.gates) {
          if (!y.blocking || !human(y) || y.status !== 'pending') continue
          out.push({
            task: { key: t.key, title: t.title, status_key: t.status },
            transition: { key: x.key, name: x.name, to_status: x.to_status, lands_in: x.lands_in, lands_in_name: wf.statuses.find((s: any) => s.key === x.lands_in)?.name },
            gate: { gate_key: y.gate_key, gate_name: y.gate_name, type: y.type, status: y.status },
          })
        }
      }
    }
    return out
  }

  const conflict = (message: string, details: Record<string, unknown> = {}): Reply => ({ status: 409, body: { error: { code: 'conflict', message, details } } })

  const L: Lab = {
    tasks,
    levels: { NOVA: 'maintainer', FORGE: 'admin' },
    posts: [],
    refuse: null,
    noApprovals: false,
    writes: () => L.posts,
    submit(key, kind) {
      const t = tasks[key]!
      t.artifacts.push({ id: `a-${++ids}`, kind, title: n(kind), version: 2, stage_run_id: t.run, created_at: at() })
    },
    decide(key, transition, gate, status) {
      const t = tasks[key]!
      const x = (t.transitions[t.status] ?? []).find(y => y.key === transition)!
      const y = x.gates.find(z => z.gate_key === gate)!
      y.status = status
      settle(t)
    },
    serve(method, path, body) {
      if (method === 'GET') {
        let m = /^\/tasks\/([A-Z]+-\d+)$/.exec(path)
        if (m && tasks[m[1]!]) return { status: 200, body: detail(tasks[m[1]!]!) }
        m = /^\/workflows\/(.+)$/.exec(path)
        if (m && workflows[m[1]!]) return { status: 200, body: workflows[m[1]!] }
        m = /^\/projects\/([A-Z]+)\/approvals$/.exec(path)
        if (m) {
          if (L.noApprovals) return undefined
          const level = L.levels[m[1]!] ?? 'viewer'
          const can = level === 'maintainer' || level === 'admin'
          return { status: 200, body: { approvals: can ? approvalsOf().filter((a: any) => a.task.key.startsWith(`${m![1]}-`)) : [], can_decide: can, truncated: false } }
        }
        m = /^\/projects\/([A-Z]+)$/.exec(path)
        if (m) return { status: 200, body: { project: { key: m[1] }, access: { level: L.levels[m[1]!] ?? 'viewer', allowed_roles: null } } }
        if (path === '/instance') return { status: 200, body: { version: 'v9.9.9' } }
        return undefined
      }
      const post = { path, body }
      L.posts.push(post)
      const refused = L.refuse?.(post)
      if (refused) return refused
      let m = /^\/tasks\/([A-Z]+-\d+)\/transitions\/([a-z_]+)\/gates\/([a-z_]+)\/decision$/.exec(path)
      if (m) {
        const t = tasks[m[1]!]!
        const x = (t.transitions[t.status] ?? []).find(y => y.key === m![2])
        const y = x?.gates.find(z => z.gate_key === m![3])
        if (!x || !y) return { status: 404, body: { error: { code: 'not_found', message: 'нет гейта' } } }
        const b = body as { status: string; details?: any; seen_artifact_id?: string }
        if (b.seen_artifact_id !== undefined && b.seen_artifact_id !== latest(t)) {
          return conflict('результат этапа изменился', { reason: 'stage_result_changed' })
        }
        if (y.type === 'human_approval') {
          if (b.status === 'failed') {
            y.status = 'failed'
            y.details = { decision: b.details }
          } else {
            const min = Number((y.details as any)?.min_approvals ?? 1)
            const got = Number((y.details as any)?.approvals ?? 0) + 1
            y.details = { approvals: got, min_approvals: min }
            y.status = got >= min ? 'passed' : 'pending'
            y.reason = got >= min ? null : `апрувов ${got} из ${min}`
          }
        } else if (y.type === 'checklist') {
          // Как движок: «Не пройден» — failed с причиной, пока чек-лист не
          // отметят заново; отметка — passed.
          if (b.status === 'failed') {
            y.status = 'failed'
            y.reason = `чек-лист отмечен непройденным: ${b.details?.reason ?? ''}`
            y.details = { decision: b.details }
          } else {
            y.status = 'passed'
            y.reason = null
            y.details = null
          }
        }
        settle(t)
        return { status: 200, body: { gate_key: y.gate_key, status: b.status } }
      }
      m = /^\/tasks\/([A-Z]+-\d+)\/transitions\/([a-z_]+)$/.exec(path)
      if (m) {
        const t = tasks[m[1]!]!
        const x = (t.transitions[t.status] ?? []).find(y => y.key === m![2])
        if (!x) return { status: 404, body: { error: { code: 'not_found', message: 'нет перехода' } } }
        if (!x.allowed) return { status: 409, body: { error: { code: 'gates_not_satisfied', message: `переход «${x.key}» заблокирован гейтами` } } }
        t.status = x.lands_in ?? x.to_status
        t.run = `run-${t.key}-${t.status}-${++ids}`
        settle(t)
        const wf = workflows[t.workflow]
        return { status: 200, body: { task: { key: t.key, status_key: t.status, status_name: wf.statuses.find((s: any) => s.key === t.status)?.name } } }
      }
      m = /^\/tasks\/([A-Z]+-\d+)\/artifacts$/.exec(path)
      if (m) {
        const t = tasks[m[1]!]!
        const b = body as { kind: string; title: string }
        const a = { id: `a-${++ids}`, kind: b.kind, title: b.title, version: 1, stage_run_id: t.run, created_at: at() }
        t.artifacts.push(a)
        for (const x of t.transitions[t.status] ?? []) {
          for (const y of x.gates) {
            const missing = (y.details as any)?.missing as string[] | undefined
            if (y.type === 'artifacts_required' && missing?.includes(b.kind)) {
              y.status = 'passed'
              y.details = null
            }
          }
        }
        settle(t)
        return { status: 201, body: a }
      }
      return undefined
    },
  }
  return L
}
