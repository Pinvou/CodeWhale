import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { createCmsdSwitchFollow } from '../src/features/megacube/switch-follow.js';
import * as cmsApi from '../src/features/megacube/cms-api.js';
import { mergeHandoffRecovery } from '../src/features/megacube/handoff-recovery.js';

const details = JSON.parse(fs.readFileSync(new URL('./fixtures/cmsd-switch-task-details.json', import.meta.url), 'utf8'));
const task = details.succeeded.body;
const receipt = (id = task.id, sessionId = 'origin') => ({ task: { ...task, id }, sessionId, binding: 'source', fallback: 'global', started: Date.now() - 60000 });
const flush = () => new Promise(resolve => setImmediate(resolve));
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return { resolve, promise }; };
function fixture({ pending = [receipt()], queryTask = async () => ({ available: true, data: task }), ...overrides } = {}) {
  const rows = [], selections = [], observations = [];
  let savedPending = [];
  const bindings = new Map(pending.map(value => [value.sessionId, 'source']));
  const state = { activeSessionId: 'origin', activeModelId: 'global', currentSessionModelId: 'source', sessionBusy: {} };
  const api = createCmsdSwitchFollow({
    getState: () => state,
    queryModels: async () => ({ available: true, data: { models: [{ card: 'target', model_id: 'Target', managed: true, status: 'running', healthy: true, version_id: 'v1', deployment_revision: 'rev1', port: 18081 }] } }),
    queryStatus: async () => ({ available: true, data: { active_model: 'target', gateway: '127.0.0.1:8000' } }),
    queryTask, listModels: async () => ({ models: rows, active_model_id: 'global' }), sessionModelId: async id => bindings.get(id),
    saveModel: async row => { rows.push(row); }, switchModel: async (id, modelId) => { selections.push({ id, modelId }); bindings.set(id, modelId); },
    makeRegistrationId: async () => 'm-target', loadPending: () => pending,
    savePending: values => { savedPending = structuredClone(values); }, onTaskRecovered: value => observations.push(value),
    listCards: async () => [], notify: () => {}, ...overrides,
  });
  return { api, rows, selections, observations, bindings, state, pending: () => savedPending };
}
function hookSource(name) {
  return fs.readFileSync(new URL(`../src/features/megacube/${name}.js`, import.meta.url), 'utf8')
    .replace(/^import .*;\n/gm, '').replace(/export function /g, 'function ');
}

test('the real renderer recovery hook resolves an accepted switch absent from the latest-20 task index', async () => {
  const rows = [], selected = [], intervals = [], effects = [], cleanups = [];
  const original = receipt();
  const recent = Array.from({ length: 20 }, (_, index) => ({ id: `t-later-${index}`, kind: 'set_default', card: 'target', status: 'succeeded' }));
  const detailReads = [], stateSlots = [];
  let stateIndex = 0;
  const previousTauri = globalThis.__TAURI__;
  globalThis.__TAURI__ = { core: { invoke: async (command, args) => {
    assert.equal(command, 'cms_task'); detailReads.push(args.taskId);
    return { available: true, data: task };
  } } };
  let binding = 'source';
  const state = { activeSessionId: 'origin', currentSessionModelId: 'source', activeModelId: 'global', sessionBusy: {} };
  const context = {
    useRef: value => ({ current: value }), useCallback: callback => callback, useEffect: callback => effects.push(callback),
    useState: initial => {
      const index = stateIndex++; stateSlots[index] = index === 0 ? true : initial;
      return [stateSlots[index], value => { stateSlots[index] = typeof value === 'function' ? value(stateSlots[index]) : value; }];
    },
    createCmsdSwitchFollow, mergeHandoffRecovery,
    invokeTauri: async (command, args) => {
      if (command === 'cms_service_status') return { recoveryScope: 'a'.repeat(64), switchFollowRecovery: [original] };
      if (command === 'list_models') return { models: rows, active_model_id: 'global' };
      if (command === 'get_session_model_id') return binding;
      throw new Error(`unexpected command ${command}: ${JSON.stringify(args)}`);
    },
    fetchCmsModels: async () => ({ available: true, data: { models: [{ card: 'target', model_id: 'Target', status: 'running', healthy: true, version_id: 'v1', deployment_revision: 'rev1', port: 18081 }] } }),
    fetchCmsStatus: async () => ({ available: true, data: { active_model: 'target', gateway: '127.0.0.1:8000' } }),
    fetchCmsTask: cmsApi.fetchCmsTask,
    fetchCmsTasks: async () => ({ available: true, data: { tasks: recent, store_query: { available: true } } }),
    probeCmsAvailable: async () => true,
    tauriEvents: { listen: async () => () => {} }, listenManualModelSelection: () => () => {},
    localStorage: { length: 0, key: () => null, getItem: () => null, setItem: () => {}, removeItem: () => {} },
    setInterval: (callback, delay) => { intervals.push({ callback, delay }); return intervals.length; }, clearInterval: () => {},
    setTimeout, clearTimeout, console,
  };
  vm.createContext(context);
  vm.runInContext(hookSource('use-cmsd-tasks') + '\n' + hookSource('use-cmsd-switch-follow') + '\nthis.hook = useCmsdSwitchFollow;', context);
  context.hook({ bridge: { available: true, state: { getMany: () => state }, models: {
    saveModel: async row => { rows.push(row); }, switchModel: async (_session, id) => { binding = id; selected.push(id); }, loadModels: async () => {},
  } }, snapshot: state, copy: { linkAutoSelected: () => '', switchAutoSelected: () => '' }, onNotice: () => {} });
  for (const effect of effects) { const cleanup = effect(); if (cleanup) cleanups.push(cleanup); }
  try {
    // No old task is supplied through the public watcher. The production hook
    // must query its known receipt even if the recent index only has later work.
    await flush();
    assert.equal(selected.length, 1);
    assert.equal(binding, rows[0].id);
    assert.deepEqual(detailReads, [task.id]);
    assert.equal(recent.length, 20); assert.ok(!recent.some(value => value.id === task.id));
    assert.ok(!stateSlots.some(value => value?.id === task.id), 'terminal recovery must not create active/settled history dialogs');
    assert.equal(intervals.filter(value => value.delay === 3000).length, 2);
  } finally { for (const cleanup of cleanups) cleanup(); globalThis.__TAURI__ = previousTauri; }
});

test('the detail API uses the existing task JSON object and the registered read-only command', async () => {
  const previous = globalThis.__TAURI__;
  try {
    let request;
    globalThis.__TAURI__ = { core: { invoke: async (...args) => { request = args; return { available: true, data: task }; } } };
    const result = await cmsApi.fetchCmsTask(task.id);
    assert.deepEqual(request, ['cms_task', { taskId: task.id }]);
    assert.equal(result.data.id, task.id);
    assert.equal(result.data.task, undefined);
  } finally { globalThis.__TAURI__ = previous; }
});

test('an unavailable, rejected, malformed or future-state lookup never guesses completion', async () => {
  for (const result of [{ available: false }, { available: false, data: details.not_found.body }, null, { available: true }, { available: true, data: { ...task, status: 'future' } },
    { available: true, data: { ...task, status: undefined } }]) {
    const f = fixture({ queryTask: async () => result });
    try {
      await f.api.recoverPending();
      assert.equal(f.selections.length, 0); assert.equal(f.rows.length, 0); assert.equal(f.pending().length, 1);
    } finally { f.api.dispose(); }
  }
  const f = fixture({ queryTask: async () => { throw new Error('response lost'); } });
  try { await f.api.recoverPending(); assert.equal(f.selections.length, 0); assert.equal(f.pending().length, 1); } finally { f.api.dispose(); }
});

test('task, requested card, physical card, version and deployment mismatches cannot authorize recovery', async () => {
  for (const changed of [{ id: 't-other' }, { kind: 'enable' }, { requested_card: 'other' }, { card: 'different-runtime' },
    { selected_version: 'v2' }, { deployment_revision: 'rev2' }]) {
    const pending = receipt();
    pending.task.card = 'physical';
    pending.task.requested_card = 'logical';
    const matching = { ...task, card: 'physical', requested_card: 'logical' };
    const f = fixture({ pending: [pending], queryTask: async () => ({ available: true, data: { ...matching, ...changed } }) });
    try {
      await f.api.recoverPending(); assert.equal(f.selections.length, 0); assert.equal(f.rows.length, 0); assert.equal(f.pending().length, 1);
    } finally { f.api.dispose(); }
  }
});

test('known running work remains pending, then its exact successful task follows once', async () => {
  let status = 'running', calls = 0;
  const f = fixture({ queryTask: async () => { calls++; return { available: true, data: { ...task, status } }; } });
  try {
    await f.api.recoverPending(); assert.equal(f.selections.length, 0); assert.equal(f.pending().length, 1); assert.equal(f.observations[0].status, 'running');
    status = 'succeeded'; await f.api.recoverPending(); await f.api.recoverPending();
    assert.equal(f.selections.length, 1); assert.equal(f.pending().length, 0); assert.equal(calls, 2);
  } finally { f.api.dispose(); }
});

test('manual selection after a recovered running observation clears progress at terminal without reviving handoff', async () => {
  let status = 'running', calls = 0;
  const f = fixture({ queryTask: async () => { calls++; return { available: true, data: { ...task, status } }; } });
  try {
    await f.api.recoverPending();
    f.api.manualSelection({ sessionId: 'origin' });
    assert.equal(f.pending().length, 0);
    status = 'succeeded'; await f.api.recoverPending(); await f.api.recoverPending();
    assert.deepEqual(f.observations.map(value => value.status), ['running', 'succeeded']);
    assert.equal(f.selections.length, 0); assert.equal(f.rows.length, 0); assert.equal(calls, 2);
  } finally { f.api.dispose(); }
});

test('failed and cancelled known task receipts terminate without registration or selection', async () => {
  for (const status of ['failed', 'cancelled']) {
    const f = fixture({ queryTask: async () => ({ available: true, data: details[status].body }) });
    try { await f.api.recoverPending(); assert.equal(f.selections.length, 0); assert.equal(f.rows.length, 0); assert.equal(f.pending().length, 0); } finally { f.api.dispose(); }
  }
});

for (const mode of ['manual', 'dispose']) test(`${mode} during a detail lookup prevents the late response from applying`, async () => {
  const reply = deferred();
  const f = fixture({ queryTask: () => reply.promise });
  try {
    const reading = f.api.recoverPending();
    if (mode === 'manual') f.api.manualSelection({ sessionId: 'origin' }); else f.api.dispose();
    reply.resolve({ available: true, data: task }); await reading;
    assert.equal(f.selections.length, 0); assert.equal(f.rows.length, 0); assert.equal(f.observations.length, 0);
  } finally { f.api.dispose(); }
});

test('a slow pass is single-flight, at most eight serial reads, and rotates fairly past unknown tasks', async () => {
  const first = deferred();
  let active = 0, maximum = 0;
  const calls = [];
  const f = fixture({ pending: Array.from({ length: 19 }, (_, index) => receipt(`t-${index}`, `session-${index}`)), queryTask: async id => {
    active++; maximum = Math.max(maximum, active); calls.push(id);
    try { return calls.length === 1 ? await first.promise : { available: false }; } finally { active--; }
  } });
  try {
    const reading = f.api.recoverPending(); await flush();
    await f.api.recoverPending(); assert.equal(calls.length, 1);
    first.resolve({ available: false }); await reading;
    assert.equal(calls.length, 8); assert.equal(maximum, 1);
    await f.api.recoverPending(); assert.equal(calls.length, 16); assert.equal(new Set(calls).size, 16);
    await f.api.recoverPending(); assert.equal(new Set(calls).size, 19); assert.equal(maximum, 1);
  } finally { f.api.dispose(); }
});

test('an unknown task does not block a later valid receipt in the same pass', async () => {
  const f = fixture({ pending: [receipt('t-unknown', 'one'), receipt('t-ready', 'two')], queryTask: async id => id === 't-unknown'
    ? { available: false } : { available: true, data: { ...task, id } } });
  try {
    await f.api.recoverPending(); assert.deepEqual(f.selections, [{ id: 'two', modelId: 'm-target' }]);
    assert.deepEqual(f.pending().map(value => value.task.id), ['t-unknown']);
  } finally { f.api.dispose(); }
});

test('recovered running tasks remain backgroundable and terminal details never replay history dialogs', () => {
  const values = [], refs = [], effects = [];
  let stateIndex = 0, refIndex = 0, callbacks = 0;
  const context = { useState: initial => {
    const index = stateIndex++;
    if (!(index in values)) values[index] = initial;
    return [values[index], next => { values[index] = typeof next === 'function' ? next(values[index]) : next; }];
  }, useRef: initial => refs[refIndex++] ||= { current: initial }, useEffect: effect => effects.push(effect),
  fetchCmsTasks: async () => ({ available: true, data: { tasks: [] } }), fetchCmsStatus: async () => ({ available: true, data: {} }), probeCmsAvailable: async () => true,
  setInterval, clearInterval, console };
  vm.createContext(context); vm.runInContext(hookSource('use-cmsd-tasks') + '\nthis.hook = useCmsdTaskWatch;', context);
  const render = () => { stateIndex = 0; refIndex = 0; return context.hook({ onTaskSettled: () => { callbacks++; } }); };
  let state = render();
  state.observeRecoveredTask(task); state = render();
  assert.equal(state.settledTask, null); assert.equal(callbacks, 0);
  state.observeRecoveredTask({ ...task, status: 'running' }); state = render(); assert.equal(state.activeTask.id, task.id);
  state.dismissSettled(); state = render(); assert.equal(state.activeTask, null);
  state.observeRecoveredTask(task); state = render(); assert.equal(state.activeTask, null); assert.equal(state.settledTask, null); assert.equal(callbacks, 0);
});

// Execute both production hooks with stable hook slots. The library mock captures
// CMS state before awaiting, as model_store_library does before gateway I/O;
// saveModel mirrors its real current/deployed/running route precondition.
function routeRefreshHooks({ libraryRead = async () => ({}), indexIncludesTask = false, initiallyAvailable = true } = {}) {
  const target = { ...task, card: 'target--v2', requested_card: 'target', selected_version: 'v2', deployment_revision: 'recipe2' };
  let status = 'running', routeReady = false, binding = 'source', storeAvailable = initiallyAvailable, follower, manualListener;
  let refIndex = 0, stateIndex = 0, effectIndex = 0, callbackIndex = 0;
  const refs = [], states = [], effects = [], callbacks = [], pendingEffects = [], intervals = [], delayed = [];
  const rows = [], selections = [], notices = [], events = [], storage = new Map();
  const counts = { library: 0, saves: 0, followers: 0 };
  const state = { activeSessionId: 'origin', currentSessionModelId: 'source', activeModelId: 'global', sessionBusy: {} };
  const sameDeps = (a, b) => a && b && a.length === b.length && a.every((value, i) => Object.is(value, b[i]));
  const bridge = { available: true, state: { getMany: () => state }, models: {
    saveModel: async row => { counts.saves++; events.push(`save:${routeReady}`); if (!routeReady) throw Error('model store endpoint changed or is unavailable; refresh the card and retry'); rows.push(row); },
    switchModel: async (_sessionId, modelId) => { selections.push(modelId); binding = modelId; }, loadModels: async () => {},
  } };
  const context = {
    useRef: current => refs[refIndex++] ||= { current },
    useState: initial => {
      const index = stateIndex++; if (!(index in states)) states[index] = index === 0 ? true : initial;
      return [states[index], next => { states[index] = typeof next === 'function' ? next(states[index]) : next; }];
    },
    useEffect: (effect, deps) => {
      const index = effectIndex++;
      if (!sameDeps(effects[index]?.deps, deps)) pendingEffects.push(() => { effects[index]?.cleanup?.(); effects[index] = { deps, cleanup: effect() }; });
    },
    useCallback: (callback, deps) => {
      const index = callbackIndex++;
      if (!sameDeps(callbacks[index]?.deps, deps)) callbacks[index] = { deps, callback };
      return callbacks[index].callback;
    },
    createCmsdSwitchFollow: options => {
      counts.followers++;
      follower = createCmsdSwitchFollow({ ...options, makeRegistrationId: async () => 'm-route-target', schedule: (callback, delay) => { delayed.push({ callback, delay }); return callback; }, cancel: () => {} });
      return follower;
    }, mergeHandoffRecovery,
    invokeTauri: async command => {
      if (command === 'cms_service_status') return { recoveryScope: 'a'.repeat(64), switchFollowRecovery: [{ ...receipt(), task: target }] };
      if (command === 'get_session_model_id') return binding;
      if (command === 'list_models') return { models: rows, active_model_id: 'global' };
      if (command === 'model_store_library') {
        const captured = status, index = ++counts.library;
        events.push(`library:${captured}:start`);
        const result = await libraryRead({ index, captured });
        if (!result?.link_sync_error) routeReady = captured === 'succeeded';
        events.push(`library:${captured}:end`);
        return result;
      }
      throw Error(`unexpected command ${command}`);
    },
    fetchCmsModels: async () => ({ available: true, data: { store_bindings: { target: 'target--v2' }, models: [{ card: 'target--v2', managed: true, model_id: 'Target', version_id: 'v2', deployment_revision: 'recipe2', status: 'running', healthy: status === 'succeeded' }] } }),
    fetchCmsStatus: async () => ({ available: true, data: { active_model: 'target--v2', gateway: '127.0.0.1:8000' } }),
    fetchCmsTask: async () => ({ available: true, data: { ...target, status } }),
    fetchCmsTasks: async () => ({ available: true, data: { tasks: indexIncludesTask ? [{ ...target, status }] : Array.from({ length: 20 }, (_, i) => ({ id: `t-later-${i}`, kind: 'set_default', card: target.card, status: 'succeeded' })) } }),
    probeCmsAvailable: async () => true, tauriEvents: { listen: async () => () => {} },
    listenManualModelSelection: callback => { manualListener = callback; return () => {}; },
    localStorage: { get length() { return storage.size; }, key: index => [...storage.keys()][index], getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key) },
    setInterval: (callback, delay) => { const timer = { callback, delay }; intervals.push(timer); return timer; }, clearInterval: () => {},
    setTimeout: () => {}, clearTimeout: () => {}, console: { warn: () => {} },
  };
  vm.createContext(context); vm.runInContext(hookSource('use-cmsd-tasks') + '\n' + hookSource('use-cmsd-switch-follow') + '\nthis.hook = useCmsdSwitchFollow;', context);
  const render = () => {
    refIndex = stateIndex = effectIndex = callbackIndex = 0;
    const result = context.hook({ bridge, snapshot: state, modelStoreAvailable: storeAvailable, copy: { linkAutoSelected: () => 'selected', switchAutoSelected: () => 'selected', linkAutoSelectFailed: 'failed' }, onNotice: notice => notices.push(notice) });
    for (const effect of pendingEffects.splice(0)) effect();
    return result;
  };
  const destroy = () => { for (const effect of effects) effect?.cleanup?.(); follower?.dispose(); };
  render();
  return { counts, rows, selections, notices, events, delayed, states,
    complete: () => { status = 'succeeded'; },
    poll: async (index = false) => { intervals.filter(timer => timer.delay === 3000)[index ? 1 : 0].callback(); await flush(); },
    pending: () => [...storage].filter(([key]) => !key.includes('.done.')).map(([, value]) => JSON.parse(value)),
    manual: () => { binding = 'manual'; manualListener({ sessionId: 'origin' }); }, binding: () => binding,
    enableStore: () => { storeAvailable = true; render(); }, render, destroy,
  };
}

test('recovery refreshes store routes after success when startup read the still-running task', async () => {
  const f = routeRefreshHooks();
  try {
    await flush(); assert.equal(f.counts.library, 1); assert.equal(f.counts.saves, 0);
    f.complete(); await f.poll();
    assert.equal(f.counts.library, 2); assert.equal(f.counts.saves, 1); assert.deepEqual(f.selections, ['m-route-target']);
    assert.equal(f.pending().length, 0); assert.deepEqual(f.delayed, []); assert.deepEqual(f.notices, ['selected']);
    assert.ok(f.events.indexOf('library:succeeded:end') < f.events.indexOf('save:true'));
  } finally { f.destroy(); }
});

for (const failure of ['throw', 'link_sync_error']) test(`a ${failure} route refresh remains unknown until next recovery poll without spending follow retries`, async () => {
  const f = routeRefreshHooks({ libraryRead: async ({ index }) => {
    if (index === 2) { if (failure === 'throw') throw Error('temporary scan failure'); return { link_sync_error: 'temporary link failure' }; }
    return {};
  } });
  try {
    await flush(); f.complete(); await f.poll();
    assert.equal(f.counts.library, 2); assert.equal(f.counts.saves, 0); assert.equal(f.pending().length, 1);
    assert.deepEqual(f.delayed, []); assert.deepEqual(f.notices, []);
    await f.poll(); assert.equal(f.counts.library, 3); assert.equal(f.counts.saves, 1); assert.deepEqual(f.selections, ['m-route-target']);
  } finally { f.destroy(); }
});

test('a delayed fresh route scan finishes before recovered task registration', async () => {
  const scan = deferred();
  const f = routeRefreshHooks({ libraryRead: ({ index }) => index === 2 ? scan.promise : Promise.resolve({}) });
  try {
    await flush(); f.complete(); await f.poll();
    assert.equal(f.counts.library, 2); assert.equal(f.counts.saves, 0); assert.deepEqual(f.selections, []);
    scan.resolve({}); await flush(); assert.equal(f.counts.saves, 1); assert.deepEqual(f.selections, ['m-route-target']);
  } finally { scan.resolve({}); f.destroy(); }
});

for (const mode of ['manual', 'dispose']) test(`${mode} while a recovered success awaits route refresh prevents late registration and selection`, async () => {
  const scan = deferred();
  const f = routeRefreshHooks({ libraryRead: ({ index }) => index === 2 ? scan.promise : Promise.resolve({}) });
  try {
    await flush(); f.complete(); await f.poll(); assert.equal(f.counts.library, 2);
    if (mode === 'manual') f.manual(); else f.destroy();
    scan.resolve({}); await flush();
    assert.equal(f.counts.saves, 0); assert.deepEqual(f.rows, []); assert.deepEqual(f.selections, []); assert.deepEqual(f.notices, []);
    if (mode === 'manual') { assert.equal(f.binding(), 'manual'); assert.equal(f.pending().length, 0); }
  } finally { scan.resolve({}); f.destroy(); }
});

for (const indexed of [false, true]) test(`${indexed ? 'indexed' : 'private'} success waits for a pre-success flight then performs a fresh store scan`, async () => {
  const oldScan = deferred();
  const f = routeRefreshHooks({ indexIncludesTask: indexed, libraryRead: ({ index }) => index === 1 ? oldScan.promise : Promise.resolve({}) });
  try {
    await flush(); assert.equal(f.counts.library, 1); f.complete(); await f.poll(indexed);
    assert.equal(f.counts.library, 1); assert.equal(f.counts.saves, 0);
    oldScan.resolve({}); await flush();
    assert.equal(f.counts.library, 2); assert.equal(f.counts.saves, 1); assert.deepEqual(f.selections, ['m-route-target']);
    assert.deepEqual(f.delayed, []); assert.deepEqual(f.notices, ['selected']);
    assert.ok(f.events.indexOf('library:running:end') < f.events.indexOf('library:succeeded:start'));
  } finally { oldScan.resolve({}); f.destroy(); }
});

test('recovery uses the current store capability after the hook initialized with it unavailable', async () => {
  const f = routeRefreshHooks({ initiallyAvailable: false });
  try {
    await flush(); assert.equal(f.counts.library, 0); assert.equal(f.counts.followers, 1);
    f.enableStore(); await flush(); assert.equal(f.counts.library, 1); assert.equal(f.counts.followers, 1);
    f.complete(); await f.poll();
    assert.equal(f.counts.library, 2); assert.equal(f.counts.saves, 1); assert.deepEqual(f.selections, ['m-route-target']);
    assert.deepEqual(f.delayed, []); assert.equal(f.counts.followers, 1);
  } finally { f.destroy(); }
});
