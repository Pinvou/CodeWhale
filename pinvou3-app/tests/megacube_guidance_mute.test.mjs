import assert from 'node:assert/strict';
import { test } from 'node:test';
import { guidanceStorageKey } from '../src/features/megacube/model-guidance-state.js';
import { guidanceSelectionKey } from '../src/features/megacube/model-guidance-connection.js';
import { guidanceReminderCanMute, guidanceMuteKey, readGuidanceMute, muteGuidanceUntilAvailable, clearGuidanceMute, reconcileGuidanceMute } from '../src/features/megacube/model-guidance-mute.js';

const scope = guidanceStorageKey('a'.repeat(64));
const selected = { id: 'local/a', model: 'ornith', base_url: 'http://127.0.0.1:8000/v1' };
const other = { ...selected, id: 'alternative', base_url: 'http://127.0.0.1:18080/v1' };
const service = model => ({ base_url: model.base_url, status: 'ready', models: [{ id: model.model, loaded: true }] });
const checked = (model, ok, candidates = [service(model)]) => ({ phase: 'available', available: candidates, localCandidates: candidates,
  selectedConnection: { key: guidanceSelectionKey(model), result: { ok, code: ok ? 'ok' : 'server_unavailable' } } });
function memory(seed = {}) {
  const values = new Map(Object.entries(seed)); const writes = [];
  return { values, writes, getItem: key => values.get(key) ?? null,
    setItem: (key, value) => { writes.push(['set', key]); values.set(key, value); },
    removeItem: key => { writes.push(['remove', key]); values.delete(key); } };
}

test('mute survives a new storage reader and stays isolated by profile/socket and saved model', () => {
  const storage = memory(); const key = guidanceMuteKey(scope, selected);
  assert.equal(muteGuidanceUntilAvailable(storage, key, 100), true);
  const reopened = memory(JSON.parse(JSON.stringify(Object.fromEntries(storage.values))));
  assert.equal(readGuidanceMute(reopened, key).muted, true);
  assert.equal(readGuidanceMute(reopened, guidanceMuteKey(guidanceStorageKey('b'.repeat(64)), selected)).muted, false);
  assert.equal(readGuidanceMute(reopened, guidanceMuteKey(scope, other)).muted, false);
  assert.equal(guidanceMuteKey(scope, { ...selected, base_url: other.base_url }), key);
  assert.ok(!key.includes(selected.base_url));
  assert.equal(guidanceMuteKey('invalid', selected), null);
  assert.equal(guidanceMuteKey(scope, { ...selected, id: '\ud800' }), null);
});

test('another healthy registered model never resets the unavailable current model reminder', () => {
  const storage = memory(); const key = guidanceMuteKey(scope, selected);
  muteGuidanceUntilAvailable(storage, key, 100);
  for (let i = 0; i < 100; i++) {
    assert.equal(reconcileGuidanceMute(storage, scope, selected, checked(selected, false, [service(other)]), [selected, other]).muted, true);
    assert.equal(muteGuidanceUntilAvailable(storage, key, 200 + i), true);
  }
  assert.equal(storage.getItem(key), '100'); assert.equal(storage.writes.length, 1);
});

test('confirmed current availability clears the marker once and the next outage is shown', () => {
  const storage = memory(); const key = guidanceMuteKey(scope, selected);
  muteGuidanceUntilAvailable(storage, key, 100);
  muteGuidanceUntilAvailable(storage, guidanceMuteKey(scope, null), 100);
  for (let i = 0; i < 100; i++) {
    const state = reconcileGuidanceMute(storage, scope, selected, checked(selected, true), [selected]);
    assert.equal(state.available, true); assert.equal(state.muted, false);
  }
  assert.equal(storage.writes.filter(([action]) => action === 'remove').length, 2);
  assert.equal(reconcileGuidanceMute(storage, scope, selected, checked(selected, false, []), [selected]).muted, false);
});

test('stale probes, missing discovery, unloaded or different models and pending tasks do not reset mute', () => {
  const storage = memory(); const key = guidanceMuteKey(scope, selected);
  muteGuidanceUntilAvailable(storage, key, 100);
  const snapshots = [checked(other, true), checked(selected, true, []),
    checked(selected, true, [service({ ...selected, model: 'different' })]),
    checked(selected, true, [{ ...service(selected), models: [{ id: selected.model, loaded: false }] }]),
    { ...checked(selected, true), selectedConnection: null },
    ...['checking', 'hidden', 'installCms', 'starting'].map(phase => ({ ...checked(selected, true), phase }))];
  for (const snapshot of snapshots) {
    assert.equal(reconcileGuidanceMute(storage, scope, selected, snapshot, [selected, other]).muted, true);
  }
  assert.equal(storage.writes.length, 1);
});

test('recovery of a newly selected model does not erase another model’s muted outage', () => {
  const storage = memory(); const key = guidanceMuteKey(scope, selected);
  muteGuidanceUntilAvailable(storage, key, 100);
  reconcileGuidanceMute(storage, scope, other, checked(other, true), [selected, other]);
  assert.equal(readGuidanceMute(storage, key).muted, true);
  const cloud = { id: 'cloud', base_url: 'https://example.test/v1' };
  const localKey = guidanceMuteKey(scope, cloud); muteGuidanceUntilAvailable(storage, localKey, 100);
  assert.equal(reconcileGuidanceMute(storage, scope, cloud, checked(other, true), [cloud]).muted, true);
  assert.equal(reconcileGuidanceMute(storage, scope, cloud, checked(other, true), [cloud, other]).muted, false);
  assert.equal(readGuidanceMute(storage, key).muted, true);
});

test('storage denial, dropped writes and corrupt values never silently hide a reminder', () => {
  const key = guidanceMuteKey(scope, selected);
  for (const storage of [undefined, { getItem() { throw new Error('denied'); } },
    { getItem: () => null, setItem() { throw new Error('quota'); } }, { getItem: () => null, setItem() {} }]) {
    assert.equal(readGuidanceMute(storage, key).muted, false);
    assert.equal(muteGuidanceUntilAvailable(storage, key, 100), false);
  }
  for (const value of ['bad', '0', '-1', 'null', '{}', '1.5']) assert.equal(readGuidanceMute(memory({ [key]: value }), key).muted, false);
  assert.equal(muteGuidanceUntilAvailable(memory(), null), false);
  assert.equal(clearGuidanceMute(undefined, key), false);
});

test('setup and task progress cannot be persistently muted', () => {
  for (const phase of ['installCms', 'starting', 'checking', 'hidden', 'available']) assert.equal(guidanceReminderCanMute(phase), false);
  for (const phase of ['recover', 'unknown', 'connectionUnavailable', 'connectionUnknown', 'unlinked']) assert.equal(guidanceReminderCanMute(phase), true);
});
