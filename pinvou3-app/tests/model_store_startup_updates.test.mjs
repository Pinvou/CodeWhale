import assert from 'node:assert/strict';
import test from 'node:test';
import { scheduleStartupUpdateCheck, STARTUP_UPDATE_DELAY_MS, updatesForLibrary, initialStartupUpdateState, reduceStartupUpdates, hasUnreadStartupUpdates } from '../src/features/model-store/startup-update-check.js';

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

test('network failure does not schedule automatic retries', async () => {
  let calls = 0;
  const f = fixture(true, async () => { calls += 1; throw Error('offline'); });
  f.paint(); f.paint(); f.fire(); await new Promise(setImmediate);
  assert.equal(calls, 1); assert.equal(f.reports.length, 0);
  assert.equal(f.frames.size, 0); assert.equal(f.timers.size, 0);
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
