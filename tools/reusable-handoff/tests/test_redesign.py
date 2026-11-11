"""RH-REDESIGN call-before-I/O and retirement boundary proofs."""
import importlib.util
import io
import json
import os
import sqlite3
import subprocess
import sys
import time
import tempfile
import unittest
import uuid
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("redesign_handoff", Path(__file__).parents[1] / "handoff.py")
h = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(h)


class RefreshProofStartTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.real_run = subprocess.run

    def fixture(self, name="case"):
        from test_handoff import ProtocolTests
        self.home = h.mkdir(Path(self.tmp.name) / name)
        self.data = h.mkdir(self.home / ".ao/data")
        self.request = ProtocolTests.request(SimpleNamespace(root=self.data))
        self.run_id = self.request["request_id"]
        self.root = h.mkdir(self.data / "projects" / h.PROJECT / "orchestrator/.private-handoff-runs")
        self.slot = h.mkdir(self.root / ".start/old")
        self.journal_path = self.root.parent / ".handoff-journal" / self.run_id
        self.snap = h.decode(h.private_read(self.data / "evidence/human.json"))
        self.snap["transcript"]["turns"].append(dict(id="preparing", state="completed"))
        source = h.canonical(self.snap)
        h.publish(self.slot / "evidence/human.json", source)
        for ref in self.request["sources"]:
            ref["raw_sha256"] = h.digest(source)
        # Deliberately noncanonical: proof must bind the original bytes.
        self.raw = json.dumps(self.request, indent=2).encode() + b"\n"
        h.publish(self.slot / "request.json", self.raw)
        self.proof = dict(run_id=self.run_id, start_sha256=h.digest(self.raw),
                          scope="daemon-account-catalog-global", source_refs=["human"],
                          original_authority_covers_global_refresh=True)
        self.proof_raw = json.dumps(self.proof, indent=2).encode() + b"\n"
        h.publish(self.slot / "capabilities.json", self.proof_raw)
        self.repo = h.mkdir(self.home / "repo")
        for args in (["init", "-b", "main"], ["-c", "user.name=fixture", "-c", "user.email=x@example.invalid",
                     "commit", "--allow-empty", "-m", "fixture"]):
            self.real_run(["git", "-C", str(self.repo), *args], check=True, capture_output=True)
        workspace = Path(self.request["workspace_index"][0]["path"])
        workspace.parent.mkdir(parents=True)
        self.real_run(["git", "-C", str(self.repo), "worktree", "add", "-b", "original", str(workspace)],
                      check=True, capture_output=True)
        self.cli = h.mkdir(self.home / "bin") / "ao"
        self.cli.write_bytes(b"synthetic CLI bytes")
        self.cli.chmod(0o700)
        h.publish(self.root / ".qualified/ao", self.cli.read_bytes())
        (self.root / ".qualified/ao").chmod(0o700)
        h.publish(self.root / ".qualified/qualification.json", h.canonical(dict(schema="rh-cli-qualification/2",
            call_protocol=h.CALL_PROTOCOL, cli_sha256=h.digest(self.cli.read_bytes()),
            script_sha256=h.digest(Path(h.__file__).read_bytes()))))
        self.run_file = self.home / ".ao/running.json"
        h.publish(self.run_file, b'{"port":1}')
        with sqlite3.connect(self.data / "ao.db") as db:
            db.execute("CREATE TABLE sessions(id,project_id,harness,provider_conversation_id)")
            db.execute("INSERT INTO sessions VALUES('old',?,'codex','native-old')", (h.PROJECT,))
        self.refreshes, self.source_reads = 0, 0

    def cli_transport(self, argv, **kwargs):
        if Path(argv[0]) not in (self.cli, self.root / ".qualified/ao"):
            return self.real_run(argv, **kwargs)
        args = argv[1:]
        if args[:3] == ["project", "documents", "info"]:
            result = dict(documents=dict(sharedDir=str(self.root.parent / "shared")))
        elif args[:2] == ["session", "get"]:
            self.source_reads += 1
            result = self.snap
        elif args == ["spawn", "--help"]:
            return SimpleNamespace(stdout=b"--approval-mode", stderr=b"", returncode=0)
        elif args[:2] == ["account", "ls"]:
            self.refreshes += 1
            self.assertGreater(self.source_reads, 0)
            self.assertEqual(h.private_read(self.journal_path / "capabilities.json"), self.proof_raw)
            admission = h.decode(h.private_read(self.journal_path / "admission.json"))
            self.assertEqual(admission["status"], "open")
            intents = [h.decode(h.private_read(x)) for x in self.journal_path.glob("executions/*/calls/*/intent.json")]
            refresh = next(x for x in intents if x["kind"] == "metadata-refresh")
            self.assertEqual(refresh["refresh_authority"], h.file_reference(self.journal_path / "capabilities.json"))
            self.assertEqual(refresh["target"]["scope"], "all-eligible-daemon-accounts")
            result = dict(accounts=[dict(id="acct", provider="codex", email="fixture@example.invalid", signedIn=True)])
        elif args[:2] == ["session", "ls"]:
            result = dict(data=[], meta=dict(hiddenTerminatedCount=0, hiddenOrchestratorCount=0))
        else:
            raise AssertionError(args)
        return SimpleNamespace(stdout=h.canonical(result), stderr=b"", returncode=0)

    def http_transport(self, url, **kwargs):
        path = url.split("/api/v1/", 1)[1]
        if path == "projects/" + h.PROJECT:
            result = dict(project=dict(id=h.PROJECT, path=str(self.repo)))
        elif path == "sessions/old":
            result = dict(session=self.snap["session"])
        elif path.startswith("agents/codex/models?"):
            result = dict(accountBinding=dict(accountId="acct"), models=[dict(id="model", efforts=["high"])])
        else:
            raise AssertionError(path)
        return io.BytesIO(h.canonical(result))

    def invoke(self):
        with patch.dict(os.environ, dict(HOME=str(self.home), AO_DATA_DIR=str(self.data),
                AO_RUN_FILE=str(self.run_file), PATH=str(self.cli.parent) + os.pathsep + os.environ["PATH"])), \
                patch.object(h.subprocess, "run", side_effect=self.cli_transport), \
                patch.object(h.urllib.request.OpenerDirector, "open", side_effect=self.http_transport), \
                patch.object(h, "launch") as launch, patch("sys.stdout", new_callable=io.StringIO), \
                patch("sys.stderr", new_callable=io.StringIO) as errors:
            code = h.main(["start", "--run", self.run_id, "--old", "old", "--harness", "codex",
                           "--account", "acct", "--model", "model", "--effort", "high",
                           "--approval-mode", "bypass-permissions"])
        return code, errors.getvalue(), launch.call_count

    def test_first_main_start_binds_slot_bytes_before_refresh_and_later_uses_only_J(self):
        self.fixture()
        self.assertFalse(self.journal_path.exists())
        self.assertEqual(self.invoke(), (0, "", 1))
        self.assertEqual(self.refreshes, 1)
        # Cached start does not re-import a mutable slot proof or refresh again.
        (self.slot / "capabilities.json").write_bytes(b"changed slot")
        self.assertEqual(self.invoke(), (0, "", 0))
        self.assertEqual(self.refreshes, 1)
        journal = h.Journal(self.data, self.run_id)
        execution = journal.register("_run", "old")
        cli = h.CLI(self.root / ".qualified/ao", "old", self.data, self.run_file, self.data)
        run = h.load_run(self.root, self.run_id, cli, execution)
        self.assertEqual(run.cli.execution.refresh_authority, h.file_reference(journal.root / "capabilities.json"))
        execution.close()
        facts = journal.facts()
        self.assertEqual(sum(x["kind"] == "metadata-refresh" for x in facts), 1)

    def test_missing_or_mismatched_slot_proof_never_refreshes(self):
        cases = ["missing", "run_id", "start_sha256", "scope", "empty_refs", "foreign_refs", "control_ref", "false",
                 "symlink", "public_mode"]
        for case in cases:
            with self.subTest(case=case):
                self.fixture(case)
                proof = dict(self.proof)
                if case == "missing":
                    (self.slot / "capabilities.json").unlink()
                elif case == "symlink":
                    (self.slot / "capabilities.json").rename(self.slot / "other.json")
                    (self.slot / "capabilities.json").symlink_to(self.slot / "other.json")
                elif case == "public_mode":
                    (self.slot / "capabilities.json").chmod(0o644)
                else:
                    values = dict(run_id=str(uuid.uuid4()), start_sha256=h.digest(h.canonical(self.request)),
                                  scope="target-only", empty_refs=[], foreign_refs=["other"],
                                  control_ref=["workspace"], false=False)
                    key = "source_refs" if case.endswith("refs") or case == "control_ref" else \
                          "original_authority_covers_global_refresh" if case == "false" else case
                    proof[key] = values[case]
                    (self.slot / "capabilities.json").write_bytes(h.canonical(proof))
                self.assertEqual(self.invoke()[0], 1)
                self.assertEqual(self.refreshes, 0)

    def test_live_human_identity_is_checked_before_proof_and_refresh(self):
        self.fixture()
        self.snap["transcript"]["entries"][0]["origin"] = "automation"
        self.assertEqual(self.invoke()[0], 1)
        self.assertEqual(self.refreshes, 0)
        self.assertFalse((self.journal_path / "capabilities.json").exists())

    def test_valid_slot_does_not_repair_incomplete_or_fenced_J(self):
        for case in ("missing_lock", "missing_state", "retiring", "retired"):
            with self.subTest(case=case):
                self.fixture(case)
                journal = h.Journal(self.data, self.run_id)
                if case.startswith("missing"):
                    (journal.root / ("admission.lock" if case == "missing_lock" else "admission.json")).unlink()
                else:
                    state = journal.freeze("operator", "D-100", "a" * 64)
                    if case == "retired":
                        h.replace_private(journal.root / "admission.json", dict(state, status="retired"))
                self.assertEqual(self.invoke()[0], 1)
                self.assertEqual(self.refreshes, 0)

    def test_conflicting_J_proof_is_not_overwritten(self):
        self.fixture()
        journal = h.Journal(self.data, self.run_id)
        original = h.canonical(dict(self.proof, scope="different"))
        h.publish(journal.root / "capabilities.json", original)
        self.assertEqual(self.invoke()[0], 1)
        self.assertEqual(self.refreshes, 0)
        self.assertEqual(h.private_read(journal.root / "capabilities.json"), original)

    def test_prebuilt_J_is_not_an_alternate_first_proof_input(self):
        self.fixture()
        journal = h.Journal(self.data, self.run_id)
        h.publish(journal.root / "capabilities.json", self.proof_raw)
        (self.slot / "capabilities.json").unlink()
        self.assertEqual(self.invoke()[0], 1)
        self.assertEqual(self.refreshes, 0)
        self.assertEqual(h.private_read(journal.root / "capabilities.json"), self.proof_raw)


class JournalTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.data = Path(self.tmp.name)
        self.data.chmod(0o700)
        self.run_id = str(uuid.uuid4())
        h.publish(self.data / "running.json", b'{"port":1}')

    def cli(self):
        journal = h.Journal(self.data, self.run_id)
        execution = journal.register("start", "old")
        cli = h.CLI(Path("/usr/bin/true"), "old", self.data, self.data / "running.json", self.data)
        cli.execution = execution
        execution.bindings = {"old"}
        return cli

    def test_intent_attempt_exist_before_actual_io_and_close_binds_all_bytes(self):
        cli = self.cli()
        def transport(*args, **kwargs):
            calls = list(cli.execution.directory.glob("calls/*"))
            self.assertEqual(len(calls), 1)
            self.assertTrue((calls[0] / "intent.json").is_file())
            self.assertTrue((calls[0] / "attempt.json").is_file())
            self.assertFalse((calls[0] / "result.json").exists())
            return SimpleNamespace(returncode=0, stdout=b'{"ok":true}', stderr=b"")
        with patch.object(h.subprocess, "run", side_effect=transport):
            self.assertEqual(cli.read(["session", "get", "old"]), {"ok": True})
        cli.execution.close()
        facts = cli.execution.journal.facts()
        self.assertEqual(len(facts), 1)
        self.assertEqual(facts[0]["kind"], "observation")
        self.assertEqual(facts[0]["expected_effect"], "none")

    def test_no_context_no_transport_and_write_cannot_impersonate_read(self):
        cli = h.CLI(Path("/usr/bin/true"), "old", self.data, self.data / "running.json", self.data)
        with patch.object(h.subprocess, "run") as transport:
            with self.assertRaises(h.Stop):
                cli.read(["session", "get", "old"])
            transport.assert_not_called()
        cli = self.cli()
        with patch.object(h.subprocess, "run") as transport:
            with self.assertRaises(h.Stop):
                cli.read(["kill", "old"])
            transport.assert_not_called()

    def test_retiring_rejects_guarded_helper_and_late_registration(self):
        cli = self.cli()
        journal = cli.execution.journal
        journal.freeze("operator", "D-51", "a" * 64)
        with patch.object(h.subprocess, "run") as transport:
            with self.assertRaises(h.Stop):
                cli.session("old")
            transport.assert_not_called()
        with self.assertRaises(h.Stop):
            journal.register("locate", "old")
        cli.execution.close()
        self.assertEqual(journal.state()["status"], "retiring")

    def test_timeout_has_terminal_bytes_and_unclosed_execution_blocks(self):
        cli = self.cli()
        with patch.object(h.subprocess, "run", side_effect=h.subprocess.TimeoutExpired("ao", 30, output=b"partial")):
            with self.assertRaises(h.subprocess.TimeoutExpired):
                cli.session("old")
        with self.assertRaises(h.Stop):
            cli.execution.journal.facts()
        cli.execution.close()
        facts = cli.execution.journal.facts()
        self.assertEqual(facts[0]["terminal"], "timeout")

    def test_effect_handle_binds_payload_and_recovery_target_before_io(self):
        cli = self.cli()
        cli.execution.close()
        cli.execution = cli.execution.journal.register("_run", "old")
        cli.execution.phase = "workers"
        cli.execution.bindings = {"old", "other"}
        self.addCleanup(cli.execution.close)
        handle = str(uuid.uuid4())
        args = ["send", "--session", "old", "--steer", "--client-message-id", handle, "--message", "original"]
        with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{}', stderr=b"")) as transport:
            with self.assertRaises(h.WindowElapsed):
                cli.call(args, deadline=0)
            with self.assertRaisesRegex(h.Stop, "effect handle parameters changed"):
                cli.call(args[:-1] + ["changed"])
            transport.assert_not_called()
        recover = ["send", "--session", "old", "--steer", "--recover-only", "--client-message-id", handle]
        with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{}', stderr=b"")):
            cli.call(args)
            cli.call(recover)
        with patch.object(h.subprocess, "run") as transport:
            with self.assertRaisesRegex(h.Stop, "effect handle binding changed"):
                cli.call(recover[:2] + ["other"] + recover[3:])
            transport.assert_not_called()
        fresh = args[:5] + [str(uuid.uuid4())] + args[6:]
        with self.assertRaises(h.WindowElapsed):
            cli.call(fresh, deadline=0)
        prior = next(p for p in cli.execution.directory.glob("calls/*/intent.json")
                     if h.decode(h.private_read(p))["effect_id"] == fresh[5])
        outside = self.data / "outside-call.raw"
        h.publish(outside, b"original")
        corrupted = h.decode(h.private_read(prior))
        corrupted["parameters"]["argv"][-1]["payload"] = str(outside)
        prior.write_bytes(h.canonical(corrupted))
        with patch.object(h, "hash_reference", wraps=h.hash_reference) as reader, \
                patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{}', stderr=b"")) as transport:
            with self.assertRaises(h.Stop):
                cli.call(fresh)
            reader.assert_not_called()
            transport.assert_not_called()

    def test_refresh_requires_global_authority_before_io(self):
        cli = self.cli()
        with patch.object(h.subprocess, "run") as transport:
            with self.assertRaisesRegex(h.Stop, "global account refresh"):
                cli.read(["account", "ls", "--provider", "codex", "--refresh"])
            transport.assert_not_called()
        cli.execution.refresh_authority = {"source_refs": ["original-human"], "scope": "daemon-account-catalog-global"}
        with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{"accounts":[]}', stderr=b"")):
            cli.read(["account", "ls", "--provider", "codex", "--refresh"])
        cli.execution.close()
        fact = cli.execution.journal.facts()[0]
        self.assertEqual(fact["kind"], "metadata-refresh")
        self.assertEqual(fact["target"]["scope"], "all-eligible-daemon-accounts")
        self.assertEqual(fact["target"]["client_filter"], "codex")

    def test_different_catalog_and_cursor_and_bound_reads_use_one_reader(self):
        cli = self.cli()
        cli.execution.bindings = {"old", "worker"}
        with patch.object(h.urllib.request.OpenerDirector, "open", side_effect=lambda *a, **k: io.BytesIO(b'{}')):
            cli.get("agents/codex/models?projectId=" + h.PROJECT)
            cli.get("agents/claude-code/models?projectId=" + h.PROJECT)
            cli.get("sessions/worker")
        with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{}', stderr=b"")):
            cli.read(["session", "get", "old", "--before", "7"])
        cli.execution.close()
        self.assertEqual(h.effect_closure(cli.execution.journal.facts()), {})

    def test_phase_actor_scope_and_unknown_GET_are_rejected_before_io(self):
        cli = self.cli()
        cli.execution.bindings = {"old"}
        with patch.object(h.urllib.request.OpenerDirector, "open") as transport:
            for path in ("sessions/other", "shutdown", "agents/other/models"):
                with self.assertRaises(h.Stop):
                    cli.get(path)
            transport.assert_not_called()
        cli.execution.phase = "invented"
        with self.assertRaises(h.Stop):
            cli.session("old")
        cli.execution.phase = "prepared"
        cli.env["AO_SESSION_ID"] = "other"
        with self.assertRaises(h.Stop):
            cli.session("old")

    def test_raw_orphan_and_result_gap_cannot_be_read_only(self):
        cli = self.cli()
        with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{}', stderr=b"")):
            cli.session("old")
        call = next(cli.execution.directory.glob("calls/*"))
        h.publish(call / "orphan.raw", b"not a call")
        cli.execution.close()
        with self.assertRaisesRegex(h.Stop, "orphan"):
            cli.execution.journal.facts()

    def test_HTTP_failure_is_captured_after_durable_intent_before_consumer_error(self):
        cli = self.cli()
        def transport(url, **kwargs):
            call = next(cli.execution.directory.glob("calls/*"))
            self.assertTrue((call / "attempt.json").exists())
            self.assertEqual(h.decode(h.private_read(call / "intent.json"))["parameters"],
                             dict(method="GET", path="projects/" + h.PROJECT))
            raise h.urllib.error.HTTPError(url, 503, "synthetic unavailable", {}, io.BytesIO(b'{"code":"synthetic"}'))
        with patch.object(h.urllib.request.OpenerDirector, "open", side_effect=transport):
            with self.assertRaises(h.urllib.error.HTTPError):
                cli.get("projects/" + h.PROJECT)
        cli.execution.close()
        fact = cli.execution.journal.facts()[0]
        self.assertEqual(fact["terminal"], "http-error")
        self.assertEqual(fact["result"]["returncode"], 503)

    def test_prefreeze_lease_can_finish_but_active_writer_blocks_fixed_set(self):
        cli = self.cli()
        journal = cli.execution.journal
        def transport(*args, **kwargs):
            self.assertEqual(journal.state()["status"], "retiring")
            with self.assertRaises(h.Stop):
                journal.facts()
            lease = next(cli.execution.directory.glob("calls/*/lease.lock"))
            with lease.open("r+b") as handle, self.assertRaises(BlockingIOError):
                h.fcntl.flock(handle, h.fcntl.LOCK_EX | h.fcntl.LOCK_NB)
            return SimpleNamespace(returncode=0, stdout=b'{}', stderr=b"")
        with patch.object(h.subprocess, "run", side_effect=transport):
            cli.call(["session", "get", "old", "--json"], read=True,
                     on_dispatch=lambda: journal.freeze("operator", "D-91", "a" * 64))
        cli.execution.close()
        self.assertEqual(len(journal.facts()), 1)
        with self.assertRaises(h.Stop):
            journal.facts(require_ended=True)
        fence = journal.state()
        maintenance = journal.register("retire", "operator", maintenance=fence["token"])
        maint_cli = h.CLI(Path("/usr/bin/true"), "operator", self.data, self.data / "running.json", self.data)
        maint_cli.execution = maintenance
        maintenance.bindings = {"old"}
        with patch.object(h.subprocess, "run") as invoke:
            with self.assertRaises(h.Stop):
                maint_cli.call(["send", "--session", "old", "--steer", "--client-message-id", "bad", "--message", "synthetic"])
            invoke.assert_not_called()
        maintenance.close()

    def test_unsealed_foreign_and_mutable_argv_cannot_reach_transport(self):
        cli = self.cli()
        execution = cli.execution
        with patch.object(h.subprocess, "run") as invoke:
            with self.assertRaises(h.Stop):
                execution.dispatch(cli, dict(kind="observation", expected_effect="none"))
            invoke.assert_not_called()
        args = ["session", "get", "old", "--json"]
        expected = list(args)
        with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{}', stderr=b"")) as invoke:
            cli.call(args, read=True, on_dispatch=lambda: args.__setitem__(0, "kill"))
            self.assertEqual(invoke.call_args.args[0][1:], expected)
        operation = execution.descriptor(cli, "cli", dict(argv=expected, timeout=30), read=True)
        with self.assertRaises(TypeError):
            operation["kind"] = "worker-control"
        other = execution.journal.register("start", "old")
        with self.assertRaises(h.Stop):
            other.dispatch(cli, operation)
        other.close()
        execution.close()

    def test_dead_unclosed_and_orphan_maintenance_block_new_admission(self):
        journal = h.Journal(self.data, self.run_id)
        code = """import importlib.util,pathlib,sys
s=importlib.util.spec_from_file_location('h',sys.argv[1]);h=importlib.util.module_from_spec(s);s.loader.exec_module(h)
h.Journal(pathlib.Path(sys.argv[2]),sys.argv[3]).register('start','old')
"""
        subprocess.run([sys.executable, "-I", "-B", "-c", code, h.__file__, str(self.data), self.run_id], check=True, capture_output=True)
        with self.assertRaisesRegex(h.Stop, "dead unclosed"):
            journal.register("start", "old")
        other = h.Journal(self.data, str(uuid.uuid4()))
        fence = other.freeze("operator", "D-91", "a" * 64)
        with patch.object(h, "replace_private", side_effect=OSError("registration publication crash")):
            with self.assertRaises(OSError):
                other.register("retire", "operator", maintenance=fence["token"])
        with self.assertRaisesRegex(h.Stop, "orphan maintenance"):
            other.register("retire", "operator", maintenance=fence["token"])

    def test_preflight_originals_survive_slot_replacement_and_prior_uuid_is_guarded(self):
        cli = self.cli()
        root = h.mkdir(self.data / "projects" / h.PROJECT / "orchestrator/.private-handoff-runs")
        slot = h.mkdir(root / ".start/old")
        request = dict(old_id="old", target={}, authority=dict(source_refs=[]), workers=[], sources=[])
        raw = h.canonical(request)
        h.publish(slot / "request.json", raw)
        qraw = h.canonical(dict(schema="rh-cli-qualification/2", call_protocol=h.CALL_PROTOCOL))
        h.publish(root / ".qualified/qualification.json", qraw)
        cli.execution.journal.bind(dict(root=str(root), old_id="old", start_sha256=h.digest(raw), qualification_sha256=h.digest(qraw)), source_root=slot)
        cli.execution.configure(request, h.digest(raw))
        with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{}', stderr=b"")):
            cli.session("old")
        cli.execution.close()
        (slot / "request.json").write_bytes(b"replacement")
        self.assertEqual(h.private_read(cli.execution.journal.root / "bound/start.json"), raw)
        self.assertEqual(len(cli.execution.journal.facts()), 1)
        new = h.Journal(self.data, str(uuid.uuid4()))
        with self.assertRaisesRegex(h.Stop, "unresolved prior journal"):
            h.prior_journal_guard(new, "old")


    def test_bootstrap_loaded_before_new_id_syncs_its_own_execution(self):
        from test_handoff import snapshot
        for mode in ("late-binding", "wrong-actor", "binding-conflict"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temporary:
                data = h.mkdir(Path(temporary) / "data")
                run_id = str(uuid.uuid4())
                root = h.mkdir(data / run_id)
                draft = h.mkdir(root / "bootstrap/new/draft")
                h.publish(data / "running.json", b'{"port":1}')
                request = dict(request_id=run_id, old_id="old", workers=[], target={}, authority=dict(source_refs=["human"]))
                h.publish(root / "start.json", h.canonical(request))
                state = dict(start_sha256=h.digest(h.canonical(request)), stage="spawn", outcome="running")
                h.publish(root / "state.json", h.canonical(state))
                cli = h.CLI(Path("/usr/bin/true"), "old", data, data / "running.json", data)
                cli.execution = h.Journal(data, run_id).register("bootstrap", "new")
                run = h.Run(root, cli)
                self.assertNotIn("new", cli.execution.bindings)
                if mode == "binding-conflict":
                    cli.execution.new_id = "unrelated"
                receipt = dict(run_id=run_id, new_id="new", evidence_turn_id="bootstrap")
                def publish_binding(seconds):
                    h.replace_private(root / "state.json", dict(state, new_id="new"))
                    h.publish(root / "bootstrap/new/receipt.json", h.canonical(receipt))
                snap = snapshot("active", turns=[dict(id="bootstrap", state="running")])
                snap["session"].update(id="new")
                with patch.dict(os.environ, AO_SESSION_ID="other" if mode == "wrong-actor" else "new", TMPDIR=str(draft)), \
                        patch.object(h.time, "sleep", side_effect=publish_binding), \
                        patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=h.canonical(snap), stderr=b"")) as transport:
                    if mode == "late-binding":
                        self.assertEqual(h.producer(run, "bootstrap"), receipt)
                        self.assertEqual(cli.execution.new_id, "new")
                        self.assertIn("new", cli.execution.bindings)
                        self.assertEqual(transport.call_count, 1)
                    else:
                        with self.assertRaises(h.Stop):
                            h.producer(run, "bootstrap")
                        transport.assert_not_called()
                cli.execution.close()


class HistoricalDatasetTests(unittest.TestCase):
    def test_every_historical_capture_has_source_and_explicit_classification(self):
        rows = h.decode((Path(__file__).parent / "fixtures/historical-captures.json").read_bytes())["captures"]
        self.assertEqual(len(rows), 206)
        self.assertEqual(sum(x["group"] == "retirement" for x in rows), 5)
        self.assertEqual(sum(x["expected_kind"] == "worker-control" for x in rows), 2)
        self.assertEqual(sum(x["expected_kind"] == "progress-notification" for x in rows), 2)
        for row in rows:
            self.assertRegex(row["raw_sha256"], "^[0-9a-f]{64}$")
            self.assertRegex(row["metadata_sha256"], "^[0-9a-f]{64}$")
            if row["cli_argv"]:
                self.assertIsNone(row["method"])
                self.assertIsNone(row["path"])
            if row["run"] == "60cd2080":
                self.assertNotIn("received_at_utc_derived", row["time"])
        # The observations are historical shapes, not native call records.
        with self.assertRaises(h.Stop):
            h.effect_closure(rows)

    def test_206_shapes_replay_through_the_same_native_constructor_executor(self):
        from test_wire_contract import wire_bytes
        rows = h.decode((Path(__file__).parent / "fixtures/historical-captures.json").read_bytes())["captures"]
        with tempfile.TemporaryDirectory() as temporary:
            data = Path(temporary)
            data.chmod(0o700)
            h.publish(data / "running.json", b'{"port":1}')
            journal = h.Journal(data, str(uuid.uuid4()))
            cli = h.CLI(Path("/usr/bin/true"), "agent-orchestrator-1", data, data / "running.json", data)
            produced = []
            for row in rows:
                kind = row["expected_kind"]
                cli.execution = execution = journal.register("_run" if kind in ("worker-control", "progress-notification") else "start", "agent-orchestrator-1")
                execution.phase = "workers" if execution.registration["role"] == "_run" else "preflight"
                execution.bindings = {"agent-orchestrator-1", "agent-orchestrator-2", "agent-orchestrator-3"}
                execution.refresh_authority = {"scope": "daemon-account-catalog-global", "source": "synthetic-approved-human"}
                if row["cli_argv"]:
                    args = row["cli_argv"]
                    name = "send" if kind == "worker-control" else "help" if kind == "local-help" else "accounts" if kind == "metadata-refresh" else "snapshot"
                    with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=row["cli_returncode"], stdout=wire_bytes(name), stderr=b"")):
                        try:
                            cli.call(args, read=kind in ("observation", "metadata-refresh"))
                        except h.Stop:
                            self.assertNotEqual(row["cli_returncode"], 0, row["capture_id"])
                else:
                    with patch.object(h.urllib.request.OpenerDirector, "open", side_effect=lambda *a, **kw: io.BytesIO(b'{}')):
                        cli.get(row["path"])
                execution.close()
                intent = h.decode(h.private_read(next(execution.directory.glob("calls/*/intent.json"))))
                self.assertEqual(intent["kind"], kind, (row["run"], row["capture_id"]))
                produced.append(intent["call_id"])
            facts = journal.facts()
            self.assertEqual(len(facts), 206)
            self.assertEqual(len(set(produced)), 206)
            self.assertEqual(sum(f["kind"] == "worker-control" for f in facts), 2)

    def test_new_generation_never_uses_review_as_repair(self):
        with self.assertRaisesRegex(h.Stop, "new generation"):
            h.historical_facts(None, None, None, {"call_protocol": h.CALL_PROTOCOL}, {}, {})

    def historical_fixture(self, temporary):
        data = h.mkdir(Path(temporary) / "data")
        run_id = str(uuid.uuid4())
        root = h.mkdir(data / "projects" / h.PROJECT / "orchestrator/.private-handoff-runs")
        current = h.mkdir(root / run_id)
        directory = h.mkdir(root / ".retirement" / run_id)
        external = h.mkdir(data / "artifacts/operator/attempt")
        h.publish(current / "handoff.py", b"# frozen pre-protocol fixture; never execute\n")
        request = dict(old_id="old", workers=[], target={}, authority=dict(source_refs=["original-human"]))
        h.publish(current / "start.json", h.canonical(request))
        executable = Path("/usr/bin/true")
        state = dict(run_id=run_id, script_sha256=h.digest(h.private_read(current / "handoff.py")),
                     cli_sha256=h.digest(executable.read_bytes()), data_dir=str(data))
        h.publish(current / "state.json", h.canonical(state))
        h.publish(directory / "qualification.original.json", h.canonical(dict(schema="rh-cli-qualification/1",
                  script_sha256=state["script_sha256"], cli_sha256=state["cli_sha256"])))
        h.publish(directory / "prior/authorization.original.json", b'{"historical":"original bytes"}')
        h.publish(directory / "prior/material-reference.json", h.canonical(h.file_reference(current / "handoff.py")))
        later = directory / "prior/later-handoff.py"
        h.publish(later, b"# frozen later retirement producer; never execute\n")
        later_sha = h.digest(h.private_read(later))
        calls = []
        scopes = {"run": current, "retirement": directory, "external-one": external}
        examples = [
            ("run", "wire/project", "http", {"path": "projects/" + h.PROJECT}, "observation", "project-observation", "none", "start", None, None),
            ("retirement", "preconditions/failed", "cli", {"argv": ["report", "--needs-input", "--note", "synthetic progress"], "returncode": 1},
             "progress-notification", "progress-report-only", "notification", "_run", run_id + ":progress-notification", 1),
            ("external-one", "attempt", "cli", {"argv": ["project", "documents", "info", "--project", h.PROJECT, "--json"], "returncode": 1},
             "observation", "project-observation", "none", "start", None, 1),
        ]
        for scope, stem, transport, metadata, kind, capability, expected, role, effect_id, code in examples:
            raw = b'{}'
            metadata = dict(metadata, sha256=h.digest(raw))
            h.publish(scopes[scope] / (stem + ".raw"), raw)
            h.publish(scopes[scope] / (stem + ".json"), h.canonical(metadata))
            producer_sha = state["script_sha256"] if scope == "run" else later_sha
            calls.append(dict(call_id=str(uuid.uuid4()), run_id=run_id, origin="retrospective-review", script_sha256=producer_sha, cli_sha256=state["cli_sha256"],
                kind=kind, capability=capability, expected_effect=expected, transport=transport, phase="preflight", actor="old",
                target=dict(project_id=h.PROJECT), effect_id=effect_id, terminal="returned", result=dict(returncode=code, stdout_sha256=h.digest(raw)),
                producer_ref=dict(script_sha256=producer_sha, location="CLI.get/call", role=role),
                source_refs=[dict(scope=scope, path=stem + ext, sha256=h.digest(h.private_read(scopes[scope] / (stem + ext))),
                                  role="capture-metadata" if ext == ".json" else "capture-output") for ext in (".json", ".raw")]))
        h.publish(current / "old-initial-response.raw", b"retained response copy")
        for fact in calls[:2]:
            fact["source_refs"].append(dict(scope="run", path="old-initial-response.raw",
                sha256=h.digest(h.private_read(current / "old-initial-response.raw")), role="supporting"))
        calls[0]["source_refs"].append(dict(scope="retirement", path="prior/material-reference.json",
            sha256=h.digest(h.private_read(directory / "prior/material-reference.json")), role="supporting"))
        inputs = dict(run=h.private_manifest(current), prior_retirement=[x for x in h.private_manifest(directory)
                      if x["path"].startswith(("prior/", "preconditions/"))],
                      external_attempts=[dict(root=str(external), scope="external-one", manifest=h.private_manifest(external), authorization_ref="approved-original-attempt")])
        coverage = []
        for scope, rows in [("run", inputs["run"]), ("retirement", inputs["prior_retirement"]), ("external-one", inputs["external_attempts"][0]["manifest"])]:
            coverage.extend(dict(scope=scope, path=x["path"], sha256=x["sha256"]) for x in rows if not x["directory"])
        review = dict(origin="retrospective-review", run_id=run_id, operator_session_id="operator", reviewed_at="synthetic-review-time",
                      decision_ref="accepted-original-limits", accepted_preintent_limitation=True,
                      input_manifest_sha256=h.digest(h.canonical(inputs)), script_sha256=state["script_sha256"], cli_sha256=state["cli_sha256"],
                      coverage=coverage, calls=calls,
                      producer_bindings=[dict(scope=scope, script_ref=dict(scope="retirement", path="prior/later-handoff.py", sha256=later_sha),
                                              cli_sha256=state["cli_sha256"]) for scope in ("retirement", "external-one")])
        h.publish(directory / "historical-review.json", h.canonical(review))
        authorization = dict(operator_session_id="operator", input_manifest_sha256=review["input_manifest_sha256"],
                             historical_review_sha256=h.digest(h.canonical(review)), evidence_roots=[str(external)])
        return SimpleNamespace(data=data, run_id=run_id, root=root, current=current, directory=directory,
                               request=request, executable=executable, state=state, inputs=inputs,
                               review=review, authorization=authorization)

    def review_facts(self, fixture):
        raw = h.canonical(fixture.review)
        (fixture.directory / "historical-review.json").write_bytes(raw)
        fixture.authorization["historical_review_sha256"] = h.digest(raw)
        return h.historical_facts(fixture.root, fixture.current, fixture.directory, fixture.state, fixture.authorization, fixture.inputs)

    def test_prior_failed_attempt_external_evidence_and_native_maintenance_coexist(self):
        with tempfile.TemporaryDirectory() as temporary:
            c = self.historical_fixture(temporary)
            with patch.object(h.subprocess, "run") as transport:
                historical = self.review_facts(c)
                transport.assert_not_called()
            self.assertEqual(len(historical), 3)
            self.assertEqual(historical[1]["result"]["returncode"], 1)
            self.assertNotEqual(historical[0]["script_sha256"], historical[1]["script_sha256"])
            self.assertEqual(historical[0]["script_sha256"], c.state["script_sha256"])
            self.assertEqual(h.effect_closure(historical), {})
            h.publish(c.data / "running.json", b'{"port":1}')
            journal = h.Journal(c.data, c.run_id)
            journal.bind(dict(root=str(c.root), old_id="old", start_sha256=h.digest(h.canonical(c.request)), qualification_sha256="a" * 64))
            fence = journal.freeze("operator", "D-99", "b" * 64)
            execution = journal.register("retire", "operator", maintenance=fence["token"])
            execution.configure(c.request)
            cli = h.CLI(c.executable, "operator", c.data, c.data / "running.json", c.data)
            cli.execution = execution
            with patch.object(h.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout=b'{"documents":{}}', stderr=b"")):
                cli.documents()
            execution.close()
            native = journal.facts()
            self.assertEqual([f["origin"] for f in historical + native], ["retrospective-review"] * 3 + ["native-intent"])
            c.review["coverage"] = c.review["coverage"][:-1]
            with self.assertRaisesRegex(h.Stop, "coverage"):
                self.review_facts(c)

    def test_complete_coverage_cannot_hide_unclassified_or_duplicate_capture(self):
        for mode in ("missing-call", "supporting", "external-missing", "external-unknown-missing", "outside-supporting", "duplicate", "cross-pair", "scope-alias"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temporary:
                c = self.historical_fixture(temporary)
                # Same producer versions isolate capture accounting from version checks.
                for fact in c.review["calls"]:
                    fact["script_sha256"] = fact["producer_ref"]["script_sha256"] = c.state["script_sha256"]
                c.review["producer_bindings"] = [dict(scope=scope, cli_sha256=c.state["cli_sha256"],
                    script_ref=dict(scope="run", path="handoff.py", sha256=c.state["script_sha256"]))
                    for scope in ("retirement", "external-one")]
                if mode in ("missing-call", "supporting"):
                    removed = c.review["calls"].pop(1)
                    if mode == "supporting":
                        for source in removed["source_refs"]:
                            source["role"] = "supporting"
                        c.review["calls"][0]["source_refs"].extend(removed["source_refs"])
                elif mode in ("external-missing", "external-unknown-missing"):
                    c.review["calls"].pop()
                    if mode == "external-unknown-missing":
                        external = Path(c.inputs["external_attempts"][0]["root"])
                        metadata = h.decode(h.private_read(external / "attempt.json"))
                        metadata["argv"] = ["unknown-control", "synthetic-target"]
                        (external / "attempt.json").write_bytes(h.canonical(metadata))
                        c.inputs["external_attempts"][0]["manifest"] = h.private_manifest(external)
                        for row in c.review["coverage"]:
                            if row["scope"] == "external-one" and row["path"] == "attempt.json":
                                row["sha256"] = h.digest(h.private_read(external / "attempt.json"))
                        c.authorization["input_manifest_sha256"] = c.review["input_manifest_sha256"] = h.digest(h.canonical(c.inputs))
                elif mode == "outside-supporting":
                    removed = c.review["calls"].pop(1)
                    for ref in removed["source_refs"]:
                        if ref["role"].startswith("capture-"):
                            old = c.directory / ref["path"]
                            renamed = "prior/relocated" + old.suffix
                            old.rename(c.directory / renamed)
                            for row in c.review["coverage"]:
                                if (row["scope"], row["path"]) == (ref["scope"], ref["path"]):
                                    row["path"] = renamed
                            ref["path"] = renamed
                        ref["role"] = "supporting"
                    c.review["calls"][0]["source_refs"].extend(removed["source_refs"])
                    c.inputs["prior_retirement"] = [row for row in h.private_manifest(c.directory)
                        if row["path"].startswith(("prior/", "preconditions/"))]
                    c.authorization["input_manifest_sha256"] = c.review["input_manifest_sha256"] = h.digest(h.canonical(c.inputs))
                elif mode == "duplicate":
                    duplicate = h.decode(h.canonical(c.review["calls"][0]))
                    duplicate["call_id"] = str(uuid.uuid4())
                    c.review["calls"].append(duplicate)
                elif mode == "cross-pair":
                    c.review["calls"][0]["source_refs"][1] = c.review["calls"][2]["source_refs"][1]
                else:
                    attempt = h.decode(h.canonical(c.inputs["external_attempts"][0]))
                    attempt["scope"] = "external-two"
                    c.inputs["external_attempts"].append(attempt)
                    c.review["coverage"].extend([dict(row, scope="external-two") for row in c.review["coverage"]
                                                if row["scope"] == "external-one"])
                    duplicate = h.decode(h.canonical(c.review["calls"][2]))
                    duplicate["call_id"] = str(uuid.uuid4())
                    for source in duplicate["source_refs"]:
                        source["scope"] = "external-two"
                    c.review["calls"].append(duplicate)
                    c.review["producer_bindings"].append(dict(c.review["producer_bindings"][-1], scope="external-two"))
                    c.authorization["input_manifest_sha256"] = c.review["input_manifest_sha256"] = h.digest(h.canonical(c.inputs))
                with self.assertRaisesRegex(h.Stop, "capture"):
                    self.review_facts(c)

    def test_raw_only_capture_crash_is_unknown_in_all_producer_scopes(self):
        for scope, metadata_path in (("run", "wire/project.json"), ("retirement", "preconditions/failed.json"),
                                     ("external-one", "attempt.json"), ("run", "workers/worker/wire/cli-crash.json")):
            for supporting in (False, True):
                with self.subTest(scope=scope, path=metadata_path, supporting=supporting), tempfile.TemporaryDirectory() as temporary:
                    c = self.historical_fixture(temporary)
                    roots = {"run": c.current, "retirement": c.directory, "external-one": Path(c.inputs["external_attempts"][0]["root"])}
                    metadata = roots[scope] / metadata_path
                    if metadata.exists():
                        metadata.unlink()
                        c.review["coverage"] = [row for row in c.review["coverage"] if (row["scope"], row["path"]) != (scope, metadata_path)]
                        c.review["calls"] = [fact for fact in c.review["calls"] if not any(
                            ref["role"] == "capture-metadata" and (ref["scope"], ref["path"]) == (scope, metadata_path)
                            for ref in fact["source_refs"])]
                    else:
                        # CLI.capture's first durable publish, before its sidecar.
                        raw = metadata.with_suffix(".raw")
                        h.publish(raw, b"partial response, effect UNKNOWN")
                        c.review["coverage"].append(dict(scope=scope, path=str(Path(metadata_path).with_suffix(".raw")),
                                                         sha256=h.digest(h.private_read(raw))))
                    if supporting:
                        raw_path = str(Path(metadata_path).with_suffix(".raw"))
                        c.review["calls"][0]["source_refs"].append(dict(scope=scope, path=raw_path, role="supporting",
                            sha256=h.digest(h.private_read(roots[scope] / raw_path))))
                    self.assertFalse(metadata.exists())
                    self.assertTrue(metadata.with_suffix(".raw").is_file())
                    c.inputs["run"] = h.private_manifest(c.current)
                    c.inputs["prior_retirement"] = [row for row in h.private_manifest(c.directory)
                        if row["path"].startswith(("prior/", "preconditions/"))]
                    c.inputs["external_attempts"][0]["manifest"] = h.private_manifest(roots["external-one"])
                    c.authorization["input_manifest_sha256"] = c.review["input_manifest_sha256"] = h.digest(h.canonical(c.inputs))
                    before = h.private_manifest(c.current)
                    with self.assertRaisesRegex(h.Stop, "capture raw.*UNKNOWN"):
                        self.review_facts(c)
                    self.assertEqual(before, h.private_manifest(c.current))
                    self.assertFalse((c.directory / "receipt.json").exists())

    def test_unapproved_or_unfrozen_historical_producer_is_rejected(self):
        for mode in ("unapproved", "wrong-bytes"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temporary:
                c = self.historical_fixture(temporary)
                if mode == "unapproved":
                    c.review["producer_bindings"] = []
                else:
                    c.review["producer_bindings"][0]["script_ref"]["sha256"] = "f" * 64
                with self.assertRaises(h.Stop):
                    self.review_facts(c)


class LegacyReceiptTests(unittest.TestCase):
    def test_receipt_v1_and_void_tombstone_are_only_read_verified(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = h.mkdir(Path(temporary))
            run_id = str(uuid.uuid4())
            archive = h.mkdir(root / ".retired" / run_id)
            directory = h.mkdir(root / ".retirement" / run_id)
            h.publish(archive / "state.json", b'{"outcome":"stopped","effects":{"original-stop":"accepted"}}')
            original = h.private_manifest(archive)
            slot = h.mkdir(archive / "retired-start-slot")
            h.publish(slot / "request.json", b'{"historical":"original slot"}')
            preconditions = h.mkdir(directory / "preconditions")
            h.publish(preconditions / "capture.raw", b'{}')
            h.publish(directory / "authorization.json", b'{"schema":"rh-retirement-authorization/1"}')
            manifest = dict(run=original, slot=h.private_manifest(slot), preconditions=h.private_manifest(preconditions), saved=[])
            h.publish(directory / "manifest.json", h.canonical(manifest))
            closed = [dict(worker_id="historical-worker", held=True, last_ack_d="D-1", next_d="D-3")]
            intent = dict(schema="rh-retirement-intent/1", run_id=run_id, old_id="historical-old", closed_effects=closed,
                          authorization_sha256=h.digest(h.private_read(directory / "authorization.json")),
                          manifest_sha256=h.digest(h.canonical(manifest)), preconditions_sha256=h.digest(h.canonical(manifest["preconditions"])))
            h.publish(directory / "intent.json", h.canonical(intent))
            final = directory / ("preconditions-final-" + uuid.uuid4().hex + ".json")
            h.publish(final, h.canonical(h.private_manifest(preconditions)))
            receipt = dict(schema="rh-retired-run/1", run_id=run_id, old_id="historical-old", closed_effects=closed,
                           intent_sha256=h.digest(h.canonical(intent)), manifest_sha256=intent["manifest_sha256"],
                           authorization_sha256=intent["authorization_sha256"], preconditions=h.file_reference(final))
            h.publish(directory / "receipt.json", h.canonical(receipt))
            void_id = str(uuid.uuid4())
            h.publish(root / ".voided" / void_id / "void-receipt.json", b'{"result":"voided-administratively"}')
            before = h.private_manifest(root)
            self.assertEqual(len(h.retirement_guard(root, "historical-old")), 1)
            self.assertEqual(h.decode(h.private_read(directory / "receipt.json"))["closed_effects"], closed)
            for old_id in (run_id, void_id):
                with self.assertRaisesRegex(h.Stop, "retired"):
                    h.retirement_guard(root, run_id=old_id)
            self.assertEqual(before, h.private_manifest(root))


class ControlledMaterialTests(unittest.TestCase):
    def test_worker_control_preservation_requires_existing_authorized_materials(self):
        for mode in ("external-ok", "internal-ok", "missing-and-unapproved", "external-unapproved", "external-missing",
                     "hash-drift", "wrong-worker", "duplicate-authorization", "internal-missing", "extra-missing"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temporary:
                data = h.mkdir(Path(temporary) / "data")
                current = h.mkdir(data / "run")
                workspace = h.mkdir(data / "worktrees" / h.PROJECT / "worker")
                run_id, sid = str(uuid.uuid4()), "worker"
                handle = run_id + ":worker-stop:" + sid
                rules = dict(worker_id=sid, reserved_stop_d="D-77", hold_state="held", release=dict(mode="keep-held"))
                request = dict(old_id="old", workers=[rules], workspace_index=[dict(session_id=sid, kind="worker", path=str(workspace))])
                state = dict(run_id=run_id)
                payload = dict(run_id=run_id, recipient=sid, old_id="old", phase="worker-stop", instruction=dict(directive="D-77", rules=rules))
                raw = h.canonical(payload)
                control = current / "control" / ("worker-stop-" + sid)
                argv = ["send", "--session", sid, "--steer", "--client-message-id", handle, "--message", raw.decode()]
                h.publish(control / "payload.json", raw)
                h.publish(control / "intent.json", h.canonical(dict(argv=argv)))
                material = (current / "workers" / sid / "saved/necessary.txt") if mode.startswith("internal") else workspace / "necessary.txt"
                h.publish(material, b"synthetic necessary material")
                sha = h.digest(h.read_regular(material))
                if not mode.startswith("internal"):
                    material.chmod(0o644)
                reference = dict(path=str(material), sha256=sha, kind="necessary-material")
                item = dict(reference, worker_id=sid, basis="original worker preservation")
                materials = [] if mode in ("internal-ok", "internal-missing", "missing-and-unapproved", "external-unapproved") else [item]
                if mode == "wrong-worker":
                    item["worker_id"] = "other"
                if mode == "duplicate-authorization":
                    materials.append(dict(item))
                if mode == "extra-missing":
                    materials.append(dict(item, path=str(workspace / "also-necessary.txt")))
                if mode in ("missing-and-unapproved", "external-missing", "internal-missing"):
                    material.unlink()
                if mode == "hash-drift":
                    material.write_bytes(b"changed material")
                stop = dict(schema="rh-worker-stop/1", run_id=run_id, worker_id=sid, project_id=h.PROJECT,
                            state="blocked", resume_state="held", evidence_turn_id="turn", preservation=[reference],
                            stop_directive=dict(last_ack_d="D-1", next_d="D-77"))
                h.publish(current / "workers" / sid / "stop.json", h.canonical(stop))
                message = dict(id="directive", kind="message", role="user", sequence=1, turnId="turn", clientMessageId=handle,
                               origin="automation", senderSessionId="old", senderProjectId=h.PROJECT, text="[from old] " + raw.decode())
                def snapshot(actor, kind, entries):
                    return dict(session=dict(id=actor, kind=kind, projectId=h.PROJECT, isTerminated=False, activity=dict(state="idle")),
                                transcript=dict(incomplete=[], truncatedFields=0, turns=[dict(id="turn", state="completed", completedAt="synthetic-completed")], entries=entries))
                snaps = {"old": snapshot("old", "orchestrator", []), sid: snapshot(sid, "worker", [message])}
                cli = SimpleNamespace(data_dir=data, session=lambda actor: snaps[actor], documents=lambda: dict(lease=dict(sessionId="old", frozen=False)),
                                      sessions=lambda: [dict(id=k, role=v["session"]["kind"], projectId=h.PROJECT, isTerminated=False) for k, v in snaps.items()])
                fact = dict(kind="worker-control", expected_effect="worker-control", effect_id=handle, target=dict(session_id=sid),
                            origin="retrospective-review", message_sha256=h.digest(raw), terminal="returned", result=dict(returncode=0))
                if mode.endswith("-ok"):
                    closed = h.retirement_preconditions(cli, state, request, {handle: [fact]}, dict(materials=materials), current)
                    self.assertTrue(closed[0]["held"])
                    self.assertEqual(closed[0]["preservation"][0]["sha256"], sha)
                    self.assertEqual((closed[0]["last_ack_d"], closed[0]["next_d"]), ("D-1", "D-77"))
                else:
                    with self.assertRaises(h.Stop):
                        h.retirement_preconditions(cli, state, request, {handle: [fact]}, dict(materials=materials), current)
                self.assertFalse((current.parent / ".retirement" / run_id / "receipt.json").exists())
                self.assertTrue(current.is_dir())


class RetirementTests(unittest.TestCase):
    def setUp(self):
        from test_handoff import snapshot
        from test_wire_contract import list_row, wire
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.data = h.mkdir(Path(self.tmp.name) / "data")
        self.run_id = str(uuid.uuid4())
        self.root = h.mkdir(self.data / "projects" / h.PROJECT / "orchestrator/.private-handoff-runs")
        self.run = h.mkdir(self.root / self.run_id)
        self.slot = h.mkdir(self.root / ".start/old")
        self.directory = h.mkdir(self.root / ".retirement" / self.run_id)
        workers = [dict(worker_id="arbitrary-worker-" + str(i), reserved_stop_d="D-" + str(77 + i), hold_state="held",
                        release=dict(mode="keep-held")) for i in range(3 if "three_workers" in self._testMethodName else 0)]
        self.request = dict(schema="rh-start-request/1", request_id=self.run_id, project_id=h.PROJECT, old_id="old",
                            authority=dict(source_refs=["human"]), workers=workers, target={}, sources=[])
        raw = h.canonical(self.request)
        for path in (self.run / "start.json", self.slot / "request.json"):
            h.publish(path, raw)
        h.publish(self.run / "handoff.py", Path(h.__file__).read_bytes())
        h.publish(self.run / "runner.lock", b"")
        h.publish(self.data / "running.json", b'{"port":1}')
        executable = Path("/usr/bin/true")
        qualification = dict(schema="rh-cli-qualification/2", call_protocol=h.CALL_PROTOCOL,
                             script_sha256=h.digest(Path(h.__file__).read_bytes()), cli_sha256=h.digest(executable.read_bytes()))
        qraw = h.canonical(qualification)
        h.publish(self.directory / "qualification.original.json", qraw)
        h.publish(self.root / ".qualified/qualification.json", qraw)
        self.state = dict(schema="rh-state/1", run_id=self.run_id, project_id=h.PROJECT, old_id="old", target={},
                          start_sha256=h.digest(raw), script_sha256=qualification["script_sha256"], cli_sha256=qualification["cli_sha256"],
                          cli_path=str(executable), data_dir=str(self.data), run_file=str(self.data / "running.json"),
                          daemon_base="http://127.0.0.1:1/api/v1/", journal=str(self.data / "projects" / h.PROJECT / "orchestrator/.handoff-journal" / self.run_id), call_protocol=h.CALL_PROTOCOL,
                          qualification_sha256=h.digest(qraw), stage="prepared", outcome="stopped", effects={})
        h.publish(self.run / "state.json", h.canonical(self.state))
        self.journal = h.Journal(self.data, self.run_id)
        self.journal.bind(dict(root=str(self.root), old_id="old", start_sha256=h.digest(raw), qualification_sha256=h.digest(qraw)))
        # Real ended process, not a mocked process probe.
        code = """import importlib.util,pathlib,sys
s=importlib.util.spec_from_file_location('h',sys.argv[1]);h=importlib.util.module_from_spec(s);s.loader.exec_module(h)
j=h.Journal(pathlib.Path(sys.argv[2]),sys.argv[3]);e=j.register('start','old')
c=h.CLI(pathlib.Path('/usr/bin/true'),'old',pathlib.Path(sys.argv[2]),pathlib.Path(sys.argv[2])/'running.json',pathlib.Path(sys.argv[2]));c.execution=e
c.call(['spawn','--help']);e.close()
"""
        subprocess.run([sys.executable, "-I", "-B", "-c", code, h.__file__, str(self.data), self.run_id], check=True, capture_output=True)
        self.snaps = {"old": snapshot()}
        holds = {}
        for worker in workers:
            sid = worker["worker_id"]
            anchor = dict(id="held-" + sid, kind="message", role="assistant", sequence=1, turnId="turn", revision=1,
                          text="synthetic existing hold", streaming=False)
            snap = snapshot(entries=[anchor])
            snap["session"].update(id=sid, kind="worker")
            self.snaps[sid] = snap
            path = self.directory / "holds" / (sid + ".json")
            h.publish(path, h.canonical(snap))
            holds[sid] = dict(snapshot=h.file_reference(path), message_id=anchor["id"], held=True)
        self.inputs = dict(run=h.private_manifest(self.run), slot=h.private_manifest(self.slot), qualification_sha256=h.digest(qraw),
                           prior_retirement=[x for x in h.private_manifest(self.directory) if x["path"].startswith("holds/")])
        h.publish(self.directory / "input-manifest.json", h.canonical(self.inputs))
        self.authorization = dict(schema="rh-retirement-authorization/2", run_id=self.run_id, old_id="old", project_id=h.PROJECT,
                                  operator_session_id="operator", decision_ref="synthetic-user-approval", directive_ref="D-91",
                                  start_sha256=h.digest(raw), input_manifest_sha256=h.digest(h.canonical(self.inputs)), expected_effect_ids=[], hold_evidence=holds)
        h.publish(self.directory / "authorization.json", h.canonical(self.authorization))
        self.list_row, self.wire, self.snapshot = list_row, wire, snapshot

    def transport(self, argv, **kwargs):
        args = argv[1:]
        if args[:2] == ["session", "get"]:
            value = self.snaps[args[2]]
        elif args[:2] == ["session", "ls"]:
            value = dict(data=[self.list_row(x["session"]) for x in self.snaps.values()], meta=self.wire("list")["meta"])
        elif args[:3] == ["project", "documents", "info"]:
            value = dict(documents=dict(lease=dict(sessionId="old", frozen=False)))
        else:
            raise AssertionError(args)
        return SimpleNamespace(returncode=0, stdout=h.canonical(value), stderr=b"")

    def retire(self):
        with patch.dict(os.environ, AO_SESSION_ID="operator"), patch.object(h.subprocess, "run", side_effect=self.transport):
            return h.retire_run(self.root, self.run_id)

    def test_arbitrary_uuid_retirement_preserves_original_and_journal_and_guards(self):
        original = h.private_manifest(self.run)
        receipt = self.retire()
        self.assertEqual(receipt["schema"], "rh-retired-run/2")
        self.assertEqual(receipt["hold"], "unchanged")
        archive = self.root / ".retired" / self.run_id
        self.assertEqual([x for x in h.private_manifest(archive) if not x["path"].startswith("retired-start-slot")], original)
        self.assertEqual(self.retire(), receipt)
        self.assertEqual(self.journal.state()["status"], "retired")
        with self.assertRaises(h.Stop):
            self.journal.register("locate", "old")
        with self.assertRaises(h.Stop):
            h.retirement_guard(self.root, run_id=self.run_id)
        self.assertTrue(list((self.directory / "journal.original/executions").glob("*/calls/*/stdout.raw")))

    def test_crash_after_manifest_before_intent_recovers_without_another_io(self):
        publish = h.publish
        def crash(path, raw):
            if Path(path) == self.directory / "intent.json":
                raise OSError("synthetic crash")
            return publish(path, raw)
        with patch.object(h, "publish", side_effect=crash), self.assertRaisesRegex(OSError, "synthetic crash"):
            self.retire()
        self.assertTrue((self.directory / "manifest.json").exists())
        before = h.private_manifest(self.directory / "journal.original")
        with self.assertRaises(h.Stop):
            h.retirement_guard(self.root, "old")
        h.publish(self.directory / "recovery-authorization.json", h.canonical(dict(operator_session_id="operator", directive_ref="D-92", fence_token=self.journal.state()["token"])))
        with patch.dict(os.environ, AO_SESSION_ID="operator"), patch.object(h.subprocess, "run") as transport:
            h.retire_run(self.root, self.run_id)
            transport.assert_not_called()
        self.assertEqual(before, h.private_manifest(self.directory / "journal.original"))

    def test_slot_drift_fences_without_receipt_and_cannot_reopen(self):
        (self.slot / "request.json").write_bytes(b"changed")
        with self.assertRaises(h.Stop):
            self.retire()
        self.assertEqual(self.journal.state()["status"], "retiring")
        self.assertFalse((self.directory / "receipt.json").exists())
        self.assertFalse((self.root / ".retired" / self.run_id).exists())

    def test_three_workers_with_other_reserved_directives_keep_their_holds(self):
        receipt = self.retire()
        self.assertEqual(len(receipt["closed_effects"]), 3)
        self.assertTrue(all(x["held"] for x in receipt["closed_effects"]))
        self.assertEqual([w["reserved_stop_d"] for w in self.request["workers"]], ["D-77", "D-78", "D-79"])

    def test_three_workers_later_durable_steer_blocks_even_after_completed(self):
        from producer_transport import directive
        sid = self.request["workers"][0]["worker_id"]
        snap = self.snaps[sid]
        snap["session"]["activity"]["state"] = "active"
        snap["session"]["status"] = "working"
        snap["transcript"]["turns"][0].update(state="running")
        snap["transcript"]["turns"][0].pop("completedAt", None)
        message = dict(Text="[from old] synthetic later directive", ClientMessageID=str(uuid.uuid4()),
                       Origin="automation", SenderSessionID="old", SenderProjectID=h.PROJECT)
        actual = directive(snap, [message], True)
        # Preserve the original anchor; the new steer itself is the unmodified
        # durable Controller/SQLite projection, never a handwritten user message.
        anchor = snap["transcript"]["entries"][0]
        actual["transcript"]["entries"] = [anchor, *[e for e in actual["transcript"]["entries"] if e.get("id") != anchor["id"]]]
        actual["session"]["activity"]["state"] = "idle"
        actual["session"]["status"] = "idle"
        actual["transcript"]["incomplete"] = []
        for turn in actual["transcript"]["turns"]:
            turn.update(state="completed", completedAt=h.now())
        self.snaps[sid] = actual
        with self.assertRaisesRegex(h.Stop, "new directive/authority"):
            self.retire()
        self.assertFalse((self.directory / "receipt.json").exists())

    def test_canonical_success_before_business_receipt_reenters_without_dispatch(self):
        cli = h.CLI(Path("/usr/bin/true"), "old", self.data, self.data / "running.json", self.data)
        cli.execution = self.journal.register("_run", "old")
        self.addCleanup(cli.execution.close)
        run = h.Run(self.run, cli)
        original_publish = h.publish
        def crash(path, raw):
            if Path(path) == self.run / "control/kill/result.json":
                raise OSError("business publication crash")
            return original_publish(path, raw)
        args = ["session", "kill", "old", "--project", h.PROJECT]
        with patch.object(h, "publish", side_effect=crash), self.assertRaises(OSError):
            run.effect("kill", args, 60)
        self.assertFalse((self.run / "control/kill/result.json").exists())
        with patch.object(h.subprocess, "run") as transport:
            receipt = run.effect("kill", args, 60)
            transport.assert_not_called()
        self.assertTrue(receipt["call_id"])
        self.assertEqual(run.state["effects"]["kill"], "accepted")

    def test_existing_helper_guard_and_locate_registration_cannot_cross_fence(self):
        os.rename(self.directory, self.data / "prepared-retirement-authority")
        code = """import importlib.util,pathlib,sys,time
s=importlib.util.spec_from_file_location('h',sys.argv[1]);h=importlib.util.module_from_spec(s);s.loader.exec_module(h)
data=pathlib.Path(sys.argv[2]);run_id=sys.argv[3];mode=sys.argv[4];signal=pathlib.Path(sys.argv[5]);gate=pathlib.Path(sys.argv[6]);j=h.Journal(data,run_id);e=None
try:
 if mode=='helper':
  e=j.register('old-receipt','old');r=h.load_run(pathlib.Path(sys.argv[7]),run_id,None,e)
 signal.write_text('guard passed' if mode=='helper' else 'before registration')
 end=time.monotonic()+10
 while not gate.exists():
  if time.monotonic()>end:raise RuntimeError('test gate timeout')
  time.sleep(.01)
 if mode=='helper':r.snapshot('old')
 else:
  e=j.register('start','old');h.locate('old',create=True,execution=e)
except h.Stop as exc:
 print(str(exc));sys.exit(17)
finally:
 if e:e.close()
"""
        for mode in ("helper", "locate"):
            run_id = self.run_id if mode == "helper" else str(uuid.uuid4())
            journal = self.journal if mode == "helper" else h.Journal(self.data, run_id)
            signal, gate = self.data / (mode + ".ready"), self.data / (mode + ".go")
            child = subprocess.Popen([sys.executable, "-I", "-B", "-c", code, h.__file__, str(self.data), run_id,
                                      mode, str(signal), str(gate), str(self.root)], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                end = time.monotonic() + 10
                while not signal.exists() and child.poll() is None and time.monotonic() < end:
                    time.sleep(.01)
                self.assertTrue(signal.exists(), child.communicate(timeout=1) if child.poll() is not None else "no guard signal")
                journal.freeze("operator", "D-91", "a" * 64)
                gate.write_text("resume")
                stdout, stderr = child.communicate(timeout=10)
                self.assertEqual(child.returncode, 17, stderr.decode())
                self.assertIn(b"admission denied", stdout)
                self.assertEqual(journal.state()["status"], "retiring")
            finally:
                if child.poll() is None:
                    gate.write_text("resume")
                    child.communicate(timeout=12)


if __name__ == "__main__":
    unittest.main()
