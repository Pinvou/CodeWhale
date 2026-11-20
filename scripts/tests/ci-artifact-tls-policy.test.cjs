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

function checkPolicy(text) {
  const steps = [...text.matchAll(stepPattern)].map(match => match[0]);
  let uploads = 0;
  for (let i = 0; i < steps.length; i++) {
    const step = steps[i];
    if (!/^        uses: actions\/upload-artifact@v7$/m.test(step)) continue;
    uploads++;
    const assignments = [...step.matchAll(/^          NODE_OPTIONS: (.*)$/gm)];
    assert.equal(assignments.length, 1, 'each upload must use the prepared runtime options');
    assert.equal(assignments[0][1], '${{ env.PINVOU_CI_ARTIFACT_NODE_OPTIONS }}');
    const config = steps[i - 1] || '';
    const diagnostics = step.includes("          PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1'");
    assert.equal(config.match(/^        run: (.*)$/m)?.[1],
      'node scripts/ci-artifact-node-options.cjs' + (diagnostics ? ' --diagnostics' : ''));
    assert.match(config, /^        working-directory: \$\{\{ github.workspace \}\}$/m);
    assert.equal(config.match(/^        if: (.*)$/m)?.[1], step.match(/^        if: (.*)$/m)?.[1],
      'configuration must run under the same condition as its upload');
    assert.doesNotMatch(config, /continue-on-error:|NODE_OPTIONS:/);
  }
  assert.ok(uploads > 0, 'the policy must inspect real upload steps');
  const remainder = text.replace(stepPattern, step =>
    /^        uses: actions\/upload-artifact@v7$/m.test(step) ? '' : step);
  assert.doesNotMatch(remainder, /NODE_OPTIONS:|--tls-max-v1\.2/,
    'business steps and job-wide Node settings must not receive the upload cap');
  assert.doesNotMatch(text, /NODE_TLS_REJECT_UNAUTHORIZED|--tls-min-v1\.[01]/);
}

test('every PR artifact upload has a condition-matched preparation and an action-only TLS policy', () => {
  checkPolicy(workflow);
  for (const mutant of [
    workflow.replace('NODE_OPTIONS: ${{ env.PINVOU_CI_ARTIFACT_NODE_OPTIONS }}', 'NODE_OPTIONS: --tls-max-v1.2'),
    workflow.replace('run: node scripts/ci-artifact-node-options.cjs', 'run: node scripts/unreviewed.cjs'),
    workflow.replace('run: node scripts/ci-artifact-node-options.cjs --diagnostics', 'run: node scripts/ci-artifact-node-options.cjs'),
    workflow.replace('      - name: Configure artifact transport for Gate test resource lifecycle receipts\n        if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != \'\' }}',
      '      - name: Configure artifact transport for Gate test resource lifecycle receipts\n        if: success()'),
    workflow.replace(/^env:$/m, 'env:\n  NODE_OPTIONS: --tls-max-v1.2'),
    workflow.replace('        uses: actions/upload-artifact@v7', '        uses: actions/checkout@v7'),
  ]) {
    assert.ok(mutant !== workflow, 'the negative control must change the workflow');
    assert.throws(() => checkPolicy(mutant));
  }
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
