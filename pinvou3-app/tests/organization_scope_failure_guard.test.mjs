import assert from 'node:assert/strict';
import test from 'node:test';
import { createOrganizationScopeFailureGuard } from '../src/features/account-platform/organization-scope.js';

test('same authority remains denied across child remounts and fresh equivalent profiles', () => {
 const guard = createOrganizationScopeFailureGuard();
 const profile = { owner: 'actor:org', context: 1, authorization: 2, epoch: 3 };
 const key = value => JSON.stringify(value);
 assert.equal(guard.deny(key(profile), 'unit-a'), true);
 // Fresh server DTO and child lifecycle do not constitute new authority.
 for (let remount = 0; remount < 10; remount++) {
  const freshProfile = { ...profile };
  assert.equal(guard.isDenied(key(freshProfile), 'unit-a'), true);
  assert.equal(guard.deny(key(freshProfile), 'unit-a'), false);
 }
 assert.equal(guard.isDenied(key(profile), 'unit-b'), false);
 assert.equal(guard.deny(key(profile), 'unit-b'), true);
});

test('explicit read retry does not replenish automatic refresh, new authority does', () => {
 const guard = createOrganizationScopeFailureGuard();
 const identity = ['owner', 'user', 'org', 1, 2, 3];
 const key = JSON.stringify(identity);
 assert.equal(guard.deny(key, 'root'), true);
 guard.retry(key, 'root');
 assert.equal(guard.isDenied(key, 'root'), false);
 assert.equal(guard.deny(key, 'root'), false);
 assert.equal(guard.isDenied(key, 'root'), true);
 for (const position of [0, 1, 2, 3, 4, 5]) {
  const next = [...identity]; next[position] += 1;
  const nextKey = JSON.stringify(next);
  assert.equal(guard.isDenied(nextKey, 'root'), false);
  assert.equal(guard.deny(nextKey, 'root'), true);
 }
});
