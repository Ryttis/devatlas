"""Rollback orchestration with fake systemctl; real exec only for an owned fixture."""
import hashlib
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name('deploy-web.sh').resolve()
OLD = 'a' * 40
NEW = 'b' * 40
COMMON = r'''
set -euo pipefail
sudo() { echo 'FORBIDDEN sudo' >&2; exit 97; }
systemctl() { echo 'FORBIDDEN systemctl' >&2; exit 97; }
ss() { echo 'FORBIDDEN ss' >&2; exit 97; }
curl() { echo 'FORBIDDEN curl' >&2; exit 97; }
kill() { echo 'FORBIDDEN kill' >&2; exit 97; }
source "$TEST_SCRIPT"
fake_sudo() { if [ "${1:-}" = -v ]; then return 0; fi; "$@"; }
restarts=0
fake_ctl() {
    echo "ctl:$*" >> "$EVENTS"
    if [ "$1" = restart ]; then
        restarts=$((restarts + 1))
        if [ "$MODE" = restart-failure ] && [ "$restarts" = 1 ]; then return 42; fi
    fi
}
SUDO=fake_sudo
SYSTEMCTL=fake_ctl
listener_pid() { echo 42; }
unit_pid() { echo 42; }
release_ok() { return 0; }
cmd_unit() { echo fixture-unit; }
wait_port_free() { return 0; }
wait_ready() {
    echo "health:$2" >> "$EVENTS"
    [ "$MODE" != readiness-failure ] || [ "$2" != bbbbbbbbbbbb ]
}
'''


@unittest.skipUnless(sys.platform.startswith('linux') and os.getuid() != 0,
                     'deployment rehearsals require unprivileged Linux')
class DeploymentRollbackTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='devatlas-rollback-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.legacy = self.root / 'rollback/legacy'
        self.legacy.mkdir(parents=True)
        self.cwd = self.root / 'original cwd'
        self.cwd.mkdir()
        self.current = self.root / 'releases' / OLD
        self.current.mkdir(parents=True)
        shutil.copy2('/bin/true', self.current / 'devatlas')
        (self.root / 'current').symlink_to(self.current)
        self.binary = self.legacy / 'devatlas'
        shutil.copy2('/bin/true', self.binary)
        self.checksum()
        (self.legacy / 'cmdline').write_bytes(b'\0'.join([b'/original/devatlas', b'serve', b'-port', b'19876']) + b'\0')
        (self.legacy / 'environ').write_bytes(b'HOME=/fixture\0PATH=/usr/bin:/bin\0KEEP=recorded value\0')
        (self.legacy / 'cwd').write_text(str(self.cwd) + '\n')
        (self.legacy / 'commit').write_text('c' * 12 + '\n')
        (self.legacy / 'starttime').write_text('123\n')
        self.state = self.root / 'state'
        self.state.mkdir()
        (self.state / 'history.json').write_text('history unchanged')
        self.registry = self.root / 'registry.yaml'
        self.registry.write_text('registry unchanged')
        (self.root / 'unit').write_text('fixture-unit\n')
        self.events = self.root / 'events'
        self.events.touch()

    def checksum(self):
        (self.legacy / 'SHA256').write_text(hashlib.sha256(self.binary.read_bytes()).hexdigest() + '  devatlas\n')

    def run_shell(self, command, mode='normal'):
        env = dict(os.environ, TEST_SCRIPT=str(SCRIPT), DEVATLAS_DEPLOY_ROOT=str(self.root),
                   DEVATLAS_REGISTRY=str(self.registry), DEVATLAS_STATE_DIR=str(self.state),
                   DEVATLAS_UNIT_PATH=str(self.root / 'unit'), DEVATLAS_PORT='19876',
                   EVENTS=str(self.events), MODE=mode, LEAK='must not reach restored child')
        result = subprocess.run(['bash', '-s'], input=COMMON + '\n' + command,
                                env=env, text=True, capture_output=True, timeout=8)
        self.assertNotIn('FORBIDDEN', result.stderr)
        self.assertEqual(self.registry.read_text(), 'registry unchanged')
        self.assertEqual((self.state / 'history.json').read_text(), 'history unchanged')
        return result

    def test_bad_hash_does_not_stop_running_service(self):
        self.binary.write_bytes(b'corrupted')
        self.assert_refused_before_stop()

    def test_missing_cwd_does_not_stop_running_service(self):
        self.cwd.rmdir()
        self.assert_refused_before_stop()

    def test_incomplete_or_malformed_record_does_not_stop_service(self):
        for name, content in [('environ', b'BROKEN'), ('cmdline', b'not nul terminated'),
                              ('commit', b'unknown'), ('starttime', b'wrong')]:
            with self.subTest(name=name):
                path = self.legacy / name
                original = path.read_bytes()
                path.write_bytes(content)
                self.assert_refused_before_stop()
                path.write_bytes(original)
        (self.legacy / 'environ').unlink()
        self.assert_refused_before_stop()

    def assert_refused_before_stop(self):
        result = self.run_shell('cmd_rollback')
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.events.read_text(), '', result.stdout)
        self.assertEqual((self.root / 'current').resolve(), self.current)

    def test_failed_legacy_launch_returns_to_current_release(self):
        result = self.run_shell('start_legacy() { echo legacy-launch-failed >> "$EVENTS"; return 1; }; cmd_rollback')
        self.assertEqual(result.returncode, 1, result.stderr)
        actions = self.events.read_text().splitlines()
        self.assertEqual(actions, ['ctl:stop devatlas-web.service', 'legacy-launch-failed',
                                   'ctl:restart devatlas-web.service', 'health:' + OLD[:12]])
        self.assertEqual((self.root / 'current').resolve(), self.current)
        self.assertIn('is serving again', result.stderr)

    def test_fallback_failure_is_not_reported_as_success(self):
        result = self.run_shell('start_legacy() { return 1; }; wait_ready() { return 1; }; cmd_rollback')
        self.assertEqual(result.returncode, 1)
        self.assertIn('ROLLBACK FAILED', result.stderr)
        self.assertNotIn('is serving again', result.stdout)
        self.assertNotIn('disable', self.events.read_text())

    def test_success_disables_unit_only_after_legacy_start(self):
        result = self.run_shell('start_legacy() { echo legacy-healthy >> "$EVENTS"; }; cmd_rollback')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.events.read_text().splitlines(),
                         ['ctl:stop devatlas-web.service', 'legacy-healthy', 'ctl:disable devatlas-web.service'])

    def test_occupied_port_with_matching_starttime_is_not_success(self):
        result = self.run_shell('''
wait_port_free() { return 1; }
proc_starttime() { echo 123; }
RESTORE=legacy
restore_prior
''')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('nothing to restore', result.stdout)
        self.assertNotIn('restored legacy process', result.stdout)
        self.assertIn('NOT verified', result.stdout)

    def test_cutover_restart_and_readiness_errors_restore_prior(self):
        for mode in ('restart-failure', 'readiness-failure'):
            with self.subTest(mode=mode):
                self.events.write_text('')
                result = self.run_shell('cmd_cutover ' + NEW, mode)
                self.assertEqual(result.returncode, 1, result.stderr)
                actions = self.events.read_text()
                self.assertEqual(actions.count('ctl:restart devatlas-web.service'), 2)
                self.assertIn('ctl:stop devatlas-web.service', actions)
                self.assertIn('ROLLED BACK:', result.stdout)
                self.assertEqual((self.root / 'current').resolve(), self.current)

    @unittest.skipUnless(shutil.which('cc'), 'real exec fixture requires a C compiler')
    def test_saved_binary_restores_real_argv_cwd_and_selected_environment(self):
        # This owned, non-network fixture pauses until test cleanup. No systemd
        # operation is used. It proves the saved executable is really launched.
        source = self.root / 'fixture.c'
        source.write_text(r'''
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
int main(int argc, char **argv) {
    char cwd[4096];
    printf("pid=%ld\ncwd=%s\n", (long)getpid(), getcwd(cwd, sizeof(cwd)));
    for (int i=0; i<argc; i++) printf("arg%d=%s\n", i, argv[i]);
    printf("KEEP=%s\nLEAK=%s\nREADY\n", getenv("KEEP"), getenv("LEAK") ? getenv("LEAK") : "absent");
    fflush(stdout);
    for (;;) pause();
}
''')
        subprocess.run(['cc', str(source), '-o', str(self.binary)], check=True, capture_output=True)
        self.checksum()
        try:
            result = self.run_shell(r'''
wait_ready() { for i in {1..100}; do grep -q READY "$LEGACY/server.log" 2>/dev/null && return 0; sleep .01; done; return 1; }
listener_pid() { sed -n 's/^pid=//p' "$LEGACY/server.log"; }
start_legacy
''')
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            log = (self.legacy / 'server.log').read_text()
            for value in ('cwd=' + str(self.cwd), 'arg0=/original/devatlas', 'arg1=serve',
                          'arg2=-port', 'arg3=19876', 'KEEP=recorded value', 'LEAK=absent'):
                self.assertIn(value, log)
        finally:
            logpath = self.legacy / 'server.log'
            if logpath.exists():
                for line in logpath.read_text().splitlines():
                    if line.startswith('pid='):
                        pid = int(line[4:])
                        fd = os.pidfd_open(pid)
                        try:
                            self.assertEqual(Path('/proc', str(pid), 'exe').resolve(), self.binary)
                            signal.pidfd_send_signal(fd, signal.SIGTERM)
                        finally:
                            os.close(fd)


if __name__ == '__main__':
    unittest.main()
