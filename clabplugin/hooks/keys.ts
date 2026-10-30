// Поимка ключей задач. Мод только смотрит на то, что уже происходит в чате, —
// вход хуков он не меняет и вызовов не запрещает.
//
// Префикс ключа — проект из clab whoami или настроек плагина, а не литерал:
// у другой компании проект называется иначе.

const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

// Ключи проекта в тексте: PROJ-12, но не XPROJ-12 и не PROJ-12a.
export function keysOf(text: string, project: string | null): string[] {
  if (!project) return []
  const re = new RegExp(`(?<![A-Za-z0-9_])${escape(project)}-\\d+(?![A-Za-z0-9_])`, 'g')
  return [...new Set(text.toUpperCase().match(re) ?? [])]
}

// Любой ключ вида ABC-12: так человек добавляет задачу руками, в том числе
// из другого проекта того же инстанса.
export function anyKeys(text: string): string[] {
  return [...new Set(text.toUpperCase().match(/(?<![A-Z0-9_])[A-Z][A-Z0-9_]*-\d+(?![A-Z0-9_])/g) ?? [])]
}

// Команда агента в первой строке промпта: /clab-run KEY, /clab-work KEY role,
// /<пространство>:clab-status KEY. Ниже первой строки может оказаться
// развёрнутый текст скила с примерами ключей — их не ловим.
export function promptKeys(text: string, project: string | null, own: string): string[] {
  const first = text.trimStart().split('\n', 1)[0] ?? ''
  const m = /^\/(?:[\w-]+:)?(clab-[\w-]+)\b(.*)$/.exec(first)
  if (!m || m[1] === own) return []
  return keysOf(m[2] ?? '', project)
}

// Команды clab, которые только читают: после остальных задача на сервере
// могла измениться, и строку стоит перечитать. Список читающих короче и
// надёжнее списка меняющих: новая меняющая команда не потеряется.
const READING = new Set(['show', 'gates', 'tasks', 'artifact', 'search', 'drift', 'whoami', 'version', 'auth', 'help'])

function clabMutates(command: string): boolean {
  const m = /\bclab\s+(?:--\S+\s+)*([a-z-]+)/.exec(command)
  return !!m && !READING.has(m[1] ?? '')
}

export type ToolHit = { keys: string[]; mutates: boolean }

// Ключи из вызова тула: скил clab-*, Bash с clab, тулы MCP-сервера sdlc.
export function toolKeys(tool: string, input: Record<string, unknown>, project: string | null): ToolHit {
  if (tool === 'Skill') {
    const skill = String(input.skill ?? '').replace(/^.*:/, '')
    if (!skill.startsWith('clab-')) return { keys: [], mutates: false }
    return { keys: keysOf(String(input.args ?? ''), project), mutates: false }
  }
  if (tool === 'Bash') {
    const command = String(input.command ?? '')
    if (!/\bclab\b/.test(command)) return { keys: [], mutates: false }
    return { keys: keysOf(command, project), mutates: clabMutates(command) }
  }
  if (tool.startsWith('mcp__sdlc__')) {
    const name = tool.slice('mcp__sdlc__'.length)
    const args: Record<string, unknown> = {}
    for (const [k, v] of Object.entries(input)) {
      if (k !== 'tool' && k !== 'tool_use_id' && k !== 'agentId' && k !== 'consent' && k !== 'requestMeta') args[k] = v
    }
    return { keys: keysOf(JSON.stringify(args), project), mutates: !/(get|list|search|show|status|check|whoami)/.test(name) }
  }
  return { keys: [], mutates: false }
}
