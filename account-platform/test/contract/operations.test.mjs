import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');

test('restore audit SQL is read-only and covers financial/event invariants', async () => {
  const sqlDirectory = path.join(root, 'deploy/runbooks/sql');
  const files = await readdir(sqlDirectory);
  assert.deepEqual(files.sort(), ['account-restore-audit.sql', 'billing-ledger-audit.sql', 'outbox-inventory.sql', 'token-restore-audit.sql']);
  const combined = (await Promise.all(files.map((file) => readFile(path.join(sqlDirectory, file), 'utf8')))).join('\n');
  assert.match(combined, /READ ONLY/i);
  assert.match(combined, /sum\(delta_microcredits\) <> 0/i);
  assert.match(combined, /dead_lettered_at/i);
  assert.match(combined, /organization_without_active_owner/i);
  assert.match(combined, /committed_usage_missing_facts/i);
  assert.doesNotMatch(combined, /\b(INSERT|UPDATE|DELETE|TRUNCATE|DROP|ALTER)\b/i);
});

test('recovery and cutover runbooks preserve payment convergence and old-system isolation', async () => {
  const restore = await readFile(path.join(root, 'deploy/runbooks/backup-restore.md'), 'utf8');
  const cutover = await readFile(path.join(root, 'deploy/runbooks/cutover-rollback.md'), 'utf8');
  for (const phrase of ['PITR', 'RPO', 'RTO', 'Outbox', 'Inbox', 'Hold']) assert.match(restore, new RegExp(phrase, 'i'));
  assert.match(cutover, /不迁移、不双写旧账号/);
  assert.match(cutover, /保留 Billing 回调/);
  assert.match(cutover, /真实 New API/);
  assert.match(cutover, /真实支付/);
});

test('production values keep rate limiting enabled', async () => {
  const schema = await readFile(path.join(root, 'deploy/helm/account-platform/values.schema.json'), 'utf8');
  const values = await readFile(path.join(root, 'deploy/helm/account-platform/values-production-new.yaml'), 'utf8');
  assert.match(schema, /rateLimitRPS/);
  assert.doesNotMatch(values, /rateLimit(RPS|Burst):\s*0/);
});
