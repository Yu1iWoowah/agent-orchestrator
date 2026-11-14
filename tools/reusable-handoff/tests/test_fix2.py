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

    def test_clean_porcelain_unfinished_git_operation_is_blocked_and_fingerprinted(self):
        repo, module, oid = self.initialized_link()
        self.git(module, "checkout", "-b", "side")
        self.git(module, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "side")
        self.git(module, "checkout", "--detach", oid)
        self.git(module, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "merge", "--no-ff", "--no-commit", "side")
        self.assertEqual(self.git(module, "status", "--porcelain"), "")
        before = h.git_operation_facts(module)
        (module / ".git/MERGE_MSG").write_text("changed ongoing merge message")
        self.assertNotEqual(before, h.git_operation_facts(module))
        for role in ("worker", "old"):
            with self.subTest(role=role), self.assertRaisesRegex(h.Stop, "unfinished operation"):
                h.gitlink_facts(repo, role)
        self.git(module, "merge", "--abort")
        self.assertTrue(h.gitlink_facts(repo))
        self.git(module, "bisect", "start")
        with self.assertRaisesRegex(h.Stop, "unfinished operation"):
            h.gitlink_facts(repo)

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

    def prepared(self, rebatch=False):
        fake = FlowFixture(self.root, "claude-code", "codex", worker=True)
        fake.rebatch_directive = rebatch
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
        controls = [e for e in fake.snapshots["worker"]["transcript"]["entries"] if e.get("detail", {}).get("event") == "steer"]
        self.assertEqual(len(controls), 1)
        self.assertEqual(controls[0]["activityKind"], "system")
        self.assertFalse(any(e.get("clientMessageId") == controls[0]["detail"]["clientMessageId"] for e in fake.snapshots["worker"]["transcript"]["entries"]))
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

    def test_real_steer_identity_payload_and_turn_are_not_interchangeable(self):
        fake, _ = self.prepared()
        original = copy.deepcopy(fake.snapshots['worker'])
        for field, value in [('origin', 'human'), ('senderSessionId', 'other'), ('senderProjectId', 'other'), ('text', 'altered')]:
            snap = copy.deepcopy(original)
            control = next(e for e in snap['transcript']['entries'] if e.get('detail', {}).get('event') == 'steer')
            control['detail'][field] = value
            with self.subTest(field=field), self.assertRaises(h.Stop):
                h.producer_control(fake.run, 'worker', 'worker-stop', snap)
        snap = copy.deepcopy(original)
        control = next(e for e in snap['transcript']['entries'] if e.get('detail', {}).get('event') == 'steer')
        control.pop('turnId')
        with self.assertRaisesRegex(h.Stop, 'turn/sequence'):
            h.producer_control(fake.run, 'worker', 'worker-stop', snap)

    def test_idle_fallback_requeued_directive_is_real_batch_follower(self):
        fake, _ = self.prepared(rebatch=True)
        snap = fake.snapshots['worker']
        handle = fake.run_id+':worker-stop:worker'
        message = next(e for e in snap['transcript']['entries'] if e.get('clientMessageId')==handle)
        self.assertNotEqual(message['turnId'], message['consumedByTurnId'])
        self.assertEqual(message['consumedByTurnId'], snap['transcript']['turns'][-1]['id'])
        self.assertNotIn(message['turnId'], [t['id'] for t in snap['transcript']['turns']])
        raw = h.private_read(fake.run.root/'control/worker-stop-worker/payload.json').decode()
        control = h.directive_entry(snap, handle, 'old', raw)
        self.assertEqual(control['turnId'], message['turnId'])
        self.assertEqual(control['dispatch_turn_id'], message['consumedByTurnId'])
        receipt = fake.dispatch_actor(2)
        self.assertEqual(receipt['evidence_turn_id'], message['consumedByTurnId'])
        self.assertEqual(fake.run.worker_stop({'id':'worker'})['state'], 'safe')
        self.assertEqual(next(e for e in fake.snapshots['worker']['transcript']['entries'] if e.get('clientMessageId')==handle)['id'], message['id'])

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
        fake.snapshots['worker'] = activity(fake.snapshots['worker'], command, '', None, 'worker', fake.snapshots['worker']['transcript']['turns'][-1]['id'], 'duplicate-helper', 9)
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

    def test_active_old_2a_uses_real_durable_steer(self):
        with tempfile.TemporaryDirectory() as temp:
            fake = self.prepare(Path(temp))
            fake.snapshots['old']['session']['activity']['state'] = 'active'
            receipt = h.phase_2a(fake.run)
            controls = [e for e in fake.snapshots['old']['transcript']['entries'] if e.get('detail', {}).get('event') == 'steer']
            self.assertEqual(len(controls), 1)
            self.assertEqual(receipt['evidence_turn_id'], controls[0]['turnId'])
            self.assertEqual(fake.run.state['old_control_sequence'], controls[0]['sequence'])

    def test_saved_claude_pending_input_produces_old_result_without_self_proof(self):
        # Q29 pending/revision3 structure is a direct RH consumer input here.
        # The canonical command and success are counterfactual, not live Q29.
        # No Go producer output/status/flags are rewritten after production.
        def consumer_input(snapshot, command, stdout, code, actor, turn, identifier, sequence):
            if code is not None:
                return activity(snapshot, command, stdout, code, actor, turn, identifier, sequence)
            value = copy.deepcopy(snapshot)
            value['transcript']['entries'].append(dict(id=identifier, sequence=sequence, revision=3,
                kind='activity', activityKind='command', status='pending', turnId=turn,
                detail=dict(protocol='acp', providerToolName='Bash', toolKind='execute', rawCommand=command)))
            value['transcript']['latestSequence'] = sequence
            value['transcript']['incomplete'] = [dict(type='turn', id=turn, state='running')]
            return value
        with tempfile.TemporaryDirectory() as temp:
            fake = FlowFixture(Path(temp), 'claude-code', 'codex')
            fake.run.state['stage'] = 'workers'
            self.assertTrue(fake.run.workers())
            fake.run.state['stage'] = '2a'
            with patch('producer_transport.activity', side_effect=consumer_input):
                receipt = h.phase_2a(fake.run)
            observed = h.decode(h.hash_reference(receipt['producer_observation']))
            tool = next(e for e in observed['transcript']['entries'] if e.get('id') == 'old-receipt-tool')
            self.assertEqual((tool['status'], tool['revision']), ('pending', 3))
            self.assertNotIn('self_activity', observed)
            self.assertFalse((fake.run.root / 'old/observations/producer.json').exists())
            self.assertEqual(fake.helper_events[-1]['exit'], 0)

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


# Retirement contract coverage lives in test_redesign.py; /1 receipts are read-only.
