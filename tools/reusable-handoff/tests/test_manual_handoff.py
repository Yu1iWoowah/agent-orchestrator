"""Manual handoff against the fixed-base Cobra wire, without an AO daemon."""

import datetime as dt
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit


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
            lease_receipt = shared / "handoff/receipt-old-new.resumed-lease.json"
            receipt.parent.mkdir(parents=True)
            identity = dict(projectId="project", oldId="old", runId="run", newId="new")
            content = json.dumps(identity)
            state = dict(projectId="project", oldId="old", runId="run", oldLeaseEpoch=1,
                         sharedDir=str(shared), lastConfirmed="prepared", daemon={}, version="fixture",
                         outcome="pending")
            target = dict(harness="codex", accountId="acct", model="fixture-model",
                          effort="high", approvalMode="bypass-permissions")
            if failure == "invalid-approval":
                target["approvalMode"] = "typo"
            request = dict(target=target)
            raw = json.dumps(request).encode()
            (root / "request.json").write_bytes(raw)
            state["requestSha256"] = hashlib.sha256(raw).hexdigest()
            manual.write_state(root, state)
            commands = []
            old_dead = resumed = receipt_read = False
            notification_reads = 0
            elapsed = 0
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
                    epoch = 3 if lease_receipt.exists() else 2 if receipt.exists() else 1
                    lease_id = "old" if epoch == 1 else "new"
                    if failure == "lease" and receipt.exists():
                        lease_id = "wrong-owner"
                    return result(dict(documents=dict(sharedDir=str(shared),
                                       lease=dict(sessionId=lease_id, epoch=epoch, frozen=False))))
                if args[:3] == ["project", "documents", "read"]:
                    if args[3] == str(lease_receipt.relative_to(shared)):
                        self.assertEqual(args[4:], ["--json"])
                        observed = lease_receipt.read_text()
                        return result(dict(file=dict(path=args[3], binary=False, content=observed,
                                                     hash=hashlib.sha256(observed.encode()).hexdigest())))
                    self.assertEqual(args[3:], [str(receipt.relative_to(shared)), "--json"])
                    if failure == "permission":
                        return subprocess.CompletedProcess(argv, 1, "", "open handoff: permission denied")
                    if not receipt_read:
                        receipt_read = True
                        if failure == "receipt-published-during-read":
                            receipt.write_text(content)
                            receipt.chmod(0o600)
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
                handle = args[args.index("--client-message-id") + 1]
                commands[-1] = handle
                if handle == "run:renew-lease":
                    self.assertTrue(resumed)
                    self.assertEqual(current["pending"], "renew-new-lease")
                    self.assertEqual(current.get("bootstrapLeaseEpoch"), 2)
                    self.assertFalse(lease_receipt.exists())
                    message = args[args.index("--message") + 1]
                    command = shlex.split(message.splitlines()[1])
                    self.assertEqual(command, ["ao", "project", "documents", "write", "--project", "project",
                                               str(lease_receipt.relative_to(shared)), "--expected-hash", "missing",
                                               "--file", str(root / "resumed-lease.json")])
                    shutil.copyfile(command[-1], lease_receipt)
                    lease_receipt.chmod(0o600)
                    return result(None)
                if failure == "lease-renewed-after-resume":
                    self.assertTrue(lease_receipt.exists(), "verification notified new with the stale epoch")
                self.assertEqual(current["lastConfirmed"], "verified")
                self.assertEqual(handle, "run:verified")
                if failure == "notification-steered":
                    return subprocess.CompletedProcess(argv, 0, "Steering accepted by provider for active turn "
                        "native-steer with delivery handle run:verified; agent action is not confirmed.\n", "")
                return subprocess.CompletedProcess(argv, 0, "No active turn to steer. Message accepted as a "
                    "normal Chat turn in queued state with delivery handle run:verified; provider delivery is not confirmed.\n", "")

            def get(path):
                nonlocal notification_reads
                if path.startswith("agents/codex/models?"):
                    self.assertFalse(old_dead)
                    self.assertEqual(parse_qs(urlsplit(path).query), dict(
                        projectId=["project"], role=["orchestrator"], agentAccountId=["acct"], catalogOnly=["true"]))
                    stale = failure == "stale-catalog"
                    return dict(agentId="codex",
                                accountBinding=dict(accountId="other" if failure == "invalid-account" else "acct"),
                                stale=stale, refreshState="error" if stale else "idle",
                                refreshError="discovery unavailable" if stale else "",
                                lastSuccessAt=dt.datetime.now(dt.timezone.utc).isoformat(),
                                models=[dict(id="fixture-model", efforts=["low" if failure == "invalid-effort" else "high"])])
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
                if path.startswith("sessions/new/conversation?limit=500"):
                    self.assertEqual(commands.count("run:verified"), 1)
                    if failure == "notification-on-older-page":
                        params = parse_qs(urlsplit(path).query)
                        if "beforeSequence" in params:
                            self.assertEqual(params["beforeSequence"], ["501"])
                            self.assertEqual(params["afterSequence"], ["10"])
                            return dict(sessionId="new", conversationId="conversation", activities=[], turns=[],
                                        messages=[dict(role="user", clientMessageId="run:verified",
                                                       turnId="notification", sequence=11)])
                        return dict(sessionId="new", conversationId="conversation", messages=[], activities=[],
                                    hasMoreBefore=True, oldestSequence=501, latestSequence=1000,
                                    turns=[dict(id="notification", state="completed", providerTurnId="native-notification")])
                    if failure == "notification-steered":
                        return dict(sessionId="new", conversationId="conversation", messages=[], turns=[],
                                    activities=[dict(id="steer-receipt", activityKind="system", status="completed",
                                                     providerItemId="steer:run:verified", sequence=11,
                                                     detail=dict(event="steer", clientMessageId="run:verified"))])
                    notification_reads += 1
                    queued = failure == "notification-queued" or notification_reads == 1
                    return dict(sessionId="new", conversationId="conversation",
                                messages=[dict(role="user", clientMessageId="other:verified", turnId="other", sequence=12),
                                          dict(role="user", clientMessageId="run:verified", turnId="notification", sequence=11)],
                                activities=[],
                                turns=[dict(id="other", state="completed", providerTurnId="native-other"),
                                       dict(id="notification", state="queued" if queued else "completed",
                                            providerTurnId="native-notification")])
                self.assertEqual(path, "sessions/new/conversation?limit=1")
                effort = None if failure == "empty-effort" or failure == "post-effort" and resumed else "high"
                return dict(sessionId="new", conversationId="conversation", latestSequence=10, settings=dict(
                    model="fixture-model", reasoningEffort=effort, approvalMode="bypass-permissions"))

            preflight_failed = failure in ("invalid-approval", "invalid-account", "invalid-effort", "stale-catalog")
            expected_stop = {
                "invalid-approval": "invalid target approvalMode",
                "invalid-account": "target account",
                "invalid-effort": "target model/effort",
                "stale-catalog": "target catalog",
                "notification-queued": "timed out waiting for verification notification delivery",
                "missing": "no session with the exact run token", "multiple": "multiple sessions",
                "scope": "wrong project", "predates": "predates intent", "permission": "permission denied",
                "receipt": "receipt run/project", "lease": "lease missing", "empty-account": "missing codex",
                "empty-effort": "missing reasoningEffort", "post-effort": "missing reasoningEffort",
                "replay": "did not complete in native mode",
                "runner-interrupted": "runner ended without settling",
            }.get(failure)

            def advance(seconds):
                nonlocal elapsed
                elapsed += seconds

            with patch.object(manual.subprocess, "run", side_effect=run), patch.object(cli, "get", side_effect=get), \
                    patch.object(cli, "locate", return_value={}), \
                    patch.object(manual.time, "monotonic", side_effect=lambda: elapsed), \
                    patch.object(manual.time, "sleep", side_effect=advance):
                if failure == "runner-interrupted":
                    (root / "launch.lock").touch()
                    with self.assertRaises(KeyboardInterrupt):
                        manual.run_handoff(cli, root, state)
                else:
                    result = manual.run_handoff(cli, root, state)
                    self.assertEqual(result, int(bool(expected_stop)), state.get("reason"))
            status = subprocess.run([sys.executable, "-I", str(script), "status", str(root)],
                                    cwd=root, capture_output=True, text=True)
            self.assertEqual(old_worktree.exists(), preflight_failed)
            self.assertEqual(commands.count("spawn"), int(not preflight_failed))
            self.assertEqual(commands.count("run:verified"), int(not expected_stop or failure == "notification-queued"))
            self.assertEqual(status.returncode, int(bool(expected_stop)), status.stderr)
            saved = json.loads(status.stdout)
            if expected_stop:
                self.assertIn(expected_stop, saved["reason"])
            if preflight_failed:
                self.assertEqual(saved["lastConfirmed"], "prepared")
                self.assertIsNone(saved.get("pending"))
            if failure == "notification-queued":
                self.assertEqual(saved["lastConfirmed"], "verified")
                self.assertEqual(saved["pending"], "notify-new")
                self.assertEqual(saved["notification"]["clientMessageId"], "run:verified")
            if failure in ("missing", "multiple", "scope", "predates", "runner-interrupted"):
                self.assertEqual(saved["lastConfirmed"], "old-terminated")
                self.assertEqual(saved["pending"], "spawn")
                self.assertNotIn("newId", saved)
            elif not expected_stop:
                self.assertEqual(saved["newId"], "new")
                self.assertEqual(saved["outcome"], "complete")
                self.assertEqual(saved["resume"]["resumeMode"], "native")
                self.assertEqual(saved["before"], saved["after"])
                if failure == "lease-renewed-after-resume":
                    self.assertEqual(commands.count("run:renew-lease"), 1)
                    self.assertEqual(saved.get("bootstrapLeaseEpoch"), 2)
                    self.assertEqual(saved.get("resumedLeaseEpoch"), 3)
                if failure == "notification-steered":
                    self.assertEqual(saved["notification"]["activityId"], "steer-receipt")
                else:
                    self.assertEqual(saved["notification"]["providerTurnId"], "native-notification")

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

    def test_invalid_target_never_kills_old(self):
        for scenario in ("invalid-approval", "invalid-account", "invalid-effort"):
            with self.subTest(scenario=scenario):
                self.run_handoff(scenario)

    def test_queued_verification_notification_does_not_complete_handoff(self):
        self.run_handoff("notification-queued")

    def test_stale_catalog_with_matching_model_never_kills_old(self):
        self.run_handoff("stale-catalog")

    def test_verification_notification_is_recovered_from_an_older_page(self):
        self.run_handoff("notification-on-older-page")

    def test_verification_notification_recovers_original_steer_receipt(self):
        self.run_handoff("notification-steered")

    def test_receipt_published_after_404_is_read_again(self):
        self.run_handoff("receipt-published-during-read")

    def test_native_resume_requires_a_fresh_writer_lease(self):
        self.run_handoff("lease-renewed-after-resume")

    def test_prepare_uses_the_scoped_canonical_project_directory(self):
        for project, key in (("Demo", "%44emo"), ("demo.ops", "demo%2Eops")):
            with self.subTest(project=project), tempfile.TemporaryDirectory() as tmp:
                data = Path(tmp)
                shared = data / "projects" / key / "orchestrator" / "shared"
                shared.mkdir(parents=True)
                old = project + "-1"
                request = data / "request.json"
                request.write_text(json.dumps(dict(projectId=project, oldId=old, confirmation="fixture",
                    handoff="A–F", target={}, version="fixture", workers={}, materials={})))
                cli = manual.AO(sys.executable, project)
                docs = dict(sharedDir=str(shared), lease=dict(sessionId=old, epoch=1, frozen=False))
                with patch.dict(os.environ, AO_SESSION_ID=old), patch.object(manual.shutil, "which", return_value=sys.executable), \
                        patch.object(manual, "AO", return_value=cli) as create_cli, \
                        patch.object(cli, "locate", return_value=dict(dataDir=str(data))), \
                        patch.object(cli, "checked", return_value="fixture"), patch.object(cli, "preflight_target"), \
                        patch.object(cli, "session", return_value=dict(isTerminated=False)), \
                        patch.object(cli, "documents", return_value=docs), patch("builtins.print"):
                    manual.prepare(request)
                create_cli.assert_called_once_with(str(Path(sys.executable).resolve()), project)
                state_path, = (data / ".manual-handoff").glob("*/state.json")
                state = json.loads(state_path.read_bytes())
                self.assertEqual((state["projectId"], state["oldId"], state["sharedDir"]),
                                 (project, old, str(shared)))

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
