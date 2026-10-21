---
name: clab-status
description: Show the state of work in Cadence Lab — the queue, epics and tasks, what blocks a given task's transition — and close an approval gate when the human asks. Use for questions like "what is in the queue", "where is ABC-5 stuck", "approve the analysis of ABC-3".
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

Web interface: the board `https://<instance>/p/<KEY>/board`, a task card
`https://<instance>/p/<KEY>/tasks/<task key>` (the instance address —
`clab whoami`).

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

A `human_approval` gate is closed by a human — on the task card or with a
command, and **only when explicitly asked**:

```bash
clab decide ABC-12 --transition <transition> --gate <gate key> --status passed
```

Take the transition and the gate key from `clab gates ABC-12`, not from
memory. `--status failed` with `--details '{"reason":"…"}'` is a refusal;
after it the executor returns the task with a return transition. Before
approving, show the human what exactly they approve: the stage's artifacts
with titles and a short gist.
