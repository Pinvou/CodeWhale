import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');

test('dead-letter replay is inspectable, digest-bound, idempotent, and service audited', async () => {
  const store = await readFile(path.join(root, 'internal/platform/events/postgres.go'), 'utf8');
  const cli = await readFile(path.join(root, 'cmd/event-replay/main.go'), 'utf8');
  assert.match(store, /InspectDeadLetter/);
  assert.match(store, /digest != request\.ExpectedDigest/);
  assert.match(store, /event_replay_requests WHERE idempotency_key/);
  assert.match(store, /identity\.ActorType != "SERVICE"/);
  assert.match(store, /identity\.JobType != "EVENT_REPLAY"/);
  assert.match(store, /FOR UPDATE/);
  assert.doesNotMatch(store, /func \(store \*PostgresStore\) Requeue/);
  for (const option of ['inspect', 'expected-digest', 'idempotency-key', 'reason', 'confirm']) assert.match(cli, new RegExp(option));
  assert.match(cli, /ContextForJob\(ctx, "event-replay", "EVENT_REPLAY"/);
});

test('all three event stores retain immutable replay audit facts', async () => {
  for (const [domain, version] of [['account', '000030'], ['billing', '000032'], ['token', '000018']]) {
    const migration = await readFile(path.join(root, `migrations/${domain}/${version}_event_replay_audit.up.sql`), 'utf8');
    assert.match(migration, /CREATE TABLE event_replay_requests/);
    assert.match(migration, /actor_type TEXT NOT NULL CHECK \(actor_type = 'SERVICE'\)/);
    assert.match(migration, /idempotency_key TEXT NOT NULL UNIQUE/);
    assert.match(migration, /event_replay_requests_immutable BEFORE UPDATE OR DELETE/);
  }
});
