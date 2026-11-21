"""Manual handoff against the fixed-base Cobra wire, without an AO daemon."""

import datetime as dt
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).parents[1] / "manual-handoff.py"
SPEC = importlib.util.spec_from_file_location("manual_handoff", SCRIPT)
manual = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(manual)


class ManualHandoffTests(unittest.TestCase):
    def run_handoff(self, failure=None):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            old_worktree = root / "old-checkout"
            old_worktree.mkdir()
            script = root / SCRIPT.name
            shutil.copyfile(SCRIPT, script)
            shared = root / "shared"
            receipt = shared / "handoff/receipt-old-new.json"
            receipt.parent.mkdir(parents=True)
            identity = dict(projectId="project", oldId="old", runId="run", newId="new")
            content = json.dumps(identity)
            state = dict(projectId="project", oldId="old", runId="run", oldLeaseEpoch=1,
                         sharedDir=str(shared), lastConfirmed="prepared", daemon={}, version="fixture",
                         outcome="pending")
            target = dict(harness="codex", accountId="acct", model="fixture-model",
                          effort="high", approvalMode="bypass-permissions")
            request = dict(target=target)
            raw = json.dumps(request).encode()
            (root / "request.json").write_bytes(raw)
            state["requestSha256"] = hashlib.sha256(raw).hexdigest()
            manual.write_state(root, state)
            commands = []
            old_dead = resumed = receipt_read = False
            activity = "idle"
            token = "RH3[run]"
            cli = manual.AO("/fake/ao", "project")

            def run(argv, *, env, **kwargs):
                nonlocal resumed, old_dead, activity, receipt_read
                commands.append(argv[1])
                self.assertNotIn("AO_SESSION_ID", env)
                self.assertNotIn("AO_PROJECT_ID", env)
                if argv[1] == "version":
                    return subprocess.CompletedProcess(argv, 0, "fixture\n", "")
                if argv[1] != "send":
                    self.assertEqual(argv[-2:], ["--project", "project"])
                args = argv[1:-2] if argv[1] != "send" else argv[1:]
                current = json.loads((root / "state.json").read_bytes())

                def result(value):
                    return subprocess.CompletedProcess(argv, 0, json.dumps(value), "")

                if args[:2] == ["session", "kill"]:
                    self.assertEqual(current["pending"], "kill-old")
                    old_dead = True
                    old_worktree.rmdir()
                    return result(None)
                if args[0] == "spawn":
                    self.assertTrue(old_dead)
                    self.assertEqual(current["spawnIntent"]["token"], token)
                    self.assertEqual(current["pending"], "spawn")
                    self.assertEqual(args[args.index("--name") + 1], token + " orchestrator")
                    if failure == "human-output":
                        # spawn.go emits a human line, while identity comes from session.go's list.
                        return subprocess.CompletedProcess(argv, 0, 'spawned session other "name" (idle)\n', "")
                    raise subprocess.TimeoutExpired(argv[:2], 180)
                if args[:2] == ["session", "ls"]:
                    if failure == "runner-interrupted":
                        raise KeyboardInterrupt
                    self.assertEqual(args, ["session", "ls", "--all", "--include-terminated", "--json"])
                    created = dt.datetime.fromisoformat(current["spawnIntent"]["createdAt"])
                    delta = -1 if failure == "predates" else 1
                    row = dict(id="new", projectId="other-project" if failure == "scope" else "project",
                               role="orchestrator", displayName=token + " orchestrator", isTerminated=False,
                               createdAt=(created + dt.timedelta(seconds=delta)).isoformat())
                    rows = [dict(row, id="near-match", displayName=token + "x orchestrator")]
                    if failure != "missing":
                        rows.append(row)
                    if failure == "multiple":
                        rows.append(dict(row, id="second"))
                    return result(dict(data=rows, meta=dict(hiddenTerminatedCount=0, hiddenOrchestratorCount=0)))
                if args[:3] == ["project", "documents", "info"]:
                    lease_id, epoch = ("new", 2) if receipt.exists() else ("old", 1)
                    if failure == "lease" and receipt.exists():
                        lease_id = "wrong-owner"
                    return result(dict(documents=dict(sharedDir=str(shared),
                                       lease=dict(sessionId=lease_id, epoch=epoch, frozen=False))))
                if args[:3] == ["project", "documents", "read"]:
                    self.assertEqual(args[3:], [str(receipt.relative_to(shared)), "--json"])
                    if failure == "permission":
                        return subprocess.CompletedProcess(argv, 1, "", "open handoff: permission denied")
                    if not receipt_read:
                        receipt_read = True
                        return subprocess.CompletedProcess(argv, 1, "",
                            "Project document not found (PROJECT_DOCUMENT_NOT_FOUND) [request fixture]")
                    observed = content if failure != "receipt" else json.dumps(dict(identity, projectId="other"))
                    receipt.write_text(observed)
                    receipt.chmod(0o600)
                    return result(dict(file=dict(path=args[3], binary=False, content=observed,
                                                 hash=hashlib.sha256(observed.encode()).hexdigest())))
                if args[:2] == ["session", "exit-agent"]:
                    self.assertEqual(args[2:], ["new", "--json"])
                    self.assertEqual(current["pending"], "exit-agent")
                    activity = "exited"
                    return result(dict(ok=True, sessionId="new"))
                if args[:2] == ["session", "resume-agent"]:
                    self.assertEqual(args[2:], ["new", "--json"])
                    self.assertEqual(current["pending"], "resume-agent")
                    resumed = True
                    activity = "idle"
                    return result(dict(ok=True, sessionId="new",
                                       resumeMode="replay" if failure == "replay" else "native"))
                self.assertEqual(args[:3], ["send", "--session", "new"])
                self.assertEqual(current["lastConfirmed"], "verified")
                return result(None)

            def get(path):
                if path == "sessions/old":
                    return dict(session=dict(id="old", projectId="project", kind="orchestrator",
                                             isTerminated=old_dead, activity=dict(state="idle")))
                if path == "sessions/new":
                    return dict(session=dict(id="new", projectId="project", kind="orchestrator",
                                             isTerminated=False, activity=dict(state=activity), mode="chat",
                                             harness="codex", spawnAgentSettings=dict(
                                                 accountId="acct", settings=dict(model="fixture-model", effort="high"))))
                if path == "agents/accounts/sessions/new":
                    account = None if failure == "empty-account" else "acct"
                    return dict(codex=account, bindings=dict(codex=dict(accountId=account, available=True)))
                self.assertEqual(path, "sessions/new/conversation?limit=1")
                effort = None if failure == "empty-effort" or failure == "post-effort" and resumed else "high"
                return dict(conversationId="conversation", settings=dict(
                    model="fixture-model", reasoningEffort=effort, approvalMode="bypass-permissions"))

            expected_stop = {
                "missing": "no session with the exact run token", "multiple": "multiple sessions",
                "scope": "wrong project", "predates": "predates intent", "permission": "permission denied",
                "receipt": "receipt run/project", "lease": "lease missing", "empty-account": "missing codex",
                "empty-effort": "missing reasoningEffort", "post-effort": "missing reasoningEffort",
                "replay": "did not complete in native mode",
                "runner-interrupted": "runner ended without settling",
            }.get(failure)
            with patch.object(manual.subprocess, "run", side_effect=run), patch.object(cli, "get", side_effect=get), \
                    patch.object(cli, "locate", return_value={}), patch.object(manual.time, "sleep"):
                if failure == "runner-interrupted":
                    (root / "launch.lock").touch()
                    with self.assertRaises(KeyboardInterrupt):
                        manual.run_handoff(cli, root, state)
                else:
                    result = manual.run_handoff(cli, root, state)
                    self.assertEqual(result, int(bool(expected_stop)), state.get("reason"))
            status = subprocess.run([sys.executable, "-I", str(script), "status", str(root)],
                                    cwd=root, capture_output=True, text=True)
            self.assertFalse(old_worktree.exists())
            self.assertEqual(commands.count("spawn"), 1)
            self.assertEqual(commands.count("send"), int(not expected_stop))
            self.assertEqual(status.returncode, int(bool(expected_stop)), status.stderr)
            saved = json.loads(status.stdout)
            if expected_stop:
                self.assertIn(expected_stop, saved["reason"])
            if failure in ("missing", "multiple", "scope", "predates", "runner-interrupted"):
                self.assertEqual(saved["lastConfirmed"], "old-terminated")
                self.assertEqual(saved["pending"], "spawn")
                self.assertNotIn("newId", saved)
            elif not expected_stop:
                self.assertEqual(saved["newId"], "new")
                self.assertEqual(saved["outcome"], "complete")
                self.assertEqual(saved["resume"]["resumeMode"], "native")
                self.assertEqual(saved["before"], saved["after"])

    def test_original_spawn_recovered_without_using_human_stdout(self):
        for scenario in (None, "human-output"):
            with self.subTest(scenario=scenario):
                self.run_handoff(scenario)

    def test_unproved_spawn_is_not_reissued_and_status_survives_old(self):
        for scenario in ("missing", "multiple", "scope", "predates", "runner-interrupted"):
            with self.subTest(scenario=scenario):
                self.run_handoff(scenario)

    def test_unproved_receipt_or_native_settings_stop_before_notify(self):
        for scenario in ("permission", "receipt", "lease", "empty-account", "empty-effort", "replay", "post-effort"):
            with self.subTest(scenario=scenario):
                self.run_handoff(scenario)

    def test_start_detaches_from_old_actor_and_cannot_relaunch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            old = root / "old-worktree"
            old.mkdir()
            script = root / SCRIPT.name
            shutil.copyfile(SCRIPT, script)
            cli = root / "fake-ao"
            cli.write_text("#!/bin/sh\n"
                           'test "$PWD" = "${AO_DATA_DIR}" && test -z "$AO_SESSION_ID$AO_PROJECT_ID" || exit 2\n'
                           'printf "detached fixture\\n" >&2\nexit 1\n')
            cli.chmod(0o700)
            daemon = dict(runFile=str(root / "run-file"), dataDir=str(root))
            manual.write_state(root, dict(executable=str(cli), projectId="project", oldId="old",
                                          daemon=daemon, lastConfirmed="prepared", outcome="pending"))
            start = [sys.executable, "-I", str(script), "start", str(root)]
            started = subprocess.run(start, cwd=old, capture_output=True, text=True, timeout=5,
                                     env=dict(os.environ, AO_SESSION_ID="old", AO_PROJECT_ID="project"))
            self.assertEqual(started.returncode, 0, started.stderr)
            old.rmdir()
            status = [sys.executable, "-I", str(script), "status", str(root)]
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                result = subprocess.run(status, cwd=root, capture_output=True, text=True, timeout=5)
                saved = json.loads(result.stdout)
                if saved["outcome"] == "STOP":
                    break
                time.sleep(0.01)
            self.assertEqual(saved.get("reason"), "status --json: detached fixture")
            self.assertEqual(saved["lastConfirmed"], "prepared")
            restarted = subprocess.run(start, cwd=root, capture_output=True, text=True, timeout=5,
                                       env=dict(os.environ, AO_SESSION_ID="old"))
            self.assertEqual(restarted.returncode, 1)


if __name__ == "__main__":
    unittest.main()
