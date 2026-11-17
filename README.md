# DevAtlas

DevAtlas is a small local Go service that will provide a single inventory of
locally developed projects, their actual runtime status, and project context
for the developer and AI coding agents. It targets Windows 11 + WSL2 and runs
as one Go process inside WSL.

**Current state: slice 04, technology status dashboard (first stage).**
This build provides the `devatlas` CLI, a loopback-only HTTP server,
persistent registration of existing project directories as checkouts of
explicitly named projects, a read-only Git observation for each checkout, and
a per-project technology status: a freshness and a security status decided by
documented rules from facts read out of Composer and npm manifests and lock
files, Dockerfiles and Compose files, checked against OSV, endoflife.date,
the npm registry and Packagist, optionally explained by an AI model through
one OpenAI-compatible adapter. See "Technology status dashboard" below. It
does not yet read `project-registry.yaml`, scan directories, talk to Docker,
analyse ports, run HTTP checks, show ahead/behind counts, or export context;
every checkout's runtime status is reported as `unknown` with the reason
"not collected" regardless of its Git state. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Prerequisites

- Go 1.22 or newer, available inside WSL. 1.22 is the real minimum: the
  router relies on `net/http.ServeMux` method and `{$}` patterns, which are
  enabled by the `go 1.22` directive in `go.mod`. Use a currently supported
  Go release in practice; slice 03 was verified with Go 1.27.3 and Go 1.22.12,
  slice 04 with Go 1.22.12.
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
| `GET /`         | HTML home page: technology status table, registered checkouts with Git columns, build info, implementation scope |
| `GET /projects/{id}` | HTML project card: statuses, assessed checkout, findings with evidence, sources and actions, technologies, dependencies, Git |
| `GET /api/projects/{id}/assessment` | JSON: the stored assessment record (last successful result and last attempt); 404 before the first run |
| `POST /projects/{id}/assess` | Queues an assessment of one checkout (form field `checkout`, optional); answers 303 |
| `POST /assess-all` | Queues an assessment of every project; answers 303 |
| `GET /health`   | JSON: `status`, `service`, `version`, `commit`, `modified`, `checked_at`, `uptime_seconds` |
| `GET /api/projects` | JSON: `schema_version`, `generated_at`, `registry_file`, `collection`, `runtime_status`, and `projects[]` with `checkouts[]`, each carrying a `git` observation |

Other paths return 404; other methods return 405. Registration changes go
through the CLI only. The two POST endpoints change nothing in any project:
they start an assessment whose result is written to DevAtlas's own record
directory. They are accepted only when the `Host` header names this loopback
listener and the browser's `Origin` and `Sec-Fetch-Site` headers, when
present, say the request is same-origin, so a web page open in the same
browser cannot trigger assessments (or AI spending).

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

## Technology status dashboard

The home page shows one row per project: purpose, technologies and versions,
freshness, security, the time of the check, and an "Atnaujinti" button; the
"Atnaujinti visus" button queues every project. The project card
(`/projects/{id}`) shows the assessed checkout, every finding with its
evidence, sources and recommended action, technologies, direct and transitive
dependencies, and Git state. The dashboard UI is in Lithuanian.

**Scope.** A status covers the support status of declared technologies and
known vulnerabilities of locked dependencies. It is not a security audit of
the whole system: the running environment, source code, configuration, and
secrets are not examined.

### What is read

Only these files, as data, with size limits (2 MiB for manifests and
infrastructure files, 32 MiB for lock files):

- `composer.json`, `composer.lock`, `package.json`, `package-lock.json`
  (lockfileVersion 2 or 3) in the checkout root;
- `Dockerfile*` and `compose*.yml|yaml` / `docker-compose*.yml|yaml` in the
  root and up to three directory levels below it, plus Dockerfiles named by a
  Compose `build` section inside the checkout.

No project code, package manager, or install script is ever executed.
Symbolic links are rejected at every path component. `.env` files are never
opened; a Compose image that depends on a variable is reported as having no
determinable version. `vendor`, `node_modules`, `var`, and hidden directories
are not entered.

Declared and locked versions are kept apart. A constraint such as `^8.2` or
an image tag is a declaration, shown as such, and never presented as the
version that runs. A constraint that admits several release lines yields no
version (it is not narrowed to its lower bound); `8.2.*` or `~8.2.0` does. A
Dockerfile tag written with `ARG` is resolved from the `ARG` default and
labelled accordingly.

Yarn, pnpm, Python, Go, Ruby, Rust, Maven, Gradle and .NET are detected and
shown as "Nepatikrinta" with the reason; so are dependency manifests in
subdirectories. Coverage is counted over every detected ecosystem, so a
Composer project with an unchecked `yarn.lock` is not fully checked.

### Status rules

Rules version `2026-11-17.1` (`internal/assess/rules.go`). The colour is
always decided by these rules; AI text cannot change it.

| Rule | Area | Level | Condition |
| --- | --- | --- | --- |
| `SEC-CRITICAL` | security | red | OSV vulnerability of a locked public package with severity critical (database label `CRITICAL` or CVSS v3 base score ≥ 9.0). A development dependency is not exempt. |
| `SEC-KNOWN` | security | yellow | Any other OSV vulnerability with a known severity. |
| `SEC-UNKNOWN-SEVERITY` | security | yellow | OSV vulnerability whose severity cannot be established (no rating, only a CVSS v4 vector, or no details record). It is a finding that asks for manual assessment, never the absence of one. |
| `FRESH-EOL-CRITICAL` | freshness | red | A runtime, framework, or database whose end-of-support date on endoflife.date is today or earlier. |
| `FRESH-EOL-OTHER` | freshness | yellow | The same for a web server, broker, proxy, or base OS image. |
| `FRESH-EOL-SOON` | freshness | yellow | End of support within 180 days. |
| `FRESH-DEPRECATED` | freshness | yellow | Direct dependency marked abandoned on Packagist, or its locked version deprecated on npm. |
| `FRESH-MAJORS-BEHIND` | freshness | yellow | Direct dependency two or more major versions behind the latest stable release (`@types/*` excepted). |
| `FRESH-MAJOR-AVAILABLE` | freshness | note | One major version behind. A new major version alone is not a problem. |
| `FRESH-SECURITY-ONLY` | freshness | note | Active support ended, security fixes continue. |

Area status:

| Findings (red/yellow) | Scope fully checked | Shown |
| --- | --- | --- |
| none | yes | 🟢 OK |
| none | no | ⚪ Nepatikrinta, with the reason |
| some | yes | the worst colour |
| some | no | the worst colour + "Dalinė patikra" |

The scope is not fully checked when an ecosystem is unsupported or has no
usable lock file, a locked package is private or of unknown origin or locked
to a branch, a recognised technology has no determinable version or its
release cycle is unknown to endoflife.date (a tag naming only a major version
is not matched to a minor cycle), a direct dependency is unknown to its
registry, or a file was rejected. Images that are not in the built-in table
of official images (private or third-party images) are listed on the card as
outside the scope and do not count against coverage.

Support dates are compared with the date of each run; the service's own
"is EOL" flag is used only when it gives no date.

### External sources and what is sent

| Source | Used for | What is sent |
| --- | --- | --- |
| OSV (`api.osv.dev`) | known vulnerabilities | name and locked version of public packages. The batch endpoint returns identifiers only and is paginated per query; details are fetched per identifier. Severity arrives as a CVSS vector, so the v3 base score is computed locally. |
| endoflife.date (v1 API) | support dates | a product name from a fixed table, nothing from the project. Date fields may be null. |
| npm registry | latest version, deprecation | name and locked version of public direct dependencies. Only the small `/{name}/latest` and `/{name}/{version}` manifests are requested, never the full package document; deprecation is read from the locked version. |
| Packagist (`repo.packagist.org/p2`) | latest version, abandoned flag | name of public direct dependencies. The metadata is minified (only the first entry is complete) and lists pre-releases first, so the latest stable release is found by comparing version numbers. |

A package name leaves the machine only when the lock file itself proves a
public origin: `notification-url` equal to Packagist's for Composer, a
`resolved` URL on `registry.npmjs.org` for npm. Anything else is private or
of unknown origin, is never named to any service (its publicity is not
tested by asking a registry), and counts as unchecked. Requests have a
20 s timeout, at most 2 retries on network errors, 429 and 5xx, a budget of
1200 requests per assessment, response size limits, and no redirects.
Answers are cached in memory for 10 minutes, except the OSV batch query.

### AI explanations

Optional. Copy `.env.example` to `.env` in the directory where `devatlas
serve` is started, or to `~/.config/devatlas/.env`, or pass `-env-file`:

```sh
DEVATLAS_AI_BASE_URL=https://api.openai.com/v1   # any OpenAI-compatible API root
DEVATLAS_AI_MODEL=<model name>
DEVATLAS_AI_API_KEY=<key>                        # optional for local servers
```

The base URL must use https unless the host is local. The file is parsed
into a structure; the values are not exported into the process environment,
and any inherited `DEVATLAS_AI_*` variable is removed from it at start-up and
filtered from the environment of `git` child processes. The key is sent only
in the `Authorization` header of the AI request and never appears in HTML,
API responses, stored records, or logs; provider error bodies are not shown
or logged.

The model receives the rule findings (component names, versions, dates,
advisory identifiers and summaries) and returns, per finding, an explanation
and concrete steps. It receives no file paths, raw declarations, private
names, or `.env` content, and it has no tools. Its answer is validated: only
the agreed JSON shape is accepted, texts for unknown finding identifiers are
dropped, as are texts containing an address or an advisory identifier that
was not in the finding's evidence. Findings, levels, sources, and the rule's
own recommended action are always rendered from the rule results, so an
unavailable or rejected AI answer hides nothing.

Explanations are reused while their input is identical. The reuse key covers
the assessed files' hash, the findings with their evidence (which carries the
external conclusions), the area statuses, the rules version, the prompt and
its version, and the model.

### Stored results, times, and staleness

One JSON record per project under `assessments/` next to the registration
file, written atomically with mode 0600. A record keeps the last successful
result and the last attempt separately:

- `sources_checked_at`: when the sources were queried and the rules
  evaluated;
- `ai.generated_at`: when the explanations were generated (unchanged on
  reuse);
- `last_attempt.at`, `ok`, `error`: the most recent run.

An attempt fails when the checkout cannot be read or an external source is
unreachable or answers with something uninterpretable. A failed attempt
leaves the previous result and its dates untouched and is shown next to it.
External data is fetched on every run, whether or not project files changed.

A result is tied to the checkout path, HEAD, the uncommitted-changes flag,
and a hash of the assessed files. It is marked "Pasenęs" when it is older
than 24 hours, the rules version changed, HEAD or the uncommitted-changes
flag changed, the checkout was unregistered, or (on the card) the assessed
files changed. With several checkouts the card offers a choice; the table
names the checkout that was assessed.

Assessments run one at a time from a bounded queue (64 jobs), at most one
queued or running job per project, with a 6-minute limit per job and a
20-second pause before the same project can be queued again. Periodic
re-assessment is not implemented: statuses change only when a button is
pressed. DevAtlas never changes a project or updates a dependency.

## Dependencies

| Module | Version | Why |
| --- | --- | --- |
| `go.yaml.in/yaml/v3` | v3.0.5 | YAML for the registration file. This is the YAML organisation's maintained continuation of `gopkg.in/yaml.v3` (same API; the original repository was archived in 2025 with v3.0.1 as its last release). v4 was still a release candidate when chosen. Pinned in `go.mod` and `go.sum`. |

Everything else is the Go standard library.

## Repository layout

```text
cmd/devatlas/            CLI entry point
internal/cli/            command dispatch, flags, signal handling
internal/assess/         status rules, AI adapter and answer validation, record store, queue
internal/atomicfile/     atomic file replacement for assessment records
internal/collect/git/    read-only Git observations: safe execution, status parsing, remote sanitisation
internal/collect/stack/  technology facts from manifests, lock files, Dockerfiles, Compose files
internal/config/         .env parsing into a structure; secret type
internal/sources/        OSV, endoflife.date, npm and Packagist clients; public-package filter
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
reserved by `.gitignore` but not read yet. `.env` (ignored by Git) holds the
optional AI adapter settings described under "AI explanations"; assessment
records live in `assessments/` next to the registration file.
