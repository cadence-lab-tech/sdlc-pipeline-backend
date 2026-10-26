---
name: clab-rules
description: Change the roles or the cycle of a Cadence Lab project — a role instruction, the artifact contract, statuses, transitions and gates — through bundle export/import and draft activation. Use when asked to adjust a role, add a gate or a stage, or create a new role.
argument-hint: what to change
---

# Roles and cycle of a project

Request: `$ARGUMENTS`. The project comes from `git remote`; outside a
repository of the project — `SDLC_PROJECT=<KEY>`.

Roles and cycles are project data. They change by versions, never in
place: a role gets a new version of its instruction, a cycle gets a draft
that is validated and activated. Changing them needs the `maintainer` level
in the project; a stage executor's task token cannot do it at all.

Rules are the team's agreements. Change them only when the human asked for
it, and show them what exactly changes before applying.

## Steps

1. **Export what is active**: `clab export > bundle.json` (into a temporary
   directory). Roles are in `roles[]`, cycles in `workflows[]`. Remove the
   repositories and documents from the bundle before import unless you meant
   to change them — import applies everything in the file.
2. **Edit the JSON**: the instruction (`instructions`), the contract
   (`output_contract[]` with `kind`, `title`, `format`, `required`), inputs
   (`input_kinds`), statuses (`role` — the key of the executing role),
   transitions and gates. Gate types: `artifacts_required`,
   `schema_validation`, `checklist`, `human_approval`, `agent_review`,
   `external_check`, `repo_ref_required`, `drift_clear`, `repo_scope`,
   `dependencies_closed`, `attempts_limit`. The role in `agent_review` cannot
   be the role executing the status — validation refuses it.
   `attempts_limit` caps how many times a task may enter the transition's
   target status: `{"max_attempts": 3}`, an integer of at least 1, never on a
   transition into a terminal status. It goes on the transition itself,
   usually a `reject`; roles carry no such setting. Only a human lifts it, with
   `waived`, one attempt per decision. `agent_review` takes an optional
   `max_attempts` too: after that many failed reviews on one stage run no new
   review starts, the gate fails with "N of N reviews" and only a human lifts
   it with `waived`; a new submission clears that waiver.
3. **Dry run**: `clab import --file bundle.json --dry-run`. In the report
   roles are `unchanged | new_version`, cycles `draft_created`, and
   `workflow_validations[].valid` must be `true`. Show the report to the
   human.
4. **Apply**: `clab import --file bundle.json`, then
   `clab workflow activate <draft id>` for every new cycle draft.
   `clab workflow list` shows the active versions. Activation declares the
   cycle's task types in the project.
5. **Verify**: `clab role show <key>`, `clab workflow list`. Tasks started
   on the previous cycle version finish on it — that is expected; backlog
   tasks nobody has claimed move to the new version — the activation report
   lists both.

A single role is easier to change without a bundle:

```bash
clab role put --key <key> --name "Name" --file instructions.md \
  [--contract contract.json] [--inputs kind1,kind2] --changelog "what changed"
```

A new version is created only when the instruction or the contract changed.

## Do not

Do not edit an active cycle or a role version "in place" through the
database. Do not hard-code a role for a particular status anywhere. Do not
delete roles that statuses of active cycles refer to. Do not change rules
silently: a cycle change is a change of the team's agreements.
