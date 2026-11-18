import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';

const backendModule = 'module github.com/aoagents/agent-orchestrator/backend\n\ngo 1.27.4\n\nrequire github.com/google/uuid v1.6.0\n';
const work = 'go 1.27.4\n\nuse (\n ./backend\n ./cloud\n ./components/hcg\n)\n';
const cases = [
  ['module godebug directive', root => writeFileSync(path.join(root, 'backend/go.mod'), backendModule + 'godebug default=go1.27\n'), null],
  ['workspace godebug directive', root => writeFileSync(path.join(root, 'go.work'), work + 'godebug default=go1.27\n'), null],
  ['retained fork modules and in-tree or versioned replacements', root => writeFileSync(path.join(root, 'go.work'), work + '\nreplace github.com/aoagents/agent-orchestrator/cloud v0.1.0 => ./cloud\nreplace example.com/module => example.org/public v1.0.0\n'), null],
  ['in-tree paths and import-based tools', root => {
    mkdirSync(path.join(root, 'backend/generated'), { mode: 0o700 });
    symlinkSync(path.join(root, 'backend'), path.join(root, 'linked'), 'dir');
    writeFileSync(path.join(root, 'go.work'), work + 'use ./linked\n');
    writeFileSync(path.join(root, 'backend/go.mod'), backendModule + 'ignore ./generated\ntool github.com/google/uuid\n');
  }, null],
  ['missing cloud module', root => rmSync(path.join(root, 'cloud/go.mod')), /retained cloud/],
  ['missing workspace module', root => writeFileSync(path.join(root, 'go.work'), work + 'use ./missing\n'), /go.work uses/],
  ['public code importing cloud', root => writeFileSync(path.join(root, 'backend/bad.go'), 'package backend\nimport _ "github.com/aoagents/agent-orchestrator/cloud/internal/httpapi"\n'), /Go files import/],
  ['npm script pointing to private checkout', root => writeFileSync(path.join(root, 'package.json'), JSON.stringify({ scripts: { cloud: 'cd ../ao-control-plane && go build' } })), /external private control plane/],
  ['workspace relative replace escaping checkout', root => writeFileSync(path.join(root, 'go.work'), work + '\nreplace github.com/google/uuid => ../ao-control-plane/uuid\n'), /go.work: local replace targets outside checkout/],
  ['workspace absolute replace escaping checkout', root => writeFileSync(path.join(root, 'go.work'), work + '\nreplace github.com/google/uuid => /opt/private/ao-control-plane/uuid\n'), /go.work: local replace targets outside checkout/],
  ...['relative', 'absolute', 'symlink'].map(form => [`existing external workspace use (${form})`, (root, outside) => {
    if (form === 'symlink') symlinkSync(outside, path.join(root, 'linked'), 'dir');
    const target = form === 'relative' ? '../outside' : form === 'symlink' ? './linked' : outside;
    writeFileSync(path.join(root, 'go.work'), work + `use ${JSON.stringify(target)}\n`);
  }, /go.work: use targets outside checkout/]),
  ...['module', 'workspace'].map(kind => [`${kind} replace through external symlink`, (root, outside) => {
    symlinkSync(outside, path.join(root, 'linked'), 'dir');
    const file = kind === 'module' ? 'backend/go.mod' : 'go.work';
    const source = kind === 'module' ? backendModule : work;
    writeFileSync(path.join(root, file), source + `replace github.com/google/uuid => ${kind === 'module' ? '../linked' : './linked'}\n`);
  }, /local replace targets outside checkout/]),
  ...['relative', 'absolute', 'symlink'].map(form => [`external ignore path (${form})`, (root, outside) => {
    if (form === 'symlink') symlinkSync(outside, path.join(root, 'linked'), 'dir');
    const target = form === 'relative' ? '../../outside' : form === 'symlink' ? '../linked' : outside;
    writeFileSync(path.join(root, 'backend/go.mod'), backendModule + `ignore ${JSON.stringify(target)}\n`);
  }, /ignore targets outside checkout/]),
  ...['go.mod', 'go.work'].map(name => [`external ${name} symlink`, (root, outside) => {
    const file = path.join(root, name === 'go.mod' ? 'backend/go.mod' : name);
    const source = name === 'go.mod' ? backendModule : work;
    writeFileSync(path.join(outside, name), source);
    rmSync(file);
    symlinkSync(path.join(outside, name), file);
  }, /manifest is outside checkout/]),
  ['in-tree module whose manifest escapes', (root, outside) => {
    mkdirSync(path.join(root, 'local-module'), { mode: 0o700 });
    symlinkSync(path.join(outside, 'go.mod'), path.join(root, 'local-module/go.mod'));
    writeFileSync(path.join(root, 'go.work'), work + 'use ./local-module\n');
  }, /manifest is outside checkout/],
  ['nested workspace use resolves from its own directory', (root) => {
    mkdirSync(path.join(root, 'nested'), { mode: 0o700 });
    writeFileSync(path.join(root, 'nested/go.work'), 'go 1.27.4\nuse ../../outside\n');
  }, /nested\/go.work: use targets outside checkout/],
  ...['../../outside', '/opt/private/ao-control-plane/uuid'].map(target => [`local tool path ${target}`, root => {
    writeFileSync(path.join(root, 'backend/go.mod'), backendModule + `tool ${JSON.stringify(target)}\n`);
  }, /tool must be an import path/]),
  ['relative replace escaping checkout', root => writeFileSync(path.join(root, 'backend/go.mod'), backendModule + '\nreplace github.com/aoagents/agent-orchestrator/cloud => ../../ao-control-plane\n'), /local replace targets outside checkout/],
  ['required module replaced by absolute private path', root => writeFileSync(path.join(root, 'backend/go.mod'), backendModule + '\nrequire github.com/aoagents/agent-orchestrator/cloud v0.0.0\nreplace github.com/aoagents/agent-orchestrator/cloud => /opt/private/ao-control-plane\n'), /local replace targets outside checkout/],
];

for (const [name, change, failure] of cases) {
  test(name, t => {
    const slot = mkdtempSync(path.join(tmpdir(), 'public-boundary-'));
    const root = path.join(slot, 'repo');
    const outside = path.join(slot, 'outside');
    mkdirSync(root, { mode: 0o700 });
    mkdirSync(outside, { mode: 0o700 });
    writeFileSync(path.join(outside, 'go.mod'), 'module github.com/google/uuid\n\ngo 1.27.4\n');
    writeFileSync(path.join(outside, 'uuid.go'), 'package uuid\n');
    t.after(() => rmSync(slot, { recursive: true, force: true }));
    for (const dir of ['scripts', 'backend', 'cloud', 'components', 'components/hcg']) {
      mkdirSync(path.join(root, dir), { recursive: true, mode: 0o700 });
    }
    copyFileSync(new URL('./check-public-boundary.sh', import.meta.url), path.join(root, 'scripts/check-public-boundary.sh'));
    writeFileSync(path.join(root, 'go.work'), work);
    writeFileSync(path.join(root, 'backend/go.mod'), backendModule + '\nreplace github.com/aoagents/agent-orchestrator/components/hcg => ../components/hcg\n');
    writeFileSync(path.join(root, 'cloud/go.mod'), 'module github.com/aoagents/agent-orchestrator/cloud\n\ngo 1.27.4\n\nreplace github.com/aoagents/agent-orchestrator/backend => example.com/public/backend v1.0.0\n');
    writeFileSync(path.join(root, 'components/hcg/go.mod'), 'module github.com/aoagents/agent-orchestrator/components/hcg\n\ngo 1.27.4\n');
    writeFileSync(path.join(root, 'package.json'), JSON.stringify({ scripts: { cloud: 'cd cloud && go test ./...' } }));
    change(root, outside);
    execFileSync('git', ['init', '-q'], { cwd: root });
    execFileSync('git', ['add', '.'], { cwd: root });
    const result = spawnSync('bash', ['scripts/check-public-boundary.sh'], { cwd: root, encoding: 'utf8' });
    assert.ifError(result.error);
    const output = result.stdout + result.stderr;
    assert.equal(result.status, failure ? 1 : 0, output);
    if (failure) assert.match(output, failure);
    else assert.match(output, /public boundary: ok/);
  });
}
