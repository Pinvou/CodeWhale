import assert from 'node:assert/strict';
import { test } from 'node:test';
import { assertTestSummary } from '../../scripts/windows-relay-tools-test-summary.mjs';
import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdtempSync, mkdirSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { requireWindowsPowerShell51, POWERSHELL_PREFLIGHT_TIMEOUT_MS } from '../../scripts/windows-relay-tools-preflight.mjs';

test('PowerShell preflight has a bounded cold-start budget and fails closed on every probe failure', () => {
  assert.equal(POWERSHELL_PREFLIGHT_TIMEOUT_MS, 60_000);
  let calls = 0;
  requireWindowsPowerShell51((command, args, options) => {
    calls++;
    assert.equal(command, 'powershell.exe');
    assert.deepEqual(args, ['-NoProfile', '-NonInteractive', '-Command', '$PSVersionTable.PSVersion.ToString()']);
    assert.equal(options.timeout, POWERSHELL_PREFLIGHT_TIMEOUT_MS);
    assert.equal(options.windowsHide, true);
    return { status: 0, stdout: '5.1.26100.7462\r\n' };
  });
  assert.equal(calls, 1);
  for (const [result, reason] of [
    [{ error: Object.assign(new Error('timeout'), { code: 'ETIMEDOUT' }), status: null, stdout: '5.1.26100.7462' }, /preflight timed out/],
    [{ error: Object.assign(new Error('missing'), { code: 'ENOENT' }) }, /could not start/],
    [{ status: 1, stdout: '5.1.26100.7462' }, /exited unsuccessfully/],
    [{ status: null, stdout: '5.1.26100.7462' }, /exited unsuccessfully/],
    ...['', '7.4.0', '5.1.garbage', '5.1.26100.7462\n7.4.0'].map((stdout) => [{ status: 0, stdout }, /unexpected version/]),
  ]) assert.throws(() => requireWindowsPowerShell51(() => result), reason);
});

const summary = '# tests 2\n# pass 2\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n';
test('Windows tools gate requires real successful execution with a complete summary', () => {
  assert.doesNotThrow(() => assertTestSummary(summary));
  for (const output of ['', summary.replace('# tests 2', '# tests 0'),
    summary.replace('# pass 2', '# pass 1'), summary + summary,
    ...['fail', 'cancelled', 'skipped', 'todo'].map((name) => summary.replace(`# ${name} 0`, `# ${name} 1`)),
    ...['tests', 'pass', 'fail', 'cancelled', 'skipped', 'todo'].map((name) => summary.replace(new RegExp(`^# ${name} .*\\n`, 'm'), ''))]) {
    assert.throws(() => assertTestSummary(output));
  }
});

test('Windows tools CLI cannot silently succeed through a linked directory', (t) => {
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-windows-tools-cli-'));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const scripts = join(directory, 'scripts');
  const alias = join(directory, 'linked-scripts');
  mkdirSync(scripts);
  for (const file of ['run-windows-relay-tools-tests.mjs', 'windows-relay-tools-test-summary.mjs', 'windows-relay-tools-preflight.mjs']) {
    copyFileSync(fileURLToPath(new URL('../../scripts/' + file, import.meta.url)), join(scripts, file));
  }
  symlinkSync(scripts, alias, process.platform === 'win32' ? 'junction' : 'dir');
  // No test files exist in this isolated root. Both entries MUST run and fail;
  // never execute the host's real suite or accept a lexical-entry no-op.
  for (const entry of [scripts, alias]) {
    const result = spawnSync(process.execPath, [join(entry, 'run-windows-relay-tools-tests.mjs')],
      { encoding: 'utf8', timeout: 90_000 });
    assert.equal(result.error, undefined);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Native Windows execution required|Windows relay regression failed/);
  }
});
