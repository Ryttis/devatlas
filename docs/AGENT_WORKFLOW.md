# Agent development pilot

Use the root [AGENTS.md](../AGENTS.md) for repository rules and verification.
Keep one active slice per implementation agent, with an agreed scope and
acceptance criteria recorded in repository Markdown. Inspect the starting
revision and working tree, then work on a task branch.

## Saved roles

[Agent roles](../scripts/agent-roles.json) is the runner's versioned
configuration. It saves the model, instructions, and execution limits for:

- **Coder (Fable / Claude Code):** implement one approved slice in an isolated
  local checkout. The runner owns verification and the local commit.
- **Reviewer (Astra / Codex):** review the exact verified base-to-head diff
  in read-only mode and return a verdict, summary, and actionable findings.

The runner makes one coding attempt and one review. If a check fails or a
review requests changes, inspect the run and decide what to do next.

For changes to architecture, security boundaries, or data contracts, arrange
an additional plan review before implementation. Request another review when
a material design change invalidates an earlier review.
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

The command below runs the saved roles on demand. The DevAtlas HTTP service
remains read-only; it does not start agents or display runs. There is no
scheduler, automatic retry, push, merge, or deployment.

## Run one task

Use a trusted DevAtlas clone and a trusted task Markdown file. Write the scope,
acceptance criteria, and any permitted paths in the task. The runner requires
Linux/WSL, Python 3.9+, the repository's Go/Git/C toolchain, and current Claude
Code and Codex CLIs signed in to their respective accounts. The CLI checks
verify login type, not subscription billing; check Fable usage-credit settings
before running unattended. Tools and the Go tests still run under your host
account, so do not use untrusted revisions or task files.

From the checkout that contains the runner, run:

```sh
python3 scripts/agent-runner.py /absolute/path/task.md BASE_COMMIT_SHA
```

The base must already exist in this clone. The runner makes a separate local
clone, removes its `origin`, and checks out the selected commit. The original
working tree is left alone. It runs formatting, build, vet, tests, and race
tests before committing and starting the reviewer.

On `vaicaicio4`, to keep the job running after SSH disconnects:

```sh
export PATH="$HOME/.local/bin:$HOME/.local/opt/go1.27.3/bin:$PATH"
umask 077
log=$(mktemp "$HOME/devatlas-agent.XXXXXX.log")
nohup python3 scripts/agent-runner.py /absolute/path/task.md BASE_COMMIT_SHA \
  </dev/null >"$log" 2>&1 &
printf 'PID: %s\nLog: %s\n' "$!" "$log"
```

The command prints the run directory under
`${XDG_STATE_HOME:-$HOME/.local/state}/devatlas/agent-runs`. Its `state.json`
tracks progress and the base/head commits. `coder.json`, `checks.log`, and
`review.json` show the evidence. A completed review returns `0` for `pass` or
`2` for `changes_requested`; other run failures return `1` and identify the
failed stage. The commit stays in `checkout/`; `change.patch` can be applied
to the same base for human review. Treat run files as private project data.

After the run, review the changes and decide whether to push them. A local
pass does not prove hosted CI passed or grant permission to merge or deploy.

The runner's integration tests use local stand-ins for both agent CLIs and
the Go tools; they do not consume model usage:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -v
```
