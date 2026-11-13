#!/usr/bin/env python3
"""Validate a trusted operator-owned legacy record, or launch that record."""
import argparse
import hashlib
import os
from pathlib import Path
import re
import subprocess
import sys


def record(directory, port):
    directory = Path(directory)
    for name in ('devatlas', 'SHA256', 'cmdline', 'environ', 'cwd', 'commit', 'starttime'):
        if not (directory / name).is_file():
            raise ValueError('incomplete legacy record: ' + name)
    expected = (directory / 'SHA256').read_bytes()
    if not re.fullmatch(rb'[0-9a-f]{64}  devatlas\n', expected):
        raise ValueError('invalid legacy checksum record')
    if hashlib.sha256((directory / 'devatlas').read_bytes()).hexdigest().encode() != expected[:64]:
        raise ValueError('saved legacy binary does not match recorded SHA256')
    if not os.access(directory / 'devatlas', os.X_OK):
        raise ValueError('saved legacy binary is not executable')

    def items(name):
        data = (directory / name).read_bytes()
        if not data.endswith(b'\0'):
            raise ValueError('invalid NUL-delimited legacy ' + name)
        return data[:-1].split(b'\0')

    argv = items('cmdline')
    if (len(argv) < 4 or any(not arg for arg in argv[:2]) or argv[1] != b'serve' or
            not any(arg == b'-port' and argv[i + 1:i + 2] == [str(port).encode()]
                    for i, arg in enumerate(argv))):
        raise ValueError('legacy command is not serve on the selected port')
    env = {}
    for item in items('environ'):
        key, sep, value = item.partition(b'=')
        if not sep or not re.fullmatch(rb'[A-Za-z_][A-Za-z0-9_]*', key) or key in env:
            raise ValueError('invalid legacy environment record')
        env[key] = value
    cwd = (directory / 'cwd').read_text().removesuffix('\n')
    if not os.path.isabs(cwd) or not os.path.isdir(cwd) or not os.access(cwd, os.X_OK):
        raise ValueError('original working directory is missing or inaccessible')
    if not re.fullmatch(r'[0-9a-f]{12,40}\n?', (directory / 'commit').read_text()):
        raise ValueError('invalid legacy commit record')
    if not re.fullmatch(r'[0-9]+\n?', (directory / 'starttime').read_text()):
        raise ValueError('invalid legacy process start time')
    return argv, env, cwd


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('validate', 'launch'))
    parser.add_argument('directory', type=Path)
    parser.add_argument('port', type=int)
    args = parser.parse_args()
    try:
        argv, env, cwd = record(args.directory, args.port)
        if args.action == 'launch':
            # Popen preserves argv[0] while selecting the verified saved binary.
            with (args.directory / 'server.log').open('ab') as log:
                child = subprocess.Popen(argv, executable=bytes(args.directory / 'devatlas'),
                                         cwd=cwd, env=env, start_new_session=True,
                                         stdin=subprocess.DEVNULL, stdout=log, stderr=log)
            try:
                start = Path('/proc', str(child.pid), 'stat').read_text().rsplit(')', 1)[1].split()[19]
            except FileNotFoundError:
                child.wait(timeout=1)
                raise ValueError('legacy child exited during startup')
            print(child.pid, start)
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print('legacy record/launch failed: ' + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
