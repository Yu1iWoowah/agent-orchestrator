"""RH-FIX2 boundary proofs; all repositories and actors are synthetic."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

from test_handoff import h, FlowFixture
from producer_transport import activity
import copy
import os
from unittest.mock import patch


class ProducerBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.root.chmod(0o700)

    def git(self, path, *args):
        return subprocess.check_output(["git", "-c", "core.hooksPath=/dev/null", "-C", str(path), *args], stderr=subprocess.PIPE).decode().strip()

    def repo(self):
        repo = self.root / "repo"
        repo.mkdir(mode=0o700)
        self.git(repo, "init", "--template=")
        (repo / "file").write_text("preserved\n")
        self.git(repo, "add", "file")
        self.git(repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
        return repo

    def test_uninitialized_real_gitlink_has_explicit_snapshot_facts(self):
        repo = self.repo()
        oid = self.git(repo, "rev-parse", "HEAD")
        self.git(repo, "update-index", "--add", "--cacheinfo", "160000," + oid + ",module")
        (repo / "module").mkdir(mode=0o700)
        result = h.git_snapshot(repo, self.root / "snapshot")
        snap = json.loads(Path(result["path"]).read_bytes())
        self.assertEqual(snap["schema"], "rh-workspace-snapshot/2")
        self.assertEqual(snap["gitlinks"]["module"]["state"], "uninitialized")
        self.assertEqual(snap["gitlinks"]["module"]["index_oid"], oid)

    def test_blocked_v2_can_represent_unavailable_snapshots(self):
        stop = h.worker_template("run", "worker")
        stop.update(schema="rh-worker-stop/2", state="blocked", block_reason="Git evidence unavailable")
        stop["workspace"].update(path=str(self.root), before_snapshot=None, after_snapshot=None)
        h.validate_stop(stop, "run", "worker")
        stop["state"] = "safe"
        with self.assertRaises(h.Stop):
            h.validate_stop(stop, "run", "worker")

    def test_workspace_snapshot_does_not_copy_auth_material(self):
        repo = self.repo()
        (repo / 'auth.json').write_text('synthetic secret')
        with self.assertRaisesRegex(h.Stop, 'sensitive workspace'):
            h.git_snapshot(repo, self.root / 'snapshot')

    def initialized_link(self, problem=None):
        repo = self.repo()
        module = repo / "module"
        module.mkdir()
        self.git(module, "init", "--template=")
        (module / "child").write_text("child content")
        (module / ".gitignore").write_text("ignored\n")
        self.git(module, "add", ".")
        self.git(module, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "child")
        oid = self.git(module, "rev-parse", "HEAD")
        self.git(repo, "update-index", "--add", "--cacheinfo", "160000," + oid + ",module")
        if problem == "dirty":
            (module / "child").write_text("dirty")
        elif problem == "untracked":
            (module / "untracked").write_text("necessary")
        elif problem == "ignored":
            (module / "ignored").write_text("necessary ignored")
        elif problem == "nested":
            self.git(module, "update-index", "--add", "--cacheinfo", "160000," + oid + ",nested")
        elif problem == "shallow":
            (module / ".git/shallow").write_text(oid + "\n")
        elif problem == "alternates":
            (module / ".git/objects/info/alternates").write_text(str(repo / ".git/objects") + "\n")
        return repo, module, oid

    def test_initialized_worker_gitlink_is_real_and_clean(self):
        repo, module, oid = self.initialized_link()
        result = h.git_snapshot(repo, self.root / "snapshot")
        snap = json.loads(Path(result["path"]).read_bytes())
        self.assertEqual(snap["gitlinks"]["module"]["state"], "initialized-clean")
        self.assertEqual(snap["gitlinks"]["module"]["head"], oid)
        self.assertEqual(h.git_snapshot(repo, self.root / "again")["sha256"], result["sha256"])

    def test_unsupported_gitlink_states_do_not_become_missing(self):
        for problem in ["dirty", "untracked", "ignored", "nested", "shallow", "alternates"]:
            with self.subTest(problem=problem), tempfile.TemporaryDirectory(dir=self.root) as tmp:
                original = self.root
                self.root = Path(tmp)
                repo, module, oid = self.initialized_link(problem)
                with self.assertRaises(h.Stop):
                    h.git_snapshot(repo, self.root / "snapshot")
                self.root = original

    def test_gitlink_nonempty_fake_directory_and_symlink_block(self):
        repo = self.repo()
        oid = self.git(repo, "rev-parse", "HEAD")
        self.git(repo, "update-index", "--add", "--cacheinfo", "160000," + oid + ",module")
        module = repo / "module"
        module.mkdir()
        (module / "unique").write_text("not a child repo")
        with self.assertRaises(h.Stop):
            h.git_snapshot(repo, self.root / "snapshot")
        (module / "unique").unlink()
        module.rmdir()
        module.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(h.Stop):
            h.git_snapshot(repo, self.root / "other")

    def test_absent_gitlink_is_explicitly_uninitialized(self):
        repo = self.repo()
        oid = self.git(repo, 'rev-parse', 'HEAD')
        self.git(repo, 'update-index', '--add', '--cacheinfo', '160000,' + oid + ',module')
        result = h.git_snapshot(repo, self.root / 'snapshot')
        self.assertEqual(h.decode(h.hash_reference(result))['gitlinks']['module']['presence'], 'absent')

    def test_gitlink_head_mismatch_and_conflicted_stage_stop(self):
        repo, child, oid = self.initialized_link()
        parent = self.git(repo, 'rev-parse', 'HEAD')
        self.git(repo, 'update-index', '--cacheinfo', '160000,' + parent + ',module')
        with self.assertRaisesRegex(h.Stop, 'HEAD/index mismatch'):
            h.git_snapshot(repo, self.root / 'mismatch')
        subprocess.run(['git', '-C', str(repo), 'update-index', '--index-info'], input=('0 ' + '0'*40 + '\tmodule\n160000 ' + oid + ' 1\tmodule\n').encode(), check=True, capture_output=True)
        with self.assertRaises(h.Stop):
            h.git_snapshot(repo, self.root / 'conflict')

    def test_old_gitlink_objects_in_reclaim_scope_stop(self):
        repo, child, oid = self.initialized_link()
        with self.assertRaisesRegex(h.Stop, 'overlap old reclaim scope'):
            h.gitlink_facts(repo, 'old', repo / '.git', repo / '.git/worktrees/old')


class WorkerProducerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.root.chmod(0o700)

    def prepared(self):
        fake = FlowFixture(self.root, "claude-code", "codex", worker=True)
        rules = fake.run.request["workers"][0]
        rules.update(reserved_stop_d="D-7", hold_state="held", pending_question_refs=[],
                     stop_d_source_refs=["human"], hold_source_refs=["human"], task_owner_id=None, task_refs=[],
                     release=dict(mode="keep-held", authority_refs=["human"], reserved_resume_d=None, resume_d_source_refs=[]))
        fake.run.root.joinpath("start.json").write_bytes(h.canonical(fake.run.request))
        fake.run.state["start_sha256"] = h.digest(h.canonical(fake.run.request))
        fake.run.workspace("worker")
        worker = fake.snapshots["worker"]
        worker["session"]["activity"]["state"] = "active"
        worker["transcript"]["turns"][-1].update(state="running")
        worker["transcript"]["turns"][-1].pop("completedAt", None)
        artifact = fake.data_dir / "artifacts/worker/necessary.txt"
        h.publish(artifact, b"original necessary material")
        fake.worker_statement = dict(schema="rh-worker-statement/1", run_id=fake.run_id, worker_id="worker", requested_state="safe", block_reason=None,
                                     preservation_sources=[dict(path=str(artifact), kind="original-task", basis="necessary input")], in_flight=[], pending_question_refs=[])
        self.assertFalse(fake.run.worker_stop({"id": "worker"}))
        return fake, artifact

    def test_real_worker_helper_copies_material_and_runner_verifies_same_snapshot(self):
        fake, artifact = self.prepared()
        receipt = fake.dispatch_actor(2)
        self.assertEqual(receipt["schema"], "rh-worker-stop/2")
        self.assertEqual(receipt["state"], "safe")
        readers = [json.loads(line) for line in fake.transport.root.joinpath("readers.jsonl").read_text().splitlines()]
        self.assertTrue(readers)
        self.assertTrue(all(row["actor"] == "worker" for row in readers if row["argv"][2] == "worker"))
        saved = Path(receipt["preservation"][0]["path"])
        self.assertEqual(saved.read_bytes(), artifact.read_bytes())
        self.assertEqual(artifact.read_bytes(), b"original necessary material")
        self.assertTrue(saved.is_relative_to(fake.run.root / "workers/worker/saved"))
        proof = json.loads(h.hash_reference(receipt["producer_observation"]))
        self.assertTrue(any(x.get("entry_id") for x in proof["excluded"]))
        self.assertEqual(proof["retained"], [])
        self.assertEqual(fake.run.worker_stop({"id": "worker"})["state"], "safe")
        (fake.worker_path / "tracked").write_text("later drift")
        with self.assertRaises(h.Stop):
            fake.run.worker_stop({"id": "worker"})

    def test_worker_safe_cannot_exclude_another_turn(self):
        fake, artifact = self.prepared()
        fake.snapshots["worker"]["transcript"]["turns"].append(dict(id="queued", state="queued"))
        # The executing evidence turn remains the unique running turn; a queued turn is separate work.
        current = fake.snapshots["worker"]["transcript"]["turns"].pop(0)
        fake.snapshots["worker"]["transcript"]["turns"].append(current)
        receipt = fake.dispatch_actor(2)
        self.assertEqual(receipt["state"], "blocked")
        self.assertFalse(fake.run.worker_stop({"id": "worker"}))

    def test_worker_source_escape_and_sensitive_material_stop(self):
        fake, artifact = self.prepared()
        fake.worker_statement["preservation_sources"][0]["path"] = str(self.root / "outside")
        self.root.joinpath("outside").write_text("outside actor roots")
        with self.assertRaisesRegex(h.Stop, "outside actor roots"):
            fake.dispatch_actor(2)
        self.assertFalse((fake.run.root / "workers/worker/stop.json").exists())

    def test_helper_command_parser_rejects_shell_expansion_and_additional_writers(self):
        fake, artifact = self.prepared()
        command = h.helper_command(fake.run.root, "worker-receipt", "worker")
        self.assertEqual(h.helper_identity(command, fake.run.root, "worker"), ("worker-receipt", {}))
        for extra in ["; touch other", " &", " $(pwd)", " `pwd`", " --extra", "\ntrue"]:
            self.assertIsNone(h.helper_identity(command + extra, fake.run.root, "worker"))

    def test_worker_receipt_published_but_turn_running_waits(self):
        fake, _ = self.prepared()
        fake.dispatch_actor(2)
        final = copy.deepcopy(fake.snapshots["worker"])
        fake.snapshots["worker"] = copy.deepcopy(fake.helper_events[-1]["snapshot"])
        self.assertFalse(fake.run.worker_stop({"id": "worker"}))
        fake.snapshots["worker"] = final
        self.assertEqual(fake.run.worker_stop({"id": "worker"})["state"], "safe")

    def test_blocked_reference_is_confined_before_reading(self):
        stop = h.worker_template("run", "worker")
        stop.update(state="blocked", block_reason="missing evidence")
        stop["settled"]["in_flight"] = [dict(reference="job", state="running", evidence_path=str(self.root / "secret"), sha256="0"*64)]
        directory = h.mkdir(self.root / "workers/worker")
        with patch.object(h, "hash_reference", side_effect=AssertionError("read before confinement")):
            with self.assertRaisesRegex(h.Stop, "outside fixed directory"):
                h.validate_stop(stop, "run", "worker", directory)

    def test_other_same_turn_tool_is_not_self_exclusion(self):
        fake, _ = self.prepared()
        fake.snapshots["worker"] = activity(fake.snapshots["worker"], 'external-command', '', None, 'worker', 'turn', 'other-tool', 8)
        receipt = fake.dispatch_actor(2)
        self.assertEqual(receipt["state"], "blocked")
        proof = h.decode(h.hash_reference(receipt["producer_observation"]))
        self.assertTrue(proof["retained"])

    def test_different_worker_schema_and_forged_hash_are_rejected(self):
        fake, _ = self.prepared()
        receipt = fake.dispatch_actor(2)
        for change in ('schema', 'run_id', 'sha256', 'null'):
            bad = copy.deepcopy(receipt)
            if change == 'schema': bad['schema'] = 'rh-worker-stop/1'
            elif change == 'run_id': bad['run_id'] = 'another-run'
            elif change == 'sha256': bad['preservation'][0]['sha256'] = '0'*64
            else: bad['workspace']['before_snapshot'] = None
            with self.subTest(change=change), self.assertRaises(h.Stop):
                h.validate_stop(bad, fake.run_id, 'worker', fake.run.root / 'workers/worker')

    def test_sensitive_or_symlink_source_does_not_publish_receipt(self):
        fake, artifact = self.prepared()
        secret = artifact.parent / 'auth.json'
        h.publish(secret, b'synthetic secret')
        fake.worker_statement['preservation_sources'][0]['path'] = str(secret)
        with self.assertRaisesRegex(h.Stop, 'sensitive'):
            fake.dispatch_actor(2)
        self.assertFalse((fake.run.root / 'workers/worker/stop.json').exists())


    def test_worker_D_mismatch_stops_before_receipt(self):
        fake, _ = self.prepared()
        fake.run.request['workers'][0]['reserved_stop_d'] = 'D-8'
        fake.run.root.joinpath('start.json').write_bytes(h.canonical(fake.run.request))
        fake.run.state['start_sha256'] = h.digest(h.canonical(fake.run.request))
        with self.assertRaisesRegex(h.Stop, 'D/rules mismatch'):
            fake.dispatch_actor(2)
        self.assertFalse((fake.run.root / 'workers/worker/stop.json').exists())

    def test_worker_wrong_run_statement_stops_before_receipt(self):
        fake, _ = self.prepared()
        with self.assertRaisesRegex(h.Stop, 'worker producer not in bound rules'):
            fake.transport.execute('old', 'worker-receipt')
        fake.worker_statement['run_id'] = 'another-run'
        with self.assertRaisesRegex(h.Stop, 'statement identity'):
            fake.dispatch_actor(2)

    def test_duplicate_running_helper_cannot_be_self_excluded(self):
        fake, _ = self.prepared()
        command = h.helper_command(fake.run.root, 'worker-receipt', 'worker')
        fake.snapshots['worker'] = activity(fake.snapshots['worker'], command, '', None, 'worker', 'turn', 'duplicate-helper', 9)
        with self.assertRaisesRegex(h.Stop, 'self activity missing/ambiguous'):
            fake.dispatch_actor(2)

    def test_symlink_source_cannot_be_copied(self):
        fake, artifact = self.prepared()
        link = artifact.parent / 'link'
        link.symlink_to(artifact)
        fake.worker_statement['preservation_sources'][0]['path'] = str(link)
        with self.assertRaisesRegex(h.Stop, 'symlink'):
            fake.dispatch_actor(2)


class ProducerChainTests(unittest.TestCase):
    def test_C01_real_bootstrap_stdout_crosses_actual_truncation_boundary(self):
        with tempfile.TemporaryDirectory() as temp:
            fake = FlowFixture(Path(temp), "codex", "claude-code", worker=True)
            fake.run.request["workers"][0]["pending_question_refs"] = ["Q-" + "x"*70000]
            fake.run.request["workers"][0]["release"] = dict(mode="keep-held")
            fake.run.root.joinpath("start.json").write_bytes(h.canonical(fake.run.request))
            fake.run.state["start_sha256"] = h.digest(h.canonical(fake.run.request))
            elapsed = [0]
            fake.run.clock = lambda: elapsed[0]
            def advance(seconds):
                if fake.pending_actor is None and fake.run.state.get("stage") == "bootstrap":
                    elapsed[0] += 600
                else:
                    fake.dispatch_actor(seconds)
            fake.run.sleep = advance
            with self.assertRaisesRegex(h.Stop, "wait deadline reached: bootstrap"):
                h.run_protocol(fake.run)
            event = next(x for x in fake.helper_events if x["action"] == "bootstrap")
            self.assertEqual(h.private_read(event["capture_path"] / "stdout").decode(), event["stdout"])
            self.assertEqual(h.decode(h.private_read(event["capture_path"] / "invocation.json"))["argv"], event["argv"])
            self.assertGreater(len(event["stdout"]), 65536)
            entry = next(x for x in event["snapshot"]["transcript"]["entries"] if x.get("id") == "bootstrap-tool")
            self.assertTrue(entry["detail"]["outputTruncated"])
            self.assertLessEqual(len(entry["detail"]["output"]), 65536)
            self.assertFalse(h.quiescent(fake.snapshots["new"], run=fake.run))
            self.assertNotIn("exit-agent", fake.mutations)

class GitChainTests(unittest.TestCase):
    def git(self, path, *args):
        return subprocess.check_output(['git', '-c', 'core.hooksPath=/dev/null', '-C', str(path), *args], stderr=subprocess.PIPE).decode().strip()

    def setup_link(self, fake, state):
        child = fake.workspace / 'module'
        common = fake.repo / '.git/modules/fixture'
        oid = self.git(fake.workspace, 'rev-parse', 'HEAD')
        if state != 'uninitialized':
            child.mkdir()
            common.parent.mkdir(parents=True)
            self.git(child, 'init', '--template=', '--separate-git-dir=' + str(common))
            (child / 'child').write_text('necessary child content')
            self.git(child, 'add', '.')
            self.git(child, '-c', 'user.name=fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '-m', 'child')
            oid = self.git(child, 'rev-parse', 'HEAD')
        self.git(fake.workspace, 'update-index', '--add', '--cacheinfo', '160000,' + oid + ',module')
        tree = self.git(fake.workspace, 'write-tree')
        parent = self.git(fake.workspace, 'rev-parse', 'HEAD')
        commit = self.git(fake.workspace, '-c', 'user.name=fixture', '-c', 'user.email=fixture@example.invalid', 'commit-tree', tree, '-p', parent, '-m', 'gitlink')
        self.git(fake.workspace, 'update-ref', 'refs/heads/original', commit)
        if state == 'dirty':
            (child / 'child').write_text('dirty child')
        if state == 'unprotected':
            self.git(child, 'checkout', '--detach')
            for ref in self.git(child, 'for-each-ref', '--format=%(refname)', 'refs/heads').splitlines():
                self.git(child, 'update-ref', '-d', ref)
        return common

    def test_old_real_gitlink_chain_preserves_objects_after_clean_reclaim(self):
        for state in ('uninitialized', 'clean'):
            with self.subTest(state=state), tempfile.TemporaryDirectory() as temp:
                fake = FlowFixture(Path(temp), 'claude-code', 'codex')
                common = self.setup_link(fake, state)
                h.run_protocol(fake.run)
                self.assertEqual(fake.run.state['outcome'], 'complete')
                self.assertEqual(fake.workspace.exists(), state == 'uninitialized')
                facts = h.decode(h.private_read(fake.run.root / 'old/preservation-evidence.json'))['gitlinks']
                self.assertEqual(facts['module']['state'], 'uninitialized' if state == 'uninitialized' else 'initialized-clean')
                h.verify_gitlink_protection(facts)
                if state == 'clean':
                    self.assertTrue(common.exists())

    def test_old_dirty_or_unprotected_gitlink_stops_before_kill(self):
        for state in ('dirty', 'unprotected'):
            with self.subTest(state=state), tempfile.TemporaryDirectory() as temp:
                fake = FlowFixture(Path(temp), 'codex', 'claude-code')
                self.setup_link(fake, state)
                with self.assertRaises(h.Stop):
                    h.run_protocol(fake.run)
                self.assertNotIn('kill', fake.mutations)
                self.assertTrue(fake.workspace.exists())

class OldProducerTests(unittest.TestCase):
    def prepare(self, directory):
        fake = FlowFixture(directory, 'codex', 'claude-code')
        fake.run.state['stage'] = 'workers'
        self.assertTrue(fake.run.workers())
        fake.run.state['stage'] = '2a'
        return fake

    def test_old_helper_success_reentry_reads_original_and_changed_statement_stops(self):
        with tempfile.TemporaryDirectory() as temp:
            fake = self.prepare(Path(temp))
            receipt = h.phase_2a(fake.run)
            before = h.private_read(fake.run.root / 'old/phase-2a.json')
            self.assertEqual(fake.transport.execute('old', 'old-receipt'), receipt)
            self.assertEqual(h.private_read(fake.run.root / 'old/phase-2a.json'), before)
            path = fake.run.root / 'old/draft/statement.json'
            statement = h.decode(h.private_read(path))
            statement['classification'][0]['basis'] = 'changed semantic decision'
            path.write_bytes(h.canonical(statement))
            with self.assertRaises(h.Stop):
                fake.transport.execute('old', 'old-receipt')
            self.assertEqual(h.private_read(fake.run.root / 'old/phase-2a.json'), before)

    def test_old_partial_output_cannot_be_repaired_by_reinvocation(self):
        with tempfile.TemporaryDirectory() as temp:
            fake = self.prepare(Path(temp))
            fake.preservation_problem = 'metadata'
            with self.assertRaises(h.Stop):
                h.phase_2a(fake.run)
            self.assertTrue((fake.run.root / 'old/producer-intent.json').exists())
            self.assertTrue((fake.run.root / 'old/preservation.json').exists())
            self.assertFalse((fake.run.root / 'old/phase-2a.json').exists())
            with self.assertRaises(h.Stop):
                fake.transport.execute('old', 'old-receipt')
            self.assertFalse((fake.run.root / 'old/phase-2a.json').exists())


class ToolResultTests(unittest.TestCase):
    def test_real_producer_async_and_ordinary_json_are_distinct(self):
        from test_handoff import snapshot
        for text, expected in [(json.dumps(dict(lease=dict(session_id="old"), prepared_by=dict(session_id="old"))), True),
                               ('Script running with session ID job42', False),
                               (json.dumps(dict(type="job", reference="job42", state="running")), False),
                               (json.dumps(dict(type="job", reference="job42", state="UNKNOWN")), False),
                               (json.dumps(dict(type="job", state="completed")), False)]:
            with self.subTest(text=text):
                snap = activity(snapshot(), 'external-tool', text, 0, 'old', 'turn', 'tool', 5)
                self.assertEqual(h.quiescent(snap), expected)
        with tempfile.TemporaryDirectory() as temp:
            fake = FlowFixture(Path(temp), 'claude-code', 'codex')
            h.run_protocol(fake.run)
            command = h.helper_command(fake.run.root, 'inspect', 'new', path='start.json', offset=0)
            result = fake.transport.execute('new', 'inspect', command)
            self.assertIn('session_id', result['text'])
            entry = next(e for e in fake.snapshots['new']['transcript']['entries'] if e.get('id') == 'inspect-tool')
            self.assertTrue(h.known_helper(fake.run, 'new', entry))
            self.assertTrue(h.quiescent(fake.snapshots['new'], run=fake.run))


class RetirementTests(unittest.TestCase):
    def setUp(self):
        from test_handoff import snapshot
        from types import SimpleNamespace
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.data = h.mkdir(Path(self.temp.name) / 'data')
        self.root = h.mkdir(self.data / 'projects' / h.PROJECT / 'orchestrator/.private-handoff-runs')
        self.run = h.mkdir(self.root / h.RETIRABLE_RUN)
        self.old = 'historical-old'
        self.slot = h.mkdir(self.root / '.start' / self.old)
        self.directory = h.mkdir(self.root / '.retirement' / h.RETIRABLE_RUN)
        self.snaps = {}
        self.old_snapshot = snapshot()
        self.old_snapshot['session'].update(id=self.old, kind='orchestrator')
        self.snaps[self.old] = self.old_snapshot
        script, cli_bytes = b'fixed historical script\n', Path('/usr/bin/true').read_bytes()
        h.publish(self.run / 'handoff.py', script)
        h.publish(self.run / 'runner.lock', b'')
        child = subprocess.Popen(['python3', '-I', '-B', '-c', 'pass'], start_new_session=True)
        child.wait(timeout=10)
        h.publish(self.run / 'runner.json', h.canonical(dict(pid=child.pid, sid=child.pid, launched_at=h.now())))
        h.publish(self.run / 'runner-intent.json', h.canonical(dict(started_at=h.now())))
        workers = [dict(worker_id=sid, reserved_stop_d='D-3', release=dict(mode='keep-held')) for sid in h.RETIRABLE_WORKERS]
        request = dict(schema='rh-start-request/1', request_id=h.RETIRABLE_RUN, old_id=self.old, project_id=h.PROJECT,
                       target=dict(harness='codex'), workers=workers, sources=[], workspace_index=[])
        request_raw = h.canonical(request)
        h.publish(self.run / 'start.json', request_raw)
        h.publish(self.slot / 'request.json', request_raw)
        effects = {'worker-stop-' + sid: 'accepted' for sid in h.RETIRABLE_WORKERS}
        state = dict(schema='rh-state/1', run_id=h.RETIRABLE_RUN, old_id=self.old, project_id=h.PROJECT,
                     stage='workers', outcome='stopped', completed_stages=[], target=request['target'], effects=effects,
                     start_sha256=h.digest(request_raw), script_sha256=h.digest(script), cli_sha256=h.digest(cli_bytes),
                     cli_path=str(self.root / '.qualified/ao'), data_dir=str(self.data), run_file=str(self.data / 'running.json'),
                     waits=dict(workers=dict(boot_id=h.boot_id())))
        h.publish(self.run / 'state.json', h.canonical(state))
        h.publish(self.root / '.qualified/ao', cli_bytes)
        h.publish(self.root / '.qualified/qualification.json', h.canonical(dict(schema='rh-cli-qualification/1', cli_sha256=state['cli_sha256'], script_sha256=state['script_sha256'])))
        for rules in workers:
            sid = rules['worker_id']
            control = h.mkdir(self.run / 'control' / ('worker-stop-' + sid))
            payload = dict(run_id=h.RETIRABLE_RUN, old_id=self.old, actor='detached-runner', recipient=sid, phase='worker-stop', instruction=dict(directive='D-3', rules=rules))
            raw = h.canonical(payload)
            handle = h.RETIRABLE_RUN + ':worker-stop:' + sid
            h.publish(control / 'payload.json', raw)
            h.publish(control / 'intent.json', h.canonical(dict(argv=['send', '--session', sid, '--steer', '--client-message-id', handle, '--message', raw.decode()])))
            h.publish(control / 'sent.json', h.canonical(dict(at=h.now())))
            h.publish(control / 'result.json', h.canonical(dict(stdout='accepted', sha256=h.digest(b'accepted'))))
            snap = snapshot()
            snap['session'].update(id=sid, kind='worker')
            snap['transcript']['entries'].append(dict(id='stop-'+sid, kind='message', role='user', origin='automation', senderSessionId=self.old,
                 senderProjectId=h.PROJECT, clientMessageId=handle, turnId='turn', sequence=2, text='[from '+self.old+'] '+raw.decode()))
            self.snaps[sid] = snap
            stop = dict(schema='rh-worker-stop/1', run_id=h.RETIRABLE_RUN, project_id=h.PROJECT, worker_id=sid, state='blocked',
                        resume_state='held', evidence_turn_id='turn', preservation=[], stop_directive=dict(last_ack_d='D-1'))
            h.publish(self.run / 'workers' / sid / 'stop.json', h.canonical(stop))
        self.authorization = dict(schema='rh-retirement-authorization/1', run_id=h.RETIRABLE_RUN, old_id=self.old, operator_session_id='operator',
                                  decision_ref='synthetic-authorization', directive_ref='D-100', start_sha256=state['start_sha256'], expected_effects=list(effects), materials=[])
        h.publish(self.directory / 'authorization.json', h.canonical(self.authorization))
        self.initial = SimpleNamespace(data_dir=self.data, run_file=Path(state['run_file']))
        self.cli = SimpleNamespace(sha256=state['cli_sha256'], data_dir=self.data, session=lambda sid:copy.deepcopy(self.snaps[sid]),
            documents=lambda:dict(lease=dict(sessionId=self.old, frozen=False)), sessions=lambda:[dict(id=sid, projectId=h.PROJECT,
                 role='orchestrator' if sid==self.old else 'worker', isTerminated=False) for sid in self.snaps])

    def retire(self):
        with patch.dict(os.environ, {'AO_SESSION_ID':'operator'}), patch.object(h, 'CLI', return_value=self.cli):
            return h.retire_run(self.root, h.RETIRABLE_RUN, self.initial)

    def test_retire_preserves_original_bytes_holds_and_forbids_old_UUID(self):
        before = h.private_manifest(self.run)
        receipt = self.retire()
        self.assertEqual(receipt['hold'], 'unchanged')
        self.assertEqual(len(receipt['closed_effects']), 2)
        self.assertEqual(receipt['closed_effects'][1]['last_ack_d'], 'D-1')
        archive = self.root / '.retired' / h.RETIRABLE_RUN
        self.assertEqual([x for x in h.private_manifest(archive) if not x['path'].startswith('retired-start-slot')], before)
        self.assertEqual(h.retirement_guard(self.root, self.old)[0]['old_id'], self.old)
        self.assertEqual(self.retire(), receipt)
        self.assertEqual(list(self.slot.iterdir()), [])
        with self.assertRaises(h.Stop):
            h.load_run(self.root, h.RETIRABLE_RUN, self.initial)

    def test_retirement_nonterminal_or_unknown_effect_never_moves(self):
        self.snaps[h.RETIRABLE_WORKERS[0]]['transcript']['turns'][0]['state'] = 'running'
        with self.assertRaises(h.Stop):
            self.retire()
        self.assertTrue(self.run.exists())
        self.snaps[h.RETIRABLE_WORKERS[0]]['transcript']['turns'][0]['state'] = 'completed'
        state = h.decode(h.private_read(self.run / 'state.json'))
        state['effects']['spawn'] = 'UNKNOWN'
        (self.run / 'state.json').write_bytes(h.canonical(state))
        with self.assertRaises(h.Stop):
            self.retire()
        self.assertTrue(self.run.exists())

    def test_retirement_crash_before_run_rename_requires_named_recovery(self):
        self.crash_recovery('before')

    def test_retirement_crash_after_slot_rename_requires_named_recovery(self):
        self.crash_recovery('slot')

    def test_retirement_crash_after_run_rename_requires_named_recovery(self):
        self.crash_recovery('run')

    def crash_recovery(self, where):
        rename = h.os.rename
        def crash(source, destination):
            if where == 'before' and Path(source) == self.run:
                raise OSError('synthetic crash before rename')
            rename(source, destination)
            if (where == 'run' and Path(source) == self.run) or (where == 'slot' and Path(source) == self.slot):
                raise OSError('synthetic crash after rename')
        with patch.object(h.os, 'rename', side_effect=crash), self.assertRaises(OSError):
            self.retire()
        with self.assertRaisesRegex(h.Stop, 'pending retirement'):
            h.retirement_guard(self.root, self.old)
        with self.assertRaises((h.Stop, FileNotFoundError)):
            self.retire()
        intent = h.digest(h.private_read(self.directory / 'intent.json'))
        h.publish(self.directory / 'recovery-authorization.json', h.canonical(dict(intent_sha256=intent, operator_session_id='operator', directive_ref='D-101')))
        self.assertEqual(self.retire()['hold'], 'unchanged')
        self.assertTrue(h.retirement_guard(self.root, self.old))

    def test_retirement_alive_runner_and_wrong_slot_are_rejected(self):
        runner = self.run / 'runner.json'
        original = h.private_read(runner)
        runner.write_bytes(h.canonical(dict(pid=os.getpid(), sid=os.getpid(), launched_at=h.now())))
        with self.assertRaisesRegex(h.Stop, 'PID alive'):
            self.retire()
        runner.write_bytes(original)
        self.slot.joinpath('request.json').write_bytes(b'{}')
        with self.assertRaisesRegex(h.Stop, 'slot not the original'):
            self.retire()
        self.assertTrue(self.run.exists())
        with self.assertRaises(h.Stop):
            h.retire_run(self.root, '00000000-0000-4000-8000-000000000001', self.initial)
