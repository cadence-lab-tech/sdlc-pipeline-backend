---
name: clab-backlog
description: Create an epic or tasks in Cadence Lab with a breakdown and an order — from a plan, from a bug found along the way or on request. Use when asked "file a task", "break this into tasks", "add it to the backlog".
argument-hint: what to create
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
- Order is set **only by links** `blocks` (A blocks B: A goes first); the
  engine reads links, not the order of creation.
- Write long descriptions to a file and pass them with `$(cat …)`.
- Keys are issued by the server. Tasks are not deleted — an unneeded one
  is taken off with the `cancel` transition.

## Afterwards

Show the list of what was created: keys, titles, links. Tasks stay in the
initial status of their cycle; start them (`clab transition <key> --to
<start transition>`) only when asked — starting work is the human's
decision.
