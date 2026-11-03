# Contributing

This repository holds the Cadence Lab service: REST API, MCP server, workers and the `clab` CLI. It is part of
[Cadence Lab](https://github.com/cadence-lab-tech/self-hosted), which is
developed by its maintainers, mostly with AI agents working through the
product's own development cycle. The source is available under the
[FSL-1.1-ALv2](LICENSE) license. We do not use a CLA and we do not take
contributions without an agreement made first, as described below.

## Report a problem or propose a change: open an issue

Issues are open to everyone. Use the issue forms: a bug report or a feature
request. A good report has the version (`clab version`, the `VERSION` of the
installation or the image tag), the deployment mode, steps to reproduce and
what you expected.

- **Security problems are never public issues.** Follow
  [SECURITY.md](SECURITY.md) and report them privately through GitHub.
- Questions about installing or running Cadence Lab are fine as issues too.
  Read [docs/admin.md](https://github.com/cadence-lab-tech/self-hosted/blob/main/docs/admin.md)
  first.

A maintainer reads every issue. We answer, ask for details, or turn it into a
task of our own backlog and tell you in the issue. An issue that is not
reproducible gets an answer with what we found and possible workarounds.

## Pull requests: only for an accepted issue

We take a pull request from outside the team only if all of this holds:

1. It is tied to an issue that a maintainer has labelled **`accepted`**. The
   label means: "we want this change, the approach is agreed." Open the issue
   first and wait for the label; do not start with the code.
2. The pull request says `Closes #<issue>` in its description, and the issue
   links back to the pull request.
3. It is small and does one thing the issue describes. Refactoring, formatting
   or dependency bumps that nobody asked for are closed.
4. Its checks are green. Workflows of a fork run only after a maintainer
   approves them, every time.
5. You license your contribution under the license of this repository and
   confirm that you have the right to do so (the box in the pull request
   template).

A pull request that does not meet these points is closed with a link to this
page. Do not take it personally: it saves both of us the work. Only the
maintainers merge pull requests; merging is by squash, and the history of
`main` is linear.

Maintainers and invited members of the organization work in branches of this
repository and follow the same rules: a pull request, green checks and a
squash merge.

## Behaviour

Be polite and stay on topic. We close threads and block accounts that are
abusive, off topic or spam, without discussion.
