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
                              state={"new_id": "old", "nonce_completed": True, "bootstrap_sequence": 3},
                              request={"workers": []})
        calls = []
        def transport(args, **kwargs):
            calls.append(args[1:])
            name = "list" if args[1:3] == ["session", "ls"] else "active" if args[1:3] == ["session", "get"] else "send"
            return SimpleNamespace(returncode=0, stdout=wire_bytes(name), stderr=b"")
        with patch.object(h, "CLI", return_value=self.cli), patch.object(h.subprocess, "run", side_effect=transport), \
             patch.dict(os.environ, AO_SESSION_ID="old", TMPDIR=str(self.root / "bootstrap/new/draft")):
            result = h.producer(run, "finalize")
        self.assertEqual([n["worker_id"] for n in result["notifications"]], ["worker"])
        self.assertEqual([a[a.index("--session") + 1] for a in calls if a[0] == "send"], ["worker"])

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
            with sqlite3.connect(database) as db:
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
