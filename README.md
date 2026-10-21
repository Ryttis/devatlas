# DevAtlas

DevAtlas is a small local Go service that will provide a single inventory of
locally developed projects, their actual runtime status, and project context
for the developer and AI coding agents. It targets Windows 11 + WSL2 and runs
as one Go process inside WSL.

**Current state: slice 01, application bootstrap.** This build provides the
`devatlas` CLI, a loopback-only HTTP server, a JSON health endpoint, and a
home page. It does not yet register projects, read `project-registry.yaml`,
talk to Git or Docker, analyse ports, run HTTP checks, or export context.
See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the planned structure.

## Prerequisites

- Go 1.22 or newer, available inside WSL. 1.22 is the real minimum: the
  router relies on `net/http.ServeMux` method and `{$}` patterns, which are
  enabled by the `go 1.22` directive in `go.mod`. Use a currently supported
  Go release in practice; this slice was verified with Go 1.27.3 and Go 1.22.
  DevAtlas uses only the Go standard library; there are no module
  dependencies. Install Go from <https://go.dev/dl/> (for example unpack the
  Linux tarball to `/usr/local/go` and add `/usr/local/go/bin` to `PATH`) or
  through your distribution.
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
devatlas <command> [flags]

Commands:
  serve     Start the local HTTP server (loopback only).
  version   Print build information.
  help      Show this help.
```

`devatlas serve` flags:

| Flag                | Default     | Meaning                                                   |
| ------------------- | ----------- | --------------------------------------------------------- |
| `-host`             | `127.0.0.1` | Loopback IP to bind. `::1` is also accepted. Anything else is rejected. |
| `-port`             | `8642`      | TCP port. `0` picks a free port; the chosen URL is logged. |
| `-shutdown-timeout` | `10s`       | Time allowed for in-flight requests after SIGINT/SIGTERM. |
| `-log-level`        | `info`      | `debug`, `info`, `warn`, or `error`.                      |

Exit codes: `0` success, `1` runtime failure (for example the port is already
in use), `2` invalid command line (unknown command or flag, non-loopback host,
port out of range).

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
| `GET /`         | HTML home page with build info and implementation scope   |
| `GET /health`   | JSON: `status`, `service`, `version`, `commit`, `modified`, `checked_at`, `uptime_seconds` |

Other paths return 404; other methods return 405.

## Repository layout

```text
cmd/devatlas/            CLI entry point
internal/cli/            command dispatch, flags, signal handling
internal/httpserver/     loopback HTTP server, handlers, templates
internal/version/        build identification
docs/ARCHITECTURE.md     current structure and intended adapter boundaries
examples/project-registry.yaml
                         example project manifest (not yet read by the tool)
implementation_brief.md  approved scope document this work follows
```

## Local configuration

Machine-specific settings (absolute paths, private addresses, port overrides)
are intended to live in local files that are ignored by Git, such as
`devatlas.local.yaml` or `*.local.yaml`. No such file is read yet.
