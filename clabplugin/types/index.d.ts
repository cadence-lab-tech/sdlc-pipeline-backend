// Контракт состояния мода clab-plugin в $.state сессии. Токена здесь нет и
// быть не может: он живёт только в памяти модуля (решение 0067, п. 3).

// Строка задачи во вкладке «Этот чат»: ключ, заголовок и подпись статуса из
// версии цикла задачи. error — последняя ошибка чтения; прежние поля при
// ошибке остаются, как были.
export type TaskRow = {
  key: string
  title: string
  statusName: string
  error: string | null
  fetchedAt: number
}

export type Tab = 'chat' | 'approvals' | 'tasks'

// Что мод знает об инстансе: адрес и проект (clab whoami или настройки
// плагина), версия сервера из /v1/instance и есть ли у него ручка событий
// проекта (null — ещё не спрашивали). error — почему данные не обновляются.
export type Connection = {
  host: string | null
  project: string | null
  serverVersion: string | null
  events: boolean | null
  error: string | null
}

declare module 'claude-code' {
  interface PluginState {
    'clab-plugin': {
      keys: Shaped<string[]>
      rows: Shaped<Record<string, TaskRow>>
      tab: Shaped<Tab>
      connection: Shaped<Connection>
    }
  }
}
