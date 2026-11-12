#!/usr/bin/env python3
"""Plan, approve and run a small four-role DevAtlas workflow."""
import argparse
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import runpy
import signal
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
runner = runpy.run_path(str(HERE / "agent-runner.py"))
ROOT = HERE.parent
SHA = re.compile(r"[a-f0-9]{40}")
CASE = {
    "type": "object",
    "properties": {
        "args": {"type": "array", "items": {"type": "string"}},
        "stdout_contains": {"type": "string"},
    },
    "required": ["args", "stdout_contains"],
    "additionalProperties": False,
}
SLICE = {
    "type": "object",
    "properties": {
        "task": {"type": "string"},
        "checks": {"type": "array", "items": CASE},
    },
    "required": ["task", "checks"],
    "additionalProperties": False,
}
PLAN = {
    "type": "object",
    "properties": {"slices": {"type": "array", "items": SLICE}},
    "required": ["slices"],
    "additionalProperties": False,
}
TEST = {
    "type": "object",
    "properties": {"verdict": {"type": "string", "enum": ["PASS", "FAIL", "BLOCKED"]},
                   "summary": {"type": "string"}},
    "required": ["verdict", "summary"],
    "additionalProperties": False,
}
HANDOFF = {
    "type": "object",
    "properties": {"summary": {"type": "string"}},
    "required": ["summary"],
    "additionalProperties": False,
}
save = runner["save_json"]
git = runner["git"]
command = runner["command"]


def read(path, limit=32768):
    with path.open("rb") as f:
        data = f.read(limit + 1)
    if len(data) > limit:
        raise ValueError(f"{path.name} exceeds the input limit")
    return data


def validate_plan(plan):
    def fields(value, keys):
        return isinstance(value, dict) and set(value) == set(keys)

    def nonempty(value, limit):
        return isinstance(value, str) and 0 < len(value.strip()) <= limit and "\0" not in value

    if not fields(plan, ["slices"]) or not isinstance(plan["slices"], list) or not 1 <= len(plan["slices"]) <= 2:
        raise ValueError("Expected one or two slices")
    for item in plan["slices"]:
        if (not fields(item, ["task", "checks"]) or not nonempty(item["task"], 6000)
                or not isinstance(item["checks"], list) or not 1 <= len(item["checks"]) <= 5):
            raise ValueError("Each slice needs a task and one to five functional checks")
        for check in item["checks"]:
            if (not fields(check, ["args", "stdout_contains"]) or not isinstance(check["args"], list)
                    or not 1 <= len(check["args"]) <= 10
                    or not all(nonempty(arg, 200) for arg in check["args"])
                    or check["args"][0] not in ("version", "help", "list")
                    or any(arg not in ("-json", "-no-git") for arg in check["args"][1:])
                    or not nonempty(check["stdout_contains"], 250)):
                raise ValueError("Functional checks must use a supported CLI command and expected output")


def codex(role, prompt, schema, job, label, cwd, env):
    if role["backend"] != "codex":
        raise ValueError(f"{label} must use the Codex CLI")
    schema_path, output = job / f"{label}-schema.json", job / f"{label}.json"
    save(schema_path, schema)
    with (job / f"{label}.log").open("w") as log:
        command(["codex", "exec", "--model", role["model"], "--sandbox", "read-only",
                 "-c", f'model_reasoning_effort={json.dumps(role["reasoning_effort"])}',
                 "--output-schema", str(schema_path), "--output-last-message", str(output),
                 role["instructions"] + "\n\n" + prompt],
                cwd, env, role["timeout_seconds"], log, subprocess.STDOUT)
    return json.loads(read(output))


def state(job, status, **details):
    save(job / "workflow.json", {
        "status": status, "updated_at": datetime.now(timezone.utc).isoformat(), **details,
    })
    print(f"{status}: {job}", flush=True)


def prepare(task, base, workflows, env):
    request = read(task).decode("utf-8").strip()
    if not request:
        raise ValueError("Task is empty")
    if not re.fullmatch(r"[a-fA-F0-9]{7,40}", base):
        raise ValueError("BASE must be a Git commit SHA")
    base = git(ROOT, env, "rev-parse", "--verify", f"{base}^{{commit}}")
    roles = json.loads(read(HERE / "agent-roles.json"))
    job = Path(tempfile.mkdtemp(prefix="task-", dir=workflows))
    checkout = job / "checkout"
    state(job, "PLANNING")
    try:
        git(ROOT, env, "clone", "--local", "--no-hardlinks", "--no-checkout", "--", str(ROOT), str(checkout))
        git(checkout, env, "remote", "remove", "origin")
        branch = f"devatlas/{job.name}"
        git(checkout, env, "switch", "-c", branch, base)
        status = command(["codex", "login", "status"], checkout, env)
        if "logged in using chatgpt" not in " ".join(status).lower():
            raise RuntimeError("Codex must be signed in with ChatGPT")
        plan = codex(roles["architect"], request, PLAN, job, "architect", checkout, env)
        validate_plan(plan)
        snapshot = job / "plan.json"
        save(snapshot, {"task": request, "base": base, "branch": branch, "roles": roles, "plan": plan})
        digest = hashlib.sha256(read(snapshot, 131072)).hexdigest()
        state(job, "AWAITING_APPROVAL", plan_sha256=digest)
        print(f"Review {snapshot}\nApproval SHA-256: {digest}")
    except (OSError, ValueError, RuntimeError, KeyError, subprocess.TimeoutExpired, KeyboardInterrupt):
        state(job, "BLOCKED", stage="architect")
        raise


def checkout_at(checkout, branch, sha, env):
    if (git(checkout, env, "rev-parse", "HEAD") != sha
            or git(checkout, env, "branch", "--show-current") != branch
            or git(checkout, env, "status", "--porcelain")
            or git(checkout, env, "remote")):
        raise RuntimeError("Workflow checkout no longer matches the expected local branch and commit")


def functional_checks(checks, run_job, env):
    results = []
    with tempfile.TemporaryDirectory(dir=run_job) as scratch:
        cwd = Path(scratch)
        check_env = {**env, "XDG_CONFIG_HOME": scratch, "XDG_STATE_HOME": scratch,
                     "DEVATLAS_CONFIG": str(cwd / "registrations.yaml")}
        for check in checks:
            try:
                stdout, stderr = command([str(run_job / "devatlas"), *check["args"]],
                                         cwd, check_env, timeout=20)
                verdict = "PASS" if check["stdout_contains"] in stdout else "FAIL"
                evidence = stdout[:3000] + stderr[:1000]
            except RuntimeError as exc:
                verdict, evidence = "FAIL", str(exc)
            except (OSError, subprocess.TimeoutExpired) as exc:
                verdict, evidence = "BLOCKED", type(exc).__name__
            results.append({"check": check, "verdict": verdict, "evidence": evidence})
    return results


def execute(job, approval, runs, env):
    previous = json.loads(read(job / "workflow.json"))
    if previous["status"] != "AWAITING_APPROVAL":
        raise ValueError("Workflow has already started or is not awaiting approval")
    raw = read(job / "plan.json", 131072)
    if hashlib.sha256(raw).hexdigest() != approval or previous["plan_sha256"] != approval:
        raise ValueError("Approval does not match the exact saved plan")
    snapshot = json.loads(raw)
    validate_plan(snapshot["plan"])
    checkout, head, branch = job / "checkout", snapshot["base"], snapshot["branch"]
    if not SHA.fullmatch(head) or branch != f"devatlas/{job.name}":
        raise ValueError("Invalid checkout identity")
    checkout_at(checkout, branch, head, env)
    completed = []
    progress = {"plan_sha256": approval, "completed": completed}
    save(job / "approval.json", {"plan_sha256": approval,
                                 "approved_at": datetime.now(timezone.utc).isoformat()})
    state(job, "RUNNING", **progress)
    try:
        for index, item in enumerate(snapshot["plan"]["slices"], 1):
            run_job = Path(tempfile.mkdtemp(prefix="run-", dir=runs))
            progress.update(slice=index, run_id=run_job.name, review=None, tester=None, candidate=None)
            state(job, "RUNNING", **progress)
            task = run_job / "slice-input.md"
            task.write_text(f"{item['task']}\n\nAcceptance criteria:\n{json.dumps(item['checks'])}\n",
                            encoding="utf-8")
            code = runner["run"](task, head, runs, working_checkout=checkout, job_dir=run_job,
                                 role_snapshot=snapshot["roles"])
            review = json.loads(read(run_job / "state.json"))
            candidate = review.get("head")
            progress.update(review=review["stage"], candidate=candidate)
            if code != 0 or review["stage"] != "pass":
                state(job, "BLOCKED", **progress)
                return 2
            if not isinstance(candidate, str) or not SHA.fullmatch(candidate):
                raise RuntimeError("Review did not identify the candidate SHA")
            checkout_at(checkout, branch, candidate, env)
            if git(checkout, env, "rev-parse", f"{candidate}^") != head:
                raise RuntimeError("Candidate does not directly follow the previous accepted commit")
            state(job, "TESTING", **progress)
            results = functional_checks(item["checks"], run_job, env)
            evidence = {"head": candidate, "checks": results}
            save(run_job / "functional-checks.json", evidence)
            tester = codex(snapshot["roles"]["tester"],
                           f"Assess slice {index}: {item['task']}\nEvidence:\n{json.dumps(evidence)}",
                           TEST, run_job, "tester", checkout, env)
            if (not isinstance(tester, dict) or set(tester) != {"verdict", "summary"}
                    or tester["verdict"] not in ("PASS", "FAIL", "BLOCKED")
                    or not isinstance(tester["summary"], str) or not tester["summary"].strip()):
                raise ValueError("Invalid tester result")
            checkout_at(checkout, branch, candidate, env)
            progress["tester"] = tester["verdict"]
            if tester["verdict"] != "PASS" or any(r["verdict"] != "PASS" for r in results):
                state(job, "BLOCKED", **progress)
                return 2
            handoff = {"slice": index, "base": head, "head": candidate, "run_id": run_job.name,
                       "review": "PASS", "tester": "PASS"}
            completed.append(handoff)
            head = candidate
            state(job, "NOTIFYING_ARCHITECT", **progress)
            architect = {**snapshot["roles"]["architect"],
                         "instructions": "Acknowledge this completed slice. Summarize the review and "
                                         "test evidence; do not revise the approved plan or edit files."}
            receipt = codex(architect, json.dumps({
                "slice": handoff,
                "task": item["task"],
                "review": json.loads(read(run_job / "review.json")),
                "tester": tester,
            }), HANDOFF, run_job, "architect-handoff", checkout, env)
            if (not isinstance(receipt, dict) or set(receipt) != {"summary"}
                    or not isinstance(receipt["summary"], str) or not receipt["summary"].strip()):
                raise ValueError("Invalid architect handoff receipt")
            checkout_at(checkout, branch, candidate, env)
            state(job, "SLICE_PASSED", **progress)
        state(job, "PASSED", **progress)
        return 0
    except (OSError, ValueError, RuntimeError, KeyError, subprocess.TimeoutExpired, KeyboardInterrupt):
        state(job, "BLOCKED", **progress)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    plan = sub.add_parser("plan")
    plan.add_argument("task", type=Path)
    plan.add_argument("base")
    run = sub.add_parser("run")
    run.add_argument("job", type=Path)
    run.add_argument("--approve", required=True)
    args = parser.parse_args()
    os.umask(0o077)
    root = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state").resolve() / "devatlas"
    runs, workflows = root / "agent-runs", root / "workflows"
    try:
        for directory in (runs, workflows):
            directory.mkdir(parents=True, exist_ok=True)
        env = runner["child_env"]()
        # Coordinate with the existing single-slice runner; no overlapping jobs.
        with (runs / ".lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            if args.action == "plan":
                prepare(args.task, args.base, workflows, env)
                return 0
            job = args.job.resolve(strict=True)
            if job.parent != workflows or not re.fullmatch(r"task-[A-Za-z0-9_-]+", job.name):
                raise ValueError("Expected an existing DevAtlas workflow directory")
            return execute(job, args.approve, runs, env)
    except (OSError, ValueError, RuntimeError, KeyError, subprocess.TimeoutExpired, KeyboardInterrupt) as exc:
        print(f"DevAtlas workflow: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, signal.default_int_handler)
    sys.exit(main())
