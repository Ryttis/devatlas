#!/usr/bin/env python3
"""Run one trusted DevAtlas coding task and review the verified commit."""

import argparse
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import re
import signal
import subprocess as sp
import sys
import tempfile


ROOT = Path(__file__).resolve().parent.parent
REVIEW_SCHEMA = {
    "type": "object",
    "properties": {
        "verdict": {"type": "string", "enum": ["pass", "changes_requested"]},
        "summary": {"type": "string"},
        "findings": {"type": "array", "items": {"type": "string"}},
    },
    "required": ["verdict", "summary", "findings"],
    "additionalProperties": False,
}


def child_env():
    env = os.environ.copy()
    for key in (
        "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
        "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK",
        "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
        "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_BASE_URL",
        "GH_TOKEN", "GITHUB_TOKEN", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
    ):
        env.pop(key, None)
    env.update(GIT_TERMINAL_PROMPT="0", CGO_ENABLED="1")
    env.setdefault("GOMAXPROCS", "2")
    return env


class CommandFailed(RuntimeError):
    def __init__(self, program, returncode):
        super().__init__(f"{program} exited with status {returncode}")
        self.returncode = returncode


def command(args, cwd, env, timeout=120, stdout=sp.PIPE, stderr=sp.PIPE):
    with sp.Popen(args, cwd=cwd, env=env, stdin=sp.DEVNULL, stdout=stdout,
                  stderr=stderr, start_new_session=True, text=True) as proc:
        try:
            out, err = proc.communicate(timeout=timeout)
        except (sp.TimeoutExpired, KeyboardInterrupt):
            for sig in (signal.SIGTERM, signal.SIGKILL):
                try:
                    os.killpg(proc.pid, sig)
                except ProcessLookupError:
                    break
                if sig == signal.SIGTERM:
                    try:
                        proc.wait(timeout=2)
                    except sp.TimeoutExpired:
                        pass
            proc.wait()
            raise
        if proc.returncode:
            raise CommandFailed(args[0], proc.returncode)
        return out or "", err or ""


def git(cwd, env, *args):
    return command(["git", "-c", "core.hooksPath=/dev/null", "-C", str(cwd), *args],
                   cwd, env)[0].strip()


def save_json(path, data):
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
    tmp.replace(path)


def run(task, revision, runs, *, working_checkout=None, job_dir=None, role_snapshot=None):
    if not re.fullmatch(r"[0-9a-fA-F]{7,40}", revision):
        raise ValueError("BASE must be a Git commit SHA (at least seven hex characters)")
    if not task.is_file() or task.stat().st_size > 32768:
        raise ValueError("TASK must be a readable file of at most 32 KiB")
    description = task.read_text(encoding="utf-8").strip()
    if not description:
        raise ValueError("TASK is empty")
    env = child_env()
    base = git(working_checkout or ROOT, env, "rev-parse", "--verify", f"{revision}^{{commit}}")
    roles = role_snapshot if role_snapshot is not None else json.loads((ROOT / "scripts/agent-roles.json").read_text(encoding="utf-8"))
    coder, reviewer = roles["coder"], roles["reviewer"]
    if (coder["backend"], reviewer["backend"]) != ("claude-code", "codex"):
        raise ValueError("Unsupported coder/reviewer backends")

    job = job_dir if job_dir is not None else Path(tempfile.mkdtemp(prefix="run-", dir=runs))
    checkout = working_checkout or job / "checkout"
    state = {"run_id": job.name, "base": base, "head": None}

    def mark(stage, **details):
        state.update(stage=stage, updated_at=dt.datetime.now(dt.timezone.utc).isoformat(), **details)
        save_json(job / "state.json", state)
        print(f"{stage}: {job}", flush=True)

    try:
        mark("preparing")
        (job / "task.md").write_text(description + "\n", encoding="utf-8")
        save_json(job / "roles.json", roles)
        if working_checkout is None:
            git(ROOT, env, "clone", "--local", "--no-hardlinks", "--no-checkout", "--",
                str(ROOT), str(checkout))
            git(checkout, env, "remote", "remove", "origin")
            git(checkout, env, "switch", "--detach", base)
        elif (git(checkout, env, "rev-parse", "HEAD") != base
              or git(checkout, env, "status", "--porcelain")):
            raise RuntimeError("Expected a clean workflow checkout at the approved base")
        claude_status, _ = command(["claude", "auth", "status"], checkout, env)
        codex_status = command(["codex", "login", "status"], checkout, env)
        auth = json.loads(claude_status)
        if not isinstance(auth, dict) or auth.get("authMethod") != "claude.ai":
            raise RuntimeError("Claude Code must be signed in to a Claude account")
        if "logged in using chatgpt" not in " ".join(codex_status).lower():
            raise RuntimeError("Codex must be signed in with ChatGPT")

        mark("coding")
        tools = "Read,Glob,Grep,Edit,Write"
        with (job / "coder.json").open("w") as out, (job / "coder.log").open("w") as err:
            command(["claude", "--model", coder["model"], "--tools", tools,
                     "--allowedTools", tools, "--strict-mcp-config", "--mcp-config",
                     '{"mcpServers":{}}', "--max-turns", str(coder["max_turns"]),
                     "--output-format", "json", "-p",
                     f"{coder['instructions']}\nBase commit: {base}\n\n{description}"],
                    checkout, env, coder["timeout_seconds"], out, err)
        result = json.loads((job / "coder.json").read_text(encoding="utf-8"))
        if (not isinstance(result, dict) or result.get("subtype") != "success" or
                result.get("is_error") is not False):
            raise RuntimeError("Coder did not report success; see coder.json")
        if git(checkout, env, "rev-parse", "HEAD") != base:
            raise RuntimeError("Coder changed HEAD; inspect the isolated checkout")
        git(checkout, env, "add", "--all")
        tree = git(checkout, env, "write-tree")
        if tree == git(checkout, env, "rev-parse", f"{base}^{{tree}}"):
            raise RuntimeError("Coder produced no changes")

        mark("verifying")
        git(checkout, env, "diff", "--cached", "--check")
        with (job / "checks.log").open("w") as log:
            formatted = command(["gofmt", "-l", "."], checkout, env, stderr=log)[0]
            if formatted:
                log.write(formatted)
                raise RuntimeError("Formatting failed; see checks.log")
            for args in (["go", "build", "-trimpath", "-o", str(job / "devatlas"), "./cmd/devatlas"],
                         ["go", "vet", "./..."], ["go", "test", "-count=1", "./..."],
                         ["go", "test", "-race", "-count=1", "./..."]):
                print("$", *args, file=log, flush=True)
                command(args, checkout, env, 600, log, sp.STDOUT)
        if (git(checkout, env, "write-tree") != tree or
                git(checkout, env, "status", "--porcelain", "--untracked-files=all") !=
                git(checkout, env, "status", "--porcelain", "--untracked-files=no")):
            raise RuntimeError("Checks changed the index or created untracked files")
        git(checkout, env, "diff", "--quiet")
        if git(checkout, env, "rev-parse", "HEAD") != base:
            raise RuntimeError("Checks changed HEAD")
        git(checkout, env, "-c", "user.name=DevAtlas Agent Runner", "-c",
            "user.email=devatlas-runner@localhost", "-c", "commit.gpgsign=false",
            "commit", "-m", "Implement DevAtlas task")
        head = git(checkout, env, "rev-parse", "HEAD")
        with (job / "change.patch").open("w") as patch:
            command(["git", "format-patch", "--stdout", "--binary", f"{base}..{head}"],
                    checkout, env, stdout=patch)

        mark("reviewing", head=head)
        schema, review = job / "review-schema.json", job / "review.json"
        save_json(schema, REVIEW_SCHEMA)
        with (job / "review.log").open("w") as log:
            command(["codex", "exec", "--model", reviewer["model"], "--sandbox", "read-only",
                     "-c", f'model_reasoning_effort={json.dumps(reviewer["reasoning_effort"])}',
                     "--output-schema", str(schema), "--output-last-message", str(review),
                     f"{reviewer['instructions']}\nReview only {base}..{head}. "
                     f"Formatting, build, vet, tests and race tests passed. Check log: {job / 'checks.log'}.\n\nTask:\n{description}"],
                    checkout, env, reviewer["timeout_seconds"], log, sp.STDOUT)
        report = json.loads(review.read_text(encoding="utf-8"))
        if (not isinstance(report, dict) or set(report) != set(REVIEW_SCHEMA["required"]) or
                report["verdict"] not in ("pass", "changes_requested") or
                not isinstance(report["summary"], str) or not report["summary"].strip() or
                not isinstance(report["findings"], list) or
                not all(isinstance(item, str) and item.strip() for item in report["findings"]) or
                bool(report["findings"]) != (report["verdict"] == "changes_requested")):
            raise RuntimeError("Review output is incomplete or inconsistent; see review.json")
        if git(checkout, env, "rev-parse", "HEAD") != head or git(checkout, env, "status", "--porcelain"):
            raise RuntimeError("Review changed the checkout")
        mark(report["verdict"])
        return 0 if report["verdict"] == "pass" else 2
    except (OSError, ValueError, RuntimeError, sp.TimeoutExpired, KeyboardInterrupt) as error:
        stage = state["stage"]
        reason = ("Interrupted" if isinstance(error, KeyboardInterrupt) else
                  f"Command exceeded {error.timeout} seconds" if isinstance(error, sp.TimeoutExpired)
                  else str(error))
        mark("failed", failed_stage=stage, error=reason)
        print(reason, file=sys.stderr)
        return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("task", type=Path, help="trusted task Markdown file")
    parser.add_argument("base", help="base Git commit SHA, already present locally")
    args = parser.parse_args()
    os.umask(0o077)
    state_home = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state")
    runs = (state_home / "devatlas/agent-runs").resolve()
    try:
        runs.mkdir(parents=True, exist_ok=True)
        with (runs / ".lock").open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RuntimeError("Another agent task is already running") from None
            return run(args.task, args.base, runs)
    except (OSError, ValueError, RuntimeError, KeyError, sp.TimeoutExpired) as error:
        print(f"Agent runner: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, signal.default_int_handler)
    sys.exit(main())
