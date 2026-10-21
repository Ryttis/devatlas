# Cloud development pilot

Use the root [AGENTS.md](../AGENTS.md) for repository rules and verification.
Keep one active slice per implementation agent, with an agreed scope and
acceptance criteria recorded in repository Markdown. Inspect the starting
revision and working tree, then work on a task branch.

## Proposed model routing

- **Sol implements:** inspect the relevant code and Markdown, propose a bounded
  approach, implement the approved slice, and collect verification evidence.
- **Astra reviews at important checkpoints:** review the plan before changes to
  architecture, security boundaries, or data contracts; review the resulting
  diff and test evidence before a slice is handed to a human for merge. Request
  another review when a material design change invalidates the earlier review.
- **Escalate after two unsuccessful attempts to fix the same problem:** stop
  further attempts on that problem and provide Astra (or the human owner if
  Astra is unavailable) the reproduction, errors, two attempted fixes and
  outcomes, and the decision or access needed. Continue only independent work
  within the same slice while the blocker is unresolved. Do not weaken checks
  or broaden scope to bypass it.

The implementer addresses review findings and reruns affected checks. Handoff
includes the commit, changed files, check results, known failures, unresolved
findings, and limitations. Merge and deployment remain separate actions, each
requiring explicit human approval.

## What is actually configured

The GitHub Actions workflow in `.github/workflows/ci.yml` runs formatting
validation, build, vet, tests, and race tests on pushes and pull requests using
a supported Go release on Linux, with read-only token permissions. Formatting
differences fail CI; CI does not rewrite files.

Sol/Astra routing and escalation above are a proposed operating process, not
an automated model router. This change configures no agent scheduler, automatic
review requests, automatic escalation, merge, or deployment. A human or the
task orchestrator must arrange model sessions and checkpoint reviews. Branch
protection and required reviewer/check enforcement are GitHub settings and are
not configured by these files. Local passing checks do not prove that the
hosted workflow has run.
