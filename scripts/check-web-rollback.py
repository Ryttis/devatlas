#!/usr/bin/env python3
"""Patch/check the operator's uncommitted deploy-web.sh, without real cutover.

Defaults to check-only. --apply backs up and updates that file only after tests.
No sudo, systemd operation, production HTTP request or production signal is used.
"""
import argparse
import difflib
import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile


HARNESS = r'''
set -euo pipefail
source "$TEST_SCRIPT"
mock_sudo() { if [ "${1:-}" = -v ]; then return 0; fi; "$@"; }
mock_systemctl() {
    printf '%s\n' "systemctl:$*" >> "$EVENTS"
    if [ "$1" = restart ] && [ "$SCENARIO" = restart-fails ]; then return 42; fi
}
# Any unexpected production-facing operation aborts the rehearsal.
sudo() { echo 'unexpected sudo' >&2; exit 97; }
systemctl() { echo 'unexpected systemctl' >&2; exit 97; }
curl() { echo 'unexpected curl' >&2; exit 97; }
ss() { echo 'unexpected ss' >&2; exit 97; }
kill() { echo 'unexpected kill' >&2; exit 97; }
SUDO=mock_sudo
SYSTEMCTL=mock_systemctl
release_ok() { return 0; }
cmd_unit() { echo fixture-unit; }
listener_pid() { echo "$FIXTURE_PID"; }
unit_pid() { echo 0; }
is_devatlas_web() { return 0; }
wait_port_free() { return 0; }
port_free() { return 0; }
wait_ready() { return 1; }
cmd_verify() { echo 'unexpected verification' >&2; exit 97; }

if [ "$SCENARIO" = pidfd-mismatch ]; then
    rc=0
    pidfd_term "$FIXTURE_PID" 0 "$(printf '%064d' 0)" 0.1 || rc=$?
    [ "$rc" = 3 ] || exit 98
    exit 0
fi

if [ "$SCENARIO" = bad-hash ] || [ "$SCENARIO" = missing-cwd ]; then
    mkdir -p "$LEGACY"
    printf 'fixture\0serve\0' > "$LEGACY/cmdline"
    printf 'HOME=/nonexistent\0' > "$LEGACY/environ"
    printf '%s\n' "$ROOT/missing" > "$LEGACY/cwd"
    printf 'not an executable\n' > "$LEGACY/devatlas"
    if [ "$SCENARIO" = bad-hash ]; then
        printf '%064d  devatlas\n' 0 > "$LEGACY/SHA256"
    else
        (cd "$LEGACY" && sha256sum devatlas > SHA256)
    fi
    # Even if validation regresses, no saved executable can be started.
    setsid() { echo 'unexpected launch' >> "$EVENTS"; return 97; }
    if start_legacy; then exit 98; fi
    exit 0
fi

# Only these rehearsal functions substitute legacy capture/termination/relaunch.
record_legacy() {
    mkdir -p "$LEGACY"
    printf 'fixture\0serve\0' > "$LEGACY/cmdline"
    echo 123 > "$LEGACY/starttime"
}
recorded_sha256() { printf '%064d\n' 0; }
pidfd_term() { echo simulated-term >> "$EVENTS"; return 0; }
start_legacy() { echo restored-legacy >> "$EVENTS"; return 0; }
cmd_cutover aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
'''


def patch(text):
    required = ('trap on_exit EXIT', 'pidfd_term()', 'record_legacy()',
                'os.pidfd_open', 'signal.pidfd_send_signal',
                '[ "${BASH_SOURCE[0]}" = "$0" ] || return 0')
    if not all(item in text for item in required):
        raise RuntimeError('Unexpected script version; refusing to patch or source it')
    pattern = r'(?m)^[ \t]*if \[ ! -d "\$cwd" \]; then\s+say "WARN[^\n]*\$cwd[^\n]*"\s+cwd=\$LEGACY\s+fi'
    replacement = ('\tif [ ! -d "$cwd" ]; then\n'
                   '\t\tsay "original working directory $cwd no longer exists; refusing rollback with a different cwd"\n'
                   '\t\treturn 1\n\tfi')
    result, count = re.subn(pattern, lambda _: replacement, text)
    if count != 1 and 'refusing rollback with a different cwd' not in text:
        raise RuntimeError('Expected exactly one legacy cwd fallback; nothing changed')
    # Some pasted logs contain Markdown escapes; normalize only a literal shell
    # backslash before review.json, if it really exists in the source file.
    return result.replace(r'review\.json', 'review.json')


def tests(script, root):
    subprocess.run(['bash', '-n', str(script)], check=True)
    for scenario in ('restart-fails', 'readiness-fails', 'bad-hash', 'missing-cwd', 'pidfd-mismatch'):
        case = root / scenario
        case.mkdir()
        registry = case / 'unchanged-registry.yaml'
        registry.write_text('fixture registry — must not be changed\n')
        state = case / 'state'
        state.mkdir()
        history = state / 'unchanged.json'
        history.write_text('{"fixture":true}\n')
        unit = case / 'unit'
        unit.write_text('fixture-unit\n')
        events = case / 'events'
        events.touch()
        # This child exists solely to supply a real /proc/cmdline containing
        # -config for cmd_cutover. pidfd_term is mocked: it is never signalled.
        child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)',
                                  '-config', str(registry)])
        try:
            env = dict(os.environ, TEST_SCRIPT=str(script), SCENARIO=scenario,
                       DEVATLAS_DEPLOY_ROOT=str(case), DEVATLAS_REGISTRY=str(registry),
                       DEVATLAS_STATE_DIR=str(state), DEVATLAS_UNIT_PATH=str(unit),
                       DEVATLAS_PORT='19876', FIXTURE_PID=str(child.pid), EVENTS=str(events))
            result = subprocess.run(['bash', '-s'], input=HARNESS, env=env,
                                    text=True, capture_output=True, timeout=5)
            log = events.read_text()
            if scenario.endswith('fails'):
                assert result.returncode == 1, (scenario, result.stdout, result.stderr)
                assert 'systemctl:restart devatlas-web.service' in log, log
                assert 'systemctl:stop devatlas-web.service' in log, log
                assert 'simulated-term' in log and 'restored-legacy' in log, log
                assert 'ROLLED BACK:' in result.stdout, result.stdout
            else:
                assert result.returncode == 0, (scenario, result.stdout, result.stderr)
                expected = {'bad-hash': 'recorded SHA256',
                            'missing-cwd': 'refusing rollback with a different cwd',
                            'pidfd-mismatch': 'not signalled'}[scenario]
                assert expected in result.stdout + result.stderr, result.stdout + result.stderr
                assert not log, log
            assert registry.read_text() == 'fixture registry — must not be changed\n'
            assert history.read_text() == '{"fixture":true}\n'
            assert child.poll() is None, 'rehearsal child was unexpectedly terminated'
            print('PASS:', scenario, '(isolated mocks; fixture data unchanged)')
        finally:
            child.terminate()
            child.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('script', type=Path)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    if os.getuid() == 0:
        parser.error('run as the unprivileged operator, without sudo')
    target = args.script.resolve(strict=True)
    original = target.read_bytes()
    changed = patch(original.decode()).encode()
    print(''.join(difflib.unified_diff(original.decode().splitlines(True),
                                     changed.decode().splitlines(True),
                                     fromfile='a/scripts/deploy-web.sh',
                                     tofile='b/scripts/deploy-web.sh')), end='')
    with tempfile.TemporaryDirectory(prefix='devatlas-rollback-check-') as temp:
        root = Path(temp)
        candidate = root / 'deploy-web.sh'
        candidate.write_bytes(changed)
        tests(candidate, root)
    if not args.apply:
        print('CHECK ONLY: target unchanged; proposed patch tested')
        return
    if original == changed:
        print('Already patched; target unchanged')
        return
    if target.read_bytes() != original:
        raise RuntimeError('Target changed during tests; refusing overwrite')
    backup = target.with_name(target.name + '.before-rollback-' + hashlib.sha256(original).hexdigest()[:12])
    with backup.open('xb') as stream:
        stream.write(original)
    shutil.copystat(target, backup)
    # Preserve the file's mode and use same-directory atomic replacement.
    fd, name = tempfile.mkstemp(prefix=target.name + '.', dir=target.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(changed)
            stream.flush()
            os.fsync(stream.fileno())
        shutil.copystat(target, name)
        os.replace(name, target)
    finally:
        if os.path.exists(name):
            os.unlink(name)
    print('UPDATED:', target)
    print('BACKUP:', backup)
    print('No unit installation, cutover, merge or deployment performed.')


if __name__ == '__main__':
    main()
