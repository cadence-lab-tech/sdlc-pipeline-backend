---
name: clab-status
description: Show the state of work in Cadence Lab — the queue, epics and tasks, what blocks a given task's transition — close an approval gate or move a task to a status when the human asks. Use for questions like "what is in the queue", "what is next in the backlog", "where is ABC-5 stuck", "approve the analysis of ABC-3", "send ABC-7 back to analysis".
---

# State of work in Cadence Lab

The request is the text this skill was invoked with. `clab` finds the
project itself — `--project`, then `SDLC_PROJECT`, then the binding line in
`AGENTS.md` or `CLAUDE.md`, then the `git remote` of the current directory;
where it finds none, bind the directory (`clab setup --project <KEY>`) or
pass `--project <KEY>` to the commands that take a project.

## Overview

```bash
clab tasks --type epic          # epics and their statuses
clab tasks --parent ABC-2       # tasks of an epic
clab queue                      # what can be claimed now, by role
clab metrics --period 30d       # where tasks get stuck, which gates fail
clab index                      # index state of the repositories
```

Web interface: the project Overview `https://<instance>/p/<KEY>/overview` —
what awaits a decision, what is stuck, what is in progress and in the queue;
a task card `https://<instance>/p/<KEY>/tasks/<task key>` (the instance
address — `clab whoami`).

## The backlog and what is next

Answer "what is next in the backlog" with `clab backlog --next` (needs server
v0.25.0 or newer; a kit of an older version has no such commands and keeps
working). Agents should add `--json`: the human-readable output may change.

```bash
clab backlog --next --json      # {next, epic, skipped, first_blocked}
clab backlog --next             # the same for a human
clab backlog                    # the top of the list: no., key, type, priority, marks
clab backlog --parent ABC-2     # the backlog of one epic
clab backlog --limit 100        # how many rows to show, 1…500 (default 50)
```

`--next` takes the first task without open blockers; an epic that is in work
opens up to its first free child (`epic` in the answer is that epic). "In
work" and "waits for <keys>" are marks from the server's output; do not name
statuses or roles in the answer. The lines of the human-readable output
come from the CLI as it is built (today they are in Russian), so go by the
meaning below and prefer `--json`. Four cases:

- **A "next" line** (key, priority, title) — name the key, the priority and
  the title; a second line names the epic when the task was found inside
  one.
- **Blocked tasks skipped** — rows above the next one wait for their
  blockers (`skipped` in `--json`); name them and what they wait for.
- **No free task** — all rows wait for blockers: `next` is empty and
  `first_blocked` names the first task in order with its blockers; say
  exactly that.
- **Empty backlog** — say the backlog is empty (exit code 0).

Then show the top of the list with `clab backlog` (for an epic —
`--parent <epic>`).

Over MCP, without the CLI, there is no `--next`: call `sdlc_list_tasks` with
`backlog: true` and `top_level: true`, take the first row without
`blocked_by`; for an epic in work (`in_work: true`) take its first free child
with a second call, `sdlc_list_tasks` with `parent=<epic>`. The CLI chooses the
same way.

**Priority.** `clab edit ABC-12 --priority low|normal|high|urgent` (MCP —
`sdlc_update_task`). A priority **does not change the place in the
backlog**; to raise or lower a task use `clab rank` (skill `clab-backlog`,
level `agent` and above).

**Page.** The same backlog in the web interface is
`https://<instance>/p/<KEY>/backlog` (the instance address — `clab whoami`);
the page exists on server v0.25.0 and newer.

## A task

```bash
clab show ABC-12                          # status, run, scope, transitions
clab gates ABC-12                         # what blocks each transition
clab gates ABC-12 --transition <transition>
clab drift ABC-12                         # open divergences from main
clab usage ABC-12 --show                  # cost of the runs
```

Answer to the point: which stage, who holds the lease, which gates have not
passed and why, what is needed from the human. Read artifact contents with
`clab artifact <id>` or `sdlc_get_artifact` when the answer needs them.

## Approval

A `human_approval` gate is closed by a human on the task card, with a
command or with the MCP tool `sdlc_decide_gate`, and **only when explicitly
asked**. For a person the right of the stage comes from the level: an
employee — a person at level `agent` — decides the gates of any stage, the
role of the status is not needed for that; a maintainer or admin may too.
A task token never closes it. Check the right with `clab gates ABC-12` (it
prints, per transition and per gate, whether the actor may and why not)
instead of guessing. Approving the work of one's own agent is allowed and
is visible in the task feed:

```bash
clab decide ABC-12 --transition <transition> --gate <gate key> --status passed
```

Take the transition and the gate key from `clab gates ABC-12`, not from
memory. `--status failed` with `--details '{"reason":"…"}'` is a refusal;
after it the executor returns the task with a return transition. Before
approving, show the human what exactly they approve: the stage's artifacts
with titles and a short gist. The choice of optional stages at the approval
is set by the same person (`clab edit ABC-12 --stage <status>=off`).

## Moving a task to a status

A task that stands in the wrong place is moved, not copied — and **only
when the human explicitly asks**, with a reason:

```bash
clab move ABC-12 --to <status> --reason "…"
```

An employee or a maintainer may move a task (`actor_rights.move` in
`clab show ABC-12 --json`); a bot and a task token may not. The status is
any status of the task's cycle: forward, back, to a terminal status and
out of it; the current status means one more pass of the stage. Gates are
not checked on a move: the current run closes with the outcome `moved`,
the lease is dropped, and the target status opens a new attempt with its
own gates; the task feed gets a `task.moved` event with the reason. After
the move tell the human the new status and the attempt number.

- A `stage_disabled` refusal — the stage is switched off for the task:
  first `clab edit ABC-12 --stage <status>=on`, then the move.
- "The server does not know moving to a status" (the CLI prints it in its
  own language) — the server is older (v0.25.0 or newer is needed): tell
  the human and do not look for a workaround such as a copy of the task or
  another token.
- Over MCP — `sdlc_move_task` with `task_key`, `to`, `reason`.
