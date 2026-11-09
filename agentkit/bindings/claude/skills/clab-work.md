---
argument-hint: ABC-12 [role]
---

## Notes for Claude Code

- **The task.** `$ARGUMENTS`.
- **Model level.** Without a model in `execution.models` compare the level
  with your own model by the table of `clab-run`: `low` — haiku, `medium` —
  sonnet, `high` — opus. Your model is outside the table (another
  provider) — name the level and your model without judging whether they
  match.
- **Cost.** The full model id of the session looks like `claude-opus-5-5`,
  not the alias `opus`.
- **Role documents.** They are not skills or agents of Claude Code: never
  `.claude/skills` or `.claude/agents`.
- **Stage executor.** Under `/clab-run` it is a sub-agent of type
  `clab-stage` (`.claude/agents/clab-stage.md`, installed by `clab setup`);
  the hook in that definition refuses a `clab` without the task-token
  prefix — a safety net, not a boundary.
- **In the background.** The Bash tool with `run_in_background`.
