"""RH-SCRIPT1 protocol proofs; never connects to an installed AO daemon."""
import copy
import importlib.util
import json
import os
import sqlite3
import subprocess
from pathlib import Path
import tempfile
import time
import unittest
import uuid
from unittest.mock import patch

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
    return {"session": {"id": "old", "projectId": h.PROJECT, "kind": "orchestrator",
                        "harness": "codex", "mode": "chat", "isTerminated": False,
                        "branch": "fixture", "status": "working" if state == "active" else "idle",
                        "createdAt": "2026-11-08T00:00:00Z", "updatedAt": "2026-11-08T00:00:00Z", "prs": [],
                        "activity": {"state": state, "lastActivityAt": "2026-11-08T00:00:00Z"}},
            "transcript": {"source": "ao_conversation", "order": "oldest_first", "requested": 500,
                           "returned": len(entries), "cap": {"unit": "chars", "value": 0},
                           "conversationId": "conv", "activeBranchId": "branch",
                           "latestSequence": 1, "hasMoreBefore": False, "entries": entries,
                           "turns": turns, "incomplete": incomplete}}


class ProtocolTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.root.chmod(0o700)

    def request(self):
        raw = json.dumps(snapshot(entries=[dict(id="human", role="user", origin="human",
                                                text="handoff", revision=1)] )).encode()
        evidence = self.root / "evidence"
        evidence.mkdir(mode=0o700)
        h.publish(evidence / "human.json", raw)
        return {"schema": "rh-start-request/1", "request_id": str(uuid.uuid4()),
                "project_id": h.PROJECT, "old_id": "old",
                "prepared_by": {"session_id": "old", "turn_id": "preparing"},
                "target": target(), "workers": [],
                "authority": {"source_refs": ["human"], "old_verified_scope": "three-step-handoff-this-project",
                              "t0_source_ref": "human", "t0_created_at": None, "t0_precision": "UNKNOWN"},
                "sources": [{"ref": "human", "kind": "human-message", "conversation_id": "conv",
                             "branch_id": "branch", "message_id": "human", "revision": 1,
                             "ask_ref": None, "raw_relpath": "evidence/human.json",
                             "raw_sha256": h.digest(raw), "message_text_sha256": h.digest(b"handoff")}]}

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

            def call(self, args, timeout=30, deadline=None, on_dispatch=None):
                if on_dispatch:
                    on_dispatch()
                self.calls.append(args)
                if self.fail:
                    raise subprocess.TimeoutExpired(args, timeout)
                return b'{"accepted":true}'
        return h.Run(self.root, FakeCLI())

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
        cli = h.CLI(Path("/usr/bin/true"), "old", self.root, run_file, self.root)
        marked = []
        with patch.object(h.time, "monotonic", side_effect=[0, 2]), patch.object(h.subprocess, "run") as invoked:
            with self.assertRaises(h.WindowElapsed):
                cli.call([], deadline=1, on_dispatch=lambda: marked.append(True))
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
        def expired(args, timeout=30, deadline=None, on_dispatch=None):
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
        self.assertFalse((run.root / "control/kill/sent.json").exists())
        run.cli.call = original
        run.effect("kill", ["session", "kill", "old"], 60)
        self.assertEqual(len(run.cli.calls), 1)

    def test_audit_all_active_workers_receive_stop_in_same_round(self):
        run = self.run_fixture()
        workers = [dict(id=sid, projectId=h.PROJECT, kind="worker", isTerminated=False) for sid in ("w1", "w2")]
        run.cli.sessions = lambda: workers
        def active(sid):
            snap = snapshot("active", turns=[{"id": "busy", "state": "running"}])
            snap["session"].update(id=sid, kind="worker")
            return snap
        run.cli.session = active
        run.request["workers"] = [dict(worker_id=sid, reserved_stop_d="D-7") for sid in ("w1", "w2")]
        self.assertFalse(run.workers())
        self.assertEqual({args[args.index("--session") + 1] for args in run.cli.calls}, {"w1", "w2"})
        self.assertFalse(run.workers())
        self.assertEqual(len(run.cli.calls), 2)

    def test_audit_workspace_uses_branch_registration_without_fake_path(self):
        fake = FlowFixture(self.root, "codex", "codex")
        self.assertNotIn("workspacePath", fake.get("sessions/old")["session"])
        self.assertEqual(fake.run.workspace("old"), str(fake.workspace))
        subprocess.run(["git", "-C", str(fake.repo), "worktree", "add", "--force", str(self.root / "duplicate"), "original"], check=True, capture_output=True)
        with self.assertRaisesRegex(h.Stop, "no unique registered worktree"):
            fake.run.workspace("old")
        fake.snapshots["old"]["session"]["branch"] = "missing-branch"
        with self.assertRaises(h.Stop):
            fake.run.workspace("old")

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
        item = dict(id="new", projectId=h.PROJECT, kind="orchestrator", displayName=token,
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
        with sqlite3.connect(path) as db:
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
        with sqlite3.connect(self.root / "ao.db") as db:
            db.execute("INSERT INTO conversation_activities(id,conversation_id,sequence,kind,status) VALUES('plan','conv',9,'plan','completed')")
        h.run_protocol(fake.run)
        self.assertEqual(fake.run.state["outcome"], "complete")
        for phase in ("manifest", "delta"):
            manifest = h.decode(h.private_read(fake.run.state[phase]["path"]))
            self.assertEqual(manifest["cut_sequence"], 11)
            raw = h.decode(h.private_read(manifest["raw"]["path"]))
            self.assertEqual(max(x["sequence"] for table in ("conversation_messages", "conversation_activities") for x in raw[table]), 10)

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
        self.assertEqual(h.decode(receipt["stdout"]), {"sessionId": "new", "resumeMode": "native"})
        self.assertNotIn("resume", fake.run.state["completed_stages"])
        self.assertEqual(fake.snapshots["new"]["session"]["activity"]["state"], "idle")
        # Recreate the runner from disk as an explicit same-run continue would.
        fake.run = h.Run(fake.run.root, fake)
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
        self.assertEqual(h.private_read(fake.run.root / "old/saved/ignored.cache"), b"unique original")

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
                fake.run.sleep = lambda _: elapsed.__setitem__(0, elapsed[0] + 2)
                fake.running_receipt_reads = {sid: 2}
                h.run_protocol(fake.run)
                self.assertEqual(fake.run.state["outcome"], "complete")
                self.assertEqual(fake.run.state["waits"][phase]["elapsed"], 4)
                self.assertEqual(fake.mutations.count("2a"), 1)
                self.assertEqual(fake.mutations.count("spawn"), 1)

    def test_T01_worker_self_safe_file_waits_for_true_completed_turn(self):
        run = self.run_fixture()
        directory = h.mkdir(self.root / "workers/worker")
        repo = self.root / "repo"
        repo.mkdir()
        subprocess.run(["git", "-C", str(repo), "init", "-b", "fixture"], check=True, capture_output=True)
        (repo / "file").write_text("preserved")
        subprocess.run(["git", "-C", str(repo), "add", "file"], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(repo), "-c", "user.email=x@example.invalid", "-c", "user.name=x", "commit", "-m", "fixture"], check=True, capture_output=True)
        before = h.git_snapshot(repo, directory / "before")
        after = h.git_snapshot(repo, directory / "after")
        stop = h.worker_template(run.run_id, "worker")
        stop.update(state="safe", evidence_turn_id="safe-turn", observed_at=h.now())
        stop["stop_directive"]["last_ack_d"] = "D-7"
        stop["workspace"] = dict(path=str(repo), branch=after["branch"], head=after["head"], before_snapshot=before, after_snapshot=after)
        stop["preservation"] = [dict(after, kind="workspace-snapshot")]
        h.publish(directory / "stop.json", h.canonical(stop))
        run.request["workers"] = [{"worker_id": "worker", "reserved_stop_d": "D-7"}]
        snap = snapshot("active", turns=[{"id": "safe-turn", "state": "running"}])
        snap["session"].update(id="worker", kind="worker")
        run.cli.session = lambda _: copy.deepcopy(snap)
        run.cli.get = lambda path: {"project": dict(id=h.PROJECT, path=str(repo))} if path.startswith("projects/") else {"session": dict(id="worker", projectId=h.PROJECT, branch="fixture")}
        self.assertFalse(run.worker_stop({"id": "worker"}))
        snap["session"]["activity"]["state"] = "idle"
        snap["transcript"]["turns"][0].update(state="completed", completedAt=h.now())
        snap["transcript"]["incomplete"] = []
        self.assertEqual(run.worker_stop({"id": "worker"})["source_type"], "worker-self")

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
        workspace = self.root / "workspace"
        workspace.mkdir()
        subprocess.run(["git", "-C", str(workspace), "init", "-b", "fixture"], check=True, capture_output=True)
        (workspace / "file").write_text("base")
        subprocess.run(["git", "-C", str(workspace), "add", "file"], check=True, capture_output=True)
        subprocess.run(["git", "-C", str(workspace), "-c", "user.email=x@example.invalid", "-c", "user.name=x", "commit", "-m", "fixture"], check=True, capture_output=True)
        (workspace / "file").write_text("dirty preserved")
        idle = snapshot()
        idle["session"].update(id="worker", kind="worker")
        run.cli.session = lambda _: copy.deepcopy(idle)
        run.cli.get = lambda path: {"project": dict(id=h.PROJECT, path=str(workspace))} if path.startswith("projects/") else {"session": dict(id="worker", projectId=h.PROJECT, branch="fixture")}
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
        class PreparedCLI:
            executable = Path(h.__file__)
            sha256 = h.digest(executable.read_bytes())
            data_dir = self.root
            run_file = self.root / "run.json"
        cli = PreparedCLI()
        with patch.object(h, "qualified", return_value=cli), patch.object(h, "validate_sources_live", return_value=snapshot()), \
             patch.object(h, "target_preflight", return_value={}), patch.object(h, "native_identity", return_value="old-native"), \
             patch.object(h, "launch") as launch:
            first = h.start(root, cli, "old", target())
            second = h.start(root, cli, "old", target())
            self.assertEqual(first["run_id"], second["run_id"])
            self.assertEqual(launch.call_count, 1)
            self.assertEqual(h.private_read(root / req["request_id"] / "start.json"), raw)
            saved_script = root / req["request_id"] / "handoff.py"
            self.assertEqual(h.private_read(saved_script), Path(h.__file__).read_bytes())
            self.assertIn(str(saved_script), h.status_command(root / req["request_id"], req["request_id"]))
            # A byte change cannot steal the already-bound UUID.
            (slot / "request.json").write_bytes(raw + b"\n")
            with self.assertRaises(h.Stop):
                h.start(root, cli, "old", target())

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
        async_snapshot = snapshot(entries=[{"kind": "activity", "status": "completed", "activityKind": "command",
                                            "detail": {"output": "Script running with cell ID job42"}}])
        self.assertFalse(h.quiescent(async_snapshot))
        self.assertTrue(h.quiescent(async_snapshot, [{"reference": "job42", "state": "completed"}]))
        self.assertFalse(h.quiescent(async_snapshot, [{"reference": "job42", "state": "running"}]))

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
        with self.assertRaises(h.Stop):
            h.validate_request(req, self.root, "old", target())
        worker["release"]["mode"] = "keep-held"
        h.validate_request(req, self.root, "old", target())



class FlowFixture:
    """A persisted fake daemon: actual exporter/receipt checks, zero live I/O."""
    def __init__(self, root, old_harness, new_harness):
        self.data_dir = root
        self.old_harness, self.target = old_harness, target(new_harness)
        root.chmod(0o700)
        self.shared = h.mkdir(root / "shared")
        self.workspace = root / "workspace"
        self.repo = root / "repository"
        self.repo.mkdir()
        def git(*args):
            return subprocess.run(["git", "-C", str(self.repo), *args], check=True, capture_output=True).stdout
        git("init", "-b", "main")
        (self.repo / "tracked").write_text("committed")
        (self.repo / ".gitignore").write_text("ignored.cache\n")
        git("add", ".")
        git("-c", "user.email=x@example.invalid", "-c", "user.name=x", "commit", "-m", "fixture")
        git("worktree", "add", "-b", "original", str(self.workspace))
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
        self.owner = "old"
        self.run_id = str(uuid.uuid4())
        directory = h.mkdir(root / self.run_id)
        h.publish(directory / "handoff.py", Path(h.__file__).read_bytes())
        req = {"request_id": self.run_id, "old_id": "old", "target": self.target,
               "authority": {"source_refs": ["human"], "t0_precision": "UNKNOWN", "t0_created_at": None}, "workers": []}
        h.publish(directory / "start.json", h.canonical(req))
        h.publish(directory / "state.json", h.canonical({"start_sha256": h.digest(h.canonical(req)), "effects": {},
                  "original_t0": req["authority"], "old_settings": h.old_settings(self.snapshots["old"]), "old_native": "native-old"}))
        self.run = h.Run(directory, self)
        self.run.sleep = lambda _: (_ for _ in ()).throw(AssertionError("unexpected fixture wait: " + self.run.state.get("stage", "?")))
        with sqlite3.connect(root / "ao.db") as db:
            db.execute("CREATE TABLE sessions(id,project_id,harness,provider_conversation_id)")
            db.execute("INSERT INTO sessions VALUES('old',?,?,'native-old')", (h.PROJECT, old_harness))
            for table, columns in h.EXPORT_COLUMNS.items():
                db.execute("CREATE TABLE " + table + "(" + ",".join(columns) + ")")

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
            snap["transcript"]["entries"] += [
                {"kind": "message", "role": "assistant", "turnId": turn["id"], "sequence": 8, "streaming": True, "text": "partial"},
                {"kind": "activity", "activityKind": "command", "turnId": turn["id"], "sequence": 9, "status": "running"}]
            snap["transcript"]["returned"] = len(snap["transcript"]["entries"])
            snap["transcript"]["incomplete"] = snapshot(turns=snap["transcript"]["turns"], entries=snap["transcript"]["entries"])["transcript"]["incomplete"]
        return snap

    def sessions(self):
        return copy.deepcopy(self.session_list)

    def documents(self):
        return {"sharedDir": str(self.shared), "documentPath": "handoff.md", "lease":
                {"sessionId": self.owner, "epoch": 1 if self.owner == "old" else 2, "frozen": False}}

    def read(self, args):
        return {"session": copy.deepcopy(self.snapshots[args[2]]["session"])}

    def get(self, path):
        if path == "projects/" + h.PROJECT:
            return {"status": "ok", "project": {"id": h.PROJECT, "path": str(self.repo), "config":
                    {"preRemove": ["unknown-hook"] if self.preservation_problem == "hook" else []}}}
        if path.startswith("agents/accounts/sessions/"):
            p = h.provider(self.target)
            return {p: "acct", "bindings": {p: {"accountId": "acct", "available": True}}}
        if path.endswith("/conversation"):
            return {"settings": {"model": "model", "reasoningEffort": "high", "approvalMode": "bypass-permissions", "fastMode": False}}
        sid = path.split("/")[1]
        return {"session": copy.deepcopy(self.snapshots[sid]["session"])}

    def managed(self, actor, relative, directory):
        content = ("handoff_run_id: " + self.run_id + "\nA–F preserved\n").encode()
        path = self.shared / relative
        path.parent.mkdir(mode=0o700, exist_ok=True)
        path.write_bytes(content)
        raw = h.canonical({"file": {"path": relative, "content": content.decode(), "hash": h.digest(content)}})
        reply = directory / "managed.json"
        h.publish(reply, raw)
        return {"path": str(path), "sha256": h.digest(content)}, {"path": str(reply), "sha256": h.digest(raw)}

    def call(self, args, timeout=30, deadline=None, on_dispatch=None):
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
            snap["transcript"]["entries"] = [{"kind": "activity", "activityKind": "command", "status": "completed",
                "turnId": "bootstrap", "detail": {"command": h.producer_command(self.run.root, "bootstrap"), "exitCode": 0}},
                {"id": "opening", "kind": "message", "role": "user", "origin": "human", "turnId": "bootstrap",
                 "sequence": 1, "text": args[args.index("--prompt") + 1]},
                {"kind": "message", "role": "assistant", "turnId": "bootstrap", "text": "已接手 old；原題與hold保留。"}]
            self.snapshots["new"] = snap
            self.session_list.append(snap["session"])
            self.owner = "new"
            with sqlite3.connect(self.data_dir / "ao.db") as db:
                db.execute("INSERT INTO sessions VALUES('new',?,?,'native-new')", (h.PROJECT, self.target["harness"]))
            reference, reply = self.managed("new", "receipt.md", self.run.root / "bootstrap/new")
            receipt = {"schema": "rh-new-receipt/1", "run_id": self.run_id, "old_id": "old", "new_id": "new",
                "evidence_turn_id": "bootstrap", "created_at": h.now(), "native_id": "native-new",
                "target": {k: v for k, v in self.target.items() if k != "mode"},
                "sources": {"handoff_sha256": self.run.state["handoff"]["sha256"],
                            "manifest_sha256": self.run.state["manifest"]["sha256"], "delta_sha256": self.run.state["delta"]["sha256"]},
                "managed_reply": reply, "managed_receipt": reference,
                "lease": h.lease_view(self.documents()), "pending_question_refs": [], "diagnostics": [], "outcome": "ready"}
            h.publish(self.run.root / "bootstrap/new/receipt.json", h.canonical(receipt))
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
                    (self.run.root / "old/saved/ignored.cache").write_bytes(b"changed")
                if self.kill_outcome == "lost":
                    raise subprocess.TimeoutExpired(args, timeout)
                suffix = " (workspace cleanup pending)" if self.kill_outcome == "pending" else " (workspace preserved)" if dirty else ""
                return ("session old killed" + suffix + "\n").encode()
            elif operation == "exit-agent":
                self.snapshots[sid]["session"]["activity"]["state"] = "exited"
            elif operation == "resume-agent":
                self.snapshots[sid]["session"]["activity"]["state"] = "idle"
                return h.canonical({"sessionId": sid, "resumeMode": "native"})
            return b"{}"
        if args[0] == "send":
            sid = args[args.index("--session") + 1]
            payload = h.decode(args[args.index("--message") + 1])
            phase = payload["phase"]
            self.mutations.append(phase)
            if phase == "2a":
                if self.preservation_problem == "alternate":
                    (self.repo / ".git/objects/info/alternates").write_text(str(self.workspace) + "\n")
                if self.preservation_problem == "metadata":
                    metadata = subprocess.run(["git", "-C", str(self.workspace), "rev-parse", "--absolute-git-dir"], check=True, capture_output=True).stdout.decode().strip()
                    (Path(metadata) / "unknown-state").write_text("necessary state not classified")
                if self.preservation_problem == "nested":
                    (self.workspace / "nested/.git").mkdir(parents=True)
                if self.preservation_problem == "empty-directory":
                    (self.workspace / "unclassified-empty").mkdir()
                h.publish(self.run.root / "old/saved/ignored.cache", b"unique original")
                classification = [{"path": "ignored.cache", "disposition": "copy", "basis": "唯一必要附件原件；本 turn 核對",
                    "copy": {"path": str(self.run.root / "old/saved/ignored.cache"), "sha256": h.digest(b"unique original")}}]
                if self.preservation_problem == "classification":
                    classification = []
                h.publish(self.run.root / "old/preservation.json", h.canonical({"schema": "rh-old-preservation/1",
                    "run_id": self.run_id, "old_id": "old", "evidence_turn_id": "turn",
                    "original_ref": "refs/heads/missing" if self.preservation_problem == "ref" else "refs/heads/original",
                    "classification": classification}))
                reference, reply = self.managed("old", "handoff.md", self.run.root / "old")
                receipt = {"schema": "rh-old-2a/1", "run_id": self.run_id, "old_id": "old", "evidence_turn_id": "turn",
                           "created_at": h.now(), "managed_reply": reply, "handoff": reference,
                           "lease": h.lease_view(self.documents()), "worker_index_sha256": self.run.state["stop_index_sha256"],
                           "pending_question_refs": [], "user_status_instruction": h.status_command(self.run.root, self.run_id)}
                h.publish(self.run.root / "old/phase-2a.json", h.canonical(receipt))
                self.snapshots["old"]["transcript"]["entries"].append({"id": "2a-control", "kind": "message", "role": "user", "origin": "automation",
                    "senderSessionId": "old", "sequence": 2, "clientMessageId": self.run_id + ":2a:old", "text": "[from old] " + h.canonical(payload).decode()})
                self.snapshots["old"]["transcript"]["entries"].append({"id": "old-ready", "sequence": 10, "kind": "message", "role": "assistant", "turnId": "turn",
                    "text": h.status_command(self.run.root, self.run_id) + "；10分鐘未見new首報就查原run。"})
                with sqlite3.connect(self.data_dir / "ao.db") as db:
                    for entry in self.snapshots["old"]["transcript"]["entries"]:
                        if entry["kind"] != "message":
                            continue
                        db.execute("INSERT INTO conversation_messages(id,conversation_id,sequence,role,text,branch_id) VALUES(?,?,?,?,?,?)",
                                   (entry["id"], "conv", entry["sequence"], entry["role"], entry["text"], "branch"))
                if self.preservation_problem == "drift":
                    original = self.run.workers
                    calls = [0]
                    def workers():
                        calls[0] += 1
                        if calls[0] == 2:
                            (self.workspace / "ignored.cache").write_bytes(b"late drift")
                        return original()
                    self.run.workers = workers
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
                    h.publish(self.run.root / "bootstrap/new/final.json", h.canonical(
                        {"run_id": self.run_id, "new_id": "new", "evidence_turn_id": "final", "notifications": []}))
            return b"accepted"
        raise AssertionError(args)


if __name__ == "__main__":
    unittest.main()
