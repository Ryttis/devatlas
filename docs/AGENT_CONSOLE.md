# Read-only execution console

The Go service serves saved execution history at `/` and `/runs`. Open a row
to see `/runs/{id}`. The existing Git project inventory moved to `/projects`;
`/health` and `/api/projects` are unchanged. The separate MCP connector and
OAuth routes are unchanged. No execution, approval, merge or deployment can
be initiated through this UI.

## Data and evidence

`serve -state-dir /absolute/path` selects the DevAtlas state root, independently
of the project registry's `-config`. The default is `$XDG_STATE_HOME/devatlas`
or `$HOME/.local/state/devatlas`. Use the same root and OS user as the runner.
The console reads these known files afresh on every page request:

- `agent-runs/run-*/state.json`: identity, stage, timestamp, base/head and failed stage.
- `agent-runs/run-*/review.json`: terminal review verdict, summary and findings.
- `agent-runs/run-*/tester.json`: tester verdict and summary, when available.
- `workflows/task-*/workflow.json`: status, timestamp, candidate and linked attempts.
  Older records with only completed slices and a current run are also supported.

The UI never reads raw logs, prompts, environment, checkout files or arbitrary
paths. Go checks are marked passed only where the runner state establishes
completion of verification: reviewing, terminal review, or failure in reviewing.
A reviewer pass is not a deployment or a workflow pass. A tester summary is
the tester's recorded opinion; individual functional check logs are not served.
Nonterminal states are explicitly historical observations, not live process checks.
Task descriptions and architect handoff prose are not included in this slice.

Records sort newest first, with deterministic ID ordering for tied timestamps.
There are 50 rows per page and optional run/workflow filters. Counts cover all
valid records matching the filter, not just the current page. Workflows and
their child runs count as separate records. Missing state directories are empty;
unreadable, invalid or oversized records produce a visible incomplete-list warning.
Each family is limited to 2000 directory entries; exceeding that limit omits
that family with a warning instead of presenting a misleading latest subset.
Each JSON file is limited to 64 KiB; reports have at most 50 findings and each
displayed prose field is limited to 4000 Unicode characters.

Reviewer and tester reports are validated and projected separately, mirroring
the producer contracts. `review.json` must be an object with exactly `verdict`,
`summary` and `findings`; the verdict must equal the terminal run stage, and
findings must be an array of non-blank strings that is non-empty exactly for
`changes_requested`. `tester.json` must be an object with exactly `verdict`
(`PASS`, `FAIL` or `BLOCKED`) and `summary`; tester findings are never shown.
Unknown fields, wrong types, `null` values, blank prose and trailing data make
that report unavailable with a generic message, without affecting the other.

The operator-selected state path and its ancestors are trusted. On Linux/WSL,
all relative components are opened through directory descriptors with no-follow;
record files must be regular, singly linked files. Symlinks, hard links, FIFOs
and device files are rejected. Other operating systems report history unavailable.
The state writer is trusted: JSON is schema-checked, not cryptographically signed.
Separate state/report reads are not an atomic snapshot; an inconsistent review
is suppressed and can be retried with Refresh after the writer finishes.

HTML is escaped. Common credential formats, URIs of any scheme (masked whole,
including userinfo), `user:password@host` forms and user home paths in report
prose are masked as defense in depth; this cannot detect every arbitrary secret.
Console responses disable caching and apply a restrictive CSP. Keep the entire
console behind owner authentication when proxying outside loopback, including
detail routes. Do not expose this UI as a public report viewer.

## Existing server rollout

The reviewed Git-based procedure, the `devatlas-web.service` unit and rollback
are in [DEPLOYMENT.md](DEPLOYMENT.md); the notes below describe the same instance.

Build and test in the source checkout after applying the patch:

```sh
go build -trimpath -o bin/devatlas-console ./cmd/devatlas
go vet ./...
go test ./...
CGO_ENABLED=1 go test -race ./...
```

Inspect the existing service unit and executable before replacing it. Preserve
its user, project registry path and loopback port. For Rytis's instance the
runner state root is `/home/rytis/.local/state/devatlas` and Nginx already proxies
the protected root to `127.0.0.1:18642`. The corresponding process arguments are:

```sh
bin/devatlas-console serve -port 18642 \
  -state-dir /home/rytis/.local/state/devatlas
```

Do not run that command alongside the existing listener; update its confirmed
service definition and restart only after deployment approval. Retain the old
binary and service arguments for rollback. No Nginx/OAuth changes are needed
when the listener and existing authenticated proxy remain unchanged.

After rollout verify authenticated `/`, a known `/runs/run-…` detail, `/projects`
and `/health`; an unauthenticated request to console routes must be challenged.
Check that a known reviewer finding appears and no raw log route is exposed.
The web console does not start an agent and does not itself fix the repository.

## Validation for this slice

Validated on Linux with Go 1.27.3: gofmt, build, vet, all Go tests and all Go
race tests passed. HTTP tests exercise empty/list/detail/filter/pagination flows,
HTML escaping, credential masking, unknown raw-field exclusion, read-only methods,
and unchanged project routes. Reader tests cover state/report disagreement,
workflow links, symlinks, hard links, FIFOs, malformed and oversized files,
invalid IDs and bounded listings. These use fixtures, not the production server.

Visual browser verification remains uncompleted: the available Chromium launch
failed with `socket() failed: Operation not permitted` in this execution
sandbox. The first Playwright attempt also lacked its bundled browser; selecting
system Chromium reached that sandbox restriction. Mobile styling is implemented
but has not been visually verified. No production rollout has been performed.
