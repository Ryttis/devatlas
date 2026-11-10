"""Offline integration tests for the agent runner's Git and failure boundaries."""

import json
import os
from pathlib import Path
import shutil
import subprocess as sp
import sys
import tempfile
import unittest


FAKE_TOOL = r'''#!/usr/bin/env python3
import json, os, pathlib, re, subprocess, sys

name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
mode = os.environ['RUNNER_TEST_MODE']
if name == 'claude':
    if args == ['auth', 'status']:
        print(json.dumps({'authMethod': 'api_key' if mode == 'api_key' else 'claude.ai'}))
    else:
        assert 'ANTHROPIC_API_KEY' not in os.environ
        assert subprocess.check_output(['git', 'remote'], text=True) == ''
        pathlib.Path('answer.txt').write_text('updated\n')
        print(json.dumps({'subtype': 'success', 'is_error': False}))
elif name == 'go' and args[:1] == ['test'] and mode == 'test_failure':
    print('fake Go test failure')
    sys.exit(7)
elif name == 'codex':
    if args == ['login', 'status']:
        print('Logged in using ChatGPT', file=sys.stderr)
    else:
        assert 'OPENAI_API_KEY' not in os.environ
        assert args[args.index('--sandbox') + 1] == 'read-only'
        base, head = re.search(r'Review only ([0-9a-f]+)\.\.([0-9a-f]+)', args[-1]).groups()
        assert subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip() == head
        assert subprocess.check_output(['git', 'rev-parse', 'HEAD^'], text=True).strip() == base
        assert subprocess.check_output(['git', 'diff', '--name-only', base, head], text=True) == 'answer.txt\n'
        result = {'verdict': 'pass', 'summary': 'Reviewed change.', 'findings': []}
        if mode == 'findings':
            result.update(verdict='changes_requested', findings=['A specific issue.'])
        if mode == 'invalid_review':
            result.update(verdict='pass', findings=['Contradictory result.'])
        pathlib.Path(args[args.index('--output-last-message') + 1]).write_text(json.dumps(result))
'''


class AgentRunnerTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        sp.run(["git", "init", "-q"], cwd=self.repo, check=True)
        (self.repo / "answer.txt").write_text("base\n")
        sp.run(["git", "add", "answer.txt"], cwd=self.repo, check=True)
        sp.run(["git", "-c", "user.name=Test", "-c", "user.email=test@localhost",
                "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
                "commit", "-qm", "base"], cwd=self.repo, check=True)
        self.base = sp.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo, text=True).strip()
        scripts = self.repo / "scripts"
        scripts.mkdir()
        for name in ("agent-runner.py", "agent-roles.json"):
            shutil.copyfile(Path(__file__).with_name(name), scripts / name)
        (self.repo / "answer.txt").write_text("uncommitted source work\n")
        self.task = self.root / "task.md"
        self.task.write_text("Update the answer fixture only.\n")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for name in ("claude", "codex", "go", "gofmt"):
            exe = self.bin / name
            exe.write_text(FAKE_TOOL)
            exe.chmod(0o700)

    def launch(self, mode, exit_code):
        env = os.environ.copy()
        env.update(PATH=f"{self.bin}:{env['PATH']}", XDG_STATE_HOME=str(self.root / "state"),
                   RUNNER_TEST_MODE=mode, ANTHROPIC_API_KEY="fake", OPENAI_API_KEY="fake")
        result = sp.run([sys.executable, "scripts/agent-runner.py", str(self.task), self.base],
                        cwd=self.repo, env=env, capture_output=True, text=True, timeout=25)
        self.assertEqual(result.returncode, exit_code, result.stdout + result.stderr)
        self.assertEqual((self.repo / "answer.txt").read_text(), "uncommitted source work\n")
        self.assertEqual(sp.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo,
                                         text=True).strip(), self.base)
        job, = (self.root / "state/devatlas/agent-runs").glob("run-*")
        return job, json.loads((job / "state.json").read_text())

    def test_handoff_preserves_source_and_exports_exact_commit(self):
        job, state = self.launch("pass", 0)
        self.assertEqual(state["stage"], "pass")
        self.assertNotEqual(state["head"], self.base)
        self.assertIn("+updated", (job / "change.patch").read_text())
        self.assertEqual((job / "checkout/answer.txt").read_text(), "updated\n")

    def test_failed_checks_do_not_create_commit_or_review(self):
        job, state = self.launch("test_failure", 1)
        self.assertEqual(state["failed_stage"], "verifying")
        self.assertIsNone(state["head"])
        self.assertFalse((job / "review.log").exists())
        self.assertIn("fake Go test failure", (job / "checks.log").read_text())

    def test_review_findings_and_inconsistent_verdicts_do_not_pass(self):
        for mode, code, stage in (("findings", 2, "changes_requested"),
                                  ("invalid_review", 1, "failed")):
            with self.subTest(mode=mode):
                job, state = self.launch(mode, code)
                self.assertEqual(state["stage"], stage)
                self.assertTrue((job / "change.patch").exists())
                shutil.rmtree(job)

    def test_api_auth_stops_before_coding(self):
        job, state = self.launch("api_key", 1)
        self.assertEqual(state["failed_stage"], "preparing")
        self.assertFalse((job / "coder.json").exists())


if __name__ == "__main__":
    unittest.main()
