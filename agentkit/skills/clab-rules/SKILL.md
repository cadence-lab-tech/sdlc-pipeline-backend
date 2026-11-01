---
name: clab-rules
description: Change the roles or the cycle of a Cadence Lab project — a role's documents, the artifact contract, statuses, transitions and gates — through a rules directory (export, edit, import) and draft activation. Use when asked to adjust a role, add a gate or a stage, or create a new role.
argument-hint: what to change
---

# Roles and cycle of a project

Request: `$ARGUMENTS`. The project comes from `git remote`; outside a
repository of the project — `SDLC_PROJECT=<KEY>`.

Roles and cycles are project data. They change by versions, never in
place: a role gets a new version of its whole set of documents, a cycle gets
a draft that is validated and activated. Changing them needs the
`maintainer` level in the project; a stage executor's task token cannot do
it at all.

Rules are the team's agreements. Change them only when the human asked for
it, and show them what exactly changes before applying.

Two kinds of documents, do not mix them up:

- **role documents** — `documents` inside a role (`main.md`, `git.md`, …):
  ordered parts of one role's rules, delivered only to the executor of that
  role. They are not skills or agents of Claude Code and never go into
  `.claude/skills` or `.claude/agents`;
- **project documents** — `documents` at the top of the bundle (guidelines,
  architecture, ADRs): read by every role.

The rules of the import format (names, types, what is refused) are in
`clab import-guide`; run it and follow it instead of copying its text here.

## Steps

1. **Export what is active into a directory**: `clab export --dir rules/`
   (a temporary directory; `--force` replaces `bundle.json` and `roles/` in
   a non-empty one). The layout:
   - `rules/bundle.json` — repositories, project documents and cycles
     (`workflows[]`); roles are not in it;
   - `rules/roles/<role key>/role.json` — the role's fields without document
     content, and `documents` as a list of `{name, kind}` in delivery order;
   - `rules/roles/<role key>/<name>.md` — the content of a role document,
     byte for byte.

   Import applies everything in the directory: remove the repositories and
   project documents from `bundle.json` unless you meant to change them.
2. **Edit the files.**
   - A role document: edit `<name>.md`. Add one: put the file next to the
     others and add its `{name, kind}` line to `documents` of `role.json` at
     the place where it should be delivered. Remove one: delete both the file
     and the line. The order of lines is the order of delivery. A file
     without a line or a line without a file is refused before anything is
     sent.
   - The role's other fields live in `role.json`: the contract
     (`output_contract[]` with `kind`, `title`, `format`, `required`), inputs
     (`input_kinds`), the model level.
   - A cycle in `bundle.json`: statuses (`role` — the key of the executing
     role), transitions and gates. Gate types: `artifacts_required`,
     `schema_validation`, `checklist`, `human_approval`, `agent_review`,
     `external_check`, `repo_ref_required`, `drift_clear`, `repo_scope`,
     `dependencies_closed`, `attempts_limit`, `stage_time_limit`. The role in
     `agent_review` cannot be the role executing the status — validation
     refuses it.
     `attempts_limit` caps how many times a task may enter the transition's
     target status: `{"max_attempts": 3}`, an integer of at least 1, never on a
     transition into a terminal status. It goes on the transition itself,
     usually a `reject`; roles carry no such setting. Only a human lifts it, with
     `waived`, one attempt per decision. `agent_review` takes an optional
     `max_attempts` too: after that many failed reviews on one stage run no new
     review starts, the gate fails with "N of N reviews" and only a human lifts
     it with `waived`; a new submission clears that waiver.
     `stage_time_limit` caps how long the stage opened by this transition may
     stay open: `{"max_duration": "4h"}`, a Go duration (`90m`, `4h`, `24h`;
     a day is `24h`, there is no `d`), above zero and at most a year, one per
     transition, never into a terminal status. The clock runs from entering the
     status, wall time, through every claim and release. It never blocks the
     transition itself and never moves the task: once overdue, new claims by
     agents and runners fail with `stage_time_exceeded`, a live lease keeps
     working. Only a human lifts it, with `waived` on the entry transition,
     until the stage ends.
3. **Dry run**: `clab import --dir rules/ --dry-run`. In the report roles
   are `unchanged | new_version` (only the roles you edited are
   `new_version`), cycles `draft_created`, and
   `workflow_validations[].valid` must be `true`. Show the report to the
   human.
4. **Apply**: `clab import --dir rules/`, then
   `clab workflow activate <draft id>` for every new cycle draft.
   `clab workflow list` shows the active versions. Activation declares the
   cycle's task types in the project.
5. **Verify**: `clab role show <key>`, `clab workflow list`. Tasks started
   on the previous cycle version finish on it — that is expected; backlog
   tasks nobody has claimed move to the new version — the activation report
   lists both.

A single role is easier to change without the whole directory: export, edit
the files of `rules/roles/<key>/`, then

```bash
clab role put --key <key> --dir rules/roles/<key> --changelog "what changed"
```

The version is built whole from `role.json` and the files: a field missing
there is missing in the new version. A change to any document, to their
set, order or type, creates version N+1 of the whole role; documents you did
not touch keep their content. No change — no new version. A role of one
document can also be put as `clab role put --key <key> --file main.md …`;
that makes the version a single `main.md`, so do not use it on a role with
several documents.

A bundle written as one JSON file (`clab export > bundle.json`,
`clab import --file bundle.json`) still works and suits carrying the whole
bundle between projects; for editing roles use the directory.

## Do not

Do not edit an active cycle or a role version "in place" through the
database. Do not hard-code a role for a particular status anywhere. Do not
delete roles that statuses of active cycles refer to. Do not change rules
silently: a cycle change is a change of the team's agreements. Do not put
role documents into `.claude/skills` or `.claude/agents`.
