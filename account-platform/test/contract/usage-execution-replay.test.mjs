import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = (name) => readFile(path.join(root, name), 'utf8');

test('model inference idempotency has a stable user namespace and encrypted replay tiers', async () => {
  const migration = await read('migrations/token/000014_usage_execution_replay.up.sql');
  const replay = await read('internal/token/replay_repository.go');
  assert.match(migration, /ON usage_requests\(user_id,idempotency_namespace,idempotency_key_digest\)/);
  assert.match(migration, /canonicalization_version/);
  assert.match(migration, /request_fingerprint/);
  for (const table of [
    'usage_execution_transitions',
    'usage_dispatch_preparations',
    'usage_dispatch_intents',
    'usage_stream_events',
    'usage_replay_results',
    'usage_idempotency_tombstones',
    'usage_replay_retention_cases',
    'usage_replay_case_payloads',
  ]) assert.match(migration, new RegExp(`CREATE TABLE ${table}`));
  assert.match(replay, /streamRetention\s+= 24 \* time\.Hour/);
  assert.match(replay, /resultRetention\s+= 30 \* 24 \* time\.Hour/);
  assert.match(replay, /tombstoneRetention = 90 \* 24 \* time\.Hour/);
  assert.match(replay, /expires_at IS NULL/);
});

test('gateway persists before delivery and never binds inference keys to sessions', async () => {
  const proxy = await read('internal/token/proxy.go');
  const replay = await read('internal/token/replay_repository.go');
  assert.match(proxy, /IDEMPOTENCY_KEY_REUSED/);
  assert.match(proxy, /AppendReplayEvent[\s\S]*fmt\.Fprintf/);
  assert.match(proxy, /context\.WithTimeout\(context\.WithoutCancel\(r\.Context\(\)\)/);
  assert.doesNotMatch(proxy, /resolved\.SessionID[\s\S]{0,120}Idempotency/);
  assert.match(replay, /worker_acquired_at IS NULL/);
  assert.match(replay, /dispatch_result_unknown/);
  assert.match(replay, /idempotency_key_digest=NULL/);
});
