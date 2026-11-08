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
    if source["kind"] == "human-message":
        require(message.get("role") == "user" and message.get("origin") == "human", "not a structural human source")
    elif source["kind"] == "old-control-record":
        require(message.get("role") == "assistant", "not an old control record")
    else:
        require(source["kind"] == "structured-answer" and isinstance(source.get("ask_ref"), dict), "bad source kind/ask ref")
        require(message.get("origin") == "human" and message.get("role") == "user", "answer lacks human provenance")
        ask = source["ask_ref"]
        require(all(ask.get(k) is not None for k in ("id", "version", "answer_id")), "incomplete ask reference")
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
    return req


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


def quiescent(snapshot, settled_receipts=()):
    session, transcript = snapshot["session"], snapshot["transcript"]
    if session.get("activity", {}).get("state") not in ("idle", "exited"):
        return False
    if transcript.get("incomplete") or transcript.get("truncatedFields"):
        return False
    if any(t.get("state") in ("queued", "running") for t in transcript.get("turns", [])):
        return False
    for entry in transcript.get("entries", []):
        if entry.get("kind") != "activity":
            continue
        if entry.get("status") not in ("completed", "resolved", "cancelled", "failed", "recovered"):
            return False
        detail = canonical(entry.get("detail", {})).decode()
        if re.search(r"session_id|session ID|Script running|job[_ -]?id|still running|approval pending", detail, re.I):
            if not any(item["reference"] in detail and item["state"] in ("completed", "cancelled", "failed") for item in settled_receipts):
                return False
    return True


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

    def call(self, args, timeout=30, deadline=None):
        require(digest(self.executable.read_bytes()) == self.sha256, "qualified CLI changed")
        dispatched = time.monotonic()
        if deadline is not None and dispatched > deadline:
            raise WindowElapsed()
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
        return self.read(["session", "ls", "--all", "--include-terminated", "--project", PROJECT])["sessions"]

    def documents(self):
        return self.read(["project", "documents", "info", "--project", PROJECT])["documents"]


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
    mkdir(destination)
    values = {"head": git("rev-parse", "HEAD").decode().strip(),
              "branch": git("rev-parse", "--abbrev-ref", "HEAD").decode().strip(),
              "status": git("status", "--porcelain=v1", "-z", "--untracked-files=all").hex(),
              "index": git("ls-files", "--stage", "-z").hex(), "files": {}}
    for name, args in (("index.patch", ("diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv")),
                       ("worktree.patch", ("diff", "--binary", "--no-ext-diff", "--no-textconv"))):
        raw = git(*args)
        publish(Path(destination) / name, raw)
        values[name] = digest(raw)
    files = set(git("ls-files", "-z", "--cached", "--others", "--exclude-standard").split(b"\0")) - {b""}
    for raw_name in sorted(files):
        name = os.fsdecode(raw_name)
        source = confined(path, name)
        if not source.exists():
            values["files"][name] = {"missing": True}
            continue
        require(source.is_file(), "unsupported workspace object: " + name)
        require(not re.search(r"(^|/)(\.env(?:\..*)?|credentials|id_rsa|id_ed25519)$", name), "sensitive workspace content cannot be preserved: " + name)
        raw = source.read_bytes()
        sha = digest(raw)
        publish(Path(destination) / "objects" / sha, raw)
        values["files"][name] = {"sha256": sha, "mode": stat.S_IMODE(source.stat().st_mode)}
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


def old_git_state(run, plan):
    workspace = no_symlinks(run.workspace(run.old))
    git = lambda *args: old_git(args, workspace=workspace)
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
    require(not any(x[2:].startswith(b"160000 ") for x in index.split(b"\0")), "submodule dependencies unresolved")
    tracked = set(git("ls-files", "-z").split(b"\0")) - {b""}
    untracked = set(git("ls-files", "--others", "--exclude-standard", "-z").split(b"\0")) - {b""}
    ignored = set(git("ls-files", "--others", "--ignored", "--exclude-standard", "-z").split(b"\0")) - {b""}
    rules = plan.get("classification")
    require(isinstance(rules, list), "old classification required")
    used, classified, tree = set(), {}, {}
    for directory, dirs, names in os.walk(workspace, followlinks=False):
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
                require(not re.search(r"(^|/)(\.env(?:\..*)?|credentials|id_rsa|id_ed25519)(/|$)", relative), "credential copy refused")
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
    return {"workspace": str(workspace), "common": str(common), "metadata_path": str(metadata),
            "head": head, "original_ref": ref, "ref_tip": tip, "environment": environment,
            "status": git("status", "--porcelain=v1", "-z", "--untracked-files=all").hex(),
            "index": index.hex(), "staged_patch": git("diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv").hex(),
            "dirty_patch": git("diff", "--binary", "--no-ext-diff", "--no-textconv").hex(),
            "metadata": meta, "gitfile": read_regular(workspace / ".git").hex(), "tree": tree, "classification": classified,
            "plan_sha256": digest(private_read(run.root / "old/preservation.json"))}


def capture_old(run):
    plan = decode(private_read(run.root / "old/preservation.json"))
    require(plan.get("schema") == "rh-old-preservation/1" and plan.get("run_id") == run.run_id and
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
        require(not transcript.get("truncatedFields") and not transcript.get("incomplete"), "incomplete transcript")
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
        self.state.setdefault("effects", {})[key] = "UNKNOWN"
        self.save()
        publish(directory / "sent.json", canonical({"at": now(), "monotonic": self.clock()}))
        while True:
            deadline = before_call() if before_call else None
            try:
                raw = self.cli.call(args, timeout, deadline=deadline)
                break
            except WindowElapsed:
                # The adapter proved no subprocess was started. Reobserve S1/S2,
                # retaining the original bounded idle wait and mutation intent.
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
        workers = [s for s in self.cli.sessions() if s["projectId"] == PROJECT and s["kind"] == "worker" and not s["isTerminated"]]
        self.evidence("worker-list", workers)
        return workers

    def workspace(self, sid):
        session = self.cli.get("sessions/" + identifier(sid))["session"]
        require(session["id"] == sid and session["projectId"] == PROJECT, "workspace scope changed")
        path = session.get("workspacePath")
        require(path, "UNKNOWN registered workspace: " + sid)
        return path

    def worker_stop(self, worker):
        sid = identifier(worker["id"])
        rules = next((x for x in self.request["workers"] if x["worker_id"] == sid), None)
        directory = mkdir(self.root / "workers" / sid)
        path = directory / "stop.json"
        snap = self.snapshot(sid)
        if path.exists():
            stop = decode(private_read(path))
            for reference in [*stop.get("preservation", []), *[stop.get("workspace", {}).get(key, {}) for key in ("before_snapshot", "after_snapshot")],
                              *[{"path": item.get("evidence_path", "")} for item in stop.get("settled", {}).get("in_flight", [])]]:
                require(Path(reference.get("path", "")).is_relative_to(directory), "worker reference outside fixed directory")
            validate_stop(stop, self.run_id, sid)
            if stop["state"] != "safe":
                return False
            if stop["source_type"] == "worker-self":
                require(rules and rules["reserved_stop_d"], "self stop lacks original reserved directive")
                require(stop["stop_directive"]["last_ack_d"] == rules["reserved_stop_d"], "worker stop D does not match reserved directive")
                if not completed(snap, stop["evidence_turn_id"]):
                    return False
            require(quiescent(snap, stop["settled"]["in_flight"]), "worker safe receipt invalidated: " + sid)
            require(stop["workspace"]["path"] == self.workspace(sid), "stop workspace differs from registered workspace")
            actual = git_snapshot(stop["workspace"]["path"], directory / ("verify-" + uuid.uuid4().hex))
            require(actual["sha256"] == stop["workspace"]["after_snapshot"]["sha256"], "worker content drift: " + sid)
            return stop
        if quiescent(snap):
            workspace = self.workspace(sid)
            before = git_snapshot(workspace, directory / ("before-" + uuid.uuid4().hex))
            self.sleep(2)
            after = git_snapshot(workspace, directory / ("after-" + uuid.uuid4().hex))
            if before["sha256"] != after["sha256"] or not quiescent(self.snapshot(sid)):
                return False
            stop = {"schema": "rh-worker-stop/1", "run_id": self.run_id, "project_id": PROJECT,
                    "worker_id": sid, "observed_at": now(), "source_type": "script-quiescent",
                    "state": "safe", "block_reason": None, "evidence_turn_id": None,
                    "stop_directive": {"last_ack_d": None, "next_d": rules["reserved_stop_d"] if rules else None},
                    "settled": {"running_turn_ids": [], "queued_turn_ids": [], "pending_activity_ids": [], "in_flight": []},
                    "workspace": {"path": workspace, "branch": after["branch"], "head": after["head"],
                                  "before_snapshot": before, "after_snapshot": after},
                    "preservation": [dict(before, kind="workspace-snapshot"), dict(after, kind="workspace-snapshot")], "pending_question_refs": rules["pending_question_refs"] if rules else [],
                    "authority_ref": None, "resume_state": "held", "qualification": "未經worker自述"}
            publish(path, canonical(stop))
            return stop
        if rules and rules["reserved_stop_d"]:
            self.send(sid, "worker-stop", {"directive": rules["reserved_stop_d"], "output": str(path),
                      "schema": "rh-worker-stop/1", "template": worker_template(self.run_id, sid),
                      "rules": rules, "requirements": "收束當前操作及外部job，保全ref/dirty/原題；私有原子發布JSON，evidence_turn_id填本turn；completed後hold。放行前只用ao report --needs-input，safe後不送checkpoint/done，不用--stuck。無法證明即pending/blocked，不可自填完成。"})
        return False

    def workers(self):
        results = []
        for worker in self.live_workers():
            result = self.worker_stop(worker)
            if not result:
                return False
            results.append(result)
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
    return {"schema": "rh-worker-stop/1", "run_id": run_id, "project_id": PROJECT, "worker_id": sid,
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


def validate_stop(stop, run_id, sid):
    require(stop.get("schema") == "rh-worker-stop/1" and stop.get("run_id") == run_id and
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
    for item in settled["in_flight"]:
        require(isinstance(item.get("reference"), str) and item["reference"] and isinstance(item.get("state"), str), "invalid operation reference")
        raw = hash_reference({"path": item["evidence_path"], "sha256": item["sha256"]})
        receipt = decode(raw)
        if stop["state"] == "safe":
            require(item["state"] in ("completed", "cancelled", "failed") and receipt.get("reference") == item["reference"] and
                    receipt.get("state") == item["state"], "external operation lacks terminal evidence")
    workspace = stop["workspace"]
    require(all(key in workspace for key in ("path", "branch", "head", "before_snapshot", "after_snapshot")), "workspace fields")
    for key in ("before_snapshot", "after_snapshot"):
        hash_reference(workspace[key])
    require(workspace["before_snapshot"]["sha256"] == workspace["after_snapshot"]["sha256"], "worker snapshots differ")
    for reference in stop["preservation"]:
        require(isinstance(reference.get("kind"), str) and reference["kind"], "preservation kind missing")
        hash_reference(reference)


# Explicit value whitelist. In particular, never SELECT * from sessions/turns.
EXPORT_COLUMNS = {
    "conversation_branches": "id conversation_id session_id provider_conversation_id parent_branch_id fork_after_turn_id replaced_turn_id replacement_turn_id fork_after_sequence created_at strategy replay_cutoff_sequence replay_truncated provider_scope_id provider_ids_scoped review_id handoff_note".split(),
    "conversation_messages": "id conversation_id turn_id sequence revision role origin text streaming provider_item_id client_message_id created_at updated_at delivery_content_json branch_id client_payload_hash sender_session_id sender_project_id sender_display_name continuation".split(),
    "conversation_turns": "id conversation_id handled_by_session_id provider_turn_id controller_generation state error_message requested_at started_at completed_at diff_json rolled_back_at plan_json branch_id promotion_started_at promoted_to_turn_id retry_of_turn_id handled_by_review_id usage_limit_resets_at usage_limit_auto_resume usage_limit_resume_error batched_input intake_sequence".split(),
    "conversation_activities": "id conversation_id turn_id sequence revision kind status summary detail_json request_id provider_item_id created_at updated_at command_output command_output_truncated streamed_text streamed_text_truncated branch_id".split(),
}


def export_conversation(database, old, conversation, harness):
    path = no_symlinks(database)
    uri = path.as_uri() + "?mode=ro"
    with sqlite3.connect(uri, uri=True, timeout=30) as db:
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA query_only=ON")
        db.execute("BEGIN")
        session = db.execute("SELECT id,project_id,harness,provider_conversation_id FROM sessions WHERE id=?", (old,)).fetchone()
        require(session and session["project_id"] == PROJECT and session["harness"] == harness, "DB source identity mismatch")
        scope = db.execute("SELECT id,project_id,current_session_id,latest_sequence FROM conversations WHERE id=?", (conversation,)).fetchone()
        require(scope and scope["project_id"] == PROJECT and scope["current_session_id"] == old, "DB conversation scope changed")
        result = {"session": dict(session), "conversation": dict(scope)}
        for table, columns in EXPORT_COLUMNS.items():
            # Qualified schema only; an unknown column never widens the read.
            rows = db.execute("SELECT " + ",".join(columns) + " FROM " + table + " WHERE conversation_id=? ORDER BY id", (conversation,))
            result[table] = [dict(row) for row in rows]
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
    instruction = {"output": str(path), "worker_index_sha256": run.state["stop_index_sha256"],
                   "worker_index": {"path": str(run.root / "workers/index.json"), "sha256": run.state["stop_index_sha256"]},
                   "start": str(run.root / "start.json"), "schema": "rh-old-2a/1",
                   "requirements": "以自己的managed documents write及expected-hash更新A–F，正文帶handoff_run_id；讀worker_index與各stop原件，必要檔案引用其run內snapshot/objects固定原件，不拿可變worktree檔案當舊原件。保存原CLI JSON、handoff path/hash、lease及本turn id。依preservation要求原子發布分類，不得自填completed。向使用者列run-id、責任人與ref/dirty的切前證據／預期處置，不能預先宣告kill後已保留；附status及10分鐘靜默提示，然後completed。不要輪詢或等待runner。",
                   "preservation": {"output": str(run.root / "old/preservation.json"), "schema": "rh-old-preservation/1",
                       "fields": "run_id/old_id/evidence_turn_id/original_ref/classification[]",
                       "classification": "逐項path/disposition(copy|rebuildable|discardable)/basis。可重建或可丟棄目錄可加scope=directory涵蓋全樹，不能憑檔名推定。copy必須列精確regular file及copy={path,sha256}，副本固定在old/saved/，原mode另存證據；保留相對關係。",
                       "requirements": "閉合所有untracked/ignored及空目錄；必要唯一內容先驗hash副本。original_ref須既存refs/heads並在common repository保護HEAD，不新增ref。submodule/巢狀repo/未知per-worktree metadata需人工核清，腳本停止不猜。PreRemove非空亦停止。"},
                   "user_status_instruction": status_command(run.root, run.run_id),
                   "silence_instruction": "從這則ready回覆起10分鐘仍未看到新主控正式接手首報，請查原run status，不再start/spawn。",
                   "template": {"schema": "rh-old-2a/1", "run_id": run.run_id, "old_id": run.old,
                                "evidence_turn_id": "<actual turn>", "created_at": "<UTC>",
                                "managed_reply": {"path": str(run.root / "old" / "managed.json"), "sha256": "<hash>"},
                                "handoff": {"path": str(Path(documents["sharedDir"]) / documents["documentPath"]), "sha256": "<hash>"},
                                "lease": {"session_id": run.old, "epoch": 0, "frozen": False},
                                "worker_index_sha256": run.state["stop_index_sha256"], "pending_question_refs": [],
                                "user_status_instruction": status_command(run.root, run.run_id)}}
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
        replies = [e.get("text", "") for e in entries(snap) if e.get("role") == "assistant" and e.get("turnId") == finished_turn]
        require(any(status_command(run.root, run.run_id) in text and "10" in text for text in replies), "old ready reply lacks exact status/silence instruction")
        controls = [e for e in entries(snap) if e.get("clientMessageId") == run.run_id + ":2a:" + run.old]
        payload = private_read(run.root / "control" / ("2a-" + run.old) / "payload.json").decode()
        require(len(controls) == 1 and controls[0].get("origin") == "automation" and controls[0].get("senderSessionId") == run.old and
                controls[0].get("text") == "[from " + run.old + "] " + payload, "2a original control provenance mismatch")
        run.state["old_control_sequence"] = controls[0]["sequence"]
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
    if not quiescent(snapshot):
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
    conversation, branch = transcript["conversationId"], transcript["activeBranchId"]
    pages, cursor = [], None
    directory = mkdir(run.root / ("delta" if delta else "cut") / uuid.uuid4().hex)
    while True:
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
    raw = export_conversation(run.cli.data_dir / "ao.db", run.old, conversation, snap["session"]["harness"])
    require(raw["conversation"]["latest_sequence"] == transcript["latestSequence"], "conversation changed during export; retain partial evidence")
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


def same_idle(first, second):
    a, b = selected_turn(first), selected_turn(second)
    sequence = first["transcript"].get("latestSequence")
    return (type(sequence) is int and sequence == second["transcript"].get("latestSequence") and
            a.get("id") == b.get("id") and b.get("state") == "completed" and b.get("completedAt") and
            second["session"]["activity"]["state"] == "idle" and quiescent(second))


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
        if not same_idle(first, second):
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
    matches = [s for s in sessions if s["projectId"] == PROJECT and s["kind"] == "orchestrator" and
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
    command = "umask 077; export TMPDIR=" + shlex.quote(str(draft)) + "; python3 " + shlex.quote(helper) + " bootstrap --run " + run.run_id
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
    helper = str(root / "handoff.py")
    expected = "umask 077; export TMPDIR=" + shlex.quote(str(root / "bootstrap/new/draft")) + "; python3 " + shlex.quote(helper)
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
            expected_command = expected + " bootstrap --run " + root.name
            if not command.strip().startswith(expected + " "):
                wrapper = shlex.split(command)
                require(len(wrapper) == 3 and Path(wrapper[0]).name in ("bash", "sh", "zsh") and
                        wrapper[1] in ("-c", "-lc"),
                        "UNVERIFIABLE_WRITE: non-fixed bootstrap command")
                command = wrapper[2]
            if command.strip() == expected_command:
                observed = True
            else:
                require(command.startswith(expected + " "), "UNVERIFIABLE_WRITE: non-fixed helper")
                args = shlex.split(command[len(expected) + 1:])
                require(len(args) == 7 and args[:3] == ["inspect", "--run", root.name] and args[3] == "--path" and
                        args[5] == "--offset" and args[6].isdigit(), "UNVERIFIABLE_WRITE: non-fixed inspect")
                confined(root, args[4])
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
            export_material(run, delta=True)
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
        run.effect("exit", ["session", "exit-agent", sid, "--project", PROJECT, "--json"], LIMITS["exit"], guard)
        exited = run.snapshot(sid)
        require(exited["session"]["activity"]["state"] == "exited" and quiescent(exited) and
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
    return "umask 077; export TMPDIR=" + shlex.quote(str(root / "bootstrap/new/draft")) + "; python3 " + shlex.quote(str(root / "handoff.py")) + " " + action + " --run " + root.name


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
        if worker["projectId"] != PROJECT or worker["kind"] != "worker" or worker["isTerminated"]:
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
    data = no_symlinks(os.environ.get("AO_DATA_DIR", str(Path.home() / ".ao/data")))
    run_file = no_symlinks(os.environ.get("AO_RUN_FILE", str(Path.home() / ".ao/running.json")))
    require(data.is_relative_to(Path.home() / ".ao") and run_file.is_relative_to(Path.home() / ".ao"), "AO state must remain under ~/.ao")
    installed = shutil.which("ao")
    require(installed, "ao CLI missing")
    # Resolve the explicitly installed executable, not arbitrary data symlinks.
    initial = CLI(Path(installed).resolve(), old or os.environ.get("AO_SESSION_ID", ""), data, run_file, data)
    documents = initial.documents()
    shared = no_symlinks(documents["sharedDir"])
    require(shared.is_relative_to(data), "shared outside AO data root")
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
    require(any(t["id"] == req["prepared_by"]["turn_id"] for t in snap["transcript"].get("turns", [])), "preparing turn not observed")
    return snap


def write_initial_state(root, req, raw, cli):
    state = {"schema": "rh-state/1", "run_id": req["request_id"], "project_id": PROJECT, "old_id": req["old_id"],
             "target": req["target"], "start_sha256": digest(raw), "authority_source_refs": req["authority"]["source_refs"],
             "worker_index_sha256": digest(canonical(req["workers"])), "original_t0": req["authority"],
             "cli_path": str(cli.executable), "cli_sha256": cli.sha256,
             "script_sha256": digest(private_read(root / "handoff.py")), "data_dir": str(cli.data_dir), "run_file": str(cli.run_file),
             "daemon_base": getattr(cli, "base", None),
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


def start(root, initial, old, target):
    with lock(root / (".old-" + identifier(old) + ".lock")):
        slot = root / ".start" / old
        raw = private_read(slot / "request.json")
        req = validate_request(decode(raw), slot, old, target)
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
    directory = root / run_id
    state = decode(private_read(directory / "state.json"))
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
    for action in ("status", "continue", "_run", "bootstrap", "finalize"):
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
