#!/usr/bin/env bash
# Git-based build, candidate check, cutover and rollback for devatlas-web.service.
# See docs/DEPLOYMENT.md. Run as the unprivileged operator, never as root.
#
# Only `cutover` and `rollback` change the running server, and only they call
# sudo (for systemctl on devatlas-web.service). Nothing here touches ssh,
# nginx, the MCP connector or Keycloak.
set -euo pipefail

ROOT=${DEVATLAS_DEPLOY_ROOT:-$HOME/devatlas-web}
REPO=${DEVATLAS_REPO_URL:-https://github.com/Ryttis/devatlas.git}
REGISTRY=${DEVATLAS_REGISTRY:-$HOME/.config/devatlas/registrations.yaml}
STATE_DIR=${DEVATLAS_STATE_DIR:-$HOME/.local/state/devatlas}
PORT=${DEVATLAS_PORT:-18642}
PUBLIC_HOST=${DEVATLAS_PUBLIC_HOST:-devatlas.etno-lentos.com}
UNIT=devatlas-web.service
UNIT_PATH=${DEVATLAS_UNIT_PATH:-/etc/systemd/system/$UNIT}
SUDO=${DEVATLAS_SUDO:-sudo}
SYSTEMCTL=${DEVATLAS_SYSTEMCTL:-systemctl} # overridden only by rehearsals
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

say() { printf '%s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
need_sha() { [[ ${1:-} =~ ^[0-9a-f]{40}$ ]] || die "a full 40-character commit SHA is required"; }
[ "$(id -u)" -ne 0 ] || die "run as the unprivileged operator, not root"

# --- observation helpers (read-only) ---------------------------------------

listener_pid() { # PID listening on 127.0.0.1:$1, empty if none or not ours
	ss -H -ltnp "sport = :$1" 2>/dev/null | sed -n 's/.*pid=\([0-9]\+\).*/\1/p' | head -n1
}
unit_pid() {
	local pid
	pid=$("$SYSTEMCTL" show -p MainPID --value "$UNIT" 2>/dev/null || true)
	[ -n "$pid" ] && [ "$pid" != 0 ] && say "$pid" || true
}
is_devatlas_web() { # exact process targeting: our user, devatlas binary, serve on $PORT
	local pid=$1
	[ -d "/proc/$pid" ] || return 1
	[ "$(stat -c %u "/proc/$pid")" = "$(id -u)" ] || return 1
	[ "$(basename "$(readlink "/proc/$pid/exe")" | sed 's/ (deleted)$//')" = devatlas ] || return 1
	tr '\0' ' ' <"/proc/$pid/cmdline" | grep -Eq "^[^ ]*devatlas serve .*-port $PORT( |$)"
}
health() { curl -fsS --max-time 5 "http://127.0.0.1:$1/health"; }
health_field() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], ""))' "$1"; }
code() { curl -sS -o /dev/null --max-time 10 -w '%{http_code}' "$@" || true; }
wait_ready() { # port, expected 12-char commit, seconds
	local i out
	for ((i = 0; i < $3 * 2; i++)); do
		if out=$(health "$1" 2>/dev/null) &&
			[ "$(health_field status <<<"$out")" = ok ] &&
			[ "$(health_field commit <<<"$out")" = "$2" ] &&
			[ "$(health_field modified <<<"$out")" = False ]; then
			return 0
		fi
		sleep 0.5
	done
	return 1
}
binary_revision() { go version -m "$1" | awk '$1 == "build" && $2 ~ /^vcs\.revision=/ {sub(/^vcs\.revision=/, "", $2); print $2}'; }
binary_clean() { go version -m "$1" | grep -q 'vcs\.modified=false'; }
release_ok() { [ -x "$ROOT/releases/$1/devatlas" ] && [ "$(binary_revision "$ROOT/releases/$1/devatlas")" = "$1" ] && binary_clean "$ROOT/releases/$1/devatlas"; }
check() { # label, expected, observed
	if [ "$2" = "$3" ]; then say "PASS  $1 ($3)"; else say "FAIL  $1: expected $2, observed $3"; FAILED=1; fi
}

# --- unit ------------------------------------------------------------------

cmd_unit() {
	sed -e "s|@USER@|$(id -un)|g" -e "s|@GROUP@|$(id -gn)|g" -e "s|@HOME@|$HOME|g" \
		-e "s|@DEPLOY_ROOT@|$ROOT|g" -e "s|@REGISTRY@|$REGISTRY|g" \
		-e "s|@STATE_DIR@|$STATE_DIR|g" -e "s|@PORT@|$PORT|g" \
		"$HERE/../deploy/devatlas-web.service.in" | grep -v '^#'
}

# --- build -----------------------------------------------------------------

ci_passed() { # every GitHub check run on the commit completed successfully
	local slug
	slug=$(sed -E 's#^(git@github\.com:|https://github\.com/)##; s#\.git$##' <<<"$REPO")
	curl -fsS --max-time 20 -H 'Accept: application/vnd.github+json' \
		"https://api.github.com/repos/$slug/commits/$1/check-runs" |
		python3 -c '
import json, sys
runs = json.load(sys.stdin).get("check_runs", [])
for r in runs:
    print("  check %s: %s/%s" % (r["name"], r["status"], r["conclusion"]))
sys.exit(0 if runs and all(r["status"] == "completed" and r["conclusion"] == "success" for r in runs) else 1)'
}

cmd_build() {
	need_sha "${1:-}"
	local sha=$1 src=$ROOT/src out=$ROOT/releases/$1
	command -v go >/dev/null || die "go is not on PATH"
	if release_ok "$sha"; then
		say "release $sha already built: $out/devatlas"
		return 0
	fi
	mkdir -p "$ROOT/releases"
	chmod 700 "$ROOT"
	if [ ! -d "$src/.git" ]; then
		git clone --quiet "$REPO" "$src"
	fi
	[ "$(git -C "$src" remote get-url origin)" = "$REPO" ] || die "$src origin is not $REPO"
	# The deployment checkout must never hold work; refuse rather than discard it.
	[ -z "$(git -C "$src" status --porcelain 2>/dev/null)" ] || die "$src has uncommitted changes; resolve them by hand"
	git -C "$src" fetch --quiet --prune origin '+refs/heads/*:refs/remotes/origin/*' '+refs/pull/*/head:refs/remotes/origin/pr/*'
	git -C "$src" cat-file -e "$sha^{commit}" 2>/dev/null || die "$sha was not found on GitHub ($REPO)"
	[ -n "$(git -C "$src" branch -r --contains "$sha")" ] || die "$sha is not reachable from any GitHub branch or pull request"
	say "GitHub checks for $sha:"
	ci_passed "$sha" || die "GitHub checks for $sha are missing, unfinished or failed"
	git -C "$src" checkout --quiet --detach "$sha"
	[ "$(git -C "$src" rev-parse HEAD)" = "$sha" ] || die "checkout is not at $sha"
	(
		cd "$src"
		files=$(gofmt -l .)
		[ -z "$files" ] || die "gofmt: $files"
		go vet ./...
		go test ./...
		CGO_ENABLED=1 go test -race ./...
		mkdir -p "$out"
		go build -trimpath -o "$out/devatlas.new" ./cmd/devatlas
	)
	[ "$(binary_revision "$out/devatlas.new")" = "$sha" ] && binary_clean "$out/devatlas.new" ||
		die "built binary is not stamped with clean revision $sha"
	mv -f "$out/devatlas.new" "$out/devatlas"
	(cd "$out" && sha256sum devatlas >SHA256)
	say "built $out/devatlas"
	"$out/devatlas" version
	cat "$out/SHA256"
}

# --- candidate on a temporary port -----------------------------------------

cmd_candidate() {
	need_sha "${1:-}"
	local sha=$1 port=${2:-18652} before
	release_ok "$sha" || die "release $sha is not built; run: $0 build $sha"
	[ "$port" != "$PORT" ] || die "the candidate port must differ from $PORT"
	[ -z "$(ss -H -ltn "sport = :$port")" ] || die "port $port is already in use"
	before=$(listener_pid "$PORT")
	CAND_TMP=$(mktemp -d)
	local tmp=$CAND_TMP
	# The candidate reads a private copy of the registry and the real, read-only
	# runner state, so it shows the same history without touching production files.
	[ -f "$REGISTRY" ] && cp -p "$REGISTRY" "$tmp/registrations.yaml"
	"$ROOT/releases/$sha/devatlas" serve -host 127.0.0.1 -port "$port" \
		-config "$tmp/registrations.yaml" -state-dir "$STATE_DIR" >"$tmp/candidate.log" 2>&1 &
	CAND_PID=$!
	trap 'kill "$CAND_PID" 2>/dev/null || true; wait "$CAND_PID" 2>/dev/null || true; rm -rf "$CAND_TMP"' EXIT
	FAILED=0
	if wait_ready "$port" "${sha:0:12}" 15; then say "PASS  /health reports commit ${sha:0:12}, unmodified"; else
		say "FAIL  candidate did not become ready"
		cat "$tmp/candidate.log"
		exit 1
	fi
	local base=http://127.0.0.1:$port
	for path in / /runs /projects /api/projects; do check "GET $path" 200 "$(code "$base$path")"; done
	# A real run detail: prefer a listed run whose review.json exists on disk, so
	# the raw-file check below asks for a file that is really there.
	local id run="" task
	for id in $(curl -fsS --max-time 10 "$base/runs?kind=run" | grep -oE 'run-[A-Za-z0-9_-]{6,64}' | sort -u); do
		[ -n "$run" ] || run=$id
		if [ -f "$STATE_DIR/agent-runs/$id/review.json" ]; then
			run=$id
			break
		fi
	done
	task=$(curl -fsS --max-time 10 "$base/runs?kind=workflow" | grep -oE 'task-[A-Za-z0-9_-]{6,64}' | head -n1 || true)
	if [ -n "$run" ]; then
		check "GET /runs/$run (run detail)" 200 "$(code "$base/runs/$run")"
		if [ -f "$STATE_DIR/agent-runs/$run/review.json" ]; then
			check "GET /runs/$run/review.json (file exists on disk, no raw route)" 404 "$(code "$base/runs/$run/review.json")"
		else
			say "SKIP  raw review.json of an existing file: $run has no review.json on disk"
		fi
		check "GET /runs/$run/state.json (no raw route)" 404 "$(code "$base/runs/$run/state.json")"
	else
		say "SKIP  run detail: no saved runs in $STATE_DIR/agent-runs"
	fi
	if [ -n "$task" ]; then check "GET /runs/$task (workflow detail)" 200 "$(code "$base/runs/$task")"; else say "SKIP  workflow detail: none saved"; fi
	check "GET /runs/run-missing" 404 "$(code "$base/runs/run-missing")"
	check "GET /runs/run-missing/review.json" 404 "$(code "$base/runs/run-missing/review.json")"
	check "POST /runs (read-only)" 405 "$(code -X POST "$base/runs")"
	check "production listener on $PORT untouched" "${before:-none}" "$(listener_pid "$PORT" | grep . || say none)"
	[ "$FAILED" = 0 ] || die "candidate checks failed"
	say "candidate $sha passed on 127.0.0.1:$port and was stopped"
}

# --- status and verification -----------------------------------------------

cmd_status() {
	local pid
	pid=$(listener_pid "$PORT")
	if [ -z "$pid" ]; then
		say "listener $PORT: none"
	else
		say "listener $PORT: pid $pid exe $(readlink "/proc/$pid/exe") cgroup $(cut -d: -f3 "/proc/$pid/cgroup")"
		say "  managed by $UNIT: $([ "$pid" = "$(unit_pid)" ] && say yes || say no)"
		health "$PORT" | tr -d '\n' || true
		say
	fi
	say "unit: $("$SYSTEMCTL" is-enabled "$UNIT" 2>/dev/null || true) / $("$SYSTEMCTL" is-active "$UNIT" 2>/dev/null || true)"
	say "current:  $(readlink "$ROOT/current" 2>/dev/null || say none)"
	say "previous: $(readlink "$ROOT/previous" 2>/dev/null || say none)"
	say "registry: $REGISTRY ($([ -f "$REGISTRY" ] && say present || say absent))"
	say "state:    $STATE_DIR ($(find "$STATE_DIR" -mindepth 2 -maxdepth 2 -type d 2>/dev/null | wc -l) records)"
}

cmd_verify() { # [expected full sha]; authenticated checks need DEVATLAS_WEB_NETRC
	FAILED=0
	local out https=https://$PUBLIC_HOST resolve=(--resolve "$PUBLIC_HOST:443:127.0.0.1")
	out=$(health "$PORT") || die "no healthy listener on $PORT"
	check "local /health status" ok "$(health_field status <<<"$out")"
	[ -z "${1:-}" ] || check "running commit" "${1:0:12}" "$(health_field commit <<<"$out")"
	for path in / /runs /projects; do
		check "unauthenticated $https$path is challenged" 401 "$(code "${resolve[@]}" "$https$path")"
	done
	check "nginx /healthz" 200 "$(code "${resolve[@]}" "$https/healthz")"
	check "MCP route still demands a token" 401 "$(code "${resolve[@]}" "$https/mcp")"
	if [ -n "${DEVATLAS_WEB_NETRC:-}" ]; then
		for path in / /runs /projects /health; do
			check "authenticated $https$path" 200 "$(code "${resolve[@]}" --netrc-file "$DEVATLAS_WEB_NETRC" "$https$path")"
		done
	else
		say "SKIP  authenticated pages: set DEVATLAS_WEB_NETRC to a netrc file for $PUBLIC_HOST"
	fi
	[ "$FAILED" = 0 ] || die "verification failed"
}

# --- cutover and rollback (change the running server) ----------------------

LEGACY=$ROOT/rollback/legacy
RESTORE="" # what on_exit must bring back: "", none, release or legacy
OLD=""     # release directory that served before this cutover
TARGET=""

switch_current() { # atomically point current at a release directory
	ln -sfn "$1" "$ROOT/current.new" && mv -Tf "$ROOT/current.new" "$ROOT/current"
}
port_free() { [ -z "$(ss -H -ltn "sport = :$PORT")" ]; }
wait_port_free() { # seconds
	local i
	for ((i = 0; i < $1 * 2; i++)); do
		port_free && return 0
		sleep 0.5
	done
	return 1
}
proc_starttime() { sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20; }
exe_sha256() { sha256sum <"/proc/$1/exe" 2>/dev/null | cut -d' ' -f1; }
recorded_sha256() { cut -d' ' -f1 "$LEGACY/SHA256"; }

# Signal one process through a pidfd. The identity (start time, owner, binary
# hash, "serve ... -port PORT") is re-read after the pidfd is open: if the
# process has exited and its PID was reused, those reads may describe another
# process, but the signal still goes only to the original one and fails
# harmlessly. Never SIGKILL. Exit: 0 exited, 3 not signalled, 4 still running.
pidfd_term() { # pid starttime sha256 timeout
	python3 - "$1" "$2" "$3" "$PORT" "$4" <<'PY'
import hashlib, os, select, signal, sys

pid, start, sha, port, timeout = int(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], float(sys.argv[5])
try:
    fd = os.pidfd_open(pid)
except OSError as err:
    print("pidfd_open(%d): %s" % (pid, err), file=sys.stderr)
    sys.exit(3)
try:
    with open("/proc/%d/stat" % pid) as f:
        now = f.read().rsplit(")", 1)[1].split()[19]
    with open("/proc/%d/exe" % pid, "rb") as f:
        digest = hashlib.sha256(f.read()).hexdigest()
    with open("/proc/%d/cmdline" % pid, "rb") as f:
        argv = f.read().split(b"\0")[:-1]
    owner = os.stat("/proc/%d" % pid).st_uid
except OSError as err:
    print("pid %d cannot be inspected: %s" % (pid, err), file=sys.stderr)
    sys.exit(3)
want = port.encode()
serves = len(argv) > 1 and argv[1] == b"serve" and any(
    a == b"-port" and argv[i + 1:i + 2] == [want] for i, a in enumerate(argv))
problems = [name for name, ok in (
    ("start time", now == start), ("owner", owner == os.getuid()),
    ("binary sha256", digest == sha), ("command line", serves),
    ("alive", not select.select([fd], [], [], 0)[0])) if not ok]
if problems:
    print("pid %d is not the recorded process (%s); not signalled" % (pid, ", ".join(problems)), file=sys.stderr)
    sys.exit(3)
signal.pidfd_send_signal(fd, signal.SIGTERM)
sys.exit(0 if select.select([fd], [], [], timeout)[0] else 4)
PY
}

record_legacy() { # pid: save what rollback needs, all-or-nothing
	local pid=$1 new=$LEGACY.new
	rm -rf "$new"
	mkdir -p "$new"
	chmod 700 "$ROOT/rollback" "$new"
	cat "/proc/$pid/cmdline" >"$new/cmdline"
	readlink "/proc/$pid/cwd" >"$new/cwd"
	proc_starttime "$pid" >"$new/starttime"
	health "$PORT" | health_field commit >"$new/commit"
	# Only the environment the process needs; session and agent variables are
	# not persisted. Values may hold paths, so the file stays private.
	python3 - "$pid" >"$new/environ" <<'PY'
import re, sys
keep = re.compile(rb"^(HOME|USER|LOGNAME|PATH|LANG|LANGUAGE|LC_[A-Z_]+|TZ|TMPDIR|XDG_(CONFIG|STATE|DATA|CACHE)_HOME|XDG_(CONFIG|DATA)_DIRS|DEVATLAS_[A-Z0-9_]+|GIT_[A-Z0-9_]+)=")
with open("/proc/%s/environ" % sys.argv[1], "rb") as f:
    for item in f.read().split(b"\0"):
        if keep.match(item):
            sys.stdout.buffer.write(item + b"\0")
PY
	cat "/proc/$pid/exe" >"$new/devatlas"
	chmod 700 "$new/devatlas"
	(cd "$new" && sha256sum devatlas >SHA256)
	chmod 600 "$new"/{cmdline,cwd,starttime,commit,environ,SHA256}
	# The copy must be the binary that is running, and the process must not
	# have been replaced while it was being recorded.
	[ "$(cut -d' ' -f1 "$new/SHA256")" = "$(exe_sha256 "$pid")" ] || die "the saved binary differs from the running one"
	[ "$(cat "$new/starttime")" = "$(proc_starttime "$pid")" ] || die "pid $pid changed while it was being recorded"
	[ -n "$(cat "$new/commit")" ] || die "the running process did not report its commit"
	rm -rf "$LEGACY.old"
	[ ! -d "$LEGACY" ] || mv "$LEGACY" "$LEGACY.old"
	mv "$new" "$LEGACY"
}

start_legacy() { # relaunch the recorded process from the saved, verified binary
	local cwd pid
	[ -f "$LEGACY/cmdline" ] && [ -f "$LEGACY/SHA256" ] && [ -f "$LEGACY/environ" ] || {
		say "no complete legacy record in $LEGACY"
		return 1
	}
	(cd "$LEGACY" && sha256sum --status -c SHA256) || {
		say "the saved legacy binary does not match its recorded SHA256; refusing to run it"
		return 1
	}
	cwd=$(cat "$LEGACY/cwd")
	if [ ! -d "$cwd" ]; then
		say "original working directory $cwd no longer exists; refusing rollback with a different cwd"
		return 1
	fi
	# New session; execve with exactly the recorded argv (including argv[0]) and
	# environment, so nothing from this shell leaks into the restored process.
	(cd "$cwd" && exec setsid python3 -c '
import os, sys
rec = sys.argv[1]
def items(name):
    with open(os.path.join(rec, name), "rb") as f:
        return f.read().split(b"\0")[:-1]
env = dict(item.split(b"=", 1) for item in items("environ"))
os.execve(os.path.join(rec, "devatlas").encode(), items("cmdline"), env)
' "$LEGACY" >>"$LEGACY/server.log" 2>&1 </dev/null &)
	wait_ready "$PORT" "$(cat "$LEGACY/commit")" 15 || {
		say "the legacy process did not become healthy; inspect $LEGACY/server.log"
		return 1
	}
	pid=$(listener_pid "$PORT")
	[ -n "$pid" ] && [ "$(exe_sha256 "$pid")" = "$(recorded_sha256)" ] || {
		say "port $PORT is not served by the saved legacy binary"
		return 1
	}
	say "restored legacy process pid $pid (commit $(cat "$LEGACY/commit"), sha256 $(recorded_sha256)) on $PORT"
}

restore_prior() { # best effort, never aborts half way; 0 only if the prior service answers
	local pid
	say "restoring what served before ($RESTORE)"
	$SUDO "$SYSTEMCTL" stop "$UNIT" || say "WARN  systemctl stop $UNIT returned an error"
	case $RESTORE in
	release)
		switch_current "$OLD" || say "WARN  could not switch current back to $OLD"
		$SUDO "$SYSTEMCTL" restart "$UNIT" || say "WARN  systemctl restart $UNIT returned an error"
		wait_ready "$PORT" "$(basename "$OLD" | cut -c1-12)" 20 || return 1
		say "restored release $(basename "$OLD") under $UNIT"
		;;
	legacy)
		rm -f "$ROOT/current"
		if ! wait_port_free 20; then
			pid=$(listener_pid "$PORT")
			if [ -n "$pid" ] && [ "$(proc_starttime "$pid")" = "$(cat "$LEGACY/starttime")" ]; then
				say "the original process pid $pid still holds port $PORT; nothing to restore"
				return 0
			fi
			say "port $PORT is still held by pid ${pid:-unknown}; cannot start the legacy process"
			return 1
		fi
		start_legacy || return 1
		;;
	none)
		rm -f "$ROOT/current"
		say "nothing was serving before; $UNIT left stopped"
		;;
	esac
}

# Once RESTORE is set, every way out of the script (a failing command under
# set -e, die, or a signal) passes through here and restores the prior service.
on_exit() {
	local rc=$?
	trap - EXIT INT TERM HUP
	set +e
	if [ -n "$RESTORE" ]; then
		say "cutover to $TARGET did not complete (exit $rc)"
		if restore_prior; then
			say "ROLLED BACK: the prior service is restored; see: journalctl -u $UNIT"
		else
			say "ROLLBACK FAILED: nothing healthy on $PORT; act by hand (docs/DEPLOYMENT.md, Rollback)"
		fi
		rc=1
	fi
	exit "$rc"
}

cmd_rollback() {
	local prev cur pid
	prev=$(readlink "$ROOT/previous" 2>/dev/null || true)
	cur=$(readlink "$ROOT/current" 2>/dev/null || true)
	if [ -n "$prev" ]; then
		[ -x "$prev/devatlas" ] || die "previous release $prev is missing"
		[ "$prev" != "$cur" ] || die "$UNIT already runs the previous release $(basename "$prev")"
		say "rolling back $UNIT to $prev"
		switch_current "$prev"
		$SUDO "$SYSTEMCTL" restart "$UNIT" || say "WARN  systemctl restart $UNIT returned an error"
		if wait_ready "$PORT" "$(basename "$prev" | cut -c1-12)" 20; then
			say "rolled back to $(basename "$prev")"
			return 0
		fi
		say "previous release is not healthy; returning to $(basename "$cur")"
		switch_current "$cur"
		$SUDO "$SYSTEMCTL" restart "$UNIT" || say "WARN  systemctl restart $UNIT returned an error"
		wait_ready "$PORT" "$(basename "$cur" | cut -c1-12)" 20 ||
			die "ROLLBACK FAILED: neither release is healthy; inspect: journalctl -u $UNIT"
		die "rollback to $(basename "$prev") failed; $(basename "$cur") is serving again"
	fi
	pid=$(listener_pid "$PORT")
	if [ -n "$pid" ] && [ "$pid" != "$(unit_pid)" ]; then
		die "port $PORT is served by pid $pid outside $UNIT and there is no previous release; nothing to roll back"
	fi
	[ -f "$LEGACY/cmdline" ] || die "no previous release and no recorded legacy process"
	say "rolling back to the recorded pre-unit process"
	$SUDO "$SYSTEMCTL" disable "$UNIT" || say "WARN  systemctl disable $UNIT returned an error"
	RESTORE=legacy
	if restore_prior; then RESTORE=""; else
		RESTORE=""
		die "ROLLBACK FAILED: nothing healthy on $PORT; inspect $LEGACY/server.log"
	fi
}

cmd_cutover() {
	need_sha "${1:-}"
	local sha=$1 pid managed=no rc legacy_registry
	TARGET=$sha
	release_ok "$sha" || die "release $sha is not built; run: $0 build $sha"
	diff -q <(cmd_unit) "$UNIT_PATH" >/dev/null 2>&1 ||
		die "$UNIT_PATH is missing or differs from '$0 unit'; install it first (docs/DEPLOYMENT.md)"
	[ -d "$STATE_DIR" ] || die "runner state directory $STATE_DIR does not exist"
	$SUDO -v
	pid=$(listener_pid "$PORT")
	[ -n "$pid" ] && [ "$pid" = "$(unit_pid)" ] && managed=yes
	OLD=$(readlink "$ROOT/current" 2>/dev/null || true)
	if [ "$managed" = yes ] && [ "$OLD" = "$ROOT/releases/$sha" ]; then
		say "$UNIT already runs $sha"
		cmd_verify "$sha"
		return 0
	fi
	trap on_exit EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM HUP
	if [ "$managed" = yes ]; then
		RESTORE=release
	elif [ -z "$pid" ]; then
		port_free || die "port $PORT is held by a process of another user"
		RESTORE=none
	else
		# First migration: the unit must keep the registry the process uses now.
		is_devatlas_web "$pid" || die "port $PORT is held by pid $pid, which is not the DevAtlas web process"
		legacy_registry=$(tr '\0' '\n' <"/proc/$pid/cmdline" | grep -A1 -x -- '-config' | tail -n1 || true)
		[ "$legacy_registry" = "$REGISTRY" ] ||
			die "the running process uses registry '${legacy_registry:-<default>}' but the unit would use '$REGISTRY'; set DEVATLAS_REGISTRY"
		record_legacy "$pid"
		say "stopping legacy pid $pid ($(tr '\0' ' ' <"$LEGACY/cmdline")), sha256 $(recorded_sha256)"
		RESTORE=legacy
		rc=0
		pidfd_term "$pid" "$(cat "$LEGACY/starttime")" "$(recorded_sha256)" 20 || rc=$?
		case $rc in
		0) ;;
		3)
			RESTORE=""
			die "pid $pid was not signalled; nothing was changed"
			;;
		*) die "pid $pid was sent SIGTERM but did not exit within 20s" ;;
		esac
		wait_port_free 5 || die "port $PORT was not released"
	fi
	switch_current "$ROOT/releases/$sha"
	$SUDO "$SYSTEMCTL" restart "$UNIT" || die "systemctl restart $UNIT returned an error"
	wait_ready "$PORT" "${sha:0:12}" 20 || die "$UNIT did not report commit ${sha:0:12} on /health within 20s"
	[ "$(listener_pid "$PORT")" = "$(unit_pid)" ] || die "port $PORT is not served by the main PID of $UNIT"
	# Committed: from here a failure is reported, not rolled back automatically.
	RESTORE=""
	trap - EXIT INT TERM HUP
	$SUDO "$SYSTEMCTL" enable "$UNIT" || say "WARN  systemctl enable $UNIT returned an error; the unit will not start at boot"
	# Only a release that actually served becomes the rollback target.
	if [ "$managed" = yes ] && [ -n "$OLD" ]; then ln -sfn "$OLD" "$ROOT/previous"; fi
	say "$UNIT is serving $sha on 127.0.0.1:$PORT"
	cmd_verify "$sha" || {
		say "post-cutover verification failed; run '$0 rollback' if it is not an authentication-setup issue"
		exit 1
	}
}

[ "${BASH_SOURCE[0]}" = "$0" ] || return 0 # sourced by tests: define functions only

case "${1:-}" in
unit | status) "cmd_$1" ;;
build | candidate | cutover | verify) "cmd_$1" "${@:2}" ;;
rollback) cmd_rollback ;;
*)
	cat <<EOF
usage: $0 <command>
  unit                     print the rendered $UNIT
  build <full-sha>         fetch from GitHub, require green checks, test and build a release
  candidate <full-sha> [port]  run the release on a temporary loopback port and check it
  status                   show the listener, unit, releases and data locations
  verify [full-sha]        check local health and the Nginx authentication boundary
  cutover <full-sha>       replace the listener on $PORT with $UNIT (uses sudo)
  rollback                 return to the previous release or the pre-unit process (uses sudo)
EOF
	exit 2
	;;
esac
