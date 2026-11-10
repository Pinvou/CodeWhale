#!/usr/bin/env node
import { isMainModule } from './account-platform-cli-entry.mjs';
// Explicit offline test of a supplied Helm render; never contacts a cluster.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { isAbsolute, join } from 'node:path';
import { fileURLToPath } from 'node:url';

export function prometheusRuleCommands(args) {
  assert.equal(args.length, 6, 'provide --rendered, --output and --promtool absolute paths');
  assert.equal(args[0], '--rendered'); assert.equal(args[2], '--output'); assert.equal(args[4], '--promtool');
  assert.ok([args[1], args[3], args[5]].every(isAbsolute), 'all offline fixture paths must be absolute');
  return [
    [process.execPath, [fileURLToPath(new URL('../account-platform/test/integration/prometheus-rule-fixtures.mjs', import.meta.url)), args[1], args[3]]],
    [args[5], ['test', 'rules', join(args[3], 'event-relay-missing.test.yaml'), join(args[3], 'approval-multi-scope.test.yaml')]],
  ];
}

function run(executable, args) {
  return new Promise((resolve, reject) => {
    const child = spawn(executable, args, { shell: false, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
    let bytes = 0; let exhausted = false;
    const timer = setTimeout(() => { exhausted = true; child.kill(); }, 30000);
    const collect = chunk => { bytes += chunk.length; if (bytes > 1024 * 1024) { exhausted = true; child.kill(); } };
    child.stdout.on('data', chunk => { collect(chunk); if (!exhausted) process.stdout.write(chunk); });
    child.stderr.on('data', chunk => { collect(chunk); if (!exhausted) process.stderr.write(chunk); });
    child.once('error', () => { clearTimeout(timer); reject(new Error('offline fixture tool could not start')); });
    child.once('close', code => { clearTimeout(timer); if (code === 0 && !exhausted) resolve(); else reject(new Error('offline fixture tool failed; no acceptance result')); });
  });
}

if (isMainModule(import.meta.url)) {
  try {
    for (const [executable, args] of prometheusRuleCommands(process.argv.slice(2))) await run(executable, args);
    console.log('actual rendered Prometheus rules and validated promtool fixture suite passed; NOT_DEPLOYED');
  } catch {
    console.error('offline Prometheus validation failed; supply a new output directory, a real Helm render and an installed promtool');
    process.exitCode = 1;
  }
}
