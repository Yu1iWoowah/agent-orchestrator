#!/usr/bin/env bash
# Everything installed must build from modules in this checkout. The fork
# retains its Cloud service as a separate local module; desktop code reaches
# it through contracts/cloud/openapi.yaml, not private Go imports.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

status=0
fail() {
	echo "public boundary: $*" >&2
	status=1
}

# 1. go.work may only use directories that exist in this checkout.
if [[ -f go.work ]]; then
	while read -r dir; do
		[[ -d "$dir" ]] || fail "go.work uses $dir, which is not in this repository"
	done < <(go work edit -json | python3 -c 'import json,sys; [print(u["DiskPath"]) for u in json.load(sys.stdin).get("Use") or []]')
fi

# 2. The retained Cloud service must be declared and available locally.
if [[ ! -f cloud/go.mod ]] || ! go work edit -json | python3 -c 'import json,sys; sys.exit("./cloud" not in [u["DiskPath"] for u in json.load(sys.stdin).get("Use") or []])'; then
    fail "the retained cloud/ module must exist in this checkout and go.work"
fi

# 3. No public Go code may import the control plane's module.
if matches="$(git grep -lE '"github\.com/aoagents/agent-orchestrator/cloud(/|")' -- '*.go' ':!cloud/**' || true)" && [[ -n "$matches" ]]; then
	fail "Go files import the control plane module:"$'\n'"$matches"
fi

# 4. npm scripts may use the retained tree, never an external private checkout.
while IFS= read -r manifest; do
	if python3 - "$manifest" <<'PY'
import json, re, sys
scripts = json.load(open(sys.argv[1])).get("scripts") or {}
bad = [name for name, cmd in scripts.items() if 'ao-control-plane' in cmd]
if bad:
    print(f"{sys.argv[1]}: scripts {', '.join(bad)} reference an external private control plane")
    sys.exit(1)
PY
	then :; else status=1; fi
done < <(git ls-files 'package.json' '*/package.json' | grep -v node_modules)

if [[ "$status" == 0 ]]; then
	echo "public boundary: ok"
fi
exit "$status"
