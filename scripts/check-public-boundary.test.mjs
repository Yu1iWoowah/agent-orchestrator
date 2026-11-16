import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';

const backendModule = 'module github.com/aoagents/agent-orchestrator/backend\n\ngo 1.27.4\n';
const work = 'go 1.27.4\n\nuse (\n ./backend\n ./cloud\n ./components/hcg\n)\n';
const cases = [
  ['retained fork modules and in-tree or versioned replacements', () => {}, null],
  ['missing cloud module', root => rmSync(path.join(root, 'cloud/go.mod')), /retained cloud/],
  ['missing workspace module', root => writeFileSync(path.join(root, 'go.work'), work + 'use ./missing\n'), /go.work uses/],
  ['public code importing cloud', root => writeFileSync(path.join(root, 'backend/bad.go'), 'package backend\nimport _ "github.com/aoagents/agent-orchestrator/cloud/internal/httpapi"\n'), /Go files import/],
  ['npm script pointing to private checkout', root => writeFileSync(path.join(root, 'package.json'), JSON.stringify({ scripts: { cloud: 'cd ../ao-control-plane && go build' } })), /external private control plane/],
  ['relative replace escaping checkout', root => writeFileSync(path.join(root, 'backend/go.mod'), backendModule + '\nreplace github.com/aoagents/agent-orchestrator/cloud => ../../ao-control-plane\n'), /local replace targets outside checkout/],
  ['required module replaced by absolute private path', root => writeFileSync(path.join(root, 'backend/go.mod'), backendModule + '\nrequire github.com/aoagents/agent-orchestrator/cloud v0.0.0\nreplace github.com/aoagents/agent-orchestrator/cloud => /opt/private/ao-control-plane\n'), /local replace targets outside checkout/],
];

for (const [name, change, failure] of cases) {
  test(name, t => {
    const root = mkdtempSync(path.join(tmpdir(), 'public-boundary-'));
    t.after(() => rmSync(root, { recursive: true, force: true }));
    for (const dir of ['scripts', 'backend', 'cloud', 'components', 'components/hcg']) {
      mkdirSync(path.join(root, dir), { recursive: true, mode: 0o700 });
    }
    copyFileSync(new URL('./check-public-boundary.sh', import.meta.url), path.join(root, 'scripts/check-public-boundary.sh'));
    writeFileSync(path.join(root, 'go.work'), work);
    writeFileSync(path.join(root, 'backend/go.mod'), backendModule + '\nreplace github.com/aoagents/agent-orchestrator/components/hcg => ../components/hcg\n');
    writeFileSync(path.join(root, 'cloud/go.mod'), 'module github.com/aoagents/agent-orchestrator/cloud\n\ngo 1.27.4\n\nreplace github.com/aoagents/agent-orchestrator/backend => example.com/public/backend v1.0.0\n');
    writeFileSync(path.join(root, 'components/hcg/go.mod'), 'module github.com/aoagents/agent-orchestrator/components/hcg\n\ngo 1.27.4\n');
    writeFileSync(path.join(root, 'package.json'), JSON.stringify({ scripts: { cloud: 'cd cloud && go test ./...' } }));
    change(root);
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
