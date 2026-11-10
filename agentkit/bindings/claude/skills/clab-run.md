---
argument-hint: ABC-12 [until <status>]
---

## Notes for Claude Code

- **The request.** `$ARGUMENTS`. On an epic — `/clab-run <epic>`.
- **The session temporary directory** is the one Claude Code names in the
  system prompt.
- **Starting the stage executor.** The executor is a sub-agent of type
  `clab-stage`, started with the `Agent` tool: `description` like `<key>:
  <role>`, `model` by "Executor model"; not in the background when the next
  step depends on the report, with `run_in_background` when other tasks run
  in parallel. This type has no MCP tools and no `Agent` tool. The
  definition lives in `.claude/agents/clab-stage.md` (installed by `clab
  setup`); Claude Code picks up the first definition in `.claude/agents/`
  after a session restart. In the prompt, the skill is invoked with the
  Skill tool, and the skills and agents folders of the kit are
  `.claude/skills` and `.claude/agents`.
- **The command check** is the `PreToolUse` hook on `Bash` in the
  definition of `clab-stage`. Claude Code runs the hooks of a project
  sub-agent only when the project folder is trusted (the trust dialog was
  accepted; a `-p` session does not accept it), otherwise it skips them
  silently. The probe is a sub-agent of type `clab-stage` started with the
  `Agent` tool (not in the background, `description` "clab-stage probe").
  The probe failed — likely the folder is not trusted or the definition
  appeared without a session restart: accept the trust dialog (an
  interactive `claude` in the project folder) and restart the session. Ask
  one `AskUserQuestion`: **continue as is** or **stop**.

**Executor model.** Without a model for Claude in `execution.models` the
level maps to the default table of the binding:

| Level | Model |
|---|---|
| `low` | `haiku` |
| `medium` | `sonnet` |
| `high` | `opus` |

The model goes into the `model` parameter of the `Agent` tool that starts
`clab-stage`. The agent definition keeps `model: inherit`: the start
parameter chooses the model, not the definition. No level, no `execution`
or a value not in the table — do not pass `model`: `inherit` runs the
executor on the chat's model. The line in the chat reads, for example,
"ABC-12 · developer · level high (task stage) → opus". The start with this
model is refused — say so in the chat and start the executor without
`model`.

- **Process path.** When a sub-agent cannot be started, the executor can
  be a process (section "Executor as a process"): `clab exec <key> --dir
  <run directory> --note-file <run directory>/note.md --agent claude`. The
  hook of `clab-stage` lives in the agent definition and is not attached
  to a process: the boundary is the environment `clab exec` builds. Its dry
  run says so in the hook line: the guard hook is not attached, isolation
  is by the environment.
- **Fallback path.** The old-way executor is a sub-agent of type
  `general-purpose` that claims the stage itself through `/clab-work`; its
  model is chosen by the same table: the `model` parameter of this
  sub-agent.
- **Asking the human.** "Ask the human and wait for the answer" is
  `AskUserQuestion`. The approval question is an ordinary message, not
  `AskUserQuestion` (that holds the turn).
- **In the background** — the Bash tool with `run_in_background`: the lease
  renewal, the wait for an approval; executors of a wave — the `Agent` tool
  with `run_in_background`.
- **The task panel** is the panel of the plugin `clab-plugin` (skill
  `clab-guide`).
- **Cost.** The full model id of the session looks like `claude-opus-5-5`,
  not the alias `opus`.
