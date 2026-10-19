# Security policy

This policy covers every public repository of Cadence Lab: the service
([sdlc-pipeline-backend](https://github.com/cadence-lab-tech/sdlc-pipeline-backend)),
the web interface
([sdlc-pipeline-frontend](https://github.com/cadence-lab-tech/sdlc-pipeline-frontend)),
the self-hosted package
([self-hosted](https://github.com/cadence-lab-tech/self-hosted)) and the
Homebrew tap and installer of the `clab` CLI
([homebrew-tap](https://github.com/cadence-lab-tech/homebrew-tap)). The same
file lies in each of them.

## Reporting a vulnerability

Report vulnerabilities privately through GitHub — **do not open a public
issue, pull request or discussion**. Use "Report a vulnerability" on the
Security tab of the repository where you found the problem:

- [sdlc-pipeline-backend](https://github.com/cadence-lab-tech/sdlc-pipeline-backend/security/advisories/new) — API, MCP server, worker, `clab` CLI, database migrations
- [sdlc-pipeline-frontend](https://github.com/cadence-lab-tech/sdlc-pipeline-frontend/security/advisories/new) — web interface
- [self-hosted](https://github.com/cadence-lab-tech/self-hosted/security/advisories/new) — compose stack, Caddyfile, install, upgrade, backup and restore scripts
- [homebrew-tap](https://github.com/cadence-lab-tech/homebrew-tap/security/advisories/new) — Homebrew formula and `install.sh` of the CLI

Not sure which repository it belongs to? Report it in any of them; moving it
to the right one is our job. The report is visible only to you and the
maintainers. GitHub private vulnerability reporting is the only channel: we
do not take reports by email or chat.

Please include:

- the affected component and version (`clab version`, `VERSION` of the
  installation, or the image tag);
- the deployment mode if it matters (`tls` or `proxy`, single sign-on,
  GitHub App);
- steps to reproduce or a proof of concept, and what an attacker gains;
- your own severity estimate, if you have one;
- whether and how you want to be credited.

Test against an installation of your own. Do not access data that is not
yours, and do not degrade instances run by others.

## Response times

| Step | Target |
|---|---|
| Acknowledgement of the report | 3 business days |
| Initial assessment with severity (CVSS 3.1 or 4.0) | 7 days |
| Fix released — critical | 7 days |
| Fix released — high | 30 days |
| Fix released — medium and low | the next planned release, at most 90 days |

Days count from the report. If a target slips, we tell you why and when to
expect the fix.

## Supported versions

Cadence Lab is in beta. The backend, the frontend, the self-hosted package
and the CLI share one version `vX.Y.Z`; releases are listed at
[self-hosted releases](https://github.com/cadence-lab-tech/self-hosted/releases).

| Version | Security fixes |
|---|---|
| The latest minor `0.x` (its latest patch) | Yes |
| Any older `0.x` | No — upgrade to the latest release |

A fix ships as a new release on top of the latest one; there are no
backports to older minors during the beta. To get it, upgrade the
installation (`./upgrade.sh vX.Y.Z` or `clab server upgrade`) and the CLI on
the machines of people and agents (`brew upgrade clab`). A support window for
older versions comes with the 1.0 release policy, and this table changes
then. The administrator guide covers upgrades and security notifications:
[docs/admin.md](https://github.com/cadence-lab-tech/self-hosted/blob/main/docs/admin.md#security).

## Disclosure and CVE

We follow coordinated disclosure:

1. We confirm the issue with you and agree on its severity.
2. The fix is prepared in a temporary private fork created from the GitHub
   Security Advisory.
3. We request a CVE identifier through the advisory; GitHub is the CVE
   Numbering Authority.
4. The advisory with the CVE is published together with the release that
   fixes it, and no later than 90 days after the report. You are credited in
   the advisory if you wish.

Please keep the details private until the advisory is published. If we do
not respond within the targets above, you may remind us in the same report.

## How you learn about fixes

- **GitHub Security Advisory** in the repository with the vulnerable code,
  with the CVE, affected and fixed versions.
- **Release notes** of the fixing release in
  [self-hosted releases](https://github.com/cadence-lab-tech/self-hosted/releases)
  have a "Security" section with a link to the advisory.
- **Notifications:** on
  [cadence-lab-tech/self-hosted](https://github.com/cadence-lab-tech/self-hosted)
  choose Watch → Custom → Releases and Security alerts. Every release,
  security fixes included, is published there.

## Out of scope

- There is no bug bounty program; we do not pay for reports.
- We do not hold security certifications.
- Vulnerabilities in third-party dependencies (Postgres, Caddy, Go and npm
  packages, base images) go to their projects. Tell us if one affects
  Cadence Lab in a way that needs a change or a release on our side.
- Findings that need control over the host, the `.env` or the Docker daemon
  of an installation, and volumetric denial of service.
