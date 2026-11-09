#!/usr/bin/env python3
"""Fixed Linux, project-local handoff protocol (RH-SCRIPT1).

Only CLI mutations are permitted. HTTP is read-only; SQLite is scoped query_only.
This module has no import-time I/O. Tests inject the CLI, clock and observations.
"""
import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import signal
import shutil
import sqlite3
import stat
import subprocess
import sys
import time
import urllib.parse
import urllib.request
import uuid

PROJECT = "agent-orchestrator"
SENSITIVE_SOURCE = re.compile(r"(^|/)(\.env(?:\..*)?|credentials(?:\.json)?|auth\.json|tokens?\.json|id_rsa|id_ed25519)(/|$)")
LIMITS = dict(read=30, notify=60, kill=60, exit=60, spawn=180, resume=180,
              workers=1800, old_2a=600, export=600, native=120, bootstrap=900,
              old_idle=900, new_idle=900, nonce=600, final=600)
OUTBOX = "尚未送達的report outbox不在本次conversation cut/delta範圍內，接手後才由AO既有持久outbox送達；不宣稱已匯出、已讀或已整合。"


class Stop(Exception):
    pass


class WindowElapsed(Exception):
    pass


def require(condition, reason):
    if not condition:
        raise Stop(reason)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def canonical(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


def decode(raw):
    def unique(pairs):
        obj = {}
        for key, value in pairs:
            require(key not in obj, "duplicate JSON key: " + key)
            obj[key] = value
        return obj
    try:
        return json.loads(raw, object_pairs_hook=unique,
                          parse_constant=lambda x: require(False, "nonfinite JSON: " + x))
    except (ValueError, UnicodeError) as exc:
        raise Stop("invalid JSON: " + str(exc)) from exc


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def identifier(value):
    require(isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", value),
            "invalid session identity")
    return value


def no_symlinks(path):
    path = Path(path).absolute()
    for item in (path, *path.parents):
        require(not item.is_symlink(), "symlink: " + str(item))
    return Path(os.path.abspath(path))


def private_read(path):
    path = no_symlinks(path)
    require(stat.S_IMODE(path.stat().st_mode) == 0o600 and path.is_file(), "expected private file: " + str(path))
    return path.read_bytes()


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def mkdir(path):
    path = no_symlinks(path)
    missing = []
    cursor = path
    while not cursor.exists():
        missing.append(cursor)
        cursor = cursor.parent
    for directory in reversed(missing):
        directory.mkdir(mode=0o700, exist_ok=True)
        sync_directory(directory.parent)
    require(stat.S_IMODE(path.stat().st_mode) == 0o700, "expected 0700 directory: " + str(path))
    return path


def publish(path, raw):
    """Immutable publication; a repeated identical write has no new effect."""
    path = no_symlinks(path)
    mkdir(path.parent)
    if path.exists():
        require(private_read(path) == raw, "conflicting immutable file: " + str(path))
        return
    temp = path.with_name("." + path.name + "." + uuid.uuid4().hex)
    try:
        with os.fdopen(os.open(temp, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), "wb") as out:
            out.write(raw)
            out.flush()
            os.fsync(out.fileno())
        os.link(temp, path)
    finally:
        temp.unlink(missing_ok=True)
        sync_directory(path.parent)


def confined(root, relative):
    require(isinstance(relative, str) and relative and not Path(relative).is_absolute(), "relative path required")
    path = no_symlinks(Path(root) / relative)
    require(path.is_relative_to(Path(root).absolute()) and ".." not in Path(relative).parts, "path escape")
    return path


def validate_target(target):
    require(isinstance(target, dict), "target object required")
    for key in ("harness", "account", "model", "effort", "approval_mode", "mode"):
        require(isinstance(target.get(key), str) and target[key].strip() == target[key] and target[key], "target missing: " + key)
    require(target["harness"] in ("claude-code", "codex") and target["mode"] == "chat", "only Claude/Codex Chat")
    require(target["approval_mode"] == "bypass-permissions", "target must explicitly select Full access")


def entries(snapshot):
    return snapshot["transcript"]["entries"]


def source_identity(source, message):
    if source["kind"] == "human-message":
        require(message.get("role") == "user" and message.get("origin") == "human" and
                not message.get("senderSessionId"), "not a structural human source")
    elif source["kind"] == "old-control-record":
        require(message.get("role") == "assistant", "not an old control record")
    else:
        require(source["kind"] == "structured-answer" and isinstance(source.get("ask_ref"), dict), "bad source kind/ask ref")
        require(message.get("origin") == "human" and message.get("role") == "user" and
                not message.get("senderSessionId"), "answer lacks human provenance")
        ask = source["ask_ref"]
        require(all(ask.get(k) is not None for k in ("id", "version", "answer_id")), "incomplete ask reference")


def source_message(source, raw):
    doc = decode(raw)
    transcript = doc.get("transcript", {})
    require(transcript.get("conversationId") == source["conversation_id"] and
            transcript.get("activeBranchId") == source["branch_id"], "source scope mismatch")
    matches = [x for x in transcript.get("entries", []) if x.get("id") == source["message_id"]]
    require(len(matches) == 1, "source message missing/ambiguous")
    message = matches[0]
    require(source.get("revision") == message.get("revision"), "source revision changed")
    require(digest(message.get("text", "").encode()) == source["message_text_sha256"], "source text hash mismatch")
    source_identity(source, message)
    return message


def validate_request(req, slot, old, target):
    require(stat.S_IMODE(no_symlinks(slot).stat().st_mode) == 0o700, "start slot must be 0700")
    require(req.get("schema") == "rh-start-request/1" and req.get("project_id") == PROJECT and req.get("old_id") == old,
            "start schema/project/old mismatch")
    require(str(uuid.UUID(req["request_id"])) == req["request_id"], "canonical UUID required")
    validate_target(target)
    require(req.get("target") == target, "target differs from external request")
    require(req.get("prepared_by", {}).get("session_id") == old and req["prepared_by"].get("turn_id"), "old preparation missing")
    sources = req.get("sources")
    require(isinstance(sources, list) and sources, "authority sources required")
    refs = {}
    for source in sources:
        ref = source["ref"]
        require(isinstance(ref, str) and ref and ref not in refs, "duplicate/empty source ref")
        require(source["kind"] in ("human-message", "structured-answer", "old-control-record"), "source kind")
        require(source["raw_relpath"].startswith("evidence/"), "source outside evidence")
        require(stat.S_IMODE(confined(slot, "evidence").stat().st_mode) == 0o700, "source evidence must be 0700")
        raw = private_read(confined(slot, source["raw_relpath"]))
        require(digest(raw) == source["raw_sha256"], "source bytes hash mismatch")
        source_message(source, raw)
        refs[ref] = source
    def references(values, human=False):
        require(isinstance(values, list), "references must be array")
        for ref in values:
            require(ref in refs and (not human or refs[ref]["kind"] != "old-control-record"), "invalid authority reference")
    authority = req["authority"]
    require(authority.get("old_verified_scope") == "three-step-handoff-this-project" and authority.get("source_refs"), "old scope verification missing")
    references(authority["source_refs"], True)
    require(authority.get("t0_source_ref") in authority["source_refs"], "T0 must refer to original authority")
    require("t0_created_at" in authority and isinstance(authority.get("t0_precision"), str), "T0/UNKNOWN required")
    t0_source = refs[authority["t0_source_ref"]]
    t0_message = source_message(t0_source, private_read(confined(slot, t0_source["raw_relpath"])))
    if t0_message.get("createdAt"):
        require(authority["t0_created_at"] == t0_message["createdAt"], "T0 differs from original human timestamp")
    workers = req["workers"]
    require(isinstance(workers, list), "worker index must be array")
    ids = set()
    for worker in workers:
        wid = identifier(worker["worker_id"])
        require(wid not in ids and wid != old, "duplicate worker/old in worker index")
        ids.add(wid)
        for key in ("stop_d_source_refs", "hold_source_refs", "task_refs", "pending_question_refs"):
            require(isinstance(worker.get(key), list), "worker index missing " + key)
        references(worker["stop_d_source_refs"])
        references(worker["hold_source_refs"])
        require(worker.get("hold_state") in ("held", "none", "unknown"), "invalid hold state")
        for key in ("reserved_stop_d",):
            value = worker.get(key)
            require(value is None or (isinstance(value, str) and re.fullmatch(r"D-[1-9][0-9]*", value)), "invalid reserved D")
            require(value is None or worker["stop_d_source_refs"], "D lacks original record")
        release = worker["release"]
        require(release.get("mode") in ("keep-held", "resume-original"), "invalid release mode")
        references(release["authority_refs"], True)
        references(release["resume_d_source_refs"])
        if release["mode"] == "resume-original":
            require(worker["hold_state"] == "none" and worker["task_refs"] and
                    release["authority_refs"] and release["resume_d_source_refs"] and
                    re.fullmatch(r"D-[1-9][0-9]*", release.get("reserved_resume_d") or ""), "release not authorized")
    index = req.get("workspace_index")
    require(isinstance(index, list) and index, "workspace_index required")
    expected = {old: "orchestrator", **{sid: "worker" for sid in ids}}
    paths, sessions = set(), set()
    for row in index:
        sid = identifier(row["session_id"])
        require(sid in expected and row.get("kind") == expected[sid] and sid not in sessions, "workspace session/kind mismatch")
        path = row.get("path")
        require(isinstance(path, str) and Path(path).is_absolute() and str(no_symlinks(path)) == path and path not in paths,
                "workspace path missing/aliased/shared")
        references(row.get("source_refs"))
        require(row["source_refs"], "workspace source references required")
        sessions.add(sid)
        paths.add(path)
    require(sessions == set(expected), "workspace index does not cover old/workers")
    workspace_sources(req, workspace_entry(req, old), slot, required=True)
    return req


def workspace_entry(request, sid):
    matches = [x for x in request.get("workspace_index", []) if x.get("session_id") == sid]
    require(len(matches) == 1, "missing/ambiguous bound workspace index: " + sid)
    return matches[0]


def workspace_sources(request, row, root, required=False):
    refs = row.get("source_refs")
    require(isinstance(refs, list) and refs, "workspace sources missing")
    positive = False
    for ref in refs:
        sources = [x for x in request.get("sources", []) if x["ref"] == ref]
        require(len(sources) == 1, "workspace source missing/ambiguous")
        source = sources[0]
        raw = private_read(confined(root, source["raw_relpath"]))
        require(digest(raw) == source["raw_sha256"], "workspace source bytes changed")
        message = source_message(source, raw)
        text = message.get("text", "")
        if (source["kind"] == "old-control-record" and message.get("turnId") == request["prepared_by"]["turn_id"] and
                row["session_id"] in text and (row["path"] in text or json.dumps(row["path"])[1:-1] in text)):
            positive = True
    require(not required or positive, "nonstandard workspace lacks old preparing-turn session/path evidence")


def location_info(cli):
    data = no_symlinks(cli.data_dir)
    response = cli.get("projects/" + PROJECT)
    project = response.get("project", {})
    require(response.get("status", "ok") == "ok" and project.get("id") == PROJECT and project.get("path"),
            "project location unavailable")
    config = project.get("config", {}).get("orchestratorHandoff", {})
    shared_setting = config.get("sharedDir") or "shared"
    expected = confined(data / "projects" / PROJECT / "orchestrator", shared_setting)
    shared = no_symlinks(cli.documents()["sharedDir"])
    require(shared == expected, "sharedDir contradicts fixed CLI data root/project configuration")
    return {"data_dir": str(data), "managed_root": str(data / "worktrees"),
            "project_path": str(no_symlinks(project["path"])), "shared_dir": str(shared), "shared_setting": shared_setting}


def old_settings(snapshot):
    turns = [t for t in snapshot["transcript"].get("turns", []) if t.get("state") == "completed"]
    turn = max(turns, key=lambda t: t.get("completedAt") or "", default={})
    observed = turn.get("agentSettings") or {}
    settings = observed.get("settings") or {}
    result = {"turn_id": turn.get("id"), "response_sha256": digest(canonical(snapshot))}
    for key in ("model", "effort"):
        value = settings.get(key)
        result[key] = {"value": value if observed.get("state") == "known" and value else "UNKNOWN",
                       "reason": observed.get("reason") or "absent or empty completed-turn setting"}
    for key in ("approval_mode", "fast"):
        result[key] = {"value": "UNKNOWN", "reason": "not exposed by the completed-turn settings source"}
    return result


def helper_directory(root, action, sid=None):
    if action == "worker-receipt":
        return root / "workers" / identifier(sid)
    return root / "old" if action == "old-receipt" else root / "bootstrap/new"


def helper_command(root, action, sid=None, path=None, offset=None):
    require(action in ("worker-receipt", "old-receipt", "bootstrap", "inspect", "finalize"), "unknown helper action")
    draft = helper_directory(root, action, sid) / "draft"
    command = "umask 077; export TMPDIR=" + shlex.quote(str(draft)) + "; python3 -I -B " + shlex.quote(str(root / "handoff.py"))
    command += " " + action + " --run " + root.name
    if action == "inspect":
        require(isinstance(path, str) and re.fullmatch(r"[\w./:@%+,=-]+", path) and type(offset) is int and offset >= 0,
                "inspect requires literal argv")
        confined(root, path)
        command += " --path " + path + " --offset " + str(offset)
    return command


def helper_identity(command, root, sid):
    if not command.startswith("umask 077; "):
        try:
            wrapper = shlex.split(command)
        except ValueError:
            return None
        if not (len(wrapper) == 3 and re.fullmatch(r"(?:/(?:usr/)?bin/)?(?:bash|sh|zsh)", wrapper[0]) and
                wrapper[1] in ("-c", "-lc") and shlex.join(wrapper) == command):
            return None
        command = wrapper[2]
    for action in ("worker-receipt", "old-receipt", "bootstrap", "finalize"):
        if command == helper_command(root, action, sid):
            return action, {}
    prefix = helper_command(root, "bootstrap", sid).removesuffix(" bootstrap --run " + root.name)
    literal = re.fullmatch(re.escape(prefix + " inspect --run " + root.name) + r" --path ([\w./:@%+,=-]+) --offset ([0-9]+)", command)
    if literal:
        confined(root, literal[1])
        return "inspect", {"path": literal[1], "offset": int(literal[2])}
    return None


def helper_result(run, action, sid, arguments):
    if action == "inspect":
        raw = private_read(confined(run.root, arguments["path"]))
        content, offset = raw.decode(), arguments["offset"]
        end = min(offset + 4000, len(content))
        return dict(path=arguments["path"], sha256=digest(raw), total_chars=len(content), offset=offset,
                    next_offset=end if end < len(content) else None, text=content[offset:end])
    name = {"worker-receipt": "stop.json", "old-receipt": "phase-2a.json", "bootstrap": "receipt.json", "finalize": "final.json"}[action]
    result = decode(private_read(helper_directory(run.root, action, sid) / name))
    require(result.get("run_id") == run.run_id, "helper output run mismatch")
    if action == "worker-receipt":
        validate_stop(result, run.run_id, sid, helper_directory(run.root, action, sid))
    elif action == "old-receipt":
        require(result.get("old_id") == sid and sid == run.old, "old helper identity mismatch")
        plan = decode(private_read(run.root / "old/preservation.json"))
        require(plan.get("schema") == "rh-old-preservation/2" and plan.get("evidence_turn_id") == result.get("evidence_turn_id"),
                "old helper plan mismatch")
        hash_reference(result["managed_reply"])
        reference = result["handoff"]
        require(no_symlinks(reference["path"]).is_relative_to(run.state["location"]["shared_dir"]) and
                digest(read_regular(reference["path"])) == reference["sha256"], "managed original changed")
    else:
        require(sid == run.state.get("new_id") and result.get("new_id") == sid, "new helper identity mismatch")
        if action == "bootstrap":
            require(result.get("schema") == "rh-new-receipt/1", "bootstrap schema mismatch")
            hash_reference(result["managed_reply"])
            reference = result["managed_receipt"]
            require(no_symlinks(reference["path"]).is_relative_to(run.state["location"]["shared_dir"]) and
                    digest(read_regular(reference["path"])) == reference["sha256"], "managed receipt original changed")
    return result


def known_helper(run, sid, entry):
    detail = entry.get("detail") or {}
    identity = helper_identity(detail.get("rawCommand") or detail.get("command", ""), run.root, sid)
    if not identity:
        return False
    require(detail.get("exitCode") == 0 and not detail.get("outputTruncated"), "helper output not positively complete")
    expected = helper_result(run, identity[0], sid, identity[1])
    output = detail.get("output", "")
    try:
        actual = decode(output)
    except Stop:
        require(detail.get("outputMayBePartial") is True, "helper stdout invalid")
    else:
        require(actual == expected, "helper stdout differs from fixed artifact")
    return True


def operation_handles(entry):
    """Only tool-result envelopes; never command/input/read contents or JSON descendants."""
    detail = entry.get("detail") or {}
    if detail.get("toolKind") == "read" or detail.get("toolName") in ("Read", "read_file"):
        return []
    result = detail.get("result")
    if result is None:
        result = detail.get("output")
    if isinstance(result, dict) and isinstance(result.get("content"), list):
        result = "\n".join(x.get("text", "") for x in result["content"] if x.get("type") == "text")
    if isinstance(result, str):
        try:
            result = json.loads(result)
        except ValueError:
            pass
    if isinstance(result, dict) and result.get("truncated") is True:
        return [{"reference": "UNKNOWN", "state": "UNKNOWN"}]
    name = detail.get("toolName", "").rsplit(".", 1)[-1]
    if name in ("exec_command", "write_stdin") and isinstance(result, dict) and result.get("session_id") is not None:
        return [{"reference": str(result["session_id"]), "state": "running" if result.get("exit_code") is None else "completed"}]
    if isinstance(result, dict) and (result.get("type") in ("async-operation", "job") or "job_id" in result):
        reference = result.get("reference") or result.get("job_id")
        return [{"reference": str(reference or "UNKNOWN"), "state": result.get("state", "UNKNOWN") if reference else "UNKNOWN"}]
    # A transport-level textual continuation is not a normal JSON key or an input quotation.
    text = result if isinstance(result, str) else detail.get("output", "")
    match = re.match(r"^(?:Script running with (?:cell|session) ID|Process running with session ID|Job running with ID)\s+([A-Za-z0-9._:-]+)", text)
    return [{"reference": match[1], "state": "UNKNOWN"}] if match else []


def operation_terminal(entry, handle, receipts):
    for item in receipts:
        if item.get("reference") != handle["reference"] or item.get("activity_id") != entry.get("id"):
            continue
        require(item.get("state") in ("completed", "cancelled", "failed"), "operation not terminal")
        raw = hash_reference({"path": item["evidence_path"], "sha256": item["sha256"]})
        proof = decode(raw)
        require(proof.get("reference") == handle["reference"] and proof.get("state") == item["state"] and
                proof.get("activity_id") == entry.get("id"), "terminal receipt belongs to another operation")
        return True
    return False


def quiescent(snapshot, settled_receipts=(), run=None):
    session, transcript = snapshot["session"], snapshot["transcript"]
    if session.get("activity", {}).get("state") not in ("idle", "exited"):
        return False
    if transcript.get("incomplete") or transcript.get("truncatedFields"):
        return False
    if any(t.get("state") in ("queued", "running") for t in transcript.get("turns", [])):
        return False
    return all(activity_quiescent(entry, settled_receipts, run, session.get("id")) for entry in transcript.get("entries", []))


def activity_quiescent(entry, receipts=(), run=None, sid=None):
    if entry.get("kind") != "activity":
        return True
    if entry.get("status") not in ("completed", "resolved", "cancelled", "failed", "recovered"):
        return False
    detail = entry.get("detail") or {}
    if entry.get("activityKind") == "command" and detail.get("outputTruncated"):
        return False
    if run and known_helper(run, sid, entry):
        return True
    if entry.get("activityKind") == "command" and detail.get("exitCode") is None:
        return False
    return all(handle["reference"] != "UNKNOWN" and (handle["state"] in ("completed", "cancelled", "failed") or operation_terminal(entry, handle, receipts))
               for handle in operation_handles(entry))


def cli_environment(old, data_dir, run_file, temp):
    env = {key: os.environ[key] for key in ("PATH", "HOME", "LANG", "LC_ALL", "USER") if key in os.environ}
    env.update(AO_SESSION_ID=old, AO_PROJECT_ID=PROJECT, AO_DATA_DIR=str(data_dir),
               AO_RUN_FILE=str(run_file), TMPDIR=str(temp))
    return env


class CLI:
    def __init__(self, executable, old, data_dir, run_file, temp):
        self.executable = no_symlinks(executable)
        self.sha256 = digest(self.executable.read_bytes())
        self.env = cli_environment(old, data_dir, run_file, temp)
        self.data_dir, self.run_file = Path(data_dir), Path(run_file)
        run = decode(self.run_file.read_bytes())
        port = run["port"]
        require(type(port) is int and 0 < port < 65536, "invalid daemon port")
        self.base = "http://127.0.0.1:" + str(port) + "/api/v1/"

    def capture(self, label, raw, metadata):
        if not getattr(self, "capture_root", None):
            self.__dict__.setdefault("pending_wire", []).append((label, raw, metadata))
            return
        path = self.capture_root / (label + "-" + uuid.uuid4().hex)
        publish(path.with_suffix(".raw"), raw)
        publish(path.with_suffix(".json"), canonical(dict(metadata, sha256=digest(raw), raw_path=str(path.with_suffix(".raw")))))

    def call(self, args, timeout=30, deadline=None, on_dispatch=None):
        require(digest(self.executable.read_bytes()) == self.sha256, "qualified CLI changed")
        dispatched = time.monotonic()
        if deadline is not None and dispatched > deadline:
            raise WindowElapsed()
        if on_dispatch:
            on_dispatch()
        dispatched = time.monotonic()
        if deadline is not None and dispatched > deadline:
            raise WindowElapsed()  # No subprocess: caller can record not-dispatched.
        self.last_dispatched = dispatched
        try:
            result = subprocess.run([str(self.executable), *args], env=self.env, stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, check=False)
        except subprocess.TimeoutExpired as exc:
            self.capture("cli-timeout", exc.stdout or b"", {"argv": args, "timeout": timeout,
                         "stderr": (exc.stderr or b"").decode(errors="replace"), "effect": "UNKNOWN"})
            raise
        self.last_received = time.monotonic()
        self.last_raw = result.stdout
        self.last_stderr = result.stderr.decode(errors="replace")
        self.capture("cli", result.stdout, {"argv": args, "returncode": result.returncode,
                     "stderr": result.stderr.decode(errors="replace"), "dispatched_monotonic": dispatched,
                     "received_monotonic": self.last_received})
        require(result.returncode == 0, "CLI error: " + result.stderr.decode(errors="replace"))
        return result.stdout

    def read(self, args):
        return decode(self.call([*args, "--json"]))

    def get(self, path):
        require(not path.startswith(("/", "http")) and ".." not in path, "invalid API read path")
        # No redirects, proxies or non-loopback endpoints; never HTTP mutations.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                raise Stop("API redirect refused")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        with opener.open(self.base + path, timeout=30) as response:
            raw = response.read()
            self.last_received = time.monotonic()
            self.capture("http-get", raw, {"path": path, "received_at": now()})
            return decode(raw)

    def session(self, sid):
        return self.read(["session", "get", identifier(sid), "--transcript", "500", "--cap", "0"])

    def sessions(self):
        response = self.read(["session", "ls", "--all", "--include-terminated", "--project", PROJECT])
        require(isinstance(response, dict) and isinstance(response.get("data"), list) and
                isinstance(response.get("meta"), dict), "invalid session list data/meta envelope")
        for field in ("hiddenTerminatedCount", "hiddenOrchestratorCount"):
            count = response["meta"].get(field)
            require(type(count) is int and count == 0, "session list hides sessions: " + field)
        for row in response["data"]:
            require(isinstance(row, dict) and all(isinstance(row.get(k), str) and row[k]
                    for k in ("id", "projectId")) and row.get("role") in ("worker", "orchestrator") and
                    type(row.get("isTerminated")) is bool, "invalid session list row identity/role")
        return response["data"]

    def documents(self):
        return self.read(["project", "documents", "info", "--project", PROJECT])["documents"]


def git_operation_facts(workspace):
    facts = {}
    for name in ("MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "MERGE_AUTOSTASH", "AUTO_MERGE", "CHERRY_PICK_HEAD", "REVERT_HEAD",
                 "REBASE_HEAD", "rebase-merge", "rebase-apply", "sequencer", "BISECT_LOG", "BISECT_START", "BISECT_NAMES", "BISECT_EXPECTED_REV", "BISECT_HEAD"):
        path = no_symlinks(old_git(["rev-parse", "--path-format=absolute", "--git-path", name], workspace=workspace).decode().strip())
        rows = []
        if path.exists():
            for item in [path, *sorted(path.rglob("*"))] if path.is_dir() else [path]:
                no_symlinks(item)
                require(item.is_file() or item.is_dir(), "gitlink operation metadata UNKNOWN")
                row = dict(path=str(item.relative_to(path)), mode=stat.S_IMODE(item.stat().st_mode), directory=item.is_dir())
                if item.is_file():
                    row.update(sha256=digest(read_regular(item)), bytes=item.stat().st_size)
                rows.append(row)
        facts[name] = dict(path=str(path), entries=rows)
    return facts


def gitlink_facts(workspace, role="worker", parent_common=None, old_metadata=None):
    workspace = no_symlinks(workspace)
    git = lambda *args: old_git(args, workspace=workspace)
    links = {}
    for record in filter(None, git("ls-files", "--stage", "-z").split(b"\0")):
        header, name = record.split(b"\t", 1)
        mode, oid, stage = header.decode().split()
        if mode != "160000":
            continue
        relative = os.fsdecode(name)
        require(stage == "0" and relative not in links, "gitlink index conflict: " + relative)
        path = confined(workspace, relative)
        fact = {"path": relative, "mode": mode, "stage": 0, "index_oid": oid}
        if not path.exists() or (path.is_dir() and not list(path.iterdir())):
            fact.update(state="uninitialized", presence="empty" if path.exists() else "absent")
        else:
            require(path.is_dir(), "gitlink is not a directory")
            sub = lambda *args: old_git(args, workspace=path)
            require(sub("rev-parse", "--show-toplevel").decode().strip() == str(path), "gitlink is not its own repository")
            common = no_symlinks(sub("rev-parse", "--path-format=absolute", "--git-common-dir").decode().strip())
            metadata = no_symlinks(sub("rev-parse", "--absolute-git-dir").decode().strip())
            require(sub("rev-parse", "--is-shallow-repository").strip() == b"false" and
                    not (common / "objects/info/alternates").exists() and not (common / "info/grafts").exists(),
                    "gitlink object protection UNKNOWN")
            no_symlinks(common / "objects")
            require(sub("rev-parse", "HEAD").decode().strip() == oid, "gitlink HEAD/index mismatch")
            operations = git_operation_facts(path)
            require(not any(x["entries"] for x in operations.values()), "gitlink unfinished operation metadata")
            status = sub("status", "--porcelain=v1", "--untracked-files=all", "--ignored")
            index = sub("ls-files", "--stage", "-v", "-z")
            require(not status and all(x.startswith(b"H ") and not x[2:].startswith(b"160000 ")
                    for x in index.split(b"\0") if x), "dirty/nested/hidden gitlink content")
            fact.update(state="initialized-clean", common=str(common), git_dir=str(metadata),
                        common_identity=[common.stat().st_dev, common.stat().st_ino],
                        git_identity=[metadata.stat().st_dev, metadata.stat().st_ino],
                        path_identity=[path.stat().st_dev, path.stat().st_ino], head=oid,
                        status=status.hex(), index=index.hex(), operations=operations,
                        gitfile=read_regular(path / ".git").hex() if (path / ".git").is_file() else None)
            if role == "old":
                require(parent_common and common.is_relative_to(parent_common) and
                        not common.is_relative_to(workspace) and not common.is_relative_to(old_metadata) and
                        common.stat().st_dev == Path(parent_common).stat().st_dev,
                        "gitlink objects overlap old reclaim scope or external repository")
                protected = []
                for line in old_git(["for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"], common=common).decode().splitlines():
                    ref, tip = line.split()
                    probe = subprocess.run(["git", "--no-optional-locks", "--git-dir", str(common), "merge-base", "--is-ancestor", oid, tip],
                                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
                    require(probe.returncode in (0, 1), "gitlink ref ancestry UNKNOWN")
                    if probe.returncode == 0:
                        protected.append((ref, tip))
                require(protected, "gitlink lacks existing protected ref")
                ref, tip = sorted(protected)[0]
                objects = old_git(["rev-list", "--objects", "--missing=print", tip], common=common)
                require(not any(x.startswith(b"?") for x in objects.splitlines()), "gitlink objects missing")
                fact.update(protected_ref=ref, ref_tip=tip, objects_sha256=digest(objects))
        links[relative] = fact
    return links


def verify_gitlink_protection(links):
    for fact in links.values():
        if fact["state"] == "uninitialized":
            continue
        common = no_symlinks(fact["common"])
        require([common.stat().st_dev, common.stat().st_ino] == fact["common_identity"], "gitlink common identity changed")
        require(old_git(["rev-parse", "--verify", fact["protected_ref"]], common=common).decode().strip() == fact["ref_tip"],
                "gitlink protected ref changed")
        require(digest(old_git(["rev-list", "--objects", "--missing=print", fact["ref_tip"]], common=common)) == fact["objects_sha256"],
                "gitlink protected objects changed")


def workspace_git_inputs(workspace):
    """Inventory names before preserving content; only unchanged committed templates qualify."""
    git = lambda *args: old_git(args, workspace=workspace)
    head = git("rev-parse", "HEAD").decode().strip()
    tree_raw = git("ls-tree", "-r", "-z", "--full-tree", head)
    tree = {}
    for record in filter(None, tree_raw.split(b"\0")):
        header, name = record.split(b"\t", 1)
        mode, kind, oid = header.decode().split()
        tree[name] = (mode, kind, oid)
    index_raw = git("ls-files", "--stage", "-v", "-z")
    index = {}
    for record in filter(None, index_raw.split(b"\0")):
        header, name = record.split(b"\t", 1)
        index.setdefault(name, []).append(tuple(header.decode().split()))
    staged_names = git("diff", "--cached", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv",
                       "--ita-invisible-in-index", "-z", head)
    others = git("ls-files", "--others", "--exclude-standard", "-z")
    ignored = git("ls-files", "--others", "--ignored", "--exclude-standard", "-z")
    files = set(index) | (set(others.split(b"\0")) - {b""})
    names = set(tree) | files | (set(ignored.split(b"\0")) - {b""})
    templates = {}
    for name in sorted(names):
        relative = os.fsdecode(name)
        if not SENSITIVE_SOURCE.search(relative):
            continue
        reason = "sensitive workspace content cannot be preserved: " + relative
        parts = Path(relative).parts
        require(parts[-1] == ".env.example" and not SENSITIVE_SOURCE.search("/".join(parts[:-1])), reason)
        entry = tree.get(name)
        require(entry and entry[0] in ("100644", "100755") and entry[1] == "blob", reason)
        mode, _, oid = entry
        require(index.get(name) == [("H", mode, oid, "0")], reason)
        require(name not in staged_names.split(b"\0"), reason)
        source = confined(workspace, relative)
        require(source.is_file(), reason)
        info = source.stat()
        require(bool(info.st_mode & 0o111) == (mode == "100755"), reason)
        raw = read_regular(source)
        require(raw == git("cat-file", "blob", oid), reason)
        templates[relative] = dict(raw=raw, mode=stat.S_IMODE(info.st_mode),
                                   identity=(info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns))
    return dict(head=head, tree=tree_raw, index=index_raw, staged_names=staged_names, others=others, ignored=ignored,
                files=files, templates=templates,
                patch_paths=sorted(os.fsdecode(n) for n in set(tree) | files if not SENSITIVE_SOURCE.search(os.fsdecode(n))))


def workspace_patch(workspace, inputs, cached=False):
    if not inputs["patch_paths"]:
        return b""
    raw = old_git(["--literal-pathspecs", "diff", *(["--cached"] if cached else []), "--raw", "-z", "--patch",
                   "--binary", "--no-renames", "--no-ext-diff", "--no-textconv", "--", *inputs["patch_paths"]], workspace=workspace)
    if not raw:
        return b""
    # Read paths and patch from the same diff queue: a literal file path can still match new descendants.
    metadata, separator, patch = raw.partition(b"\0\0")
    records = metadata.split(b"\0")
    approved = {os.fsencode(path) for path in inputs["patch_paths"]}
    require(separator and len(records) % 2 == 0 and
            all(records[i].startswith(b":") and records[i + 1] in approved and
                not SENSITIVE_SOURCE.search(os.fsdecode(records[i + 1])) for i in range(0, len(records), 2)),
            "workspace patch paths drifted outside approved files")
    return patch


def git_snapshot(workspace, destination):
    """Read-only Git plus preserved bytes, including staged and dirty content."""
    path = no_symlinks(workspace)
    require(path.is_dir(), "registered workspace missing")
    def git(*args):
        result = subprocess.run(["git", "--no-optional-locks", "-c", "core.fsmonitor=false", "-C", str(path), *args],
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=30, check=False)
        require(result.returncode == 0, "workspace snapshot unavailable: " + result.stderr.decode(errors="replace"))
        return result.stdout
    inputs = workspace_git_inputs(path)
    mkdir(destination)
    branch = git("rev-parse", "--abbrev-ref", "HEAD").decode().strip()
    links = gitlink_facts(path)
    values = {"schema": "rh-workspace-snapshot/2", "gitlinks": links, "head": git("rev-parse", "HEAD").decode().strip(),
              "branch": None if branch == "HEAD" else branch, "detached": branch == "HEAD",
              "status": git("status", "--porcelain=v1", "-z", "--untracked-files=all").hex(),
              "index": git("ls-files", "--stage", "-z").hex(), "files": {}}
    for name, cached in (("index.patch", True), ("worktree.patch", False)):
        raw = workspace_patch(path, inputs, cached)
        publish(Path(destination) / name, raw)
        values[name] = digest(raw)
    files = inputs["files"]
    for raw_name in sorted(files):
        name = os.fsdecode(raw_name)
        source = confined(path, name)
        if name in links:
            values["files"][name] = {"gitlink": links[name]}
            continue
        if not source.exists():
            values["files"][name] = {"missing": True}
            continue
        require(source.is_file(), "unsupported workspace object: " + name)
        template = inputs["templates"].get(name)
        raw = template["raw"] if template else source.read_bytes()
        sha = digest(raw)
        publish(Path(destination) / "objects" / sha, raw)
        values["files"][name] = {"sha256": sha, "mode": template["mode"] if template else stat.S_IMODE(source.stat().st_mode)}
    require(workspace_git_inputs(path) == inputs, "workspace source drift during snapshot")
    raw = canonical(values)
    publish(Path(destination) / "snapshot.json", raw)
    return {"path": str(Path(destination) / "snapshot.json"), "sha256": digest(raw),
            "branch": values["branch"], "head": values["head"]}


def old_git(args, workspace=None, common=None):
    location = ["-C", str(workspace)] if workspace else ["--git-dir", str(common)]
    result = subprocess.run(["git", "--no-optional-locks", "-c", "core.fsmonitor=false", *location, *args],
                            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=30, check=False)
    require(result.returncode == 0, "old Git protection unavailable: " + result.stderr.decode(errors="replace"))
    return result.stdout


def old_environment(run, workspace, common):
    response = run.cli.get("projects/" + PROJECT)
    project = response.get("project", {})
    require(response.get("status") == "ok" and project.get("id") == PROJECT and
            isinstance(project.get("config"), dict), "project cleanup configuration UNKNOWN")
    hooks = project["config"].get("preRemove", [])
    # Arbitrary hooks cannot be proved harmless by parsing their names or text.
    require(hooks == [], "PreRemove impact UNKNOWN; no kill")
    documents = run.cli.documents()
    paths = [run.root, Path(documents["sharedDir"]), common, run.cli.data_dir / "artifacts" / run.old]
    fixed_paths = list(paths)
    paths += [Path(x) for x in run.state.get("preservation_paths", [])]
    for path in paths:
        path = no_symlinks(path)
        require(not path.is_relative_to(workspace) and not workspace.is_relative_to(path),
                "protected material overlaps reclaim scope: " + str(path))
    return {"pre_remove": hooks, "protected_paths": sorted(str(p) for p in fixed_paths)}


def old_git_state(run, plan, workspace=None):
    workspace = no_symlinks(workspace or run.workspace(run.old))
    git = lambda *args: old_git(args, workspace=workspace)
    inputs = workspace_git_inputs(workspace)
    common = no_symlinks(git("rev-parse", "--path-format=absolute", "--git-common-dir").decode().strip())
    metadata = no_symlinks(git("rev-parse", "--absolute-git-dir").decode().strip())
    require(common.is_dir() and metadata != common, "old must be a linked worktree with external common repository")
    require(not (common / "objects/info/alternates").exists() and not (common / "info/grafts").exists(),
            "original ref object protection depends on unresolved alternates/grafts")
    no_symlinks(common / "objects")
    environment = old_environment(run, workspace, common)
    head = git("rev-parse", "HEAD").decode().strip()
    ref = plan.get("original_ref", "")
    require(isinstance(ref, str) and ref.startswith("refs/heads/") and ".." not in ref, "original ref required")
    tip = old_git(["rev-parse", "--verify", ref], common=common).decode().strip()
    old_git(["merge-base", "--is-ancestor", head, tip], common=common)
    registration = old_git(["worktree", "list", "--porcelain", "-z"], common=common)
    require(b"worktree " + os.fsencode(workspace) + b"\0" in registration, "old registration missing")
    index = git("ls-files", "--stage", "-v", "-z")
    require(all(x.startswith(b"H ") for x in index.split(b"\0") if x),
            "old index flags hide content or represent unresolved checkout state; no kill")
    links = gitlink_facts(workspace, "old", common, metadata)
    tracked = set(git("ls-files", "-z").split(b"\0")) - {b""}
    untracked = set(git("ls-files", "--others", "--exclude-standard", "-z").split(b"\0")) - {b""}
    ignored = set(git("ls-files", "--others", "--ignored", "--exclude-standard", "-z").split(b"\0")) - {b""}
    rules = plan.get("classification")
    require(isinstance(rules, list), "old classification required")
    used, classified, tree = set(), {}, {}
    for directory, dirs, names in os.walk(workspace, followlinks=False):
        # Only a positively classified gitlink subtree is excluded from the walker.
        for name in list(dirs):
            relative = str((Path(directory) / name).relative_to(workspace))
            if relative in links:
                tree[relative] = {"gitlink": links[relative]}
                dirs.remove(name)
        if Path(directory) == workspace:
            dirs[:] = [x for x in dirs if x != ".git"]
            names = [x for x in names if x != ".git"]
        else:
            require(".git" not in dirs + names, "nested repository dependencies unresolved")
        for name in sorted(dirs + names):
            path = no_symlinks(Path(directory) / name)
            relative = str(path.relative_to(workspace))
            info = path.stat()
            require(path.is_file() or path.is_dir(), "unsupported old workspace object")
            tree[relative] = {"directory": path.is_dir(), "mode": stat.S_IMODE(info.st_mode)}
            if path.is_file():
                tree[relative].update(size=info.st_size, mtime_ns=info.st_mtime_ns)
            if os.fsencode(relative) in tracked:
                if relative in inputs["templates"]:
                    tree[relative]["sha256"] = digest(inputs["templates"][relative]["raw"])
                continue
            matches = [(i, rule) for i, rule in enumerate(rules) if relative == rule.get("path") or
                       (rule.get("scope") == "directory" and relative.startswith(str(rule.get("path")) + "/"))]
            # Parent directories containing only tracked files need no disposal classification.
            if path.is_dir() and not matches:
                require(any(os.fsdecode(x).startswith(relative + "/") for x in tracked | untracked | ignored),
                        "unclassified empty directory: " + relative)
                continue
            require(len(matches) == 1, "unclosed/ambiguous old classification: " + relative)
            i, rule = matches[0]
            used.add(i)
            require(isinstance(rule.get("basis"), str) and rule["basis"].strip(), "classification lacks explicit basis")
            disposition = rule.get("disposition")
            require(disposition in ("copy", "rebuildable", "discardable"), "classification disposition UNKNOWN")
            item = dict(tree[relative], disposition=disposition, basis=rule["basis"],
                        kind="directory" if path.is_dir() else "ignored" if os.fsencode(relative) in ignored else "untracked")
            if disposition == "copy":
                require(path.is_file() and rule.get("path") == relative, "necessary copies must enumerate exact regular files")
                require(not SENSITIVE_SOURCE.search(relative), "credential copy refused")
                copy = rule.get("copy", {})
                saved = no_symlinks(copy.get("path", ""))
                require(saved.is_relative_to(run.root / "old/saved"), "old copy outside fixed private saved directory")
                sha = digest(read_regular(path))
                require(digest(private_read(saved)) == sha == copy.get("sha256"), "necessary old copy hash mismatch")
                item.update(copy=dict(copy, original_mode=stat.S_IMODE(info.st_mode), relative_path=relative), sha256=sha)
            classified[relative] = item
    require(used == set(range(len(rules))), "classification contains nonexistent/unmatched paths")
    # Preserve only the required linked-worktree Git state, never the common history tree.
    meta = {}
    for directory, dirs, names in os.walk(metadata):
        for name in names:
            path = no_symlinks(Path(directory) / name)
            relative = str(path.relative_to(metadata))
            require(relative in ("HEAD", "index", "commondir", "gitdir", "ORIG_HEAD", "logs/HEAD"),
                    "per-worktree Git dependency unresolved: " + relative)
            meta[relative] = {"bytes": read_regular(path).hex(), "mode": stat.S_IMODE(path.stat().st_mode)}
    staged_patch = workspace_patch(workspace, inputs, True)
    dirty_patch = workspace_patch(workspace, inputs)
    require(workspace_git_inputs(workspace) == inputs, "workspace source drift during old preservation")
    return {"workspace": str(workspace), "common": str(common), "metadata_path": str(metadata),
            "head": head, "original_ref": ref, "ref_tip": tip, "environment": environment,
            "status": git("status", "--porcelain=v1", "-z", "--untracked-files=all").hex(),
            "index": index.hex(), "staged_patch": staged_patch.hex(), "dirty_patch": dirty_patch.hex(),
            "metadata": meta, "gitfile": read_regular(workspace / ".git").hex(), "gitlinks": links, "tree": tree, "classification": classified,
            "plan_sha256": digest(private_read(run.root / "old/preservation.json"))}


def capture_old(run):
    plan = decode(private_read(run.root / "old/preservation.json"))
    require(plan.get("schema") == "rh-old-preservation/2" and plan.get("run_id") == run.run_id and
            plan.get("old_id") == run.old and plan.get("evidence_turn_id") == run.state["phase_2a"]["evidence_turn_id"],
            "old preservation producer/turn mismatch")
    evidence = old_git_state(run, plan)
    path = run.root / "old/preservation-evidence.json"
    publish(path, canonical(evidence))
    reference = {"path": str(path), "sha256": digest(canonical(evidence))}
    run.state["old_preservation"] = reference
    run.save()
    return reference


def verify_old(run, postkill=False):
    ref = run.state["old_preservation"]
    raw = private_read(ref["path"])
    require(digest(raw) == ref["sha256"], "old preservation evidence changed")
    proof = decode(raw)
    plan = decode(private_read(run.root / "old/preservation.json"))
    if not postkill:
        require(old_git_state(run, plan) == proof, "old preservation drift before kill")
        return
    result = decode(private_read(run.root / "control/kill/result.json"))
    output = result.get("stdout", "").strip()
    require(result.get("sha256") == digest(result.get("stdout", "").encode()) and
            output in ("session " + run.old + " killed", "session " + run.old + " killed (workspace preserved)"),
            "kill cleanup pending/UNKNOWN or missing original receipt; no spawn")
    require(run.cli.read(["session", "get", run.old])["session"].get("isTerminated") is True, "old not terminated")
    workspace, common = Path(proof["workspace"]), Path(proof["common"])
    require(old_environment(run, workspace, common) == proof["environment"], "cleanup protection changed")
    require(old_git(["rev-parse", "--verify", proof["original_ref"]], common=common).decode().strip() == proof["ref_tip"],
            "original ref drift after kill")
    old_git(["merge-base", "--is-ancestor", proof["head"], proof["ref_tip"]], common=common)
    verify_gitlink_protection(proof["gitlinks"])
    for item in proof["classification"].values():
        if item["disposition"] == "copy":
            require(digest(private_read(item["copy"]["path"])) == item["sha256"], "necessary old copy changed after kill")
    # Verify all immutable export bytes and required copies again, independently of terminated.
    for kind in ("manifest", "delta"):
        reference = run.state[kind]
        manifest_raw = private_read(reference["path"])
        require(digest(manifest_raw) == reference["sha256"], "export manifest changed")
        manifest = decode(manifest_raw)
        for item in [manifest["raw"], *manifest["projection"]["pages"], *manifest["dependencies"]]:
            require(digest(private_read(item["path"])) == item["sha256"], "necessary exported material changed")
    require(digest(read_regular(run.state["handoff"]["path"])) == run.state["handoff"]["sha256"], "shared original changed")
    disposition = "clean-reclaim-allowed"
    if proof["status"]:
        require(output.endswith("(workspace preserved)"), "dirty preservation not confirmed by kill")
        actual = old_git_state(run, plan)
        expected = decode(canonical(proof))
        # Normal kill's Git status may refresh index stat-cache bytes. Original
        # raw index stays saved; staged entries/flags and every other fact must match.
        for value in (actual, expected):
            value["metadata"].pop("index", None)
        require(actual == expected, "dirty original path/registration/content changed after kill")
        disposition = "dirty-original-verified"
    run.state["old_preservation_result"] = {"terminated": True, "workspace": disposition,
        "path_exists_at_observation": workspace.exists(), "original_ref": proof["original_ref"],
        "ref_tip": proof["ref_tip"], "materials_verified": True, "observed_at": now()}
    run.save()


class Run:
    def __init__(self, root, cli, clock=time.monotonic, sleep=time.sleep):
        self.root, self.cli, self.clock, self.sleep = Path(root), cli, clock, sleep
        self.request = decode(private_read(self.root / "start.json"))
        self.state = decode(private_read(self.root / "state.json"))
        require(digest(private_read(self.root / "start.json")) == self.state["start_sha256"], "bound start changed")
        self.old, self.target, self.run_id = self.request["old_id"], self.request["target"], self.request["request_id"]
        self.cli.capture_root = mkdir(self.root / "wire")
        for label, raw, metadata in getattr(self.cli, "pending_wire", []):
            self.cli.capture(label, raw, metadata)
        self.cli.pending_wire = []
        self.recovered = set()

    def save(self):
        self.state["last_progress_at"] = now()
        path = no_symlinks(self.root / "state.json")
        temp = path.with_name(".state." + uuid.uuid4().hex)
        publish(temp, canonical(self.state))
        os.replace(temp, path)
        sync_directory(path.parent)

    def evidence(self, label, value):
        path = self.root / "observations" / (label + "-" + uuid.uuid4().hex + ".json")
        publish(path, canonical(value))
        return {"path": str(path), "sha256": digest(canonical(value))}

    def snapshot(self, sid):
        snap = self.cli.session(sid)
        session, transcript = snap["session"], snap["transcript"]
        require(session["id"] == sid and session["projectId"] == PROJECT and session["mode"] == "chat", "session scope/mode changed")
        require(not transcript.get("truncatedFields"), "truncated transcript")
        self.evidence("session-" + sid, snap)
        return snap

    def wait(self, phase, predicate):
        waits = self.state.setdefault("waits", {})
        self.state["waiting_phase"] = phase
        if phase not in waits:
            waits[phase] = {"started_at": now(), "monotonic": self.clock(), "boot_id": boot_id(), "limit": LIMITS[phase]}
            self.save()
        start = waits[phase]
        require(start["boot_id"] == boot_id() and self.clock() >= start["monotonic"], "UNKNOWN elapsed clock: " + phase)
        while True:
            require(self.clock() - start["monotonic"] < start["limit"], "wait deadline reached: " + phase)
            result = predicate()
            require(self.clock() - start["monotonic"] < start["limit"], "wait deadline reached: " + phase)
            if result:
                start["completed_at"] = now()
                start["elapsed"] = self.clock() - start["monotonic"]
                self.state.pop("waiting_phase", None)
                self.save()
                return result
            self.sleep(2)

    def rearm_wait(self):
        """An explicit same-run manual attempt, never invoked by polling."""
        phase = self.state.get("waiting_phase")
        wait = self.state.get("waits", {}).get(phase)
        if wait and (wait["boot_id"] != boot_id() or self.clock() - wait["monotonic"] >= wait["limit"]):
            self.state.setdefault("wait_history", []).append(dict(wait, phase=phase, manual_continue_at=now()))
            del self.state["waits"][phase]
            self.state.pop("waiting_phase", None)
        exported = self.state.get("waits", {}).get("export")
        if (exported and exported.get("completed_at") and "retire" not in self.state.get("completed_stages", []) and
                (exported["boot_id"] != boot_id() or self.clock() - exported["monotonic"] >= exported["limit"])):
            self.state.setdefault("wait_history", []).append(dict(exported, phase="export", manual_continue_at=now()))
            del self.state["waits"]["export"]
        if self.state.get("error"):
            self.state.setdefault("previous_errors", []).append(self.state.pop("error"))
        self.state["outcome"] = "pending"
        self.save()

    def effect(self, key, args, timeout, before_call=None):
        directory = mkdir(self.root / "control" / key)
        intent_path, result_path = directory / "intent.json", directory / "result.json"
        if intent_path.exists():
            intent = decode(private_read(intent_path))
            require(intent["argv"] == args, "intent changed: " + key)
            if result_path.exists():
                return decode(private_read(result_path))
            if (directory / "sent.json").exists():
                if args[0] == "send" and "--client-message-id" in args:
                    sid = args[args.index("--session") + 1]
                    handle = args[args.index("--client-message-id") + 1]
                    raw = self.cli.call(["send", "--session", sid, "--steer", "--recover-only", "--client-message-id", handle], 60)
                    result = {"stdout": raw.decode(), "recovered_original_handle": handle, "observed_at": now()}
                    publish(result_path, canonical(result))
                    self.state["effects"][key] = "observed-original"
                    self.save()
                    return result
                if key == "kill":
                    self.evidence("kill-unknown-observation", self.cli.read(["session", "get", self.old]))
                    raise Stop("UNKNOWN kill receipt; terminated does not prove preservation; no spawn")
                raise Stop("UNKNOWN effect; recover original intent only: " + key)
        intent = {"argv": args, "created_at": now(), "actor": "detached-runner", "run_id": self.run_id,
                  "old_id": self.old, "new_id": self.state.get("new_id"), "authority_refs": self.request["authority"]["source_refs"]}
        if not intent_path.exists():
            publish(intent_path, canonical(intent))
        self.state.setdefault("effects", {})[key] = "not-dispatched"
        self.save()
        sent_path = directory / "sent.json"
        def dispatched():
            publish(sent_path, canonical({"at": now(), "monotonic": self.clock()}))
            self.state["effects"][key] = "UNKNOWN"
            self.save()
        while True:
            deadline = before_call() if before_call else None
            try:
                raw = self.cli.call(args, timeout, deadline=deadline, on_dispatch=dispatched)
                break
            except WindowElapsed:
                # The adapter proved no subprocess was started. Reobserve S1/S2,
                # retaining the original bounded idle wait and mutation intent.
                self.evidence("not-dispatched-" + key, {"at": now(), "reason": "idle window elapsed before subprocess"})
                sent_path.unlink(missing_ok=True)
                sync_directory(directory)
                self.state["effects"][key] = "not-dispatched"
                self.save()
                continue
        result = {"stdout": raw.decode(), "stderr": getattr(self.cli, "last_stderr", ""), "sha256": digest(raw), "accepted_at": now()}
        publish(result_path, canonical(result))
        self.state["effects"][key] = "accepted"
        self.save()
        return result

    def send(self, sid, phase, body):
        if sid == self.state.get("new_id") and "bootstrap_sequence" in self.state:
            reject_new_human(self.snapshot(sid), self.state["bootstrap_sequence"])
        payload = dict(actor="detached-runner", run_id=self.run_id, phase=phase, old_id=self.old,
                       new_id=self.state.get("new_id"), recipient=sid,
                       authority_refs=self.request["authority"]["source_refs"], instruction=body)
        text = canonical(payload).decode()
        key = phase + "-" + identifier(sid)
        publish(self.root / "control" / key / "payload.json", canonical(payload))
        return self.effect(key, ["send", "--session", sid, "--steer", "--client-message-id",
                                 self.run_id + ":" + phase + ":" + sid, "--message", text], LIMITS["notify"])

    def live_workers(self):
        workers = [s for s in self.cli.sessions() if s["projectId"] == PROJECT and s["role"] == "worker" and not s["isTerminated"]]
        self.evidence("worker-list", workers)
        return workers

    def workspace(self, sid):
        session = self.cli.get("sessions/" + identifier(sid))["session"]
        require(session["id"] == sid and session["projectId"] == PROJECT, "workspace scope changed")
        row = workspace_entry(self.request, sid)
        require(row.get("kind") == session.get("kind") and row["kind"] in ("worker", "orchestrator"), "workspace kind mismatch")
        location = location_info(self.cli)
        require(location == self.state.get("location"), "bound CLI data/project location changed")
        repository = no_symlinks(location["project_path"])
        path = no_symlinks(row["path"])
        project_root = Path(location["managed_root"]) / PROJECT
        require(Path(row["path"]).is_absolute() and str(path) == row["path"] and path.is_relative_to(project_root) and path.is_dir(),
                "workspace outside bound project or missing/aliased")
        standard = row["kind"] == "worker" and path == project_root / sid
        workspace_sources(self.request, row, self.root, required=not standard)
        raw = old_git(["worktree", "list", "--porcelain", "-z"], workspace=repository)
        matches = []
        for block in raw.split(b"\0\0"):
            fields = {}
            for part in filter(None, block.split(b"\0")):
                key, _, value = part.partition(b" ")
                require(key not in fields, "ambiguous Git registration field")
                fields[key] = value
            if fields.get(b"worktree") == os.fsencode(path):
                require(b"prunable" not in fields and b"HEAD" in fields, "invalid/prunable worktree registration")
                matches.append(fields)
        require(len(matches) == 1, "no unique registered worktree for bound path: " + sid)
        common = no_symlinks(old_git(["rev-parse", "--path-format=absolute", "--git-common-dir"], workspace=repository).decode().strip())
        actual_common = no_symlinks(old_git(["rev-parse", "--path-format=absolute", "--git-common-dir"], workspace=path).decode().strip())
        top = no_symlinks(old_git(["rev-parse", "--show-toplevel"], workspace=path).decode().strip())
        require(top == path and actual_common == common, "workspace top-level/common repository mismatch")
        git_dir = no_symlinks(old_git(["rev-parse", "--absolute-git-dir"], workspace=path).decode().strip())
        head = old_git(["rev-parse", "HEAD"], workspace=path).decode().strip()
        branch = old_git(["rev-parse", "--abbrev-ref", "HEAD"], workspace=path).decode().strip()
        require(matches[0][b"HEAD"].decode() == head and
                ((branch == "HEAD" and b"detached" in matches[0]) or matches[0].get(b"branch") == ("refs/heads/" + branch).encode()),
                "Git registration/HEAD drift during observation")
        identity = {"path": str(path), "common": str(common), "git_dir": str(git_dir),
                    "path_identity": [path.stat().st_dev, path.stat().st_ino], "git_identity": [git_dir.stat().st_dev, git_dir.stat().st_ino]}
        bindings = self.state.setdefault("workspace_bindings", {})
        require(sid not in bindings or bindings[sid] == identity, "bound workspace registration/path drift")
        bindings[sid] = identity
        listing = self.root / "observations" / ("workspace-list-" + sid + "-" + uuid.uuid4().hex + ".raw")
        publish(listing, raw)
        self.evidence("workspace-" + sid, dict(identity, session_id=sid, source_refs=row["source_refs"],
                      session_branch=session.get("branch"), branch=None if branch == "HEAD" else branch, head=head,
                      detached=branch == "HEAD", standard_worker=standard, registration={"path": str(listing), "sha256": digest(raw)}))
        self.save()
        return str(path)

    def worker_stop(self, worker):
        sid = identifier(worker["id"])
        require(workspace_entry(self.request, sid).get("kind") == "worker", "worker missing bound workspace")
        rules = next((x for x in self.request["workers"] if x["worker_id"] == sid), None)
        directory = mkdir(self.root / "workers" / sid)
        path = directory / "stop.json"
        snap = self.snapshot(sid)
        if path.exists():
            stop = decode(private_read(path))
            validate_stop(stop, self.run_id, sid, directory)
            if stop["state"] != "safe":
                return False
            if stop["source_type"] == "worker-self":
                require(rules and rules["reserved_stop_d"], "self stop lacks original reserved directive")
                require(stop["stop_directive"]["last_ack_d"] == rules["reserved_stop_d"], "worker stop D does not match reserved directive")
                if not completed(snap, stop["evidence_turn_id"]):
                    return False
            if not quiescent(snap, stop["settled"]["in_flight"], self):
                return False
            require(stop["workspace"]["path"] == self.workspace(sid), "stop workspace differs from registered workspace")
            actual = git_snapshot(stop["workspace"]["path"], directory / ("verify-" + uuid.uuid4().hex))
            require(actual["sha256"] == stop["workspace"]["after_snapshot"]["sha256"], "worker content drift: " + sid)
            return stop
        if quiescent(snap, run=self):
            workspace = self.workspace(sid)
            before = git_snapshot(workspace, directory / ("before-" + uuid.uuid4().hex))
            self.sleep(2)
            after = git_snapshot(workspace, directory / ("after-" + uuid.uuid4().hex))
            if before["sha256"] != after["sha256"] or not quiescent(self.snapshot(sid), run=self):
                return False
            publish(directory / "quiescent-observation.raw", getattr(self.cli, "last_raw", canonical(snap)))
            publish(directory / "provenance.json", canonical([]))
            stop = worker_record(self, sid, "script-quiescent", "safe", None, before, after,
                                 file_reference(directory / "quiescent-observation.raw"),
                                 [dict(before, kind="workspace-snapshot"), dict(after, kind="workspace-snapshot")],
                                 file_reference(directory / "provenance.json"))
            validate_stop(stop, self.run_id, sid, directory)
            publish(path, canonical(stop))
            return stop
        if rules and rules["reserved_stop_d"]:
            self.send(sid, "worker-stop", dict(directive=rules["reserved_stop_d"], output=str(path),
                      schema="rh-worker-stop/2", rules=rules, **producer_description(self, "worker-receipt", sid)))
        return False

    def workers(self):
        results = []
        all_safe = True
        for worker in self.live_workers():
            result = self.worker_stop(worker)
            if not result:
                all_safe = False
            else:
                results.append(result)
        if not all_safe:
            return False
        self.state["stop_index_sha256"] = digest(canonical(results))
        self.state["stops"] = results
        publish(self.root / "workers/index.json", canonical(results))
        self.save()
        return True


def boot_id():
    return Path("/proc/sys/kernel/random/boot_id").read_text().strip()


def completed(snapshot, turn_id):
    return any(t.get("id") == turn_id and t.get("state") == "completed" and t.get("completedAt")
               for t in snapshot["transcript"].get("turns", []))


def worker_template(run_id, sid):
    return {"schema": "rh-worker-stop/2", "run_id": run_id, "project_id": PROJECT, "worker_id": sid,
            "observed_at": "<UTC>", "source_type": "worker-self", "state": "pending", "block_reason": None,
            "evidence_turn_id": "<actual turn id>", "stop_directive": {"last_ack_d": None, "next_d": None},
            "settled": {"running_turn_ids": [], "queued_turn_ids": [], "pending_activity_ids": [], "in_flight": []},
            "workspace": {"path": "<registered path>", "branch": None, "head": None,
                          "before_snapshot": {"path": "<private snapshot>", "sha256": "<hash>"},
                          "after_snapshot": {"path": "<private snapshot>", "sha256": "<hash>"}},
            "preservation": [], "pending_question_refs": [], "authority_ref": None, "resume_state": "held"}


def hash_reference(reference):
    require(isinstance(reference, dict) and isinstance(reference.get("path"), str) and
            re.fullmatch(r"[0-9a-f]{64}", reference.get("sha256", "")), "invalid hash reference")
    raw = private_read(reference["path"])
    require(digest(raw) == reference["sha256"], "referenced bytes changed: " + reference["path"])
    return raw


def validate_stop(stop, run_id, sid, directory=None):
    require(stop.get("schema") == "rh-worker-stop/2" and stop.get("run_id") == run_id and
            stop.get("project_id") == PROJECT and stop.get("worker_id") == sid, "worker receipt identity")
    require(stop.get("source_type") in ("worker-self", "script-quiescent") and
            stop.get("state") in ("safe", "pending", "blocked"), "worker receipt state")
    require(isinstance(stop.get("observed_at"), str) and "block_reason" in stop and "authority_ref" in stop,
            "worker receipt required fields")
    require(stop.get("resume_state") in ("held", "authorized-original"), "worker resume state")
    for key in ("pending_question_refs", "preservation"):
        require(isinstance(stop.get(key), list), "worker receipt array missing")
    require(set(stop.get("stop_directive", {})) == {"last_ack_d", "next_d"}, "stop directive fields")
    settled = stop["settled"]
    for key in ("running_turn_ids", "queued_turn_ids", "pending_activity_ids", "in_flight"):
        require(isinstance(settled.get(key), list), "settled array missing")
        require(stop["state"] != "safe" or key == "in_flight" or not settled[key], "unsettled safe claim")
    workspace = stop["workspace"]
    require(all(key in workspace for key in ("path", "branch", "head", "before_snapshot", "after_snapshot")), "workspace fields")
    references = [*stop["preservation"], *[{"path": x.get("evidence_path", ""), "sha256": x.get("sha256")} for x in settled["in_flight"]]]
    for key in ("before_snapshot", "after_snapshot"):
        reference = workspace[key]
        require(reference is not None or (stop["state"] != "safe" and stop.get("block_reason")), "safe worker lacks snapshot")
        if reference is not None:
            references.append(reference)
    for key in ("producer_observation", "provenance"):
        reference = stop.get(key)
        require(reference is not None or stop["state"] != "safe", "safe worker lacks producer evidence")
        if reference is not None:
            references.append(reference)
    for reference in references:
        if directory is not None:
            require(no_symlinks(reference.get("path", "")).is_relative_to(directory), "worker reference outside fixed directory")
        hash_reference(reference)
    for item in settled["in_flight"]:
        require(isinstance(item.get("reference"), str) and item["reference"] and isinstance(item.get("state"), str), "invalid operation reference")
        raw = hash_reference({"path": item["evidence_path"], "sha256": item["sha256"]})
        receipt = decode(raw)
        if stop["state"] == "safe":
            require(item["state"] in ("completed", "cancelled", "failed") and receipt.get("reference") == item["reference"] and
                    receipt.get("state") == item["state"], "external operation lacks terminal evidence")
    if stop["state"] == "safe":
        require(workspace["before_snapshot"]["sha256"] == workspace["after_snapshot"]["sha256"], "worker snapshots differ")
    else:
        require(isinstance(stop.get("block_reason"), str) and stop["block_reason"].strip(), "blocked/pending worker lacks reason")
    for reference in stop["preservation"]:
        require(isinstance(reference.get("kind"), str) and reference["kind"], "preservation kind missing")
        hash_reference(reference)


def file_reference(path):
    return {"path": str(path), "sha256": digest(private_read(path))}


def bound_workspace(run, sid):
    row = workspace_entry(run.request, sid)
    path = no_symlinks(row["path"])
    binding = run.state["workspace_bindings"][sid]
    require(str(path) == binding["path"] and [path.stat().st_dev, path.stat().st_ino] == binding["path_identity"], "producer workspace drift")
    common = no_symlinks(old_git(["rev-parse", "--path-format=absolute", "--git-common-dir"], workspace=path).decode().strip())
    metadata = no_symlinks(old_git(["rev-parse", "--absolute-git-dir"], workspace=path).decode().strip())
    require(str(common) == binding["common"] and str(metadata) == binding["git_dir"] and
            [metadata.stat().st_dev, metadata.stat().st_ino] == binding["git_identity"] and
            old_git(["rev-parse", "--show-toplevel"], workspace=path).decode().strip() == str(path), "producer Git identity drift")
    listing = old_git(["worktree", "list", "--porcelain", "-z"], workspace=path)
    blocks = [x for x in listing.split(b"\0\0") if b"worktree " + os.fsencode(path) + b"\0" in x + b"\0"]
    require(len(blocks) == 1 and b"prunable" not in blocks[0], "producer registration missing/nonunique")
    return path


def source_inputs(run, sid, workspace, rows):
    roots = {"workspace": workspace, "artifact": no_symlinks(run.cli.data_dir / "artifacts" / sid)}
    result = []
    for row in rows:
        path = no_symlinks(row["path"])
        matches = [(kind, root) for kind, root in roots.items() if path.is_relative_to(root)]
        require(len(matches) == 1 and path.is_file(), "producer source outside actor roots or not regular")
        kind, root = matches[0]
        relative = str(path.relative_to(root))
        require(not SENSITIVE_SOURCE.search(relative), "sensitive producer source")
        require(isinstance(row.get("kind"), str) and row["kind"] and isinstance(row.get("basis"), str) and row["basis"].strip(),
                "source kind/necessity basis missing")
        result.append(dict(row, path=str(path), relative_path=relative, source_root=kind,
                           sha256=digest(read_regular(path)), original_mode=stat.S_IMODE(path.stat().st_mode)))
    require(len({x["path"] for x in result}) == len(result), "duplicate material source")
    return result


def save_sources(directory, rows, subdirectory="saved"):
    refs, provenance = [], []
    for row in rows:
        destination = confined(directory / subdirectory / row["source_root"], row["relative_path"])
        raw = read_regular(row["path"])
        require(digest(raw) == row["sha256"], "material changed during copy")
        publish(destination, raw)
        ref = dict(file_reference(destination), kind=row["kind"])
        refs.append(ref)
        provenance.append(dict(row, copy=ref))
    return refs, provenance


def directive_entry(snapshot, handle, old, payload):
    controls = []
    for entry in entries(snapshot):
        steer = entry.get("kind") == "activity" and entry.get("activityKind") == "system" and entry.get("detail", {}).get("event") == "steer"
        fields = entry.get("detail", {}) if steer else entry
        if fields.get("clientMessageId") != handle:
            continue
        require((steer and entry.get("status") == "completed") or
                (entry.get("kind") == "message" and entry.get("role") == "user"), "directive structural identity mismatch")
        require(fields.get("origin") == "automation" and fields.get("senderSessionId") == old and
                fields.get("senderProjectId") == PROJECT and fields.get("text") == "[from " + old + "] " + payload,
                "producer directive provenance mismatch")
        turn = entry.get("consumedByTurnId") or entry.get("turnId")
        require(turn and type(entry.get("sequence")) is int, "directive turn/sequence missing")
        controls.append(dict(entry, dispatch_turn_id=turn))
    require(len(controls) == 1, "producer directive provenance mismatch")
    return controls[0]


def producer_control(run, sid, phase, snapshot):
    directory = run.root / "control" / (phase + "-" + sid)
    payload = private_read(directory / "payload.json").decode()
    intent = decode(private_read(directory / "intent.json"))
    expected = ["send", "--session", sid, "--steer", "--client-message-id", run.run_id + ":" + phase + ":" + sid, "--message", payload]
    require(intent.get("argv") == expected and (directory / "result.json").is_file(), "producer lacks accepted original directive")
    return directive_entry(snapshot, expected[5], run.old, payload), decode(payload)


def producer_observation(run, sid, action, directory):
    snapshot = run.snapshot(sid)
    session, transcript = snapshot["session"], snapshot["transcript"]
    require(session.get("id") == sid and session.get("projectId") == PROJECT and session.get("mode") == "chat" and
            not session.get("isTerminated"), "producer actor identity mismatch")
    phase = "worker-stop" if action == "worker-receipt" else "2a"
    control, payload = producer_control(run, sid, phase, snapshot)
    running = [t for t in transcript.get("turns", []) if t.get("state") == "running"]
    require(len(running) == 1 and running[0]["id"] == control["dispatch_turn_id"], "producer evidence turn not unique/directive turn")
    turn = running[0]["id"]
    observation_dir = mkdir(directory / "observations")
    raw = getattr(run.cli, "last_raw", canonical(snapshot))
    publish(observation_dir / "session.raw", raw)
    all_entries = list(entries(snapshot))
    missing = {x["sequence"] for x in transcript.get("incomplete") or [] if x.get("type") == "entry"} - {x.get("sequence") for x in all_entries}
    page, previous = snapshot, None
    while missing and page["transcript"].get("hasMoreBefore"):
        cursor = page["transcript"]["next"]["before"]
        require(type(cursor) is int and (previous is None or cursor < previous), "producer cursor stalled")
        previous = cursor
        page = run.cli.read(["session", "get", sid, "--transcript", "500", "--cap", "0", "--before", str(cursor)])
        require(page["transcript"]["conversationId"] == transcript["conversationId"], "producer conversation drift")
        publish(observation_dir / ("page-" + str(cursor) + ".raw"), run.cli.last_raw)
        all_entries.extend(entries(page))
        missing -= {x.get("sequence") for x in entries(page)}
    require(not missing, "producer incomplete identity missing")
    own = [e for e in all_entries if e.get("turnId") == turn and e.get("kind") == "activity" and
           e.get("activityKind") == "command" and e.get("status") == "running" and
           helper_identity(e.get("detail", {}).get("rawCommand") or e.get("detail", {}).get("command", ""), run.root, sid) == (action, {})]
    require(len(own) == 1, "producer self activity missing/ambiguous")
    excluded, retained = [], []
    for item in transcript.get("incomplete") or []:
        evidence = dict(item)
        allowed = item.get("type") == "turn" and item.get("id") == turn and item.get("state") == "running"
        if item.get("type") == "entry":
            matches = [e for e in all_entries if e.get("sequence") == item.get("sequence")]
            require(len(matches) == 1, "producer incomplete sequence ambiguous")
            e = matches[0]
            evidence.update(entry_id=e["id"], turn_id=e.get("turnId"), kind=e.get("kind"), activity_kind=e.get("activityKind"))
            allowed = e.get("turnId") == turn and ((e.get("kind") == "message" and e.get("role") == "assistant" and e.get("streaming")) or e["id"] == own[0]["id"])
        (excluded if allowed else retained).append(evidence)
    for e in all_entries:
        if e.get("kind") == "activity" and e.get("status") in ("running", "pending") and e["id"] != own[0]["id"]:
            retained.append({"entry_id": e["id"], "turn_id": e.get("turnId"), "reason": "other active activity"})
    proof = {"schema": "rh-producer-observation/1", "run_id": run.run_id, "actor_id": sid, "evidence_turn_id": turn,
             "raw": file_reference(observation_dir / "session.raw"), "self_activity": {"id": own[0]["id"], "sequence": own[0]["sequence"], "command": helper_command(run.root, action, sid)},
             "excluded": excluded, "retained": retained, "truncated_fields": transcript.get("truncatedFields", 0)}
    publish(observation_dir / "producer.json", canonical(proof))
    return snapshot, proof, file_reference(observation_dir / "producer.json"), payload


def begin_producer(run, action, sid, directory, inputs, output):
    require(os.environ.get("AO_SESSION_ID") == sid and os.environ.get("TMPDIR") == str(directory / "draft"), "producer requires exact actor/draft environment")
    intent_path = directory / "producer-intent.json"
    if intent_path.exists():
        intent = decode(private_read(intent_path))
        require(intent.get("inputs") == inputs and intent.get("action") == action and intent.get("actor_id") == sid, "producer inputs changed")
        committed = decode(private_read(directory / "producer-result.json"))
        raw = private_read(output)
        require(digest(raw) == committed["output_sha256"], "producer committed result changed")
        helper_result(run, action, sid, {})
        return intent, decode(raw)
    require(not output.exists(), "producer output without intent")
    intent = {"schema": "rh-producer-intent/1", "action": action, "actor_id": sid, "run_id": run.run_id,
              "inputs": inputs, "created_at": now(), "command": helper_command(run.root, action, sid)}
    publish(intent_path, canonical(intent))
    return intent, None


def commit_producer(directory, output, result):
    raw = canonical(result)
    publish(directory / "producer-result.json", canonical({"output_sha256": digest(raw)}))
    publish(output, raw)
    return result


def worker_record(run, sid, source_type, state, reason, before, after, observation, preservation, provenance, turn=None, operations=()):
    rules = next(x for x in run.request["workers"] if x["worker_id"] == sid)
    record = worker_template(run.run_id, sid)
    record.update(observed_at=now(), source_type=source_type, state=state, block_reason=reason, evidence_turn_id=turn,
                  producer_observation=observation, provenance=provenance, preservation=preservation,
                  pending_question_refs=rules["pending_question_refs"], authority_ref=run.request["authority"]["source_refs"][0])
    record["workspace"].update(path=workspace_entry(run.request, sid)["path"], branch=after.get("branch") if after else None,
                               head=after.get("head") if after else None, before_snapshot=before, after_snapshot=after)
    record["settled"]["in_flight"] = list(operations)
    record["stop_directive"] = {"last_ack_d": rules["reserved_stop_d"] if source_type == "worker-self" else None,
                                "next_d": None if source_type == "worker-self" else rules["reserved_stop_d"]}
    if source_type == "script-quiescent":
        record["qualification"] = "未經worker自述"
    return record


def worker_receipt(run):
    sid = identifier(os.environ.get("AO_SESSION_ID", ""))
    rules = next((x for x in run.request["workers"] if x["worker_id"] == sid), None)
    require(rules and rules["reserved_stop_d"] and workspace_entry(run.request, sid)["kind"] == "worker", "worker producer not in bound rules")
    directory = mkdir(run.root / "workers" / sid)
    acting = CLI(run.cli.executable, sid, run.cli.data_dir, run.cli.run_file, directory / "draft")
    acting.capture_root = mkdir(directory / "wire")
    run.cli = acting
    workspace = bound_workspace(run, sid)
    workspace_git_inputs(workspace)
    statement_raw = private_read(directory / "draft/statement.json")
    statement = decode(statement_raw)
    require(statement.get("schema") == "rh-worker-statement/1" and statement.get("run_id") == run.run_id and statement.get("worker_id") == sid,
            "worker statement identity")
    require(statement.get("requested_state") in ("safe", "pending", "blocked") and
            all(isinstance(statement.get(k), list) for k in ("preservation_sources", "in_flight", "pending_question_refs")), "worker statement fields")
    rows = source_inputs(run, sid, workspace, statement["preservation_sources"])
    op_rows = source_inputs(run, sid, workspace, [dict(path=x["receipt_path"], kind="terminal-operation", basis=x["reference"]) for x in statement["in_flight"]])
    inputs = {"statement_sha256": digest(statement_raw), "materials": rows, "operations": op_rows}
    intent, cached = begin_producer(run, "worker-receipt", sid, directory, inputs, directory / "stop.json")
    if cached is not None:
        return cached
    snap, proof, observation, control = producer_observation(run, sid, "worker-receipt", directory)
    require(snap["session"]["kind"] == "worker" and control["instruction"]["directive"] == rules["reserved_stop_d"] and
            control["instruction"]["rules"] == rules, "worker D/rules mismatch")
    preserved, provenance = save_sources(directory, rows)
    op_refs, op_provenance = save_sources(directory, op_rows, "operations")
    provenance += op_provenance
    operations = []
    for item, ref in zip(statement["in_flight"], op_refs):
        receipt = decode(private_read(ref["path"]))
        require(receipt.get("reference") == item["reference"] and receipt.get("activity_id") == item["activity_id"] and
                receipt.get("state") == item["state"], "external terminal source mismatch")
        operations.append(dict(reference=item["reference"], activity_id=item["activity_id"], state=item["state"], evidence_path=ref["path"], sha256=ref["sha256"]))
    reasons = []
    if proof["retained"] or proof["truncated_fields"]:
        reasons.append("other incomplete/unknown producer activity")
    for entry in entries(snap):
        if entry.get("id") != proof["self_activity"]["id"] and not activity_quiescent(entry, operations, run, sid):
            reasons.append("other tool/operation not positively terminal: " + str(entry.get("id")))
    before = after = None
    try:
        before = git_snapshot(workspace, directory / "before")
        run.sleep(2)
        after = git_snapshot(workspace, directory / "after")
        require(before["sha256"] == after["sha256"], "worker snapshot drift")
    except (Stop, OSError, subprocess.TimeoutExpired) as exc:
        reasons.append(str(exc))
    requested = statement["requested_state"]
    require(requested == "safe" or statement.get("block_reason"), "worker blocked reason missing")
    require(statement["pending_question_refs"] == rules["pending_question_refs"], "worker pending questions changed")
    state = "blocked" if reasons else requested
    reason = "; ".join(reasons) if reasons else statement.get("block_reason")
    require(state != "safe" or not reason, "safe statement has blocked reason")
    publish(directory / "provenance.json", canonical(provenance))
    result = worker_record(run, sid, "worker-self", state, reason, before, after, observation, preserved,
                           file_reference(directory / "provenance.json"), proof["evidence_turn_id"], operations)
    validate_stop(result, run.run_id, sid, directory)
    return commit_producer(directory, directory / "stop.json", result)


def producer_description(run, action, sid):
    directory = helper_directory(run.root, action, sid)
    mkdir(directory / "draft")
    template = ({"schema": "rh-worker-statement/1", "run_id": run.run_id, "worker_id": sid, "requested_state": "safe", "block_reason": None,
                 "preservation_sources": [], "in_flight": [], "pending_question_refs": []} if action == "worker-receipt" else
                {"schema": "rh-old-statement/1", "run_id": run.run_id, "old_id": sid, "original_ref": "<existing refs/heads/...>",
                 "classification": [], "pending_question_refs": []})
    return {"command": helper_command(run.root, action, sid), "statement": str(directory / "draft/statement.json"), "template": template,
            "permissions": "目錄0700／檔0600／拒symlink；只填語意statement，不自行產snapshot/stop/phase或完成時刻。",
            "evidence_root": str(directory),
            "source_rule": "必要原件只限本actor已綁workspace或自己的AO artifact；每筆preservation_sources填path/kind/basis；helper固定copy並保留原物/mode/hash/相對關係。in_flight填reference/activity_id/state/receipt_path，必須原terminal來源，不自造終態。",
            "completion": "先用固定helper生成本輪證據；helper可辨識自己的觀測活動，不要另寫排除turn的assert。成功後短回覆並completed/hold，runner獨立全後驗；不要輪詢等自身完成。任何STOP不重試或改回條。"}


def old_receipt(run):
    sid = identifier(os.environ.get("AO_SESSION_ID", ""))
    require(sid == run.old, "old-receipt must run as old")
    workspace = bound_workspace(run, sid)
    workspace_git_inputs(workspace)
    directory = mkdir(run.root / "old")
    statement_raw = private_read(directory / "draft/statement.json")
    statement = decode(statement_raw)
    require(statement.get("schema") == "rh-old-statement/1" and statement.get("run_id") == run.run_id and statement.get("old_id") == sid and
            isinstance(statement.get("classification"), list) and isinstance(statement.get("pending_question_refs"), list), "old statement identity/fields")
    rules, paths, copy_rows = [], [], []
    for row in statement["classification"]:
        relative = row.get("path")
        require(isinstance(relative, str) and relative and not Path(relative).is_absolute() and str(Path(relative)) == relative and
                all(x not in (".", "..") for x in Path(relative).parts), "classification must be clean workspace-relative path")
        path = confined(workspace, relative)
        require(path.exists() and row.get("disposition") in ("copy", "rebuildable", "discardable") and row.get("basis"), "old classification incomplete")
        require(not any(relative == other["path"] or (other.get("scope") == "directory" and relative.startswith(other["path"] + "/")) or
                        (row.get("scope") == "directory" and other["path"].startswith(relative + "/")) for other in rules), "old classification overlap")
        rule = dict(row)
        require("copy" not in rule, "old actor may not select copy destination")
        if rule["disposition"] == "copy":
            require(path.is_file() and rule.get("scope") != "directory", "necessary copy must enumerate regular files")
            copy_rows.append(dict(path=str(path), kind="old-necessary-material", basis=rule["basis"]))
        rules.append(rule)
        paths.append(relative)
    rows = source_inputs(run, sid, workspace, copy_rows)
    managed_raw = private_read(directory / "managed.json")
    inputs = {"statement_sha256": digest(statement_raw), "managed_sha256": digest(managed_raw), "materials": rows}
    intent, cached = begin_producer(run, "old-receipt", sid, directory, inputs, directory / "phase-2a.json")
    if cached is not None:
        return cached
    snap, proof, observation, control = producer_observation(run, sid, "old-receipt", directory)
    require(snap["session"]["kind"] == "orchestrator" and control["instruction"]["worker_index_sha256"] == run.state["stop_index_sha256"],
            "old helper directive/index mismatch")
    documents = run.cli.documents()
    handoff = no_symlinks(Path(documents["sharedDir"]) / documents["documentPath"])
    receipt = {"schema": "rh-old-2a/1", "run_id": run.run_id, "old_id": sid, "evidence_turn_id": proof["evidence_turn_id"],
               "created_at": intent["created_at"], "managed_reply": file_reference(directory / "managed.json"),
               "handoff": {"path": str(handoff), "sha256": digest(read_regular(handoff))}, "lease": lease_view(documents),
               "worker_index_sha256": run.state["stop_index_sha256"], "pending_question_refs": statement["pending_question_refs"],
               "user_status_instruction": status_command(run.root, run.run_id), "producer_observation": observation}
    require(receipt["lease"]["session_id"] == sid, "old is not managed owner")
    verify_managed(receipt, run.run_id, dict(documents, lease=lease_view(documents)), old=True)
    copies, provenance = save_sources(directory, rows)
    for rule in rules:
        if rule["disposition"] == "copy":
            source = str(workspace / rule["path"])
            item = next(x for x in provenance if x["path"] == source)
            rule["copy"] = item["copy"]
    common = no_symlinks(old_git(["rev-parse", "--path-format=absolute", "--git-common-dir"], workspace=workspace).decode().strip())
    metadata = no_symlinks(old_git(["rev-parse", "--absolute-git-dir"], workspace=workspace).decode().strip())
    publish(directory / "provenance.json", canonical(provenance))
    plan = {"schema": "rh-old-preservation/2", "run_id": run.run_id, "old_id": sid, "evidence_turn_id": proof["evidence_turn_id"],
            "original_ref": statement["original_ref"], "classification": rules,
            "gitlinks": gitlink_facts(workspace, "old", common, metadata), "provenance": file_reference(directory / "provenance.json")}
    publish(directory / "preservation.json", canonical(plan))
    old_git_state(run, plan, workspace=workspace)
    receipt["preservation"] = file_reference(directory / "preservation.json")
    return commit_producer(directory, directory / "phase-2a.json", receipt)


# Explicit value whitelist. In particular, never SELECT * from sessions/turns.
EXPORT_COLUMNS = {
    "conversation_branches": "id conversation_id session_id provider_conversation_id parent_branch_id fork_after_turn_id replaced_turn_id replacement_turn_id fork_after_sequence created_at strategy replay_cutoff_sequence replay_truncated provider_scope_id provider_ids_scoped review_id handoff_note".split(),
    "conversation_messages": "id conversation_id turn_id sequence revision role origin text streaming provider_item_id client_message_id created_at updated_at delivery_content_json branch_id client_payload_hash sender_session_id sender_project_id sender_display_name continuation".split(),
    "conversation_turns": "id conversation_id handled_by_session_id provider_turn_id controller_generation state error_message requested_at started_at completed_at diff_json rolled_back_at plan_json branch_id promotion_started_at promoted_to_turn_id retry_of_turn_id handled_by_review_id usage_limit_resets_at usage_limit_auto_resume usage_limit_resume_error batched_input intake_sequence".split(),
    "conversation_activities": "id conversation_id turn_id sequence revision kind status summary detail_json request_id provider_item_id created_at updated_at command_output command_output_truncated streamed_text streamed_text_truncated branch_id".split(),
}


def export_conversation(database, old, conversation, harness, latest_sequence):
    path = no_symlinks(database)
    uri = path.as_uri() + "?mode=ro"
    with sqlite3.connect(uri, uri=True, timeout=30) as db:
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA query_only=ON")
        db.execute("BEGIN")
        session = db.execute("SELECT id,project_id,harness,provider_conversation_id FROM sessions WHERE id=?", (old,)).fetchone()
        require(session and session["project_id"] == PROJECT and session["harness"] == harness, "DB source identity mismatch")
        require(type(latest_sequence) is int and latest_sequence > 0, "CLI cut sequence unavailable")
        result = {"session": dict(session), "conversation_id": conversation, "cli_latest_sequence": latest_sequence}
        for table, columns in EXPORT_COLUMNS.items():
            # Qualified schema only; an unknown column never widens the read.
            rows = db.execute("SELECT " + ",".join(columns) + " FROM " + table + " WHERE conversation_id=? ORDER BY id", (conversation,))
            result[table] = [dict(row) for row in rows]
            require(all(row["conversation_id"] == conversation for row in result[table]), "DB message scope changed")
        messages = result["conversation_messages"]
        sequences = [row["sequence"] for table in ("conversation_messages", "conversation_activities") for row in result[table]]
        require(messages and all(type(seq) is int and 0 < seq <= latest_sequence for seq in sequences),
                "DB messages/activities exceed or lack CLI cut sequence")
        db.rollback()
        return result


def native_identity(database, sid, harness):
    with sqlite3.connect(no_symlinks(database).as_uri() + "?mode=ro", uri=True, timeout=30) as db:
        db.execute("PRAGMA query_only=ON")
        row = db.execute("SELECT id,project_id,harness,provider_conversation_id FROM sessions WHERE id=?", (sid,)).fetchone()
        require(row and row[:3] == (sid, PROJECT, harness), "native identity mismatch")
        return row[3]


def read_regular(path):
    path = no_symlinks(path)
    require(path.is_file(), "necessary source unavailable: " + str(path))
    return path.read_bytes()


def preserve_dependencies(seeds, destination, allowed_roots, directory_refs=None, workspace_dirs=()):
    pending, seen, manifest = list(seeds), set(), []
    while pending:
        source = no_symlinks(pending.pop())
        if source.is_dir():
            require(directory_refs is not None and (str(source) in workspace_dirs or any(source.is_relative_to(Path(root).absolute()) for root in allowed_roots)), "directory reference outside known scope")
            if source not in seen:
                directory_refs.append({"source": str(source), "kind": "directory-reference", "contents": "preserved workspace snapshots or original location; not recursively copied"})
                seen.add(source)
            continue
        require(any(source.is_relative_to(Path(root).absolute()) for root in allowed_roots), "dependency outside allowed roots: " + str(source))
        require(not re.search(r"(^|/)(\.env(?:\..*)?|credentials|privatehome|agent-accounts|ao\.db|running\.json)(/|$)", str(source)), "sensitive dependency refused")
        if source in seen:
            continue
        seen.add(source)
        raw = read_regular(source)
        relative = str(source).lstrip("/")
        target = Path(destination) / relative
        publish(target, raw)
        manifest.append({"source": str(source), "path": str(target), "sha256": digest(raw)})
        if source.suffix.lower() not in (".md", ".mdx"):
            continue
        text = raw.decode()
        links = re.findall(r"!?\[[^\]]*\]\(([^)]+)\)", text)
        links += re.findall(r"`(/[^`\n]+)`", text)
        for link in links:
            link = link.strip().strip("<>").split("#", 1)[0]
            if not link or re.match(r"[a-zA-Z][a-zA-Z0-9+.-]*:", link):
                continue
            path = Path(urllib.parse.unquote(link))
            pending.append(path if path.is_absolute() else source.parent / path)
    return manifest


def report_interruption(snapshot, phase_turn):
    """Positive report cause only; words like '404' never drive recovery."""
    turns = snapshot["transcript"].get("turns", [])
    interrupted = next((t for t in turns if t.get("id") == phase_turn and t.get("state") == "interrupted"), None)
    if not interrupted:
        return False
    following = [e for e in entries(snapshot) if e.get("createdAt", "") >= interrupted.get("startedAt", "~")]
    if any(e.get("origin") == "human" for e in following):
        raise Stop("human input/cancellation after interrupted phase")
    require(not any(e.get("activityKind") == "error" or e.get("status") == "failed" for e in following), "tool failure is not report interruption")
    # The persisted semantic report is an automation message with a durable
    # report-batch client id; text alone is never sufficient evidence.
    return any(e.get("kind") == "message" and e.get("role") == "user" and
               e.get("origin") == "automation" and e.get("clientMessageId", "").startswith("report-batch:") and
               "ao://sessions/" + PROJECT + "/" in e.get("text", "") and
               interrupted.get("completedAt") and e.get("createdAt", "") >= interrupted["completedAt"]
               for e in following)


def verify_managed(receipt, run_id, documents, old=False):
    raw = hash_reference(receipt["managed_reply"])
    reply = decode(raw)["file"]
    reference = receipt["handoff"] if old else receipt["managed_receipt"]
    content = read_regular(reference["path"])
    require(digest(content) == reference["sha256"] == reply["hash"], "managed content hash mismatch")
    require(reply["content"].encode() == content and "handoff_run_id: " + run_id in reply["content"], "managed reply not bound to run")
    require(str(Path(documents["sharedDir"]) / reply["path"]) == reference["path"], "managed reply path mismatch")
    lease = receipt["lease"]
    require(type(lease.get("epoch")) is int and type(lease.get("frozen")) is bool and
            lease == documents.get("lease") and not lease["frozen"], "managed lease mismatch/frozen")
    return content


def phase_2a(run):
    path = run.root / "old" / "phase-2a.json"
    documents = run.cli.documents()
    instruction = dict(output=str(path), worker_index_sha256=run.state["stop_index_sha256"],
                       worker_index={"path": str(run.root / "workers/index.json"), "sha256": run.state["stop_index_sha256"]},
                       start=str(run.root / "start.json"), schema="rh-old-2a/1", managed_output=str(run.root / "old/managed.json"),
                       user_status_instruction=status_command(run.root, run.run_id),
                       requirements="old自己用正常documents write/expected-hash更新A–F，正文含handoff_run_id，原CLI --json保存old/managed.json。classification.path必為workspace乾淨相對路徑，disposition/basis需閉合untracked/ignored及空目錄；copy只列精確regular file，目的地由helper決定。original_ref只准既存refs/heads，不能新建ref。先填statement，再執行固定helper；preservation/2完整後phase-2a最後發布。完成回覆列run/ref/dirty切前證據、精確status及10分鐘未見new首報只查原run提示，然後completed，不輪詢。",
                       **producer_description(run, "old-receipt", run.old))
    run.send(run.old, "2a", instruction)
    def ready():
        if not path.exists():
            return False
        receipt = decode(private_read(path))
        require(Path(receipt.get("managed_reply", {}).get("path", "")).is_relative_to(run.root / "old"), "2a reply outside fixed directory")
        require(receipt.get("schema") == "rh-old-2a/1" and receipt.get("run_id") == run.run_id and receipt.get("old_id") == run.old,
                "2a receipt identity")
        require(receipt.get("worker_index_sha256") == run.state["stop_index_sha256"] and
                receipt.get("user_status_instruction") == status_command(run.root, run.run_id) and
                isinstance(receipt.get("pending_question_refs"), list) and isinstance(receipt.get("created_at"), str), "2a receipt fields")
        snap = run.snapshot(run.old)
        finished_turn = phase_completed(run, run.old, "2a", receipt["evidence_turn_id"], snap)
        if not finished_turn:
            continue_phase(run, run.old, "2a", receipt["evidence_turn_id"], snap)
            return False
        if not quiescent(snap, run=run):
            return False
        replies = [e.get("text", "") for e in entries(snap) if e.get("role") == "assistant" and e.get("turnId") == finished_turn]
        require(any(status_command(run.root, run.run_id) in text and "10" in text for text in replies), "old ready reply lacks exact status/silence instruction")
        payload = private_read(run.root / "control" / ("2a-" + run.old) / "payload.json").decode()
        control = directive_entry(snap, run.run_id + ":2a:" + run.old, run.old, payload)
        run.state["old_control_sequence"] = control["sequence"]
        documents = run.cli.documents()
        require(documents.get("lease", {}).get("sessionId") == run.old, "old not managed owner")
        # Wrapper uses snake_case while the normal CLI lease uses camelCase.
        check_documents = dict(documents, lease=lease_view(documents))
        verify_managed(receipt, run.run_id, check_documents, old=True)
        require(run.workers() and run.state["stop_index_sha256"] == receipt["worker_index_sha256"], "worker set changed after 2a")
        run.state["handoff"] = receipt["handoff"]
        run.state["phase_2a"] = receipt
        run.state["phase_2a_completed_turn"] = finished_turn
        run.save()
        return receipt
    return run.wait("old_2a", ready)


def lease_view(documents):
    lease = documents.get("lease") or {}
    return {"session_id": lease.get("sessionId"), "epoch": lease.get("epoch"), "frozen": lease.get("frozen")}


def continue_phase(run, sid, phase, turn, snapshot):
    if not report_interruption(snapshot, turn):
        return
    key = phase + ":" + turn
    recovered = run.state.setdefault("phase_continuations", {})
    if key in recovered:
        continuation = continuation_turn(run, sid, phase, snapshot)
        if continuation and report_interruption(snapshot, continuation):
            raise Stop("second report interruption requires recovery")
        return
    if not quiescent(snapshot, run=run):
        return
    run.send(sid, phase + "-continue", {"original_turn": turn, "original_phase": phase,
             "receipt": str(run.root / ("old/phase-2a.json" if phase == "2a" else "bootstrap/new/receipt.json")),
             "nonce": run.state.get("nonce"), "requirements": "核原已成功檔與managed hash；只補未完成回報，不重寫已提交效果，不重發nonce。人類取消／UNKNOWN或真失敗即停。"})
    recovered[key] = {"interrupted_turn": turn, "sent_at": now()}
    run.save()


def continuation_turn(run, sid, phase, snapshot):
    key = run.run_id + ":" + phase + "-continue:" + sid
    matches = [e for e in entries(snapshot) if e.get("clientMessageId") == key and e.get("origin") == "automation"]
    require(len(matches) <= 1, "continuation handle ambiguous")
    return matches[0].get("consumedByTurnId", matches[0].get("turnId")) if matches else None


def phase_completed(run, sid, phase, original, snapshot):
    if completed(snapshot, original):
        return original
    if phase + ":" + original in run.state.get("phase_continuations", {}):
        turn = continuation_turn(run, sid, phase, snapshot)
        if turn and completed(snapshot, turn):
            return turn
    return None


def export_material(run, delta=False):
    snap = run.snapshot(run.old)
    transcript = snap["transcript"]
    if transcript.get("incomplete"):
        return False
    conversation, branch = transcript["conversationId"], transcript["activeBranchId"]
    pages, cursor = [], None
    directory = mkdir(run.root / ("delta" if delta else "cut") / uuid.uuid4().hex)
    while True:
        if snap["transcript"].get("incomplete"):
            return False
        require(snap["transcript"]["conversationId"] == conversation and snap["transcript"]["activeBranchId"] == branch,
                "projection branch/scope changed")
        require(not snap["transcript"].get("truncatedFields") and not snap["transcript"].get("incomplete") and
                snap["transcript"]["latestSequence"] == transcript["latestSequence"], "projection changed/truncated")
        page_raw = getattr(run.cli, "last_raw", canonical(snap))
        path = directory / ("page-" + str(len(pages)) + ".json")
        publish(path, page_raw)
        pages.append({"path": str(path), "sha256": digest(page_raw)})
        if not snap["transcript"]["hasMoreBefore"]:
            break
        next_cursor = snap["transcript"].get("next", {}).get("before")
        require(type(next_cursor) is int and (cursor is None or next_cursor < cursor), "projection pagination unavailable")
        cursor = next_cursor
        snap = run.cli.read(["session", "get", run.old, "--transcript", "500", "--cap", "0", "--before", str(cursor)])
    raw = export_conversation(run.cli.data_dir / "ao.db", run.old, conversation, snap["session"]["harness"], transcript["latestSequence"])
    latest_visible = max((entry["sequence"] for entry in transcript["entries"] if type(entry.get("sequence")) is int), default=None)
    require(latest_visible is not None and any(row["sequence"] == latest_visible for table in
            ("conversation_messages", "conversation_activities") for row in raw[table]), "latest CLI entry missing from DB cut")
    after = run.snapshot(run.old)["transcript"]
    require(after["conversationId"] == conversation and after["activeBranchId"] == branch and
            after["latestSequence"] == transcript["latestSequence"], "conversation changed during export; retain partial evidence")
    if after.get("incomplete"):
        return False
    publish(directory / "conversation.json", canonical(raw))
    handoff = run.state["handoff"]
    require(digest(read_regular(handoff["path"])) == handoff["sha256"], "handoff changed")
    artifact_root = run.cli.data_dir / "artifacts"
    shared = run.cli.documents()["sharedDir"]
    old_workspace = run.workspace(run.old)
    seeds = [Path(handoff["path"])]
    # Durable delivery content carries public attachment references, never provider_input_text.
    for message in raw["conversation_messages"]:
        content = message["delivery_content_json"]
        if content:
            for attachment in attachment_paths(decode(content)):
                seeds.append(Path(old_workspace) / attachment if not Path(attachment).is_absolute() else Path(attachment))
    directory_refs = []
    dependencies = preserve_dependencies(seeds, directory / "materials", [shared, artifact_root, old_workspace, run.root],
                                         directory_refs, {s["workspace"]["path"] for s in run.state["stops"]})
    run.state["preservation_paths"] = sorted({x["path"] for x in dependencies})
    old_preservation = capture_old(run)
    parent = run.state.get("manifest")
    manifest = {"schema": "rh-manifest/1", "run_id": run.run_id, "old_id": run.old,
                "conversation_id": conversation, "branch_id": branch, "cut_sequence": transcript["latestSequence"],
                "parent_sha256": parent["sha256"] if parent else None,
                "projection": {"scope": "active-lineage CLI projection", "pages": pages},
                "worker_index": {"path": str(run.root / "workers/index.json"), "sha256": run.state["stop_index_sha256"]},
                "old_preservation": old_preservation,
                "directory_references": directory_refs,
                "raw": {"path": str(directory / "conversation.json"), "sha256": digest(canonical(raw)),
                        "tables": list(EXPORT_COLUMNS)}, "dependencies": dependencies,
                "limitations": {"report_outbox": OUTBOX, "write_observation": "正常AO transcript可觀察範圍；非OS隔離或任意同UID程序保證"},
                "controls": [str(p) for p in (run.root / "control").glob("*/payload.json")], "created_at": now()}
    path = directory / "manifest.json"
    publish(path, canonical(manifest))
    reference = {"path": str(path), "sha256": digest(canonical(manifest))}
    run.state["delta" if delta else "manifest"] = reference
    run.save()
    return reference


def attachment_paths(value):
    paths = []
    if isinstance(value, dict):
        if value.get("type") in ("image", "file", "resource", "attachment", "file_reference") and isinstance(value.get("path"), str):
            paths.append(value["path"])
        if isinstance(value.get("fileReference"), dict):
            require(value["fileReference"].get("lines"), "file reference lacks captured original lines")
        if value.get("type") in ("image", "resource", "attachment") and isinstance(value.get("uri"), str) and value["uri"].startswith("file:"):
            uri = urllib.parse.urlparse(value["uri"])
            require(uri.netloc in ("", "localhost"), "nonlocal file URI refused")
            paths.append(urllib.parse.unquote(uri.path))
        for child in value.values():
            paths.extend(attachment_paths(child))
    elif isinstance(value, list):
        for child in value:
            paths.extend(attachment_paths(child))
    return paths


def status_command(root, run_id):
    return "python3 " + shlex.quote(str(Path(root) / "handoff.py")) + " status --run " + run_id


def selected_turn(snapshot):
    return max(snapshot["transcript"].get("turns", []), key=lambda t: t.get("requestedAt", ""), default={})


def same_idle(first, second, run=None):
    a, b = selected_turn(first), selected_turn(second)
    sequence = first["transcript"].get("latestSequence")
    return (type(sequence) is int and sequence == second["transcript"].get("latestSequence") and
            a.get("id") == b.get("id") and b.get("state") == "completed" and b.get("completedAt") and
            second["session"]["activity"]["state"] == "idle" and quiescent(second, run=run))


def reject_new_human(snapshot, after_sequence):
    require(type(after_sequence) is int, "UNKNOWN human input cutoff")
    for entry in entries(snapshot):
        if entry.get("role") == "user" and entry.get("origin") == "human":
            require(type(entry.get("sequence")) is int, "UNKNOWN human input sequence")
            require(entry["sequence"] <= after_sequence, "new human input requires operator recovery; no new authority inferred")


def idle_window(run, sid, phase):
    def ready():
        first = run.snapshot(sid)
        second = run.snapshot(sid)
        observed = getattr(run.cli, "last_received", run.clock())
        if not same_idle(first, second, run):
            return False
        # All observation persistence was completed before starting this timer.
        if run.clock() - observed > 1:
            return False
        return {"observed": observed, "sequence": second["transcript"]["latestSequence"],
                "turn_id": selected_turn(second)["id"], "snapshot": second}
    return run.wait(phase, ready)


def provider(target):
    return "claude" if target["harness"] == "claude-code" else "codex"


def target_preflight(cli, target):
    validate_target(target)
    require(b"--approval-mode" in cli.call(["spawn", "--help"]), "qualified CLI lacks --approval-mode")
    accounts = cli.read(["account", "ls", "--provider", provider(target), "--refresh"])["accounts"]
    matches = [a for a in accounts if a["id"] == target["account"] and a["provider"] == provider(target)]
    require(len(matches) == 1 and matches[0].get("email") and matches[0].get("signedIn") is True and
            not matches[0].get("problem"), "account lacks positive signed-in identity")
    windows = (matches[0].get("limits") or {}).get("windows", [])
    require(not any(w.get("usedPercent", 0) >= 100 for w in windows), "target account quota exhausted")
    query = urllib.parse.urlencode(dict(projectId=PROJECT, role="orchestrator", agentAccountId=target["account"], catalogOnly="true"))
    catalog = cli.get("agents/" + target["harness"] + "/models?" + query)
    require(catalog.get("accountBinding", {}).get("accountId") == target["account"], "catalog not bound to target account")
    models = [m for m in catalog.get("models", []) if m["id"] == target["model"]]
    require(len(models) == 1 and target["effort"] in models[0].get("efforts", []), "model/effort not positively cataloged")
    return {"account": matches[0], "catalog": catalog}


def verify_target(cli, sid, target, snapshot, database):
    session = snapshot["session"]
    require(session["id"] == sid and session["projectId"] == PROJECT and session["kind"] == "orchestrator" and
            session["harness"] == target["harness"] and session["mode"] == "chat" and not session["isTerminated"], "new identity mismatch")
    account = cli.get("agents/accounts/sessions/" + identifier(sid))
    binding = account.get("bindings", {}).get(provider(target), {})
    require(account.get(provider(target)) == target["account"] and binding.get("accountId") == target["account"] and
            binding.get("available") is True, "actual account binding mismatch")
    public = cli.get("sessions/" + sid + "/conversation")
    settings = public.get("settings", {})
    require(settings.get("approvalMode") == target["approval_mode"], "actual approval mismatch")
    require(settings.get("model") == target["model"] and settings.get("reasoningEffort") == target["effort"], "actual settings mismatch")
    native = native_identity(database, sid, target["harness"])
    require(native, "native id not yet observable")
    return {"native_id": native, "settings": settings, "account": account}


def known_turn(turn, target):
    settings = turn.get("agentSettings") or {}
    return (settings.get("state") == "known" and settings.get("settings", {}).get("model") == target["model"] and
            settings.get("settings", {}).get("effort") == target["effort"] and turn.get("providerTurnId"))


def match_spawn(sessions, token, intent_at):
    matches = [s for s in sessions if s["projectId"] == PROJECT and s["role"] == "orchestrator" and
               token in s.get("displayName", "")]
    require(len(matches) == 1, "UNKNOWN spawn: no unique exact token candidate")
    require(dt.datetime.fromisoformat(matches[0]["createdAt"].replace("Z", "+00:00")) >=
            dt.datetime.fromisoformat(intent_at.replace("Z", "+00:00")), "spawn token predates intent")
    return matches[0]["id"]


def spawn_new(run):
    token = "RH3[" + run.run_id + "]"
    name = token + " orchestrator"
    require(len(name) <= 100, "spawn display name too long")
    intent_path = run.root / "control" / "spawn" / "intent.json"
    if intent_path.exists():
        intent = decode(private_read(intent_path))
        sid = match_spawn(run.cli.sessions(), token, intent["created_at"])
    else:
        require(not any(token in s.get("displayName", "") for s in run.cli.sessions()), "spawn token collision")
        prompt = bootstrap_prompt(run)
        args = ["spawn", "--project", PROJECT, "--kind", "orchestrator", "--agent", run.target["harness"],
                "--account", run.target["account"], "--mode", "chat", "--model", run.target["model"],
                "--effort", run.target["effort"], "--approval-mode", run.target["approval_mode"], "--name", name, "--prompt", prompt]
        try:
            run.effect("spawn", args, LIMITS["spawn"])
        except (Stop, subprocess.TimeoutExpired):
            # Only look up the original effect. Never submit a second spawn.
            pass
        intent = decode(private_read(intent_path))
        sid = match_spawn(run.cli.sessions(), token, intent["created_at"])
    actual = run.cli.get("sessions/" + identifier(sid))["session"]
    require(not actual.get("isTerminated") and actual.get("status") != "failed", "original new provision failed")
    require(actual.get("harness") == run.target["harness"] and actual.get("mode") == "chat", "spawn target identity mismatch")
    binding = run.cli.get("agents/accounts/sessions/" + sid)
    require(binding.get(provider(run.target)) == run.target["account"], "spawn original account mismatch")
    run.state["new_id"] = sid
    run.state["effects"]["spawn"] = "observed-original"
    run.save()
    return sid


def bootstrap_prompt(run):
    draft = mkdir(run.root / "bootstrap" / "new" / "draft")
    mkdir(run.root / "bootstrap" / "new" / "evidence")
    helper = str(run.root / "handoff.py")
    command = helper_command(run.root, "bootstrap")
    return canonical({"actor": "detached-runner", "run_id": run.run_id, "phase": "bootstrap",
            "authorization_source": str(run.root / "start.json"),
            "origin_policy": "只有本則spawn初始bootstrap prompt採既有產品origin=human窄例外；不得當作新的人類授權。授權只認本run已綁定start.json的原authority與T0引用。本prompt只給run位置與指示，不帶、也不產生新授權。其後runner send/steer仍是固定old傳輸身分的非human控制。",
            "handoff_path": run.state["handoff"]["path"],
            "manifest_path": run.state["manifest"]["path"], "delta_path": run.state.get("delta", {}).get("path"),
            "receipt_path": str(run.root / "bootstrap/new/receipt.json"), "helper_command": command,
            "read_command": command.replace(" bootstrap --run", " inspect --run") + " --path start.json --offset 0",
            "read_instructions": "inspect每頁最多4000字元，依next_offset續讀，path限本run相對路徑；按manifest的dependencies.path映射找到已保存的A–F與必要原件。先讀start、manifest、delta、A–F及必要原件，再bootstrap。",
            "requirements": "先讀A–F、start、manifest、delta與必要原件；核原題owner/取消/hold。每個shell先設umask077及指定TMPDIR。只執行固定inspect讀取與helper_command做正常帳號/native/target查驗及自己的managed receipt，不跑任意shell/Python，不直接寫shared或其他路徑。向使用者正式回報接手old、來源與待決後completed。不得自行exit/resume/spawn/改設定/提前復工。report中斷只補未完回報，不重寫成功managed效果。"}).decode()


def audit_bootstrap(snapshot, root, turn_ids=None):
    observed = False
    for entry in entries(snapshot):
        if entry.get("kind") != "activity":
            continue
        if turn_ids is not None:
            if entry.get("turnId") and entry["turnId"] not in turn_ids:
                continue
            require(entry.get("turnId") or entry.get("activityKind") in ("usage", "system"), "UNVERIFIABLE_WRITE: missing bootstrap turn identity")
        require(entry.get("status") not in ("running", "pending", "failed"), "bootstrap unfinished/failed activity")
        kind, detail = entry.get("activityKind"), entry.get("detail", {})
        if kind in ("reasoning", "plan", "usage", "system", "auto_review"):
            continue
        if kind == "command":
            command = detail.get("rawCommand") or detail.get("command", "")
            identity = helper_identity(command, root, snapshot["session"]["id"])
            require(identity and identity[0] in ("bootstrap", "inspect"), "UNVERIFIABLE_WRITE: non-fixed bootstrap/inspect command")
            if identity[0] == "bootstrap":
                observed = True
            require(detail.get("exitCode") == 0 and not detail.get("outputTruncated"), "bootstrap command not positively complete")
        elif kind == "mcp_tool":
            tool = detail.get("toolName", "")
            args = detail.get("arguments", {})
            if isinstance(args, str):
                args = decode(args)
            acp_read = detail.get("protocol") == "acp" and detail.get("toolKind") == "read" and isinstance(detail.get("input"), dict)
            require(acp_read or (tool in ("read_file", "Read") and isinstance(args, dict)), "UNVERIFIABLE_WRITE: opaque tool")
        else:
            raise Stop("UNVERIFIABLE_WRITE: " + str(kind))
    require(observed, "bootstrap helper activity missing")


def bootstrap_origin(run, snapshot):
    intent = decode(private_read(run.root / "control/spawn/intent.json"))
    argv = intent["argv"]
    expected = argv[argv.index("--prompt") + 1]
    initial = [e for e in entries(snapshot) if e.get("kind") == "message" and e.get("role") == "user" and e.get("text") == expected]
    require(len(initial) == 1 and initial[0].get("origin") == "human" and not initial[0].get("senderSessionId"),
            "initial bootstrap does not match the sole spawn intent / narrow human-origin exception")
    message = initial[0]
    run.state["bootstrap_origin_exception"] = {"message_id": message["id"], "turn_id": message.get("turnId"),
        "origin": "human", "prompt_sha256": digest(expected.encode()), "grants_authority": False,
        "authorization_source": str(run.root / "start.json"), "start_sha256": run.state["start_sha256"]}
    require(type(message.get("sequence")) is int, "initial bootstrap sequence missing")
    run.state["bootstrap_sequence"] = message["sequence"]
    started = dt.datetime.fromisoformat(intent["created_at"].replace("Z", "+00:00"))
    owned = set()
    for turn in snapshot["transcript"].get("turns", []):
        owner = (turn.get("agentSettings") or {}).get("owner") or {}
        requested = turn.get("requestedAt")
        if owner.get("sessionId") == run.state["new_id"] or (requested and dt.datetime.fromisoformat(requested.replace("Z", "+00:00")) >= started):
            owned.add(turn["id"])
    require(message.get("turnId") in owned, "bootstrap turn not owned by new")
    run.save()
    return owned


def new_receipt(run):
    path = run.root / "bootstrap/new/receipt.json"
    if not path.exists():
        return False
    receipt = decode(private_read(path))
    require(Path(receipt.get("managed_reply", {}).get("path", "")).is_relative_to(run.root / "bootstrap/new"), "new reply outside fixed directory")
    require(receipt.get("schema") == "rh-new-receipt/1" and receipt.get("run_id") == run.run_id and
            receipt.get("old_id") == run.old and receipt.get("new_id") == run.state["new_id"] and
            receipt.get("target") == {k: run.target[k] for k in ("harness", "account", "model", "effort", "approval_mode")}, "new receipt identity/target")
    require(receipt.get("outcome") == "ready" and isinstance(receipt.get("created_at"), str) and
            isinstance(receipt.get("pending_question_refs"), list) and isinstance(receipt.get("diagnostics"), list), "new not ready/receipt fields")
    expected_sources = {"handoff_sha256": run.state["handoff"]["sha256"], "manifest_sha256": run.state["manifest"]["sha256"],
                        "delta_sha256": run.state.get("delta", {}).get("sha256")}
    require(receipt.get("sources") == expected_sources, "new source hashes mismatch")
    for item in receipt["diagnostics"]:
        require(all(key in item for key in ("code", "source", "stage", "classification", "required", "effect")), "diagnostic schema")
        require(item["classification"] in ("recorded-diagnostic", "historical") and item["effect"] != "unknown", "required error/WAIT/UNKNOWN")
    sid = run.state["new_id"]
    snap = run.snapshot(sid)
    owned_turns = bootstrap_origin(run, snap)
    require(receipt["evidence_turn_id"] in owned_turns, "new receipt borrows an earlier controller's turn")
    finished_turn = phase_completed(run, sid, "bootstrap", receipt["evidence_turn_id"], snap)
    if not finished_turn:
        continue_phase(run, sid, "bootstrap", receipt["evidence_turn_id"], snap)
        return False
    if not quiescent(snap, run=run):
        return False
    require(any(e.get("role") == "assistant" and e.get("turnId") == finished_turn and e.get("text", "").strip()
                for e in entries(snap)), "formal first report missing")
    actual = verify_target(run.cli, sid, run.target, snap, run.cli.data_dir / "ao.db")
    require(actual["native_id"] == receipt["native_id"] and actual["native_id"] != run.state["old_native"], "new native id mismatch")
    require(known_turn(next(t for t in snap["transcript"]["turns"] if t["id"] == finished_turn), run.target), "new completed settings UNKNOWN")
    verify_managed(receipt, run.run_id, dict(run.cli.documents(), lease=lease_view(run.cli.documents())))
    require(receipt["lease"]["session_id"] == sid and not receipt["lease"]["frozen"], "new not writer")
    audit_bootstrap(snap, run.root, owned_turns)
    run.state["new_binding"] = actual
    run.state["new_receipt"] = receipt
    run.save()
    return receipt


def nonce_completed(run):
    sid = run.state["new_id"]
    snap = run.snapshot(sid)
    reject_new_human(snap, run.state["bootstrap_sequence"])
    nonce = run.state["nonce"]
    control = run.run_id + ":nonce:" + sid
    prompts = [e for e in entries(snap) if e.get("clientMessageId") == control]
    require(len(prompts) <= 1, "nonce prompt ambiguous")
    if not prompts:
        return False
    turn_id = prompts[0]["turnId"]
    turns = {t["id"]: t for t in snap["transcript"].get("turns", [])}
    for entry in entries(snap):
        turn = turns.get(entry.get("turnId"), {})
        if (entry.get("role") == "assistant" and nonce in entry.get("text", "") and
                turn.get("state") == "completed" and turn.get("completedAt") and known_turn(turn, run.target)):
            require(entry.get("turnId") == turn_id or (run.state.get("phase_continuations", {}).get("nonce:" + turn_id) and
                    entry.get("turnId") == continuation_turn(run, sid, "nonce", snap)),
                    "nonce answer not tied to original challenge")
            actual = verify_target(run.cli, sid, run.target, snap, run.cli.data_dir / "ao.db")
            require(actual == run.state["new_binding"], "native/target/account drift after resume")
            run.state["nonce_completed"] = {"turn_id": turn["id"], "provider_turn_id": turn["providerTurnId"],
                                              "message_id": entry["id"], "observed_at": now()}
            run.save()
            return True
    continue_phase(run, sid, "nonce", turn_id, snap)
    return False


def run_protocol(run):
    state = run.state
    def stage(name, action):
        if name in state.setdefault("completed_stages", []):
            return
        state["stage"] = name
        state.setdefault("stages", {}).setdefault(name, {"started_at": now()})
        run.save()
        action()
        state["stages"][name]["completed_at"] = now()
        state["completed_stages"].append(name)
        run.save()
    stage("workers", lambda: run.wait("workers", run.workers))
    stage("2a", lambda: phase_2a(run))
    stage("export", lambda: run.wait("export", lambda: export_material(run)))

    def retire():
        if not (run.root / "control/kill/sent.json").exists():
            require(run.workers() and state["stop_index_sha256"] == state["phase_2a"]["worker_index_sha256"], "worker stop invalidated before kill")
            run.wait("export", lambda: export_material(run, delta=True))
        def guard():
            verify_old(run)
            observation = idle_window(run, run.old, "old_idle")
            reject_new_human(observation["snapshot"], state["old_control_sequence"])
            # No further reads or serialization between this check and invocation.
            require(run.clock() - observation["observed"] <= 1, "old idle window elapsed")
            return observation["observed"] + 1
        run.effect("kill", ["session", "kill", run.old, "--project", PROJECT], LIMITS["kill"], guard)
        def terminated():
            session = run.cli.read(["session", "get", run.old])["session"]
            return session.get("isTerminated") is True
        run.wait("old_idle", terminated)
        verify_old(run, postkill=True)
    stage("retire", retire)
    # Even a resumed run with retire already marked complete cannot bypass Q3.
    verify_old(run, postkill=True)
    stage("spawn", lambda: spawn_new(run))
    def native_ready():
        native = native_identity(run.cli.data_dir / "ao.db", state["new_id"], run.target["harness"])
        return native or False
    stage("native", lambda: run.wait("native", native_ready))
    stage("bootstrap", lambda: run.wait("bootstrap", lambda: new_receipt(run)))

    def resume():
        sid = state["new_id"]
        def guard():
            actual = verify_target(run.cli, sid, run.target, run.snapshot(sid), run.cli.data_dir / "ao.db")
            require(actual == state["new_binding"], "new binding changed before exit")
            observation = idle_window(run, sid, "new_idle")
            reject_new_human(observation["snapshot"], state["bootstrap_sequence"])
            require(run.clock() - observation["observed"] <= 1, "new idle window elapsed")
            return observation["observed"] + 1
        resume_directory = run.root / "control/resume"
        if not (resume_directory / "sent.json").exists() and not (resume_directory / "result.json").exists():
            run.effect("exit", ["session", "exit-agent", sid, "--project", PROJECT, "--json"], LIMITS["exit"], guard)
            exited = run.snapshot(sid)
            require(exited["session"]["activity"]["state"] == "exited" and quiescent(exited, run=run) and
                    selected_turn(exited).get("state") == "completed", "exit effect not observed or interrupted turn")
        response = run.effect("resume", ["session", "resume-agent", sid, "--project", PROJECT, "--json"], LIMITS["resume"])
        result = decode(response["stdout"])
        require(result.get("sessionId") == sid and result.get("resumeMode") == "native", "resume was not same-session native")
        actual = verify_target(run.cli, sid, run.target, run.snapshot(sid), run.cli.data_dir / "ao.db")
        require(actual == state["new_binding"], "resume binding/native/fast drift")
    stage("resume", resume)
    def nonce():
        if "nonce" not in state:
            state["nonce"] = "RH3-NONCE-" + str(uuid.uuid4())
            run.save()
        run.send(state["new_id"], "nonce", {"challenge": state["nonce"],
                 "requirements": "在同native的新completed回覆包含此唯一nonce；不要重發或建立另一個挑戰。"})
        run.wait("nonce", lambda: nonce_completed(run))
    stage("nonce", nonce)
    def finish():
        run.send(state["new_id"], "final", {"command": producer_command(run.root, "finalize"),
                 "requirements": "用自己的身份執行固定helper，只對當下本專案存活worker依start原授權送具名D或不帶D的FYI；既有hold不解除。保存接受/未讀/未執行，不等ACK。最後向使用者回報完成與待決，然後completed。"})
        def ready():
            path = run.root / "bootstrap/new/final.json"
            if not path.exists():
                return False
            receipt = decode(private_read(path))
            require(receipt.get("run_id") == run.run_id and receipt.get("new_id") == state["new_id"], "final receipt identity")
            return completed(run.snapshot(state["new_id"]), receipt["evidence_turn_id"])
        run.wait("final", ready)
    stage("final", finish)
    state.update(stage="complete", outcome="complete", finished_at=now())
    run.save()


def producer_command(root, action):
    return helper_command(root, action)


def producer(run, action):
    """Called only by new's own tool turn, never impersonated by the runner."""
    deadline = time.monotonic() + LIMITS["native"]
    while not run.state.get("new_id"):
        require(time.monotonic() < deadline and run.state.get("outcome") != "stopped", "new binding not observed; no producer effect")
        time.sleep(2)
        run.state = decode(private_read(run.root / "state.json"))
    sid = run.state["new_id"]
    require(os.environ.get("AO_SESSION_ID") == sid, "producer must run as actual new session")
    draft = run.root / "bootstrap/new/draft"
    require(os.environ.get("TMPDIR") == str(draft), "bootstrap TMPDIR mismatch")
    acting = CLI(run.cli.executable, sid, run.cli.data_dir, run.cli.run_file, draft)
    snap = acting.session(sid)
    running = [t for t in snap["transcript"].get("turns", []) if t["state"] == "running"]
    require(len(running) == 1, "producer needs unique actual running turn")
    turn = running[0]["id"]
    if action == "bootstrap":
        path = run.root / "bootstrap/new/receipt.json"
        if path.exists():
            return decode(private_read(path))
        handoff = run.state["handoff"]
        require(digest(read_regular(handoff["path"])) == handoff["sha256"], "new cannot read unchanged handoff")
        for key in ("manifest", "delta"):
            if run.state.get(key):
                manifest = decode(hash_reference(run.state[key]))
                hash_reference(manifest["worker_index"])
                for reference in manifest["dependencies"]:
                    hash_reference(reference)
                hash_reference(manifest["raw"])
        actual = verify_target(acting, sid, run.target, snap, acting.data_dir / "ao.db")
        require(known_turn(running[0], run.target), "bootstrap turn settings unknown")
        relative = "handoff-receipts/" + run.run_id + ".md"
        text = "handoff_run_id: " + run.run_id + "\nnew_id: " + sid + "\nold_id: " + run.old + "\n"
        text += "Sources: " + json.dumps({k: run.state.get(k) for k in ("handoff", "manifest", "delta")}, ensure_ascii=False) + "\n"
        text += "Pending/holds: " + json.dumps(run.request["workers"], ensure_ascii=False) + "\n"
        source = draft / "managed-receipt.md"
        publish(source, text.encode())
        documents = acting.documents()
        shared_path = Path(documents["sharedDir"]) / relative
        managed_path = run.root / "bootstrap/new/evidence/managed.json"
        intent = run.root / "bootstrap/new/evidence/managed-intent.json"
        require(not intent.exists(), "UNKNOWN managed write; do not rewrite")
        publish(intent, canonical({"path": relative, "sha256": digest(text.encode()), "actor": sid}))
        # This grant is used only by its owning new process for the managed call.
        capability = os.environ.get("AO_BROWSER_CAPABILITY")
        require(capability, "own managed capability missing")
        acting.env["AO_BROWSER_CAPABILITY"] = capability
        raw = acting.call(["project", "documents", "write", relative, "--project", PROJECT,
                           "--file", str(source), "--expected-hash", "missing", "--json"])
        del acting.env["AO_BROWSER_CAPABILITY"]
        publish(managed_path, raw)
        documents = acting.documents()
        receipt = {"schema": "rh-new-receipt/1", "run_id": run.run_id, "old_id": run.old, "new_id": sid,
                   "evidence_turn_id": turn, "created_at": now(),
                   "target": {k: run.target[k] for k in ("harness", "account", "model", "effort", "approval_mode")},
                   "native_id": actual["native_id"],
                   "sources": {"handoff_sha256": run.state["handoff"]["sha256"], "manifest_sha256": run.state["manifest"]["sha256"],
                               "delta_sha256": run.state.get("delta", {}).get("sha256")},
                   "managed_reply": {"path": str(managed_path), "sha256": digest(raw)},
                   "managed_receipt": {"path": str(shared_path), "sha256": digest(text.encode())},
                   "lease": lease_view(documents),
                   "pending_question_refs": [q for w in run.request["workers"] for q in w["pending_question_refs"]],
                   "diagnostics": [], "outcome": "ready"}
        verify_managed(receipt, run.run_id, dict(documents, lease=lease_view(documents)))
        publish(path, canonical(receipt))
        return receipt
    require(action == "finalize", "unsupported producer action")
    require(run.state.get("nonce_completed"), "cannot notify workers before native nonce verification")
    notifications = []
    for worker in acting.sessions():
        if worker["projectId"] != PROJECT or worker["role"] != "worker" or worker["isTerminated"]:
            continue
        wid = identifier(worker["id"])
        reject_new_human(acting.session(sid), run.state["bootstrap_sequence"])
        rules = next((x for x in run.request["workers"] if x["worker_id"] == wid), None)
        text = "FYI：新主控 " + sid + "，交接run " + run.run_id + "；既有hold維持，不推定復工授權。收到本通知後可恢復原回報規則。"
        if rules and rules["release"]["mode"] == "resume-original":
            text = rules["release"]["reserved_resume_d"] + "：新主控 " + sid + "；依原授權與原題續原工作。" + canonical(rules).decode()
        directory = run.root / "bootstrap/new/notifications" / wid
        intent, result = directory / "intent.json", directory / "result.json"
        if not result.exists():
            require(not intent.exists(), "UNKNOWN final notification; recover original handle")
            publish(intent, canonical({"worker_id": wid, "payload": text, "sha256": digest(text.encode()), "at": now()}))
            raw = acting.call(["send", "--session", wid, "--steer", "--client-message-id", run.run_id + ":release:" + wid, "--message", text], 60)
            publish(result, canonical({"accepted": True, "stdout": raw.decode(), "read": "UNKNOWN", "executed": "UNKNOWN"}))
        notifications.append({"worker_id": wid, "receipt": str(result), "accepted": True, "read": "UNKNOWN", "executed": "UNKNOWN"})
    final = {"run_id": run.run_id, "new_id": sid, "evidence_turn_id": turn, "notifications": notifications, "created_at": now()}
    publish(run.root / "bootstrap/new/final.json", canonical(final))
    return final


def lock(path):
    no_symlinks(path)
    mkdir(Path(path).parent)
    handle = open(path, "a+b")
    os.chmod(path, 0o600)
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError as exc:
        handle.close()
        raise Stop("another start/runner holds lock") from exc
    return handle


def locate(old=None, create=False):
    require(sys.platform == "linux", "Linux only")
    data = no_symlinks(os.environ.get("AO_DATA_DIR") or str(Path.home() / ".ao/data"))
    run_file = no_symlinks(os.environ.get("AO_RUN_FILE") or str(Path.home() / ".ao/running.json"))
    require(data.is_relative_to(Path.home() / ".ao") and run_file.is_relative_to(Path.home() / ".ao"), "AO state must remain under ~/.ao")
    installed = shutil.which("ao")
    require(installed, "ao CLI missing")
    # Resolve the explicitly installed executable, not arbitrary data symlinks.
    initial = CLI(Path(installed).resolve(), old or os.environ.get("AO_SESSION_ID", ""), data, run_file, data)
    documents = initial.documents()
    location = location_info(initial)
    shared = no_symlinks(documents["sharedDir"])
    require(str(shared) == location["shared_dir"], "shared outside fixed project data root")
    root = shared.parent / ".private-handoff-runs"
    if create:
        mkdir(root)
    else:
        require(root.is_dir() and stat.S_IMODE(root.stat().st_mode) == 0o700, "private run root missing or not 0700")
    return root, initial


def qualified(root, old, initial, temp):
    directory = root / ".qualified"
    qualification = decode(private_read(directory / "qualification.json"))
    executable = directory / "ao"
    require(digest(read_regular(executable)) == qualification.get("cli_sha256") and
            digest(read_regular(Path(__file__).absolute())) == qualification.get("script_sha256") and
            qualification.get("schema") == "rh-cli-qualification/1", "fixed CLI/script qualification mismatch")
    cli = CLI(executable, old, initial.data_dir, initial.run_file, temp)
    cli.script_sha256 = qualification["script_sha256"]
    return cli


def validate_sources_live(req, cli):
    snap = cli.session(req["old_id"])
    cli.old_snapshot_raw = getattr(cli, "last_raw", canonical(snap))
    session = snap["session"]
    require(session["id"] == req["old_id"] and session["projectId"] == PROJECT and session["kind"] == "orchestrator" and
            session["harness"] in ("codex", "claude-code") and session["mode"] == "chat" and not session["isTerminated"], "old identity invalid")
    all_entries = list(entries(snap))
    page = snap
    wanted = {s["message_id"] for s in req["sources"]}
    cursor = None
    while not wanted.issubset({e.get("id") for e in all_entries}) and page["transcript"]["hasMoreBefore"]:
        before = page["transcript"]["next"]["before"]
        require(type(before) is int and (cursor is None or before < cursor), "authority cursor did not advance")
        cursor = before
        page = cli.read(["session", "get", req["old_id"], "--transcript", "500", "--cap", "0", "--before", str(before)])
        require(page["transcript"]["conversationId"] == snap["transcript"]["conversationId"], "authority conversation changed")
        all_entries.extend(entries(page))
    for source in req["sources"]:
        require(source["conversation_id"] == snap["transcript"]["conversationId"], "source outside old conversation")
        matches = [e for e in all_entries if e.get("id") == source["message_id"]]
        require(len(matches) == 1 and matches[0].get("revision") == source["revision"] and
                digest(matches[0].get("text", "").encode()) == source["message_text_sha256"], "authority changed/missing")
        source_identity(source, matches[0])
        workspace_refs = {ref for row in req.get("workspace_index", []) for ref in row["source_refs"]}
        if source["kind"] == "old-control-record" and source["ref"] in workspace_refs:
            require(matches[0].get("turnId") == req["prepared_by"]["turn_id"], "live workspace record not from preparing turn")
    require(any(t["id"] == req["prepared_by"]["turn_id"] for t in snap["transcript"].get("turns", [])), "preparing turn not observed")
    return snap


def write_initial_state(root, req, raw, cli):
    state = {"schema": "rh-state/1", "run_id": req["request_id"], "project_id": PROJECT, "old_id": req["old_id"],
             "target": req["target"], "start_sha256": digest(raw), "authority_source_refs": req["authority"]["source_refs"],
             "worker_index_sha256": digest(canonical(req["workers"])), "original_t0": req["authority"],
             "cli_path": str(cli.executable), "cli_sha256": cli.sha256,
             "script_sha256": digest(private_read(root / "handoff.py")), "data_dir": str(cli.data_dir), "run_file": str(cli.run_file),
             "daemon_base": getattr(cli, "base", None),
             "location": location_info(cli), "retirement_sources": retirement_guard(root.parent, req["old_id"]),
             "created_at": now(), "stage": "prepared", "outcome": "pending", "effects": {}}
    publish(root / "state.json", canonical(state))


def launch(root, continuation=False):
    directory = mkdir(root / "runner-continuations" / str(uuid.uuid4())) if continuation else root
    intent = directory / "runner-intent.json"
    require(not intent.exists(), "runner launch already attempted; inspect original run")
    publish(intent, canonical({"at": now(), "script_sha256": digest(private_read(root / "handoff.py"))}))
    log = os.fdopen(os.open(no_symlinks(directory / "runner.log"), os.O_APPEND | os.O_CREAT | os.O_WRONLY, 0o600), "ab")
    env = {k: os.environ[k] for k in ("PATH", "HOME", "LANG", "LC_ALL", "USER", "AO_DATA_DIR", "AO_RUN_FILE") if k in os.environ}
    child = subprocess.Popen([sys.executable, "-I", str(root / "handoff.py"), "_run", "--run", root.name],
                             env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                             start_new_session=True, close_fds=True)
    log.close()
    publish(directory / "runner.json", canonical({"pid": child.pid, "sid": child.pid, "launched_at": now()}))


RETIRABLE_RUN = "33da56c1-a652-4d16-b1aa-1042d3322f3f"
ZERO_CONTROL_RUN = "ff137a22-e159-4603-ad70-1abd41681e5d"
RETIRABLE_RUNS = (RETIRABLE_RUN, ZERO_CONTROL_RUN)
RETIRABLE_WORKERS = ("agent-orchestrator-2", "agent-orchestrator-3")


def private_manifest(root):
    result = []
    for path in [root, *sorted(root.rglob("*"))]:
        no_symlinks(path)
        info = path.stat()
        require(path.is_dir() or path.is_file(), "retirement unsupported object")
        mode = stat.S_IMODE(info.st_mode)
        require(mode == (0o700 if path.is_dir() else 0o600), "retirement original permissions invalid")
        item = {"path": str(path.relative_to(root)), "directory": path.is_dir(), "mode": mode}
        if path.is_file():
            require(info.st_nlink == 1, "retirement hardlink refused")
            item.update(bytes=info.st_size, sha256=digest(path.read_bytes()))
        result.append(item)
    return result


def verify_retired(root, run_id, intent):
    directory = root / ".retirement" / run_id
    receipt_raw = private_read(directory / "receipt.json")
    receipt = decode(receipt_raw)
    manifest_raw = private_read(directory / "manifest.json")
    require(receipt.get("schema") == "rh-retired-run/1" and receipt.get("run_id") == run_id and
            receipt.get("old_id") == intent["old_id"] and receipt.get("closed_effects") == intent["closed_effects"] and receipt.get("intent_sha256") == digest(private_read(directory / "intent.json")) and
            receipt.get("manifest_sha256") == digest(manifest_raw) == intent["manifest_sha256"] and
            receipt.get("authorization_sha256") == digest(private_read(directory / "authorization.json")) == intent["authorization_sha256"],
            "retirement receipt binding invalid")
    manifest = decode(manifest_raw)
    require(digest(canonical(manifest["preconditions"])) == intent["preconditions_sha256"], "retirement precondition binding changed")
    require(Path(receipt["preconditions"]["path"]).parent == directory and Path(receipt["preconditions"]["path"]).name.startswith("preconditions-final-"), "retirement precondition reference outside fixed directory")
    hash_reference(receipt["preconditions"])
    final = decode(private_read(Path(receipt["preconditions"]["path"])))
    require(private_manifest(directory / "preconditions") == final and all(x in final for x in manifest["preconditions"]),
            "retirement precondition raw evidence changed")
    archive = no_symlinks(root / ".retired" / run_id)
    # The original run tree is immutable; the separately archived slot is the only added subtree.
    actual = [x for x in private_manifest(archive) if x["path"] != "retired-start-slot" and not x["path"].startswith("retired-start-slot/")]
    require(actual == manifest["run"] and private_manifest(archive / "retired-start-slot") == manifest["slot"], "retired original bytes/layout changed")
    for reference in manifest["saved"]:
        hash_reference(reference)
    return dict(path=str(directory / "receipt.json"), sha256=digest(receipt_raw), old_id=intent["old_id"])


def retirement_guard(root, old=None, run_id=None):
    if run_id:
        require(not (root / ".voided" / run_id).exists() and not (root / ".retired" / run_id).exists(), "retired UUID cannot execute")
    sources = []
    retired = no_symlinks(root / ".retired")
    if retired.exists():
        require(all(x.is_dir() and not x.is_symlink() and x.name in RETIRABLE_RUNS and
                    (root / ".retirement" / x.name / "intent.json").is_file() for x in retired.iterdir()),
                "retirement archive lacks original intent")
    directory = no_symlinks(root / ".retirement")
    if not directory.exists():
        return sources
    for item in directory.iterdir():
        no_symlinks(item)
        require(item.is_dir() and item.name in RETIRABLE_RUNS, "unknown retirement record")
        if not (item / "intent.json").exists():
            continue
        intent = decode(private_read(item / "intent.json"))
        require(intent.get("schema") == "rh-retirement-intent/1" and intent.get("run_id") == item.name and
                intent.get("old_id"), "invalid retirement intent")
        require(run_id != item.name, "retired UUID cannot execute")
        if old == intent["old_id"]:
            require((item / "receipt.json").exists(), "pending retirement blocks this old")
            sources.append(verify_retired(root, item.name, intent))
    return sources


def zero_control_history(root, run_directory, cli, authorization, state, request):
    require(state.get("effects") == {} and authorization.get("expected_effects") == [] and
            not (run_directory / "control").exists() and not state.get("new_id") and
            set(state.get("stages", {})) <= {"workers"} and
            state.get("error") == "sensitive workspace content cannot be preserved: cloud/.env.example",
            "zero-control retirement effects/phase UNKNOWN")
    for key in ("project_id", "target", "script_sha256", "cli_sha256"):
        require(authorization.get(key) == state[key], "zero-control authorization binding: " + key)
    require(authorization.get("qualification_sha256") == digest(private_read(root / ".qualified/qualification.json")),
            "zero-control original qualification binding")
    declared = {Path(source["raw_relpath"]) for source in request["sources"]}
    require(all(path.parts[0] == "evidence" and len(path.parts) > 1 for path in declared), "zero-control source scope")
    parents = {parent for path in declared for parent in path.parents}
    evidence = no_symlinks(run_directory / "evidence")
    if evidence.exists():
        require(evidence.is_dir(), "zero-control evidence type")
        for path in evidence.rglob("*"):
            no_symlinks(path)
            relative = path.relative_to(run_directory)
            require((path.is_file() and relative in declared) or (path.is_dir() and relative in parents),
                    "unknown zero-control source evidence")
    observations = no_symlinks(run_directory / "observations")
    actors = {state["old_id"], *RETIRABLE_WORKERS}
    labels = {"target-preflight", "worker-list"} | {prefix + sid for sid in actors for prefix in ("session-", "workspace-")}
    registrations, observed_registrations = set(), set()
    if observations.exists():
        require(observations.is_dir(), "zero-control observations type")
        for path in observations.iterdir():
            no_symlinks(path)
            require(path.is_file(), "unknown zero-control observation type")
            raw_label = next((sid for sid in actors if re.fullmatch(r"workspace-list-" + re.escape(sid) + r"-[0-9a-f]{32}\.raw", path.name)), None)
            if raw_label:
                registrations.add(path.name)
                continue
            label, _, nonce = path.stem.rpartition("-")
            require(path.suffix == ".json" and label in labels and re.fullmatch(r"[0-9a-f]{32}", nonce),
                    "unknown zero-control observation")
            value = decode(private_read(path))
            require(isinstance(value, list) if label == "worker-list" else isinstance(value, dict), "zero-control observation shape")
            if label == "worker-list":
                require(all(isinstance(row, dict) and "id" in row for row in value) and
                        {row["id"] for row in value} == set(RETIRABLE_WORKERS) and
                        all(row.get("projectId") == PROJECT and row.get("role") == "worker" for row in value), "zero-control worker-list source")
            elif label == "target-preflight":
                require(value.get("account", {}).get("id") == request["target"]["account"] and
                        value.get("catalog", {}).get("accountBinding", {}).get("accountId") == request["target"]["account"],
                        "zero-control target-preflight source")
            elif label.startswith("session-"):
                require(value.get("session", {}).get("id") == label[len("session-"):] and
                        value["session"].get("projectId") == PROJECT, "zero-control session observation source")
            else:
                sid = label[len("workspace-"):]
                binding = state.get("workspace_bindings", {}).get(sid)
                require(binding and value.get("session_id") == sid and value.get("path") == workspace_entry(request, sid)["path"] and
                        all(value.get(key) == expected for key, expected in binding.items()), "zero-control workspace observation source")
                reference = value["registration"]
                source = Path(reference["path"])
                require(source.parent == root / ZERO_CONTROL_RUN / "observations" and
                        re.fullmatch(r"workspace-list-" + re.escape(sid) + r"-[0-9a-f]{32}\.raw", source.name) and
                        digest(private_read(observations / source.name)) == reference["sha256"], "zero-control registration source")
                observed_registrations.add(source.name)
        require(registrations == observed_registrations, "unknown zero-control registration observation")
    # Only the observed partial before-snapshot layout is permitted, never a producer or dispatch trace.
    workers = run_directory / "workers"
    for actor in workers.iterdir() if workers.exists() else []:
        no_symlinks(actor)
        require(actor.name in RETIRABLE_WORKERS and actor.is_dir(), "unknown zero-control worker evidence")
        for before in actor.iterdir():
            no_symlinks(before)
            require(re.fullmatch(r"before-[0-9a-f]{32}", before.name) and before.is_dir(), "zero-control producer/phase evidence")
            for item in before.rglob("*"):
                no_symlinks(item)
                relative = item.relative_to(before)
                require((len(relative.parts) == 1 and ((item.name == "objects" and item.is_dir()) or
                        (item.name in ("index.patch", "worktree.patch") and item.is_file()))) or
                        (len(relative.parts) == 2 and relative.parts[0] == "objects" and
                         re.fullmatch(r"[0-9a-f]{64}", item.name) and item.is_file()), "unknown partial snapshot evidence")
    wire_paths = set()
    for metadata in (run_directory / "wire").glob("*.json"):
        wire = decode(private_read(metadata))
        raw = no_symlinks(wire["raw_path"])
        require(raw.parent == root / ZERO_CONTROL_RUN / "wire", "zero-control wire source outside run")
        actual = run_directory / "wire" / raw.name
        require(digest(private_read(actual)) == wire["sha256"], "zero-control wire bytes changed")
        wire_paths.update((metadata, actual))
        if "argv" in wire:
            args = wire["argv"]
            notice = ["report", "--needs-input", "--note", "RH3 " + ZERO_CONTROL_RUN + " STOP: " + state["error"][:650] +
                      "; " + status_command(root / ZERO_CONTROL_RUN, ZERO_CONTROL_RUN)]
            read_only = wire.get("returncode") == 0 and (args[:2] in (["account", "ls"], ["session", "get"], ["session", "ls"]) or
                        args[:3] == ["project", "documents", "info"] or args == ["spawn", "--help"])
            require(read_only or (args == notice and wire.get("returncode") in (0, 1)), "zero-control wire dispatch UNKNOWN")
        else:
            require(wire.get("path") in ("projects/" + PROJECT, "sessions/" + state["old_id"]) or
                    wire.get("path", "").startswith("agents/codex/models?"), "zero-control API phase UNKNOWN")
    require(not (run_directory / "wire").exists() or set((run_directory / "wire").iterdir()) == wire_paths,
            "unknown zero-control wire evidence")
    require(not any(p.name in ("intent.json", "sent.json", "result.json", "statement.json", "stop.json", "phase-2a.json", "preservation.json")
                    for p in run_directory.rglob("*")), "zero-control dispatch/producer evidence")
    previous = root / ".retirement" / RETIRABLE_RUN
    previous_intent = decode(private_read(previous / "intent.json"))
    verify_retired(root, RETIRABLE_RUN, previous_intent)
    receipt = decode(private_read(previous / "receipt.json"))
    require(receipt["old_id"] == state["old_id"] and len(receipt["closed_effects"]) == 2 and
            {x["worker_id"] for x in receipt["closed_effects"]} == set(RETIRABLE_WORKERS), "prior hold responsibility changed")
    archive = root / ".retired" / RETIRABLE_RUN
    for closed in receipt["closed_effects"]:
        sid = closed["worker_id"]
        snap = cli.session(sid)
        require(snap["session"].get("kind") == "worker" and snap["session"].get("projectId") == PROJECT and
                not snap["session"].get("isTerminated") and quiescent(snap), "zero-control worker terminal state UNKNOWN")
        payload = private_read(archive / "control" / ("worker-stop-" + sid) / "payload.json").decode()
        message = directive_entry(snap, closed["handle"], state["old_id"], payload)
        require(completed(snap, message["dispatch_turn_id"]) and message["id"] == closed["message_id"] and closed["held"],
                "zero-control original hold receipt changed")
        require(not any(e.get("sequence", 0) > message["sequence"] and
                        ((e.get("kind") == "message" and e.get("role") == "user") or
                         (e.get("kind") == "activity" and e.get("activityKind") == "system" and e.get("detail", {}).get("event") == "steer"))
                        for e in entries(snap)), "later zero-control worker directive UNKNOWN")
    require(all(w.get("reserved_stop_d") == "D-4" and w.get("hold_state") == "held" and
                w.get("release", {}).get("mode") == "keep-held" for w in request["workers"]) and
            {w["worker_id"] for w in request["workers"]} == set(RETIRABLE_WORKERS), "zero-control request hold/index changed")


def historical_retirement_checks(root, original, run_directory, cli, authorization):
    state = decode(private_read(run_directory / "state.json"))
    request_raw = private_read(run_directory / "start.json")
    request = decode(request_raw)
    old = state["old_id"]
    run_id = authorization["run_id"]
    require(state.get("schema") == "rh-state/1" and state.get("run_id") == run_id and state.get("project_id") == PROJECT and
            state.get("stage") == "workers" and state.get("outcome") == "stopped" and state.get("completed_stages") == [], "retirement history not workers/stopped")
    require(request.get("schema") == "rh-start-request/1" and request.get("request_id") == run_id and request.get("old_id") == old and
            request.get("target") == state["target"] and request.get("project_id") == PROJECT and
            digest(request_raw) == state["start_sha256"] == authorization["start_sha256"], "retirement start binding mismatch")
    require(digest(private_read(run_directory / "handoff.py")) == state["script_sha256"] and cli.sha256 == state["cli_sha256"] and
            state["cli_path"] == str(root / ".qualified/ao"), "historical fixed bytes mismatch")
    qualification = decode(private_read(root / ".qualified/qualification.json"))
    require(qualification == dict(schema="rh-cli-qualification/1", cli_sha256=state["cli_sha256"], script_sha256=state["script_sha256"]),
            "retirement original qualification mismatch")
    allowed = {"handoff.py", "start.json", "state.json", "old-initial-response.raw", "runner-intent.json", "runner.json", "runner.log", "runner.lock",
               "workers", "control", "wire", "evidence", "observations"}
    if run_directory == root / ".retired" / run_id:
        allowed.add("retired-start-slot")
    require(all(p.name in allowed for p in run_directory.iterdir()) and not (run_directory / "runner-continuations").exists(), "unexpected historical phase/producer")
    effects = {"worker-stop-" + sid for sid in RETIRABLE_WORKERS}
    if run_id == RETIRABLE_RUN:
        require(set(state.get("effects", {})) == effects and all(v in ("accepted", "observed-original") for v in state["effects"].values()) and
                set(p.name for p in (run_directory / "control").iterdir()) == effects and set(authorization["expected_effects"]) == effects,
                "retirement effects not exactly the two known stops")
    old_snapshot = cli.session(old)
    require(old_snapshot["session"].get("kind") == "orchestrator" and old_snapshot["session"].get("projectId") == PROJECT and
            not old_snapshot["session"].get("isTerminated") and quiescent(old_snapshot), "old responsibility/terminal state unknown")
    documents = cli.documents()
    require(documents.get("lease", {}).get("sessionId") == old and not documents.get("lease", {}).get("frozen"), "retirement old managed responsibility changed")
    live = [row for row in cli.sessions() if row["projectId"] == PROJECT and not row["isTerminated"]]
    require({row["id"] for row in live if row["role"] == "worker"} == set(RETIRABLE_WORKERS) and
            [row["id"] for row in live if row["role"] == "orchestrator"] == [old], "retirement live actor set changed")
    closed = []
    if run_id == ZERO_CONTROL_RUN:
        zero_control_history(root, run_directory, cli, authorization, state, request)
    for sid in RETIRABLE_WORKERS if run_id == RETIRABLE_RUN else ():
        control = run_directory / "control" / ("worker-stop-" + sid)
        payload_raw = private_read(control / "payload.json")
        payload = decode(payload_raw)
        intent = decode(private_read(control / "intent.json"))
        result = decode(private_read(control / "result.json"))
        private_read(control / "sent.json")
        handle = RETIRABLE_RUN + ":worker-stop:" + sid
        require(intent.get("argv") == ["send", "--session", sid, "--steer", "--client-message-id", handle, "--message", payload_raw.decode()] and
                result.get("sha256") == digest(result.get("stdout", "").encode()), "historical dispatch receipt mismatch")
        rules = next(x for x in request["workers"] if x["worker_id"] == sid)
        require(payload.get("run_id") == RETIRABLE_RUN and payload.get("old_id") == old and payload.get("actor") == "detached-runner" and
                payload.get("recipient") == sid and payload.get("phase") == "worker-stop" and payload["instruction"]["directive"] == rules["reserved_stop_d"] == "D-3" and
                payload["instruction"]["rules"] == rules and rules["release"]["mode"] == "keep-held", "historical directive/hold mismatch")
        snap = cli.session(sid)
        require(snap["session"].get("kind") == "worker" and snap["session"].get("projectId") == PROJECT and
                not snap["session"].get("isTerminated") and quiescent(snap), "worker not terminal/held")
        message = directive_entry(snap, handle, old, payload_raw.decode())
        require(completed(snap, message["dispatch_turn_id"]), "original D delivery/completion not proven")
        require(not any(e.get("sequence", 0) > message["sequence"] and
                        ((e.get("kind") == "message" and e.get("role") == "user") or
                         (e.get("kind") == "activity" and e.get("activityKind") == "system" and e.get("detail", {}).get("event") == "steer"))
                        for e in entries(snap)), "later worker directive/authority UNKNOWN")
        stop_raw = private_read(run_directory / "workers" / sid / "stop.json")
        stop = decode(stop_raw)
        # Deliberately do not validate/follow a historical /1 receipt as executable /2 evidence.
        require(stop.get("schema") == "rh-worker-stop/1" and stop.get("run_id") == RETIRABLE_RUN and stop.get("worker_id") == sid and
                stop.get("project_id") == PROJECT and stop.get("state") == "blocked" and stop.get("resume_state") == "held" and
                stop.get("evidence_turn_id") == message["dispatch_turn_id"], "historical blocked/hold facts mismatch")
        declared_materials = {(x["path"], x["sha256"]) for x in authorization.get("materials", []) if x.get("worker_id") == sid}
        for reference in stop.get("preservation", []):
            # Compare historical claims only. Read sources exclusively through the separately authorized actor-root list.
            if not Path(reference["path"]).is_relative_to(original / "workers" / sid):
                require((reference["path"], reference["sha256"]) in declared_materials, "necessary historical material not explicitly preserved")
        closed.append({"worker_id": sid, "handle": handle, "message_id": message["id"], "turn_id": message["dispatch_turn_id"],
                       "directive_received": "D-3", "stop_state": "blocked", "last_ack_d": stop.get("stop_directive", {}).get("last_ack_d"),
                       "stop_sha256": digest(stop_raw), "payload_sha256": digest(payload_raw), "held": True})
    runner = decode(private_read(run_directory / "runner.json"))
    require(runner.get("pid") == runner.get("sid") and type(runner.get("pid")) is int and runner["pid"] > 1 and
            runner.get("launched_at") and state.get("waits", {}).get("workers", {}).get("boot_id") == boot_id(), "runner PID/SID/boot provenance unknown")
    try:
        os.stat("/proc/" + str(runner["pid"]))
    except FileNotFoundError:
        pass
    except OSError as exc:
        raise Stop("runner liveness UNKNOWN: " + str(exc)) from exc
    else:
        raise Stop("runner PID alive or reused; do not force")
    require((run_directory / "runner-intent.json").is_file(), "original launch intent missing")
    for source in request["sources"]:
        require(digest(private_read(confined(run_directory, source["raw_relpath"]))) == source["raw_sha256"], "retirement source bytes changed")
    return state, request, closed


def retire_run(root, run_id, initial):
    require(run_id in RETIRABLE_RUNS, "only the named historical runs may retire")
    directory = mkdir(root / ".retirement" / run_id)
    authorization_raw = private_read(directory / "authorization.json")
    authorization = decode(authorization_raw)
    operator = identifier(os.environ.get("AO_SESSION_ID", ""))
    require(authorization.get("schema") == "rh-retirement-authorization/1" and authorization.get("run_id") == run_id and
            authorization.get("operator_session_id") == operator and authorization.get("decision_ref") and
            re.fullmatch(r"D-[0-9]+", authorization.get("directive_ref", "")), "explicit operator retirement authorization missing")
    old = identifier(authorization["old_id"])
    original, archive = root / run_id, root / ".retired" / run_id
    with lock(root / (".old-" + old + ".lock")):
        existing = directory / "intent.json"
        if (directory / "receipt.json").exists():
            verify_retired(root, run_id, decode(private_read(existing)))
            return decode(private_read(directory / "receipt.json"))
        if existing.exists():
            recovery = decode(private_read(directory / "recovery-authorization.json"))
            require(recovery.get("intent_sha256") == digest(private_read(existing)) and recovery.get("operator_session_id") == operator and
                    recovery.get("directive_ref") != authorization["directive_ref"] and re.fullmatch(r"D-[0-9]+", recovery.get("directive_ref", "")),
                    "pending retirement requires new named recovery authorization")
        require(existing.exists() or not archive.exists(), "retirement archive without original intent UNKNOWN")
        current = archive if archive.exists() else original
        require(not (archive.exists() and original.exists()), "retirement source/archive conflict")
        require((current / "runner.lock").is_file(), "historical runner lock missing")
        with lock(current / "runner.lock"):
            state = decode(private_read(current / "state.json"))
            require(state["old_id"] == old and state["run_file"] == str(initial.run_file) and state["data_dir"] == str(initial.data_dir),
                    "retirement root/old/data identity mismatch")
            cli = CLI(root / ".qualified/ao", old, initial.data_dir, initial.run_file, mkdir(directory / "preconditions"))
            cli.capture_root = mkdir(directory / "preconditions" / uuid.uuid4().hex)
            state, request, closed = historical_retirement_checks(root, original, current, cli, authorization)
            require(list(cli.capture_root.glob("*.raw")), "retirement precondition raw responses missing")
            slot = root / ".start" / old
            saved_slot = archive / "retired-start-slot"
            slot_source = saved_slot if saved_slot.exists() else slot
            require(private_read(slot_source / "request.json") == private_read(current / "start.json"), "retirement slot not the original request")
            preparation = dict(authorization_sha256=digest(authorization_raw), operator_session_id=operator,
                               old_id=old, start_sha256=state["start_sha256"], closed_effects=closed)
            manifest_path = directory / "manifest.json"
            if not existing.exists() and not manifest_path.exists():
                material_refs = []
                for item in authorization.get("materials", []):
                    sid = identifier(item["worker_id"])
                    require(sid in RETIRABLE_WORKERS, "retirement material actor mismatch")
                    workspace = no_symlinks(workspace_entry(request, sid)["path"])
                    rows = source_inputs(type("Sources", (), {"cli": cli})(), sid, workspace,
                                         [dict(path=item["path"], kind="historical-necessary-material", basis=item["basis"])])
                    require(rows[0]["sha256"] == item["sha256"], "retirement material changed")
                    refs, provenance = save_sources(mkdir(directory / "materials" / sid), rows)
                    material_refs.extend(refs)
                qualification_raw = private_read(root / ".qualified/qualification.json")
                publish(directory / "qualification.original.json", qualification_raw)
                publish(directory / "cli.original.bytes", read_regular(root / ".qualified/ao"))
                material_refs += [file_reference(directory / "qualification.original.json"), file_reference(directory / "cli.original.bytes")]
                manifest = {"run": private_manifest(original), "slot": private_manifest(slot), "saved": material_refs, "preconditions": private_manifest(directory / "preconditions"), "preparation": preparation}
                publish(directory / "manifest.json", canonical(manifest))
            manifest = decode(private_read(manifest_path))
            if existing.exists():
                intent = decode(private_read(existing))
            else:
                intent = {"schema": "rh-retirement-intent/1", "run_id": run_id, "old_id": old, "actor": "external-operator",
                          "operator_session_id": operator, "start_sha256": state["start_sha256"], "closed_effects": closed,
                          "preconditions_sha256": digest(canonical(manifest["preconditions"])),
                          "authorization_sha256": digest(authorization_raw), "manifest_sha256": digest(private_read(directory / "manifest.json")), "created_at": now()}
            require(intent["authorization_sha256"] == digest(authorization_raw) and intent["manifest_sha256"] == digest(private_read(manifest_path)) and
                    intent["closed_effects"] == closed and intent["preconditions_sha256"] == digest(canonical(manifest["preconditions"])),
                    "retirement original intent drift")
            require(manifest.get("preparation") == preparation, "retirement preparation binding drift")
            preconditions = private_manifest(directory / "preconditions")
            require(all(x in preconditions for x in manifest["preconditions"]), "retirement original preconditions changed")
            actual = [x for x in private_manifest(current) if x["path"] != "retired-start-slot" and not x["path"].startswith("retired-start-slot/")]
            require(actual == manifest["run"] and private_manifest(slot_source) == manifest["slot"], "retirement originals changed")
            for ref in manifest["saved"]:
                require(Path(ref["path"]).is_relative_to(directory), "retirement saved copy outside fixed directory")
                confined(directory, str(Path(ref["path"]).relative_to(directory)))
                hash_reference(ref)
            if not existing.exists():
                publish(existing, canonical(intent))
            mkdir(archive.parent)
            require(current.stat().st_dev == archive.parent.stat().st_dev == slot_source.stat().st_dev, "retirement filesystem mismatch")
            if current == original:
                require(not archive.exists(), "retirement archive already exists")
                os.rename(original, archive)
                sync_directory(root)
                sync_directory(archive.parent)
            if not saved_slot.exists():
                os.rename(slot, saved_slot)
                sync_directory(slot.parent)
                sync_directory(archive)
            if not slot.exists():
                mkdir(slot)
                sync_directory(slot.parent)
            else:
                require(not list(slot.iterdir()), "candidate slot changed during retirement")
            require(private_manifest(saved_slot) == manifest["slot"], "retired slot changed")
            final_preconditions = directory / ("preconditions-final-" + uuid.uuid4().hex + ".json")
            publish(final_preconditions, canonical(private_manifest(directory / "preconditions")))
            receipt = {"schema": "rh-retired-run/1", "run_id": run_id, "old_id": old, "actor": "external-operator",
                       "operator_session_id": operator, "authorization_sha256": intent["authorization_sha256"],
                       "intent_sha256": digest(private_read(existing)), "manifest_sha256": intent["manifest_sha256"],
                       "preconditions": file_reference(final_preconditions),
                       "closed_effects": closed, "original_run": str(original), "archive": str(archive), "hold": "unchanged", "created_at": now()}
            publish(directory / "receipt.json", canonical(receipt))
            verify_retired(root, run_id, intent)
            return receipt


def start(root, initial, old, target):
    with lock(root / (".old-" + identifier(old) + ".lock")):
        slot = root / ".start" / old
        raw = private_read(slot / "request.json")
        req = validate_request(decode(raw), slot, old, target)
        require(not no_symlinks(root / ".voided" / req["request_id"]).exists(), "request UUID is retired")
        retirement_guard(root, old, req["request_id"])
        destination = root / req["request_id"]
        if (destination / "start.json").exists():
            require(private_read(destination / "start.json") == raw, "UUID bound to different request")
            return decode(private_read(destination / "state.json"))
        for directory in root.iterdir():
            if directory.name.startswith(".") or not directory.is_dir():
                continue
            require((directory / "state.json").exists(), "incomplete prior run requires inspection")
            state = decode(private_read(directory / "state.json"))
            require(state["old_id"] != old or state["outcome"] == "complete", "old already has unresolved run")
        cli = qualified(root, old, initial, slot)
        old_snap = validate_sources_live(req, cli)
        preflight = target_preflight(cli, target)
        mkdir(destination)
        publish(destination / "start.json", raw)
        script = read_regular(Path(__file__).absolute())
        require(digest(script) == getattr(cli, "script_sha256", digest(script)), "qualified script changed during preflight")
        publish(destination / "handoff.py", script)
        for source in req["sources"]:
            publish(destination / source["raw_relpath"], private_read(confined(slot, source["raw_relpath"])))
        write_initial_state(destination, req, raw, cli)
        run = Run(destination, cli)
        run.workspace(old)
        for worker in run.live_workers():
            run.workspace(worker["id"])
        run.state["old_settings"] = old_settings(old_snap)
        source_raw = getattr(cli, "old_snapshot_raw", canonical(old_snap))
        publish(destination / "old-initial-response.raw", source_raw)
        run.state["old_settings"]["response_sha256"] = digest(source_raw)
        run.state["old_native"] = native_identity(cli.data_dir / "ao.db", old, old_snap["session"]["harness"])
        require(run.state["old_native"], "old native identity missing")
        run.evidence("target-preflight", preflight)
        run.save()
        launch(destination)
        return run.state


def load_run(root, run_id, initial):
    require(str(uuid.UUID(run_id)) == run_id, "canonical run UUID required")
    retirement_guard(root, run_id=run_id)
    directory = root / run_id
    state = decode(private_read(directory / "state.json"))
    retirement_guard(root, state["old_id"], run_id)
    require(state["script_sha256"] == digest(Path(__file__).read_bytes()), "run script changed")
    cli = CLI(state["cli_path"], state["old_id"], state["data_dir"], state["run_file"], directory)
    require(cli.sha256 == state["cli_sha256"], "run CLI changed")
    require(cli.base == state["daemon_base"], "run daemon location changed")
    return Run(directory, cli)


def main(argv=None):
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    begin = sub.add_parser("start")
    begin.add_argument("--old", required=True)
    for key in ("harness", "account", "model", "effort", "approval-mode"):
        begin.add_argument("--" + key, required=True)
    for action in ("status", "continue", "_run", "bootstrap", "finalize", "worker-receipt", "old-receipt", "retire"):
        sub.add_parser(action).add_argument("--run", required=True)
    inspect = sub.add_parser("inspect")
    inspect.add_argument("--run", required=True)
    inspect.add_argument("--path", required=True)
    inspect.add_argument("--offset", type=int, required=True)
    args = parser.parse_args(argv)
    try:
        entry = no_symlinks(Path(__file__).absolute()).parent
        if args.action != "start" and entry.name == args.run and entry.parent.name == ".private-handoff-runs":
            require(entry.is_relative_to(Path.home() / ".ao"), "bound entry outside AO state")
            root, initial = entry.parent, None
        else:
            root, initial = locate(getattr(args, "old", None), create=args.action == "start")
        if args.action == "start":
            target = {k: getattr(args, k) for k in ("harness", "account", "model", "effort", "approval_mode")}
            target["mode"] = "chat"
            result = start(root, initial, args.old, target)
        elif args.action == "status":
            require(str(uuid.UUID(args.run)) == args.run, "canonical run UUID required")
            result = decode(private_read(root / args.run / "state.json"))
            result["next_step"] = "只查此run；未明效果先核原handle，不再start/spawn。"
            result["responsibility"] = result.get("new_id") or result["old_id"]
            result["wait_remaining_seconds"] = {key: max(0, value["limit"] - (time.monotonic() - value["monotonic"]))
                if value["boot_id"] == boot_id() else "UNKNOWN: boot changed" for key, value in result.get("waits", {}).items()
                if "completed_at" not in value or key == result.get("waiting_phase")}
        elif args.action == "retire":
            result = retire_run(root, args.run, initial)
        else:
            run = load_run(root, args.run, initial)
            if args.action == "inspect":
                require(os.environ.get("AO_SESSION_ID") == run.state.get("new_id"), "inspect must run as new")
                require(args.offset >= 0, "invalid inspect offset")
                raw = private_read(confined(run.root, args.path))
                content = raw.decode()
                end = min(args.offset + 4000, len(content))
                result = {"path": args.path, "sha256": digest(raw), "total_chars": len(content),
                          "offset": args.offset, "next_offset": end if end < len(content) else None,
                          "text": content[args.offset:end]}
            elif args.action == "worker-receipt":
                result = worker_receipt(run)
            elif args.action == "old-receipt":
                result = old_receipt(run)
            elif args.action in ("bootstrap", "finalize"):
                result = producer(run, args.action)
            elif args.action == "continue":
                with lock(run.root / "runner.lock"):
                    if run.state["outcome"] != "complete":
                        run.rearm_wait()
                        launch(run.root, continuation=True)
                result = run.state
            else:
                with lock(run.root / "runner.lock"):
                    signal.signal(signal.SIGHUP, signal.SIG_IGN)
                    try:
                        run_protocol(run)
                    except (Stop, OSError, ValueError, TypeError, LookupError, sqlite3.Error, subprocess.TimeoutExpired) as exc:
                        run.state.update(outcome="stopped", error=str(exc), responsibility=run.state.get("new_id") or run.old)
                        run.save()
                        # Short notification only. It cannot change the original error/effect.
                        try:
                            run.cli.call(["report", "--needs-input", "--note", "RH3 " + run.run_id + " STOP: " + str(exc)[:650] + "; " + status_command(run.root, run.run_id)], 60)
                        except (Stop, OSError, subprocess.TimeoutExpired):
                            pass
                        raise
                result = run.state
        print(json.dumps(result, ensure_ascii=False, indent=2))
        if args.action == "start":
            print(status_command(root / result["run_id"], result["run_id"]))
        return 0
    except (Stop, OSError, ValueError, TypeError, LookupError, sqlite3.Error, subprocess.TimeoutExpired) as exc:
        print("STOP: " + str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
