import assert from 'node:assert/strict';
import { test } from 'node:test';
import { cmsInstallReminderKey, readCmsInstallReminder, setCmsInstallReminder, reconcileCmsInstallReminder } from '../src/features/megacube/cms-install-reminder.js';
import { guidanceMuteKey, muteGuidanceUntilAvailable, readGuidanceMute, reconcileGuidanceMute } from '../src/features/megacube/model-guidance-mute.js';
import { guidanceStorageKey } from '../src/features/megacube/model-guidance-state.js';

const absent = { supported: true, scope: 'user', installed: false, canInstall: true, recoveryScope: 'a'.repeat(64) };
function memory(seed = {}) {
  const values = new Map(Object.entries(seed)), writes = [];
  return { values, writes, getItem: key => values.get(key) ?? null,
    setItem: (key, value) => { writes.push(['set', key]); values.set(key, value); },
    removeItem: key => { writes.push(['remove', key]); values.delete(key); } };
}

test('installation dismissal survives reopening and is specific to the profile/socket, not chat model', () => {
  const storage = memory();
  assert.equal(setCmsInstallReminder(storage, absent, true, 100), true);
  const reopened = memory(Object.fromEntries(storage.values));
  assert.equal(readCmsInstallReminder(reopened, absent).muted, true);
  assert.equal(readCmsInstallReminder(reopened, { ...absent, recoveryScope: 'b'.repeat(64) }).muted, false);
  assert.equal(cmsInstallReminderKey({ recoveryScope: 'invalid' }), null);
  assert.equal(cmsInstallReminderKey(undefined), null);
  for (let i = 0; i < 1000; i++) {
    assert.equal(reconcileCmsInstallReminder(storage, absent).muted, true);
    assert.equal(setCmsInstallReminder(storage, absent, true, 200 + i), true);
  }
  assert.equal(storage.writes.length, 1);
});

test('healthy models, offline queries and unverified installations never reset installation dismissal', () => {
  const storage = memory(); setCmsInstallReminder(storage, absent, true, 100);
  reconcileGuidanceMute(storage, guidanceStorageKey(absent.recoveryScope), null, { phase: 'available', available: [] });
  assert.equal(readCmsInstallReminder(storage, absent).muted, true);
  for (const service of [null, { ...absent, activeState: 'active' }, { ...absent, installed: null },
    { ...absent, installed: true, scope: 'unverified' }, { ...absent, installed: true, supported: false }]) {
    reconcileCmsInstallReminder(storage, service);
    assert.equal(readCmsInstallReminder(storage, absent).muted, true);
  }
  assert.equal(storage.writes.length, 1);
});

test('confirmed installation resets once, so a later uninstall can show the banner again', () => {
  const storage = memory(); setCmsInstallReminder(storage, absent, true, 100);
  for (let i = 0; i < 1000; i++) {
    const result = reconcileCmsInstallReminder(storage, { ...absent, installed: true, canInstall: false, activeState: 'inactive' });
    assert.equal(result.installed, true); assert.equal(result.muted, false);
  }
  assert.equal(storage.writes.length, 2);
  assert.equal(reconcileCmsInstallReminder(storage, absent).muted, false);
});

test('manual restore and installation reconciliation do not alter model outage preferences', () => {
  const storage = memory(), modelKey = guidanceMuteKey(guidanceStorageKey(absent.recoveryScope), null);
  muteGuidanceUntilAvailable(storage, modelKey, 100);
  setCmsInstallReminder(storage, absent, true, 100);
  assert.equal(setCmsInstallReminder(storage, absent, false), true);
  assert.equal(readCmsInstallReminder(storage, absent).muted, false);
  assert.equal(readGuidanceMute(storage, modelKey).muted, true);
  setCmsInstallReminder(storage, absent, true, 100);
  reconcileCmsInstallReminder(storage, { ...absent, installed: true });
  assert.equal(readGuidanceMute(storage, modelKey).muted, true);
});

test('denied storage, malformed values, dropped writes and failed removals fail visibly', () => {
  const key = cmsInstallReminderKey(absent);
  for (const storage of [undefined, { getItem() { throw new Error('denied'); } },
    { getItem: () => null, setItem() { throw new Error('quota'); } }, { getItem: () => null, setItem() {} }]) {
    assert.equal(readCmsInstallReminder(storage, absent).muted, false);
    assert.equal(setCmsInstallReminder(storage, absent, true), false);
  }
  for (const value of ['bad', '0', '-1', '{}', '1.5']) assert.equal(readCmsInstallReminder(memory({ [key]: value }), absent).muted, false);
  const stuck = { getItem: () => '100', removeItem() {} };
  assert.equal(setCmsInstallReminder(stuck, absent, false), false);
  assert.equal(setCmsInstallReminder(memory(), absent, true, -1), false);
  assert.equal(setCmsInstallReminder(memory(), null, true), false);
});
