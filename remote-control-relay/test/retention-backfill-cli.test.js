import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, rmdirSync, symlinkSync, unlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import test from 'node:test';
import { RetentionFacts } from '../retention-store.js';

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

test('linked backfill CLI applies confirmed offline data and writes reloadable retention facts', t => {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), 'pinvou-retention-cli-')));
  const alias = join(dir, 'relay alias');
  const dataDir = join(dir, 'fixture data');
  symlinkSync(relay, alias, process.platform === 'win32' ? 'junction' : 'dir');
  t.after(() => {
    // Unlink the repository alias first; recursively remove only our fixture data.
    unlinkSync(alias);
    rmSync(dataDir, { recursive:true, force:true });
    rmdirSync(dir);
  });
  mkdirSync(dataDir);
  const now = Date.now();
  const firstSuccess = now - 31 * 86_400_000;
  const device = {
    device_id:'dev_linked_backfill_fixture', hardware_source:'installation_id',
    first_seen_at:firstSuccess, last_seen_at:now,
  };
  writeFileSync(join(dataDir, 'devices.json'), JSON.stringify({ devices:[device] }));
  const rawPath = join(dataDir, 'behavior-events.jsonl');
  const events = [firstSuccess, firstSuccess + 30 * 86_400_000].map((occurred_at, index) => ({
    device_id:device.device_id, event_id:`linked_backfill_${index}`, attempt_id:`linked_backfill_${index}`,
    event_name:'task_finished', status:'success', initiator_type:'user', mode:'assistant', occurred_at,
  }));
  writeFileSync(rawPath, `${events.map(event => JSON.stringify(event)).join('\n')}\n`);
  const rawBefore = readFileSync(rawPath);
  assert.equal(existsSync(join(dataDir, 'retention-facts.json')), false);
  const result = run(join(alias, 'scripts', 'backfill-retention.mjs'), ['--data-dir', dataDir, '--apply', '--offline']);
  assert.equal(result.status, 0);
  assert.equal(result.stderr, '');
  const report = JSON.parse(result.stdout);
  assert.equal(report.applied, true);
  assert.equal(report.scanned_events, 2);
  assert.equal(report.after.d30.retained_installations, 1);
  assert.equal(readFileSync(join(dataDir, 'retention-facts.initialized'), 'utf8'), '1\n');
  const reloaded = new RetentionFacts(dataDir);
  assert.equal(reloaded.rows.size, 1);
  assert.equal(reloaded.metrics(now).d30.retained_installations, 1);
  assert.deepEqual(readFileSync(rawPath), rawBefore);
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
