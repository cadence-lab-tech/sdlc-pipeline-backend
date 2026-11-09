---
name: clab-backlog
description: Create an epic or tasks in Cadence Lab with a breakdown and an order — from a plan, from a bug found along the way or on request. Use when asked "file a task", "break this into tasks", "add it to the backlog".
---

# Cadence Lab backlog

Request: `$ARGUMENTS`. The project comes from `git remote`; outside a
repository of the project put `SDLC_PROJECT=<KEY>` before the commands.

## What to create

- **Epic** (`--type epic`) — a feature or a plan step done by several
  tasks. In the description: where it comes from, what is wrong today, the
  expected behaviour, the scope by repository. The analyst of the epic
  breaks it down at their stage; if the plan already has a breakdown,
  create the child tasks right away and the analyst will refine them.
- **Task** (`task`) — one verifiable result. In the description: the
  symptom or the goal, the cause in the code with links to files and
  functions, the expected behaviour, what is out of scope. A bug —
  `--labels bug` and priority `high`.

Before creating, check that it does not exist yet: `clab tasks`,
`clab tasks --type epic`. The task type must be declared by an active
cycle — `clab workflow list` shows what there is.

## Commands

```bash
clab create --title "…" --type epic --priority high --labels phase2 \
  --repos acme/api,acme/web --properties '{"multi_repo":true}' \
  --description "$(cat description.md)"
clab create --title "…" --parent ABC-2 --repos acme/api --description "…"
clab relate ABC-3 --to ABC-4 --rel blocks --note "why"
clab assign ABC-3 --to agent:runner
```

- Name repositories the way they are bound to the project (`clab index`
  lists them).
- Two or more repositories in scope need
  `--properties '{"multi_repo":true}'`, otherwise the scope is refused.
- A `blocks` link says **what waits for what** (A blocks B: B waits for A),
  not where a task stands in the backlog. The place in the backlog is a
  rank, moved with `clab rank` (section "Put it in place"); `clab backlog
  --next` skips tasks that still have open blockers. The order of creation
  means nothing.
- Write long descriptions to a file and pass them with `$(cat …)`.
- Keys are issued by the server. Tasks are not deleted — an unneeded one
  is taken off with the `cancel` transition.

## Put it in place

A new task goes to the **end of its group**. A group is the top level of the
project or the children of one epic: a task moves only inside its own group.
For another place use `clab rank` with exactly one of four places (needs
server v0.25.0 or newer; a kit of an older version has no order commands and
keeps working):

```bash
clab rank ABC-12 --before ABC-15   # above ABC-15 (it becomes the neighbour below)
clab rank ABC-12 --after ABC-15    # below ABC-15 (it becomes the neighbour above)
clab rank ABC-12 --top             # to the start of the group: top level or epic
clab rank ABC-12 --bottom          # to the end of the group
clab backlog                       # check: the top level in backlog order
clab backlog --parent ABC-2        # check: the backlog of an epic
```

Example. The request "raise ABC-12 above ABC-15" is `clab rank ABC-12
--before ABC-15`. The CLI prints one line (a move above a neighbour, a move
to the start of the backlog or of an epic, or "already in place"); pass it
to the human. Then run `clab backlog` (for the tasks of an epic —
`clab backlog --parent <epic>`) and show that ABC-12 stands above ABC-15.
Over MCP the same is `sdlc_rank_task` (`task` and exactly one of `before`,
`after`, `position`).

Refusals to understand, not to work around:

- **"… in different backlog groups"** (`400`): the neighbour belongs to
  another group. Explain to the human that a task moves inside one epic, or
  an epic moves by itself; a task of an epic is not carried to the top level
  with this command, and do not try to carry it yourself.
- **"the server does not know the backlog order: update the server to
  v0.25.0 or newer"** (`404`): an older server. Pass this to the human and do
  not look for a workaround with a command from elsewhere.
- Two places or none: the CLI answers with an error before any request;
  repeat with one place. A closed task is not moved (`409`).
- The backlog order is moved at level `agent` and above. A task token (a
  stage executor) moves only the children of its own task; the order of the
  project is the human's decision, and the executor says so in its report.

A priority (`clab edit ABC-12 --priority high`) does not change the place in
the backlog: to raise a task use `clab rank`.

## Afterwards

Show the list of what was created: keys, titles, links — and, if tasks were
put in place, `clab backlog` after the move; the page
`https://<instance>/p/<KEY>/backlog` (the instance address — `clab whoami`)
shows the same order. Tasks stay in the initial status of their cycle; start
them (`clab transition <key> --to <start transition>`) only when asked —
starting work is the human's decision.
