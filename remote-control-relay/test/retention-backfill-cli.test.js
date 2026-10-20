import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, realpathSync, rmdirSync, symlinkSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import test from 'node:test';

const relay = dirname(dirname(fileURLToPath(import.meta.url)));
const entry = join(relay, 'scripts', 'backfill-retention.mjs');
function run(script, args) {
  const result = spawnSync(process.execPath, [script, ...args], {
    env:Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('PINVOU_TELEMETRY_'))),
    encoding:'utf8', windowsHide:true, timeout:10_000, maxBuffer:64 * 1024,
  });
  assert.ifError(result.error);
  assert.equal(result.signal, null);
  return result;
}

test('backfill CLI executes through a directory link instead of silently succeeding', t => {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), 'pinvou-retention-cli-')));
  const alias = join(dir, 'relay alias');
  symlinkSync(relay, alias, process.platform === 'win32' ? 'junction' : 'dir');
  t.after(() => { unlinkSync(alias); rmdirSync(dir); });
  const canonical = run(entry, ['--invalid-retention-option']);
  const linked = run(join(alias, 'scripts', 'backfill-retention.mjs'), ['--invalid-retention-option']);
  assert.equal(canonical.status, 1);
  assert.equal(linked.status, 1);
  assert.equal(linked.stdout, canonical.stdout);
  assert.equal(linked.stderr, canonical.stderr);
});

test('backfill CLI accepts Windows casing aliases', { skip:process.platform !== 'win32' }, () => {
  const canonical = run(entry, ['--help']);
  const alias = run(join(relay.toUpperCase(), 'scripts', 'backfill-retention.mjs'), ['--help']);
  assert.equal(canonical.status, 0);
  assert.equal(alias.status, 0);
  assert.match(canonical.stdout, /--data-dir/);
  assert.equal(alias.stdout, canonical.stdout);
});

test('backfill help and argument failures are readable without a stack trace', () => {
  const help = run(entry, ['--help']);
  assert.equal(help.status, 0);
  assert.match(help.stdout, /--apply --offline/);
  for (const args of [[], ['--data-dir'], ['--data-dir', '--apply'], ['--source'], ['--invalid-retention-option']]) {
    const result = run(entry, args);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /usage:/i);
    assert.doesNotMatch(result.stderr, /\n\s+at |file:\/\//);
    assert.doesNotMatch(result.stderr, /offline service/);
  }
  const unsafeApply = run(entry, ['--data-dir', 'unused-fixture', '--apply']);
  assert.equal(unsafeApply.status, 1);
  assert.match(unsafeApply.stderr, /offline/);
});

test('backfill import has no CLI side effects', () => {
  const result = run('--input-type=module', ['--eval', `await import(${JSON.stringify(pathToFileURL(entry).href)})`]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout, '');
  assert.equal(result.stderr, '');
});
