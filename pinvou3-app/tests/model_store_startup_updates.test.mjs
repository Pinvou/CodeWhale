import assert from 'node:assert/strict';
import test from 'node:test';
import { scheduleStartupUpdateCheck, STARTUP_UPDATE_DELAY_MS, updatesForLibrary, initialStartupUpdateState, reduceStartupUpdates, hasUnreadStartupUpdates } from '../src/features/model-store/startup-update-check.js';
import { VIEWED_UPDATES_STORAGE_KEY, loadViewedUpdateRevisions, saveViewedUpdateRevisions } from '../src/features/model-store/startup-update-read-state.js';

function memoryStorage(raw) {
  const values = new Map(raw === undefined ? [] : [[VIEWED_UPDATES_STORAGE_KEY, raw]]);
  return {
    writes: 0,
    getItem: (key) => values.get(key) ?? null,
    setItem(key, value) { values.set(key, value); this.writes += 1; },
  };
}

function fixture(available = true, check = async () => ({ updates: [] })) {
  let counter = 0;
  const frames = new Map();
  const timers = new Map();
  const reports = [];
  const cleanup = scheduleStartupUpdateCheck({
    available, check, onReport: (report) => reports.push(report),
    requestFrame: (callback) => { frames.set(++counter, callback); return counter; },
    cancelFrame: (id) => frames.delete(id),
    setTimer: (callback, delay) => { timers.set(++counter, { callback, delay }); return counter; },
    clearTimer: (id) => timers.delete(id),
  });
  const paint = () => { const batch = [...frames.values()]; frames.clear(); batch.forEach((callback) => callback()); };
  const fire = () => { const batch = [...timers.values()]; timers.clear(); batch.forEach(({ callback }) => callback()); };
  return { frames, timers, reports, cleanup, paint, fire };
}

test('unavailable model stores schedule no frames, timers or requests', () => {
  const f = fixture(false, () => assert.fail('must not request an unavailable feature'));
  assert.equal(f.frames.size, 0); assert.equal(f.timers.size, 0); f.cleanup();
});

test('startup detection runs once after two frames and a fifteen-second delay', async () => {
  let calls = 0;
  const f = fixture(true, async () => { calls += 1; return { updates: [{ card_id: 'a' }] }; });
  assert.equal(calls, 0); f.paint(); assert.equal(f.timers.size, 0);
  f.paint(); assert.equal([...f.timers.values()][0].delay, STARTUP_UPDATE_DELAY_MS);
  assert.equal(STARTUP_UPDATE_DELAY_MS, 15000); assert.equal(calls, 0);
  f.fire(); await new Promise(setImmediate);
  assert.equal(calls, 1); assert.equal(f.reports.length, 1);
  assert.equal(f.frames.size, 0); assert.equal(f.timers.size, 0);
});

test('unmount or availability loss cancels the delayed check', () => {
  const f = fixture(true, () => assert.fail('cancelled checks must not run'));
  f.paint(); f.paint(); f.cleanup(); f.fire();
  assert.equal(f.frames.size, 0); assert.equal(f.timers.size, 0);
});

test('network failure logs only the operation and does not schedule automatic retries', async (t) => {
  const warning = t.mock.method(console, 'warn', () => {});
  let calls = 0;
  const f = fixture(true, async () => { calls += 1; throw Error('private backend error with credential'); });
  f.paint(); f.paint(); f.fire(); await new Promise(setImmediate);
  assert.equal(calls, 1); assert.equal(f.reports.length, 0);
  assert.equal(f.frames.size, 0); assert.equal(f.timers.size, 0);
  assert.deepEqual(warning.mock.calls.map(({ arguments: args }) => args), [['model_store_startup_update_check failed']]);
});

test('late results do not update an unmounted consumer', async () => {
  let resolve;
  const f = fixture(true, () => new Promise((done) => { resolve = done; }));
  f.paint(); f.paint(); f.fire(); await new Promise(setImmediate); f.cleanup();
  resolve({ updates: [{ card_id: 'a' }] }); await new Promise(setImmediate);
  assert.equal(f.reports.length, 0);
});

test('completed updates, version switches and removals clear stale reminders', () => {
  const updates = [{ card_id: 'a', local_revision: 'old' }, { card_id: 'b', local_revision: 'same' }];
  assert.equal(updatesForLibrary(updates, null), updates);
  assert.equal(updatesForLibrary(updates, [{ card_id: 'a', card_revision: 'old' }, { card_id: 'b', card_revision: 'same' }]), updates);
  assert.deepEqual(updatesForLibrary(updates, [{ card_id: 'a', card_revision: 'new' }, { card_id: 'b', card_revision: 'same' }]), [updates[1]]);
  assert.deepEqual(updatesForLibrary(updates, []), []);
});

test('entering the store acknowledges sidebar reminders and retains model update badges', () => {
  const updates = [{ card_id: 'a', local_revision: 'installed', revision: 'new' }];
  let state = reduceStartupUpdates(initialStartupUpdateState(), { type: 'report', updates, models: null });
  assert.equal(hasUnreadStartupUpdates(state), true);
  state = reduceStartupUpdates(state, { type: 'viewed' });
  assert.equal(hasUnreadStartupUpdates(state), false);
  assert.equal(state.updates, updates, 'viewing the store must not consume available model updates');
  assert.equal(reduceStartupUpdates(state, { type: 'viewed' }), state);
  state = reduceStartupUpdates(state, { type: 'report', updates: [...updates], models: null });
  assert.equal(hasUnreadStartupUpdates(state), false, 'replaying the same report must not restore the sidebar reminder');
});

test('results arriving while the store is open are already viewed', () => {
  const updates = [{ card_id: 'a', local_revision: 'installed', revision: 'new' }];
  const state = reduceStartupUpdates(initialStartupUpdateState(), { type: 'report', updates, models: null, viewingStore: true });
  assert.equal(hasUnreadStartupUpdates(state), false);
  assert.equal(state.updates.length, 1);
});

test('acknowledgement is scoped to the card and remote revision', () => {
  let state = reduceStartupUpdates(initialStartupUpdateState(), { type: 'report', updates: [{ card_id: 'a', revision: 'v1' }], models: null, viewingStore: true });
  state = reduceStartupUpdates(state, { type: 'report', updates: [{ card_id: 'a', revision: 'v2' }], models: null });
  assert.equal(hasUnreadStartupUpdates(state), true);
  state = reduceStartupUpdates(state, { type: 'viewed' });
  state = reduceStartupUpdates(state, { type: 'report', updates: [{ card_id: 'b', revision: 'v2' }], models: null });
  assert.equal(hasUnreadStartupUpdates(state), true);
});

test('app restart keeps viewed sidebar updates read without hiding model badges or skipping detection', async () => {
  const storage = memoryStorage();
  const updates = [{ card_id: 'a', local_revision: 'installed', revision: 'new' }];
  let checks = 0;
  async function startApp(viewingStore) {
    let state = initialStartupUpdateState(loadViewedUpdateRevisions(storage));
    const startup = fixture(true, async () => { checks += 1; return { updates }; });
    startup.paint(); startup.paint(); startup.fire(); await new Promise(setImmediate);
    state = reduceStartupUpdates(state, { type: 'report', updates: startup.reports[0].updates, models: null, viewingStore });
    return state;
  }
  const viewed = await startApp(true);
  assert.equal(saveViewedUpdateRevisions(viewed.viewedRevisions, storage), true);
  const restarted = await startApp(false);
  assert.equal(checks, 2, 'each app launch still performs its own startup check');
  assert.equal(hasUnreadStartupUpdates(restarted), false);
  assert.deepEqual(restarted.updates, updates, 'installed cards keep their available update badges');
});

test('persisted acknowledgement does not suppress a new revision or a different card', () => {
  const storage = memoryStorage();
  const viewed = reduceStartupUpdates(initialStartupUpdateState(), { type: 'report', updates: [{ card_id: 'a', revision: 'v1' }], viewingStore: true });
  saveViewedUpdateRevisions(viewed.viewedRevisions, storage);
  for (const updates of [[{ card_id: 'a', revision: 'v2' }], [{ card_id: 'b', revision: 'v1' }]]) {
    const restarted = reduceStartupUpdates(initialStartupUpdateState(loadViewedUpdateRevisions(storage)), { type: 'report', updates });
    assert.equal(hasUnreadStartupUpdates(restarted), true);
  }
});

test('saving a window merges acknowledgements saved by another window', () => {
  const storage = memoryStorage();
  const first = reduceStartupUpdates(initialStartupUpdateState(), { type: 'report', updates: [{ card_id: 'a', revision: 'v1' }], viewingStore: true });
  const second = reduceStartupUpdates(initialStartupUpdateState(), { type: 'report', updates: [{ card_id: 'b', revision: 'v2' }], viewingStore: true });
  saveViewedUpdateRevisions(first.viewedRevisions, storage);
  saveViewedUpdateRevisions(second.viewedRevisions, storage);
  const restarted = reduceStartupUpdates(initialStartupUpdateState(loadViewedUpdateRevisions(storage)), {
    type: 'report', updates: [...first.updates, ...second.updates],
  });
  assert.equal(hasUnreadStartupUpdates(restarted), false);
  assert.equal(restarted.updates.length, 2);
});

test('malformed persisted values are ignored and valid revision pairs are retained', () => {
  for (const raw of ['{', 'null', '{}', '42', '"invalid"']) {
    assert.equal(loadViewedUpdateRevisions(memoryStorage(raw)).size, 0);
  }
  const key = JSON.stringify(['a', 'v1']);
  const storage = memoryStorage(JSON.stringify([key, key, 'not json', null, 4, '{}', '[]', '["a",null]', '["", "v1"]', '["a","v1","extra"]']));
  assert.deepEqual([...loadViewedUpdateRevisions(storage)], [key]);
});

test('unavailable or full persistence keeps acknowledgement usable in memory', () => {
  const blocked = { getItem() { throw Error('blocked'); }, setItem() { throw Error('blocked'); } };
  const full = { getItem: () => null, setItem() { throw Error('quota'); } };
  const viewed = reduceStartupUpdates(initialStartupUpdateState(), { type: 'report', updates: [{ card_id: 'a', revision: 'v1' }], viewingStore: true });
  for (const storage of [null, blocked, full]) {
    assert.equal(loadViewedUpdateRevisions(storage).size, 0);
    assert.equal(saveViewedUpdateRevisions(viewed.viewedRevisions, storage), false);
    assert.equal(hasUnreadStartupUpdates(viewed), false);
    assert.equal(viewed.updates.length, 1);
  }
});

test('initialization and repeated saves do not rewrite storage unnecessarily', () => {
  const storage = memoryStorage();
  saveViewedUpdateRevisions(new Set(), storage);
  assert.equal(storage.writes, 0);
  const keys = new Set([JSON.stringify(['a', 'v1'])]);
  saveViewedUpdateRevisions(keys, storage);
  saveViewedUpdateRevisions(keys, storage);
  assert.equal(storage.writes, 1);
});
