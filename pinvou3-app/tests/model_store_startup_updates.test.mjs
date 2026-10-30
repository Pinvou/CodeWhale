import assert from 'node:assert/strict';
import test from 'node:test';
import { scheduleStartupUpdateCheck, STARTUP_UPDATE_DELAY_MS, updatesForLibrary, initialStartupUpdateState, reduceStartupUpdates, hasUnreadStartupUpdates } from '../src/features/model-store/startup-update-check.js';
import { createStartupUpdateController, scheduleDelayedStartupUpdateCheck, schedulePreferredStartupUpdateCheck } from '../src/features/model-store/startup-update-runtime.js';
import { prepareAfterStartupPaint } from '../src/features/model-store/startup-update-scheduler.js';
import { readViewedUpdateSnapshot } from '../src/features/model-store/startup-update-snapshot.js';
import { VIEWED_UPDATES_STORAGE_KEY, loadViewedUpdateRevisions, saveViewedUpdateRevisions } from '../src/features/model-store/startup-update-read-state.js';

function memoryStorage(raw) {
  const values = new Map(raw === undefined ? [] : [[VIEWED_UPDATES_STORAGE_KEY, raw]]);
  return {
    writes: 0,
    getItem: (key) => values.get(key) ?? null,
    setItem(key, value) { values.set(key, value); this.writes += 1; },
  };
}

function fixture(available = true, check = async () => ({ updates: [] }), prepare) {
  let counter = 0;
  const frames = new Map();
  const timers = new Map();
  const reports = [];
  const cleanup = scheduleStartupUpdateCheck({
    available, check, prepare, onReport: (report) => reports.push(report),
    host: {
      performance: { now: () => 100 },
      requestAnimationFrame: (callback) => { frames.set(++counter, callback); return counter; },
      cancelAnimationFrame: (id) => frames.delete(id),
      setTimeout: (callback, delay) => { timers.set(++counter, { callback, delay }); return counter; },
      clearTimeout: (id) => timers.delete(id),
    },
  });
  const paint = () => { const batch = [...frames.values()]; frames.clear(); batch.forEach((callback) => callback()); };
  const fire = () => { const batch = [...timers.values()]; timers.clear(); batch.forEach(({ callback }) => callback()); };
  return { frames, timers, reports, cleanup, paint, fire };
}

function preferredFixture(readSettings) {
  const timers = new Map();
  const reports = [];
  let checks = 0;
  const cleanup = schedulePreferredStartupUpdateCheck({
    deadline: 15100, readSettings,
    host: {
      performance: { now: () => 100 },
      setTimeout(callback, delay) { timers.set(1, { callback, delay }); return 1; },
      clearTimeout(id) { timers.delete(id); },
    },
    check: async () => { checks += 1; return { updates: [{ card_id: 'a' }] }; },
    onReport: (report) => reports.push(report),
  });
  return { timers, reports, cleanup, checks: () => checks, fire() { const batch = [...timers.values()]; timers.clear(); batch.forEach(({ callback }) => callback()); } };
}

test('startup opt-out queues no timer or check and missing old-host fields retain the enabled default', async () => {
  for (const settings of [{ startup_update_check_enabled: false }, null, [], { startup_update_check_enabled: 'true' }]) {
    const f = preferredFixture(async () => settings);
    await new Promise(setImmediate);
    assert.equal(f.timers.size, 0);
    assert.equal(f.checks(), 0);
    f.cleanup();
  }
  const f = preferredFixture(async () => ({}));
  await new Promise(setImmediate);
  assert.equal(f.timers.get(1).delay, 15000);
  f.fire(); await new Promise(setImmediate);
  assert.equal(f.checks(), 1);
  f.cleanup();
});

test('a queued startup check rereads a newly saved opt-out before invoking the command', async () => {
  let enabled = true;
  let reads = 0;
  const f = preferredFixture(async () => { reads += 1; return { startup_update_check_enabled: enabled }; });
  await new Promise(setImmediate);
  assert.equal(reads, 1);
  enabled = false;
  f.fire(); await new Promise(setImmediate);
  assert.equal(reads, 2);
  assert.equal(f.checks(), 0);
  assert.deepEqual(f.reports, [{ updates: [] }]);
  f.cleanup();
});

test('failed startup policy reads fail closed without exposing details or retrying', async (t) => {
  const warning = t.mock.method(console, 'warn', () => {});
  for (const phase of ['initial', 'queued']) {
    let reads = 0;
    const f = preferredFixture(async () => {
      reads += 1;
      if (phase === 'initial' || reads === 2) throw Error('private policy path');
      return { startup_update_check_enabled: true };
    });
    await new Promise(setImmediate);
    if (phase === 'queued') { f.fire(); await new Promise(setImmediate); }
    assert.equal(f.checks(), 0);
    assert.equal(f.timers.size, 0);
    f.cleanup();
  }
  assert.deepEqual(warning.mock.calls.map(({ arguments: args }) => args), Array(2).fill(['model_store_startup_update_check failed']));
});

test('cleanup during either policy read prevents a late timer or request', async () => {
  for (const phase of ['initial', 'queued']) {
    let resolve;
    let reads = 0;
    const f = preferredFixture(() => {
      reads += 1;
      if (phase === 'initial' || reads === 2) return new Promise((done) => { resolve = done; });
      return { startup_update_check_enabled: true };
    });
    await new Promise(setImmediate);
    if (phase === 'queued') { f.fire(); await new Promise(setImmediate); }
    f.cleanup();
    resolve({ startup_update_check_enabled: true });
    await new Promise(setImmediate);
    assert.equal(f.timers.size, 0);
    assert.equal(f.checks(), 0);
    assert.deepEqual(f.reports, []);
  }
});

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


test('prepared-check compatibility callers retain the original two-frame timer schedule', async () => {
  let preparations = 0;
  let requests = 0;
  const f = fixture(true, async () => { requests += 1; return { updates: [] }; }, () => {
    preparations += 1;
    return new Promise(() => {});
  });
  assert.equal(preparations, 0);
  f.paint();
  assert.equal(preparations, 0);
  f.paint();
  assert.equal(preparations, 1);
  assert.equal([...f.timers.values()][0].delay, 15000);
  f.fire();
  await new Promise(setImmediate);
  assert.equal(requests, 1);
  assert.equal(f.reports.length, 1);
  f.cleanup();
});

test('cancellation before painting prevents runtime preparation as well as IPC', () => {
  let preparations = 0;
  const f = fixture(true, () => assert.fail('must not request'), () => { preparations += 1; });
  f.paint();
  f.cleanup();
  f.paint();
  assert.equal(preparations, 0);
});

test('a loaded controller applies report, acknowledgement and library changes synchronously', () => {
  const updates = [{ card_id: 'a', local_revision: 'old', revision: 'new' }];
  let latest;
  const controller = createStartupUpdateController('[]', (state) => { latest = state; });
  controller.send({ type: 'report', updates, models: [{ card_id: 'a', card_revision: 'old' }], viewingStore: false });
  assert.equal(latest.hasUnread, true);
  controller.send({ type: 'viewed' });
  assert.deepEqual(latest.updates, updates);
  assert.equal(latest.hasUnread, false);
  controller.send({ type: 'library', models: [{ card_id: 'a', card_revision: 'new' }] });
  assert.deepEqual(latest.updates, []);
});

test('controller responses received while the store is open are already read', () => {
  let latest;
  const controller = createStartupUpdateController('[]', (state) => { latest = state; });
  controller.send({ type: 'report', updates: [{ card_id: 'a', revision: 'new' }], models: null, viewingStore: true });
  assert.equal(latest.hasUnread, false);
  assert.equal(latest.updates.length, 1);
});

test('runtime parsing uses the mounted snapshot and saving merges newer window acknowledgements', () => {
  const first = JSON.stringify(['a', 'v1']);
  const second = JSON.stringify(['b', 'v2']);
  const storage = memoryStorage(JSON.stringify([first]));
  const snapshot = readViewedUpdateSnapshot(storage);
  storage.setItem(VIEWED_UPDATES_STORAGE_KEY, JSON.stringify([second]));
  let latest;
  const controller = createStartupUpdateController(snapshot, (state) => { latest = state; }, storage);
  controller.send({ type: 'report', updates: [{ card_id: 'a', revision: 'v1' }], models: null });
  assert.equal(latest.hasUnread, false, 'parsing later must retain the value read synchronously during mount');
  assert.deepEqual(new Set(loadViewedUpdateRevisions(storage)), new Set([first, second]));
  let restarted;
  const next = createStartupUpdateController(readViewedUpdateSnapshot(storage), (state) => { restarted = state; });
  next.send({ type: 'report', updates: [{ card_id: 'a', revision: 'v1' }, { card_id: 'b', revision: 'v2' }], models: null });
  assert.equal(restarted.hasUnread, false);
  assert.equal(restarted.updates.length, 2);
});


test('delayed runtime loading uses the original deadline, including an already elapsed delay', async () => {
  for (const [now, remaining] of [[100, 15000], [4100, 11000], [16100, 0]]) {
    let timer;
    let calls = 0;
    let reports = 0;
    const cleanup = scheduleDelayedStartupUpdateCheck({
      deadline: 15100,
      host: {
        performance: { now: () => now },
        setTimeout(callback, delay) { timer = { callback, delay }; return 1; },
        clearTimeout() { timer = null; },
      },
      check: async () => { calls += 1; return { updates: [] }; },
      onReport: () => { reports += 1; },
    });
    assert.equal(timer.delay, remaining);
    assert.equal(calls, 0);
    timer.callback();
    await new Promise(setImmediate);
    assert.equal(calls, 1);
    assert.equal(reports, 1);
    cleanup();
    assert.equal(timer, null);
  }
});


test('the earliest deadline is measured after the second paint rather than mounting', () => {
  const frames = new Map();
  let id = 0;
  let now = 0;
  let deadline;
  const cleanup = prepareAfterStartupPaint((value) => { deadline = value; }, {
    performance: { now: () => now },
    requestAnimationFrame(callback) { frames.set(++id, callback); return id; },
    cancelAnimationFrame(frame) { frames.delete(frame); },
  });
  const paint = () => {
    const callbacks = [...frames.values()];
    frames.clear();
    callbacks.forEach((callback) => callback());
  };
  now = 1000;
  paint();
  assert.equal(deadline, undefined);
  now = 5000;
  paint();
  assert.equal(deadline, 20000);
  cleanup();
  assert.equal(frames.size, 0);
});


test('availability loss cancels the loaded runtime timer before a request starts', async () => {
  let callback;
  let requests = 0;
  const cleanup = scheduleDelayedStartupUpdateCheck({
    deadline: 15000,
    host: {
      performance: { now: () => 0 },
      setTimeout(run) { callback = run; return 7; },
      clearTimeout(id) { assert.equal(id, 7); callback = null; },
    },
    check: () => { requests += 1; },
    onReport: () => assert.fail('cancelled checks must not publish'),
  });
  cleanup();
  callback?.();
  await new Promise(setImmediate);
  assert.equal(requests, 0);
});

test('loaded runtime suppresses an in-flight response after cleanup without a retry', async () => {
  let callback;
  let resolve;
  let requests = 0;
  const reports = [];
  const cleanup = scheduleDelayedStartupUpdateCheck({
    deadline: 15000,
    host: {
      performance: { now: () => 15000 },
      setTimeout(run, delay) { assert.equal(delay, 0); callback = run; return 8; },
      clearTimeout(id) { assert.equal(id, 8); callback = null; },
    },
    check: () => { requests += 1; return new Promise((done) => { resolve = done; }); },
    onReport: (report) => reports.push(report),
  });
  callback();
  await new Promise(setImmediate);
  cleanup();
  resolve({ updates: [{ card_id: 'late' }] });
  await new Promise(setImmediate);
  assert.equal(requests, 1);
  assert.deepEqual(reports, []);
  assert.equal(callback, null);
});


test('controller persists only acknowledgement changes and keeps failures usable in memory', () => {
  const storage = memoryStorage();
  const updates = [{ card_id: 'a', local_revision: 'old', revision: 'v1' }];
  let latest;
  const controller = createStartupUpdateController('[]', (state) => { latest = state; }, storage);
  assert.equal(storage.writes, 0, 'initialization must not rewrite an empty snapshot');
  controller.send({ type: 'report', updates, models: null, viewingStore: false });
  assert.equal(storage.writes, 0, 'an unread response must not write acknowledgement data');
  controller.send({ type: 'viewed' });
  assert.equal(storage.writes, 1);
  assert.equal(latest.hasUnread, false);
  controller.send({ type: 'viewed' });
  controller.send({ type: 'library', models: [] });
  assert.equal(storage.writes, 1, 'repeat acknowledgement and library changes must not rewrite storage');
  const blocked = { getItem() { throw Error('private'); }, setItem() { throw Error('quota'); } };
  let inMemory;
  const failing = createStartupUpdateController('[]', (state) => { inMemory = state; }, blocked);
  failing.send({ type: 'report', updates, models: null, viewingStore: true });
  assert.equal(inMemory.hasUnread, false);
  assert.deepEqual(inMemory.updates, updates);
});
