#!/usr/bin/env python3
"""Offline Codex JSON-RPC fixture for the shared custody binary probe."""

import sys, json, os

if "--version" in sys.argv:
    print("codex-cli 0.165.0")
    raise SystemExit()
if "login" in sys.argv:
    print("Logged in using an API key")
    raise SystemExit()
log = os.environ["S0_PROVIDER_LOG"]
turn_number = 0
for line in sys.stdin:
    try:
        q = json.loads(line)
    except:
        continue
    with open(log, "a") as f:
        f.write(json.dumps(q) + "\n")
    if "id" not in q:
        continue
    m = q.get("method", "")
    result = {}
    if m == "initialize":
        result = {"userAgent": "s0/fake", "codexHome": os.environ["HOME"] + "/.codex"}
    if m == "model/list":
        result = {
            "data": [
                {
                    "id": "gpt-test",
                    "model": "gpt-test",
                    "displayName": "GPT Test",
                    "isDefault": True,
                    "defaultReasoningEffort": "medium",
                    "supportedReasoningEfforts": [
                        {"reasoningEffort": "medium", "description": "test"}
                    ],
                }
            ]
        }
    if m == "config/read":
        result = {
            "config": {
                "model": "gpt-test",
                "model_reasoning_effort": "medium",
                "service_tier": "flex",
            }
        }
    if m == "account/read":
        result = {"account": {"type": "apiKey"}, "requiresOpenaiAuth": False}
    if m in ("thread/start", "thread/resume", "thread/read"):
        result = {
            "thread": {
                "id": "thread-1",
                "turns": [],
                "cwd": os.environ["S0_WORKSPACE"],
            },
            "model": "gpt-test",
            "modelProvider": "openai",
            "cwd": os.environ["S0_WORKSPACE"],
            "approvalPolicy": "on-request",
            "sandbox": {
                "type": "workspaceWrite",
                "writableRoots": [os.environ["S0_WORKSPACE"]],
            },
        }
    if m == "turn/start":
        turn_number += 1
        result = {
            "turn": {
                "id": "ordinary-provider-turn-" + str(turn_number),
                "status": "inProgress",
                "items": [],
            }
        }
    print(json.dumps({"jsonrpc": "2.0", "id": q["id"], "result": result}), flush=True)
    if m == "turn/start":
        print(
            json.dumps(
                {
                    "jsonrpc": "2.0",
                    "method": "turn/completed",
                    "params": {
                        "threadId": "thread-1",
                        "turn": {
                            "id": result["turn"]["id"],
                            "status": "completed",
                            "items": [],
                        },
                    },
                }
            ),
            flush=True,
        )
