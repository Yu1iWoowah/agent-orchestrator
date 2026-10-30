#!/usr/bin/env python3
"""R0 4A dry/fake planner. Real-provider execution is deliberately unavailable."""
import argparse
import hashlib
import json
import math
import os
import pathlib
import pwd
import re
import shutil
import signal
import subprocess
import sys
import urllib.parse
import uuid

CASES = [("C1", "chat"), ("T1", "tui"), ("C2", "chat"), ("T2", "tui"), ("C3", "chat"), ("T3", "tui")]
FAKE_TESTS = "^(TestReplacementNotificationsRunOutsideProjectGateAndRecoverLostReply|TestReplacementWorkerChatFYIQueuesWithoutInterruptAndDeduplicates|TestReplacementWorkerTUINoticeUsesOneSafeWriteAndNoNudge)$"


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def subjects(key):
    prefix = "lab-replacement-" + uuid.UUID(key).hex
    return {"project": prefix, "source": prefix + "-source", "successor": prefix + "-successor", "chatWorker": prefix + "-chat-worker", "tuiWorker": prefix + "-tui-worker"}


def validate_paths(root, daemon_url):
    actual_home = pathlib.Path(pwd.getpwuid(os.getuid()).pw_dir)
    scratch = actual_home / ".ao" / "scratch"
    root = pathlib.Path(os.path.abspath(root))
    try:
        relative = root.relative_to(scratch)
    except ValueError as exc:
        raise ValueError("lab must be beneath the user's .ao/scratch directory") from exc
    if len(relative.parts) < 2:
        raise ValueError("lab must have a named scratch parent")
    for home in (actual_home, pathlib.Path.home()):
        for stable in (home / ".ao" / "data", home / ".ao" / "electron"):
            if root == stable or stable in root.parents:
                raise ValueError("stable AO paths are forbidden")
    paths = {name: root / name for name in ("home", "data", "run.json", "electron", "claude", "tmp", "tmux")}
    # Refuse aliases even when they happen to resolve inside the allowed tree.
    for path in (root, *paths.values()):
        if any(part.is_symlink() for part in (path, *path.parents)):
            raise ValueError("symlink/alias isolation paths are forbidden")
    parsed = urllib.parse.urlsplit(daemon_url)
    try:
        port = parsed.port
    except ValueError as exc:
        raise ValueError("invalid isolated daemon port") from exc
    if parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or parsed.username is not None or parsed.password is not None or parsed.path or parsed.query or parsed.fragment or port is None or port < 1024 or port == 3001 or str(port) == os.environ.get("AO_PORT"):
        raise ValueError("only a dedicated loopback daemon port is allowed; stable port 3001 is forbidden")
    return root, paths


def validate_manifest(manifest):
    if manifest["subjects"] != subjects(manifest["operationKey"]):
        raise ValueError("only fresh, operation-scoped lab subjects are allowed")
    if manifest["permissions"] != {"stable": False, "realProvider": False, "paidLive": False}:
        raise ValueError("4A has no live or stable authority")
    if manifest["caseSHA256"] != hashlib.sha256(canonical(manifest["cases"])).hexdigest():
        raise ValueError("case manifest changed")


def start_time(pid):
    try:
        return pathlib.Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19]
    except (OSError, IndexError):
        return None


def stop_owned_group(process, identity):
    if process.poll() is not None:
        return
    if not identity or start_time(process.pid) != identity or os.getpgid(process.pid) != process.pid:
        raise ValueError("cannot verify the fake test process group; refusing a broad kill")
    os.killpg(process.pid, signal.SIGTERM)
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        if start_time(process.pid) != identity:
            raise ValueError("fake test process identity changed")
        os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=5)


def execution_options(go_binary, max_load):
    requested = str(go_binary or os.environ.get("GO_BINARY", "go"))
    if not re.fullmatch(r"go(?:\d+(?:\.\d+){1,2})?(?:\.exe)?", pathlib.Path(requested).name):
        raise ValueError("--go/GO_BINARY must name a Go toolchain executable")
    limit = float(max_load if max_load is not None else os.environ.get("AO_REPLACEMENT_MAX_LOAD", os.cpu_count() or 1))
    if not math.isfinite(limit) or limit <= 0:
        raise ValueError("--max-load/AO_REPLACEMENT_MAX_LOAD must be finite and positive")
    return {"goBinary": shutil.which(requested) or requested, "maxLoad": limit}


def run_fake(root, paths, execution):
    if sys.platform != "linux":
        raise ValueError("fake runner currently requires Linux process identity checks")
    go_binary = shutil.which(execution["goBinary"])
    if go_binary is None:
        raise ValueError("Go toolchain not found; configure --go or GO_BINARY")
    print(subprocess.check_output(["uptime"], text=True).strip(), file=sys.stderr)
    if os.getloadavg()[0] >= execution["maxLoad"]:
        raise ValueError("load reached the configured limit; wait before starting the fake test group")
    os.umask(0o022)
    root.mkdir(mode=0o700)  # No reuse or overwrite of an earlier lab/evidence.
    for name, path in paths.items():
        if name != "run.json":
            path.mkdir(mode=0o700)
            if path.stat().st_mode & 0o777 != 0o700:
                raise ValueError("private isolation directory is not 0700")
    home = pathlib.Path(pwd.getpwuid(os.getuid()).pw_dir)
    env = {"PATH": os.pathsep.join((str(pathlib.Path(go_binary).parent), os.defpath)), "HOME": str(paths["home"]), "TMPDIR": str(paths["tmp"]), "GOTMPDIR": str(paths["tmp"]), "TMUX_TMPDIR": str(paths["tmux"]), "GOMODCACHE": os.environ.get("GOMODCACHE", str(home / "go/pkg/mod")), "GOCACHE": os.environ.get("GOCACHE", str(home / ".cache/go-build")), "GOMAXPROCS": "8", "GOENV": "off", "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local", "TZ": "UTC"}
    command = [go_binary, "test", "-p", "8", "-count=1", "-run", FAKE_TESTS, "./internal/service/orchestratorreplacement", "./internal/service/chat", "./internal/session_manager"]
    backend = pathlib.Path(__file__).resolve().parents[2] / "backend"
    with (root / "fake-tests.log").open("x") as log:
        process = subprocess.Popen(command, cwd=backend, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        identity = start_time(process.pid)
        registry = {"pid": process.pid, "startTime": identity, "kind": "fixed_go_fixture_tests", "status": "running"}
        registry_path = root / "cleanup-registry.json"
        with registry_path.open("x") as out:
            json.dump(registry, out, indent=2)
        # SIGTERM should use the same verified cleanup as timeout/KeyboardInterrupt.
        def interrupted(_signum, _frame):
            raise InterruptedError("fake fixture runner interrupted")
        previous_handler = signal.signal(signal.SIGTERM, interrupted)
        try:
            code = process.wait(timeout=300)
        finally:
            try:
                stop_owned_group(process, identity)
                registry["status"], registry["exitCode"] = "exited", process.poll()
                with registry_path.open("w") as out:
                    json.dump(registry, out, indent=2)
            finally:
                signal.signal(signal.SIGTERM, previous_handler)
    registry["exitCode"] = code
    return {"status": "PASS" if code == 0 else "FAIL", "command": command, "log": str(root / "fake-tests.log")}, [registry]


def run(root, daemon_url, mode, go_binary=None, max_load=None):
    if mode not in ("dry-run", "fake"):
        raise ValueError("4B live is not authorized: no test account or paid budget (A-Q20=C, A-Q21=C)")
    root, paths = validate_paths(root, daemon_url)
    execution = execution_options(go_binary, max_load)
    key = str(uuid.uuid4())
    cases = [{"id": name, "sourceMode": source, "successorMode": "chat", "liveStatus": "NOT_RUN", "reason": "4B/account/budget not authorized"} for name, source in CASES]
    manifest = {"schemaVersion": 1, "mode": mode, "operationKey": key, "subjects": subjects(key), "isolation": {"root": str(root), "paths": {name: str(path) for name, path in paths.items()}, "daemonURL": daemon_url}, "permissions": {"stable": False, "realProvider": False, "paidLive": False}, "cases": cases, "caseSHA256": hashlib.sha256(canonical(cases)).hexdigest(), "providerCalls": 0, "prerequisites": {"account": "NOT_PROVIDED", "budget": "NOT_AUTHORIZED", "nativeProfile": "NOT_CHECKED", "electron": "NOT_RUN"}, "acceptance": {"A15": "NOT_RUN", "A23": "NOT_RUN"}, "cleanupRegistry": [], "fakeValidation": {"status": "NOT_RUN"}}
    validate_manifest(manifest)
    manifest["execution"] = execution
    if mode == "fake":
        manifest["fakeValidation"], manifest["cleanupRegistry"] = run_fake(root, paths, execution)
        with (root / "manifest.json").open("x") as out:
            json.dump(manifest, out, indent=2)
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lab-root", type=pathlib.Path, required=True)
    parser.add_argument("--daemon-url", default="http://127.0.0.1:52969")
    parser.add_argument("--mode", choices=("dry-run", "fake", "live"), default="dry-run")
    parser.add_argument("--go", help="Go executable; defaults to GO_BINARY or go on PATH")
    parser.add_argument("--max-load", type=float, help="positive load limit; defaults to AO_REPLACEMENT_MAX_LOAD or CPU count")
    args = parser.parse_args()
    try:
        manifest = run(args.lab_root, args.daemon_url, args.mode, args.go, args.max_load)
    except (ValueError, OSError, subprocess.SubprocessError) as exc:
        print(str(exc), file=sys.stderr)
        return 2
    print(json.dumps(manifest, indent=2))
    return 1 if manifest["fakeValidation"]["status"] == "FAIL" else 0


if __name__ == "__main__":
    sys.exit(main())
