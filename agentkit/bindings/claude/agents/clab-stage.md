---
tools: Bash, Read, Edit, Write, Grep, Glob, Skill, TodoWrite
model: inherit
hooks:
  PreToolUse:
    - matcher: Bash
      hooks:
        - type: command
          command: |
            # clab only with the task-token directory.
            # Looks for a call as a command: clab under any name clab* — bare
            # clab, clab.exe, the clab of the package (clab-<os>-<arch>), a clab
            # built under its own name (clab-v0.17.0, clab_dev) and clab-runner
            # too, one rule without exceptions; go run of cmd/clab* (a local path
            # or a module path with @version); and the scripts of the package
            # (install.sh, upgrade.sh, backup.sh, restore.sh, get-cadence.sh) —
            # by any path, at the start, after ; & | ( ` {, environment variables
            # and env/command/exec/time/nohup/sudo/sh/bash with flags. The scripts
            # run clab and get the directory from the environment. A name as an
            # argument (cat install.sh, ls /tmp/clab-v99/, go build -o
            # bin/clab-v0.17.0 ./cmd/clab, go test ./cmd/clab/...), a path or a
            # branch like clab/... does not count.
            # Known and kept: sh -n install.sh is refused too, and so is | inside
            # quotes (grep 'a|clab x'): a safety net, not a shell parser. Not
            # caught: a clab built under a name that does not start with clab
            # (go build -o /tmp/x), go run . inside cmd/clab, go run of a file or
            # of ./cmd/clab/. (go run ./cmd/clab/main.go), go run with ; & | or a
            # tab inside a quoted flag before the package path.
            # Newline and tab arrive from JSON as \n and \t.
            # Claude Code runs the hook only when the project folder is trusted;
            # /clab-run checks that it is alive with a probe before the first run.
            in=$(cat)
            sep='(^|"command": *"|[;&|(`{]|\\[nt])'
            pre='[[:space:]]*((env|command|exec|time|nohup|sudo|sh|bash)([[:space:]]+-[A-Za-z]+([[:space:]]+[A-Za-z_][A-Za-z0-9_]*)?)*[[:space:]]+|[A-Za-z_][A-Za-z0-9_]*=[^[:space:];&|]*[[:space:]]+)*'
            bin='([^[:space:];&|"(]*/)?(clab([-_.][A-Za-z0-9._-]*)?|(install|upgrade|backup|restore|get-cadence)\.sh)([[:space:];&|)"]|\\[nt]|$)'
            run='go[[:space:]]+run[[:space:]](([^;&|"\\]|\\")*[[:space:]/])?cmd/clab([-_.@][A-Za-z0-9._@-]*)?/?([[:space:];&|)"]|\\[nt]|$)'
            printf '%s' "$in" | grep -Eq "$sep$pre($bin|$run)" || exit 0
            case "$in" in *CLAB_CONFIG_DIR=*) case "$in" in *"-u SDLC_TOKEN"*) exit 0 ;; esac ;; esac
            echo 'clab without the task-token directory (clab under any name clab*, go run of cmd/clab or a script of the package). Every such call needs the prefix from the prompt: env -u SDLC_TOKEN -u SDLC_API_URL CLAB_CONFIG_DIR=<dir> <command>' >&2
            exit 2
---

## Notes for Claude Code

- The orchestrator is `/clab-run`; it starts you as a sub-agent of type
  `clab-stage`, and the automatic check is the `PreToolUse` hook of this
  definition. Claude Code runs it only in a trusted project folder, so it
  is a safety net, not a boundary.
- The skills and agents folders of the kit are `.claude/skills` and
  `.claude/agents`: role documents never go there.
- The session's temporary directory is the one Claude Code names in the
  system prompt.
