#!/usr/bin/env python3
"""Test a binary on an OS-assigned loopback port using temporary fixtures only."""
import argparse
import json
from pathlib import Path
import re
import selectors
import subprocess
import tempfile
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("binary", type=Path)
    parser.add_argument("--sha", default="9b92a6127d721c2e91d12f29b780a48827389d1c")
    args = parser.parse_args()
    binary = args.binary.resolve()
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with tempfile.TemporaryDirectory(prefix="devatlas-smoke-") as temp:
        root = Path(temp)
        run = root / "state/agent-runs/run-smoke1"
        run.mkdir(parents=True)
        fixtures = {
            "state.json": dict(run_id="run-smoke1", stage="changes_requested", base="a" * 40, head=args.sha, updated_at="2026-11-14T00:00:00Z"),
            "review.json": dict(verdict="changes_requested", summary="REVIEW_VISIBLE ssh://alice:prefix'TAIL_SENTINEL@example.invalid/repo", findings=["[P2] docs/example.md:12 — FINDING_VISIBLE"]),
            "tester.json": dict(verdict="PASS", summary="TESTER_VISIBLE"),
        }
        for name, data in fixtures.items():
            (run / name).write_text(json.dumps(data))
        before = {path: path.read_bytes() for path in run.iterdir()}
        # Port 0 is atomically allocated by the OS, avoiding a check/bind race.
        process = subprocess.Popen([str(binary), "serve", "-host", "127.0.0.1", "-port", "0", "-config", str(root / "registry.yaml"), "-state-dir", str(root / "state")], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        try:
            sel = selectors.DefaultSelector()
            sel.register(process.stdout, selectors.EVENT_READ)
            output = b""
            for _ in range(100):
                if not sel.select(timeout=0.1):
                    continue
                chunk = process.stdout.read1(4096)
                if not chunk:
                    raise RuntimeError("Candidate exited: " + output.decode())
                output += chunk
                match = re.search(rb"url=http://127\.0\.0\.1:(\d+)", output)
                if match:
                    break
            else:
                raise RuntimeError("Candidate did not become ready")
            base = "http://127.0.0.1:" + match[1].decode()

            def get(path):
                with client.open(base + path, timeout=5) as response:
                    return response.read().decode()

            health = json.loads(get("/health"))
            assert health["commit"] == args.sha[:12] and health["modified"] is False, health
            assert "run-smoke1" in get("/runs") and "run-smoke1" in get("/")
            detail = get("/runs/run-smoke1")
            for marker in ("REVIEW_VISIBLE", "FINDING_VISIBLE", "TESTER_VISIBLE", "[paslėpta]"):
                assert marker in detail, marker
            assert "TAIL_SENTINEL" not in detail and "example.invalid" not in detail
            get("/projects")
            for path in ("/runs/run-smoke1/review.json", "/runs/run-smoke1/log.txt"):
                try:
                    get(path)
                    raise AssertionError("Raw route exposed: " + path)
                except urllib.error.HTTPError as error:
                    assert error.code == 404, error.code
            assert all(path.read_bytes() == data for path, data in before.items())
            assert not (root / "registry.yaml").exists()
            print(json.dumps(dict(result="PASS", url=base, health=health, fixtures="temporary only", checks=["list", "detail", "projects", "redaction", "raw-route 404", "files unchanged"])))
        finally:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    main()
