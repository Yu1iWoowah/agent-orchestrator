"""Cobra/DTO producer → unchanged transport adapters → RH consumers.

Run through TestReusableHandoffWire; there is no handwritten fallback schema.
"""
import copy
import importlib.util
import json
import os
import io
import sqlite3
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
import uuid
from contextlib import closing
from unittest.mock import patch


def wire_bytes(name):
    directory = os.environ.get("RH_WIRE_FIXTURES")
    if not directory:
        raise RuntimeError("Use go test -count=1 -run '^TestReusableHandoffWire$' ./internal/cli (python3 and fresh producer fixtures required)")
    return (Path(directory) / (name + ".json")).read_bytes()


def wire(name):
    return json.loads(wire_bytes(name))


def true_cli(directory):
    """Private no-op CLI; a host /usr/bin/true may be a symlink, which CLI() must reject."""
    path = Path(directory) / "true-cli"
    path.write_bytes(b"#!/bin/sh\nexit 0\n")
    path.chmod(0o700)
    return path


def list_row(session):
    """Change scenario values on a real CLI list row, never copy detail keys."""
    row = copy.deepcopy(next(row for row in wire("list")["data"] if row["id"] == "new"))
    for key in row:
        if key in session and key not in ("activity", "role"):
            row[key] = session[key]
    row["role"] = session["kind"]
    row["activity"] = session["activity"]["state"]
    return row


SPEC = importlib.util.spec_from_file_location("wire_handoff", Path(__file__).parents[1] / "handoff.py")
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)


class WireContractTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.root.chmod(0o700)
        h.publish(self.root / "run.json", b'{"port":1}')
        self.cli = h.CLI(true_cli(self.root), "old", self.root, self.root / "run.json", self.root)
        self.cli.execution = h.Journal(self.root, str(uuid.uuid4())).register("_run", "old")
        self.cli.execution.bindings = {"old", "worker", "new"}
        self.cli.execution.request_target = dict(harness="codex", account="acct", model="model", effort="high", approval_mode="bypass-permissions")
        self.addCleanup(self.cli.execution.close)
        self.cli.execution.refresh_authority = {"path": "synthetic-human-scope", "sha256": "a" * 64}

    def transport(self, name):
        return patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=wire_bytes(name), stderr=b""))

    def http_transport(self, document):
        return patch.object(h.urllib.request.OpenerDirector, "open", side_effect=lambda *a, **kw: io.BytesIO(h.canonical(document)))

    def final_snapshot(self, run):
        snap = wire("active")
        running = next(turn["id"] for turn in snap["transcript"]["turns"] if turn["state"] == "running")
        handle = run.run_id + ":final:old"
        payload = h.canonical({"phase": "final"}).decode()
        directory = run.root / "control/final-old"
        h.publish(directory / "payload.json", payload.encode())
        h.publish(directory / "intent.json", h.canonical({"argv": ["send", "--session", "old", "--steer",
                  "--client-message-id", handle, "--message", payload]}))
        h.publish(directory / "result.json", b"{}")
        snap["transcript"]["entries"].append(dict(kind="message", role="user", origin="automation", id="dispatch",
                senderSessionId="old", senderProjectId=h.PROJECT, turnId=running, sequence=4,
                clientMessageId=handle, text="[from old] " + payload))
        return h.canonical(snap)

    def test_admission_contention_proves_no_dispatch_and_releases_to_one_attempt(self):
        execution = self.cli.execution
        execution.phase = "retire"
        journal = execution.journal
        argv = ["session", "kill", "old", "--project", h.PROJECT]
        effect = journal.run_id + ":kill"
        delivered = []
        flock = h.fcntl.flock
        def nonblocking(fd, operation):
            self.assertEqual(operation, h.fcntl.LOCK_EX | h.fcntl.LOCK_NB)
            return flock(fd, operation)

        with (journal.root / "admission.lock").open("r+b") as held, self.transport("kill") as transport:
            flock(held, h.fcntl.LOCK_EX)
            with patch.object(h.fcntl, "flock", side_effect=nonblocking), patch.object(h, "ADMISSION_LIMIT", 0.02):
                with self.assertRaisesRegex(h.AdmissionBusy, "this attempt not dispatched"):
                    self.cli.call(argv, effect_id=effect, on_dispatch=lambda: delivered.append(effect))
                with self.assertRaises(h.AdmissionBusy):
                    self.cli.call(argv, deadline=h.time.monotonic(), effect_id=effect, on_dispatch=lambda: delivered.append(effect))
            self.assertEqual(execution.effect_attempts(effect), [])
            self.assertEqual(delivered, [])
            transport.assert_not_called()
            with patch.object(h.time, "sleep", side_effect=lambda _: flock(held, h.fcntl.LOCK_UN)):
                self.cli.call(argv, effect_id=effect, on_dispatch=lambda: delivered.append(effect))
            with self.assertRaisesRegex(h.Stop, "effect already attempted"):
                self.cli.call(argv, effect_id=effect, on_dispatch=lambda: delivered.append(effect))
            self.assertEqual(delivered, [effect])
            self.assertEqual(len(execution.effect_attempts(effect)), 1)
            transport.assert_called_once()

    def test_spawn_after_admission_timeout_reloads_the_original_intent_once(self):
        journal = self.cli.execution.journal
        request = h.canonical(dict(request_id=journal.run_id, old_id="old", workers=[],
                                   target=dict(self.cli.execution.request_target, mode="chat"),
                                   authority={"source_refs": ["human"]}))
        h.publish(self.root / "start.json", request)
        h.publish(self.root / "state.json", h.canonical(dict(start_sha256=h.digest(request), stage="spawn",
                  handoff={"path": "handoff.md"}, manifest={"path": "manifest.json"})))
        run = h.Run(self.root, self.cli)
        directory = self.root / "control/spawn"
        sessions = []
        sent = []

        def spawn(argv, **kwargs):
            self.assertEqual(argv[1:], original["argv"])
            sent.append(argv)
            name = argv[argv.index("--name") + 1]
            sessions.append(dict(id="new", projectId=h.PROJECT, role="orchestrator", displayName=name, createdAt=h.now()))
            return SimpleNamespace(returncode=0, stdout=b"spawned session new", stderr=b"")

        def get(path):
            if path == "sessions/new":
                return {"session": dict(harness="codex", mode="chat", isTerminated=False)}
            self.assertEqual(path, "agents/accounts/sessions/new")
            return {h.provider(run.target): "acct"}

        with patch.object(self.cli, "sessions", side_effect=lambda: list(sessions)), patch.object(self.cli, "get", side_effect=get), \
             patch.object(h.subprocess, "run", side_effect=spawn):
            with (journal.root / "admission.lock").open("r+b") as held, patch.object(h, "ADMISSION_LIMIT", 0.01):
                h.fcntl.flock(held, h.fcntl.LOCK_EX)
                with self.assertRaises(h.AdmissionBusy):
                    h.spawn_new(run)
            raw = h.private_read(directory / "intent.json")
            original = h.decode(raw)
            self.assertEqual(run.state["effects"]["spawn"], "not-dispatched")
            self.assertFalse((directory / "sent.json").exists())
            self.assertEqual(self.cli.execution.effect_attempts("spawn"), [])
            self.assertEqual(sent, [])
            self.cli.execution.close()
            self.cli.execution = journal.register("_run", "old")
            self.addCleanup(self.cli.execution.close)
            resumed = h.Run(self.root, self.cli)
            self.assertEqual(h.spawn_new(resumed), "new")
            self.assertEqual(h.spawn_new(h.Run(self.root, self.cli)), "new")
            sessions.clear()
            with self.assertRaisesRegex(h.Stop, "UNKNOWN spawn"):
                h.spawn_new(h.Run(self.root, self.cli))
            self.assertEqual(len(sent), 1)
            self.assertEqual(h.private_read(directory / "intent.json"), raw)
            self.assertEqual(len(self.cli.execution.effect_attempts("spawn")), 1)

    def test_true_cli_list_and_all_role_callers(self):
        with self.transport("list"):
            rows = self.cli.sessions()
        self.assertNotIn("kind", rows[0])
        self.assertIsInstance(rows[0]["activity"], str)
        self.assertEqual(wire("http-detail")["session"]["kind"], "orchestrator")
        self.assertNotIn("workspacePath", wire("http-detail")["session"])
        run = object.__new__(h.Run)
        run.cli = self.cli
        run.evidence = lambda *args: None
        with self.transport("list"):
            self.assertEqual([x["id"] for x in run.live_workers()], ["worker"])
        self.assertEqual(h.match_spawn(rows, "RH3[wire]", "2026-11-07T00:00:00Z"), "new")

    def test_spawn_matching_uses_list_role_not_detail_kind(self):
        self.assertEqual(h.match_spawn(wire("list")["data"], "RH3[wire]", "2026-11-07T00:00:00Z"), "new")

    def test_list_rejects_malformed_or_hidden_population(self):
        good = wire("list")
        bad = [None, [], {"sessions": good["data"]}, dict(good, data=None), dict(good, meta=None)]
        for key in ("data", "meta"):
            value = copy.deepcopy(good)
            del value[key]
            bad.append(value)
        for key, value in (("role", None), ("role", "unknown"), ("id", ""), ("projectId", None), ("isTerminated", "false")):
            doc = copy.deepcopy(good)
            doc["data"][0][key] = value
            bad.append(doc)
        for key in good["meta"]:
            for value in (None, True, 1):
                doc = copy.deepcopy(good)
                doc["meta"][key] = value
                bad.append(doc)
        for doc in bad:
            with self.subTest(doc=doc), patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=h.canonical(doc), stderr=b"")):
                with self.assertRaises(h.Stop):
                    self.cli.sessions()
        empty = dict(good, data=[])
        with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=h.canonical(empty), stderr=b"")):
            self.assertEqual(self.cli.sessions(), [])

    def test_finalize_notifies_only_true_list_workers(self):
        self.cli.execution.close()
        self.cli.execution = self.cli.execution.journal.register("finalize", "old")
        self.addCleanup(self.cli.execution.close)
        self.cli.execution.phase = "final"
        self.cli.execution.bindings = {"old", "new", "worker"}
        run = SimpleNamespace(cli=self.cli, root=self.root, run_id="wire", old="old",
                              state={"new_id": "old", "nonce_completed": True, "bootstrap_sequence": 3, "cli_sha256": self.cli.sha256},
                              request={"workers": []})
        active = self.final_snapshot(run)
        calls = []
        def transport(args, **kwargs):
            calls.append(args[1:])
            name = "list" if args[1:3] == ["session", "ls"] else "active" if args[1:3] == ["session", "get"] else "send"
            return SimpleNamespace(returncode=0, stdout=active if name == "active" else wire_bytes(name), stderr=b"")
        with patch.object(h, "CLI", return_value=self.cli), patch.object(h.subprocess, "run", side_effect=transport), \
             patch.dict(os.environ, AO_SESSION_ID="old", TMPDIR=str(self.root / "bootstrap/new/draft")):
            result = h.producer(run, "finalize")
        self.assertEqual([n["worker_id"] for n in result["notifications"]], ["worker"])
        self.assertEqual([a[a.index("--session") + 1] for a in calls if a[0] == "send"], ["worker"])

    def test_finalize_records_only_controller_not_ready_as_refused(self):
        # B1: a worker whose agent is not running refuses the steer; final still completes and the hold stays.
        refused = b"the agent controller for this session is not running (CHAT_CONTROLLER_NOT_READY) [request r-1]\n"
        cases = {"refused": (1, refused), "other-code": (1, b"session not found (SESSION_NOT_FOUND) [request r-2]\n"),
                 "other-exit": (75, refused), "before-dispatch": None}
        self.cli.execution.close()
        for case, outcome in cases.items():
            with self.subTest(case=case):
                execution = self.cli.execution.journal.register("finalize", "old")
                self.addCleanup(execution.close)
                execution.phase, execution.bindings = "final", {"old", "new", "worker"}
                self.cli.execution = execution
                root = h.mkdir(self.root / case)
                run = SimpleNamespace(cli=self.cli, root=root, run_id="wire-" + case, old="old",
                                      state={"new_id": "old", "nonce_completed": True, "bootstrap_sequence": 3, "cli_sha256": self.cli.sha256}, request={"workers": []})
                active = self.final_snapshot(run)
                sends = []
                def transport(args, **kwargs):
                    if args[1] == "send":
                        sends.append(args)
                        return SimpleNamespace(returncode=outcome[0], stdout=b"", stderr=outcome[1])
                    name = "list" if args[1:3] == ["session", "ls"] else "active"
                    return SimpleNamespace(returncode=0, stdout=active if name == "active" else wire_bytes(name), stderr=b"")
                original = execution.dispatch
                def dispatch(cli, descriptor, *args, **kwargs):
                    if outcome is None and descriptor["parameters"]["argv"][0] == "send":
                        raise h.Stop("synthetic refusal before dispatch")
                    return original(cli, descriptor, *args, **kwargs)
                with patch.object(h, "CLI", return_value=self.cli), patch.object(h.subprocess, "run", side_effect=transport), \
                     patch.object(execution, "dispatch", side_effect=dispatch), \
                     patch.dict(os.environ, AO_SESSION_ID="old", TMPDIR=str(root / "bootstrap/new/draft")):
                    if case != "refused":
                        with self.assertRaises(h.Stop):
                            h.producer(run, "finalize")
                        self.assertFalse((root / "bootstrap/new/final.json").exists())
                        continue
                    result = h.producer(run, "finalize")
                    receipt = h.decode(h.private_read(root / "bootstrap/new/notifications/worker/result.json"))
                    self.assertEqual((receipt["accepted"], receipt["error_code"]), (False, "CHAT_CONTROLLER_NOT_READY"))
                    self.assertEqual([n["accepted"] for n in result["notifications"]], [False])
                    (root / "bootstrap/new/final.json").unlink()
                    h.producer(run, "finalize")
                self.assertEqual(len(sends), 1 if outcome else 0)

    def test_finalize_recovers_a_journaled_refusal_after_a_crash_without_resending(self):
        # dispatch persisted the CHAT_CONTROLLER_NOT_READY result, then the helper died before the notification receipt.
        refused = b"the agent controller for this session is not running (CHAT_CONTROLLER_NOT_READY) [request r-1]\n"
        self.cli.execution.close()
        run = SimpleNamespace(cli=self.cli, root=self.root, run_id="wire", old="old", request={"workers": []},
                              state={"new_id": "old", "nonce_completed": True, "bootstrap_sequence": 3, "cli_sha256": self.cli.sha256})
        active = self.final_snapshot(run)
        result = self.root / "bootstrap/new/notifications/worker/result.json"
        sends = []
        def transport(args, **kwargs):
            if args[1] == "send":
                sends.append(args)
                return SimpleNamespace(returncode=1, stdout=b"", stderr=refused)
            return SimpleNamespace(returncode=0, stdout=wire_bytes("list") if args[1:3] == ["session", "ls"] else active, stderr=b"")
        original = h.publish
        def crash(path, raw):
            if Path(path) == result:
                raise OSError("synthetic crash before the notification receipt")
            return original(path, raw)
        for attempt in ("crash", "recover"):
            execution = self.cli.execution.journal.register("finalize", "old")
            self.addCleanup(execution.close)
            execution.phase, execution.bindings = "final", {"old", "new", "worker"}
            self.cli.execution = execution
            with patch.object(h, "CLI", return_value=self.cli), patch.object(h.subprocess, "run", side_effect=transport), \
                 patch.object(h, "publish", side_effect=crash if attempt == "crash" else original), \
                 patch.dict(os.environ, AO_SESSION_ID="old", TMPDIR=str(self.root / "bootstrap/new/draft")):
                if attempt == "crash":
                    with self.assertRaisesRegex(OSError, "synthetic crash"):
                        h.producer(run, "finalize")
                    execution.close()
                    continue
                final = h.producer(run, "finalize")
        receipt = h.decode(h.private_read(result))
        self.assertEqual((receipt["accepted"], receipt["error_code"]), (False, "CHAT_CONTROLLER_NOT_READY"))
        self.assertEqual([n["accepted"] for n in final["notifications"]], [False])
        self.assertEqual(len(sends), 1)

    def test_actor_adapter_keeps_the_run_cli_hash_across_an_update_while_waiting(self):
        # The run CLI was checked at load; AO is updated while the producer waits for new_id.
        self.cli.execution.close()
        self.cli.execution = self.cli.execution.journal.register("finalize", "old")
        self.addCleanup(self.cli.execution.close)
        state = {"nonce_completed": True, "bootstrap_sequence": 3, "cli_sha256": self.cli.sha256}
        h.publish(self.root / "state.json", h.canonical(dict(state, new_id="old")))
        run = SimpleNamespace(cli=self.cli, root=self.root, run_id="wire", old="old", state=dict(state), request={"workers": []})
        before = h.digest(self.cli.executable.read_bytes())
        def update_while_waiting(_):
            self.cli.executable.write_bytes(b"#!/bin/sh\n# updated AO\nexit 0\n")
        with patch.object(h.time, "sleep", side_effect=update_while_waiting), patch.object(h.subprocess, "run") as transport, \
             patch.dict(os.environ, AO_SESSION_ID="old", TMPDIR=str(self.root / "bootstrap/new/draft")), \
             self.assertRaisesRegex(h.Stop, "AO CLI changed since run start: .* " + before[:8] + " → "):
            h.producer(run, "finalize")
        transport.assert_not_called()

    def test_http_DTOs_support_both_targets_and_fail_closed(self):
        self.cli.execution.registration["role"] = "start"
        for harness in ("codex", "claude-code"):
            provider = "codex" if harness == "codex" else "claude"
            target = dict(harness=harness, account="acct", model="model", effort="high", approval_mode="bypass-permissions", mode="chat")
            with self.transport("accounts"), self.http_transport(wire(provider + "-models")):
                # Help is a real Cobra output too; do not replace target_preflight.
                def command(args, **kwargs):
                    name = "help" if args[1] == "spawn" else "accounts"
                    return SimpleNamespace(returncode=0, stdout=wire_bytes(name), stderr=b"")
                with patch.object(h.subprocess, "run", side_effect=command):
                    self.assertEqual(h.target_preflight(self.cli, target)["account"]["provider"], provider)
            for missing in ("account", "model", "effort"):
                accounts, catalog = wire("accounts"), wire(provider + "-models")
                if missing == "account":
                    accounts["accounts"] = []
                elif missing == "model":
                    catalog["models"] = []
                else:
                    catalog["models"][0].pop("efforts")
                def command(args, **kwargs):
                    return SimpleNamespace(returncode=0, stdout=wire_bytes("help") if args[1] == "spawn" else h.canonical(accounts), stderr=b"")
                with patch.object(h.subprocess, "run", side_effect=command), self.http_transport(catalog), self.assertRaises(h.Stop):
                    h.target_preflight(self.cli, target)
            database = self.root / (provider + ".db")
            with closing(sqlite3.connect(database)) as db, db:
                db.execute("CREATE TABLE sessions(id,project_id,harness,provider_conversation_id)")
                db.execute("INSERT INTO sessions VALUES('old',?,?, 'native')", (h.PROJECT, harness))
            snapshot = wire("claude-snapshot" if provider == "claude" else "snapshot")
            binding, settings = wire(provider + "-binding"), wire("conversation")
            def http(path, *a, **kw):
                return io.BytesIO(h.canonical(binding if "/accounts/" in path else settings))
            with patch.object(h.urllib.request.OpenerDirector, "open", side_effect=http):
                self.assertEqual(h.verify_target(self.cli, "old", target, snapshot, database)["native_id"], "native")
                for field in ("approvalMode", "model", "reasoningEffort"):
                    original = settings["settings"].pop(field)
                    with self.assertRaises(h.Stop):
                        h.verify_target(self.cli, "old", target, snapshot, database)
                    settings["settings"][field] = original
                binding["bindings"].pop(provider)
                with self.assertRaises(h.Stop):
                    h.verify_target(self.cli, "old", target, snapshot, database)

    def test_managed_original_bytes_hash_path_and_lease(self):
        documents, reply = wire("documents")["documents"], wire("managed")
        documents["sharedDir"] = str(self.root)
        content = reply["file"]["content"].encode()
        handoff = self.root / reply["file"]["path"]
        h.publish(handoff, content)
        original = self.root / "managed.json"
        h.publish(original, wire_bytes("managed"))
        receipt = dict(managed_reply=dict(path=str(original), sha256=h.digest(wire_bytes("managed"))),
                       managed_receipt=dict(path=str(handoff), sha256=h.digest(content)), lease=h.lease_view(documents))
        docs = dict(documents, lease=h.lease_view(documents))
        self.assertEqual(h.verify_managed(receipt, "wire", docs), content)
        for bad in (dict(docs, lease={}), dict(docs, sharedDir=str(self.root / "wrong"))):
            with self.assertRaises(h.Stop):
                h.verify_managed(receipt, "wire", bad)
        receipt["managed_receipt"]["sha256"] = "0" * 64
        with self.assertRaises(h.Stop):
            h.verify_managed(receipt, "wire", docs)

    def test_original_source_pagination_revision_hash_and_turn(self):
        snapshot = wire("snapshot")
        raw = wire_bytes("snapshot")
        source = dict(kind="human-message", conversation_id="conv", branch_id="branch", message_id="human",
                      revision=1, message_text_sha256=h.digest(b"handoff"))
        self.assertEqual(h.source_message(source, raw)["id"], "human")
        for changed in (dict(source, revision=2), dict(source, message_text_sha256="0" * 64)):
            with self.assertRaises(h.Stop):
                h.source_message(changed, raw)
        request = dict(old_id="old", sources=[source], prepared_by=dict(turn_id="preparing"), workspace_index=[])
        pages = iter((wire_bytes("page"), wire_bytes("before")))
        with patch.object(h.subprocess, "run", side_effect=lambda *a, **kw: SimpleNamespace(returncode=0, stdout=next(pages), stderr=b"")):
            h.validate_sources_live(request, self.cli)
        ctl = snapshot["transcript"]["entries"][1]
        source.update(kind="old-control-record", message_id=ctl["id"], message_text_sha256=h.digest(ctl["text"].encode()), ref="workspace")
        request["workspace_index"] = [dict(source_refs=["workspace"])]
        request["prepared_by"]["turn_id"] = "wrong"
        with self.transport("snapshot"), self.assertRaises(h.Stop):
            h.validate_sources_live(request, self.cli)

    def test_project_DTO_location_and_degraded_boundary(self):
        documents = wire("documents")
        documents["documents"]["sharedDir"] = str(self.root / "projects" / h.PROJECT / "orchestrator/shared")
        with self.http_transport(wire("project")), patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=h.canonical(documents), stderr=b"")):
            self.assertEqual(h.location_info(self.cli)["managed_root"], str(self.root / "worktrees"))
        with self.http_transport(wire("degraded")), self.assertRaises(h.Stop):
            h.location_info(self.cli)
        project = wire("project")
        project["project"]["config"]["preRemove"] = ["unknown-hook"]
        with self.http_transport(project), self.assertRaisesRegex(h.Stop, "PreRemove"):
            h.old_environment(SimpleNamespace(cli=self.cli), self.root, self.root)

    def test_control_stdout_is_not_required_to_be_JSON(self):
        for name in ("send", "spawn", "exit", "report"):
            with self.subTest(name=name), self.transport(name):
                self.assertEqual(self.cli.call({"send": ["send", "--session", "old", "--steer", "--client-message-id", "handle", "--message", "fixture"], "spawn": ["spawn", "--project", h.PROJECT, "--kind", "orchestrator", "--agent", "codex", "--account", "acct", "--mode", "chat", "--model", "model", "--effort", "high", "--approval-mode", "bypass-permissions", "--name", "fixture", "--prompt", "fixture"], "exit": ["session", "exit-agent", "old", "--project", h.PROJECT, "--json"], "report": ["report", "--needs-input", "--note", "fixture"]}[name], effect_id=name), wire_bytes(name))

    def test_detail_and_transcript_are_real_cli_projections(self):
        for name in ("snapshot", "claude-snapshot", "active", "page", "before"):
            with self.subTest(name=name), self.transport(name):
                snap = self.cli.session("old")
                self.assertEqual(snap["session"]["kind"], "orchestrator")
                self.assertIsInstance(snap["session"]["activity"], dict)
                self.assertNotIn("workspacePath", snap["session"])
                self.assertNotIn("workspace_path", snap["session"])
                if name == "active":
                    self.assertFalse(h.quiescent(snap))
                    self.assertTrue(snap["transcript"]["incomplete"])
                if name == "page":
                    self.assertTrue(snap["transcript"]["hasMoreBefore"])
                    self.assertGreater(snap["transcript"]["next"]["before"], 0)
        self.assertTrue(h.known_turn(wire("snapshot")["transcript"]["turns"][0], dict(model="model", effort="high")))

    def test_documents_and_resume_keep_real_envelopes(self):
        with self.transport("documents"):
            documents = self.cli.documents()
        self.assertEqual(h.lease_view(documents), dict(session_id="old", epoch=1, frozen=False))
        with self.transport("resume"):
            result = h.decode(self.cli.call(["session", "resume-agent", "old", "--project", h.PROJECT, "--json"], effect_id="resume"))
        self.assertEqual(result["resumeMode"], "native")
        for name, text in [("kill", "killed"), ("kill-preserved", "workspace preserved"), ("kill-pending", "workspace cleanup pending")]:
            with self.subTest(name=name), self.transport(name):
                self.assertIn(text.encode(), self.cli.call(["session", "kill", "old", "--project", h.PROJECT], effect_id=name))


if __name__ == "__main__":
    unittest.main()
