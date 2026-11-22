import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { createCmsdSwitchFollow } from '../src/features/megacube/switch-follow.js';
import * as cmsApi from '../src/features/megacube/cms-api.js';
import { createTaskEventBuffer } from '../src/features/megacube/task-event-buffer.js';
import { mergeHandoffRecovery } from '../src/features/megacube/handoff-recovery.js';
import { publishRuntimeTask, subscribeRuntimeTasks, runtimeTasksFromOutput } from '../src/features/megacube/runtime-task-events.js';

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
    .replace(/^import .*;\n/gm, '').replace(/export function /g, 'function ')
    // VM fixtures inject the real controller; exercise both production hooks.
    .replace("import('./switch-follow.js')", 'Promise.resolve({ createCmsdSwitchFollow: globalThis.createCmsdSwitchFollow })');
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
  Object.assign(context, { createTaskEventBuffer, publishRuntimeTask, subscribeRuntimeTasks, runtimeTasksFromOutput }); vm.createContext(context);
  vm.runInContext(hookSource('use-cmsd-event-buffer') + '\n' + hookSource('use-cmsd-tasks') + '\n' + hookSource('use-cmsd-switch-follow') + '\nthis.hook = props => useCmsdSwitchFollow({ ...props, eventBuffer: useCmsdEventBuffer(props) });', context);
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
  Object.assign(context, { createTaskEventBuffer, publishRuntimeTask, subscribeRuntimeTasks, runtimeTasksFromOutput }); vm.createContext(context); vm.runInContext(hookSource('use-cmsd-tasks') + '\nthis.hook = useCmsdTaskWatch;', context);
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
function routeRefreshHooks({ libraryRead = async () => ({}), indexIncludesTask = false, initiallyAvailable = true, taskKind = 'switch', initialStatus = 'running', taskRead = null, indexRead = null } = {}) {
  let clock = Date.now(), latestWatch;
  const toolEvents = new Map(), detailReads = [];
  const target = { ...task, kind: taskKind, card: 'target--v2', requested_card: 'target', selected_version: 'v2', deployment_revision: 'recipe2' };
  let indexVisible = indexIncludesTask;
  let status = initialStatus, routeReady = false, binding = 'source', storeAvailable = initiallyAvailable, follower, manualListener;
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
        return { linked_runtime_cards: ['target', 'target--v2'], linked_runtime_pending: [],
          models: [{ card_id: 'target', runtime_card_id: 'target--v2', active_version_id: 'v2', status: 'deployed',
            cmsd: { managed: true, status: 'running', healthy: true, deploymentRevision: 'recipe2' } }], ...result };
      }
      throw Error(`unexpected command ${command}`);
    },
    fetchCmsModels: async () => ({ available: true, data: { store_bindings: { target: 'target--v2' }, models: [{ card: 'target--v2', managed: true, model_id: 'Target', version_id: 'v2', deployment_revision: 'recipe2', status: 'running', healthy: status === 'succeeded' }] } }),
    fetchCmsStatus: async () => ({ available: true, data: { active_model: 'target--v2', gateway: '127.0.0.1:8000' } }),
    fetchCmsTask: async id => { detailReads.push(id); return taskRead ? taskRead(id, { ...target, status }) : { available: true, data: { ...target, id, status } }; },
    fetchCmsTasks: async () => { const result = { available: true, data: { tasks: indexVisible ? [{ ...target, status }] : Array.from({ length: 20 }, (_, i) => ({ id: `t-later-${i}`, kind: 'set_default', card: target.card, status: 'succeeded' })) } }; return indexRead ? indexRead(result) : result; },
    probeCmsAvailable: async () => true, tauriEvents: { listen: async (name, callback) => { toolEvents.set(name, callback); return () => toolEvents.delete(name); } },
    listenManualModelSelection: callback => { manualListener = callback; return () => {}; },
    localStorage: { get length() { return storage.size; }, key: index => [...storage.keys()][index], getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key) },
    setInterval: (callback, delay) => { const timer = { callback, delay }; intervals.push(timer); return timer; }, clearInterval: () => {},
    Date: class extends Date { static now() { return clock; } },
    setTimeout: () => {}, clearTimeout: () => {}, console: { warn: () => {} },
  };
  Object.assign(context, { createTaskEventBuffer, publishRuntimeTask, subscribeRuntimeTasks, runtimeTasksFromOutput }); vm.createContext(context); vm.runInContext(hookSource('use-cmsd-event-buffer') + '\n' + hookSource('use-cmsd-tasks') + '\n' + hookSource('use-cmsd-switch-follow') + '\nthis.hook = props => useCmsdSwitchFollow({ ...props, eventBuffer: useCmsdEventBuffer(props) });', context);
  const render = () => {
    refIndex = stateIndex = effectIndex = callbackIndex = 0;
    const result = context.hook({ bridge, snapshot: state, modelStoreAvailable: storeAvailable, copy: { linkAutoSelected: () => 'selected', switchAutoSelected: () => 'selected', linkAutoSelectFailed: 'failed' }, onNotice: notice => notices.push(notice) });
    latestWatch = result;
    for (const effect of pendingEffects.splice(0)) effect();
    return result;
  };
  const destroy = () => { for (const effect of effects) effect?.cleanup?.(); follower?.dispose(); };
  render();
  return { counts, rows, selections, notices, events, delayed, states, target, detailReads,
    accepted: value => latestWatch.observeAcceptedRuntimeTask(value),
    tool: (kind, payload) => toolEvents.get(`chat:tool_${kind}`)?.({ payload }),
    complete: () => { status = 'succeeded'; },
    showTask: () => { indexVisible = true; },
    poll: async (index = false, advance = 30000) => { clock += advance; intervals.filter(timer => timer.delay === 3000)[index ? 1 : 0].callback(); await flush(); },
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

test('ordinary device/MCP enable success gets one fresh library and no modal or selection', async () => {
  const f = routeRefreshHooks({ indexIncludesTask: true, taskKind: 'enable' });
  try {
    await flush(); assert.equal(f.counts.library, 1);
    f.complete(); await f.poll(true); await flush();
    assert.equal(f.counts.library, 2, 'nonmodal enable must start a read after success');
    await f.poll(true); await f.poll(true);
    assert.equal(f.counts.library, 2, 'same completed enable is idempotent');
    assert.equal(f.selections.length, 0); assert.equal(f.rows.length, 0); assert.equal(f.notices.length, 0);
    assert.ok(!f.states.some(value => value?.kind === 'enable'), 'enable never creates maintenance overlay');
  } finally { f.destroy(); }
});

test('first successful enable index establishes one fresh baseline without replaying history effects', async () => {
  const scan = deferred();
  const f = routeRefreshHooks({ indexIncludesTask: true, taskKind: 'enable', initialStatus: 'succeeded', libraryRead: ({ index }) => index === 1 ? scan.promise : {} });
  try {
    await flush(); assert.equal(f.counts.library, 1, 'old startup scan is still in flight');
    scan.resolve({}); await flush(); await flush();
    assert.equal(f.counts.library, 2, 'first success requires a read begun after the old scan');
    await f.poll(true); await f.poll(true);
    assert.equal(f.counts.library, 2, 'history baseline is established once');
    assert.equal(f.selections.length, 0); assert.equal(f.notices.length, 0);
    assert.ok(!f.states.some(value => value?.kind === 'enable'));
  } finally { scan.resolve({}); f.destroy(); }
});

test('failed fresh read retries ordinary enable without replaying completion effects', async () => {
  const f = routeRefreshHooks({ indexIncludesTask: true, taskKind: 'enable', libraryRead: async ({ index }) => index === 2 ? { link_sync_error: 'unknown' } : {} });
  try {
    await flush(); f.complete(); await f.poll(true); await flush(); assert.equal(f.counts.library, 2);
    await f.poll(true); await flush(); assert.equal(f.counts.library, 3, 'failure remains pending');
    await f.poll(true); assert.equal(f.counts.library, 3);
    assert.equal(f.selections.length, 0); assert.equal(f.notices.length, 0);
  } finally { f.destroy(); }
});

test('failed/cancelled/unknown enable tasks do not refresh', async () => {
  for (const initialStatus of ['failed', 'cancelled', 'unknown']) {
    const f = routeRefreshHooks({ indexIncludesTask: true, taskKind: 'enable', initialStatus });
    try { await flush(); await f.poll(true); await f.poll(true); assert.equal(f.counts.library, 1); assert.equal(f.selections.length, 0); }
    finally { f.destroy(); }
  }
});

test('an enable first seen already succeeded after monitoring starts is refreshed once', async () => {
  const f = routeRefreshHooks({ taskKind: 'enable', initialStatus: 'succeeded' });
  try {
    await flush(); assert.equal(f.counts.library, 1);
    f.showTask(); await f.poll(true); await flush();
    assert.equal(f.counts.library, 2);
    await f.poll(true); assert.equal(f.counts.library, 2);
    assert.equal(f.selections.length, 0); assert.equal(f.notices.length, 0);
  } finally { f.destroy(); }
});

test('unknown target facts stay pending until a later positive read, without replaying enable UI', async () => {
  const f = routeRefreshHooks({ indexIncludesTask: true, taskKind: 'enable', libraryRead: ({ index }) => index === 2 ? { linked_runtime_pending: ['target', 'target--v2'], models: [{ card_id: 'target', runtime_card_id: 'target--v2', status: 'deployed', running: false }] } : {} });
  try {
    await flush(); f.complete(); await f.poll(true); assert.equal(f.counts.library, 2);
    await f.poll(true); assert.equal(f.counts.library, 3, 'unknown was not registered as refreshed');
    await f.poll(true); assert.equal(f.counts.library, 3);
    assert.equal(f.selections.length, 0); assert.equal(f.notices.length, 0);
  } finally { f.destroy(); }
});

test('accepted MCP enable ID absent from the latest-20 index is queried exactly then refreshes', async () => {
  const f = routeRefreshHooks({ taskKind: 'enable' });
  try {
    await flush();
    f.tool('start', { id: 'call-enable', session_id: 'origin', name: 'mcp__megacube_admin__cube_start_model', args: { card: 'target' } });
    f.tool('end', { id: 'call-enable', session_id: 'origin', success: true,
      output: JSON.stringify({ content: [{ type: 'text', text: JSON.stringify({ data: { task_id: f.target.id, kind: 'enable', card: 'target--v2', requested_card: 'target', logical_card: 'target' } }) }] }) });
    f.complete(); await f.poll(true);
    assert.deepEqual(f.detailReads, [f.target.id]); assert.equal(f.counts.library, 2);
    await f.poll(true); assert.equal(f.counts.library, 2);
    assert.equal(f.selections.length, 0); assert.equal(f.notices.length, 0);
  } finally { f.destroy(); }
});

test('accepted device enable and successful MCP get_task can seed the same nonmodal observer', async () => {
  const f = routeRefreshHooks({ taskKind: 'enable' });
  const previous = globalThis.__TAURI__;
  try {
    await flush();
    globalThis.__TAURI__ = { core: { invoke: async command => {
      assert.equal(command, 'cms_model_action'); return { task_id: f.target.id, card: 'target--v2', requested_card: 'target', kind: 'enable' };
    } } };
    await cmsApi.actOnCmsModel('target', 'enable');
    f.complete(); await f.poll(true); assert.deepEqual(f.detailReads, [f.target.id]); assert.equal(f.counts.library, 2);
    f.tool('start', { id: 'call-detail', session_id: 'origin', name: 'cube_get_task', args: { task_id: 'other-known' } });
    f.tool('end', { id: 'call-detail', session_id: 'origin', success: true, output: { data: { ...f.target, id: 'other-known', status: 'succeeded' } } });
    await f.poll(true); assert.equal(f.counts.library, 3); assert.equal(f.selections.length, 0);
  } finally { globalThis.__TAURI__ = previous; f.destroy(); }
});

test('known enable ID retains unknown lookup outside the index and retries without a fabricated success', async () => {
  let queries = 0;
  const f = routeRefreshHooks({ taskKind: 'enable', taskRead: async (id, target) => ++queries === 1 ? { available: false } : { available: true, data: { ...target, id, status: 'succeeded' } } });
  try {
    await flush(); f.accepted({ ...f.target, status: 'unknown' });
    await f.poll(true); assert.equal(f.counts.library, 1);
    await f.poll(true); assert.equal(f.counts.library, 2); assert.equal(queries, 2);
    await f.poll(true); assert.equal(queries, 2);
  } finally { f.destroy(); }
});

test('known enable detail polling is serial, bounded to eight per pass and rotates fifty pending IDs', async () => {
  let active = 0, maximum = 0;
  const f = routeRefreshHooks({ taskKind: 'enable', taskRead: async (id, target) => { active++; maximum = Math.max(maximum, active); await Promise.resolve(); active--; return { available: true, data: { ...target, id, status: 'running' } }; } });
  try {
    await flush(); for (let i = 0; i < 51; i++) f.accepted({ ...f.target, id: `known-${i}`, status: 'unknown' });
    await f.poll(true); assert.equal(f.detailReads.length, 8); assert.equal(maximum, 1); assert.equal(f.counts.library, 1);
    for (let i = 0; i < 6; i++) await f.poll(true);
    assert.equal(new Set(f.detailReads).size, 50); assert.ok(!f.detailReads.includes('known-50'));
    assert.equal(f.counts.library, 1);
  } finally { f.destroy(); }
});

test('runtime classification isolates unrelated stopped rows and recognizes superseded current selection', () => {
  const source = hookSource('use-cmsd-switch-follow');
  const context = {}; vm.createContext(context); vm.runInContext(source + '\nthis.classify = runtimeTaskState;', context);
  const task = { card: 'target--v2', logical_card: 'target', requested_card: 'target', selected_version: 'v2', deployment_revision: 'rev2' };
  const current = { linked_runtime_cards: ['target', 'target--v2', 'stopped-other'], linked_runtime_pending: ['stopped-other'],
    models: [{ card_id: 'target', runtime_card_id: 'target--v2', active_version_id: 'v2', status: 'deployed' }] };
  assert.equal(context.classify(task, current), 'confirmed');
  const unknown = { ...current, linked_runtime_pending: ['target', 'target--v2'] };
  assert.equal(context.classify(task, unknown), 'pending');
  unknown.models[0].active_version_id = 'v3'; unknown.models[0].runtime_card_id = 'target--v3';
  assert.equal(context.classify(task, unknown), 'superseded');
  assert.equal(context.classify({ card: 'unlinked' }, unknown), 'irrelevant');
});


test('fact confirmation retries at 3/6/12/24/30 second intervals and never expires unknown as success', async () => {
  const f = routeRefreshHooks({ indexIncludesTask: true, taskKind: 'enable', libraryRead: () => ({ linked_runtime_pending: ['target'] }) });
  try {
    await flush(); f.complete(); await f.poll(true, 0); assert.equal(f.counts.library, 2);
    for (const delay of [3000, 6000, 12000, 24000, 30000, 30000]) {
      const previous = f.counts.library;
      await f.poll(true, delay - 1); assert.equal(f.counts.library, previous);
      await f.poll(true, 1); assert.equal(f.counts.library, previous + 1);
    }
    for (let i = 0; i < 20; i++) await f.poll(true, 30000);
    assert.ok(f.counts.library > 20, 'no exhausted retry is mislabeled confirmed');
    assert.equal(f.notices.length, 0); assert.equal(f.selections.length, 0);
  } finally { f.destroy(); }
});

test('first-poll historical enable superseded by an explicit different current version establishes only one baseline', async () => {
  const f = routeRefreshHooks({ indexIncludesTask: true, taskKind: 'enable', initialStatus: 'succeeded', libraryRead: () => ({
    linked_runtime_pending: ['target'], models: [{ card_id: 'target', runtime_card_id: 'target--v3', active_version_id: 'v3', status: 'deployed', running: false }],
  }) });
  try {
    await flush(); assert.equal(f.counts.library, 2);
    for (let i = 0; i < 5; i++) await f.poll(true);
    assert.equal(f.counts.library, 2); assert.equal(f.notices.length, 0); assert.equal(f.selections.length, 0);
  } finally { f.destroy(); }
});


test('known accepted enable can complete when the public task index query throws', async () => {
  const f = routeRefreshHooks({ taskKind: 'enable', indexRead: () => { throw new Error('index unavailable'); } });
  try {
    await flush(); f.accepted({ ...f.target, status: 'unknown' }); f.complete();
    await f.poll(true); assert.deepEqual(f.detailReads, [f.target.id]); assert.equal(f.counts.library, 2);
    await f.poll(true); assert.equal(f.counts.library, 2); assert.equal(f.selections.length, 0);
  } finally { f.destroy(); }
});

test('runtime task receipts reject errors, unknown lookup records and oversized or ambiguous identities', () => {
  for (const data of [
    { task_id: 'known', card: 'target', kind: 'enable', observation: 'unconfirmed' },
    { task_id: 'known', card: 'target', kind: 'enable', error: { code: 'unknown' } },
    { id: 'known', status: 'unknown' },
    { task_id: '../other', card: 'target', kind: 'enable' },
    { task_id: 'known', card: 'target', kind: 'switch' },
  ]) assert.deepEqual(runtimeTasksFromOutput({ data }), []);
  assert.deepEqual(runtimeTasksFromOutput(' '.repeat(262145)), []);
  assert.deepEqual(runtimeTasksFromOutput({ isError: true, data: { task_id: 'known', card: 'target', kind: 'enable' } }), []);
});

test('a watched task is observed once per (id, status) transition, not on every poll', async () => {
  const effects = [], cleanups = [], intervals = [];
  const observed = [];
  const switchTask = { id: 'sw-1', kind: 'switch', card: 'target', status: 'succeeded' };
  let stateIndex = 0;
  const previousTauri = globalThis.__TAURI__;
  globalThis.__TAURI__ = { core: { invoke: async () => ({}) } };
  const context = {
    useRef: value => ({ current: value }), useCallback: callback => callback, useEffect: callback => effects.push(callback),
    useState: initial => {
      const index = stateIndex++;
      const slots = context.__slots || (context.__slots = []);
      slots[index] = index === 0 ? true : initial; // cmsAvailable 直达 true
      return [slots[index], value => { slots[index] = typeof value === 'function' ? value(slots[index]) : value; }];
    },
    fetchCmsStatus: async () => ({ available: true, data: { active_model: 'target' } }),
    fetchCmsTasks: async () => ({ available: true, data: { tasks: [switchTask] } }),
    fetchCmsTask: async () => ({ available: true, data: switchTask }),
    probeCmsAvailable: async () => true,
    setInterval: (callback, delay) => { intervals.push({ callback, delay }); return intervals.length; }, clearInterval: () => {},
    setTimeout, clearTimeout, console,
  };
  vm.createContext(context);
  vm.runInContext(hookSource('use-cmsd-tasks') + '\nthis.hook = useCmsdTaskWatch;', context);
  context.hook({ onTaskObserved: task => observed.push(task.status) });
  for (const effect of effects) { const cleanup = effect(); if (cleanup) cleanups.push(cleanup); }
  try {
    const tick = intervals.find(value => value.delay === 3000).callback;
    await flush(); // 初始 tick
    // r14 R3：同一 (id, succeeded) 在公开索引里长期可见，但观察回调只允许
    // 触发一次——否则消费方的 reconcileRoutes 会每 3s 重跑。
    for (let poll = 0; poll < 3; poll++) { await tick(); await flush(); }
    assert.deepEqual(observed, ['succeeded']);
    // running→succeeded 的真实迁移仍要再次触发。
    switchTask.status = 'running'; await tick(); await flush();
    switchTask.status = 'succeeded'; await tick(); await flush();
    assert.deepEqual(observed, ['succeeded', 'running', 'succeeded']);
  } finally { for (const cleanup of cleanups) cleanup(); globalThis.__TAURI__ = previousTauri; }
});
