import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../../..');

test('E2E runner generates secrets and exercises financial idempotency', async () => {
  const runner = await readFile(path.join(root, 'scripts/account-platform.mjs'), 'utf8');
  const acceptance = await readFile(path.join(root, 'account-platform/test/integration/compose-acceptance.mjs'), 'utf8');
  const fault = await readFile(path.join(root, 'account-platform/test/integration/compose-fault.mjs'), 'utf8');

  assert.match(runner, /generatedComposeEnvironment/);
  assert.match(runner, /randomBytes\(32\)/);
  assert.match(runner, /compose-smoke\.mjs', 'compose-acceptance\.mjs/);
  assert.match(runner, /action === 'e2e'/);
  assert.match(acceptance, /\['UNIONPAY', 'ALIPAY', 'WECHAT'\]/);
  assert.match(acceptance, /length: 100/);
  assert.match(acceptance, /concurrent reserves must create one hold/);
  assert.match(acceptance, /concurrent commits must debit once/);
  assert.match(acceptance, /partial-refund acceptance/);
  assert.match(acceptance, /reconciliation must close without mismatches/);
  assert.doesNotMatch(acceptance, /replace-with-|Bearer [A-Za-z0-9_-]{20,}/);
  assert.match(fault, /stop', 'billing-service/);
  assert.match(fault, /start', 'billing-service/);
  assert.match(fault, /stop', 'nats/);
  assert.match(fault, /payment callback must converge after Billing restarts/);
  assert.doesNotMatch(fault, /replace-with-|Bearer [A-Za-z0-9_-]{20,}/);
});
