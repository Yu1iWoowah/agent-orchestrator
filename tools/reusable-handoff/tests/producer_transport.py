"""Synthetic actor transport. Tool detail is produced by Go, never patched here."""
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import threading
import urllib.request
import urllib.error


def activity(snapshot, command, stdout, exit_code, actor, turn, identifier, sequence):
    endpoint = os.environ.get("RH_ACTIVITY_PRODUCER")
    if not endpoint:
        raise RuntimeError("RH flow tests require README's CLI + both driver producer gate")
    body = dict(snapshot=snapshot, command=command, stdout=stdout, exit_code=exit_code,
                actor=actor, turn=turn, id=identifier, sequence=sequence)
    request = urllib.request.Request(endpoint, data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=60) as response:
        return json.load(response)


def directive(snapshot, messages, active, rebatch=False):
    endpoint = os.environ["RH_ACTIVITY_PRODUCER"]
    body = dict(snapshot=snapshot, actor=snapshot["session"]["id"], directive=dict(active=active, messages=messages, rebatch=rebatch))
    request = urllib.request.Request(endpoint + "/directive", data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    try:
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=60) as response:
            return json.load(response)
    except urllib.error.HTTPError as exc:
        raise RuntimeError(exc.read().decode()) from exc


class ActorTransport:
    def __init__(self, fixture):
        self.fixture = fixture
        self.root = fixture.data_dir / "synthetic-transport"
        self.root.mkdir(mode=0o700)
        outer = self
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                path = self.path.split("/api/v1/", 1)[1]
                raw = json.dumps(outer.fixture.get(path)).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(raw)
            def log_message(self, *args):
                pass
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.config = self.root / "transport.json"
        self.cli = self.root / "ao"
        code = '''import sys,json,pathlib,hashlib,os
p=pathlib.Path(CONFIG);s=json.loads(p.read_text());a=sys.argv[1:]
if a[:2]==['session','get']:
 fd=os.open(p.parent/'readers.jsonl',os.O_WRONLY|os.O_APPEND|os.O_CREAT,0o600)
 with os.fdopen(fd,'a') as out:out.write(json.dumps(dict(actor=os.environ.get('AO_SESSION_ID'),argv=a))+'\\n')
if a[:2]==['session','get']:print(json.dumps(s['sessions'][a[2]]))
elif a[:3]==['project','documents','info']:print(json.dumps(s['documents']))
elif a[:3]==['project','documents','write']:
 source=pathlib.Path(a[a.index('--file')+1]);text=source.read_text();relative=a[3];dest=pathlib.Path(s['documents']['documents']['sharedDir'])/relative;dest.parent.mkdir(mode=0o700,parents=True,exist_ok=True);dest.write_text(text);dest.chmod(0o600)
 reply=s['managed'];reply['file'].update(path=relative,content=text,hash=hashlib.sha256(text.encode()).hexdigest(),size=len(text.encode()));print(json.dumps(reply))
elif a[:2]==['session','ls']:print(json.dumps(s['list']))
elif a[0]=='send':print(s['send'])
else:raise SystemExit('unimplemented synthetic CLI transport: '+str(a))
'''.replace("CONFIG", repr(str(self.config)))
        self.cli.write_text("#!" + sys.executable + "\n" + code)
        self.cli.chmod(0o700)
        self.run_file = fixture.data_dir.parent / "running.json"
        self.run_file.write_text(json.dumps(dict(port=self.server.server_port, pid=os.getpid())))
        self.run_file.chmod(0o600)
        self.base = "http://127.0.0.1:" + str(self.server.server_port) + "/api/v1/"

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def sync(self):
        from test_wire_contract import wire, wire_bytes
        f = self.fixture
        value = dict(sessions=f.snapshots, documents=dict(documents=f.documents()), managed=wire("managed"),
                     list=dict(data=f.sessions(), meta=wire("list")["meta"]), send=wire_bytes("send").decode())
        self.config.write_text(json.dumps(value))
        self.config.chmod(0o600)

    def execute(self, actor, action, command=None):
        from test_handoff import h
        f = self.fixture
        directory = h.helper_directory(f.run.root, action, actor)
        h.mkdir(directory / "draft")
        command = command or h.helper_command(f.run.root, action, actor)
        argv = ["/bin/sh", "-c", command]
        raw_command = shlex.join(argv)
        snap = f.snapshots[actor]
        turn = snap["transcript"]["turns"][-1]
        turn.update(state="running")
        turn.pop("completedAt", None)
        snap["session"]["activity"]["state"] = "active"
        snap["session"]["status"] = "working"
        sequence = max([snap["transcript"]["latestSequence"], *[e.get("sequence", 0) for e in snap["transcript"]["entries"]]]) + 1
        identifier = action + "-tool"
        f.snapshots[actor] = activity(snap, raw_command, "", None, actor, turn["id"], identifier, sequence)
        self.sync()
        f.run.save()
        journal = h.Journal(f.data_dir, f.run.run_id)
        if not (journal.root / "binding.json").exists():
            # Scenario preparation ends before the first real actor execution.
            journal.bind(dict(root=str(f.run.root.parent), old_id=f.run.old,
                              start_sha256=h.digest(h.private_read(f.run.root / "start.json")),
                              qualification_sha256=h.digest(h.private_read(f.run.root.parent / ".qualified/qualification.json"))))
        env = {k: v for k, v in os.environ.items() if k in ("PATH", "LANG", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL")}
        env.update(HOME=str(f.home), TMPDIR=str(directory / "draft"), AO_DATA_DIR=str(f.data_dir), AO_RUN_FILE=str(self.run_file),
                   AO_SESSION_ID=actor, AO_PROJECT_ID=h.PROJECT, AO_BROWSER_CAPABILITY="synthetic-capability")
        result = subprocess.run(argv, env=env, cwd=f.workspace if actor == "old" else f.worker_path if actor == "worker" else f.repo,
                                capture_output=True, text=True, timeout=60)
        # No post-producer replacement of output/content/flags: raw result is an INPUT to Go.
        projected = activity(f.snapshots[actor], raw_command, result.stdout, result.returncode, actor, turn["id"], identifier, sequence)
        f.snapshots[actor] = projected
        capture = h.mkdir(self.root / "helper-evidence" / str(len(f.helper_events)))
        h.publish(capture / "stdout", result.stdout.encode())
        h.publish(capture / "stderr", result.stderr.encode())
        h.publish(capture / "invocation.json", h.canonical(dict(action=action, argv=argv, exit=result.returncode,
                  stdout_sha256=h.digest(result.stdout.encode()), stderr_sha256=h.digest(result.stderr.encode()))))
        h.publish(capture / "producer-transcript.json", h.canonical(projected))
        f.helper_events.append(dict(action=action, argv=argv, stdout=result.stdout, exit=result.returncode, snapshot=projected, capture_path=capture))
        self.sync()
        if result.returncode:
            raise h.Stop(result.stderr.strip())
        turn = f.snapshots[actor]["transcript"]["turns"][-1]
        turn.update(state="completed", completedAt=h.now())
        f.snapshots[actor]["session"]["activity"]["state"] = "idle"
        f.snapshots[actor]["session"]["status"] = "idle"
        # Recompute normal CLI incomplete/turn state through the same real producer and formatter.
        f.snapshots[actor] = activity(f.snapshots[actor], raw_command, result.stdout, result.returncode, actor, turn["id"], identifier, sequence)
        self.sync()
        return json.loads(result.stdout)
