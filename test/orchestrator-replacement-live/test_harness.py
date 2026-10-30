import importlib.util
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

SCRIPT = pathlib.Path(__file__).with_name("harness.py")


class IsolationHarnessTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = pathlib.Path(self.tmp.name) / "lab"

    def invoke(self, *args):
        return subprocess.run([sys.executable, str(SCRIPT), "--lab-root", str(self.root), *args], capture_output=True, text=True, timeout=10)

    def load(self):
        self.assertTrue(SCRIPT.exists(), "dry/fake harness missing")
        spec = importlib.util.spec_from_file_location("replacement_live_harness", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_default_dry_run_has_no_process_or_api_side_effects(self):
        module = self.load()
        with mock.patch.object(module.subprocess, "Popen", side_effect=AssertionError("dry run started a process")):
            manifest = module.run(self.root, "http://127.0.0.1:52969", "dry-run")
        self.assertFalse(self.root.exists())
        self.assertEqual(manifest["providerCalls"], 0)
        self.assertEqual(manifest["mode"], "dry-run")
        self.assertEqual(len(manifest["cases"]), 6)
        self.assertTrue(all(case["liveStatus"] == "NOT_RUN" for case in manifest["cases"]))
        self.assertEqual(manifest["acceptance"]["A15"], "NOT_RUN")
        self.assertEqual(manifest["acceptance"]["A23"], "NOT_RUN")
        self.assertEqual(len(manifest["caseSHA256"]), 64)

    def test_live_is_refused_before_any_launch(self):
        result = self.invoke("--mode", "live")
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("4B", result.stderr)
        self.assertFalse(self.root.exists())

    def test_stable_path_and_port_are_refused(self):
        module = self.load()
        for root, url in [(pathlib.Path.home() / ".ao" / "data", "http://127.0.0.1:52969"), (self.root, "http://127.0.0.1:3001"), (self.root, "http://0.0.0.0:52969"), (self.root, "https://127.0.0.1:52969"), (self.root, "http://user@127.0.0.1:52969")]:
            with self.subTest(root=root, url=url), self.assertRaises(ValueError):
                module.run(root, url, "dry-run")
        self.assertFalse(self.root.exists())

    def test_runfile_and_root_symlink_alias_are_refused(self):
        module = self.load()
        outside = pathlib.Path(self.tmp.name) / "outside"
        outside.mkdir()
        self.root.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(ValueError):
            module.run(self.root, "http://127.0.0.1:52969", "dry-run")
        self.root.unlink()
        self.root.mkdir()
        (self.root / "run.json").symlink_to(outside / "running.json")
        with self.assertRaises(ValueError):
            module.run(self.root, "http://127.0.0.1:52969", "dry-run")
        self.assertEqual(list(outside.iterdir()), [])

    def test_subject_identity_cannot_be_replaced_with_existing_worker(self):
        module = self.load()
        manifest = module.run(self.root, "http://127.0.0.1:52969", "dry-run")
        manifest["subjects"]["source"] = "agent-orchestrator-82"
        with self.assertRaises(ValueError):
            module.validate_manifest(manifest)
        result = self.invoke("--source-id", "agent-orchestrator-82")
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.root.exists())

    def test_cleanup_refuses_changed_identity_and_foreign_process_group(self):
        module = self.load()
        process = mock.Mock(pid=4321)
        process.poll.return_value = None
        for identity, group in [("changed", 4321), ("original", 9999)]:
            with self.subTest(identity=identity, group=group), mock.patch.object(module, "start_time", return_value=identity), mock.patch.object(module.os, "getpgid", return_value=group), mock.patch.object(module.os, "killpg") as kill:
                with self.assertRaises(ValueError):
                    module.stop_owned_group(process, "original")
                kill.assert_not_called()

    def test_dry_run_cli_is_machine_readable(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest = json.loads(result.stdout)
        self.assertEqual(manifest["permissions"], {"stable": False, "realProvider": False, "paidLive": False})
        self.assertEqual(manifest["prerequisites"]["account"], "NOT_PROVIDED")
        self.assertEqual(manifest["prerequisites"]["budget"], "NOT_AUTHORIZED")
        self.assertFalse(self.root.exists())

    def test_dry_run_accepts_another_session_scratch(self):
        module = self.load()
        home = pathlib.Path(module.pwd.getpwuid(os.getuid()).pw_dir)
        root = home / ".ao/scratch" / ("88-" + module.uuid.uuid4().hex[:4]) / "dry-only"
        with mock.patch.object(module.subprocess, "Popen", side_effect=AssertionError("dry run launched")):
            result = module.run(root, "http://127.0.0.1:52969", "dry-run")
        self.assertEqual(result["mode"], "dry-run")
        self.assertFalse(root.exists())

    def test_fake_uses_configured_go_binary(self):
        module = self.load()
        binary = pathlib.Path(self.tmp.name) / "tools/go"
        binary.parent.mkdir()
        binary.write_text("#!/bin/sh\nexit 0\n")
        binary.chmod(0o700)
        process = mock.Mock(pid=4321)
        process.wait.return_value = process.poll.return_value = 0
        with mock.patch.dict(os.environ, {"GO_BINARY": str(binary)}), mock.patch.object(module.os, "getloadavg", return_value=(0, 0, 0)), mock.patch.object(module.subprocess, "check_output", return_value="uptime fixture"), mock.patch.object(module.subprocess, "Popen", return_value=process) as launch, mock.patch.object(module, "start_time", return_value="fixture-identity"):
            result = module.run(self.root, "http://127.0.0.1:52969", "fake")
        self.assertEqual(launch.call_args.args[0][0], str(binary))
        self.assertEqual(result["execution"]["goBinary"], str(binary))
        self.assertTrue(launch.call_args.kwargs["env"]["PATH"].startswith(str(binary.parent) + os.pathsep))

    def test_configured_load_limit_refuses_before_root_or_launch(self):
        module = self.load()
        process = mock.Mock(pid=4321)
        process.wait.return_value = process.poll.return_value = 0
        with mock.patch.dict(os.environ, {"AO_REPLACEMENT_MAX_LOAD": "1"}), mock.patch.object(module.os, "getloadavg", return_value=(1.5, 0, 0)), mock.patch.object(module.subprocess, "check_output", return_value="uptime fixture"), mock.patch.object(module.subprocess, "Popen", return_value=process) as launch, mock.patch.object(module, "start_time", return_value="fixture-identity"):
            with self.assertRaises(ValueError):
                module.run(self.root, "http://127.0.0.1:52969", "fake")
            launch.assert_not_called()
        self.assertFalse(self.root.exists())

    def test_cli_accepts_toolchain_and_load_options_without_launch(self):
        result = self.invoke("--go", "go", "--max-load", "2.5")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["execution"]["maxLoad"], 2.5)
        self.assertFalse(self.root.exists())

    def test_invalid_load_limits_are_refused(self):
        module = self.load()
        for value in ("0", "-1", "nan", "inf", "invalid"):
            with self.subTest(value=value), mock.patch.dict(os.environ, {"AO_REPLACEMENT_MAX_LOAD": value}), self.assertRaises(ValueError):
                module.run(self.root, "http://127.0.0.1:52969", "dry-run")
        self.assertFalse(self.root.exists())

    def test_missing_go_is_refused_before_creating_lab(self):
        module = self.load()
        missing = pathlib.Path(self.tmp.name) / "missing/go"
        with mock.patch.dict(os.environ, {"GO_BINARY": str(missing)}), mock.patch.object(module.subprocess, "Popen") as launch:
            with self.assertRaisesRegex(ValueError, "not found"):
                module.run(self.root, "http://127.0.0.1:52969", "fake")
            launch.assert_not_called()
        self.assertFalse(self.root.exists())

    def test_non_go_command_is_refused_before_launch(self):
        module = self.load()
        with mock.patch.dict(os.environ, {"GO_BINARY": sys.executable}), mock.patch.object(module.subprocess, "Popen") as launch:
            with self.assertRaisesRegex(ValueError, "Go toolchain"):
                module.run(self.root, "http://127.0.0.1:52969", "fake")
            launch.assert_not_called()
        self.assertFalse(self.root.exists())


if __name__ == "__main__":
    unittest.main()
