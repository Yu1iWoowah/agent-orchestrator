"""v3 RH consumer cases, not live successes or modified Go producer output.

Claude pending/revision3 -> completed/revision7 comes from Q29 command219
(source raw sha256 6435911b...3673c). Command, actor/run and successful
artifacts below are explicit counterfactual test inputs; no running update.
"""
import copy
import importlib.util
from pathlib import Path
import shlex
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("handoff_self_activity", Path(__file__).parents[1] / "handoff.py")
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)


class OldResultTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = h.mkdir(Path(self.tmp.name) / "00000000-0000-4000-8000-000000000001")
        shared = h.mkdir(root / "shared")
        self.run = SimpleNamespace(root=root, run_id=root.name, old="old",
            state={"location": {"shared_dir": str(shared)}, "stop_index_sha256": h.digest(b"[]")})
        self.documents = dict(sharedDir=str(shared), documentPath="handoff.md",
                              lease=dict(sessionId="old", epoch=1, frozen=False))
        self.run.cli = SimpleNamespace(documents=lambda: self.documents)
        self.run.send = lambda *args: None
        self.run.wait = lambda phase, ready: ready()
        self.run.workers = lambda: True
        self.run.save = lambda: None
        self.run.snapshot = lambda sid: copy.deepcopy(self.snap)
        directory = h.mkdir(root / "old")
        h.publish(shared / "handoff.md", ("handoff_run_id: " + root.name).encode())
        h.publish(directory / "managed.json", h.canonical({"file": dict(path="handoff.md",
            content=(shared / "handoff.md").read_text(), hash=h.digest((shared / "handoff.md").read_bytes()))}))
        h.publish(directory / "preservation.json", h.canonical(dict(schema="rh-old-preservation/2", evidence_turn_id="2a-turn")))
        self.receipt = dict(schema="rh-old-2a/1", run_id=root.name, old_id="old", evidence_turn_id="2a-turn",
            created_at="2026-11-10T18:00:33Z", managed_reply=h.file_reference(directory / "managed.json"),
            handoff=h.file_reference(shared / "handoff.md"), lease=h.lease_view(self.documents),
            worker_index_sha256=self.run.state["stop_index_sha256"], pending_question_refs=[],
            preservation=h.file_reference(directory / "preservation.json"), user_status_instruction=h.status_command(root, root.name))
        h.publish(directory / "phase-2a.json", h.canonical(self.receipt))
        h.publish(directory / "producer-result.json", h.canonical(dict(output_sha256=h.digest(h.canonical(self.receipt)))))
        command = h.helper_command(root, "old-receipt", "old")
        h.publish(directory / "producer-intent.json", h.canonical(dict(action="old-receipt", actor_id="old", run_id=root.name, command=command)))
        control = h.mkdir(root / "control/2a-old")
        payload = h.canonical(dict(run_id=root.name, recipient="old", old_id="old", phase="2a")).decode()
        handle = root.name + ":2a:old"
        h.publish(control / "payload.json", payload.encode())
        h.publish(control / "intent.json", h.canonical(dict(argv=["send", "--session", "old", "--steer", "--client-message-id", handle, "--message", payload])))
        h.publish(control / "result.json", b"{}")
        self.entry = dict(id="helper", sequence=219, revision=3, kind="activity", activityKind="command",
            status="pending", turnId="2a-turn", createdAt="2026-11-10T18:00:23Z",
            detail=dict(protocol="acp", providerToolName="Bash", toolKind="execute", rawCommand=command,
                        exitCode=0, output=h.canonical(self.receipt).decode(), outputMayBePartial=True))
        self.snap = dict(session=dict(id="old", projectId=h.PROJECT, kind="orchestrator", mode="chat", harness="claude-code",
            activity=dict(state="idle")), transcript=dict(conversationId="conv", activeBranchId="branch",
            latestSequence=220, hasMoreBefore=False, truncatedFields=0,
            turns=[dict(id="2a-turn", state="completed", completedAt="2026-11-10T18:03:00Z")],
            incomplete=[dict(type="entry", sequence=219, reason="running")], entries=[
                dict(id="control", sequence=206, kind="message", role="user", origin="automation", turnId="2a-turn",
                     senderSessionId="old", senderProjectId=h.PROJECT, clientMessageId=handle, text="[from old] " + payload),
                self.entry, dict(id="ready", sequence=220, kind="message", role="assistant", turnId="2a-turn",
                                 text=h.status_command(root, root.name) + " 10分鐘")]))

    def test_pending_success_then_completed_without_running(self):
        original = h.canonical(self.snap)
        self.assertEqual(h.phase_2a(self.run), self.receipt)
        self.assertTrue(h.quiescent(self.snap, run=self.run))
        self.assertEqual(h.canonical(self.snap), original)
        self.entry.update(status="completed", revision=7)
        self.snap["transcript"]["incomplete"] = []
        self.assertEqual(h.phase_2a(self.run), self.receipt)

    def test_original_turn_must_complete(self):
        self.snap["transcript"]["turns"][0].update(state="running")
        self.snap["session"]["activity"]["state"] = "active"
        self.assertFalse(h.phase_2a(self.run))
        self.assertFalse(h.quiescent(self.snap, run=self.run))

    def test_failed_missing_or_partial_results_never_pass(self):
        for change in (dict(exitCode=1), dict(exitCode=None), dict(output=""),
                       dict(output="STOP: inner failed", outputMayBePartial=True),
                       dict(output="{}"), dict(outputTruncated=True)):
            with self.subTest(change=change):
                detail = self.entry["detail"].copy()
                self.entry["detail"].update(change)
                with self.assertRaises(h.Stop):
                    h.phase_2a(self.run)
                self.entry["detail"] = detail

    def test_producer_result_hash_is_required(self):
        path = self.run.root / "old/producer-result.json"
        path.write_bytes(h.canonical(dict(output_sha256="0" * 64)))
        with self.assertRaises(h.Stop):
            h.phase_2a(self.run)

    def test_duplicate_wrong_turn_or_compound_helper_is_rejected(self):
        original = copy.deepcopy(self.snap)
        for change in ("duplicate", "turn", "run", "compound"):
            with self.subTest(change=change):
                self.snap = copy.deepcopy(original)
                entry = self.snap["transcript"]["entries"][1]
                if change == "duplicate":
                    self.snap["transcript"]["entries"].append(dict(entry, id="duplicate", sequence=221))
                elif change == "turn":
                    entry["turnId"] = "another-turn"
                elif change == "run":
                    entry["detail"]["rawCommand"] += "0"
                else:
                    entry["detail"]["rawCommand"] = "python3 - <<'PY'\npass\nPY\n" + entry["detail"]["rawCommand"] + "; echo done"
                with self.assertRaises(h.Stop):
                    h.phase_2a(self.run)

    def test_only_the_verified_helper_incomplete_is_ignored(self):
        self.snap["transcript"]["entries"].append(dict(id="other", sequence=221, kind="activity",
            activityKind="command", status="pending", turnId="2a-turn", detail=dict(rawCommand="other")))
        self.assertFalse(h.phase_2a(self.run))
        self.snap["transcript"]["entries"].pop()
        for item in (dict(type="entry", sequence=999), dict(type="turn", id="other", state="running")):
            with self.subTest(item=item):
                self.snap["transcript"]["incomplete"].append(item)
                self.assertFalse(h.phase_2a(self.run))
                self.snap["transcript"]["incomplete"].pop()

    def test_pending_result_reaches_original_exporter_without_rewriting_projection(self):
        original = h.canonical(self.snap)
        self.run.cli.data_dir = self.run.root
        with patch.object(h, "export_conversation", side_effect=RuntimeError("reached original exporter")):
            with self.assertRaisesRegex(RuntimeError, "reached original exporter"):
                h.export_material(self.run)
        self.assertEqual(h.canonical(self.snap), original)

    def test_fixed_command_and_only_reversible_wrapper(self):
        command = self.entry["detail"]["rawCommand"]
        for text in (command, shlex.join(["/bin/sh", "-c", command])):
            self.assertEqual(h.helper_identity(text, self.run.root, "old"), ("old-receipt", {}))
        for suffix in (" > output", "; ls", " | cat", " &", "\ntrue"):
            self.assertIsNone(h.helper_identity(command + suffix, self.run.root, "old"))
