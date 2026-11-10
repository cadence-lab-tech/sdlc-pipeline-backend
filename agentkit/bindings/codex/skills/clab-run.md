
## Notes for Codex

- **The stage executor is a process.** The binding `codex` has `executor:
  process`: start every executor with `clab exec <key> --dir <run
  directory> --note-file <run directory>/note.md` (section "Executor as a
  process"). A Codex agent file carries no hooks, MCP servers or
  environment of its own, so a sub-agent cannot hold the task-token
  boundary.
- **What the start carries.** Without a command after `--`, `clab exec`
  runs `codex exec --json` from the binding: sandbox `workspace-write` with
  network, `--add-dir <run directory>` for temporary files,
  `--ignore-user-config` (the MCP servers and settings of the user
  `config.toml` are not loaded; the login in `CODEX_HOME` stays), only the
  MCP server `sdlc` on the task token through `bearer_token_env_var`,
  `SDLC_TOKEN` excluded from the environment of shell commands
  (`shell_environment_policy`), and the command check as a `PreToolUse`
  hook on `Bash` attached to the start with `-c hooks...` and
  `--dangerously-bypass-hook-trust` — the same script as the check in the
  executor definition of the other bindings. The project `config.toml` of a
  trusted project still loads: keep MCP servers out of it.
- **The model.** The binding has no default table of levels: without a
  model for Codex in `execution.models` the executor runs on the default
  model of Codex, and `clab exec` says so. `--model <id>` chooses one.
- **Asking the human** is an ordinary message in the chat.
- **In the background** — a shell command with its output in `<run
  directory>/exec.log`; read the report after it exits.
- **Cost.** The full model id of the session as Codex reports it.
