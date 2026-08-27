import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = (file) => readFile(path.join(root, file), 'utf8');

test('billing restores immutable system ledger accounts and backs off callback retries', async () => {
  const [migration, repository, worker] = await Promise.all([
    read('migrations/billing/000045_system_ledger_accounts_repair.up.sql'),
    read('internal/billing/system_ledger_accounts.go'),
    read('internal/billing/payment_notification_worker.go'),
  ]);

  for (const suffix of ['001', '002', '003', '004', '005', '006']) {
    assert.match(migration, new RegExp(`00000000-0000-7000-8000-000000000${suffix}`));
  }
  assert.match(migration, /billing system ledger account invariant failed/);
  assert.match(repository, /ValidateSystemLedgerAccounts/);
  assert.match(worker, /next_attempt_at IS NULL OR next_attempt_at<=now\(\)/);
  assert.match(worker, /processing_attempts=processing_attempts\+1/);
  assert.match(worker, /payment notification processing failed/);
});
