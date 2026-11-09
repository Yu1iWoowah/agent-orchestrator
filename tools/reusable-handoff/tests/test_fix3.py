"""RH-FIX3: real Git evidence; sensitive values are synthetic fixture markers."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from test_handoff import h
from test_handoff import FlowFixture
import test_fix2 as fix2


MARKER = b"RH_FIX3_UNCOMMITTED_FIXTURE_MARKER\n"


class WorkspaceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.fresh("initial")

    def fresh(self, name):
        self.root = h.mkdir(self.base / name)
        self.repo = h.mkdir(self.root / "repo")
        self.git(self.repo, "init", "-b", "main", "--template=")
        (self.repo / "plain").write_bytes(b"original\n")
        self.commit(self.repo)

    def git(self, path, *args):
        return subprocess.check_output(["git", "--no-optional-locks", "-c", "core.hooksPath=/dev/null",
                                       "-C", str(path), *args], stderr=subprocess.PIPE)

    def commit(self, path):
        self.git(path, "add", ".")
        self.git(path, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
                 "commit", "-m", "fixture")

    def template(self):
        path = self.repo / "cloud/.env.example"
        path.parent.mkdir()
        path.write_bytes(b"PUBLIC_FIXTURE=example\n")
        self.commit(self.repo)
        return path

    def old(self):
        workspace = self.root / "old-workspace"
        self.git(self.repo, "worktree", "add", "-b", "old", str(workspace))
        root = h.mkdir(self.root / "run")
        data = h.mkdir(self.root / "data")
        shared = h.mkdir(data / "shared")
        cli = SimpleNamespace(data_dir=data, get=lambda _: dict(status="ok", project=dict(id=h.PROJECT, config={})),
                              documents=lambda: dict(sharedDir=str(shared)))
        run = SimpleNamespace(root=root, old="old", cli=cli, state={})
        plan = dict(original_ref="refs/heads/old", classification=[])
        h.publish(root / "old/preservation.json", h.canonical(plan))
        return workspace, run, plan

    def no_marker(self, directory):
        for path in directory.rglob("*"):
            if path.is_file():
                self.assertNotIn(MARKER, path.read_bytes(), str(path))
                self.assertNotIn(MARKER.hex().encode(), path.read_bytes(), str(path))

    def test_committed_template_worker_and_old(self):
        source = self.template()
        oid = self.git(self.repo, "rev-parse", "HEAD").decode().strip()
        self.git(self.repo, "update-index", "--add", "--cacheinfo", "160000," + oid + ",module")
        (self.repo / "module").mkdir()
        self.commit(self.repo)
        result = h.git_snapshot(self.repo, self.root / "snapshot")
        doc = json.loads(Path(result["path"]).read_bytes())
        item = doc["files"]["cloud/.env.example"]
        self.assertEqual((self.root / "snapshot/objects" / item["sha256"]).read_bytes(), source.read_bytes())
        workspace, run, plan = self.old()
        proof = h.old_git_state(run, plan, workspace)
        self.assertEqual(proof["head"], self.git(workspace, "rev-parse", "HEAD").decode().strip())
        self.assertEqual(proof["gitlinks"]["module"]["state"], "uninitialized")

    def test_staged_sensitive_marker_never_reaches_patch(self):
        path = self.template()
        path.write_bytes(MARKER)
        self.git(self.repo, "add", "cloud/.env.example")
        destination = self.root / "snapshot"
        with self.assertRaises(h.Stop):
            h.git_snapshot(self.repo, destination)
        self.no_marker(destination)

    def test_old_tracked_template_modification_stops(self):
        self.template()
        workspace, run, plan = self.old()
        (workspace / "cloud/.env.example").write_bytes(MARKER)
        with self.assertRaises(h.Stop):
            h.old_git_state(run, plan, workspace)
        self.no_marker(run.root)

    def test_untracked_ignored_staged_and_deleted_names_stop_before_saving(self):
        cases = ["untracked", "ignored", "added", "auth-added", "staged", "working", "index-only", "deleted", "renamed", "flag", "mode", "conflict"]
        for role in ("worker", "old"):
            for case in cases:
                with self.subTest(role=role, case=case):
                    self.fresh(role + "-" + case)
                    self.template()
                    if role == "old":
                        workspace, run, plan = self.old()
                        destination = run.root
                    else:
                        workspace, destination = self.repo, self.root / "snapshot"
                    source = workspace / "cloud/.env.example"
                    if case in ("untracked", "ignored"):
                        if case == "ignored":
                            (workspace / ".gitignore").write_text(".env\n")
                        (workspace / ".env").write_bytes(MARKER)
                    elif case in ("added", "auth-added"):
                        source = workspace / ("new/.env.example" if case == "added" else "auth.json")
                        source.parent.mkdir(exist_ok=True)
                        source.write_bytes(MARKER)
                        self.git(workspace, "add", str(source.relative_to(workspace)))
                    elif case in ("staged", "working", "index-only"):
                        original = source.read_bytes()
                        source.write_bytes(MARKER)
                        if case != "working":
                            self.git(workspace, "add", "cloud/.env.example")
                        if case == "index-only":
                            source.write_bytes(original)
                    elif case == "deleted":
                        self.git(workspace, "rm", "cloud/.env.example")
                    elif case == "renamed":
                        self.git(workspace, "mv", "cloud/.env.example", "renamed")
                        (workspace / "renamed").write_bytes(MARKER)
                    elif case == "flag":
                        self.git(workspace, "update-index", "--assume-unchanged", "cloud/.env.example")
                        source.write_bytes(MARKER)
                    elif case == "mode":
                        source.chmod(0o755)
                    else:
                        oid = self.git(workspace, "rev-parse", "HEAD:cloud/.env.example").decode().strip()
                        data = ("0 " + "0" * 40 + "\tcloud/.env.example\n100644 " + oid + " 1\tcloud/.env.example\n").encode()
                        subprocess.run(["git", "-C", str(workspace), "update-index", "--index-info"], input=data, check=True, capture_output=True)
                    with self.assertRaises(h.Stop):
                        if role == "old":
                            plan["classification"] = [{"path": ".env", "disposition": "discardable", "basis": "not a bypass"}]
                            h.old_git_state(run, plan, workspace)
                        else:
                            h.git_snapshot(workspace, destination)
                    self.no_marker(destination)

    def test_other_committed_sensitive_names_and_parent_paths_still_stop(self):
        for i, relative in enumerate([".env", "auth.json", ".env.sample", ".env.template", ".env/.env.example", "auth.json/.env.example"]):
            for role in ("worker", "old"):
                with self.subTest(path=relative, role=role):
                    self.fresh(role + str(i))
                    source = self.repo / relative
                    source.parent.mkdir(parents=True, exist_ok=True)
                    source.write_bytes(MARKER)
                    self.commit(self.repo)
                    if role == "old":
                        workspace, run, plan = self.old()
                        destination = run.root
                    else:
                        workspace, destination = self.repo, self.root / "snapshot"
                    with self.assertRaises(h.Stop):
                        h.old_git_state(run, plan, workspace) if role == "old" else h.git_snapshot(workspace, destination)
                    self.no_marker(destination)

    def test_symlink_template_and_git_filter_do_not_prove_raw_equality(self):
        for problem in ("symlink", "filter"):
            self.fresh(problem)
            source = self.template()
            if problem == "symlink":
                source.unlink()
                source.symlink_to("../plain")
                self.commit(self.repo)
            else:
                (self.repo / ".gitattributes").write_text("cloud/.env.example filter=fixture\n")
                self.git(self.repo, "config", "filter.fixture.clean", "sed s/MODIFIED/PUBLIC_FIXTURE/g")
                self.commit(self.repo)
                source.write_bytes(b"MODIFIED=example\n")
                self.git(self.repo, "add", "cloud/.env.example")
                self.assertEqual(self.git(self.repo, "status", "--porcelain"), b"")
            with self.assertRaises(h.Stop):
                h.git_snapshot(self.repo, self.root / "snapshot")

    def test_normal_staged_dirty_untracked_and_literal_paths_remain_preserved(self):
        self.template()
        (self.repo / "plain").write_bytes(b"staged ordinary\n")
        self.git(self.repo, "add", "plain")
        (self.repo / "plain").write_bytes(b"dirty ordinary\n")
        (self.repo / "literal[abc]").write_bytes(b"untracked ordinary")
        result = h.git_snapshot(self.repo, self.root / "snapshot")
        doc = h.decode(Path(result["path"]).read_bytes())
        self.assertIn("literal[abc]", doc["files"])
        self.assertIn(b"staged ordinary", (self.root / "snapshot/index.patch").read_bytes())
        self.assertIn(b"dirty ordinary", (self.root / "snapshot/worktree.patch").read_bytes())
        self.assertNotIn(b".env.example", (self.root / "snapshot/index.patch").read_bytes())
        workspace, run, plan = self.old()
        (workspace / "plain").write_bytes(b"ordinary old dirty")
        self.assertIn(b"ordinary old dirty", bytes.fromhex(h.old_git_state(run, plan, workspace)["dirty_patch"]))

    def test_template_drift_uses_checked_bytes_and_never_adds_sensitive_patch(self):
        for role in ("worker", "old"):
            self.fresh(role)
            self.template()
            if role == "old":
                workspace, run, plan = self.old()
                destination = run.root
            else:
                workspace, destination = self.repo, self.root / "snapshot"
            original = h.workspace_patch
            def drift(path, inputs, cached=False):
                (workspace / "cloud/.env.example").write_bytes(MARKER)
                return original(path, inputs, cached)
            with patch.object(h, "workspace_patch", side_effect=drift), self.assertRaises(h.Stop):
                h.old_git_state(run, plan, workspace) if role == "old" else h.git_snapshot(workspace, destination)
            self.no_marker(destination)
            self.assertFalse((destination / "snapshot.json").exists())

    def test_empty_patch_inventory_does_not_fall_back_to_whole_tree(self):
        source = self.template()
        self.git(self.repo, "rm", "plain")
        self.commit(self.repo)
        inputs = h.workspace_git_inputs(self.repo)
        self.assertEqual(inputs["patch_paths"], [])
        source.write_bytes(MARKER)
        with patch.object(h, "old_git", side_effect=AssertionError("must not invoke unrestricted diff")):
            self.assertEqual(h.workspace_patch(self.repo, inputs), b"")

    def test_empty_committed_template_intent_to_add_is_not_stage_zero_proof(self):
        source = self.template()
        source.write_bytes(b"")
        self.commit(self.repo)
        self.git(self.repo, "rm", "--cached", "cloud/.env.example")
        self.git(self.repo, "add", "-N", "cloud/.env.example")
        with self.assertRaises(h.Stop):
            h.git_snapshot(self.repo, self.root / "snapshot")

    def test_unknown_partial_snapshot_object_type_is_rejected_before_history(self):
        root = h.mkdir(self.root / "runs")
        run = h.mkdir(root / h.ZERO_CONTROL_RUN)
        h.publish(root / ".qualified/qualification.json", b"{}")
        h.publish(run / "workers" / h.RETIRABLE_WORKERS[0] / ("before-" + "a" * 32) / "objects", b"not an objects directory")
        state = dict(effects={}, project_id=h.PROJECT, target={}, script_sha256="fixture", cli_sha256="fixture",
                     error="sensitive workspace content cannot be preserved: cloud/.env.example")
        authorization = dict(state, expected_effects=[], qualification_sha256=h.digest(b"{}"))
        with self.assertRaisesRegex(h.Stop, "partial snapshot"):
            h.zero_control_history(root, run, None, authorization, state, {})

    def test_copy_and_dependency_entries_do_not_inherit_template_exception(self):
        source = self.template()
        workspace, run, plan = self.old()
        row = dict(path=str(source), kind="fixture", basis="necessary")
        with self.assertRaises(h.Stop):
            h.source_inputs(run, "old", self.repo, [row])
        self.assertTrue(h.source_inputs(run, "old", self.repo, [dict(row, path=str(self.repo / "plain"))]))
        plan["classification"] = [dict(path="cloud/.env.example", disposition="copy", basis="cannot bypass")]
        with self.assertRaises(h.Stop):
            h.old_git_state(run, plan, workspace)
        with self.assertRaises(h.Stop):
            h.preserve_dependencies([source], self.root / "dependencies", [self.repo])

    def test_actual_checkout_tree_worker_and_linked_old(self):
        source = Path(h.__file__).resolve().parents[2]
        revision = self.git(source, "rev-parse", "HEAD").decode().strip()
        self.git(self.repo, "fetch", "--no-tags", "--depth=1", source.as_uri(), revision)
        self.git(self.repo, "checkout", "--detach", revision)
        result = h.git_snapshot(self.repo, self.root / "actual-snapshot")
        doc = h.decode(Path(result["path"]).read_bytes())
        self.assertIn("cloud/.env.example", doc["files"])
        self.assertEqual(doc["gitlinks"]["private/ao-cloud"]["state"], "uninitialized")
        workspace, run, plan = self.old()
        proof = h.old_git_state(run, plan, workspace)
        self.assertEqual(proof["head"], revision)
        self.assertEqual(proof["tree"]["cloud/.env.example"]["sha256"], doc["files"]["cloud/.env.example"]["sha256"])


class ProducerTests(unittest.TestCase):
    def add_template(self, workspace):
        source = workspace / "cloud/.env.example"
        source.parent.mkdir()
        source.write_bytes(b"PUBLIC_FIXTURE=example\n")
        subprocess.run(["git", "-C", str(workspace), "add", "cloud/.env.example"], check=True, capture_output=True)
        git = lambda *args: subprocess.check_output(["git", "-C", str(workspace), "-c", "user.name=fixture",
                                                      "-c", "user.email=fixture@example.invalid", *args], stderr=subprocess.PIPE).decode().strip()
        # A real commit without leaving unrelated COMMIT_EDITMSG in the linked old metadata.
        commit = git("commit-tree", git("write-tree"), "-p", "HEAD", "-m", "template")
        git("update-ref", "HEAD", commit)
        return source

    def test_script_quiescent_and_old_real_producer_capture_verify(self):
        with tempfile.TemporaryDirectory() as temp:
            fake = FlowFixture(Path(temp), "claude-code", "codex", worker=True)
            fake.run.request["workers"][0].update(stop_d_source_refs=[], hold_state="held", hold_source_refs=["human"],
                task_owner_id=None, task_refs=[], release=dict(mode="keep-held", authority_refs=[],
                reserved_resume_d=None, resume_d_source_refs=[]))
            request_raw = h.canonical(fake.run.request)
            (fake.run.root / "start.json").write_bytes(request_raw)
            fake.run.state["start_sha256"] = h.digest(request_raw)
            self.add_template(fake.worker_path)
            self.add_template(fake.workspace)
            h.run_protocol(fake.run)
            self.assertEqual(fake.run.state["outcome"], "complete")
            stop = h.decode(h.private_read(fake.run.root / "workers/worker/stop.json"))
            self.assertEqual(stop["source_type"], "script-quiescent")
            proof = h.decode(h.private_read(fake.run.root / "old/preservation-evidence.json"))
            self.assertIn("sha256", proof["tree"]["cloud/.env.example"])

    def test_worker_self_real_helper_and_consumer_positive_and_dirty_negative(self):
        for dirty in (False, True):
            with self.subTest(dirty=dirty):
                fixture = fix2.WorkerProducerTests()
                fixture.setUp()
                self.addCleanup(fixture.doCleanups)
                fake, _ = fixture.prepared()
                source = self.add_template(fake.worker_path)
                if dirty:
                    source.write_bytes(MARKER)
                    with self.assertRaises(h.Stop):
                        fake.dispatch_actor(2)
                    for path in fake.run.root.rglob("*"):
                        if path.is_file():
                            self.assertNotIn(MARKER, path.read_bytes())
                else:
                    receipt = fake.dispatch_actor(2)
                    self.assertEqual(receipt["state"], "safe")
                    self.assertEqual(fake.run.worker_stop({"id": "worker"})["state"], "safe")

    def test_old_helper_refuses_dirty_template_before_saved_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            fake = FlowFixture(Path(temp), "codex", "claude-code")
            self.add_template(fake.workspace).write_bytes(MARKER)
            with self.assertRaises(h.Stop):
                h.run_protocol(fake.run)
            self.assertNotIn("kill", fake.mutations)
            for path in fake.run.root.rglob("*"):
                if path.is_file():
                    self.assertNotIn(MARKER, path.read_bytes())


class ZeroControlRetirementTests(unittest.TestCase):
    def setUp(self):
        self.fixture = f = fix2.RetirementTests()
        f.setUp()
        self.addCleanup(f.doCleanups)
        for sid, ack, next_d in [(h.RETIRABLE_WORKERS[0], "D-3", None), (h.RETIRABLE_WORKERS[1], "D-1", "D-3")]:
            path = f.run / "workers" / sid / "stop.json"
            stop = h.decode(path.read_bytes())
            stop["stop_directive"] = dict(last_ack_d=ack, next_d=next_d)
            path.write_bytes(h.canonical(stop))
        self.previous_receipt = f.retire()
        self.previous_raw = h.private_read(f.directory / "receipt.json")
        self.archive = f.root / ".retired" / h.RETIRABLE_RUN
        self.run = h.mkdir(f.root / h.ZERO_CONTROL_RUN)
        self.directory = h.mkdir(f.root / ".retirement" / h.ZERO_CONTROL_RUN)
        request = h.decode(h.private_read(self.archive / "start.json"))
        request["request_id"] = h.ZERO_CONTROL_RUN
        for worker in request["workers"]:
            worker.update(reserved_stop_d="D-4", hold_state="held")
        self.request_raw = h.canonical(request)
        h.publish(self.run / "start.json", self.request_raw)
        h.publish(f.slot / "request.json", self.request_raw)
        state = h.decode(h.private_read(self.archive / "state.json"))
        state.update(run_id=h.ZERO_CONTROL_RUN, effects={}, start_sha256=h.digest(self.request_raw),
                     error="sensitive workspace content cannot be preserved: cloud/.env.example")
        h.publish(self.run / "handoff.py", h.private_read(self.archive / "handoff.py"))
        h.publish(self.run / "runner.lock", b"")
        child = subprocess.Popen(["python3", "-I", "-B", "-c", "pass"], start_new_session=True)
        child.wait(timeout=10)
        h.publish(self.run / "runner.json", h.canonical(dict(pid=child.pid, sid=child.pid, launched_at=h.now())))
        h.publish(self.run / "runner-intent.json", h.canonical(dict(started_at=h.now())))
        h.publish(self.run / "state.json", h.canonical(state))
        h.publish(self.run / "workers" / h.RETIRABLE_WORKERS[0] / ("before-" + "a" * 32) / "index.patch", b"")
        authorization = dict(f.authorization, run_id=h.ZERO_CONTROL_RUN, start_sha256=state["start_sha256"], expected_effects=[],
                             project_id=h.PROJECT, target=state["target"], script_sha256=state["script_sha256"],
                             cli_sha256=state["cli_sha256"], qualification_sha256=h.digest(h.private_read(f.root / ".qualified/qualification.json")))
        h.publish(self.directory / "authorization.json", h.canonical(authorization))
        # The original _run STOP notification is not a worker/lifecycle dispatch.
        f.transport.sync()
        executable = f.root / ".qualified/ao"
        executable.chmod(0o700)
        try:
            cli = h.CLI(executable, f.old, f.data, f.initial.run_file, self.run)
            cli.capture_root = h.mkdir(self.run / "wire")
            notice = ["report", "--needs-input", "--note", "RH3 " + h.ZERO_CONTROL_RUN + " STOP: " + state["error"][:650] +
                      "; " + h.status_command(self.run, h.ZERO_CONTROL_RUN)]
            with self.assertRaises(h.Stop):
                cli.call(notice)
        finally:
            executable.chmod(0o600)

    def retire(self):
        f = self.fixture
        f.transport.sync()
        cli = f.root / ".qualified/ao"
        cli.chmod(0o700)
        try:
            with patch.dict(os.environ, {"AO_SESSION_ID": "operator"}):
                return h.retire_run(f.root, h.ZERO_CONTROL_RUN, f.initial)
        finally:
            cli.chmod(0o600)

    def test_zero_control_archive_coexists_and_cached_receipt_is_immutable(self):
        before = h.private_manifest(self.run)
        receipt = self.retire()
        self.assertEqual(receipt["closed_effects"], [])
        self.assertEqual(receipt["hold"], "unchanged")
        self.assertEqual(self.retire(), receipt)
        archive = self.fixture.root / ".retired" / h.ZERO_CONTROL_RUN
        self.assertEqual([x for x in h.private_manifest(archive) if not x["path"].startswith("retired-start-slot")], before)
        self.assertEqual(h.private_read(self.archive.parent.parent / ".retirement" / h.RETIRABLE_RUN / "receipt.json"), self.previous_raw)
        self.assertEqual(len(h.retirement_guard(self.fixture.root, self.fixture.old)), 2)
        for run_id in h.RETIRABLE_RUNS:
            with self.assertRaises(h.Stop):
                h.load_run(self.fixture.root, run_id, self.fixture.initial)
        with self.assertRaises(h.Stop):
            h.retire_run(self.fixture.root, "00000000-0000-0000-0000-000000000000", self.fixture.initial)

    def test_control_unknown_producer_and_later_phase_are_not_zero(self):
        state_file = self.run / "state.json"
        original = state_file.read_bytes()
        for changes in [dict(effects={"send": "UNKNOWN"}), dict(stages={"2a": {}}), dict(new_id="new")]:
            state_file.write_bytes(h.canonical(dict(h.decode(original), **changes)))
            with self.assertRaises(h.Stop):
                self.retire()
            self.assertTrue(self.run.exists())
        state_file.write_bytes(original)
        control = h.mkdir(self.run / "control")
        with self.assertRaises(h.Stop):
            self.retire()
        control.rmdir()
        h.publish(self.run / "observations/intent.json", b"{}")
        with self.assertRaises(h.Stop):
            self.retire()

    def test_active_runner_wrong_slot_and_later_steer_block(self):
        f = self.fixture
        slot = f.slot / "request.json"
        slot.write_bytes(b"other request")
        with self.assertRaises(h.Stop):
            self.retire()
        slot.write_bytes(self.request_raw)
        runner = self.run / "runner.json"
        old = runner.read_bytes()
        runner.write_bytes(h.canonical(dict(pid=os.getpid(), sid=os.getpid(), launched_at=h.now())))
        with self.assertRaises(h.Stop):
            self.retire()
        runner.write_bytes(old)
        snap = f.snaps[h.RETIRABLE_WORKERS[0]]
        snap["transcript"]["entries"].append(dict(kind="activity", activityKind="system", sequence=3, detail=dict(event="steer")))
        with self.assertRaises(h.Stop):
            self.retire()

    def test_pending_second_retirement_blocks_new_start_and_unknown_record(self):
        f = self.fixture
        rename = h.os.rename
        def interrupted(source, target):
            if Path(source) == self.run:
                raise OSError("synthetic interrupted commit")
            return rename(source, target)
        with patch.object(h.os, "rename", side_effect=interrupted), self.assertRaises(OSError):
            self.retire()
        with self.assertRaises(h.Stop):
            h.retirement_guard(f.root, f.old)
        intent = self.directory / "intent.json"
        h.publish(self.directory / "recovery-authorization.json", h.canonical(dict(intent_sha256=h.digest(h.private_read(intent)),
                   operator_session_id="operator", directive_ref="D-101")))
        self.retire()
        self.assertEqual(len(h.retirement_guard(f.root, f.old)), 2)
        h.mkdir(f.root / ".retirement/00000000-0000-0000-0000-000000000000")
        with self.assertRaises(h.Stop):
            h.retirement_guard(f.root, f.old)
