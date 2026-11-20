'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { artifactNodeOptions } = require('../ci-artifact-node-options.cjs');
const helper = path.resolve(__dirname, '../ci-artifact-node-options.cjs');
const workflow = fs.readFileSync(path.resolve(__dirname, '../../.github/workflows/pr-check.yml'), 'utf8').replaceAll('\r\n', '\n');
const stepPattern = /^      - [\s\S]*?(?=^      - |^  [A-Za-z0-9_-]+:|$(?![\s\S]))/gm;

const { checkWorkflow } = require('../ci-network-policy.cjs');
const { networkPath } = require('../ci-network-node-options.cjs');

test('Windows archive utilities stay scoped and an existing archiver wins', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'cache-tools-'));
  try {
    assert.equal(networkPath('original-path', 'linux', () => assert.fail('unexpected probe')), 'original-path');
    fs.writeFileSync(path.join(root, 'tar.exe'), 'fixture');
    assert.equal(networkPath(root, 'win32', () => assert.fail('existing archiver must win')), root);
    assert.equal(networkPath('original-path', 'win32', () => root), `${root};original-path`);
    assert.throws(() => networkPath('original-path', 'win32', () => null));
    assert.throws(() => networkPath('path\nINJECTED=1'));
  } finally { fs.rmSync(root, { recursive: true }); }
});

test('all workflows keep storage policies scoped, condition-matched and observable', () => {
  const directory = path.resolve(__dirname, '../../.github/workflows');
  let count = 0;
  for (const name of fs.readdirSync(directory).filter(name => /\.ya?ml$/.test(name))) {
    count += checkWorkflow(fs.readFileSync(path.join(directory, name), 'utf8'), name);
  }
  assert.ok(count > 80, 'the test must cover every actual storage action family');
  for (const mutant of [
    workflow.replace('NODE_OPTIONS: ${{ env.PINVOU_CI_NETWORK_NODE_OPTIONS }}', 'NODE_OPTIONS: --tls-max-v1.2'),
    workflow.replace('run: node scripts/ci-network-node-options.cjs', 'run: node scripts/unreviewed.cjs'),
    workflow.replace('      - name: Configure verified Rust download source\n        run: node scripts/ci-rust-network.cjs\n      - name: 安装锁定的 Rust 工具链', '      - name: 安装锁定的 Rust 工具链'),
    workflow.replace('run: node scripts/ci-rust-network.cjs --report', 'run: node scripts/unreviewed-rust-report.cjs'),
    workflow.replace('working-directory: ${{ github.workspace }}', 'working-directory: wrong-directory'),
    workflow.replace("PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1'", "PINVOU_CI_ARTIFACT_DIAGNOSTICS: '0'"),
    workflow.replace(/^env:$/m, 'env:\n  NODE_OPTIONS: ${{ env.PINVOU_CI_NETWORK_NODE_OPTIONS }}'),
  ]) {
    assert.notEqual(mutant, workflow, 'negative control must change source');
    assert.throws(() => checkWorkflow(mutant));
  }
  assert.throws(() => checkWorkflow('jobs:\n  test:\n    steps:\n      - uses: fork/unknown-cache@v1\n'));
});

test('the helper reads real inherited options, writes a separate value and preloads diagnostics', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'artifact-options-test-'));
  try {
    const envFile = path.join(root, 'github-env');
    const result = spawnSync(process.execPath, [helper, '--diagnostics'], {
      env: { ...process.env, GITHUB_ENV: envFile, NODE_OPTIONS: '--max-old-space-size=192' },
      encoding: 'utf8', timeout: 10000,
    });
    assert.equal(result.status, 0, result.stderr);
    const line = fs.readFileSync(envFile, 'utf8');
    assert.match(line, /^PINVOU_CI_ARTIFACT_NODE_OPTIONS=--max-old-space-size=192 .* --tls-max-v1\.2\n$/);
    assert.equal(line.split('\n').length, 2, 'one custom environment value, never job-wide NODE_OPTIONS');
    const options = line.slice(line.indexOf('=') + 1).trimEnd();
    const child = spawnSync(process.execPath, ['-e', 'console.log("configured")'], {
      env: { ...process.env, NODE_OPTIONS: options, PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1' },
      encoding: 'utf8', timeout: 10000,
    });
    assert.equal(child.status, 0, child.stderr);
    assert.equal(child.stdout.trim(), 'configured');
    assert.match(child.stderr, /"phase":"enabled"/);
    assert.throws(() => artifactNodeOptions('--max-old-space-size=192\nINJECTED=value'));
    assert.throws(() => artifactNodeOptions('bad\roption'));
  } finally { fs.rmSync(root, { recursive: true }); }
});

test('the real Node runtime retains its secure minimum and inherited heap option, and fails closed for TLS 1.3 minimum', () => {
  const source = "const tls=require('node:tls'),v8=require('node:v8');console.log(JSON.stringify({min:tls.DEFAULT_MIN_VERSION,max:tls.DEFAULT_MAX_VERSION,heap:v8.getHeapStatistics().heap_size_limit}))";
  const run = existing => spawnSync(process.execPath, ['-e', source], {
    env: { ...process.env, NODE_OPTIONS: artifactNodeOptions(existing) }, encoding: 'utf8', timeout: 10000,
  });
  const ordinary = run('--max-old-space-size=192');
  assert.equal(ordinary.status, 0, ordinary.stderr);
  const value = JSON.parse(ordinary.stdout);
  assert.equal(value.min, 'TLSv1.2');
  assert.equal(value.max, 'TLSv1.2');
  assert.ok(value.heap < 512 * 1024 * 1024, 'the pre-existing heap option must survive');
  const stricter = run('--tls-min-v1.3');
  assert.notEqual(stricter.status, 0, 'an incompatible explicit higher minimum must fail closed');
  assert.match(stricter.stderr, /tls-min-v1\.3/);
  assert.match(stricter.stderr, /tls-max-v1\.2/);
});
