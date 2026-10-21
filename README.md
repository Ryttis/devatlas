# DevAtlas

DevAtlas is a small local Go service that will provide a single inventory of
locally developed projects, their actual runtime status, and project context
for the developer and AI coding agents. It targets Windows 11 + WSL2 and runs
as one Go process inside WSL.

**Current state: slice 03, read-only Git checkout observations.** This build
provides the `devatlas` CLI, a loopback-only HTTP server with a JSON health
endpoint, persistent registration of existing project directories as
checkouts of explicitly named projects, and a read-only Git observation for
each checkout (worktree root, branch or detached HEAD, commit, change counts,
sanitised remotes) collected by running the local `git` command whenever
`devatlas list`, `GET /api/projects`, or the HTML home page is produced. It
does not yet read `project-registry.yaml`, scan directories, talk to Docker,
analyse ports, run HTTP checks, show ahead/behind counts, or export context;
every checkout's runtime status is reported as `unknown` with the reason
"not collected" regardless of its Git state. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Prerequisites

- Go 1.22 or newer, available inside WSL. 1.22 is the real minimum: the
  router relies on `net/http.ServeMux` method and `{$}` patterns, which are
  enabled by the `go 1.22` directive in `go.mod`. Use a currently supported
  Go release in practice; slice 03 was verified with Go 1.27.3 and Go 1.22.12.
  Install Go from <https://go.dev/dl/> (for example unpack the Linux tarball
  to `/usr/local/go` and add `/usr/local/go/bin` to `PATH`) or through your
  distribution. The only module dependency is the YAML library
  `go.yaml.in/yaml/v3` (see "Dependencies"); `go build` downloads it through
  the module proxy on first use.
- Linux/WSL is the supported platform. Registration writes fail explicitly
  on other operating systems; there is no unverified fallback lock.
- Git 2.31 or newer on `PATH` inside WSL, for the Git observations
  (`git rev-parse --path-format=absolute` is the oldest feature used) and so
  that `devatlas version` and `/health` can report the commit. Without Git,
  registration still works and every Git observation reports `unavailable`.
  Builds from a working tree with uncommitted or untracked files are flagged
  as `dirty` / `"modified": true`.
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

bin/devatlas list          # table with a Git observation per checkout
bin/devatlas list -json    # same document as GET /api/projects
bin/devatlas list -no-git  # registrations only, no git commands run

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
| `GET /`         | HTML home page: registered projects and checkouts with Git columns, build info, implementation scope |
| `GET /health`   | JSON: `status`, `service`, `version`, `commit`, `modified`, `checked_at`, `uptime_seconds` |
| `GET /api/projects` | JSON: `schema_version`, `generated_at`, `registry_file`, `collection`, `runtime_status`, and `projects[]` with `checkouts[]`, each carrying a `git` observation |

Other paths return 404; other methods return 405. There are no mutation
endpoints: registration changes go through the CLI only.

`GET /api/projects` labels every fact with how it was obtained.
`path_exists` is a stat of the directory at `checked_at`: true for a directory,
false for an absent path or a non-directory, and null when the observation fails.
`path_status` distinguishes `present`, `missing`, `not a directory`, and `error`;
the same status appears in the CLI table and HTML. `git` is the observation
described in the next section. `collection.mode` is `current_request`: the
observations were produced while building that response, and nothing is
cached, stored, or refreshed in the background. `runtime` is always
`{"status": "unknown", "reason": "not collected: ..."}` because no Docker,
port, or HTTP collection exists in this build, and a successful Git
observation says nothing about containers or application health. If the
registration file becomes malformed or unreadable, both registry endpoints
return 500; the home page shows an error instead of a table. Responses and
server logs omit YAML values and parser excerpts. Fixing the file restores
responses without a restart. `/health` reports process health and does not
validate registrations.

Because every request runs git for every checkout (at most 4 at a time, see
the limits below), a request can take up to the collection deadline when a
repository is slow. Closing the HTTP connection cancels the running git
commands.

## Git observations

For each registered checkout, `list`, `/api/projects`, and the home page run
three read-only commands in the checkout directory and report the result as
one observation:

```json
"git": {
  "status": "ok",
  "source": "git-cli",
  "collected_at": "2026-10-21T11:01:15Z",
  "duration_ms": 10,
  "repository": {
    "repository_root": "/home/rytis/code/carrier-self-service",
    "worktree_root": "/home/rytis/code/carrier-self-service",
    "git_dir": "/home/rytis/code/carrier-self-service/.git",
    "common_dir": "/home/rytis/code/carrier-self-service/.git",
    "linked_worktree": false,
    "path_is_worktree_root": true,
    "head": { "branch": "main", "detached": false, "unborn": false, "commit": "203930b4e30eda1b0eb59249c5a7650f40d241de" },
    "changes": { "tracked": 0, "staged": 0, "unstaged": 0, "conflicted": 0, "untracked": 2 },
    "remotes": [ { "name": "origin", "role": "fetch", "url": "git@git.example.com:team/app.git", "host": "git.example.com", "provider": "unknown", "sanitized": false } ]
  }
}
```

**Statuses.** Exactly one applies, and `reason` explains every non-`ok` one
in words that never include command output, remote URLs, or credentials:

| `status` | Meaning |
| --- | --- |
| `ok` | All commands succeeded; `repository` is present. |
| `not_git` | The directory exists but is not inside a Git work tree. |
| `missing` | The registered path does not exist or is not a directory; git was not run. |
| `unavailable` | No `git` executable could be found or started. |
| `timeout` | A command exceeded the collection deadline. |
| `error` | Git ran and failed (the reason names the subcommand and exit status, or a recognised cause such as `safe.directory`), the output limit was exceeded, the output could not be parsed, or the request was cancelled. |

**Commands and counting semantics.** Git is invoked directly with separate
arguments, never through a shell, as
`git -C <dir> --no-optional-locks -c core.fsmonitor=false -c core.hooksPath=/dev/null <subcommand>`
with `GIT_TERMINAL_PROMPT=0`, `GIT_OPTIONAL_LOCKS=0`, empty `GIT_ASKPASS` and
`SSH_ASKPASS`, `LC_ALL=C`, and with `GIT_DIR`, `GIT_WORK_TREE`,
`GIT_INDEX_FILE`, `GIT_COMMON_DIR`, `GIT_OBJECT_DIRECTORY`,
`GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_NAMESPACE`, and `GIT_PREFIX` removed
from the environment so nothing can redirect the observation. Stdin is the
null device. Nothing is fetched, checked out, staged, committed, or written
to the index or configuration.

1. `rev-parse --path-format=absolute --show-toplevel --git-dir --git-common-dir`
   gives `worktree_root`, `git_dir`, and `common_dir`. `linked_worktree` is
   true when `git_dir` differs from `common_dir` (a `git worktree add`
   checkout). `repository_root` is the parent of `common_dir` when that
   directory is named `.git`, otherwise `common_dir` itself.
   `path_is_worktree_root` is false when the registered directory is a
   subdirectory of the work tree.
2. `status --porcelain=v2 -z --branch --untracked-files=all` gives `head`
   and `changes`. Records are NUL-delimited, so any file name is handled.
   `head.commit` is the full SHA; `unborn` is true and `commit` empty when
   the branch has no commits; `detached` is true and `branch` empty when
   HEAD is not on a branch.
   - `tracked`: tracked paths with any index, work-tree, or unmerged change.
     A path that is staged and then modified again counts once; a staged
     rename or copy counts once.
   - `staged`: tracked paths whose index differs from HEAD (first status
     column not `.`). `unstaged`: tracked paths whose work tree differs from
     the index (second column not `.`). One path can be in both.
   - `conflicted`: unmerged paths; they are included in `tracked`.
   - `untracked`: untracked files counted individually, including files in
     untracked directories. Ignored files are never counted. Submodules
     follow Git's defaults.
   Ahead/behind, upstream, and stash information are not reported.
3. `config -z --get-regexp '^remote\..+\.(url|pushurl)$'` lists remotes.
   `remote.<name>.url` has role `fetch`, `remote.<name>.pushurl` role
   `push`. No hosting API is queried and projects are never grouped by
   remote.

**Remote sanitisation.** Every URL is rewritten before it is stored in the
observation, so neither the API, the page, the CLI, nor the log ever sees a
credential. `sanitized` is true when something was removed.

| Input form | Output |
| --- | --- |
| `https://user:token@host/path?x#y`, `http://`, `git://` | user info, query, and fragment removed: `https://host/path` |
| `ssh://user:pw@host:port/path`, `git+ssh://` | password, query, fragment removed; the SSH user name is kept: `ssh://user@host:port/path` |
| `user@host:path` (scp style) | kept; a `user:pw@` prefix loses the password |
| `file:///path`, `/path`, `../path`, `C:\path` | unchanged, `provider` `local` |
| anything that fails to parse | replaced by `(unparseable remote URL, hidden)` |

`host` is the lower-cased host name. `provider` is `github`, `gitlab`, or
`bitbucket` only for the public hosts of those services (`github.com`,
`gist.github.com`, `ssh.github.com`, `gitlab.com`, `altssh.gitlab.com`,
`bitbucket.org`, `altssh.bitbucket.org`, and their `www.` forms); every other
host, including self-hosted GitLab or Bitbucket instances, is `unknown`
because a host name does not prove which product runs on it.

**Limits.** At most 4 checkouts are observed concurrently. Each git command
may run for 10 seconds and all commands for one checkout for 20 seconds
together; `list` additionally caps a whole run at 2 minutes. A command whose
standard output exceeds 8 MiB fails the observation rather than being
truncated. Git runs in its own process group and the whole group is killed on
timeout or cancellation. Up to 2 KiB of a failed command's stderr is written
to the server or CLI log; it is never part of a response. One checkout's
failure never affects another's observation.

## Dependencies

| Module | Version | Why |
| --- | --- | --- |
| `go.yaml.in/yaml/v3` | v3.0.5 | YAML for the registration file. This is the YAML organisation's maintained continuation of `gopkg.in/yaml.v3` (same API; the original repository was archived in 2025 with v3.0.1 as its last release). v4 was still a release candidate when chosen. Pinned in `go.mod` and `go.sum`. |

Everything else is the Go standard library.

## Repository layout

```text
cmd/devatlas/            CLI entry point
internal/cli/            command dispatch, flags, signal handling
internal/collect/git/    read-only Git observations: safe execution, status parsing, remote sanitisation
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
