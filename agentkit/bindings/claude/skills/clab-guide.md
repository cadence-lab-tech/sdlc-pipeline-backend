
## Notes for Claude Code

- **Setup.** `clab setup` connects the project's MCP server to Claude Code
  (`claude mcp add`, scope `local` or `user`) and installs this kit into
  `.claude/`: the skills into `.claude/skills`, the stage executor into
  `.claude/agents/clab-stage.md`, the manifest into `.claude/clab-kit.json`.
- **Role documents** are not skills or agents of Claude Code: never
  `.claude/skills` or `.claude/agents`.
- **The prompt `work_on_task`** is `/mcp__sdlc__work_on_task <key>` in
  Claude Code.
- **The plugin.** The Claude plugin `clab-plugin` is a separate,
  machine-wide step: `clab setup --plugin` (`--dry-run` shows the folder,
  version and `claude` commands without changing anything). The plugin ships
  inside `clab` and has the same version; nothing is downloaded, and it is
  installed for the user, not for one project. It signs in with your own
  `clab auth login` and has no login of its own. In a chat it opens with
  `/clab-plugin` (the command is not namespaced: `/clab-plugin`, not
  `/clab-plugin:…`): `/clab-plugin <KEY>`, `tasks`, `approvals`, `remove
  <KEY>`. It shows the tasks the chat mentions and their status, never
  changes a task or the conversation by itself, and does nothing in
  sessions without a window (`claude -p`, runners, executors).
