---
name: clab-rules
description: Change the roles or the cycle of a Cadence Lab project — a role's documents, the artifact contract, statuses, transitions and gates — through a rules directory (export, edit, import) and draft activation. Use when asked to adjust a role, add a gate or a stage, or create a new role.
---

# Roles and cycle of a project

The request is the text this skill was invoked with. `clab` finds the
project itself — `--project`, then `SDLC_PROJECT`, then the binding line in
`AGENTS.md` or `CLAUDE.md`, then the `git remote` of the current directory;
where it finds none, bind the directory (`clab setup --project <KEY>`) or
pass `--project <KEY>` to the commands that take a project.

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
  role. They are not skills or agents of your agent and never go into the
  skills or agents folder of the kit;
- **project documents** — `documents` at the top of the bundle (guidelines,
  architecture, ADRs): read by every role.

A rule shared by several roles (git, the delivery section, a report
format) can live once in the project's **instruction library**: an
instruction has a key (`git`), immutable versions, and a role takes it as
the document `<key>.md` pinned to one version. A new instruction version
changes no role: roles move to it only through "update in roles"
(`clab instruction apply`), which creates a new version of each chosen role.

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
     byte for byte;
   - `rules/instructions/<key>/instruction.json` and `v<N>.md` — the
     instruction library: the instruction's fields and `versions`
     (`{version, kind, changelog}`), and the content of each exported version
     (those pinned by roles and the latest). A role document pinned to the
     library has `"instruction": "<key>"` in its `role.json` line; its
     `<key>.md` file stays next to the others.

   Import applies everything in the directory: remove the repositories and
   project documents from `bundle.json` unless you meant to change them.
2. **Edit the files.**
   - A role document: edit `<name>.md`. Add one: put the file next to the
     others and add its `{name, kind}` line to `documents` of `role.json` at
     the place where it should be delivered. Remove one: delete both the file
     and the line. The order of lines is the order of delivery. A file
     without a line or a line without a file is refused before anything is
     sent.
   - A pinned document (`"instruction": "<key>"`) must keep the exact text
     of one version of that instruction, otherwise import is refused. To
     change the text for every role, change the instruction (see below); to
     change it in one role only, drop `"instruction"` from its line and edit
     the file.
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
     `repo_ref_required` wants a repository reference on the task, e.g.
     `{"ref_type":"pull_request","min_count":1,"per_repository":true,"states":["open","merged"]}`:
     `ref_type` is `pull_request`, `branch` or `commit` (default
     `pull_request`); `min_count` is how many references are needed (at
     least 1); with `per_repository` true that many in every repository of
     the task's scope, otherwise in the task as a whole. `states` limits the
     references that count to those in the listed states of a pull request:
     a non-empty list of `open`, `merged`, `closed`, `declined`. Without
     `states` any state counts. `require_open: true` is the older spelling of
     `states: ["open"]`: it refuses a pull request that is closed, merged or
     declined. Give a gate either `require_open` or `states`, never both: the
     draft is refused with `repo_ref_states_invalid`. A reference whose state
     has not been synchronized yet (no GitHub access) counts when `open` is
     among the `states` (or without `states`), and never counts when it is
     not. Mind the way back: if a task can return to a status before the
     merge from a status after it (a failed rollout, say), the pull request
     is already merged then, and a gate that counts only open pull requests
     holds the task until a human waives it. Put `["open","merged"]` on the
     pull request gates along that path. The price: such a gate also lets a
     pull request merged ahead of time through, so keep the human approval
     before the merge.
   - Which of a role's documents a status delivers is the cycle's choice,
     not the role's: a role knows nothing about statuses. In a status of
     `bundle.json`, `role_documents` lists the document names of the
     executing role (`role`) for this status, and `reviewer_documents`
     maps a reviewer role key to its names: `{"<reviewer key>": ["…"]}`.
     No field — the stage gets all the role's documents; there is no empty
     list. The order of names means nothing: documents come in the role
     version's order. A name must not be empty or repeated, at most 50 per
     role; `role_documents` without `role` or a key of `reviewer_documents`
     missing from `reviewers` is refused. A name the role's latest version
     lacks is not refused: validation and activation report it as the
     `role_document_missing` warning, and the stage then skips it (or gets
     all the documents if none of the names exists). Rename or remove a
     role document — check the selections that name it. The same selection
     is set in the interface in the status dialog of a cycle draft.
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

The instruction library has its own commands:

```bash
clab instruction list                       # key, latest version, pinned, lagging
clab instruction show <key> [--version N]   # versions, where used, the text
clab instruction put --key <key> --file rule.md --changelog "what changed"
clab instruction apply <key> --role <role> [--role <role>] [--version N]
```

`put` creates the instruction (version 1) or its next version; the same
text and type as the latest version is refused. `apply` gives every listed
role a new version where `<key>.md` is pinned to the chosen version (the
latest by default), everything else as in the role's latest version; a role
already on that version is left unchanged, a role without `<key>.md` or
archived refuses the whole call. Adding the instruction to a role that has
no `<key>.md` yet is a role edit: add the line with `"instruction"` and the
file, or `{"instruction": "<key>", "instruction_version": N}` through
`sdlc_put_role`. These commands exist only in builds of `clab` newer than
v0.20.0; an older server answers that it does not know the library.

A bundle written as one JSON file (`clab export > bundle.json`,
`clab import --file bundle.json`) still works and suits carrying the whole
bundle between projects; for editing roles use the directory.
`export --dir`, `import --dir` and `role put --dir` exist only in newer
builds of `clab` (after v0.20.0); with `clab` v0.20.0 the bundle goes as
one JSON file: `clab export > bundle.json`, edit `roles[]` and `workflows[]`
in it, `clab import --file bundle.json --dry-run`, then without
`--dry-run`. `clab --version` tells which build you have.

## Do not

Do not edit an active cycle or a role version "in place" through the
database. Do not hard-code a role for a particular status anywhere. Do not
delete roles that statuses of active cycles refer to. Do not change rules
silently: a cycle change is a change of the team's agreements. Do not put
role documents into the skills or agents folder of the agent's kit.
