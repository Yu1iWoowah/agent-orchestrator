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

# 1. Check every tracked manifest before Go can read local source through it.
while IFS= read -r -d '' manifest; do
	if python3 - "$manifest" <<'CHECK_GO'
import json, pathlib, subprocess, sys
root = pathlib.Path.cwd().resolve()
manifest = pathlib.Path(sys.argv[1])
def reject(message):
    print(f"{manifest}: {message}")
    sys.exit(1)
if not manifest.resolve().is_relative_to(root):
    reject("manifest is outside checkout")
kind = "work" if manifest.name == "go.work" else "mod"
module = json.loads(subprocess.check_output(["go", kind, "edit", "-json", str(manifest)], text=True))
# A new Go directive needs a boundary audit before this gate accepts it.
known = {"Go", "Toolchain", "Godebug", "Use", "Replace"} if kind == "work" else {
    "Module", "Go", "Toolchain", "Godebug", "Require", "Exclude", "Replace", "Retract", "Tool", "Ignore"
}
if module.keys() - known:
    reject("unreviewed Go fields: " + ", ".join(sorted(module.keys() - known)))
paths = [("use", entry["DiskPath"]) for entry in module.get("Use") or []]
paths += [("local replace", entry["New"]["Path"]) for entry in module.get("Replace") or [] if not entry["New"].get("Version")]
# Ignore only filters package discovery, but its directory spellings stay local too.
paths += [("ignore", entry["Path"]) for entry in module.get("Ignore") or []]
for directive, value in paths:
    target = (manifest.parent / value).resolve()
    if not target.is_relative_to(root):
        reject(f"{directive} targets outside checkout: {value}")
    if directive == "use" and not target.is_dir():
        reject(f"go.work uses {value}, which is not in this repository")
    if directive in {"use", "local replace"} and not (target / "go.mod").resolve().is_relative_to(root):
        reject(f"{directive} module manifest is outside checkout: {value}")
# Tool is an import path, not a directory target. Do not accept local spellings.
for tool in module.get("Tool") or []:
    value = tool["Path"]
    if value in {".", ".."} or value.startswith(("./", "../", "/", "\\")) or pathlib.PureWindowsPath(value).drive:
        reject(f"tool must be an import path: {value}")
CHECK_GO
	then :; else status=1; fi
done < <(git ls-files -z 'go.mod' '*/go.mod' 'go.work' '*/go.work')

# 2. The retained Cloud service must be declared and available locally.
if [[ ! -f cloud/go.mod ]] || ! go work edit -json go.work | python3 -c 'import json,sys; sys.exit("./cloud" not in [u["DiskPath"] for u in json.load(sys.stdin).get("Use") or []])'; then
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
