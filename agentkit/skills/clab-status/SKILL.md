---
name: clab-status
description: Show the state of work in Cadence Lab — the queue, epics and tasks, what blocks a given task's transition — and close an approval gate when the human asks. Use for questions like "what is in the queue", "what is next in the backlog", "where is ABC-5 stuck", "approve the analysis of ABC-3".
argument-hint: [ABC-12] [approve]
---

# State of work in Cadence Lab

Request: `$ARGUMENTS`. The project comes from `git remote`; outside a
repository of the project — `SDLC_PROJECT=<KEY>`.

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
from the same call with `parent=<epic>`. The CLI chooses the same way.

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

A `human_approval` gate is closed by a human who holds the right of the
stage — a member at level `agent` whose roles include the role of the
task's current status, or a maintainer or admin — on the task card or with a
command, and **only when explicitly asked**. A task token never closes it.
Check the right with `clab gates ABC-12` (it prints, per transition and per
gate, whether the actor may and why not) instead of guessing. Approving the
work of one's own agent is allowed and is visible in the task feed:

```bash
clab decide ABC-12 --transition <transition> --gate <gate key> --status passed
```

Take the transition and the gate key from `clab gates ABC-12`, not from
memory. `--status failed` with `--details '{"reason":"…"}'` is a refusal;
after it the executor returns the task with a return transition. Before
approving, show the human what exactly they approve: the stage's artifacts
with titles and a short gist.
