# DevAtlas architecture

Status: slice 01 (application bootstrap). This document describes what exists
now and the boundaries later slices are expected to respect. Anything listed
under "planned" is not implemented.

## Purpose

DevAtlas is a local, read-only registry of the projects developed on one
workstation: their checkouts and worktrees, declared and observed ports,
container status, configured HTTP checks, documents, and AI context. It runs
as a single Go process inside WSL2, binds only to loopback, and uses no LLM or
cloud service for inventory, status collection, or diagnostics.

## Current structure

```text
cmd/devatlas/main.go        process entry; delegates to internal/cli
internal/cli                 argument parsing, exit codes, SIGINT/SIGTERM handling
internal/httpserver          loopback listener, timeouts, graceful shutdown,
                             GET /health, GET / (html/template, embedded)
internal/version             version string, VCS commit from Go build info
```

Dependency direction is strictly inward:

```text
cmd/devatlas -> internal/cli -> internal/httpserver -> internal/version
```

Only the standard library is used. The `internal/` prefix keeps every package
private to this module.

### Design decisions already in force

- **Loopback only.** `httpserver.Config.Validate` accepts literal loopback IPs
  only (`127.0.0.0/8`, `::1`). Host names, including `localhost`, are rejected
  so that resolver behaviour can never widen exposure. There is no flag to
  bind elsewhere.
- **Configurable port, explicit conflicts.** A bind failure caused by an
  occupied port is surfaced as `httpserver.AddrInUseError` with a clear
  message and exit code 1. Identifying the other process is left to the
  port-conflict slice.
- **Timeouts and graceful shutdown.** The `http.Server` sets read-header,
  read, write, and idle timeouts. `Serve` stops on context cancellation,
  which the CLI ties to SIGINT/SIGTERM, and waits up to the shutdown timeout
  for in-flight requests.
- **Listen and Serve are separate steps.** `Listen` opens the socket and
  returns the real address (useful with port 0 and in tests); `Serve` runs
  until cancelled. This keeps the server testable without signals.
- **Honest UI.** The home page renders `httpserver.CurrentScope`, a list of
  implemented and deferred capabilities, so the UI never implies features
  that do not exist. Update it in every slice.
- **Facts carry time.** `/health` already includes `checked_at`; every future
  observation must carry its source and collection time, per the brief.

## Intended package layout and adapter boundaries (planned)

The brief separates a logical Project from its Checkouts and Runtimes, and
requires every observation to carry source, status, and collection time.
The planned layout mirrors that:

```text
internal/registry     domain model: Project, Checkout, Runtime, Service,
                      PortBinding, Observation, Finding; status vocabulary
                      (running, stopped, unhealthy, unknown, error) and
                      staleness rules. No I/O.
internal/manifest     parse and validate project-registry.yaml and the
                      Git-ignored local configuration; path resolution.
internal/store        SQLite index and observation history. Rebuildable from
                      registrations, manifests, and fresh observations;
                      never the source of truth for documents.
internal/collect      collector interfaces plus one adapter per source:
  collect/git         branch, HEAD, dirty/untracked counts, worktrees,
                      remote identification with credentials stripped.
  collect/docker      Compose project metadata, container state, health,
                      restart count, published host ports. Reports
                      `error`/`unknown` with the last successful time when
                      the daemon is unreachable.
  collect/ports       locally listening sockets (WSL scope only, labelled as
                      such) and conflict evaluation by host, protocol, port.
  collect/httpcheck   explicitly configured GET checks with timeout and
                      accepted status codes; never logs headers or secrets.
internal/rules        diagnostic rules producing Findings with evidence.
internal/context      concise project context export for coding agents.
internal/httpserver   JSON API and HTML UI over the registry (existing).
internal/cli          commands: register, remove, scan, refresh, context, ...
```

Boundary rules for adapters:

1. Collectors return `Observation` values with `source`, `status`,
   `collected_at`, and the last successful observation time. They never
   mutate the environment. The first product version has no start/stop,
   migration, commit, push, merge, or deploy capabilities.
2. External commands, if any, run as a fixed program with separate arguments,
   a timeout, and no shell. Manifest content is description, never an
   instruction to execute.
3. Secrets (`.env` values, DSNs, tokens, credentials embedded in remote URLs)
   are stripped at the adapter boundary so they cannot reach the store, the
   API, the UI, logs, or context exports.
4. Declared ports (from Compose files) and observed ports (from the Docker
   API or the OS) are distinct facts and are never merged into one value.
5. A collector failure yields `error` or `unknown`, never a fabricated
   `stopped` or `healthy`.

## Verification approach

Unit tests cover argument errors, loopback enforcement, health output, the
home page, occupied-port reporting, and graceful shutdown with an in-flight
request. Later slices add tests for port-conflict logic, checkout identity,
status and staleness modelling, and secret exclusion, using controlled
fixtures for Git and Docker, plus at least one flow verified against a real
local project.
