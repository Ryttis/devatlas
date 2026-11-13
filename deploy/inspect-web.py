#!/usr/bin/env python3
"""Read-only Linux inventory. Run with sudo; outputs no full environment."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import socket
import subprocess
import urllib.request


def sha256(path):
    with open(path, "rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest() if hasattr(hashlib, "file_digest") else hashlib.sha256(stream.read()).hexdigest()


def inspect(port=18642):
    result = subprocess.run(
        ["ss", "-H", "-lntp", "sport = :" + str(port)],
        check=True, capture_output=True, text=True, timeout=5,
    ).stdout
    pids = set(re.findall(r"pid=(\d+)", result))
    if len(pids) != 1 or "127.0.0.1:" + str(port) not in result:
        raise RuntimeError("Expected exactly one identified loopback listener")
    pid = int(pids.pop())
    proc = Path("/proc") / str(pid)
    # starttime is field 22; comm may contain spaces/parentheses.
    start = (proc / "stat").read_text().rsplit(")", 1)[1].split()[19]
    argv = (proc / "cmdline").read_bytes().rstrip(b"\0").decode().split("\0")
    exe = os.readlink(proc / "exe")
    if Path(exe).name != "devatlas" or len(argv) < 2 or argv[1] != "serve":
        raise RuntimeError("Listener is not the expected DevAtlas serve process")
    uid = proc.stat().st_uid
    user = pwd.getpwuid(uid)
    env = dict(item.split("=", 1) for item in (proc / "environ").read_bytes().decode().split("\0") if "=" in item)
    # Include only settings necessary for path resolution and rollback.
    selected = {key: env[key] for key in ("HOME", "PATH", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "DEVATLAS_CONFIG") if key in env}

    def flag(name):
        for i, value in enumerate(argv[2:], 2):
            if value in ("-" + name, "--" + name):
                return argv[i + 1]
            for prefix in ("-" + name + "=", "--" + name + "="):
                if value.startswith(prefix):
                    return value[len(prefix):]
        return None

    home = selected.get("HOME") or user.pw_dir
    config = flag("config") or selected.get("DEVATLAS_CONFIG") or str(Path(selected.get("XDG_CONFIG_HOME") or str(Path(home) / ".config")) / "devatlas/registrations.yaml")
    state = flag("state-dir") or str(Path(selected.get("XDG_STATE_HOME") or str(Path(home) / ".local/state")) / "devatlas")
    cwd = os.readlink(proc / "cwd")
    config = str((Path(cwd) / config).resolve())
    state = str((Path(cwd) / state).resolve())
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open("http://127.0.0.1:" + str(port) + "/health", timeout=5) as response:
        health = json.load(response)
    if health.get("service") != "devatlas" or health.get("status") != "ok":
        raise RuntimeError("Unexpected health response")
    snapshot = dict(hostname=socket.gethostname(), pid=pid, start_ticks=start,
                    user=user.pw_name, uid=uid, executable=exe,
                    executable_sha256=sha256(proc / "exe"), argv=argv, cwd=cwd,
                    environment=selected, cgroup=(proc / "cgroup").read_text(),
                    config=config, config_exists=Path(config).is_file(),
                    config_sha256=sha256(config) if Path(config).is_file() else None,
                    state_dir=state, state_exists=Path(state).is_dir(), health=health)
    if (proc / "stat").read_text().rsplit(")", 1)[1].split()[19] != start:
        raise RuntimeError("Process changed during inventory; retry")
    return snapshot


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=18642)
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("port must be between 1 and 65535")
    print(json.dumps(inspect(args.port), indent=2))
