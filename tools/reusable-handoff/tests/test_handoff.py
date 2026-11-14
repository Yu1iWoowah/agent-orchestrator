"""RH-SCRIPT1 protocol proofs; never connects to an installed AO daemon."""
from contextlib import closing
import copy
import importlib.util
import json
import os
import signal
import sqlite3
import subprocess
from pathlib import Path
import tempfile
import time
import unittest
import uuid
from unittest.mock import patch

from test_wire_contract import wire, wire_bytes, list_row, true_cli
from producer_transport import ActorTransport, activity as produced_activity, directive as persisted_directive

SPEC = importlib.util.spec_from_file_location("handoff", Path(__file__).parents[1] / "handoff.py")
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)


def target(harness="codex"):
    return dict(harness=harness, account="acct", model="model", effort="high",
                approval_mode="bypass-permissions", mode="chat")


def snapshot(state="idle", turns=None, entries=None):
    turns = turns if turns is not None else [{"id": "turn", "state": "completed", "completedAt": "2026-11-08T00:00:00Z"}]
    entries = entries or []
    incomplete = [{"type": "turn", "id": t["id"], "state": t["state"]} for t in turns if t["state"] in ("queued", "running")]
    for entry in entries:
        reason = "streaming" if entry.get("streaming") else "running" if entry.get("status") == "running" else None
        if not reason and entry.get("status") == "pending" and entry.get("activityKind") in ("approval", "user_input"):
            reason = "pending"
        if reason:
            item = {"type": "entry", "sequence": entry.get("sequence", 1), "reason": reason}
            if reason == "pending" and entry.get("requestId"):
                item["requestId"] = entry["requestId"]
            incomplete.append(item)
    result = wire("active" if state == "active" else "snapshot")
    result["session"].update(id="old", projectId=h.PROJECT, kind="orchestrator", harness="codex",
                             mode="chat", isTerminated=False, branch="fixture", status="working" if state == "active" else "idle")
    result["session"]["activity"]["state"] = state
    result["transcript"].update(conversationId="conv", activeBranchId="branch", latestSequence=1,
                                 hasMoreBefore=False, entries=entries, returned=len(entries), turns=turns, incomplete=incomplete)
    result["transcript"].pop("next", None)
    return result


class ProtocolTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.root.chmod(0o700)

    def request(self):
        old_path = self.root / "worktrees" / h.PROJECT / "orchestrator/old"
        raw = json.dumps(snapshot(entries=[dict(id="human", role="user", origin="human", text="handoff", revision=1),
            dict(id="workspace", role="assistant", turnId="preparing", text="old " + str(old_path), revision=1)])).encode()
        evidence = self.root / "evidence"
        evidence.mkdir(mode=0o700)
        h.publish(evidence / "human.json", raw)
        return {"schema": "rh-start-request/1", "request_id": str(uuid.uuid4()),
                "project_id": h.PROJECT, "old_id": "old",
                "prepared_by": {"session_id": "old", "turn_id": "preparing"},
                "target": target(), "workers": [],
                "workspace_index": [{"session_id": "old", "kind": "orchestrator", "path": str(old_path), "source_refs": ["workspace"]}],
                "authority": {"source_refs": ["human"], "old_verified_scope": "three-step-handoff-this-project",
                              "t0_source_ref": "human", "t0_created_at": None, "t0_precision": "UNKNOWN"},
                "sources": [{"ref": "human", "kind": "human-message", "conversation_id": "conv",
                             "branch_id": "branch", "message_id": "human", "revision": 1,
                             "ask_ref": None, "raw_relpath": "evidence/human.json",
                             "raw_sha256": h.digest(raw), "message_text_sha256": h.digest(b"handoff")},
                            {"ref": "workspace", "kind": "old-control-record", "conversation_id": "conv", "branch_id": "branch",
                             "message_id": "workspace", "revision": 1, "raw_relpath": "evidence/human.json", "raw_sha256": h.digest(raw),
                             "message_text_sha256": h.digest(("old " + str(old_path)).encode())}]}

    def worker_index(self, run, sid="worker"):
        path = self.root / "worktrees" / h.PROJECT / sid
        run.request["workspace_index"].append({"session_id": sid, "kind": "worker", "path": str(path), "source_refs": ["human"]})
        return path

    def test_T04_request_binding_and_unknown_source_settings(self):
        req = self.request()
        h.validate_request(req, self.root, "old", target())
        self.assertEqual(h.old_settings(snapshot())["model"]["value"], "UNKNOWN")
        req["target"]["model"] = "other"
        with self.assertRaises(h.Stop):
            h.validate_request(req, self.root, "old", target())

    def run_fixture(self):
        req = self.request()
        h.publish(self.root / "start.json", h.canonical(req))
        h.publish(self.root / "state.json", h.canonical({"start_sha256": h.digest(h.canonical(req)),
                  "effects": {}, "target": target(), "old_id": "old", "run_id": req["request_id"]}))
        class FakeCLI:
            calls = []
            fail = False
            last_received = 0

            def call(self, args, timeout=30, deadline=None, on_dispatch=None, **kwargs):
                if on_dispatch:
                    on_dispatch()
                self.calls.append(args)
                if self.fail:
                    raise subprocess.TimeoutExpired(args, timeout)
                return b'{"accepted":true}'
        return h.Run(self.root, FakeCLI())

    def test_retired_UUID_is_rejected_before_cached_state_or_qualification(self):
        req = self.request()
        root = h.mkdir(self.root / "runs")
        slot = h.mkdir(root / ".start/old")
        h.publish(slot / "evidence/human.json", h.private_read(self.root / "evidence/human.json"))
        h.publish(slot / "request.json", h.canonical(req))
        retired = h.mkdir(root / ".voided" / req["request_id"])
        with patch.object(h, "qualified") as qualified:
            # Even an incomplete archive reserves the retired UUID.
            with self.assertRaisesRegex(h.Stop, "retired"):
                h.start(root, None, "old", target(), req["request_id"])
            h.publish(retired / "start.json", h.canonical(req))
            destination = h.mkdir(root / req["request_id"])
            h.publish(destination / "start.json", h.canonical(req))
            h.publish(destination / "state.json", h.canonical({"outcome": "pending"}))
            with self.assertRaisesRegex(h.Stop, "retired"):
                h.start(root, None, "old", target(), req["request_id"])
            qualified.assert_not_called()

    def test_new_UUID_ignores_hidden_archive_but_not_unresolved_active_run(self):
        req = self.request()
        root = h.mkdir(self.root / "runs")
        slot = h.mkdir(root / ".start/old")
        h.publish(slot / "evidence/human.json", h.private_read(self.root / "evidence/human.json"))
        h.publish(slot / "request.json", h.canonical(req))
        h.mkdir(root / ".voided" / str(uuid.uuid4()))
        with patch.object(h, "qualified", side_effect=h.Stop("qualification reached")):
            with self.assertRaisesRegex(h.Stop, "qualification reached"):
                h.start(root, None, "old", target(), req["request_id"])
        prior = h.mkdir(root / str(uuid.uuid4()))
        h.publish(prior / "state.json", h.canonical({"old_id": "old", "outcome": "pending"}))
        with patch.object(h, "qualified") as qualified, self.assertRaisesRegex(h.Stop, "unresolved run"):
            h.start(root, None, "old", target(), req["request_id"])
        qualified.assert_not_called()

    def test_T06_unknown_effect_never_replayed(self):
        run = self.run_fixture()
        run.cli.fail = True
        with self.assertRaises(subprocess.TimeoutExpired):
            run.effect("notify", ["send", "original"], 60)
        run.cli.fail = False
        with self.assertRaises(h.Stop):
            run.effect("notify", ["send", "original"], 60)
        self.assertEqual(len(run.cli.calls), 1)

    def test_T06_accepted_effect_not_repeated(self):
        run = self.run_fixture()
        run.effect("notify", ["send", "original"], 60)
        run.effect("notify", ["send", "original"], 60)
        self.assertEqual(len(run.cli.calls), 1)

    def test_audit_guard_failure_is_not_dispatched_and_can_continue(self):
        run = self.run_fixture()
        for key in ("kill", "exit"):
            args = ["session", key, "old"]
            with self.assertRaisesRegex(h.Stop, "guard not ready"):
                run.effect(key, args, 60, lambda: h.require(False, "guard not ready"))
            self.assertFalse((run.root / "control" / key / "sent.json").exists())
            self.assertEqual(run.state["effects"][key], "not-dispatched")
            run.rearm_wait()
            continued = h.Run(run.root, run.cli)
            continued.effect(key, args, 60)
            self.assertEqual(continued.state["effects"][key], "accepted")
            run = continued
        self.assertEqual(len(run.cli.calls), 2)

    def test_audit_dispatch_deadline_checks_before_starting_subprocess(self):
        run_file = self.root / "fake-running.json"
        h.publish(run_file, b'{"port":1}')
        cli = h.CLI(true_cli(self.root), "old", self.root, run_file, self.root)
        cli.execution = h.Journal(self.root, str(uuid.uuid4())).register("start", "old")
        self.addCleanup(cli.execution.close)
        marked, clock = [], [0]
        def dispatched():
            marked.append(True)
            clock[0] = 2
        with patch.object(h.time, "monotonic", side_effect=lambda: clock[0]), patch.object(h.subprocess, "run") as invoked:
            with self.assertRaises(h.WindowElapsed):
                cli.call(["spawn", "--help"], deadline=1, on_dispatch=dispatched)
        self.assertEqual(marked, [True])
        invoked.assert_not_called()

    def test_audit_continue_rechecks_export_after_idle_guard_timeout(self):
        run = self.run_fixture()
        run.clock = lambda: 1000
        run.state.update(waiting_phase="old_idle", completed_stages=["workers", "2a", "export"], waits={
            "old_idle": {"started_at": "original", "monotonic": 0, "boot_id": h.boot_id(), "limit": 900},
            "export": {"started_at": "original", "monotonic": 0, "boot_id": h.boot_id(), "limit": 600, "completed_at": "original-finish"}})
        original_t0 = copy.deepcopy(run.request["authority"])
        run.rearm_wait()
        self.assertEqual({x["phase"] for x in run.state["wait_history"]}, {"old_idle", "export"})
        self.assertTrue(run.wait("export", lambda: True))
        self.assertEqual(run.state["waits"]["export"]["monotonic"], 1000)
        self.assertEqual(run.request["authority"], original_t0)

    def test_audit_expired_dispatch_marker_is_not_an_unknown_effect(self):
        run = self.run_fixture()
        original = run.cli.call
        def expired(args, timeout=30, deadline=None, on_dispatch=None, **kwargs):
            on_dispatch()
            raise h.WindowElapsed()
        run.cli.call = expired
        guards = []
        def guard():
            guards.append(True)
            h.require(len(guards) == 1, "guard not ready")
            return 1
        with self.assertRaisesRegex(h.Stop, "guard not ready"):
            run.effect("kill", ["session", "kill", "old"], 60, guard)
        self.assertEqual(run.state["effects"]["kill"], "not-dispatched")
        self.assertTrue((run.root / "control/kill/sent.json").exists())
        run.cli.call = original
        run.effect("kill", ["session", "kill", "old"], 60)
        self.assertEqual(len(run.cli.calls), 1)

    def test_audit_all_active_workers_receive_stop_in_same_round(self):
        run = self.run_fixture()
        template = next(row for row in wire("list")["data"] if row["id"] == "worker")
        workers = [dict(template, id=sid) for sid in ("w1", "w2")]
        run.cli.sessions = lambda: workers
        def active(sid):
            snap = snapshot("active", turns=[{"id": "busy", "state": "running"}])
            snap["session"].update(id=sid, kind="worker")
            return snap
        run.cli.session = active
        run.request["workers"] = [dict(worker_id=sid, reserved_stop_d="D-7") for sid in ("w1", "w2")]
        for sid in ("w1", "w2"):
            self.worker_index(run, sid)
        self.assertFalse(run.workers())
        self.assertEqual({args[args.index("--session") + 1] for args in run.cli.calls}, {"w1", "w2"})
        self.assertFalse(run.workers())
        self.assertEqual(len(run.cli.calls), 2)

    def test_C3_public_branch_is_evidence_not_workspace_identity(self):
        fake = FlowFixture(self.root, "codex", "codex")
        self.assertNotIn("workspacePath", fake.get("sessions/old")["session"])
        self.assertEqual(fake.run.workspace("old"), str(fake.workspace))
        subprocess.run(["git", "-C", str(fake.repo), "worktree", "add", "--force", str(self.root / "duplicate"), "original"], check=True, capture_output=True)
        self.assertEqual(fake.run.workspace("old"), str(fake.workspace))
        fake.snapshots["old"]["session"]["branch"] = "missing-branch"
        self.assertEqual(fake.run.workspace("old"), str(fake.workspace))

    def test_C3_root_sibling_detached_resolve_same_worker_path(self):
        fake = FlowFixture(self.root, "codex", "codex", worker=True)
        path = fake.worker_path
        self.assertEqual(fake.run.workspace("worker"), str(path))
        subprocess.run(["git", "-C", str(path), "switch", "-c", "fixture-sibling"], check=True, capture_output=True)
        self.assertEqual(fake.run.workspace("worker"), str(path))
        subprocess.run(["git", "-C", str(path), "switch", "--detach"], check=True, capture_output=True)
        self.assertEqual(fake.run.workspace("worker"), str(path))
        snap = h.git_snapshot(path, self.root / "detached-snapshot")
        self.assertIsNone(snap["branch"])
        self.assertIsNone(h.decode(h.private_read(snap["path"]))["branch"])

    def test_C3_wrong_session_repo_missing_or_nonunique_registration_stops(self):
        for failure in ("session", "repo", "missing", "duplicate", "prunable", "drift", "index"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temp:
                fake = FlowFixture(Path(temp), "codex", "codex", worker=True)
                original_get, original_git = fake.get, h.old_git
                fake.run.workspace("worker")
                if failure == "session":
                    fake.get = lambda p: {"session": dict(fake.snapshots["worker"]["session"], id="someone-else")} if p == "sessions/worker" else original_get(p)
                elif failure == "repo":
                    other = Path(temp) / "other-repository"
                    subprocess.run(["git", "init", str(other)], check=True, capture_output=True)
                    fake.get = lambda p: {"status": "ok", "project": dict(original_get(p)["project"], path=str(other))} if p == "projects/" + h.PROJECT else original_get(p)
                elif failure == "missing":
                    subprocess.run(["git", "-C", str(fake.repo), "worktree", "remove", str(fake.worker_path)], check=True, capture_output=True)
                elif failure == "drift":
                    subprocess.run(["git", "-C", str(fake.repo), "worktree", "move", str(fake.worker_path), str(fake.worker_path) + "-moved"], check=True, capture_output=True)
                elif failure == "index":
                    fake.run.request["workspace_index"] = [x for x in fake.run.request["workspace_index"] if x["session_id"] != "worker"]
                def registration(args, **kwargs):
                    raw = original_git(args, **kwargs)
                    if args[:2] == ["worktree", "list"]:
                        if failure == "duplicate":
                            return raw + raw
                        if failure == "prunable":
                            return b"\0\0".join(block + (b"\0prunable reason" if b"worktree " + str(fake.worker_path).encode() + b"\0" in block else b"") for block in raw.split(b"\0\0"))
                    return raw
                with patch.object(h, "old_git", side_effect=registration), self.assertRaises(h.Stop):
                    fake.run.workspace("worker")

    def test_C3_old_detached_still_needs_protected_original_ref(self):
        fake = FlowFixture(self.root, "codex", "codex")
        subprocess.run(["git", "-C", str(fake.workspace), "switch", "--detach"], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(fake.repo), "update-ref", "-d", "refs/heads/original"], check=True, capture_output=True)
        self.assertEqual(fake.run.workspace("old"), str(fake.workspace))
        with self.assertRaises(h.Stop):
            h.run_protocol(fake.run)
        self.assertNotIn("kill", fake.mutations)

    def test_C3_workspace_index_rejects_aliases_duplicates_and_wrong_sources(self):
        req = self.request()
        for problem in ("missing", "duplicate", "kind", "relative", "alias", "source", "wrong-turn"):
            changed = copy.deepcopy(req)
            if problem == "missing":
                del changed["workspace_index"]
            elif problem == "duplicate":
                changed["workspace_index"].append(copy.deepcopy(changed["workspace_index"][0]))
            elif problem == "kind":
                changed["workspace_index"][0]["kind"] = "worker"
            elif problem == "relative":
                changed["workspace_index"][0]["path"] = "relative"
            elif problem == "alias":
                changed["workspace_index"][0]["path"] += "/../old"
            elif problem == "source":
                changed["workspace_index"][0]["source_refs"] = ["human"]
            else:
                changed["prepared_by"]["turn_id"] = "unrelated"
            with self.subTest(problem=problem), self.assertRaises(h.Stop):
                h.validate_request(changed, self.root, "old", target())

    def test_C3_shared_configuration_crosschecks_fixed_data_root(self):
        fake = FlowFixture(self.root, "codex", "codex")
        original_get = fake.get
        fake.shared = h.mkdir(fake.data_dir / "projects" / h.PROJECT / "orchestrator/nested/shared")
        def project(path):
            value = original_get(path)
            if path == "projects/" + h.PROJECT:
                value["project"]["config"]["orchestratorHandoff"] = {"sharedDir": "nested/shared"}
            return value
        fake.get = project
        self.assertEqual(h.location_info(fake)["managed_root"], str(fake.data_dir / "worktrees"))
        self.assertEqual(h.location_info(fake)["shared_dir"], str(fake.shared))
        fake.shared = h.mkdir(self.root / "elsewhere")
        with self.assertRaises(h.Stop):
            h.location_info(fake)

    def test_C3_live_workspace_record_must_belong_to_preparing_turn(self):
        req = self.request()
        live = h.decode(h.private_read(self.root / "evidence/human.json"))
        live["transcript"]["turns"].append({"id": "preparing", "state": "running"})
        cli = type("SourceCLI", (), {})()
        cli.session = lambda _: copy.deepcopy(live)
        h.validate_sources_live(req, cli)
        next(e for e in live["transcript"]["entries"] if e["id"] == "workspace")["turnId"] = "unrelated"
        with self.assertRaisesRegex(h.Stop, "live workspace record not from preparing turn"):
            h.validate_sources_live(req, cli)

    def test_C3_empty_and_relative_data_overrides_are_fixed_at_start(self):
        home = h.mkdir(self.root / "h")
        repository = self.root / "repository"
        class LocationCLI:
            def __init__(self, executable, old, data, run_file, temp):
                self.data_dir, self.run_file = data, run_file
            def documents(self):
                return {"sharedDir": str(self.data_dir / "projects" / h.PROJECT / "orchestrator/custom")}
            def get(self, path):
                return {"status": "ok", "project": {"id": h.PROJECT, "path": str(repository),
                    "config": {"orchestratorHandoff": {"sharedDir": "custom"}}}}
        previous = Path.cwd()
        try:
            os.chdir(home)
            for override, expected in (("", home / ".ao/data"), (".ao/alternate", home / ".ao/alternate")):
                with self.subTest(override=override), patch.dict(os.environ, {"AO_DATA_DIR": override, "AO_RUN_FILE": ""}), \
                     patch.object(h.Path, "home", return_value=home), patch.object(h.shutil, "which", return_value="/usr/bin/true"), \
                     patch.object(h, "CLI", LocationCLI):
                    _, cli = h.locate("old", create=True)
                    self.assertEqual(cli.data_dir, expected)
                    self.assertEqual(h.location_info(cli)["managed_root"], str(expected / "worktrees"))
        finally:
            os.chdir(previous)

    def test_C3_worker_branch_and_HEAD_drift_invalidate_safe_stop(self):
        for change in ("branch", "head"):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as temp:
                fake = FlowFixture(Path(temp), "codex", "codex", worker=True)
                fake.run.sleep = lambda _: None
                self.assertTrue(fake.run.worker_stop({"id": "worker"}))
                args = ["switch", "-c", "different"] if change == "branch" else ["-c", "user.email=x@example.invalid", "-c", "user.name=x", "commit", "--allow-empty", "-m", "new-head"]
                subprocess.run(["git", "-C", str(fake.worker_path), *args], check=True, capture_output=True)
                with self.assertRaisesRegex(h.Stop, "worker content drift"):
                    fake.run.worker_stop({"id": "worker"})

    def test_audit_idle_window_waits_through_real_incomplete_then_completes(self):
        run = self.run_fixture()
        active = snapshot("active", turns=[{"id": "turn", "state": "running"}])
        current = [active]
        elapsed = [10]
        run.clock = lambda: elapsed[0]
        run.cli.session = lambda _: current[0]
        def sleep(_):
            elapsed[0] += 2
            current[0] = snapshot()
            run.cli.last_received = elapsed[0]
        run.sleep = sleep
        self.assertEqual(h.idle_window(run, "old", "old_idle")["turn_id"], "turn")
        self.assertEqual(run.state["waits"]["old_idle"]["elapsed"], 2)

    def test_audit_send_new_preserves_normal_incomplete_observation(self):
        run = self.run_fixture()
        run.state.update(new_id="new", bootstrap_sequence=1)
        active = snapshot("active", turns=[{"id": "busy", "state": "running"}])
        active["session"]["id"] = "new"
        run.cli.session = lambda _: active
        run.send("new", "nonce", {"challenge": "original"})
        self.assertEqual(len(run.cli.calls), 1)

    def test_T08_original_deadline_not_reset_on_poll(self):
        run = self.run_fixture()
        elapsed = [100.0]
        run.clock = lambda: elapsed[0]
        def sleep(seconds):
            elapsed[0] += 400
        run.sleep = sleep
        with self.assertRaises(h.Stop):
            run.wait("nonce", lambda: False)
        original = copy.deepcopy(run.state["waits"]["nonce"])
        with self.assertRaises(h.Stop):
            run.wait("nonce", lambda: True)
        self.assertEqual(run.state["waits"]["nonce"], original)
        self.assertNotIn("total", h.LIMITS)

    def test_T08_manual_continue_preserves_original_wait_and_nonce(self):
        run = self.run_fixture()
        elapsed = [100.0]
        run.clock = lambda: elapsed[0]
        run.sleep = lambda _: elapsed.__setitem__(0, elapsed[0] + 700)
        run.state.update(original_t0={"source": "original-human"}, nonce="only-one")
        with self.assertRaises(h.Stop):
            run.wait("nonce", lambda: False)
        run.rearm_wait()
        self.assertTrue(run.wait("nonce", lambda: True))
        self.assertEqual(run.state["wait_history"][0]["monotonic"], 100)
        self.assertEqual(run.state["nonce"], "only-one")
        self.assertEqual(run.state["original_t0"], {"source": "original-human"})

    def test_T07_new_human_input_is_not_the_initial_bootstrap_exception(self):
        snap = snapshot(entries=[{"kind": "message", "role": "user", "origin": "human", "id": "opening", "sequence": 5}])
        h.reject_new_human(snap, 5)
        snap["transcript"]["entries"].append({"kind": "message", "role": "user", "origin": "human", "sequence": 6, "text": "stop"})
        with self.assertRaisesRegex(h.Stop, "new human"):
            h.reject_new_human(snap, 5)

    def test_T08_over_300_seconds_is_not_failure(self):
        run = self.run_fixture()
        elapsed = [100.0]
        run.clock = lambda: elapsed[0]
        run.sleep = lambda _: elapsed.__setitem__(0, elapsed[0] + 350)
        self.assertTrue(run.wait("workers", lambda: elapsed[0] > 400))
        self.assertEqual(run.state["waits"]["workers"]["elapsed"], 350)

    def test_T05_sequence_or_active_report_prevents_exit(self):
        first = snapshot()
        second = copy.deepcopy(first)
        self.assertTrue(h.same_idle(first, second))
        second["transcript"]["latestSequence"] += 1
        self.assertFalse(h.same_idle(first, second))
        second = snapshot("active")
        self.assertFalse(h.same_idle(first, second))
        second = snapshot(turns=[{"id": "report", "state": "running"}])
        self.assertFalse(h.same_idle(first, second))

    def test_T06_spawn_recovery_requires_unique_precise_token_and_time(self):
        token = "RH3[" + str(uuid.uuid4()) + "]"
        item = dict(wire("list")["data"][0], id="new", projectId=h.PROJECT, role="orchestrator", displayName=token,
                    createdAt="2026-11-08T01:00:01Z")
        self.assertEqual(h.match_spawn([item], token, "2026-11-08T01:00:00Z"), "new")
        for candidates in ([], [item, item], [dict(item, displayName=token[:-1])],
                           [dict(item, createdAt="2026-11-08T00:59:59Z")]):
            with self.subTest(candidates=candidates), self.assertRaises(h.Stop):
                h.match_spawn(candidates, token, "2026-11-08T01:00:00Z")

    def test_T04_clean_runner_environment_never_inherits_grants(self):
        with patch.dict(os.environ, {"AO_SESSION_ID": "other", "AO_BROWSER_CAPABILITY": "secret",
                                     "OPENAI_API_KEY": "secret", "CLAUDE_CONFIG_DIR": "secret",
                                     "AO_SETTINGS_GRANT": "secret", "AO_TMUX_SOCKET": "stable"}):
            env = h.cli_environment("old", self.root, self.root / "run", self.root)
        self.assertEqual(env["AO_SESSION_ID"], "old")
        self.assertEqual(env["AO_PROJECT_ID"], h.PROJECT)
        self.assertFalse(any("secret" in value or value == "stable" for value in env.values()))

    def test_T02_dirty_content_drift_even_when_status_is_unchanged(self):
        workspace = self.root / "repo"
        workspace.mkdir()
        def git(*args):
            subprocess.run(["git", "-C", str(workspace), *args], check=True, capture_output=True)
        git("init", "-b", "fixture")
        (workspace / "file").write_text("base")
        git("add", "file")
        git("-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit", "-m", "fixture")
        (workspace / "file").write_text("first dirty")
        a = h.git_snapshot(workspace, self.root / "one")
        (workspace / "file").write_text("other dirty")
        b = h.git_snapshot(workspace, self.root / "two")
        self.assertNotEqual(a["sha256"], b["sha256"])
        self.assertEqual(h.decode(h.private_read(a["path"]))["status"], h.decode(h.private_read(b["path"]))["status"])

    def database(self):
        path = self.root / "fixture.db"
        with closing(sqlite3.connect(path)) as db, db:
            db.execute("CREATE TABLE sessions(id,project_id,harness,provider_conversation_id,secret)")
            db.execute("INSERT INTO sessions VALUES('old',?,'codex','native-old','NEVER_READ')", (h.PROJECT,))
            for table, columns in h.EXPORT_COLUMNS.items():
                extra = ",provider_input_text,agent_settings_snapshot_json" if table == "conversation_turns" else ""
                db.execute("CREATE TABLE " + table + "(" + ",".join(columns) + extra + ")")
                db.execute("INSERT INTO " + table + "(id,conversation_id) VALUES('one','conv')")
                db.execute("INSERT INTO " + table + "(id,conversation_id) VALUES('other','other-conv')")
            db.execute("UPDATE conversation_turns SET state='cancelled', provider_input_text='NEVER_READ',agent_settings_snapshot_json='NEVER_READ'")
            db.execute("UPDATE conversation_messages SET sequence=10")
            db.execute("UPDATE conversation_activities SET sequence=9")
            db.execute("CREATE TABLE reports(secret)")
            db.execute("INSERT INTO reports VALUES('NEVER_READ')")
        return path

    def test_T03_export_all_branches_cancellation_and_only_whitelist(self):
        path = self.database()
        result = h.export_conversation(path, "old", "conv", "codex", 10)
        self.assertNotIn(b"NEVER_READ", h.canonical(result))
        self.assertEqual(result["conversation_turns"][0]["state"], "cancelled")
        self.assertEqual(len(result["conversation_branches"]), 1)
        self.assertNotIn("reports", result)
        self.assertIn("outbox", h.OUTBOX)
        self.assertEqual(result["conversation_id"], "conv")
        with self.assertRaises(h.Stop):
            h.export_conversation(path, "old", "missing-conv", "codex", 10)
        with self.assertRaises(h.Stop):
            h.export_conversation(path, "old", "conv", "codex", 9)

    def test_review_plan_settle_sequence_gap_preserves_cli_cut(self):
        fake = FlowFixture(self.root, "codex", "claude-code")
        fake.snapshots["old"]["transcript"]["latestSequence"] = 11
        fake.snapshots["old"]["transcript"]["entries"].append({"id": "plan", "kind": "activity", "activityKind": "plan",
            "status": "completed", "sequence": 9, "turnId": "turn", "detail": {}})
        with closing(sqlite3.connect(fake.data_dir / "ao.db")) as db, db:
            db.execute("INSERT INTO conversation_activities(id,conversation_id,sequence,kind,status) VALUES('plan','conv',9,'plan','completed')")
        h.run_protocol(fake.run)
        self.assertEqual(fake.run.state["outcome"], "complete")
        for phase in ("manifest", "delta"):
            manifest = h.decode(h.private_read(fake.run.state[phase]["path"]))
            self.assertEqual(manifest["cut_sequence"], fake.snapshots["old"]["transcript"]["latestSequence"])
            raw = h.decode(h.private_read(manifest["raw"]["path"]))
            self.assertLessEqual(max(x["sequence"] for table in ("conversation_messages", "conversation_activities") for x in raw[table]), manifest["cut_sequence"])

    def test_export_bound_directory_locations_cut_delta_and_file_boundaries(self):
        fake = FlowFixture(self.root, "codex", "claude-code", worker=True)
        self.addCleanup(fake.transport.close)
        run = fake.run
        fake.snapshots["old"]["transcript"]["entries"] = [dict(id="exported", sequence=10, kind="message", role="assistant", text="synthetic context")]
        with closing(sqlite3.connect(fake.data_dir / "ao.db")) as db, db:
            db.execute("INSERT INTO conversation_messages(id,conversation_id,sequence,role,text) VALUES('exported','conv',10,'assistant','synthetic context')")
        run.workspace("worker")
        run.state.update(stops=[{"workspace": {"path": str(fake.worker_path)}}], stop_index_sha256="0" * 64)
        h.publish(run.root / "workers/index.json", b"[]")
        context = fake.shared / "context.md"
        handoff = fake.shared / "handoff.md"
        ordinary = fake.shared / "needed.txt"
        ordinary.write_bytes(b"necessary original")
        locations = {Path(v) for binding in run.state["workspace_bindings"].values()
                     for k, v in binding.items() if k in ("path", "common", "git_dir")}
        locations.update(Path(run.state["location"][key]) for key in
                         ("data_dir", "managed_root", "project_path", "shared_dir"))
        locations.update((fake.home / ".ao", run.root, run.root.parent))
        child = fake.repo / ".git/unreferenced-child"
        child.write_bytes(b"directory contents must not be copied")
        def prepare(text):
            context.write_text(text)
            handoff.write_text("[necessary context](context.md)\n[ordinary](needed.txt)")
            run.state["handoff"] = dict(path=str(handoff), sha256=h.digest(handoff.read_bytes()))
        prepare("\n".join("`" + str(path) + "`" for path in sorted(locations)) +
                "\n`" + str(fake.repo / ".git") + "`")
        # Git bindings, SQLite export, dependency traversal and manifests are real;
        # old Git preservation has its own protocol tests and is outside this gate.
        with patch.object(h.Path, "home", return_value=fake.home), patch.object(h, "capture_old", return_value={"path": "old-proof", "sha256": "0" * 64}):
            for delta in (False, True):
                ref = h.export_material(run, delta=delta)
                manifest = h.decode(h.private_read(ref["path"]))
                self.assertEqual(manifest["schema"], "rh-manifest/1")
                self.assertEqual({row["source"] for row in manifest["directory_references"]}, {str(p) for p in locations})
                self.assertEqual(len(manifest["directory_references"]), len(locations))
                self.assertTrue(all(row["kind"] == "directory-reference" for row in manifest["directory_references"]))
                self.assertEqual({row["source"] for row in manifest["dependencies"]}, {str(handoff), str(context), str(ordinary)})
                for row in manifest["dependencies"]:
                    self.assertEqual(h.digest(h.private_read(row["path"])), row["sha256"])
                    self.assertEqual(h.read_regular(row["source"]), h.private_read(row["path"]))
                self.assertEqual(run.state["preservation_paths"], sorted(row["path"] for row in manifest["dependencies"]))
                self.assertNotIn(str(child), h.canonical(manifest).decode())
                self.assertFalse(any(p.name == child.name for p in Path(ref["path"]).parent.rglob("*")))
                if delta:
                    self.assertEqual(manifest["parent_sha256"], run.state["manifest"]["sha256"])
            unknown = h.mkdir(fake.home / "unknown")
            sibling = h.mkdir(fake.repo.parent / "repository-sibling")
            other_run = h.mkdir(run.root.parent / str(uuid.uuid4()))
            outside_child = h.mkdir(fake.repo / ".git/unknown-child")
            alias = fake.shared / "alias"
            alias.symlink_to(fake.repo / ".git", target_is_directory=True)
            for path in (unknown, sibling, other_run, outside_child, fake.home, fake.home.parent, alias):
                with self.subTest(directory=str(path)):
                    prepare("`" + str(path) + "`")
                    with self.assertRaises(h.Stop):
                        h.export_material(run)
            for path in (child, fake.data_dir / "outside.txt", fake.data_dir / "agent-accounts/fixture.txt"):
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b"synthetic outside material")
                prepare("[necessary file](" + str(path) + ")")
                with self.subTest(file=str(path)), self.assertRaises(h.Stop):
                    h.export_material(run)
            # A metadata name is never a file grant, even after a type swap.
            swapped = h.mkdir(self.root / "metadata-location")
            metadata = {str(swapped)}
            swapped.rmdir()
            swapped.write_bytes(b"not directory metadata")
            with self.assertRaisesRegex(h.Stop, "dependency outside allowed roots"):
                h.preserve_dependencies([swapped], self.root / "rejected", [fake.shared], [], metadata)
            prepare("`" + str(fake.repo / ".git") + "`")
            del run.state["workspace_bindings"]["worker"]
            with self.assertRaisesRegex(h.Stop, "export workspace binding missing"):
                h.export_material(run)

    def test_T03_dependency_closure_hashes_and_relative_relationships(self):
        source = self.root / "source"
        source.mkdir()
        (source / "handoff.md").write_text("[dep](dep.md)\n![image](x.png)")
        (source / "dep.md").write_text("needed")
        (source / "x.png").write_bytes(b"image")
        result = h.preserve_dependencies([source / "handoff.md"], self.root / "copy", [source])
        self.assertEqual(len(result), 3)
        directories = []
        (source / "locations.md").write_text("原工作目錄 `" + str(source) + "`，不遞迴複製。")
        h.preserve_dependencies([source / "locations.md"], self.root / "directory-copy", [source], directories)
        self.assertEqual(directories[0]["source"], str(source))
        for ref in result:
            self.assertEqual(h.digest(h.private_read(ref["path"])), ref["sha256"])
        (source / "dep.md").write_text("[escape](../outside.md)")
        (self.root / "outside.md").write_text("existing private source outside allowed root")
        with self.assertRaises(h.Stop):
            h.preserve_dependencies([source / "handoff.md"], self.root / "bad-copy", [source])

    def test_T03_file_URI_and_captured_file_reference_preserve_originals(self):
        self.assertEqual(h.attachment_paths({"type": "resource", "uri": "file:///allowed/image%20one.png"}), ["/allowed/image one.png"])
        self.assertEqual(h.attachment_paths({"type": "file_reference", "fileReference": {"path": "mutable.txt", "lines": [{"text": "captured original"}]}}), [])
        with self.assertRaises(h.Stop):
            h.attachment_paths({"type": "resource", "uri": "file://other-host/private"})

    def test_T05_bootstrap_rejects_opaque_tool_and_outside_write(self):
        command = h.producer_command(self.root, "bootstrap")
        activity = dict(kind="activity", activityKind="command", status="completed",
                        detail={"command": command, "exitCode": 0})
        h.audit_bootstrap(snapshot(entries=[activity]), self.root)
        activity["detail"]["rawCommand"] = "/bin/bash -lc " + h.shlex.quote(command)
        activity["detail"]["outputMayBePartial"] = True
        h.audit_bootstrap(snapshot(entries=[activity]), self.root)
        del activity["detail"]["rawCommand"]
        acp_read = dict(kind="activity", activityKind="mcp_tool", status="completed",
                        detail={"protocol": "acp", "toolKind": "read", "input": {"file_path": "source.md"}})
        h.audit_bootstrap(snapshot(entries=[acp_read, activity]), self.root)
        read = copy.deepcopy(activity)
        read["detail"]["command"] = command.replace(" bootstrap --run", " inspect --run") + " --path start.json --offset 0"
        h.audit_bootstrap(snapshot(entries=[read, activity]), self.root)
        for bad in ("echo secret > /tmp/out", "python3 -c 'arbitrary()'", "cat /tmp/history"):
            activity["detail"]["command"] = bad
            with self.subTest(command=bad), self.assertRaises(h.Stop):
                h.audit_bootstrap(snapshot(entries=[activity]), self.root)
        with self.assertRaises(h.Stop):
            h.audit_bootstrap(snapshot(entries=[dict(kind="activity", activityKind="mcp_tool", status="completed",
                                                    detail={"toolName": "exec", "arguments": {"code": "opaque"}})]), self.root)

    def test_T06_report_interrupt_requires_structural_source_and_allows_once(self):
        interrupted = {"id": "original", "state": "interrupted", "startedAt": "2026-11-08T01:00:00Z",
                       "completedAt": "2026-11-08T01:00:01Z"}
        report = dict(kind="message", role="user", origin="automation", clientMessageId="report-batch:123",
                      text="Reports [worker](ao://sessions/agent-orchestrator/worker)", createdAt="2026-11-08T01:00:02Z")
        snap = snapshot(turns=[interrupted], entries=[report])
        self.assertTrue(h.report_interruption(snap, "original"))
        run = self.run_fixture()
        h.continue_phase(run, "old", "2a", "original", snap)
        h.continue_phase(run, "old", "2a", "original", snap)
        self.assertEqual(len(run.cli.calls), 1)
        snap["transcript"]["turns"].append({"id": "continued", "state": "completed", "completedAt": h.now()})
        snap["transcript"]["entries"].append({"kind": "message", "origin": "automation", "turnId": "continued",
            "clientMessageId": run.run_id + ":2a-continue:old"})
        self.assertEqual(h.phase_completed(run, "old", "2a", "original", snap), "continued")
        snap["transcript"]["turns"][-1].update(state="interrupted", startedAt="2026-11-08T01:00:00Z", completedAt="2026-11-08T01:00:01Z")
        with self.assertRaises(h.Stop):
            h.continue_phase(run, "old", "2a", "original", snap)
        report["origin"] = "human"
        with self.assertRaises(h.Stop):
            h.report_interruption(snap, "original")

    def test_bootstrap_and_its_continuation_require_single_fixed_calls(self):
        run = self.run_fixture()
        run.state.update(handoff={"path": "handoff", "sha256": "0" * 64}, manifest={"path": "manifest", "sha256": "1" * 64})
        requirements = h.decode(h.bootstrap_prompt(run))["requirements"]
        for phrase in ("逐字等於read_command或helper_command", "換行", "2>&1", "不呼叫任何工具", "bootstrap-continue", "exit非0"):
            self.assertIn(phrase, requirements)
        interrupted = {"id": "original", "state": "interrupted", "startedAt": "2026-11-08T01:00:00Z", "completedAt": "2026-11-08T01:00:01Z"}
        report = dict(kind="message", role="user", origin="automation", clientMessageId="report-batch:123",
                      text="Reports [worker](ao://sessions/agent-orchestrator/worker)", createdAt="2026-11-08T01:00:02Z")
        h.continue_phase(run, "new", "bootstrap", "original", snapshot(turns=[interrupted], entries=[report]))
        message = run.cli.calls[0][-1]
        sent = h.decode(message)["instruction"]["requirements"]
        self.assertIn(h.BOOTSTRAP_CALLS, sent)
        self.assertIn("不另行計算hash", sent)
        self.assertNotIn("managed hash", sent)
        self.assertLessEqual(len(message.encode()), 4096)

    def test_2a_requirements_keep_tracked_and_gitlink_out_of_classification(self):
        run = self.run_fixture()
        run.state["stop_index_sha256"], run.cli.documents = "0" * 64, dict
        with patch.object(run, "wait"):
            h.phase_2a(run)
        sent = h.decode(run.cli.calls[-1][-1])["instruction"]["requirements"]
        for phrase in ("tracked檔與gitlink", "列入則helper停止"):
            self.assertIn(phrase, sent)

    def test_T04_T05_four_harness_pairs_complete_fake_protocol(self):
        for old_harness in ("claude-code", "codex"):
            for new_harness in ("claude-code", "codex"):
                with self.subTest(old=old_harness, new=new_harness):
                    with tempfile.TemporaryDirectory() as temp:
                        fake = FlowFixture(Path(temp), old_harness, new_harness)
                        h.run_protocol(fake.run)
                        self.assertEqual(fake.run.state["outcome"], "complete")
                        self.assertEqual(fake.mutations.count("spawn"), 1)
                        self.assertEqual(fake.mutations.count("kill"), 1)
                        self.assertEqual(fake.mutations.count("exit-agent"), 1)
                        self.assertEqual(fake.mutations.count("resume-agent"), 1)
                        self.assertIn("nonce_completed", fake.run.state)
                        self.assertEqual(fake.run.state["old_settings"]["model"]["value"], "UNKNOWN")
                        self.assertEqual(fake.run.state["original_t0"]["t0_precision"], "UNKNOWN")
                        self.assertEqual(fake.run.state["bootstrap_origin_exception"]["origin"], "human")
                        self.assertFalse(fake.run.state["bootstrap_origin_exception"]["grants_authority"])
                        before = list(fake.mutations)
                        h.run_protocol(fake.run)
                        self.assertEqual(before, fake.mutations)

    def test_review_resume_receipt_reentry_verifies_without_repeating_effects(self):
        fake = FlowFixture(self.root, "codex", "claude-code")
        verify = h.verify_target
        def read_failure_after_resume(*args):
            if (fake.run.root / "control/resume/result.json").exists():
                raise h.Stop("transient post-resume read failure")
            return verify(*args)
        with patch.object(h, "verify_target", side_effect=read_failure_after_resume):
            with self.assertRaisesRegex(h.Stop, "transient post-resume read failure"):
                h.run_protocol(fake.run)
        receipt = h.decode(h.private_read(fake.run.root / "control/resume/result.json"))
        response = h.decode(receipt["stdout"])
        self.assertEqual(response["sessionId"], "new")
        self.assertEqual(response["resumeMode"], "native")
        self.assertEqual(response["session"]["id"], "new")
        self.assertNotIn("resume", fake.run.state["completed_stages"])
        self.assertEqual(fake.snapshots["new"]["session"]["activity"]["state"], "idle")
        # Recreate the runner from disk as an explicit same-run continue would.
        fake.run = h.Run(fake.run.root, fake)
        fake.run.sleep = fake.dispatch_actor
        fake.run.rearm_wait()
        with patch.object(h, "verify_target", return_value={"native_id": "different-native"}):
            with self.assertRaisesRegex(h.Stop, "resume binding/native/fast drift"):
                h.run_protocol(fake.run)
        h.run_protocol(fake.run)
        self.assertEqual(fake.run.state["outcome"], "complete")
        self.assertEqual(fake.mutations.count("exit-agent"), 1)
        self.assertEqual(fake.mutations.count("resume-agent"), 1)
        self.assertEqual(fake.mutations.count("nonce"), 1)

    def test_Q3_clean_ignored_copy_survives_normal_reclaim(self):
        fake = FlowFixture(self.root, "codex", "claude-code")
        h.run_protocol(fake.run)
        proof = h.decode(h.private_read(fake.run.root / "old/preservation-evidence.json"))
        self.assertEqual(proof["classification"]["ignored.cache"]["disposition"], "copy")
        self.assertFalse(fake.workspace.exists())
        self.assertEqual(fake.run.state["old_preservation_result"]["workspace"], "clean-reclaim-allowed")
        self.assertEqual(h.private_read(Path(proof["classification"]["ignored.cache"]["copy"]["path"])), b"unique original")

    def test_launch_survives_clean_old_reclaim_for_normal_and_continuation(self):
        for continuation in (False, True):
            with self.subTest(continuation=continuation), tempfile.TemporaryDirectory() as temp:
                base = Path(temp)
                data = h.mkdir(base / "h/.ao/data")
                old = data / "worktrees" / h.PROJECT / "orchestrator/old"
                FlowFixture.prepare_git(data / "repository", old)
                root = h.mkdir(data / "projects" / h.PROJECT / "orchestrator/.private-handoff-runs" / str(uuid.uuid4()))
                for directory in (data, old.parent, root.parent):
                    for parent in [directory, *directory.parents]:
                        if parent == base:
                            break
                        parent.chmod(0o700)
                config = dict(base=str(base), root=str(root), tests=str(Path(__file__).parent),
                              env={k: os.environ[k] for k in ("RH_WIRE_FIXTURES", "RH_ACTIVITY_PRODUCER", "TMPDIR", "TMUX_TMPDIR") if k in os.environ})
                child = '''import os,sys,json,traceback
from pathlib import Path
config=CONFIG
os.umask(0o022)
os.environ.update({k:v for k,v in config['env'].items() if k.startswith('RH_') or sys.argv[1]=='_run'})
sys.path.insert(0,config['tests'])
from test_handoff import h,FlowFixture
h.__file__=__file__
if sys.argv[1]!='_run': raise SystemExit(h.main())
result={'cwd':str(Path.cwd())};fake=None
try:
 fake=FlowFixture(Path(config['base']),'codex','claude-code',run_id=Path(config['root']).name,git_prepared=True)
 h.run_protocol(fake.run)
 proof=h.decode(h.private_read(fake.run.root/'old/preservation-evidence.json'))
 head=h.old_git(['rev-parse','refs/heads/original'],common=fake.repo/'.git').decode().strip()
 h.old_git(['cat-file','-e',head+'^{commit}'],common=fake.repo/'.git')
 result.update(outcome=fake.run.state['outcome'],old_exists=fake.workspace.exists(),
  mutations=fake.mutations,protected_head=head,
  copy_preserved=h.private_read(Path(proof['classification']['ignored.cache']['copy']['path']))==b'unique original')
except Exception:
 result['error']=traceback.format_exc()
finally:
 if fake is not None: fake.transport.close()
 h.publish(Path(config['root'],'test-result.json'),h.canonical(result))
'''.replace("CONFIG", repr(config))
                h.publish(root / "handoff.py", child.encode())
                parent = ("import importlib.util\nfrom pathlib import Path\n"
                    "spec=importlib.util.spec_from_file_location('h'," + repr(h.__file__) + ")\n"
                    "h=importlib.util.module_from_spec(spec);spec.loader.exec_module(h)\n"
                    "h.launch(Path(" + repr(str(root)) + "),continuation=" + repr(continuation) + ")\n")
                invoked = subprocess.run([h.sys.executable, "-I", "-B", "-c", parent], cwd=old, capture_output=True, timeout=10)
                self.assertEqual(invoked.returncode, 0, invoked.stderr.decode())
                runner_file = next(root.glob("runner-continuations/*/runner.json")) if continuation else root / "runner.json"
                runner = h.decode(h.private_read(runner_file))
                marker = root / "test-result.json"
                try:
                    deadline = time.monotonic() + 90
                    while not marker.exists() and time.monotonic() < deadline:
                        time.sleep(0.05)
                    self.assertTrue(marker.exists(), (root / "runner.log").read_text() if not continuation else "continuation runner did not finish")
                    result = json.loads(marker.read_text())
                    self.assertNotIn("error", result, result.get("error"))
                    self.assertEqual(result["cwd"], str(root))
                    self.assertEqual(result["outcome"], "complete")
                    self.assertFalse(result["old_exists"])
                    self.assertTrue(result["copy_preserved"])
                    self.assertTrue(result["protected_head"])
                    self.assertEqual(result["mutations"].count("kill"), 1)
                    self.assertEqual(result["mutations"].count("spawn"), 1)
                    self.assertLess(result["mutations"].index("kill"), result["mutations"].index("spawn"))
                finally:
                    if not marker.exists() and not h.process_ended(runner["process"]):
                        os.killpg(runner["pid"], signal.SIGTERM)

    def test_Q3_dirty_old_retains_original_path_and_contents(self):
        fake = FlowFixture(self.root, "codex", "codex")
        (fake.workspace / "tracked").write_text("dirty original")
        h.run_protocol(fake.run)
        self.assertEqual((fake.workspace / "tracked").read_text(), "dirty original")
        self.assertEqual(fake.run.state["old_preservation_result"]["workspace"], "dirty-original-verified")

    def test_Q3_unclear_classification_ref_or_hook_stops_before_kill(self):
        for problem in ("classification", "ref", "hook", "drift", "alternate", "metadata", "nested", "empty-directory"):
            with self.subTest(problem=problem), tempfile.TemporaryDirectory() as temp:
                fake = FlowFixture(Path(temp), "codex", "codex")
                fake.preservation_problem = problem
                with self.assertRaises(h.Stop):
                    h.run_protocol(fake.run)
                self.assertNotIn("kill", fake.mutations)
                self.assertNotIn("spawn", fake.mutations)

    def test_Q3_pending_or_lost_kill_receipt_never_spawns_even_on_continue(self):
        for outcome in ("pending", "lost", "dirty-deleted", "ref-drift", "copy-drift"):
            with self.subTest(outcome=outcome), tempfile.TemporaryDirectory() as temp:
                fake = FlowFixture(Path(temp), "codex", "codex")
                fake.kill_outcome = outcome
                if outcome == "dirty-deleted":
                    (fake.workspace / "tracked").write_text("dirty original")
                with self.assertRaises((h.Stop, subprocess.TimeoutExpired)):
                    h.run_protocol(fake.run)
                with self.assertRaises(h.Stop):
                    h.run_protocol(fake.run)
                self.assertEqual(fake.mutations.count("kill"), 1)
                self.assertNotIn("spawn", fake.mutations)

    def test_T01_active_worker_needs_reserved_D_and_real_completed_receipt(self):
        run = self.run_fixture()
        worker = dict(id="worker", projectId=h.PROJECT, kind="worker", isTerminated=False)
        active = snapshot("active", turns=[{"id": "busy", "state": "running"}])
        active["session"].update(id="worker", kind="worker")
        run.cli.session = lambda _: active
        self.worker_index(run)
        self.assertFalse(run.worker_stop(worker))
        self.assertEqual(run.cli.calls, [])
        run.request["workers"] = [{"worker_id": "worker", "reserved_stop_d": "D-7"}]
        self.assertFalse(run.worker_stop(worker))
        self.assertEqual(len(run.cli.calls), 1)
        self.assertIn("D-7", run.cli.calls[0][-1])

    def test_T01_incomplete_worker_wait_is_bounded_without_resending_D(self):
        run = self.run_fixture()
        active = snapshot("active", turns=[{"id": "busy", "state": "running"}])
        active["session"].update(id="worker", kind="worker")
        run.cli.session = lambda _: active
        self.worker_index(run)
        run.request["workers"] = [{"worker_id": "worker", "reserved_stop_d": "D-7"}]
        elapsed = [0]
        run.clock = lambda: elapsed[0]
        run.sleep = lambda _: elapsed.__setitem__(0, elapsed[0] + 600)
        with self.assertRaisesRegex(h.Stop, "wait deadline reached: workers"):
            run.wait("workers", lambda: run.worker_stop({"id": "worker"}))
        self.assertEqual(len(run.cli.calls), 1)
        self.assertEqual(run.state["waits"]["workers"]["monotonic"], 0)

    def test_T05_running_2a_and_bootstrap_receipts_wait_for_completed(self):
        for sid, phase in (("old", "old_2a"), ("new", "bootstrap")):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory() as temp:
                fake = FlowFixture(Path(temp), "codex", "claude-code")
                elapsed = [0]
                fake.run.clock = lambda: elapsed[0]
                def advance(seconds):
                    fake.dispatch_actor(seconds)
                    elapsed[0] += 2
                fake.run.sleep = advance
                fake.running_receipt_reads = {sid: 2}
                observations, original_session = [], fake.session
                def record(session_id):
                    value = original_session(session_id)
                    if session_id == sid and fake.run.state.get("stage") == ("2a" if sid == "old" else "bootstrap"):
                        observations.append(copy.deepcopy(value))
                    return value
                fake.session = record
                h.run_protocol(fake.run)
                identity = ("old-receipt" if sid == "old" else "bootstrap") + "-tool"
                tools = [next(e for e in value["transcript"]["entries"] if e.get("id") == identity) for value in observations]
                self.assertEqual([e["status"] for e in tools], ["running", "running", "completed"])
                self.assertEqual(len({e["sequence"] for e in tools}), 1)
                self.assertEqual(len({e["detail"]["rawCommand"] for e in tools}), 1)
                self.assertEqual(len({e["detail"]["output"] for e in tools}), 1)
                self.assertEqual(len({v["transcript"]["latestSequence"] for v in observations}), 1)
                self.assertEqual(fake.run.state["outcome"], "complete")
                self.assertEqual(fake.run.state["waits"][phase]["elapsed"], 6)
                self.assertEqual(fake.mutations.count("2a"), 1)
                self.assertEqual(fake.mutations.count("spawn"), 1)

    def test_review_hidden_index_flags_stop_before_clean_reclaim(self):
        for flag in ("--assume-unchanged", "--skip-worktree"):
            with self.subTest(flag=flag), tempfile.TemporaryDirectory() as temp:
                fake = FlowFixture(Path(temp), "codex", "codex")
                subprocess.run(["git", "-C", str(fake.workspace), "update-index", flag, "tracked"], check=True, capture_output=True)
                (fake.workspace / "tracked").write_text("hidden unique modification")
                status = subprocess.run(["git", "-C", str(fake.workspace), "status", "--porcelain"], check=True, capture_output=True).stdout
                self.assertEqual(status, b"")
                with self.assertRaisesRegex(h.Stop, "index flags"):
                    h.run_protocol(fake.run)
                self.assertNotIn("kill", fake.mutations)
                self.assertEqual((fake.workspace / "tracked").read_text(), "hidden unique modification")

    def test_review_inspect_shell_expansions_are_not_literal_argv(self):
        command = h.producer_command(self.root, "bootstrap")
        good = dict(kind="activity", activityKind="command", status="completed", detail={"command": command, "exitCode": 0})
        prefix = command.replace(" bootstrap --run", " inspect --run")
        for token in ('$(touch${IFS}/tmp/rh-out;printf${IFS}start.json)', '$HOME', '`touch${IFS}/tmp/rh-out`',
                      'start\\.json', '"start.json"', "'start.json'", '*.json', 'start?.json', '[s]tart.json',
                      '{start,other}.json', 'start.json;id', 'start.json|id', 'start.json>out', '~/start.json'):
            malicious = copy.deepcopy(good)
            malicious["detail"]["command"] = prefix + " --path " + token + " --offset 0"
            with self.subTest(token=token), self.assertRaises(h.Stop):
                h.audit_bootstrap(snapshot(entries=[malicious, good]), self.root)
        malicious = copy.deepcopy(good)
        malicious["detail"]["rawCommand"] = "$(touch${IFS}/tmp/rh-out)/bash -lc " + h.shlex.quote(command)
        with self.assertRaises(h.Stop):
            h.audit_bootstrap(snapshot(entries=[malicious, good]), self.root)

    def test_review_live_authority_identity_cannot_be_forged_by_evidence(self):
        req = self.request()
        original = h.decode(h.private_read(self.root / "evidence/human.json"))
        original["transcript"]["turns"].append({"id": "preparing", "state": "running"})
        cli = type("SourceCLI", (), {})()
        cli.session = lambda _: copy.deepcopy(original)
        h.validate_sources_live(req, cli)
        for changes in ({"role": "assistant"}, {"origin": "automation"}, {"senderSessionId": "other-worker"}):
            with self.subTest(changes=changes):
                changed = copy.deepcopy(original)
                changed["transcript"]["entries"][0].update(changes)
                cli.session = lambda _: copy.deepcopy(changed)
                with self.assertRaises(h.Stop):
                    h.validate_sources_live(req, cli)

    def test_T01_quiescent_worker_has_preserved_content_and_is_not_self_ACK(self):
        run = self.run_fixture()
        run.request["workers"].append({"worker_id": "worker", "reserved_stop_d": None, "pending_question_refs": []})
        workspace = self.worker_index(run)
        workspace.mkdir(parents=True)
        subprocess.run(["git", "-C", str(workspace), "init", "-b", "fixture"], check=True, capture_output=True)
        (workspace / "file").write_text("base")
        subprocess.run(["git", "-C", str(workspace), "add", "file"], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(workspace), "-c", "user.email=x@example.invalid", "-c", "user.name=x", "commit", "-m", "fixture"], check=True, capture_output=True)
        (workspace / "file").write_text("dirty preserved")
        idle = snapshot()
        idle["session"].update(id="worker", kind="worker")
        run.cli.session = lambda _: copy.deepcopy(idle)
        run.cli.get = lambda path: {"project": dict(id=h.PROJECT, path=str(workspace))} if path.startswith("projects/") else {"session": dict(id="worker", projectId=h.PROJECT, kind="worker", branch="fixture")}
        run.cli.data_dir = self.root
        shared = h.mkdir(self.root / "projects" / h.PROJECT / "orchestrator/shared")
        run.cli.documents = lambda: {"sharedDir": str(shared)}
        run.state["location"] = h.location_info(run.cli)
        run.sleep = lambda _: None
        stop = run.worker_stop({"id": "worker"})
        self.assertEqual(stop["source_type"], "script-quiescent")
        self.assertIsNone(stop["evidence_turn_id"])
        self.assertEqual(stop["qualification"], "未經worker自述")
        self.assertEqual(run.cli.calls, [])
        self.assertEqual((workspace / "file").read_text(), "dirty preserved")
        self.assertEqual(run.worker_stop({"id": "worker"})["state"], "safe")
        (workspace / "file").write_text("changed again")
        with self.assertRaises(h.Stop):
            run.worker_stop({"id": "worker"})

    def test_T04_start_binds_original_bytes_and_does_not_detach_twice(self):
        req = self.request()
        root = h.mkdir(self.root / "runs")
        slot = h.mkdir(root / ".start/old")
        h.publish(slot / "evidence/human.json", h.private_read(self.root / "evidence/human.json"))
        raw = h.canonical(req)
        h.publish(slot / "request.json", raw)
        h.publish(slot / "capabilities.json", h.canonical(dict(run_id=req["request_id"], start_sha256=h.digest(raw),
                  scope="daemon-account-catalog-global", source_refs=["human"],
                  original_authority_covers_global_refresh=True)))
        repository = self.root / "repository"
        repository.mkdir()
        subprocess.run(["git", "init", "-b", "main", str(repository)], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(repository), "-c", "user.email=x@example.invalid", "-c", "user.name=x", "commit", "--allow-empty", "-m", "fixture"], check=True, capture_output=True)
        workspace = Path(req["workspace_index"][0]["path"])
        workspace.parent.mkdir(parents=True)
        subprocess.run(["git", "-C", str(repository), "worktree", "add", "-b", "original", str(workspace)], check=True, capture_output=True)
        shared = h.mkdir(self.root / "projects" / h.PROJECT / "orchestrator/shared")
        class PreparedCLI:
            executable = Path(h.__file__)
            sha256 = h.digest(executable.read_bytes())
            data_dir = self.root
            run_file = self.root / "run.json"
            def get(self, path):
                return {"status": "ok", "project": {"id": h.PROJECT, "path": str(repository)}} if path.startswith("projects/") else {"session": snapshot()["session"]}
            def documents(self):
                return {"sharedDir": str(shared)}
            def sessions(self):
                return []
        cli = PreparedCLI()
        cli.execution = h.Journal(self.root, req["request_id"]).register("start", "old")
        self.addCleanup(cli.execution.close)
        h.publish(root / ".qualified/qualification.json", h.canonical(dict(schema="rh-cli-qualification/2", call_protocol=h.CALL_PROTOCOL)))
        with patch.object(h, "qualified", return_value=cli), patch.object(h, "validate_sources_live", return_value=snapshot()), \
             patch.object(h, "target_preflight", return_value={}), patch.object(h, "native_identity", return_value="old-native"), \
             patch.object(h, "launch") as launch:
            first = h.start(root, cli, "old", target(), req["request_id"])
            second = h.start(root, cli, "old", target(), req["request_id"])
            self.assertEqual(first["run_id"], second["run_id"])
            self.assertEqual(launch.call_count, 1)
            self.assertEqual(h.private_read(root / req["request_id"] / "start.json"), raw)
            saved_script = root / req["request_id"] / "handoff.py"
            self.assertEqual(h.private_read(saved_script), Path(h.__file__).read_bytes())
            self.assertIn(str(saved_script), h.status_command(root / req["request_id"], req["request_id"]))
            # A byte change cannot steal the already-bound UUID.
            (slot / "request.json").write_bytes(raw + b"\n")
            with self.assertRaises(h.Stop):
                h.start(root, cli, "old", target(), req["request_id"])

    def test_T05_initial_bootstrap_human_origin_is_not_new_authority(self):
        run = self.run_fixture()
        run.state.update(handoff={"path": "handoff", "sha256": "0" * 64}, manifest={"path": "manifest", "sha256": "1" * 64})
        run.request["authority"]["source_refs"] = ["AUTHORITY_ONLY_IN_BOUND_START"]
        prompt = h.decode(h.bootstrap_prompt(run))
        self.assertEqual(prompt["authorization_source"], str(run.root / "start.json"))
        self.assertNotIn("authority_refs", prompt)
        self.assertNotIn("target", prompt)
        self.assertNotIn("AUTHORITY_ONLY_IN_BOUND_START", h.canonical(prompt).decode())
        self.assertIn("不得", prompt["origin_policy"])
        run.send("new", "nonce", {"challenge": "one"})
        control = h.decode(run.cli.calls[0][-1])
        self.assertEqual(control["actor"], "detached-runner")
        self.assertEqual(control["old_id"], "old")
        self.assertEqual(control["authority_refs"], ["AUTHORITY_ONLY_IN_BOUND_START"])

    def test_T05_detached_runner_finishes_after_parent_process_exits(self):
        directory = h.mkdir(self.root / str(uuid.uuid4()))
        marker = directory / "after-parent.json"
        child = self.root / "fixture-child.py"
        h.publish(child, ("import os,time,json\nfrom pathlib import Path\n"
            "os.umask(0o077)\n"
            "parent=int(Path(" + repr(str(directory / "parent")) + ").read_text())\n"
            "deadline=time.monotonic()+10\n"
            "while time.monotonic()<deadline:\n"
            " try: os.kill(parent,0)\n"
            " except ProcessLookupError: break\n"
            " time.sleep(0.01)\n"
            "else: raise RuntimeError('parent never exited')\n"
            "Path(" + repr(str(marker)) + ").write_text(json.dumps({'pid':os.getpid(),'sid':os.getsid(0)}))\n").encode())
        h.publish(directory / "handoff.py", h.private_read(child))
        parent = ("import importlib.util,os\nfrom pathlib import Path\n"
            "spec=importlib.util.spec_from_file_location('handoff'," + repr(str(Path(h.__file__))) + ")\n"
            "h=importlib.util.module_from_spec(spec); spec.loader.exec_module(h)\n"
            "h.__file__=" + repr(str(child)) + "\n"
            "h.publish(Path(" + repr(str(directory / "parent")) + "),str(os.getpid()).encode())\n"
            "Path(" + repr(str(child)) + ").unlink()\n"
            "h.launch(Path(" + repr(str(directory)) + "))\n"
            "os._exit(0)\n")
        result = subprocess.run([h.sys.executable, "-I", "-B", "-c", parent], capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        deadline = time.monotonic() + 10
        while not marker.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(marker.exists(), "detached child did not finish after parent exited")
        receipt = json.loads(marker.read_text())
        self.assertEqual(receipt["pid"], receipt["sid"])


    def test_T04_target_rejects_missing_or_non_full_access(self):
        for harness in ("codex", "claude-code"):
            for key in target():
                bad = target(harness)
                bad[key] = ""
                with self.subTest(harness=harness, key=key), self.assertRaises(h.Stop):
                    h.validate_target(bad)
            bad = target(harness)
            bad["approval_mode"] = "auto"
            with self.assertRaises(h.Stop):
                h.validate_target(bad)

    def test_T01_quota_is_not_safe_and_pending_tools_block(self):
        self.assertFalse(h.quiescent(snapshot("blocked")))
        self.assertFalse(h.quiescent(snapshot(turns=[{"id": "queued", "state": "queued"}])))
        self.assertFalse(h.quiescent(snapshot(entries=[{"kind": "activity", "status": "completed",
                                                       "activityKind": "command", "detail": {"output": "session_id: 123"}}])))
        self.assertTrue(h.quiescent(snapshot()))
        async_snapshot = snapshot(entries=[{"id": "tool", "kind": "activity", "status": "completed", "activityKind": "command",
                                            "detail": {"exitCode": 0, "output": "Script running with cell ID job42"}}])
        self.assertFalse(h.quiescent(async_snapshot))
        source = self.root / "terminal.json"
        h.publish(source, h.canonical(dict(reference="job42", activity_id="tool", state="completed")))
        receipt = dict(reference="job42", activity_id="tool", state="completed", evidence_path=str(source), sha256=h.digest(source.read_bytes()))
        self.assertTrue(h.quiescent(async_snapshot, [receipt]))
        self.assertFalse(h.quiescent(async_snapshot, [dict(receipt, reference="other")]))
        with self.assertRaises(h.Stop):
            h.quiescent(async_snapshot, [dict(receipt, state="running")])

    def test_T04_duplicate_keys_bad_hash_and_symlink_are_rejected(self):
        with self.assertRaises(h.Stop):
            h.decode(b'{"x":1,"x":2}')
        req = self.request()
        req["sources"][0]["raw_sha256"] = "0" * 64
        with self.assertRaises(h.Stop):
            h.validate_request(req, self.root, "old", target())
        (self.root / "link").symlink_to(self.root / "evidence")
        with self.assertRaises(h.Stop):
            h.private_read(self.root / "link" / "human.json")

    def test_T07_no_invented_D_or_release_authority(self):
        req = self.request()
        worker = {"worker_id": "worker", "reserved_stop_d": None, "stop_d_source_refs": [],
                  "hold_state": "held", "hold_source_refs": [], "task_owner_id": None,
                  "task_refs": [], "pending_question_refs": [],
                  "release": {"mode": "resume-original", "authority_refs": [],
                              "reserved_resume_d": "D-2", "resume_d_source_refs": []}}
        req["workers"] = [worker]
        req["workspace_index"].append({"session_id": "worker", "kind": "worker", "path": str(self.root / "worktrees" / h.PROJECT / "worker"), "source_refs": ["human"]})
        with self.assertRaises(h.Stop):
            h.validate_request(req, self.root, "old", target())
        worker["release"]["mode"] = "keep-held"
        h.validate_request(req, self.root, "old", target())



class FlowFixture:
    """A persisted fake daemon: actual exporter/receipt checks, zero live I/O."""
    def __init__(self, root, old_harness, new_harness, worker=False, *, run_id=None, git_prepared=False):
        self.home = root / "h"
        root = h.mkdir(self.home / ".ao/data")
        self.data_dir = root
        self.old_harness, self.target = old_harness, target(new_harness)
        root.chmod(0o700)
        self.shared = h.mkdir(root / "projects" / h.PROJECT / "orchestrator/shared")
        self.workspace = root / "worktrees" / h.PROJECT / "orchestrator/old"
        h.mkdir(self.workspace.parent)
        self.repo = root / "repository"
        if not git_prepared:
            self.prepare_git(self.repo, self.workspace)
        def git(*args):
            return subprocess.run(["git", "-C", str(self.repo), *args], check=True, capture_output=True).stdout
        (self.workspace / "ignored.cache").write_bytes(b"unique original")
        self.preservation_problem, self.kill_outcome = None, None
        self.run_file = root / "run.json"
        self.mutations = []
        self.last_received = h.time.monotonic()
        self.snapshots = {"old": snapshot()}
        self.snapshots["old"]["session"]["harness"] = old_harness
        self.snapshots["old"]["session"]["branch"] = "original"
        self.snapshots["old"]["transcript"]["latestSequence"] = 10
        self.snapshots["old"]["transcript"]["turns"][0]["requestedAt"] = "2026-11-08T00:00:00Z"
        self.session_list = [self.snapshots["old"]["session"]]
        workspace_index = [{"session_id": "old", "kind": "orchestrator", "path": str(self.workspace), "source_refs": ["workspace"]}]
        workers = []
        if worker:
            self.worker_path = root / "worktrees" / h.PROJECT / "worker"
            git("worktree", "add", "-b", "fixture", str(self.worker_path))
            self.snapshots["worker"] = snapshot()
            self.snapshots["worker"]["session"].update(id="worker", kind="worker")
            self.session_list.append(self.snapshots["worker"]["session"])
            workspace_index.append({"session_id": "worker", "kind": "worker", "path": str(self.worker_path), "source_refs": ["workspace"]})
            workers = [{"worker_id": "worker", "reserved_stop_d": None, "pending_question_refs": []}]
        self.owner = "old"
        self.run_id = run_id or str(uuid.uuid4())
        directory = h.mkdir(root / "projects" / h.PROJECT / "orchestrator/.private-handoff-runs" / self.run_id)
        h.publish(directory / "handoff.py", Path(h.__file__).read_bytes())
        req = {"request_id": self.run_id, "old_id": "old", "target": self.target,
               "authority": {"source_refs": ["human"], "t0_precision": "UNKNOWN", "t0_created_at": None}, "workers": workers,
               "prepared_by": {"session_id": "old", "turn_id": "preparing"}, "workspace_index": workspace_index}
        mapping = dict(id="workspace", role="assistant", turnId="preparing", revision=1, text=json.dumps(workspace_index))
        source_raw = h.canonical(snapshot(entries=[mapping]))
        h.publish(directory / "evidence/workspace.json", source_raw)
        req["sources"] = [{"ref": "workspace", "kind": "old-control-record", "conversation_id": "conv", "branch_id": "branch",
                           "message_id": "workspace", "revision": 1, "raw_relpath": "evidence/workspace.json",
                           "raw_sha256": h.digest(source_raw), "message_text_sha256": h.digest(mapping["text"].encode())}]
        h.publish(directory / "start.json", h.canonical(req))
        h.publish(directory / "state.json", h.canonical({"start_sha256": h.digest(h.canonical(req)), "effects": {},
                  "original_t0": req["authority"], "old_settings": h.old_settings(self.snapshots["old"]), "old_native": "native-old"}))
        self.run = h.Run(directory, self)
        self.run.state["location"] = h.location_info(self)
        self.pending_actor = None
        self.helper_events = []
        self.run.sleep = self.dispatch_actor
        self.transport = ActorTransport(self)
        self.executable = self.transport.cli
        self.sha256 = h.digest(self.executable.read_bytes())
        self.run_file = self.transport.run_file
        qualification = dict(schema="rh-cli-qualification/2", call_protocol=h.CALL_PROTOCOL,
                             cli_sha256=self.sha256, script_sha256=h.digest(Path(h.__file__).read_bytes()))
        h.publish(directory.parent / ".qualified/qualification.json", h.canonical(qualification))
        journal = h.Journal(self.data_dir, self.run_id)
        self.run.state.update(call_protocol=h.CALL_PROTOCOL, journal=str(journal.root),
                              qualification_sha256=h.digest(h.canonical(qualification)),
                              schema="rh-state/1", run_id=self.run_id, old_id="old", target=self.target,
                              data_dir=str(root), run_file=str(self.run_file), daemon_base=self.transport.base,
                              cli_path=str(self.executable), cli_sha256=self.sha256, script_sha256=h.digest(Path(h.__file__).read_bytes()))
        self.run.workspace("old")
        self.run.save()
        with closing(sqlite3.connect(root / "ao.db")) as db, db:
            db.execute("CREATE TABLE sessions(id,project_id,harness,provider_conversation_id)")
            db.execute("INSERT INTO sessions VALUES('old',?,?,'native-old')", (h.PROJECT, old_harness))
            for table, columns in h.EXPORT_COLUMNS.items():
                db.execute("CREATE TABLE " + table + "(" + ",".join(columns) + ")")

    @staticmethod
    def prepare_git(repo, workspace):
        h.mkdir(workspace.parent)
        repo.mkdir()
        def git(*args):
            subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True)
        git("init", "-b", "main")
        (repo / "tracked").write_text("committed")
        (repo / ".gitignore").write_text("ignored.cache\n")
        git("add", ".")
        git("-c", "user.email=x@example.invalid", "-c", "user.name=x", "commit", "-m", "fixture")
        git("worktree", "add", "-b", "original", str(workspace))

    def dispatch_actor(self, _):
        if self.pending_actor is None:
            if self.run.state.get("stage") == "workers" or hasattr(self, "running_receipt_reads"):
                return
            raise AssertionError("unexpected fixture wait: " + self.run.state.get("stage", "?"))
        actor, action = self.pending_actor
        self.pending_actor = None
        if action == "old-receipt":
            self.prepare_old_statement()
        elif action == "worker-receipt":
            h.publish(self.run.root / "workers" / actor / "draft/statement.json", h.canonical(self.worker_statement))
        result = self.transport.execute(actor, action)
        snap = self.snapshots[actor]
        if action == "old-receipt":
            sequence = snap["transcript"]["latestSequence"] + 1
            snap["transcript"]["latestSequence"] = sequence
            snap["transcript"]["entries"].append({"id": "old-ready", "sequence": sequence, "kind": "message", "role": "assistant", "turnId": snap["transcript"]["turns"][-1]["id"],
                "text": h.status_command(self.run.root, self.run_id) + "；10分鐘未見new首報就查原run。"})
            with closing(sqlite3.connect(self.data_dir / "ao.db")) as db, db:
                for entry in snap["transcript"]["entries"]:
                    if entry["kind"] == "message":
                        db.execute("INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,text,branch_id) VALUES(?,?,?,?,?,?,?)",
                                   (entry["id"], "conv", entry.get("turnId"), entry["sequence"], entry["role"], entry["text"], "branch"))
            if self.preservation_problem == "drift":
                original, calls = self.run.workers, [0]
                def workers():
                    calls[0] += 1
                    if calls[0] == 2:
                        (self.workspace / "ignored.cache").write_bytes(b"late drift")
                    return original()
                self.run.workers = workers
        return result

    def prepare_old_statement(self):
        if self.preservation_problem == "alternate":
            (self.repo / ".git/objects/info/alternates").write_text(str(self.workspace) + "\n")
        if self.preservation_problem == "metadata":
            metadata = subprocess.check_output(["git", "-C", str(self.workspace), "rev-parse", "--absolute-git-dir"]).decode().strip()
            (Path(metadata) / "unknown-state").write_text("necessary state not classified")
        if self.preservation_problem == "nested":
            (self.workspace / "nested/.git").mkdir(parents=True)
        if self.preservation_problem == "empty-directory":
            (self.workspace / "unclassified-empty").mkdir()
        classification = [] if self.preservation_problem == "classification" else [
            {"path": "ignored.cache", "disposition": "copy", "basis": "唯一必要附件原件；本turn核對"}]
        statement = dict(schema="rh-old-statement/1", run_id=self.run_id, old_id="old", classification=classification,
                         original_ref="refs/heads/missing" if self.preservation_problem == "ref" else "refs/heads/original", pending_question_refs=[])
        h.publish(self.run.root / "old/draft/statement.json", h.canonical(statement))
        self.managed("old", "handoff.md", self.run.root / "old")

    def session(self, sid):
        self.last_received = h.time.monotonic()
        snap = copy.deepcopy(self.snapshots[sid])
        remaining = getattr(self, "running_receipt_reads", {})
        phase = "2a" if sid == "old" else "bootstrap"
        if self.run.state.get("stage") == phase and remaining.get(sid, 0):
            remaining[sid] -= 1
            turn = snap["transcript"]["turns"][-1]
            turn.update(state="running")
            turn.pop("completedAt", None)
            snap["session"]["activity"]["state"] = "active"
            current = max((e for e in snap["transcript"]["entries"] if e.get("role") == "assistant" and e.get("turnId") == turn["id"]), key=lambda e:e["sequence"])
            current["streaming"] = True
            action = "old-receipt" if sid == "old" else "bootstrap"
            original = next(e for e in reversed(self.helper_events) if e["action"] == action)
            identifier = action + "-tool"
            entry = next(e for e in snap["transcript"]["entries"] if e.get("id") == identifier)
            snap = produced_activity(snap, h.shlex.join(original["argv"]), original["stdout"], None, sid, turn["id"], identifier, entry["sequence"])
        return snap

    def sessions(self):
        return [list_row(s) for s in self.session_list]

    def documents(self):
        result = wire("documents")["documents"]
        result.update(sharedDir=str(self.shared), documentPath="handoff.md")
        result["lease"].update(sessionId=self.owner, epoch=1 if self.owner == "old" else 2, frozen=False)
        return result

    def read(self, args):
        result = wire("detail")
        result["session"].update(copy.deepcopy(self.snapshots[args[2]]["session"]))
        return result

    def get(self, path):
        if path == "projects/" + h.PROJECT:
            result = wire("project")
            result["project"].update(id=h.PROJECT, path=str(self.repo))
            result["project"]["config"]["preRemove"] = ["unknown-hook"] if self.preservation_problem == "hook" else []
            return result
        if path.startswith("agents/accounts/sessions/"):
            return wire(h.provider(self.target) + "-binding")
        if path.endswith("/conversation"):
            return wire("conversation")
        sid = path.split("/")[1]
        result = wire("http-detail")
        result["session"].update(copy.deepcopy(self.snapshots[sid]["session"]))
        return result

    def managed(self, actor, relative, directory):
        content = ("handoff_run_id: " + self.run_id + "\nA–F preserved\n").encode()
        path = self.shared / relative
        path.parent.mkdir(mode=0o700, exist_ok=True)
        path.write_bytes(content)
        result = wire("managed")
        result["file"].update(path=relative, content=content.decode(), hash=h.digest(content))
        raw = h.canonical(result)
        reply = directory / "managed.json"
        h.publish(reply, raw)
        return {"path": str(path), "sha256": h.digest(content)}, {"path": str(reply), "sha256": h.digest(raw)}

    def call(self, args, timeout=30, deadline=None, on_dispatch=None, **kwargs):
        if deadline is not None and h.time.monotonic() > deadline:
            raise h.WindowElapsed()
        if on_dispatch:
            on_dispatch()
        if args[0] == "spawn":
            self.mutations.append("spawn")
            name = args[args.index("--name") + 1]
            snap = snapshot()
            snap["session"].update(id="new", harness=self.target["harness"], displayName=name, createdAt=h.now())
            snap["transcript"]["turns"] = [{"id": "bootstrap", "state": "completed", "requestedAt": h.now(),
                "completedAt": h.now(), "providerTurnId": "native-bootstrap",
                "agentSettings": {"state": "known", "settings": {"model": "model", "effort": "high"}}}]
            snap["transcript"]["entries"] = [
                {"id": "opening", "kind": "message", "role": "user", "origin": "human", "turnId": "bootstrap",
                 "sequence": 1, "text": args[args.index("--prompt") + 1]},
                {"id": "first-report", "sequence": 2, "kind": "message", "role": "assistant", "turnId": "bootstrap", "text": "已接手old；原題與hold保留。"}]
            snap["transcript"]["latestSequence"] = 2
            self.snapshots["new"] = snap
            self.session_list.append(snap["session"])
            self.owner = "new"
            with closing(sqlite3.connect(self.data_dir / "ao.db")) as db, db:
                db.execute("INSERT INTO sessions VALUES('new',?,?,'native-new')", (h.PROJECT, self.target["harness"]))
            self.pending_actor = ("new", "bootstrap")
            return b"spawned session new"
        if args[0] == "session":
            operation, sid = args[1:3]
            self.mutations.append(operation)
            if operation == "kill":
                assert "--json" not in args, "actual kill CLI has no JSON flag"
                self.snapshots[sid]["session"]["isTerminated"] = True
                dirty = subprocess.run(["git", "-C", str(self.workspace), "status", "--porcelain"], capture_output=True, check=True).stdout
                if not dirty or self.kill_outcome == "dirty-deleted":
                    subprocess.run(["git", "-C", str(self.repo), "worktree", "remove", "--force", str(self.workspace)], check=True, capture_output=True)
                if self.kill_outcome == "ref-drift":
                    subprocess.run(["git", "-C", str(self.repo), "update-ref", "-d", "refs/heads/original"], check=True, capture_output=True)
                if self.kill_outcome == "copy-drift":
                    Path(json.loads((self.run.root / "old/preservation.json").read_bytes())["classification"][0]["copy"]["path"]).write_bytes(b"changed")
                if self.kill_outcome == "lost":
                    raise subprocess.TimeoutExpired(args, timeout)
                return wire_bytes("kill-pending" if self.kill_outcome == "pending" else "kill-preserved" if dirty else "kill")
            elif operation == "exit-agent":
                self.snapshots[sid]["session"]["activity"]["state"] = "exited"
            elif operation == "resume-agent":
                self.snapshots[sid]["session"]["activity"]["state"] = "idle"
                result = wire("resume")
                result.update(sessionId=sid)
                result["session"].update(self.snapshots[sid]["session"])
                return h.canonical(result)
            return b"{}"
        if args[0] == "send":
            sid = args[args.index("--session") + 1]
            payload = h.decode(args[args.index("--message") + 1])
            phase = payload["phase"]
            self.mutations.append(phase)
            if phase in ("worker-stop", "2a"):
                handle = args[args.index("--client-message-id") + 1]
                msg = dict(Text="[from old] " + h.canonical(payload).decode(), ClientMessageID=handle,
                           Origin="automation", SenderSessionID="old", SenderProjectID=h.PROJECT)
                active = self.snapshots[sid]["session"]["activity"]["state"] == "active"
                rebatch = getattr(self, "rebatch_directive", False)
                self.snapshots[sid] = persisted_directive(self.snapshots[sid], [msg], active and not rebatch, rebatch)
                self.pending_actor = (sid, "worker-receipt" if phase == "worker-stop" else "old-receipt")
            elif phase in ("nonce", "final"):
                snap = self.snapshots["new"]
                snap["transcript"]["latestSequence"] += 1
                turn = {"id": phase, "state": "completed", "requestedAt": h.now(), "completedAt": h.now(),
                        "providerTurnId": "provider-" + phase, "agentSettings": {"state": "known", "settings": {"model": "model", "effort": "high"}}}
                snap["transcript"]["turns"].append(turn)
                if phase == "nonce":
                    snap["transcript"]["entries"] += [
                        {"id": "prompt", "kind": "message", "turnId": "nonce", "clientMessageId": self.run_id + ":nonce:new"},
                        {"id": "answer", "kind": "message", "role": "assistant", "turnId": "nonce", "text": self.run.state["nonce"]}]
                else:
                    self.pending_actor = ("new", "finalize")
            return b"accepted"
        raise AssertionError(args)


if __name__ == "__main__":
    unittest.main()
