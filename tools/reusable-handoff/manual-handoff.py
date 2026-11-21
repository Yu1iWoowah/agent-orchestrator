#!/usr/bin/env python3
"""One confirmed manual handoff using the deployed Orchestrator CLI."""

import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid


class Stop(Exception):
    pass


def require(condition, reason):
    if not condition:
        raise Stop(reason)


def identifier(value):
    require(isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", value),
            "invalid project/session ID")
    return value


def field(value, key):
    result = value.get(key)
    require(isinstance(result, str) and result.strip(), f"missing {key}")
    return result


def write_state(root, state):
    with (root / ".state.new").open("w") as stream:
        json.dump(state, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(root / ".state.new", root / "state.json")
    fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise Stop("daemon API redirect refused")


class AO:
    def __init__(self, executable, project):
        self.executable = executable
        self.project = project
        self.env = {key: os.environ[key] for key in
                    ("PATH", "HOME", "USER", "LANG", "LC_ALL", "AO_DATA_DIR", "AO_RUN_FILE")
                    if key in os.environ}
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def call(self, args, *, scoped=True, timeout=30):
        argv = [self.executable, *args]
        if scoped:
            argv += ["--project", self.project]
        return subprocess.run(argv, env=self.env, stdin=subprocess.DEVNULL, capture_output=True,
                              text=True, encoding="utf-8", timeout=timeout)

    def checked(self, args, **kwargs):
        result = self.call(args, **kwargs)
        require(result.returncode == 0, " ".join(args[:2]) + ": " + result.stderr.strip())
        return result.stdout

    def read(self, *args, **kwargs):
        return json.loads(self.checked(args + ("--json",), **kwargs))

    def locate(self):
        status = self.read("status", scoped=False)
        require(status.get("state") == "ready", f"daemon not ready: {status.get('state')}")
        port = status.get("port")
        require(type(port) is int and 0 < port < 65536, "invalid daemon port")
        self.base = f"http://127.0.0.1:{port}/api/v1/"
        for env, key in (("AO_DATA_DIR", "dataDir"), ("AO_RUN_FILE", "runFile")):
            self.env[env] = field(status, key)
        return {key: status[key] for key in ("dataDir", "runFile", "pid", "port", "startedAt")}

    def get(self, path):
        with self.http.open(self.base + path, timeout=30) as response:
            return json.load(response)

    def session(self, sid, kind="orchestrator"):
        session = self.get("sessions/" + identifier(sid))["session"]
        require(session.get("id") == sid and session.get("projectId") == self.project and
                session.get("kind") == kind, f"session scope or role mismatch: {sid}")
        require(type(session.get("isTerminated")) is bool, f"missing termination state: {sid}")
        return session

    def documents(self):
        return self.read("project", "documents", "info")["documents"]

    def spawn_candidate(self, token, intent_at, old):
        listing = self.read("session", "ls", "--all", "--include-terminated")
        meta = listing["meta"]
        require(all(type(meta.get(key)) is int and meta[key] == 0 for key in
                    ("hiddenTerminatedCount", "hiddenOrchestratorCount")), "incomplete spawn session list")
        candidates = [row for row in listing["data"] if token in (row.get("displayName") or "").split()]
        require(candidates, "UNKNOWN spawn: no session with the exact run token")
        require(len(candidates) == 1, "UNKNOWN spawn: multiple sessions with the exact run token")
        row = candidates[0]
        sid = identifier(row.get("id"))
        require(row.get("projectId") == self.project and row.get("role") == "orchestrator" and sid != old,
                "UNKNOWN spawn: token candidate has wrong project, role or identity")
        require(dt.datetime.fromisoformat(field(row, "createdAt").replace("Z", "+00:00")) >=
                dt.datetime.fromisoformat(intent_at), "UNKNOWN spawn: token candidate predates intent")
        return sid

    def receipt(self, shared_dir, relative, expected):
        shared = Path(shared_dir)
        path = shared / relative
        result = self.call(("project", "documents", "read", relative, "--json"))
        if result.returncode:
            if re.fullmatch(r"Project document not found \(PROJECT_DOCUMENT_NOT_FOUND\)"
                            r"(?: \[request [^\]\n]+\])?", result.stderr.strip()):
                # The same daemon error can hide an inaccessible shared root.
                require(shared.resolve(strict=True) == shared and shared.is_dir() and
                        os.access(shared, os.R_OK | os.X_OK),
                        "receipt shared directory missing or inaccessible")
                require(not path.parent.is_symlink(), "receipt directory is a symlink")
                try:
                    path.lstat()
                except FileNotFoundError:
                    return None
            raise Stop("receipt read: " + result.stderr.strip())
        file = json.loads(result.stdout)["file"]
        require(file.get("path") == relative and file.get("binary") is False, "receipt path or type mismatch")
        require(path.resolve(strict=True) == path and stat.S_IMODE(path.stat().st_mode) == 0o600,
                "receipt file must be private")
        content = field(file, "content")
        require(file.get("hash") == hashlib.sha256(content.encode()).hexdigest(), "receipt hash mismatch")
        require(json.loads(content) == expected, "receipt run/project/old/new mismatch")
        return file["hash"]

    def settings(self, sid, target):
        session = self.session(sid)
        require(session.get("mode") == "chat" and session.get("isTerminated") is False and
                session.get("harness") == target["harness"], "new session mode, harness or lifecycle mismatch")
        spawn = session.get("spawnAgentSettings") or {}
        for key in ("accountId", "model", "effort"):
            source = spawn if key == "accountId" else spawn.get("settings") or {}
            require(field(source, key) == target[key], f"spawn selection mismatch: {key}")
        provider = "claude" if target["harness"] == "claude-code" else "codex"
        accounts = self.get("agents/accounts/sessions/" + sid)
        binding = (accounts.get("bindings") or {}).get(provider) or {}
        require(field(accounts, provider) == target["accountId"] and
                field(binding, "accountId") == target["accountId"] and binding.get("available") is True,
                "actual account binding mismatch or unavailable")
        conversation = self.get("sessions/" + sid + "/conversation?limit=1")
        actual = conversation.get("settings") or {}
        for key in ("model", "reasoningEffort", "approvalMode"):
            target_key = "effort" if key == "reasoningEffort" else key
            require(field(actual, key) == target[target_key], f"actual settings mismatch: {key}")
        return dict(sessionId=sid, projectId=self.project, harness=session["harness"],
                    accountId=binding["accountId"], conversationId=field(conversation, "conversationId"),
                    **{key: actual[key] for key in ("model", "reasoningEffort", "approvalMode")})


def lease_for(documents, sid):
    lease = documents.get("lease") or {}
    require(lease.get("sessionId") == sid and lease.get("frozen") is False and
            type(lease.get("epoch")) is int and lease["epoch"] > 0, f"lease missing, frozen or wrong owner: {sid}")
    return lease["epoch"]


def wait_for(label, seconds, observe):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = observe()
        if result:
            return result
        time.sleep(2)
    raise Stop(f"timed out waiting for {label}")


def status_command(root):
    return shlex.join([sys.executable, "-I", str(root / "manual-handoff.py"), "status", str(root)])


def handoff(ao, root, state, request):
    project, old, run = (state[key] for key in ("projectId", "oldId", "runId"))
    target = request["target"]

    def record(phase, pending=None, **evidence):
        state.update(lastConfirmed=phase, pending=pending, **evidence)
        write_state(root, state)

    def activity(sid):
        session = ao.session(sid)
        require(session["isTerminated"] is False, f"session already terminated: {sid}")
        return field(session.get("activity") or {}, "state")

    wait_for("old idle", 900, lambda: activity(old) == "idle")
    old_epoch = lease_for(ao.documents(), old)
    require(old_epoch == state["oldLeaseEpoch"] and activity(old) == "idle", "old stop point or lease changed")
    record("old-idle", "kill-old")
    ao.checked(("session", "kill", old), timeout=60)
    wait_for("old termination", 60, lambda: ao.session(old)["isTerminated"])
    record("old-terminated")

    token = f"RH3[{run}]"
    receipt_prefix = f"handoff/receipt-{old}-"
    prompt = (
        f"依 {root / 'request.json'} 的 handoff 說明接續專案 {project}。已宣告材料在 {root / 'materials'}。"
        "先讀共享 A–F 與材料，向使用者回報「收到，驗證中」。"
        f"用自己的身分及 lease 執行 ao project documents write --project {project} "
        f"{receipt_prefix}<你的 session ID>.json --expected-hash missing --file <私有 JSON 草稿>。"
        f"JSON 欄位為 runId={run}、projectId={project}、oldId={old}、newId=<你的 session ID>。"
        "然後結束本回合，讓同一 runner 核實 native resume。"
        "通知 worker 與回報完成一律等驗證通知，維持既有 hold。"
    )
    intent_at = dt.datetime.now(dt.timezone.utc).isoformat()
    record("old-terminated", "spawn", spawnIntent={"token": token, "createdAt": intent_at})
    args = ["spawn", "--kind", "orchestrator", "--mode", "chat", "--name", token + " orchestrator",
            "--prompt", prompt]
    for flag, key in (("agent", "harness"), ("account", "accountId"), ("model", "model"),
                      ("effort", "effort"), ("approval-mode", "approvalMode")):
        args += ["--" + flag, target[key]]
    try:
        ao.checked(args, timeout=180)
    except (Stop, OSError, subprocess.TimeoutExpired) as error:
        record("old-terminated", "spawn", spawnReplyError=str(error))
    new = ao.spawn_candidate(token, intent_at, old)
    record("new-identified", newId=new)

    expected = dict(projectId=project, oldId=old, runId=run, newId=new)
    relative = receipt_prefix + new + ".json"

    def project_documents():
        docs = ao.documents()
        require(docs.get("sharedDir") == state["sharedDir"], "receipt project shared directory changed")
        return docs

    def ready():
        receipt_hash = ao.receipt(state["sharedDir"], relative, expected)
        docs = project_documents()
        if receipt_hash is None:
            return None
        epoch = lease_for(docs, new)
        require(epoch > old_epoch, "new lease did not follow old lease")
        return (receipt_hash, epoch) if activity(new) == "idle" else None

    receipt_hash, epoch = wait_for("new receipt and idle", 2700, ready)
    record("new-idle", receipt={"path": relative, "hash": receipt_hash}, newLeaseEpoch=epoch)
    before = ao.settings(new, target)
    require(activity(new) == "idle", "new no longer idle before exit")
    record("settings-before", "exit-agent", before=before)
    exited = ao.read("session", "exit-agent", new, timeout=60)
    require(exited.get("ok") is True and exited.get("sessionId") == new and activity(new) == "exited",
            "exit-agent did not complete for new")
    record("new-exited", "resume-agent")
    resumed = ao.read("session", "resume-agent", new, timeout=180)
    require(resumed.get("ok") is True and resumed.get("sessionId") == new and resumed.get("resumeMode") == "native",
            "resume-agent did not complete in native mode for new")
    record("new-resumed-native", resume={"sessionId": new, "resumeMode": "native"})
    after = ao.settings(new, target)
    require(after == before, "session, conversation or settings changed after native resume")
    require(ao.receipt(state["sharedDir"], relative, expected) == receipt_hash and
            lease_for(project_documents(), new) == epoch, "receipt or new lease changed after resume")
    record("verified", "notify-new", after=after)
    ao.checked(("send", "--session", new, "--steer", "--client-message-id", run + ":verified",
                "--message", f"手動換手已通過驗證。先執行 {status_command(root)} 確認 lastConfirmed 為 "
                f"verified 或 complete，且 newId 是你。依 {root / 'request.json'} 的 workers 授權，"
                f"以你自己的身分通知 {project} 中仍存活的具名 worker；依原 hold 與 D 接續。"
                "向使用者回報完成與待決事項。"), scoped=False, timeout=60)
    record("complete", outcome="complete")


def prepare(path):
    raw = path.read_bytes()
    request = json.loads(raw)
    project, old = (identifier(request.get(key)) for key in ("projectId", "oldId"))
    require(os.environ.get("AO_SESSION_ID") == old, "prepare must run as the named old orchestrator")
    field(request, "confirmation")
    field(request, "handoff")
    target = request["target"]
    for key in ("harness", "accountId", "model", "effort", "approvalMode"):
        field(target, key)
    require(target["harness"] in ("claude-code", "codex"), "manual handoff requires Claude Code or Codex Chat")
    executable = shutil.which("ao")
    require(executable, "ao CLI not on PATH")
    ao = AO(str(Path(executable).resolve(strict=True)), project)
    daemon = ao.locate()
    version = field(request, "version")
    require(ao.checked(("version",), scoped=False).strip() == version, "approved CLI version mismatch")
    require(ao.session(old)["isTerminated"] is False, "old already terminated")
    docs = ao.documents()
    shared = field(docs, "sharedDir")
    require(Path(shared).resolve(strict=True) == Path(shared) and Path(shared).is_relative_to(
        Path(daemon["dataDir"]).resolve(strict=True) / "projects" / project / "orchestrator"),
        "receipt shared directory outside the named project")
    epoch = lease_for(docs, old)
    for sid, message in request["workers"].items():
        ao.session(identifier(sid), kind="worker")
        require(isinstance(message, str) and message.strip(), f"missing worker authorization: {sid}")

    run = str(uuid.uuid4())
    parent = Path(daemon["dataDir"]).resolve(strict=True) / ".manual-handoff"
    parent.mkdir(mode=0o700, exist_ok=True)
    require(not parent.is_symlink() and stat.S_IMODE(parent.stat().st_mode) == 0o700,
            "manual handoff directory must be private")
    root = parent / run
    root.mkdir(mode=0o700)
    (root / "materials").mkdir()
    for name, source in request["materials"].items():
        source = Path(source)
        require(name == Path(name).name and name not in ("", ".", "..") and source.is_absolute() and
                source.resolve(strict=True) == source and source.is_file(), f"invalid declared material: {name}")
        shutil.copyfile(source, root / "materials" / name)
    (root / "request.json").write_bytes(raw)
    shutil.copyfile(__file__, root / "manual-handoff.py")
    write_state(root, dict(runId=run, projectId=project, oldId=old, daemon=daemon, version=version,
                           executable=ao.executable, sharedDir=shared, oldLeaseEpoch=epoch,
                           requestSha256=hashlib.sha256(raw).hexdigest(),
                           lastConfirmed="prepared", pending=None, outcome="pending"))
    print("Status:", status_command(root))
    print("Start:", shlex.join([sys.executable, "-I", str(root / "manual-handoff.py"), "start", str(root)]))


def run_handoff(ao, root, state):
    try:
        require(ao.locate() == state["daemon"], "daemon changed since prepare")
        require(ao.checked(("version",), scoped=False).strip() == state["version"], "CLI version changed since prepare")
        raw = (root / "request.json").read_bytes()
        require(hashlib.sha256(raw).hexdigest() == state["requestSha256"], "approved request changed since prepare")
        handoff(ao, root, state, json.loads(raw))
    except (Stop, OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
        state.update(outcome="STOP", reason=str(error))
        write_state(root, state)
        return 1
    return 0


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("prepare", "start", "status", "_run"))
    parser.add_argument("path", type=Path)
    parser.add_argument("--lock-fd", type=int, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.action == "prepare":
        prepare(args.path)
        return 0

    root = args.path.resolve(strict=True)
    require(stat.S_IMODE(root.stat().st_mode) == 0o700, "run directory must be private")
    state = json.loads((root / "state.json").read_bytes())
    lock = root / "launch.lock"
    if args.action == "status":
        if state["outcome"] == "pending" and lock.exists():
            with lock.open() as handle:
                try:
                    fcntl.flock(handle, fcntl.LOCK_SH | fcntl.LOCK_NB)
                except BlockingIOError:
                    pass
                else:
                    state = json.loads((root / "state.json").read_bytes())
                    if state["outcome"] == "pending":
                        state.update(outcome="STOP", reason="runner ended without settling; inspect the original effect")
        print(json.dumps(state, ensure_ascii=False, indent=2))
        return int(state["outcome"] == "STOP")

    script = root / "manual-handoff.py"
    require(Path(__file__).resolve() == script, "use the prepared script")
    if args.action == "start":
        require(os.environ.get("AO_SESSION_ID") == state["oldId"], "start must run as the named old orchestrator")
        with lock.open("xb") as handle, (root / "runner.log").open("ab") as log:
            fd = handle.fileno()
            fcntl.flock(fd, fcntl.LOCK_EX)
            env = AO(state["executable"], state["projectId"]).env
            env.update(AO_RUN_FILE=state["daemon"]["runFile"], AO_DATA_DIR=state["daemon"]["dataDir"])
            subprocess.Popen([sys.executable, "-I", str(script), "_run", str(root), "--lock-fd", str(fd)],
                             cwd=root, env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                             start_new_session=True, pass_fds=(fd,))
        return 0

    require(args.lock_fd is not None and os.path.samestat(os.fstat(args.lock_fd), lock.stat()) and
            os.getsid(0) == os.getpid() and Path.cwd() == root, "runner requires the detached start entry")
    return run_handoff(AO(state["executable"], state["projectId"]), root, state)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (Stop, OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
        sys.exit(f"STOP: {error}")
