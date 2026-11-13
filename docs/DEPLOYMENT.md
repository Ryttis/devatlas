# Git-based rollout of the DevAtlas web service

This describes how the loopback web UI and agent console are built from GitHub
and run as `devatlas-web.service`. It is a procedure, not a record of a rollout:
each cutover needs explicit human approval of the exact commit and of these
steps. A review PASS and a successful runtime verification are separate facts.

The procedure never touches `ssh.service`, Nginx, the MCP connector or
Keycloak. The listener stays on `127.0.0.1:18642`, so the existing Nginx
`auth_basic` proxy keeps protecting it unchanged.

## Layout

Everything is owned by the unprivileged operator; nothing runs as root.

| Path | Purpose |
|---|---|
| `~/devatlas-web/src` | Deployment-only clone of GitHub (read-only HTTPS). Never work here. |
| `~/devatlas-web/releases/<full-sha>/devatlas` | One binary per built commit, with `SHA256`. |
| `~/devatlas-web/current` | Symlink to the release the unit runs. |
| `~/devatlas-web/previous` | Symlink to the release that served before the last upgrade. |
| `~/devatlas-web/rollback/legacy/` | Private record of the pre-unit process: binary copy with `SHA256`, argv, working directory, needed environment, commit. |
| `$DEVATLAS_REGISTRY` | Project registry (`-config`). Default `~/.config/devatlas/registrations.yaml`, the CLI default. |
| `~/.local/state/devatlas` | Runner state (`-state-dir`): agent runs and workflows, read-only for the web process. |

Paths and the port can be overridden with `DEVATLAS_DEPLOY_ROOT`,
`DEVATLAS_REGISTRY`, `DEVATLAS_STATE_DIR` and `DEVATLAS_PORT`; the rendered
unit follows the same values.

The registry path is never changed by a migration. On Rytis's instance the
running process uses `/home/rytis/devatlas-test.LGZc8T/registrations.yaml`, so
every command there is run with:

```sh
export DEVATLAS_REGISTRY=/home/rytis/devatlas-test.LGZc8T/registrations.yaml
```

`cutover` refuses, before stopping anything, if the running process's `-config`
differs from the path the unit would use.

## Commands

Run `scripts/deploy-web.sh` from any checkout that contains it, as the operator.
Requirements: Go on `PATH`, Git, curl, python3, a C compiler for race tests.

```sh
SHA=<approved 40-character commit>

scripts/deploy-web.sh status            # read-only: listener, unit, releases, data
scripts/deploy-web.sh build "$SHA"      # no effect on the running server
scripts/deploy-web.sh candidate "$SHA"  # temporary port 18652, then stopped
scripts/deploy-web.sh unit              # print the exact unit for review
```

`build` fetches from GitHub, requires the SHA to be reachable from a GitHub
branch or pull request, requires every GitHub check run on it to have completed
successfully, checks it out detached, runs gofmt, vet, tests and race tests, and
accepts the binary only if its embedded `vcs.revision` is the SHA and
`vcs.modified` is false. It refuses to run if the deployment clone has
uncommitted changes; it never resets or discards anything.

`candidate` starts that binary on another loopback port with a private copy of
the registry and the real runner state, checks `/health` (commit, unmodified),
`/`, `/runs`, `/projects`, `/api/projects`, a real run detail and a workflow
detail, that `/runs/<run>/review.json` and `/state.json` are not served even
when the file exists on disk, and that writes are rejected. It confirms the
production listener PID did not change and stops the candidate.

### One-time administrator setup

Installing the unit is the only step that needs root. It does not start anything.

```sh
scripts/deploy-web.sh unit > /tmp/devatlas-web.service
sudo install -o root -g root -m 0644 /tmp/devatlas-web.service /etc/systemd/system/devatlas-web.service
sudo systemctl daemon-reload
systemd-analyze verify /etc/systemd/system/devatlas-web.service
```

`cutover` refuses to proceed unless the installed unit is identical to the
rendered one. Later, `sudo` is needed only for `systemctl` on this unit.

### Cutover (after approval)

```sh
scripts/deploy-web.sh cutover "$SHA"
```

1. Checks the release, the installed unit and the state directory, then asks
   for `sudo` once.
2. Finds the PID listening on the port. If it is not the unit, it must be the
   operator's `devatlas serve` on that port with the same registry path;
   anything else aborts with nothing changed.
3. Records that process privately: a copy of its binary with its SHA256, its
   exact argv, working directory, start time, commit and the environment it
   needs (`HOME`, `PATH`, locale, `XDG_*` base directories, `DEVATLAS_*`,
   `GIT_*`; session variables are not stored).
4. Opens a pidfd for the PID, re-checks start time, owner, binary hash and
   command line, and only then sends SIGTERM through the pidfd, so a reused PID
   cannot be signalled. It never uses SIGKILL and never acts on a cgroup or on
   `ssh.service`. A mismatch stops with nothing changed.
5. Switches `current`, restarts the unit and waits up to 20 seconds for
   `/health` to report the approved commit from the unit's own main PID.
6. On success enables the unit and runs `verify`.

From step 4 until readiness is confirmed, every way out of the script restores
the prior service: a `systemctl` error, a failed readiness check, any other
failing command, or an interrupt. Restoration stops the unit and then either
restarts the previous release or relaunches the recorded legacy process. The
legacy process is started only from the saved binary, only if it still matches
its recorded SHA256, with the recorded argv, working directory and environment.
The script ends with `ROLLED BACK` or, if nothing healthy answers, with
`ROLLBACK FAILED` and a non-zero status.

Expect a few seconds of `502` from Nginx between steps 4 and 5. Re-running
`cutover` with the SHA that is already served changes nothing.

### Verification

```sh
scripts/deploy-web.sh verify "$SHA"
DEVATLAS_WEB_NETRC=~/.config/devatlas/web.netrc scripts/deploy-web.sh verify "$SHA"
```

Checks local health and commit, that unauthenticated `/`, `/runs` and
`/projects` through Nginx return `401`, that Nginx `/healthz` answers and that
`/mcp` still demands a token. With a netrc file (mode 0600, `machine
devatlas.etno-lentos.com login … password …`) it also checks the authenticated
pages. Restart behaviour is checked by hand:

```sh
sudo systemctl restart devatlas-web.service && scripts/deploy-web.sh verify "$SHA"
sudo systemctl kill -s KILL devatlas-web.service   # Restart=on-failure brings it back in 5s
sleep 8 && scripts/deploy-web.sh status
```

### Rollback

```sh
scripts/deploy-web.sh rollback
```

With a `previous` release it switches `current` back and restarts the unit; if
that release is not healthy it returns to the one that was serving. Without a
`previous` release (first migration) it disables and stops the unit and
relaunches the recorded legacy process from the saved, SHA256-verified binary.
Both paths tolerate `systemctl` errors, wait for `/health` to report the
expected commit and say plainly when they fail. Releases, the registry, runner
state and the legacy record are never deleted.

Manual equivalent for the first migration, if the script is unavailable:

```sh
sudo systemctl disable --now devatlas-web.service
L=~/devatlas-web/rollback/legacy
(cd "$L" && sha256sum -c SHA256) &&
(cd "$(cat "$L/cwd")" && setsid python3 -c '
import os, sys
rec = sys.argv[1]
items = lambda name: open(os.path.join(rec, name), "rb").read().split(b"\0")[:-1]
os.execve(os.path.join(rec, "devatlas").encode(), items("cmdline"),
          dict(i.split(b"=", 1) for i in items("environ")))
' "$L" >>"$L/server.log" 2>&1 </dev/null &)
curl -fsS --retry 5 --retry-connrefused http://127.0.0.1:18642/health
```

## Unit privileges

The unit runs as the operator with no capabilities, `NoNewPrivileges`,
`ProtectSystem=strict`, `ProtectHome=read-only`, private `/tmp` and devices, and
IP traffic limited to loopback. It has no writable path: the web process only
reads the registry, runner state and Git metadata of registered checkouts.
Registrations are changed with the CLI (`devatlas add|remove`), outside the unit.

## Limitations

- The sandboxing directives are checked with `systemd-analyze verify`; their
  effect on Git observations of real registered checkouts is first exercised at
  cutover, which is why readiness failure rolls back automatically.
- GitHub check status is read from the public API without authentication and
  is subject to its rate limit; a failed lookup blocks the build.
- The legacy process restored by rollback is again an unmanaged process and
  does not survive a reboot. It runs from the saved copy of the binary.
- Failure handling was exercised with a stand-in for `systemctl` on a temporary
  port, not under the system service manager.
