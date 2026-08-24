import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = (name) => readFile(path.join(root, name), 'utf8');

test('forced model removal atomically persists an immutable emergency cancellation policy', async () => {
  const migration = await read('migrations/token/000021_model_emergency_cancellation.up.sql');
  const catalog = await read('internal/token/catalog.go');
  for (const table of ['model_emergency_cancellation_policies', 'model_emergency_cancellations', 'model_emergency_cancellation_evidence']) {
    assert.match(migration, new RegExp(`CREATE TABLE ${table}`));
  }
  assert.match(migration, /MODEL_EMERGENCY/);
  assert.match(migration, /model_emergency_cancellation_policies_immutable/);
  assert.match(catalog, /TargetStatus == "REMOVED" && input\.ForceCancelInflight[\s\S]*INSERT INTO model_emergency_cancellation_policies/);
});

test('emergency cancellation targets only pre-effective accepted inflight requests and records real capability', async () => {
  const worker = await read('internal/token/emergency_cancellation.go');
  const service = await read('internal/token/service.go');
  const proxy = await read('internal/token/proxy.go');
  assert.match(worker, /request\.started_at<policy\.effective_at/);
  assert.match(worker, /ProviderCancelUnsupported/);
  assert.match(worker, /local_transport_aborted/);
  assert.match(worker, /provider_success_inferred/);
  assert.match(service, /activeUpstreamCancels/);
  assert.match(proxy, /registerActiveUpstream/);
});

test('emergency removal converges actual usage hold and settlement without inferring free service', async () => {
  const worker = await read('internal/token/emergency_cancellation.go');
  assert.match(worker, /finishPreDispatchCancellation/);
  assert.match(worker, /settlement_status IN \('SETTLED','RELEASED'\)/);
  for (const evidence of ['hold_id', 'actual_microcredits', 'usage_snapshot', 'final_settlement_status']) {
    assert.match(worker, new RegExp(evidence));
  }
  assert.match(worker, /emergency_removal_implied_free[^\n]*false/);
  assert.doesNotMatch(worker, /abortActiveUpstream[\s\S]{0,240}billing\.Release/);
});
