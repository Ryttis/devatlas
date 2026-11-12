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
scheduler, automatic retry of operational failures, push, merge, or deployment.

The optional [agent status connector](../connector/README.md) reads the
runner's saved states for an OAuth-authenticated MCP client. It runs separately
from both the runner and the inventory server.

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

## Four-role pilot

On the runner host, from a trusted DevAtlas checkout:

```sh
python3 scripts/agent-workflow.py plan /absolute/path/task.md BASE_COMMIT_SHA --max-corrections 1
# Read the printed workflow directory's plan.json before approving.
python3 scripts/agent-workflow.py run /absolute/path/to/task-ID --approve PLAN_SHA256
```

The Architect uses a fresh read-only Codex run. The plan snapshots the request,
base, branch, roles, correction limit and one or two slices with observable CLI checks. The
approval command binds execution to the SHA-256 printed for that exact plan.
Running it under the server account records approval; do not copy the command
into unattended scripts before reviewing the plan. Planning alone cannot code.

The workflow uses a private clone with one local task branch and no remote.
Fable implements each slice, the runner performs Go gates and commits, and a
fresh Astra session reviews the exact commit with the check log available.
A `changes_requested` verdict becomes `FIX_REQUIRED`: Fable receives the
findings and the rejected candidate's files, and the replacement is a single
commit on the same approved base. The original candidate and review remain in
the audit trail; a private Git ref keeps the rejected SHA reachable. The
default is one correction per slice (configurable from zero to two at
planning); at the limit the workflow stops at `PAUSED_FOR_HUMAN`. Plans saved
before the limit was recorded allow zero corrections.
After review passes, the runner runs the approved functional checks against
the built CLI in a temporary registry environment. The Tester uses a separate
read-only Codex session to evaluate that evidence. Both checks and Tester must
pass on the reviewed SHA. A fresh Architect session then receives both verdicts
and records its acknowledgment before the next slice starts from that SHA.
Check failures, malformed verdicts and interruptions stop the pilot for
inspection; there is no automatic resume.

The pilot only accepts `version`, `help`, and `list` scenarios with exit
status zero and a required output substring. Choose assertions that demonstrate
the requested behavior. Use only trusted code and tasks: checks execute the
compiled program under the server account. This is not a general test runner
for arbitrary projects, shell commands or production environments.

Inspect `workflow.json` for overall progress, candidate attempts and verdicts. Individual
slice runs remain in `agent-runs`, with `functional-checks.json` and
`tester.json` alongside the review. The existing MCP connector reports the
code-review verdict from `state.json`; that verdict alone does not mean the
workflow passed. No automatic push, merge or deployment is provided.
