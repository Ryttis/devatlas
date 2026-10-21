# Repository instructions

## Scope and source of truth

DevAtlas is a local Go service for Windows 11 + WSL2/Linux. The current
implementation is slice 03: CLI registration, YAML persistence, loopback HTTP
UI/API, and read-only Git observations. Docker, port and HTTP collectors,
SQLite history, context export, and MCP are planned, not implemented.
Preserve the read-only collector boundaries, secret exclusion, bounded
execution, and explicit unknown/error states described in the architecture.

Repository Markdown is the source of truth for scope, design, and agent
instructions. Read [README.md](README.md),
[implementation_brief.md](implementation_brief.md), and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) before implementation. Distinguish
planned capabilities from current behavior; verify runtime claims with checks.
Keep relevant Markdown aligned with approved changes. Report conflicts or
missing decisions rather than silently expanding scope.

Each implementation agent may have only one active, explicitly scoped slice.
Finish or hand off that slice before starting another. Use the existing
isolated cloud checkout; create a worktree only when explicitly requested.
Inspect branch, revision, and working-tree status before editing, and preserve
unrelated user changes. See [docs/AGENT_WORKFLOW.md](docs/AGENT_WORKFLOW.md) for
the proposed implementation and review process.

## Verification and evidence

Use a supported Go release (minimum 1.22), Git >=2.31, and Linux/WSL.
Race tests require cgo and a C compiler. Docker is unnecessary for slice 03.
Run the documented checks from the repository root:

```sh
files=$(gofmt -l .)
if [ -n "$files" ]; then
  printf '%s\n' "$files"
  exit 1
fi
go build -trimpath -o bin/devatlas ./cmd/devatlas
go vet ./...
go test ./...
CGO_ENABLED=1 go test -race ./...
```

Formatting validation must fail on any listed file. Do not weaken tests or
change application behavior to obtain passing checks. Report existing failures
separately from regressions. For behavior changes, exercise the affected flow
and record expected versus observed results; use temporary registry files
outside the checkout for smoke tests.

Completion claims require evidence: branch/commit, changed files, commands,
outcomes (including failed, skipped, or unrun checks), and remaining limitations.
Report blockers with the failing operation, relevant error, attempted remedies,
and the specific outside action needed. Do not call an unrun check passed or
claim a planned integration works.

## Human control

Merges and deployment each require explicit human approval for that action.
A passing CI run or agent review does not grant approval. Do not force-push.
Commit or push only within the task's authorization; do not start another
feature as part of completing the current slice.
