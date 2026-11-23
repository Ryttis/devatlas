# DevAtlas architecture

Status: slice 04 (technology status dashboard, first stage). This document
describes what exists now and the boundaries later slices are expected to
respect. Anything listed under "planned" is not implemented.

## Purpose

DevAtlas is a local, read-only registry of the projects developed on one
workstation: their checkouts and worktrees, declared and observed ports,
container status, configured HTTP checks, documents, and AI context. It runs
as a single Go process inside WSL2 and binds only to loopback. Inventory, Git
observation, and every status decision work without an LLM. The technology
status dashboard queries four public data services and, optionally, one AI
model that explains rule findings; see "Technology status" below for exactly
what leaves the machine.

## Current structure

```text
cmd/devatlas/main.go        process entry; delegates to internal/cli
internal/cli                 argument parsing, exit codes, SIGINT/SIGTERM handling,
                             add / list / remove / serve / version commands
internal/registry            domain model: Project, Checkout, Registry; identity
                             rules, validation, canonical paths, stable IDs. No I/O
                             beyond resolving a directory path.
internal/registryfile        persistence: versioned YAML file under the user config
                             directory, atomic writes, flock-based updates, path
                             resolution (-config flag, DEVATLAS_CONFIG, default)
internal/collect/git         first collector adapter: runs the local git command
                             read-only with deadlines, output caps, process-group
                             isolation, and a sanitised remote model; returns an
                             Observation per path with status, source, and time
internal/collect/stack       second collector adapter: reads Composer and npm
                             manifests and lock files, Dockerfiles, and Compose
                             files as data; no execution, no symlinks, no .env,
                             bounded sizes; keeps declared and locked versions apart
internal/sources             OSV, endoflife.date, npm, Packagist clients with
                             timeouts, bounded retries, a request budget, and the
                             PublicPackage type that gates what may be named
internal/assess              pure status rules, AI adapter with answer
                             validation, atomic JSON record store, engine, and
                             the single-worker bounded queue
internal/config              .env parsing into a structure; Secret type
internal/atomicfile          temp file + rename for assessment records
internal/httpserver          loopback listener, timeouts, graceful shutdown,
                             GET /health, GET /api/projects, GET / and
                             GET /projects/{id} (html/template), the two guarded
                             POST endpoints that queue assessments;
                             ObserveProjects joins registrations with observations
internal/version             version string, VCS commit from Go build info
```

Dependency direction is strictly inward:

```text
cmd/devatlas -> cli -> httpserver -> registryfile -> registry -> (stdlib)
                  \-> registryfile      \-> registry      \-> yaml
                  \-> registry          \-> collect/git
                  \-> collect/git       \-> version
```

`httpserver` depends on `registryfile` only through the `RegistrySource`
interface (`Load`, `Path`) and on the Git collector only through
`GitObserver` (`CollectAll`), so tests substitute fakes for both.
`collect/git` depends on nothing else in this module. The one external
module is `go.yaml.in/yaml/v3`, used solely by `registryfile`. The
`internal/` prefix keeps every package private to this module.

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
- **Honest UI.** The instance details page renders `httpserver.CurrentScope`, a list of
  implemented and deferred capabilities, so the UI never implies features
  that do not exist. Update it in every slice.
- **Facts carry time.** `/health` already includes `checked_at`; every future
  observation must carry its source and collection time, per the brief.
- **Project and checkout are separate identities.** A project is an explicit,
  user-chosen ID with a display name. A checkout is a canonical directory
  (absolute, symlinks resolved) with a stable ID derived from that path. One
  project may have many checkouts; one directory belongs to one checkout.
  Nothing is inferred from directory names or Git remotes.
- **One shared read model.** `registry.Registry.Views()` feeds
  `httpserver.BuildProjectsResponse`, which the CLI (`list`, `list -json`),
  the JSON API, and the HTML template all render. There is exactly one place
  that decides what a checkout looks like to a user.
- **Status is never invented.** Until collectors exist, every checkout's
  runtime status is `unknown` with an explicit "not collected" reason. The
  only observed fact is a directory stat, stamped with its check time.
  Absence and a failed observation are distinct; neither removes a registration.
- **Registry writes are serialised and atomic.** `registryfile.Store.Update`
  takes an advisory lock, loads, applies the change, validates, writes a temp
  file, and renames it over the original. A malformed file fails the
  operation and is never rewritten. The persistent sidecar lock survives
  registry replacement and waits at most 10 seconds. Parent-directory aliases
  are resolved; file symlinks and hard links are rejected. Writes are supported
  only on Linux/WSL with local filesystem locking and rename semantics.
- **HTTP changes nothing in a project.** Registrations change through the
  CLI, which is also where the lock and validation live. The only non-GET
  endpoints queue an assessment; they require a `Host` header naming this
  listener and same-origin `Origin` / `Sec-Fetch-Site` headers when a
  browser sends them, because a loopback port is reachable from any page in
  the user's browser.
- **Observations are per request and self-describing.** `ObserveProjects`
  collects Git observations for the checkouts of one response under the
  request context, so a disconnecting client cancels running commands.
  Each observation carries `status`, `source`, `collected_at`, and a safe
  `reason`; the response says `collection.mode = current_request`. Nothing
  is cached, stored, or refreshed in the background yet.
- **Git success does not imply runtime health.** The Git observation and the
  runtime status are separate fields; runtime stays `unknown` with a "not
  collected" reason until Docker, port, and HTTP collectors exist.
- **The Git adapter is strictly read-only and bounded.** It runs a fixed
  program with separate arguments (no shell), disables prompts, optional
  locks, the file-system monitor, and hook paths, strips repository
  redirecting environment variables, bounds time, output, and concurrency,
  kills the process group on cancellation, and sanitises remote URLs before
  they enter the observation. Failure reasons shown to users are classified
  text; raw stderr goes to the log only. It never fetches, checks out,
  stages, commits, or edits configuration, and it never groups projects by
  remote.

## Technology status

Data flow of one assessment, started by a button and run by the single
worker:

```text
registered checkout
  -> collect/git        HEAD, branch, uncommitted changes (ties the result to a state)
  -> collect/stack      facts + gaps + hash of the files read
  -> sources            endoflife.date per recognised product with a concrete version,
                        OSV for public locked packages, npm/Packagist for public direct ones
  -> assess.Evaluate    pure function (facts, external data, today) -> findings, statuses
  -> assess AI adapter  optional texts attached to existing finding IDs, validated
  -> assess.Store       atomic JSON record: last successful result + last attempt
  -> httpserver         table and card render the stored record; nothing is evaluated on render
```

Decisions in force:

- **Rules decide, AI explains.** `Evaluate` produces every finding, level,
  source link, and recommended action. The AI answer is a map from existing
  finding IDs to two texts; it has no field for a status, a level, a source,
  or a new finding, and texts that fail validation are dropped. A failed or
  rejected AI call is recorded on the result and changes nothing else.
- **Green needs complete coverage.** Every part of the stated scope that
  could not be checked is a gap. No findings with a gap is "Nepatikrinta";
  findings with a gap keep their colour and gain "Dalinė patikra".
- **Nothing is guessed.** A declaration that does not determine a version
  yields no version. Declared and locked values are separate fields with a
  recorded basis, and neither is called a running version.
- **What may leave the machine is a type, not a convention.** Query
  functions take `sources.PublicPackage`, which only `Publishable` can
  construct, and only for packages whose lock entry proves a public-registry
  origin. Product names come from a fixed table. AI input is built from
  findings, which exist only for those packages and products, with file
  paths and raw declarations removed.
- **A failed attempt is not a result.** Source failures fail the attempt and
  leave the previous result and its dates intact, so an outage cannot turn
  known findings into "Nepatikrinta". Sources-checked, AI-generated, and
  last-attempt times are stored separately.
- **External data is always refetched; dates are compared with today.** The
  only cache is a 10-minute in-memory one for identical lookups within and
  across the jobs of one "refresh all".
- **Secrets stay in a structure.** `.env` is parsed into `config.AI`; the
  key is a `config.Secret` that prints as a placeholder everywhere, the
  `DEVATLAS_AI_*` variables are removed from the process environment, and the
  Git adapter filters them from child environments.
- **One worker, bounded queue, one job per project**, with a per-job
  deadline and a short cooldown. Periodic refresh is deliberately deferred.

Not implemented: periodic re-assessment, Yarn/pnpm/Python/Go parsing,
manifests in subdirectories, history of results.

## Intended package layout and adapter boundaries (planned)

The brief separates a logical Project from its Checkouts and Runtimes, and
requires every observation to carry source, status, and collection time.
The planned layout mirrors that:

```text
internal/registry     (exists) grows Runtime, Service, PortBinding,
                      Observation, Finding; status vocabulary (running,
                      stopped, unhealthy, unknown, error) and staleness rules.
internal/registryfile (exists) stays the source of truth for registrations.
internal/manifest     parse and validate project-registry.yaml found in a
                      registered checkout; document path resolution.
internal/store        SQLite index and observation history. Rebuildable from
                      registrations, manifests, and fresh observations;
                      never the source of truth for registrations or
                      documents.
internal/collect      one adapter per source behind a small interface:
  collect/git         (exists) branch, HEAD, change counts, worktrees,
                      sanitised remotes, ahead/behind from local
                      remote refs, labelled as such, never fetching.
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
home page, occupied-port reporting, graceful shutdown with an in-flight
request, checkout identity (symlinks, relative paths, worktrees), project
name rules, safe removal, persistence across reloads, malformed-file
handling and recovery without exposing YAML values, the technology status
rules (status matrix, critical dev dependency, unknown severity, dates
against today, undetermined versions), the collector (declared versus locked,
origins, symlink and size rejection, no `.env` access), the source clients
against an in-process fake (OSV pagination and alias de-duplication, CVSS
computation, retries, budget, minified Packagist metadata, npm manifests),
AI answer validation, key and private-name exclusion from requests, records,
responses, and logs, failed attempts keeping the previous result, the queue,
the POST guards, and the full table → refresh → result → card flow,
concurrent updates from
separate CLI processes, alias rejection, bounded lock waits, HTML escaping of
project names and branch names, and the read-only API. The Git adapter is
tested against real temporary repositories (unborn, clean, staged, unstaged,
renamed, conflicted, detached, two worktrees, subdirectory, awkward file
names) and against scripted fake git executables for timeouts, cancellation,
failure isolation, output limits, and classified errors; credential
exclusion is asserted on the JSON, the HTML, and the log. Later slices add
tests for port-conflict logic and status and staleness modelling, using
controlled fixtures for Docker, plus at least one flow verified against a
real local project.
