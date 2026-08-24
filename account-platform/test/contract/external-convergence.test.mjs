import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('payment and model external facts both use immutable convergence evidence', async () => {
  const paymentMigration = await read('migrations/billing/000024_payment_evidence_lifecycle.up.sql');
  const usageMigration = await read('migrations/token/000019_usage_convergence_evidence.up.sql');
  assert.match(paymentMigration, /CREATE TABLE payment_evidence/);
  assert.match(paymentMigration, /payment_evidence_immutable/);
  assert.match(usageMigration, /CREATE TABLE usage_convergence_evidence/);
  assert.match(usageMigration, /usage_convergence_evidence_immutable/);
  assert.match(usageMigration, /payload_digest TEXT NOT NULL/);
});

test('dispatch result uncertainty and settlement decisions append one evidence timeline', async () => {
  const postgres = await read('internal/token/postgres.go');
  const replay = await read('internal/token/replay_repository.go');
  const reconciliation = await read('internal/token/reconciliation.go');
  for (const type of ['UPSTREAM_REFERENCE', 'UNCERTAINTY', 'SETTLEMENT', 'CUSTOMER_HOLD_RELEASE']) {
    assert.match(postgres, new RegExp(`"${type}"`));
  }
  assert.match(replay, /"DISPATCH_STARTED"/);
  assert.match(replay, /"RESULT_OBSERVED"/);
  assert.match(reconciliation, /"PROVIDER_LOOKUP"/);
  assert.match(reconciliation, /automatic_high_risk_retry.*false/);
});

test('conflicting replay results cannot replace the first complete provider result', async () => {
  const migration = await read('migrations/token/000019_usage_convergence_evidence.up.sql');
  const replay = await read('internal/token/replay_repository.go');
  assert.match(migration, /ADD COLUMN evidence_payload_digest/);
  assert.match(replay, /storedDigest != nil && \*storedDigest != responseDigest/);
  assert.match(replay, /return ErrIdempotencyConflict/);
  assert.match(replay, /ON CONFLICT\(usage_request_id\) DO NOTHING/);
});

test('operations usage detail exposes sanitized convergence evidence without model content', async () => {
  const admin = await read('internal/token/admin.go');
  assert.match(admin, /json:"convergence_evidence"/);
  assert.match(admin, /FROM usage_convergence_evidence evidence/);
  assert.match(admin, /payload_digest/);
  assert.doesNotMatch(admin, /usage_convergence_evidence[\s\S]{0,500}(messages|prompt|response_body)/i);
});
