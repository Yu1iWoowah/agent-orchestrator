#!/usr/bin/env python3
"""Exercise an extracted Linux AO binary on a synthetic S0 schema, offline.

The selected binary runs only with a fresh private HOME/data/worktree, an
isolated loopback port and a fake provider. No installed AO data is opened.
Goose transaction/fault tests remain in input_custody_migration_test.go; this
fixture replays empty-schema SQL solely to build a cross-binary test database.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def main():
    os.umask(0o022)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--scratch-parent", type=Path, required=True)
    parser.add_argument("--prefix", default="s0-")
    parser.add_argument(
        "--keep",
        action="store_true",
        help="Keep the synthetic fixture after stopping its daemon",
    )
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[1]
    binary = args.binary.resolve(strict=True)
    d = Path(tempfile.mkdtemp(prefix=args.prefix, dir=args.scratch_parent))
    try:
        exercise(repo, binary, d)
    finally:
        if args.keep:
            print("Fixture:", d)
        else:
            shutil.rmtree(d)


def exercise(repo, binary, d):
    label = hashlib.sha256(binary.read_bytes()).hexdigest()
    for part in ["h", "t", "data", "bin", "ws"]:
        (d / part).mkdir(mode=0o700)
    subprocess.run(["git", "init", "-q", "-b", "main", str(d / "ws")], check=True)
    (d / "ws/fixture").write_text("S0 synthetic workspace")
    subprocess.run(["git", "-C", str(d / "ws"), "add", "fixture"], check=True)
    subprocess.run(
        [
            "git",
            "-C",
            str(d / "ws"),
            "-c",
            "user.name=S0",
            "-c",
            "user.email=s0@example.invalid",
            "commit",
            "-qm",
            "fixture",
        ],
        check=True,
    )
    workspace = d / "data/worktrees/compat/compat-1"
    workspace.parent.mkdir(parents=True, mode=0o700)
    subprocess.run(
        [
            "git",
            "-C",
            str(d / "ws"),
            "worktree",
            "add",
            "-qb",
            "ao/compat",
            str(workspace),
            "main",
        ],
        check=True,
    )
    c = sqlite3.connect(d / "data/ao.db")
    c.execute("PRAGMA synchronous=OFF")
    versions = []
    for f in sorted(
        (repo / "backend/internal/storage/sqlite/migrations").glob("*.sql"),
        key=lambda f: int(f.name.split("_")[0]),
    ):
        if int(f.name.split("_")[0]) == 20261101152248341:
            c.execute("PRAGMA foreign_keys=OFF")
        c.executescript(
            f.read_text().split("-- +goose Up", 1)[1].split("-- +goose Down")[0]
        )
        versions.append(int(f.name.split("_")[0]))
        c.execute("PRAGMA foreign_keys=ON")
    c.execute(
        "CREATE TABLE goose_db_version(id INTEGER PRIMARY KEY AUTOINCREMENT,version_id BIGINT NOT NULL,is_applied BOOLEAN NOT NULL,tstamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP)"
    )
    c.execute("INSERT INTO goose_db_version(version_id,is_applied) VALUES(0,1)")
    c.executemany(
        "INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)",
        [(v,) for v in versions],
    )
    c.execute(
        "INSERT INTO projects(id,path,registered_at) VALUES('compat',?,CURRENT_TIMESTAMP)",
        (str(d / "ws"),),
    )
    c.execute(
        "INSERT INTO sessions(id,project_id,num,kind,harness,session_mode,activity_last_at,created_at,updated_at,workspace_path,controller_generation,provider_conversation_id,hibernated_at) VALUES('compat-1','compat',1,'orchestrator','codex','chat',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,?,'generation-1','thread-1',CURRENT_TIMESTAMP)",
        (str(workspace),),
    )
    c.execute(
        "UPDATE sessions SET branch='ao/compat',workspace_repo_path=workspace_path WHERE id='compat-1'"
    )
    c.execute(
        "INSERT INTO conversations(id,scope,project_id,current_session_id,created_at,updated_at,latest_sequence,active_branch_id) VALUES('conversation','project','compat','compat-1',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,3,'branch')"
    )
    c.execute(
        "INSERT INTO conversation_branches(id,conversation_id,session_id,provider_conversation_id,created_at) VALUES('branch','conversation','compat-1','thread-1',CURRENT_TIMESTAMP)"
    )
    for seq, tid in enumerate(["held", "unknown", "ordinary-queued"], 1):
        c.execute(
            "INSERT INTO conversation_turns(id,conversation_id,handled_by_session_id,controller_generation,state,requested_at,branch_id,intake_sequence,provider_turn_id) VALUES(?,'conversation','compat-1','generation-1',?,CURRENT_TIMESTAMP,'branch',?,?)",
            (
                tid,
                "running" if tid == "unknown" else "queued",
                seq,
                "preexisting-provider-turn" if tid == "unknown" else "",
            ),
        )
        c.execute(
            "INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,created_at,updated_at,branch_id) VALUES(?,'conversation',?,?,'user','human',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'branch')",
            (
                "message-" + tid,
                tid,
                seq,
                "ORDINARY_QUEUED" if tid == "ordinary-queued" else "NEVER_SEND_" + tid,
            ),
        )
        c.execute(
            "INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,target_owner,created_at) VALUES(?,?,'register','',0,'settings',CURRENT_TIMESTAMP)",
            ("register-" + tid, tid),
        )
        if tid == "ordinary-queued":
            continue
        c.execute(
            "INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,hold_key,hold_owner,reason,created_at) VALUES(?,?,'hold','settings',1,'delivery','settings',?,CURRENT_TIMESTAMP)",
            (
                "hold-" + tid,
                tid,
                "outcome_unknown" if tid == "unknown" else "wake_blocked",
            ),
        )
    c.commit()

    def snapshot():
        return {
            table: c.execute("SELECT * FROM " + table + " ORDER BY 1").fetchall()
            for table in [
                "conversation_input_custody",
                "conversation_input_holds",
                "conversation_input_actions",
            ]
        } | {
            "inputs": c.execute(
                "SELECT t.id,t.state,t.provider_turn_id,t.controller_generation,m.text,m.sequence,m.turn_id FROM conversation_turns t JOIN conversation_messages m ON m.turn_id=t.id WHERE t.id IN ('held','unknown') ORDER BY t.id"
            ).fetchall()
        }

    before = snapshot()
    shutil.copyfile(repo / "test/fixtures/input-custody-codex.py", d / "bin/codex")
    (d / "bin/codex").chmod(0o700)
    (d / "h/.codex").mkdir(mode=0o700)
    (d / "h/.codex/auth.json").write_text(
        json.dumps({"OPENAI_API_KEY": "s0-fake-no-network"})
    )
    (d / "h/.codex/auth.json").chmod(0o600)
    e = dict(os.environ)
    for k in list(e):
        if (
            k.startswith("AO_")
            or k in ("CODEX_HOME", "CLAUDE_CONFIG_DIR")
            or any(
                (
                    x in k
                    for x in ("API_KEY", "AUTH_TOKEN", "ACCESS_TOKEN", "SECRET_KEY")
                )
            )
        ):
            e.pop(k, None)
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    e.update(
        HOME=str(d / "h"),
        TMPDIR=str(d / "t"),
        TMUX_TMPDIR=str(d / "t"),
        PATH=str(d / "bin") + ":/usr/bin:/bin",
        AO_DATA_DIR=str(d / "data"),
        AO_RUN_FILE=str(d / "run.json"),
        AO_PORT=str(port),
        AO_TELEMETRY_EVENTS="off",
        AO_TELEMETRY_REMOTE="off",
        S0_PROVIDER_LOG=str(d / "provider.jsonl"),
        S0_WORKSPACE=str(workspace),
    )
    f = (d / "daemon.log").open("w")
    proc = subprocess.Popen(
        [str(binary), "daemon"], env=e, stdout=f, stderr=subprocess.STDOUT
    )

    def req(path, body=None):
        try:
            r = urllib.request.urlopen(
                urllib.request.Request(
                    "http://127.0.0.1:" + str(port) + path,
                    data=None if body is None else json.dumps(body).encode(),
                    headers={"Content-Type": "application/json"},
                ),
                timeout=25,
            )
            return (r.status, r.read().decode())
        except urllib.error.HTTPError as x:
            return (x.code, x.read().decode())

    try:
        for _ in range(100):
            if proc.poll() is not None:
                raise RuntimeError(
                    "daemon exited "
                    + str(proc.returncode)
                    + " "
                    + (d / "daemon.log").read_text()[-4000:]
                )
            try:
                status, body = req("/api/v1/sessions")
                if status == 200:
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.1)
        else:
            raise RuntimeError("daemon not ready")
        results = {}
        for path, body in [
            ("/api/v1/sessions/compat-1/conversation", None),
            ("/api/v1/sessions/compat-1/conversation/turns/held/retry", {}),
            ("/api/v1/sessions/compat-1/conversation/turns/unknown/retry", {}),
            (
                "/api/v1/sessions/compat-1/conversation/messages",
                {"text": "ORDINARY_CAN_SEND", "clientMessageId": "ordinary-new"},
            ),
        ]:
            status, response = req(path, body)
            results[path] = (status, response[:1000])
            print(label, path, status, response[:450], flush=True)
        time.sleep(2)
        status, response = req("/api/v1/sessions/compat-1/exit-agent", {})
        results["exit-agent"] = (status, response[:1000])
        print(label, "exit-agent", status, response[:350], flush=True)
        after = snapshot()
        print(label, "held facts unchanged", before == after, flush=True)
        calls = (
            [json.loads(x) for x in (d / "provider.jsonl").read_text().splitlines()]
            if (d / "provider.jsonl").exists()
            else []
        )
        starts = [x for x in calls if x.get("method") == "turn/start"]
        print(label, "turn starts", len(starts), json.dumps(starts)[:1000], flush=True)
        assert results["/api/v1/sessions/compat-1/conversation"][0] == 200, results
        for tid in ["held", "unknown"]:
            status, response = results[
                "/api/v1/sessions/compat-1/conversation/turns/" + tid + "/retry"
            ]
            assert (
                status == 409
                and json.loads(response)["code"] == "CHAT_RETRY_NOT_RETRYABLE"
            ), results
        assert results["/api/v1/sessions/compat-1/conversation/messages"][0] == 202, (
            results
        )
        assert results["exit-agent"][0] == 200, results
        assert before == after, "legacy cleanup mutated held inputs or receipts"
        dispatched = json.dumps(starts)
        assert (
            starts
            and "ORDINARY_QUEUED" in dispatched
            and ("ORDINARY_CAN_SEND" in dispatched)
        ), dispatched
        assert "NEVER_SEND_" not in dispatched, dispatched
        assert any(
            (
                x.get("method") == "thread/resume"
                and x.get("params", {}).get("threadId") == "thread-1"
                for x in calls
            )
        ), "same-native thread not resumed"
        print(
            json.dumps(
                {
                    "binarySha256": label,
                    "read": 200,
                    "retry": 409,
                    "send": 202,
                    "exitAgent": 200,
                    "heldUnchanged": True,
                    "heldProviderStarts": 0,
                    "ordinaryProviderStarts": len(starts),
                    "sameNativeResume": True,
                }
            )
        )
    finally:
        try:
            req("/api/v1/sessions/compat-1/exit-agent", {})
        except Exception:
            pass
        try:
            req("/shutdown", {})
        except Exception:
            pass
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.terminate()
            proc.wait(timeout=10)
        f.close()
        c.close()
        print(label, "own daemon stopped", proc.returncode, flush=True)


if __name__ == "__main__":
    main()
