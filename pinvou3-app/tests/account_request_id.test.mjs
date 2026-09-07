import assert from 'node:assert/strict';
import test from 'node:test';
import { secureRequestID } from '../src/features/account-platform/request-id.js';

test('account mutation IDs work without crypto.randomUUID using secure bytes', () => {
  let calls = 0;
  const provider = { getRandomValues: bytes => { calls += 1; bytes.fill(255); return bytes; } };
  assert.equal(secureRequestID(provider), 'ffffffff-ffff-4fff-bfff-ffffffffffff');
  assert.equal(calls, 1);
  assert.match(secureRequestID(), /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.notEqual(secureRequestID(), secureRequestID());
});

test('account mutation IDs fail closed if secure randomness is unavailable', () => {
  assert.throws(() => secureRequestID({}), TypeError);
});
