import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('../src/features/account-platform/AccountClosureWorkspace.jsx', import.meta.url), 'utf8');

test('account closure refresh policy stays visible, low frequency, read-only and terminal-aware', () => {
  assert.match(source, /const intervalMs = 15_000/);
  assert.match(source, /document\.visibilityState === 'visible'/);
  assert.match(source, /document\.addEventListener\('visibilitychange'/);
  assert.match(source, /closure\.status === 'COMPLETED'/);
  assert.match(source, /closure\.stage === 'ANONYMIZED'/);
  assert.match(source, /controller\.abort\(\)/);
  assert.match(source, /queryAccountClosure\(client, signal\)/);
  assert.doesNotMatch(source, /submitAccountClosure\(.*poll/);
});
