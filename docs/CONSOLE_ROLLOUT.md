# Console rollout preparation — issue #7

Status: prepared, NOT deployed. Application candidate approved by Astra:
`9b92a6127d721c2e91d12f29b780a48827389d1c`.
The deployment files themselves still require independent review and explicit
approval of the resulting server inventory and cutover. Do not merge this
preparation into the console PR solely to deploy the application.

## 1. Obtain preparation and inspect the actual host (read-only)

Run as `rytis` on `vaicaicio4`, in the existing DevAtlas Git checkout.
First inspect `hostname`, `pwd`, `git remote -v`, and `git status --short`.
Require the remote to be Ryttis/devatlas on GitHub; stop on unrelated changes.
Fetch the preparation branch normally, without a worktree, reset or force push:

```sh
git fetch origin chore/console-rollout
git switch --track origin/chore/console-rollout # first checkout only
git log -1 --format='%H %s'
```

On subsequent runs use the existing branch and `git merge --ff-only
origin/chore/console-rollout` only if the working tree is clean. Pin and review
the full preparation SHA before using its scripts. Fetch is not deployment.

Keep the preparation outside the source checkout so building the approved
application commit does not depend on deployment files being in that commit:

```sh
set -eu
test "$(hostname -s)" = vaicaicio4
test "$(id -un)" = rytis
test -z "$(git status --porcelain)"
umask 077
PREP=$(mktemp -d "$HOME/devatlas-rollout.XXXXXX")
cp deploy/inspect-web.py deploy/smoke-web.py deploy/devatlas-web.service "$PREP/"
git rev-parse HEAD > "$PREP/preparation-sha"
sudo python3 "$PREP/inspect-web.py" > "$PREP/before.json"
sudo systemctl show devatlas-web.service -p LoadState -p ActiveState
sudo ss -lntp 'sport = :18642'
sudo nginx -t
sudo cat /etc/nginx/conf.d/devatlas-tls.conf
```

`before.json` discovers the current listener, PID/start time, executable hash,
argv, user, cwd, cgroup, health, resolved registry and state paths. It does not
print the whole environment, registry contents, reports or credentials.
Do not paste unrelated Nginx secrets; inspect its auth configuration locally.

Required findings before proceeding:

- Host `vaicaicio4`, user `rytis`, one DevAtlas loopback listener on 18642.
- `devatlas-web.service` does not already exist. If it does, this first-migration
  procedure does not apply; preserve it and prepare a separate upgrade plan.
- The old process is manually launched, not supervised by a dedicated web unit.
  Its membership in `ssh.service` does NOT authorize stopping that service.
- Existing registry is readable by rytis (or its absence is intentional), state
  root exists and contains expected runs. Inspect both discovered paths using
  `sudo -u rytis test -r PATH` and `sudo -u rytis test -x DIRECTORY`.
- Nginx's root proxies to 127.0.0.1:18642 with the expected authentication.
  `/runs`, detail and `/projects` inherit that protection. Preserve configuration.
- Paths required by Git observations remain readable under `ProtectHome=read-only`.
  `PrivateTmp=true` hides host `/tmp`/`/var/tmp` checkouts: if any registry entry
  depends on them, revise the unit before approval. Do not silently hide projects.

State defaults are derived from the live process environment, not from this
document. Check them against the runner's actual state directory. Do not create
an empty replacement registry or copy/move/overwrite history.

## 2. Build the exact GitHub candidate and test without cutover

```sh
APP_SHA=9b92a6127d721c2e91d12f29b780a48827389d1c
git fetch origin feat/agent-console
git cat-file -e "$APP_SHA^{commit}"
git merge-base --is-ancestor "$APP_SHA" FETCH_HEAD
test -z "$(git status --porcelain)"
git switch --detach "$APP_SHA"
test "$(git rev-parse HEAD)" = "$APP_SHA"
test -z "$(git status --porcelain)"
files=$(gofmt -l .)
test -z "$files" || { printf '%s\n' "$files"; exit 1; }
go build -buildvcs=true -trimpath -o "$PREP/devatlas" ./cmd/devatlas
go vet ./...
go test ./...
CGO_ENABLED=1 go test -race ./...
"$PREP/devatlas" version
python3 "$PREP/smoke-web.py" "$PREP/devatlas" --sha "$APP_SHA"
sha256sum "$PREP/devatlas" > "$PREP/candidate.sha256"
git switch chore/console-rollout
```

No dependency or source replacement is permitted to obtain passing checks.
Verify GitHub CI for this SHA as well. Smoke uses temporary fixtures and port 0
(an atomically assigned free loopback port), never 18642. It stops only its own
child process and checks that fixture files remain unchanged. It does not prove
Nginx authentication, real project visibility, browser appearance or systemd
restart behavior. Build and test on the target before approving cutover.

For a browser check with REAL paths, after examining `before.json`, launch the
candidate as rytis in a foreground terminal with `-port 0`, the exact discovered
`-config` path and `-state-dir` path. Read its allocated URL from stdout.
Do not expose this listener publicly or modify Nginx for a test. If no browser
can reach loopback, record visual QA as pending; do not call HTTP checks visual QA.

## 3. Prepare environment and rollback inputs (no service changes)

The unit runs as rytis, keeps the web port and uses `/opt/devatlas/current/devatlas`.
The operator supplies the exact environment file from the verified snapshot:

```sh
python3 - "$PREP" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
s = json.loads((p / 'before.json').read_text())
assert s['hostname'].split('.')[0] == 'vaicaicio4' and s['user'] == 'rytis'
assert s['state_exists'], 'Confirm the real runner state directory first'
for value in (s['config'], s['state_dir']):
    assert value.startswith('/') and not any(c in value for c in '\n\r\0')
(p / 'web.env').write_text('DEVATLAS_CONFIG=' + json.dumps(s['config'], ensure_ascii=False) + '\nDEVATLAS_STATE_DIR=' + json.dumps(s['state_dir'], ensure_ascii=False) + '\n')
print((p / 'web.env').read_text())
PY
cat "$PREP/devatlas-web.service"
```

The unit template is exact; only `web.env` depends on the live server paths.
Review unit hardening with the actual checkout directories. No new privileges,
Docker access or network exposure are introduced. The process still needs read
access to registry, history and registered checkouts; no ownership changes to
those directories are part of this rollout.

## 4. Cutover — ONLY after explicit approval

The following commands are a reviewable plan, not preparation instructions.
Require an administrator and a quiet cutover window. Keep `$PREP` and the prior
binary until acceptance. Refuse an existing current symlink/release/unit; this
procedure is deliberately first-migration only, not an overwrite-on-rerun script.

```sh
test "$(sudo systemctl show devatlas-web.service -p LoadState --value)" = not-found
sudo test ! -e /opt/devatlas/current
sudo test ! -L /opt/devatlas/current
sudo test ! -e /opt/devatlas/rollback
sudo test ! -e /etc/devatlas/web.env
sudo test ! -e "/opt/devatlas/releases/$APP_SHA"
sha256sum -c "$PREP/candidate.sha256"
sudo python3 "$PREP/inspect-web.py" > "$PREP/immediate.json"
python3 - "$PREP" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
a, b = [json.loads((p / n).read_text()) for n in ('before.json', 'immediate.json')]
for key in ('pid', 'start_ticks', 'executable_sha256', 'argv', 'cwd', 'environment', 'config', 'state_dir', 'cgroup'):
    assert a[key] == b[key], 'Reinspect changed setting: ' + key
PY
sudo install -d -m 0755 /opt/devatlas/releases /etc/devatlas
sudo install -d -m 0755 /opt/devatlas/rollback
sudo install -d -m 0755 "/opt/devatlas/releases/$APP_SHA"
sudo install -m 0755 "$PREP/devatlas" "/opt/devatlas/releases/$APP_SHA/devatlas"
sudo install -m 0600 "$PREP/web.env" /etc/devatlas/web.env
sudo install -m 0600 "$PREP/before.json" /opt/devatlas/rollback/before.json
sudo install -m 0644 "$PREP/devatlas-web.service" /etc/systemd/system/devatlas-web.service
sudo ln -s "/opt/devatlas/releases/$APP_SHA" /opt/devatlas/current
sudo systemd-analyze verify /etc/systemd/system/devatlas-web.service
```

Back up the RUNNING executable and signal only the same pinned process. Linux
pidfd avoids killing a different process if the PID is recycled. If unavailable,
stop and revise the plan; do not substitute `pkill`, `killall` or stop SSH.

```sh
sudo python3 - <<'PY'
import hashlib, json, os, pathlib, signal
p = pathlib.Path('/opt/devatlas/rollback')
s = json.loads((p / 'before.json').read_text())
fd = os.pidfd_open(s['pid'])
try:
    proc = pathlib.Path('/proc') / str(s['pid'])
    assert proc.stat().st_uid == s['uid']
    assert (proc / 'stat').read_text().rsplit(')', 1)[1].split()[19] == s['start_ticks']
    assert (proc / 'cmdline').read_bytes().rstrip(b'\0').decode().split('\0') == s['argv']
    data = (proc / 'exe').read_bytes()
    assert hashlib.sha256(data).hexdigest() == s['executable_sha256']
    with (p / 'devatlas').open('xb') as target:
        target.write(data)
        target.flush()
        os.fsync(target.fileno())
    (p / 'devatlas').chmod(0o755)
    signal.pidfd_send_signal(fd, signal.SIGTERM)
finally:
    os.close(fd)
PY
```

Wait for the old listener to exit (up to its shutdown timeout); if it stays,
abort and diagnose, never kill another listener. Then:

```sh
sudo systemctl daemon-reload
sudo systemctl start devatlas-web.service
curl --fail --retry 10 --retry-connrefused --retry-delay 1 --max-time 5 http://127.0.0.1:18642/health
sudo systemctl status devatlas-web.service --no-pager
```

Require health `status=ok`, `service=devatlas`, commit `9b92a6127d72`, modified
false. On start/readiness failure run the rollback below immediately. Before
enabling boot startup, verify authenticated `/`, `/runs`, a real run detail,
`/projects`, and `/health`; inspect real reviewer/tester records and projects.
Unauthenticated console requests must get the established authentication
challenge; raw log/JSON subroutes must return 404 after authentication. Use
interactive `curl -u USERNAME` or a browser; never put passwords in commands.
Do not require new authentication on pre-existing public health/OAuth routes.

After successful checks, test `sudo systemctl restart devatlas-web.service`
and repeat health/real-data checks, then `sudo systemctl enable devatlas-web.service`.
This planned restart is part of cutover acceptance, not preparation testing.
Nginx, MCP, Keycloak and ssh.service require no changes or restarts.

## 5. Rollback (after a failed approved cutover)

Stop ONLY the new web unit. Restore the backed-up executable with its exact old
argv, cwd and path-resolution environment in a separate transient service:

```sh
sudo systemctl disable --now devatlas-web.service
sudo python3 - <<'PY'
import hashlib, json, pathlib, subprocess
p = pathlib.Path('/opt/devatlas/rollback')
s = json.loads((p / 'before.json').read_text())
assert hashlib.sha256((p / 'devatlas').read_bytes()).hexdigest() == s['executable_sha256']
assert pathlib.Path(s['cwd']).is_dir()
cmd = ['systemd-run', '--unit=devatlas-web-rollback', '--service-type=exec',
       '--uid=' + s['user'], '--working-directory=' + s['cwd'],
       '--property=Restart=on-failure', '--property=TimeoutStopSec=20']
for key, value in s['environment'].items():
    cmd.append('--setenv=' + key + '=' + value)
subprocess.run(cmd + ['--', str(p / 'devatlas')] + s['argv'][1:], check=True)
PY
curl --fail --retry 10 --retry-connrefused --retry-delay 1 --max-time 5 http://127.0.0.1:18642/health
sudo systemctl status devatlas-web-rollback.service --no-pager
```

Require the old health identity from `before.json`, then recheck authenticated
pages. Registry, state files and Nginx are never restored from stale copies:
they were not modified. The rollback unit is transient (not boot-persistent);
do not reboot until a persistent replacement has been reviewed. Re-running the
first-migration procedure must stop at existing-artifact guards. If rollback
already exists, inspect it rather than replacing it.

## Evidence and unresolved gates

Cloud Linux, Go 1.27.3: approved clean application binary reports
`9b92a6127d72`; fixture smoke passed on a separate ephemeral loopback port.
The read-only inventory also passed against a real temporary child process:
PID, selected paths and executable hash matched. Python syntax and all eight
documented shell blocks passed syntax checks. `systemd-analyze verify` passed
with only ExecStart's binary path substituted for the temporary candidate.
The exact production unit cannot verify locally until `/opt/devatlas/current`
exists; no production directory was created to make that check pass.
This is HTTP verification, not visual QA or target-server verification.
Current target process, config, paths, permissions, Nginx authentication,
systemd lifecycle and rollback execution must still be verified on vaicaicio4.
This environment is not vaicaicio4, has no systemd PID 1, and its SSH client
configuration fails ownership/permission validation before connecting. No
server settings were changed to bypass that restriction.
