import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import { relayAvatarFallback, relayAvatarTone } from '../src/features/team-relay/relay-avatar.mjs';

test('Team Relay fallback avatars use one readable name character', () => {
  assert.equal(relayAvatarFallback('刘洋'), '刘');
  assert.equal(relayAvatarFallback('alice'), 'A');
  assert.equal(relayAvatarFallback('  bob'), 'B');
  assert.equal(relayAvatarFallback('·陈晨'), '陈');
  assert.equal(relayAvatarFallback(''), '?');
});

test('Team Relay fallback colors are stable per person and independent from expert avatars', () => {
  assert.deepEqual(relayAvatarTone('principal-a'), relayAvatarTone('principal-a'));
  assert.notDeepEqual(relayAvatarTone('principal-a'), relayAvatarTone('principal-b'));
  const consecutivePeople = Array.from(
    { length: 100 },
    (_, index) => JSON.stringify(relayAvatarTone(`relay-member-${String(index).padStart(3, '0')}`)),
  );
  assert.equal(new Set(consecutivePeople).size, consecutivePeople.length);
  const component = fs.readFileSync(
    new URL('../src/features/team-relay/RelayAvatar.jsx', import.meta.url),
    'utf8',
  );
  assert.doesNotMatch(component, /expert-avatar|expertAvatarSrc/);
});
