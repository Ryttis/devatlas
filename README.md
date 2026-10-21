# DevAtlas

DevAtlas is a small local Go service that will provide a single inventory of
locally developed projects, their actual runtime status, and project context
for the developer and AI coding agents. It targets Windows 11 + WSL2 and runs
as one Go process inside WSL.

**Current state: slice 02, project and checkout registration.** This build
provides the `devatlas` CLI, a loopback-only HTTP server with a JSON health
endpoint, and persistent registration of existing project directories as
checkouts of explicitly named projects. Registrations are shown by
`devatlas list`, by the read-only `GET /api/projects` endpoint, and on the
HTML home page. It does not yet read `project-registry.yaml`, scan
directories, talk to Git or Docker, analyse ports, run HTTP checks, or export
context; every checkout's runtime status is reported as `unknown` with the
reason "not collected". See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Prerequisites

- Go 1.22 or newer, available inside WSL. 1.22 is the real minimum: the
  router relies on `net/http.ServeMux` method and `{$}` patterns, which are
  enabled by the `go 1.22` directive in `go.mod`. Use a currently supported
  Go release in practice; the slice 02 follow-up was verified with Go 1.27.3.
  Install Go from <https://go.dev/dl/> (for example unpack the Linux tarball
  to `/usr/local/go` and add `/usr/local/go/bin` to `PATH`) or through your
  distribution. The only module dependency is the YAML library
  `go.yaml.in/yaml/v3` (see "Dependencies"); `go build` downloads it through
  the module proxy on first use.
- Linux/WSL is the supported platform. Registration writes fail explicitly
  on other operating systems; there is no unverified fallback lock.
- Git, to clone the repository and so that `devatlas version` and
  `/health` can report the commit. Builds from a working tree with
  uncommitted or untracked files are flagged as `dirty` / `"modified": true`.
- Docker Desktop with the WSL2 backend is a prerequisite for later slices
  (container inventory), not for this one.
- `gcc` is only needed if you want to run the tests with `-race`.

Check with:

```sh
go version
```

## Build

```sh
cd ~/code/devatlas
go build -trimpath -o bin/devatlas ./cmd/devatlas
```

Set a release version at link time if needed:

```sh
go build -trimpath \
  -ldflags "-X github.com/Ryttis/devatlas/internal/version.Version=0.1.0" \
  -o bin/devatlas ./cmd/devatlas
```

`bin/` is ignored by Git.

## Tests and checks

```sh
gofmt -l .          # prints nothing when formatting is clean
go vet ./...
go test ./...
go test -race ./... # requires cgo and a C compiler
```

## CLI usage

```text
devatlas <command> [flags] [arguments]

Commands:
  add       Register a directory as a checkout of a project.
  list      List registered projects and their checkouts.
  remove    Remove a checkout registration by ID (never touches files).
  serve     Start the local HTTP server (loopback only).
  version   Print build information.
  help      Show this help.
```

Every command that reads or writes registrations accepts `-config PATH`.

### Registering projects

```sh
# Use a fresh demonstration registry, so this also works when CSS is already
# registered in your usual registry. Keep this variable in the server shell too.
export DEVATLAS_CONFIG="$(mktemp -d)/registrations.yaml"

# Register the existing CSS directory (adjust the path to your checkout).
# The first registration of a project ID needs -name.
bin/devatlas add -project carrier-self-service -name "Carrier Self-Service" ~/code/carrier-self-service

# Optional: run this only if this separate worktree directory already exists.
# It is a second checkout of the same project; -name may be omitted.
bin/devatlas add -project carrier-self-service ~/code/carrier-self-service-hotfix

bin/devatlas list          # table
bin/devatlas list -json    # same document as GET /api/projects

# To remove a registration, copy its actual checkout ID from list:
# bin/devatlas remove CHECKOUT_ID

# To view the demo, run bin/devatlas serve in this shell; stop with Ctrl+C.
# When finished with this demonstration, return to your registry:
unset DEVATLAS_CONFIG
```

For your usual registry, run `bin/devatlas list` first. If CSS is already
listed, skip its first `add`; use its existing project ID for another checkout.
A duplicate `add` deliberately fails. The demonstration file remains in the
temporary directory until you delete it.

Rules, all enforced by tests:

- **Project identity is explicit.** A project is whatever ID you choose
  (1-64 lowercase letters, digits, hyphens). It is never derived from a
  directory name or Git remote. Several checkouts can share one project ID.
- **Checkout identity is the directory.** The path is resolved to a canonical
  absolute directory with symlinks evaluated, and the checkout ID is the first
  12 hex characters of the SHA-256 of that path. The same directory therefore
  always gets the same ID, and the directory must exist at registration time.
  A truncated-ID collision with a different path is rejected before mutation.
- **Duplicates are rejected.** Registering a directory that is already
  registered, under any project and by any spelling (relative path, symlink,
  trailing slash), fails with exit code 1 and names the existing checkout.
  Distinct directories, such as two Git worktrees of one repository, are
  distinct checkouts. Missing or inaccessible registered directories stay
  registered until explicitly removed.
- **Names are stable.** `-name` is required when the project ID is new. For an
  existing project it may be omitted; a different name is an error, not a
  rename.
- **Unknown IDs fail.** `remove` with an ID that is not registered exits with
  code 1 and changes nothing.
- **Removing the last checkout removes the project record.** A project holds
  only its ID and name in this version, both of which are supplied again on
  the next `add`, so an empty project record would only be clutter. The
  command says so when it happens.
- **Removal never touches files.** Only the registration file changes. The
  directory, its contents, worktrees, and containers are left alone.
- **No Git, Docker, or manifest is needed** to register a directory.

### Registration file

Registrations live in one YAML file outside every inventoried repository:

| Precedence | Location |
| --- | --- |
| 1 | `-config PATH` flag |
| 2 | `DEVATLAS_CONFIG` environment variable |
| 3 | `$XDG_CONFIG_HOME/devatlas/registrations.yaml`, i.e. `~/.config/devatlas/registrations.yaml` on WSL |

```yaml
# DevAtlas registration file. Managed by "devatlas add" and "devatlas remove".
# Paths are machine-specific; keep this file out of version control.
schema_version: 1
projects:
  - id: carrier-self-service
    name: Carrier Self-Service
    registered_at: 2026-10-21T12:00:00Z
checkouts:
  - id: 3f1c9a2b7e4d
    project_id: carrier-self-service
    path: /home/rytis/code/carrier-self-service
    registered_at: 2026-10-21T12:00:00Z
```

- The file and newly created configuration directories use mode 0600
  and 0700 respectively (existing directory permissions are preserved).
  `list` and `serve` treat a missing file as an empty registry.
- Writes go to a temporary file in the same directory followed by an atomic
  rename, so a crash cannot leave a half-written file.
- Concurrent `add`/`remove` processes are serialised with an advisory lock on
  `registrations.yaml.lock` (flock, released automatically on exit). The whole
  read-modify-write happens inside the lock, so no update is lost. Acquisition
  waits at most 10 seconds. The sidecar stays in place across registry renames;
  do not delete or replace it while any process may be using it.
- Parent-directory symlink aliases resolve to the same configuration and lock
  paths. Configuration-file symlinks, hard links, and special files are rejected;
  pass the original regular file path instead. Keep the directory topology stable
  while commands run. These guarantees apply to a local Linux filesystem with
  working `flock` and atomic rename; network filesystems and Windows mounts have
  not been verified.
- A file that fails to parse, has unknown fields, declares another
  `schema_version`, contains extra YAML documents, or is internally inconsistent
  (duplicate IDs, duplicate canonical paths, a checkout whose project does not exist) makes every command fail with exit code 1 and
  a message naming the file. The file is never rewritten in that state; fix it
  by hand or delete it.
- The server reads the file on every request, so `add`/`remove` from another
  terminal shows up in the API and on the page without restarting `serve`.

`devatlas serve` flags:

| Flag                | Default     | Meaning                                                   |
| ------------------- | ----------- | --------------------------------------------------------- |
| `-host`             | `127.0.0.1` | Loopback IP to bind. `::1` is also accepted. Anything else is rejected. |
| `-port`             | `8642`      | TCP port. `0` picks a free port; the chosen URL is logged. |
| `-shutdown-timeout` | `10s`       | Time allowed for in-flight requests after SIGINT/SIGTERM. |
| `-log-level`        | `info`      | `debug`, `info`, `warn`, or `error`.                      |
| `-config`           | see above   | Registration file to serve.                               |

Exit codes: `0` success, `1` runtime failure (port already in use, duplicate
directory, unknown checkout ID, malformed registration file), `2` invalid
command line (unknown command or flag, non-loopback host, port out of range,
missing or nonexistent directory, missing project name).

Examples:

```sh
bin/devatlas version
bin/devatlas serve
bin/devatlas serve -port 9090
bin/devatlas serve -host ::1 -port 0
```

## Running the server in WSL

1. Open a WSL terminal and build as above.
2. Start the server:

   ```sh
   bin/devatlas serve
   ```

   The log line `devatlas listening url=http://127.0.0.1:8642` confirms the
   bound address.
3. Open <http://127.0.0.1:8642/> in a browser on Windows. Windows forwards
   `localhost`/`127.0.0.1` into WSL2, so the page is reachable from the Windows
   side without any extra configuration, while remaining unreachable from
   other machines.
4. Check health from any shell:

   ```sh
   curl http://127.0.0.1:8642/health
   ```

   ```json
   {
     "status": "ok",
     "service": "devatlas",
     "version": "dev",
     "commit": "7ad7c318023d",
     "modified": false,
     "checked_at": "2026-10-21T08:21:14Z",
     "uptime_seconds": 0
   }
   ```

5. Stop with `Ctrl+C` (SIGINT) or `kill <pid>` (SIGTERM). The server finishes
   in-flight requests, up to `-shutdown-timeout`, and exits with code 0. A
   second signal during shutdown terminates the process immediately.

If the port is occupied the server refuses to start and tells you so:

```text
error: address 127.0.0.1:8642 is already in use by another process; choose a different port with -port
```

The default port 8642 was chosen because it was free on the development
workstation; it is not reserved for anything. Override it with `-port` if it
conflicts on your machine.

## HTTP endpoints

| Method and path | Response                                                  |
| --------------- | --------------------------------------------------------- |
| `GET /`         | HTML home page: registered projects and checkouts, build info, implementation scope |
| `GET /health`   | JSON: `status`, `service`, `version`, `commit`, `modified`, `checked_at`, `uptime_seconds` |
| `GET /api/projects` | JSON: `schema_version`, `generated_at`, `registry_file`, `runtime_status`, and `projects[]` with `checkouts[]` |

Other paths return 404; other methods return 405. There are no mutation
endpoints: registration changes go through the CLI only.

`GET /api/projects` reports two facts per checkout and labels them honestly:
`path_exists` is a stat of the directory at `checked_at`: true for a directory,
false for an absent path or a non-directory, and null when the observation fails.
`path_status` distinguishes `present`, `missing`, `not a directory`, and `error`;
the same status appears in the CLI table and HTML. `runtime` is always
`{"status": "unknown", "reason": "not collected: ..."}` because no Git,
Docker, port, or HTTP collection exists in this build. If the registration
file becomes malformed or unreadable, both registry endpoints return 500; the
home page shows an error instead of a table. Responses and server logs omit YAML
values and parser excerpts. Fixing the file restores responses without a restart.
`/health` reports process health and does not validate registrations.

## Dependencies

| Module | Version | Why |
| --- | --- | --- |
| `go.yaml.in/yaml/v3` | v3.0.5 | YAML for the registration file. This is the YAML organisation's maintained continuation of `gopkg.in/yaml.v3` (same API; the original repository was archived in 2025 with v3.0.1 as its last release). v4 was still a release candidate when chosen. Pinned in `go.mod` and `go.sum`. |

Everything else is the Go standard library.

## Repository layout

```text
cmd/devatlas/            CLI entry point
internal/cli/            command dispatch, flags, signal handling
internal/httpserver/     loopback HTTP server, handlers, templates, /api/projects
internal/registry/       registration model: projects, checkouts, identity rules
internal/registryfile/   YAML persistence, atomic writes, locking, config path
internal/version/        build identification
docs/ARCHITECTURE.md     current structure and intended adapter boundaries
examples/project-registry.yaml
                         example project manifest (not yet read by the tool)
implementation_brief.md  approved scope document this work follows
```

## Local configuration

The registration file described above holds machine-specific absolute paths
and lives under the user configuration directory, never inside a project
repository. Other local settings (`devatlas.local.yaml`, `*.local.yaml`) are
reserved by `.gitignore` but not read yet.
