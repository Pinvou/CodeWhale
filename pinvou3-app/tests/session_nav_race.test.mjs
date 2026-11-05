/**
 * 会话管理导航竞态回归测试（PR #257 三审）：ensureSession / archiveSession
 * 的「null → null」导航窗口——用户再进草稿（enterDraft）时 activeSessionId
 * 保持 null 但 sessionSwitchRequestToken 已前移，仅判 activeSessionId 的
 * 守卫拦不住在途异步操作劫持新草稿。修复：请求开始时捕获导航 token，
 * 写入/回滚前连同 activeSessionId 一起校验。
 *
 * 覆盖 tauri 桥（platform/tauri/bridge/sessions.js，factory 注入依赖）；
 * web 桥镜像守卫由 session_nav_race_web.test.mjs 锁定。此前 sessions.js
 * 因「依赖太重」只靠代码审查（见 memory_personas_race.test.mjs 头注释），
 * 三审后注入面已验证可单测，补上可执行回归。
 */
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';
import { createNativeSurfaceTransitionGate, settleBrowserUiPublicationAfterCommit } from '../src/features/browser/native-surface-transition.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const bridgeDir = path.join(here, '..', 'src', 'platform', 'tauri', 'bridge');

/** vm 装载 sessions.js factory，注入最小依赖面。 */
function loadSessionsFeature(overrides) {
  const root = {};
  const src = fs.readFileSync(path.join(bridgeDir, 'sessions.js'), 'utf8');
  vm.runInNewContext(fs.readFileSync(path.join(bridgeDir, '..', '..', '..', 'shared', 'bridge-shared-helpers.js'), 'utf8') + '\n' + src, { window: root, globalThis: root, setTimeout, clearTimeout });
  const factory = root.__PINVOU_TAURI_BRIDGE_FEATURES__.sessions;
  const state = {
    activeSessionId: null,
    messages: [], chatItems: [], artifacts: [], queued: [],
    sessions: [], archivedSessions: [],
    scheduledTaskRecentRuns: [], scheduledTaskRuns: [],
    modeState: { mode: 'yolo', multiAgent: false },
    modeDefaults: { work: 'yolo', code: 'yolo' },
    modeLane: 'work',
    draftEpoch: 0,
    composerDraft: '',
    pendingDraftMultiAgent: false,
    scheduledRunContext: null,
    scheduledTaskPendingGuide: null,
    mountedCollections: [],
    mountedCollectionsRevision: 0,
    busy: false,
    thinking: false,
    tokens: { input: 0, max: 0 },
    turnTimeline: [],
    activeTurnTimelineId: null,
    personaEvents: [],
    pinvouReviews: [],
    pinvouSceneEvents: [],
  };
  const sessionStates = {};
  const deferreds = {};
  const calls = { invoke: [] };
  const api = factory(Object.assign({
    state,
    sessionStates,
    notify() { calls.notify = (calls.notify || 0) + 1; },
    listen: null,
    bt(key) { return key; },
    addSystemItem(text) { state.chatItems.push({ type: 'system', text, id: 'sys-' + (state.chatItems.length + 1) }); },
    addChatItem(item) { item.id = item.id || ('item-' + (state.chatItems.length + 1)); state.chatItems.push(item); },
    timeStr() { return ''; },
    invoke(name, args) {
      calls.invoke.push(name);
      if (deferreds[name] && deferreds[name].promise) return deferreds[name].promise;
      return Promise.resolve({});
    },
    runSyncOnSession(sid, fn) { fn(); },
    persistMessagesFor() {},
    resetPendingAssistant() {},
    stopThinking() {},
    rerenderFromMessages() {},
    syncModeState() { return Promise.resolve(); },
    applyAuthoritativeModeState() {},
    currentDraftModeState() { return { mode: 'yolo', multiAgent: false }; },
    syncActivePersona() { return Promise.resolve(); },
    syncMountedCollection() { return Promise.resolve(); },
    reconcileArtifacts() {},
    loadSessionModel() { return Promise.resolve(); },
    clearScheduledTaskSelection() {},
    invalidateScheduledRecentRunsForSession() {},
    refreshHistoryListForRun() { return Promise.resolve(); },
    turnUsageDirty: false,
    basename(p) { return String(p || '').split('/').pop(); },
    isAbsPath() { return false; },
    filterSessionArtifacts(list) { return list; },
    scheduleShellPoll() {},
    setScheduledTaskError() {},
    userMessageDisplayText(t) { return t; },
    loadMemoryOverview() { return Promise.resolve(); },
    isScheduledRunSession(id) { return String(id || '').indexOf('sched-') === 0; },
    invalidateScheduledTaskReads() {},
    applyScheduledRunViewed() {},
    loadScheduledTaskRecentRuns() { return Promise.resolve(); },
    scheduledRunSessionOwners: {},
    personaPlaceholderTitles: {},
  }, overrides || {}));
  return {
    api, state, sessionStates, calls,
    defer(name) {
      // 每次创建全新对象：同一 invoke 名的第二次 defer 不得覆盖第一次的 resolve。
      const d = {};
      d.promise = new Promise((resolve, reject) => { d.resolve = resolve; d.reject = reject; });
      deferreds[name] = d;
      return d;
    },
  };
}

// ── ensureSession：再进草稿不劫持 ──────────────────────────

test('scheduled and ordinary UI transitions share the real gate and respect React commit', async () => {
  for (const last of ['scheduled', 'ordinary']) {
    const pending = new Map();
    const calls = [];
    const rt = loadSessionsFeature({ invoke(name, args) {
      if (name !== 'load_session') return Promise.resolve({});
      calls.push(args.id);
      return new Promise(resolve => pending.set(args.id, resolve));
    } });
    rt.state.activeSessionId = 'local-a';
    let releaseHide;
    const hide = new Promise(resolve => { releaseHide = resolve; });
    let releaseCommit;
    const commit = new Promise(resolve => { releaseCommit = resolve; });
    let commitRequested = false;
    let released = 0;
    const gate = createNativeSurfaceTransitionGate({
      getContext: () => ({ sessionId: 'local-a', hasWorkspace: true, visible: true }),
      acquireHide: () => hide,
    });
    const uiTransition = (kind) => {
      rt.api.beginLocalNavigationIntent();
      return gate.run(({ isCurrent }) => settleBrowserUiPublicationAfterCommit({
        publish: async () => {
          const opened = kind === 'scheduled'
            ? await rt.api.openScheduledRunChat({ sessionId: 'sched-a', status: 'completed' }, {})
            : await rt.api.switchToSession('local-b');
          if (!opened || !isCurrent()) return false;
          return true;
        },
        waitForCommit: () => { commitRequested = true; return commit; },
      }), { channel: 'session', hideMode: 'workspace', serialize: true });
    };
    const first = uiTransition(last === 'scheduled' ? 'ordinary' : 'scheduled');
    await Promise.resolve();
    const second = uiTransition(last);
    releaseHide({ release() { released += 1; } });
    assert.equal(await first, false);
    for (let i = 0; i < 30 && calls.length === 0; i += 1) await Promise.resolve();
    const wanted = last === 'scheduled' ? 'sched-a' : 'local-b';
    assert.deepEqual(calls, [wanted]);
    pending.get(wanted)({ metadata: { id: wanted }, messages: [] });
    for (let i = 0; i < 50 && !commitRequested; i += 1) await Promise.resolve();
    assert.equal(commitRequested, true);
    assert.equal(released, 1, 'latest lease remains held until React commits');
    releaseCommit();
    assert.equal(await second, true);
    assert.equal(released, 2);
    assert.equal(rt.state.activeSessionId, wanted);
  }
});

test('a queued ordinary choice retires a scheduled load already inside the real gate', async () => {
  const rt = loadSessionsFeature();
  rt.state.activeSessionId = 'local-a';
  const scheduledLoad = rt.defer('load_session');
  const gate = createNativeSurfaceTransitionGate({
    getContext: () => ({ sessionId: 'local-a', hasWorkspace: false, visible: false }),
    acquireHide: async () => ({ release() {} }),
  });
  const transition = (publish) => {
    rt.api.beginLocalNavigationIntent();
    return gate.run(({ isCurrent }) => settleBrowserUiPublicationAfterCommit({
      publish: async () => (await publish()) && isCurrent(),
      waitForCommit: () => Promise.resolve(),
    }), { channel: 'session', hideMode: 'workspace', serialize: true });
  };
  const first = transition(() => rt.api.openScheduledRunChat({ sessionId: 'sched-a', status: 'completed' }, {}));
  for (let i = 0; i < 20 && !rt.calls.invoke.includes('load_session'); i += 1) await Promise.resolve();
  assert.ok(rt.calls.invoke.includes('load_session'));
  const ordinaryLoad = rt.defer('load_session');
  const second = transition(() => rt.api.switchToSession('local-b'));
  scheduledLoad.resolve({ metadata: { id: 'sched-a' }, messages: [] });
  assert.equal(await first, false);
  for (let i = 0; i < 30 && rt.calls.invoke.filter(name => name === 'load_session').length < 2; i += 1) await Promise.resolve();
  ordinaryLoad.resolve({ metadata: { id: 'local-b' }, messages: [] });
  assert.equal(await second, true);
  assert.equal(rt.state.activeSessionId, 'local-b');
});

test('superseding an in-progress scheduled preflight prevents the older open', async () => {
  const rt = loadSessionsFeature();
  rt.state.activeSessionId = 'local-a';
  let finishPreflight;
  const preflight = new Promise(resolve => { finishPreflight = resolve; });
  let enteredPreflight = false;
  const gate = createNativeSurfaceTransitionGate({
    getContext: () => ({ sessionId: 'local-a', hasWorkspace: false, visible: false }),
    acquireHide: async () => ({ release() {} }),
  });
  const transition = (fn) => {
    rt.api.beginLocalNavigationIntent();
    return gate.run(fn, { channel: 'session', hideMode: 'workspace', serialize: true });
  };
  const first = transition(async ({ isCurrent }) => {
    enteredPreflight = true;
    if (!(await preflight) || !isCurrent()) return false;
    return rt.api.openScheduledRunChat({ sessionId: 'sched-a', status: 'completed' }, {});
  });
  for (let i = 0; i < 20 && !enteredPreflight; i += 1) await Promise.resolve();
  assert.equal(enteredPreflight, true);
  const ordinaryLoad = rt.defer('load_session');
  const second = transition(() => rt.api.switchToSession('local-b'));
  finishPreflight(true);
  assert.equal(await first, false);
  for (let i = 0; i < 20 && !rt.calls.invoke.includes('load_session'); i += 1) await Promise.resolve();
  ordinaryLoad.resolve({ metadata: { id: 'local-b' }, messages: [] });
  assert.equal(await second, true);
  assert.equal(rt.calls.invoke.filter(name => name === 'load_session').length, 1);
  assert.equal(rt.state.activeSessionId, 'local-b');
});

test('reentrant later switch wins before the first switch starts loading', async () => {
  const rt = loadSessionsFeature();
  const load = rt.defer('load_session');
  let later;
  let started = false;
  const unsubscribe = rt.api.onLocalNavigation(() => {
    if (!started) { started = true; later = rt.api.switchToSession('newer'); }
  });
  const first = rt.api.switchToSession('older');
  unsubscribe();
  assert.equal(await first, false);
  load.resolve({ metadata: { id: 'newer' }, messages: [] });
  assert.equal(await later, true);
  assert.equal(rt.state.activeSessionId, 'newer');
  assert.equal(rt.calls.invoke.filter(name => name === 'load_session').length, 1);
});

test('navigation listener cleanup and reentrant dispatch preserve the latest epoch', () => {
  const rt = loadSessionsFeature();
  let first = 0;
  let second = 0;
  const unsubscribe = rt.api.onLocalNavigation(() => {
    first += 1;
    if (first === 1) rt.api.beginLocalNavigationIntent();
  });
  rt.api.onLocalNavigation(() => { second += 1; });
  rt.api.beginLocalNavigationIntent();
  assert.equal(first, 2);
  assert.equal(second, 2);
  unsubscribe();
  rt.api.beginLocalNavigationIntent();
  assert.equal(first, 2);
  assert.equal(second, 3);
  assert.equal(rt.state.controlledLocalNavigationEpoch, 3);
});

test('later coalesced scheduled intent defeats a controlled load in both completion orders', async () => {
  for (const order of ['controlled-first', 'scheduled-first']) {
    for (const rejectControlled of [false, true]) {
      const pending = new Map();
      const loaded = [];
      const rt = loadSessionsFeature({ invoke(name, args) {
        if (name !== 'load_session') return Promise.resolve({});
        loaded.push(args.id);
        return new Promise((resolve, reject) => pending.set(args.id, { resolve, reject }));
      } });
      rt.state.activeSessionId = 'local-a';
      rt.state.desktopRemoteControl = { controlledDevice: { status: 'connected', generation: 'g1' },
        controlledSession: { sessionId: 'remote-b', generation: 'g1' }, controlledTasks: [], intents: { controlled: 1 } };
      const run = { sessionId: 'sched-a', runId: 'run-a', status: 'completed' };
      const first = rt.api.openScheduledRunChat(run, {});
      const owner = rt.api.captureControlledNavigationOwner('g1');
      const controlled = rt.api.switchToControlledSession('remote-b', owner);
      const second = rt.api.openScheduledRunChat(run, {});
      assert.equal(first, second);
      assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false);
      const settleControlled = async () => {
        if (rejectControlled) pending.get('remote-b').reject(new Error('late failure'));
        else pending.get('remote-b').resolve({ metadata: { id: 'remote-b' }, messages: [] });
        assert.equal(await controlled, false);
      };
      if (order === 'controlled-first') await settleControlled();
      pending.get('sched-a').resolve({ metadata: { id: 'sched-a' }, messages: [] });
      assert.equal(await first, true);
      assert.equal(await second, true);
      if (order === 'scheduled-first') await settleControlled();
      assert.deepEqual(loaded, ['sched-a', 'remote-b']);
      assert.equal(rt.state.activeSessionId, 'sched-a');
      assert.equal(rt.state.scheduledRunContext.sessionId, 'sched-a');
    }
  }
});

test('new scheduled selection replaces a retired request without reviving it', async () => {
  for (const navigation of ['queued', 'draft', 'switch']) {
   for (const status of ['completed', 'running']) {
    for (const order of ['old-first', 'new-first']) {
    const rt = loadSessionsFeature();
    rt.state.activeSessionId = 'local-a';
    const oldLoad = rt.defer('load_session');
    const run = { sessionId: 'sched-a', status };
    const first = rt.api.openScheduledRunChat(run, {});
    let intermediate;
    let intermediateLoad;
    if (navigation === 'queued') rt.api.beginLocalNavigationIntent();
    else if (navigation === 'draft') await rt.api.createNewSession();
    else {
      intermediateLoad = rt.defer('load_session');
      intermediate = rt.api.switchToSession('other-local');
    }
    const newLoad = rt.defer('load_session');
    const second = rt.api.openScheduledRunChat(run, {});
    assert.notEqual(first, second);
    if (order === 'old-first') {
      oldLoad.resolve({ metadata: { id: 'sched-a' }, messages: [] });
      assert.equal(await first, false);
      if (status === 'running') assert.equal(rt.sessionStates['sched-a'].scheduledInitialTurnPhase, 'active');
      assert.equal(rt.api.openScheduledRunChat(run, {}), second, 'stale cleanup preserves the new in-flight owner');
    }
    newLoad.resolve({ metadata: { id: 'sched-a' }, messages: [] });
    assert.equal(await second, true);
    if (order === 'new-first') {
      oldLoad.resolve({ metadata: { id: 'sched-a' }, messages: [] });
      assert.equal(await first, false);
    }
    if (intermediate) {
      intermediateLoad.resolve({ metadata: { id: 'other-local' }, messages: [] });
      assert.equal(await intermediate, false);
    }
    assert.equal(rt.state.activeSessionId, 'sched-a');
    assert.equal(rt.state.scheduledRunContext.sessionId, 'sched-a');
    if (status === 'running') assert.equal(rt.sessionStates['sched-a'].scheduledInitialTurnPhase, 'active');
    assert.equal(rt.calls.invoke.filter(name => name === 'load_session').length, navigation === 'switch' ? 3 : 2);
    }
   }
  }
});

test('scheduled coalescing tolerates synchronous navigation and state observers', async () => {
  let rt;
  let nested;
  let stateCalls = 0;
  const run = { sessionId: 'sched-a', status: 'completed' };
  rt = loadSessionsFeature({ notify() {
    stateCalls += 1;
    if (stateCalls === 1) nested = rt.api.openScheduledRunChat(run, {});
  } });
  const load = rt.defer('load_session');
  let navigationCalls = 0;
  rt.api.onLocalNavigation(() => {
    navigationCalls += 1;
    if (navigationCalls === 1) assert.ok(rt.api.openScheduledRunChat(run, {}));
  });
  const first = rt.api.openScheduledRunChat(run, {});
  assert.equal(first, nested);
  assert.equal(rt.calls.invoke.filter(name => name === 'load_session').length, 1);
  load.resolve({ metadata: { id: 'sched-a' }, messages: [] });
  assert.equal(await first, true);
  assert.equal(navigationCalls, 2);
});

test('a later scheduled intent starts a new open after the first commits but its receipt is pending', async () => {
  const rt = loadSessionsFeature();
  rt.state.activeSessionId = 'local-a';
  const receipt = rt.defer('mark_scheduled_run_viewed');
  const run = { sessionId: 'sched-a', automationId: 'automation-a', runId: 'run-a', status: 'completed' };
  rt.defer('load_session').resolve({ metadata: { id: 'sched-a' }, messages: [] });
  const first = rt.api.openScheduledRunChat(run, {});
  for (let i = 0; i < 30 && !rt.calls.invoke.includes('mark_scheduled_run_viewed'); i += 1) await Promise.resolve();
  assert.equal(rt.state.activeSessionId, 'sched-a');
  assert.ok(rt.calls.invoke.includes('mark_scheduled_run_viewed'));
  await rt.api.createNewSession();
  const nextLoad = rt.defer('load_session');
  const second = rt.api.openScheduledRunChat(run, {});
  assert.notEqual(first, second);
  nextLoad.resolve({ metadata: { id: 'sched-a' }, messages: [] });
  receipt.resolve({});
  assert.equal(await first, true);
  assert.equal(await second, true);
  assert.equal(rt.state.activeSessionId, 'sched-a');
  assert.equal(rt.calls.invoke.filter(name => name === 'load_session').length, 2);
});

test('controlled session from an older generation cannot be opened by the new owner', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g2' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const owner = rt.api.captureControlledNavigationOwner('g2');
  assert.ok(owner);
  assert.equal(await rt.api.switchToControlledSession('session-b', owner), false);
  assert.ok(!rt.calls.invoke.includes('load_session'));
});

test('local navigation epoch getter reads only its scalar state', () => {
  const rt = loadSessionsFeature();
  Object.defineProperty(rt.state, 'sessions', { get() { throw new Error('sessions must not be cloned'); } });
  assert.equal(rt.api.getLocalNavigationEpoch(), 0);
  rt.api.beginLocalNavigationIntent();
  assert.equal(rt.api.getLocalNavigationEpoch(), 1);
});

test('controlled session load cannot commit after remote stop or refresh intent', async () => {
  for (const replacement of ['stop', 'refresh']) {
    const rt = loadSessionsFeature();
    rt.state.desktopRemoteControl = {
      controlledDevice: { status: 'connected', generation: 'g1' },
      controlledSession: { sessionId: 'session-b', generation: 'g1' },
      controlledTasks: [], intents: { controlled: 1 },
    };
    const load = rt.defer('load_session');
    const pending = rt.api.switchToControlledSession('session-b', rt.api.captureControlledNavigationOwner('g1'));
    rt.state.desktopRemoteControl = {
      ...rt.state.desktopRemoteControl,
      controlledDevice: replacement === 'stop'
        ? { status: 'idle', generation: null }
        : { status: 'connected', generation: 'g1' },
      intents: { controlled: 2 },
    };
    load.resolve({ metadata: { id: 'session-b' }, messages: [] });
    assert.equal(await pending, false);
    assert.equal(rt.state.activeSessionId, null);
  }
});

test('direct ordinary session switch invalidates the controlled owner before either load settles', async () => {
 for (const outcome of ['resolve', 'reject']) {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'local-a';
  const owner = rt.api.captureControlledNavigationOwner('g1');
  const oldLoad = rt.defer('load_session');
  const old = rt.api.switchToControlledSession('session-b', owner);
  assert.ok(rt.calls.invoke.includes('load_session'), 'old load must be in flight');
  const newLoad = rt.defer('load_session');
  let navigationNotifications = 0;
  rt.api.onLocalNavigation(() => { navigationNotifications += 1; });
  const local = rt.api.switchToSession('local-b');
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false, 'direct bridge intent must retire old owner synchronously');
  assert.ok(navigationNotifications > 0, 'bridge publishes the new epoch for UI subscribers');
  assert.notEqual(rt.state.controlledLocalNavigationEpoch, owner.bridgeNavigationEpoch);
  newLoad.resolve({ metadata: { id: 'local-b' }, messages: [], artifacts: [] });
  assert.equal(await local, true);
  if (outcome === 'reject') oldLoad.reject(new Error('late failure'));
  else oldLoad.resolve({ metadata: { id: 'session-b' }, messages: [], artifacts: [] });
  assert.equal(await old, false);
  assert.equal(rt.state.activeSessionId, 'local-b');
 }
});

test('draft-to-draft and ABA navigation never revive a captured controlled owner', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const owner = rt.api.captureControlledNavigationOwner('g1');
  await rt.api.createNewSession();
  const once = rt.state.controlledLocalNavigationEpoch;
  await rt.api.createNewSession();
  assert.ok(rt.state.controlledLocalNavigationEpoch > once);
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false);
  const fresh = rt.api.captureControlledNavigationOwner('g1');
  const b = rt.defer('load_session');
  const toB = rt.api.switchToSession('local-b');
  b.resolve({ metadata: { id: 'local-b' }, messages: [], artifacts: [] });
  assert.equal(await toB, true);
  await rt.api.createNewSession();
  assert.equal(rt.state.activeSessionId, null);
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(fresh), false, 'returning to draft must not revive the old owner');
});

test('first draft materialization retires a pending controlled load before committing the new local session', async () => {
  for (const first of ['old', 'created']) {
    for (const outcome of ['resolve', 'reject']) {
      const rt = loadSessionsFeature();
      rt.state.desktopRemoteControl = {
        controlledDevice: { status: 'connected', generation: 'g1' },
        controlledSession: { sessionId: 'session-b', generation: 'g1' },
        controlledTasks: [], intents: { controlled: 1 },
      };
      const owner = rt.api.captureControlledNavigationOwner('g1');
      const oldLoad = rt.defer('load_session');
      const old = rt.api.switchToControlledSession('session-b', owner);
      assert.ok(rt.calls.invoke.includes('load_session'), 'controlled load must be pending first');
      const creation = rt.defer('create_session');
      let notifications = 0;
      rt.api.onLocalNavigation(() => { notifications += 1; });
      const materializing = rt.api.ensureSession({ sessionId: null, draftEpoch: 0 });
      assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false, 'local materialization synchronously retires the owner');
      assert.equal(notifications, 1, 'owner retirement is visible to navigation subscribers');
      assert.ok(rt.calls.invoke.includes('create_session'), 'draft creation must be pending');
      const settleOld = async () => {
        if (outcome === 'reject') oldLoad.reject(new Error('late controlled failure'));
        else oldLoad.resolve({ metadata: { id: 'session-b' }, messages: [], artifacts: [] });
        assert.equal(await old, false);
      };
      if (first === 'old') await settleOld();
      creation.resolve({ id: 'local-new' });
      assert.equal(await materializing, 'local-new');
      if (first === 'created') await settleOld();
      assert.equal(rt.state.activeSessionId, 'local-new');
      assert.equal(rt.state.chatItems.length, 0, 'stale controlled failure must not append a local error');
    }
  }
});

test('ensureSession coalesces the same draft and does not invalidate a live session owner', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const creation = rt.defer('create_session');
  const first = rt.api.ensureSession();
  const afterFirst = rt.state.controlledLocalNavigationEpoch;
  const second = rt.api.ensureSession();
  assert.ok(rt.state.controlledLocalNavigationEpoch > afterFirst, 'a later draft intent must retire older controlled owners');
  assert.equal(rt.calls.invoke.filter(name => name === 'create_session').length, 1);
  creation.resolve({ id: 'local-new' });
  assert.equal(await first, 'local-new');
  assert.equal(await second, 'local-new');
  const owner = rt.api.captureControlledNavigationOwner('g1');
  assert.equal(await rt.api.ensureSession(), 'local-new');
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), true, 'reading an existing session is not a navigation');
});

test('later same-draft materialization wins over an intervening controlled load without duplicating create', async () => {
  for (const first of ['old', 'created']) {
    for (const outcome of ['resolve', 'reject']) {
      const rt = loadSessionsFeature();
      rt.state.desktopRemoteControl = {
        controlledDevice: { status: 'connected', generation: 'g1' },
        controlledSession: { sessionId: 'session-b', generation: 'g1' },
        controlledTasks: [], intents: { controlled: 1 },
      };
      const creation = rt.defer('create_session');
      const initial = rt.api.ensureSession({ sessionId: null, draftEpoch: 0 });
      const owner = rt.api.captureControlledNavigationOwner('g1');
      const oldLoad = rt.defer('load_session');
      const old = rt.api.switchToControlledSession('session-b', owner);
      assert.ok(rt.calls.invoke.includes('load_session'));
      const later = rt.api.ensureSession({ sessionId: null, draftEpoch: 0 });
      assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false);
      assert.equal(rt.calls.invoke.filter(name => name === 'create_session').length, 1);
      const settleOld = async () => {
        if (outcome === 'reject') oldLoad.reject(new Error('late controlled failure'));
        else oldLoad.resolve({ metadata: { id: 'session-b' }, messages: [], artifacts: [] });
        assert.equal(await old, false);
      };
      if (first === 'old') await settleOld();
      creation.resolve({ id: 'local-new' });
      assert.equal(await initial, 'local-new');
      assert.equal(await later, 'local-new');
      if (first === 'created') await settleOld();
      assert.equal(rt.state.activeSessionId, 'local-new');
    }
  }
});

test('later same-draft intent invalidates a captured controlled owner even before its load starts', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const creation = rt.defer('create_session');
  const first = rt.api.ensureSession();
  const owner = rt.api.captureControlledNavigationOwner('g1');
  const second = rt.api.ensureSession();
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false);
  assert.equal(await rt.api.switchToControlledSession('session-b', owner), false);
  creation.resolve({ id: 'local-new' });
  assert.equal(await first, 'local-new');
  assert.equal(await second, 'local-new');
});

test('queued ordinary navigation cannot revive an older coalesced draft creation', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const creation = rt.defer('create_session');
  const first = rt.api.ensureSession();
  rt.api.beginLocalNavigationIntent(); // user issued navigation; bridge switch has not begun
  const second = rt.api.ensureSession();
  assert.equal(rt.calls.invoke.filter(name => name === 'create_session').length, 1);
  creation.resolve({ id: 'old-local' });
  assert.equal(await first, null);
  assert.equal(await second, null);
  assert.equal(rt.state.activeSessionId, null);
});

test('new draft cannot revive an older coalesced draft creation', async () => {
  const rt = loadSessionsFeature();
  const creation = rt.defer('create_session');
  const first = rt.api.ensureSession();
  await rt.api.createNewSession();
  const second = rt.api.ensureSession();
  creation.resolve({ id: 'old-local' });
  assert.equal(await first, null);
  assert.equal(await second, null);
  assert.equal(rt.state.activeSessionId, null);
});

test('a reentrant queued navigation publishes its newer epoch and cancels the older creation', async () => {
  let rt;
  let notifications = 0;
  rt = loadSessionsFeature();
  rt.api.onLocalNavigation(() => {
    notifications += 1;
    if (notifications === 1) rt.api.beginLocalNavigationIntent();
  });
  const creation = rt.defer('create_session');
  const first = rt.api.ensureSession();
  assert.equal(notifications, 2, 'nested navigation must publish its newer epoch');
  assert.equal(rt.state.controlledLocalNavigationEpoch, 2);
  creation.resolve({ id: 'old-local' });
  assert.equal(await first, null);
  assert.equal(rt.state.activeSessionId, null);
});

test('same-draft coalescing does not recurse through synchronous notifications', async () => {
  let rt;
  let notifications = 0;
  const nested = [];
  rt = loadSessionsFeature();
  rt.api.onLocalNavigation(() => {
    notifications += 1;
    if (notifications <= 2) nested.push(rt.api.ensureSession());
  });
  const creation = rt.defer('create_session');
  const first = rt.api.ensureSession();
  const second = rt.api.ensureSession();
  assert.equal(notifications, 2, 'reentrant observers do not publish more navigation');
  assert.equal(nested.length, 2);
  assert.equal(rt.calls.invoke.filter(name => name === 'create_session').length, 1);
  creation.resolve({ id: 'local-new' });
  assert.deepEqual(await Promise.all([first, second, ...nested]), ['local-new', 'local-new', 'local-new', 'local-new']);
});

test('synchronous navigation notification cannot start a duplicate draft creation', async () => {
  let rt;
  let nested;
  let invoked = false;
  rt = loadSessionsFeature();
  rt.api.onLocalNavigation(() => {
    if (!invoked) { invoked = true; nested = rt.api.ensureSession(); }
  });
  const creation = rt.defer('create_session');
  const first = rt.api.ensureSession();
  assert.ok(nested, 'notification must occur before the first creation resolves');
  assert.equal(rt.calls.invoke.filter(name => name === 'create_session').length, 1);
  creation.resolve({ id: 'local-new' });
  assert.equal(await first, 'local-new');
  assert.equal(await nested, 'local-new');
});

test('direct materialization retires a controlled owner before creating its new session', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'local';
  const owner = rt.api.captureControlledNavigationOwner('g1');
  const create = rt.defer('create_session');
  const materializing = rt.api.materializeSession();
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false);
  create.resolve({ id: 'created-local' });
  assert.equal(await materializing, 'created-local');
  assert.equal(rt.state.activeSessionId, 'created-local');
});

test('direct scheduled-run navigation invalidates a previously captured controlled owner', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'local';
  const owner = rt.api.captureControlledNavigationOwner('g1');
  const load = rt.defer('load_session');
  const opening = rt.api.openScheduledRunChat({ sessionId: 'scheduled-new', runId: 'r1', status: 'completed' }, { name: 'Run' });
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false);
  load.resolve({ metadata: { id: 'scheduled-new' }, messages: [], artifacts: [] });
  assert.equal(await opening, true);
  assert.equal(rt.state.activeSessionId, 'scheduled-new');
});

test('unrelated session removal does not retire the current controlled owner', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'local';
  rt.state.sessions = [{ id: 'other' }];
  const owner = rt.api.captureControlledNavigationOwner('g1');
  await rt.api.deleteSession('other');
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), true);
});

test('controlled scheduled exit and session switch retain one bridge owner', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'scheduled-session';
  rt.state.scheduledRunContext = { sessionId: 'scheduled-session', returnSessionId: 'local' };
  const owner = rt.api.captureControlledNavigationOwner('g1');
  const exitLoad = rt.defer('load_session');
  const exiting = rt.api.exitControlledScheduledRunChat(owner);
  exitLoad.resolve({ metadata: { id: 'local' }, messages: [], artifacts: [] });
  assert.equal(await exiting, true);
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), true);
  const sessionLoad = rt.defer('load_session');
  const opening = rt.api.switchToControlledSession('session-b', owner);
  sessionLoad.resolve({ metadata: { id: 'session-b' }, messages: [], artifacts: [] });
  assert.equal(await opening, true);
  assert.equal(rt.state.activeSessionId, 'session-b');
  assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), true);
});

test('deleting or archiving the active or controlled session retires its owner immediately', async () => {
  for (const method of ['deleteSession', 'archiveSession']) {
    const rt = loadSessionsFeature();
    rt.state.desktopRemoteControl = {
      controlledDevice: { status: 'connected', generation: 'g1' },
      controlledSession: { sessionId: 'session-b', generation: 'g1' },
      controlledTasks: [], intents: { controlled: 1 },
    };
    rt.state.activeSessionId = 'session-b';
    rt.state.sessions = [{ id: 'session-b' }];
    const owner = rt.api.captureControlledNavigationOwner('g1');
    const inflight = rt.defer(method === 'deleteSession' ? 'delete_session' : 'set_session_archived');
    const pending = rt.api[method]('session-b');
    assert.equal(rt.api.isControlledNavigationOwnerCurrent(owner), false);
    inflight.resolve({});
    await pending;
  }
});

test('local navigation invalidates an in-flight controlled load before bridge commit', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  let epoch = 0;
  const captured = epoch;
  const owner = { ...rt.api.captureControlledNavigationOwner('g1'), navigationIsCurrent: () => epoch === captured };
  const load = rt.defer('load_session');
  const pending = rt.api.switchToControlledSession('session-b', owner);
  epoch += 1; // user intent, before the newer queued session switch starts
  load.resolve({ metadata: { id: 'session-b' }, messages: [] });
  assert.equal(await pending, false);
  assert.equal(rt.state.activeSessionId, null);
  assert.equal(await rt.api.switchToControlledSession('session-b', owner), false);
});

test('controlled session load rejects mismatched saved identity', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const load = rt.defer('load_session');
  const pending = rt.api.switchToControlledSession('session-b', rt.api.captureControlledNavigationOwner('g1'));
  load.resolve({ metadata: { id: 'other-session' }, messages: [] });
  assert.equal(await pending, false);
  assert.equal(rt.state.activeSessionId, null);
});

test('controlled session load commits while the same owner remains current', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const load = rt.defer('load_session');
  const pending = rt.api.switchToControlledSession('session-b', rt.api.captureControlledNavigationOwner('g1'));
  load.resolve({ metadata: { id: 'session-b' }, messages: [], artifacts: [] });
  assert.equal(await pending, true);
  assert.equal(rt.state.activeSessionId, 'session-b');
});

test('scheduled exit cannot restore an old session after its controlled owner retires', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const scheduled = { sessionId: 'scheduled-session', returnSessionId: 'old-local-session' };
  rt.state.activeSessionId = scheduled.sessionId;
  rt.state.scheduledRunContext = scheduled;
  const load = rt.defer('load_session');
  const pending = rt.api.exitControlledScheduledRunChat(rt.api.captureControlledNavigationOwner('g1'));
  rt.state.desktopRemoteControl.intents.controlled = 2;
  load.resolve({ metadata: { id: scheduled.returnSessionId }, messages: [] });
  assert.equal(await pending, false);
  assert.equal(rt.state.activeSessionId, scheduled.sessionId);
  assert.equal(rt.state.scheduledRunContext, scheduled);
});

test('scheduled exit commits the restored session and context together before async sync', async () => {
  let releaseSync;
  let reportSync;
  const syncReached = new Promise(resolve => { reportSync = resolve; });
  const syncBlocked = new Promise(resolve => { releaseSync = resolve; });
  const rt = loadSessionsFeature({ syncModeState() { reportSync(); return syncBlocked; } });
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  const scheduled = { sessionId: 'scheduled-session', returnSessionId: 'old-local-session' };
  rt.state.activeSessionId = scheduled.sessionId;
  rt.state.scheduledRunContext = scheduled;
  const load = rt.defer('load_session');
  const pending = rt.api.exitControlledScheduledRunChat(rt.api.captureControlledNavigationOwner('g1'));
  load.resolve({ metadata: { id: scheduled.returnSessionId }, messages: [], artifacts: [] });
  await syncReached;
  assert.equal(rt.state.activeSessionId, scheduled.returnSessionId);
  assert.equal(rt.state.scheduledRunContext, null);
  rt.state.desktopRemoteControl.intents.controlled = 2;
  releaseSync();
  assert.equal(await pending, false);
  assert.equal(rt.state.activeSessionId, scheduled.returnSessionId);
  assert.equal(rt.state.scheduledRunContext, null);
});

test('current controlled owner can complete a scheduled exit', async () => {
  const rt = loadSessionsFeature();
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'scheduled-session';
  rt.state.scheduledRunContext = { sessionId: 'scheduled-session', returnSessionId: 'old-local-session' };
  const load = rt.defer('load_session');
  const pending = rt.api.exitControlledScheduledRunChat(rt.api.captureControlledNavigationOwner('g1'));
  load.resolve({ metadata: { id: 'old-local-session' }, messages: [], artifacts: [] });
  assert.equal(await pending, true);
  assert.equal(rt.state.activeSessionId, 'old-local-session');
  assert.equal(rt.state.scheduledRunContext, null);
});

test('a newer user navigation wins while an old scheduled exit is syncing', async () => {
  let releaseSync;
  let reportSync;
  const syncReached = new Promise(resolve => { reportSync = resolve; });
  const syncBlocked = new Promise(resolve => { releaseSync = resolve; });
  const rt = loadSessionsFeature({ syncModeState() { reportSync(); return syncBlocked; } });
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'scheduled-session';
  rt.state.scheduledRunContext = { sessionId: 'scheduled-session', returnSessionId: 'old-local-session' };
  const load = rt.defer('load_session');
  const pending = rt.api.exitControlledScheduledRunChat(rt.api.captureControlledNavigationOwner('g1'));
  load.resolve({ metadata: { id: 'old-local-session' }, messages: [], artifacts: [] });
  await syncReached;
  rt.api.enterDraft();
  releaseSync();
  assert.equal(await pending, false);
  assert.equal(rt.state.activeSessionId, null);
  assert.equal(rt.state.scheduledRunContext, null);
});

test('new controlled owner wins while old scheduled exit is syncing', async () => {
  let releaseSync;
  let reportSync;
  let syncCalls = 0;
  const syncReached = new Promise(resolve => { reportSync = resolve; });
  const syncBlocked = new Promise(resolve => { releaseSync = resolve; });
  const rt = loadSessionsFeature({ syncModeState() { if (++syncCalls === 1) { reportSync(); return syncBlocked; } return Promise.resolve(); } });
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'scheduled-session';
  rt.state.scheduledRunContext = { sessionId: 'scheduled-session', returnSessionId: 'old-local-session' };
  const oldLoad = rt.defer('load_session');
  const oldPending = rt.api.exitControlledScheduledRunChat(rt.api.captureControlledNavigationOwner('g1'));
  oldLoad.resolve({ metadata: { id: 'old-local-session' }, messages: [], artifacts: [] });
  await syncReached;
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g2' },
    controlledSession: { sessionId: 'new-session', generation: 'g2' },
    controlledTasks: [], intents: { controlled: 2 },
  };
  const newLoad = rt.defer('load_session');
  const newPending = rt.api.switchToControlledSession('new-session', rt.api.captureControlledNavigationOwner('g2'));
  newLoad.resolve({ metadata: { id: 'new-session' }, messages: [], artifacts: [] });
  assert.equal(await newPending, true);
  releaseSync();
  assert.equal(await oldPending, false);
  assert.equal(rt.state.activeSessionId, 'new-session');
  assert.equal(rt.state.scheduledRunContext, null);
});

test('new scheduled run survives an old controlled exit finishing its sync', async () => {
  let releaseOldSync;
  let signalOldSync;
  let syncCalls = 0;
  const oldSyncReached = new Promise(resolve => { signalOldSync = resolve; });
  const oldSyncBlocked = new Promise(resolve => { releaseOldSync = resolve; });
  const rt = loadSessionsFeature({
    syncModeState() {
      if (++syncCalls === 1) { signalOldSync(); return oldSyncBlocked; }
      return Promise.resolve();
    },
  });
  rt.state.desktopRemoteControl = {
    controlledDevice: { status: 'connected', generation: 'g1' },
    controlledSession: { sessionId: 'session-b', generation: 'g1' },
    controlledTasks: [], intents: { controlled: 1 },
  };
  rt.state.activeSessionId = 'old-scheduled';
  rt.state.scheduledRunContext = { sessionId: 'old-scheduled', returnSessionId: 'local' };
  const oldLoad = rt.defer('load_session');
  const oldExit = rt.api.exitControlledScheduledRunChat(rt.api.captureControlledNavigationOwner('g1'));
  oldLoad.resolve({ metadata: { id: 'local' }, messages: [], artifacts: [] });
  await oldSyncReached;
  assert.equal(rt.state.activeSessionId, 'local');
  assert.equal(rt.state.scheduledRunContext, null);

  const newLoad = rt.defer('load_session');
  const newOpen = rt.api.openScheduledRunChat({ sessionId: 'new-scheduled', runId: 'run-new', status: 'completed' }, { name: 'New task' });
  newLoad.resolve({ metadata: { id: 'new-scheduled' }, messages: [], artifacts: [] });
  assert.equal(await newOpen, true);
  const newContext = rt.state.scheduledRunContext;
  assert.equal(rt.state.activeSessionId, 'new-scheduled');
  assert.equal(newContext.sessionId, 'new-scheduled');
  assert.equal(newContext.runId, 'run-new');
  assert.equal(newContext.returnSessionId, 'local');

  releaseOldSync();
  assert.equal(await oldExit, false);
  assert.equal(rt.state.activeSessionId, 'new-scheduled');
  assert.equal(rt.state.scheduledRunContext, newContext);
});

test('ensureSession：create_session 等待期间再进草稿 → 不物化、返回 null', async () => {
  const rt = loadSessionsFeature();
  const create = rt.defer('create_session');
  const p = rt.api.ensureSession();
  rt.api.enterDraft(); // 用户在慢 create_session 期间点了「新建对话」（再进草稿）
  create.resolve({ id: 'chat-new' });
  assert.equal(await p, null, '导航 token 已前移，物化必须中止');
  assert.equal(rt.state.activeSessionId, null, '不得劫持用户的新草稿');
  assert.ok(rt.sessionStates['chat-new'], '新会话仍应登记为后台 buffer 等待下次切换');
  assert.equal(rt.sessionStates['chat-new'].loadedFromDisk, true);
});

test('ensureSession：无导航 → 正常物化返回 meta.id', async () => {
  const rt = loadSessionsFeature();
  const create = rt.defer('create_session');
  const p = rt.api.ensureSession();
  create.resolve({ id: 'chat-ok' });
  assert.equal(await p, 'chat-ok');
  assert.equal(rt.state.activeSessionId, 'chat-ok');
});

test('ensureSession：尾部 await 期间再进草稿 → 返回 null 不漂移', async () => {
  const rt = loadSessionsFeature();
  const create = rt.defer('create_session');
  const list = rt.defer('list_sessions'); // 挂住尾部链，制造真实 await 窗口
  const p = rt.api.ensureSession();
  create.resolve({ id: 'chat-tail' });
  await new Promise(r => setTimeout(r, 0)); // 物化推进到尾部 refreshHistoryList 并挂起
  rt.api.enterDraft(); // 尾部 await 期间再进草稿：activeSessionId 被 enterDraft 置 null、token 前移
  list.resolve([]);
  assert.equal(await p, null, '尾部窗口的再进草稿同样必须中止物化');
  assert.equal(rt.state.activeSessionId, null);
});

test('ensureSession：create_session 等待期间切到既有会话 → 返回 null（既有行为保持）', async () => {
  const rt = loadSessionsFeature();
  const create = rt.defer('create_session');
  const p = rt.api.ensureSession();
  rt.state.activeSessionId = 'chat-b'; // 用户直接切到既有会话 B
  create.resolve({ id: 'chat-new' });
  assert.equal(await p, null);
  assert.equal(rt.state.activeSessionId, 'chat-b', '不得改写已激活的会话 B');
  assert.ok(rt.sessionStates['chat-new'], '新会话登记为后台 buffer');
});

// ── archiveSession：失败回滚不劫持新草稿 ──────────────────────────

test('archiveSession（普通会话）：归档失败但等待期间再进草稿 → 不回滚 active', async () => {
  const rt = loadSessionsFeature();
  rt.state.sessions = [{ id: 'chat-a', title: 'A' }];
  rt.state.activeSessionId = 'chat-a';
  const set = rt.defer('set_session_archived');
  const p = rt.api.archiveSession('chat-a');
  await new Promise(r => setTimeout(r, 0)); // 推进到 leaveSessionView 已执行（active 已置 null）
  assert.equal(rt.state.activeSessionId, null);
  rt.api.enterDraft(); // 归档等待期间用户再进草稿：token 前移、active 仍为 null
  set.reject(new Error('backend down'));
  assert.equal(await p, false);
  assert.equal(rt.state.activeSessionId, null, '失败回滚不得把用户拽回归档会话（劫持新草稿）');
  assert.equal(rt.state.sessions.length, 1, '会话列表仍按失败语义回滚恢复');
});

test('archiveSession（普通会话）：归档失败且无导航 → 回滚 active（既有行为保持）', async () => {
  const rt = loadSessionsFeature();
  rt.state.sessions = [{ id: 'chat-a', title: 'A' }];
  rt.state.activeSessionId = 'chat-a';
  const set = rt.defer('set_session_archived');
  const p = rt.api.archiveSession('chat-a');
  await new Promise(r => setTimeout(r, 0));
  set.reject(new Error('backend down'));
  assert.equal(await p, false);
  assert.equal(rt.state.activeSessionId, 'chat-a', '无新导航时仍应回滚恢复 active');
});

test('archiveSession（scheduled run）：归档失败但等待期间再进草稿 → 不回滚 active/context', async () => {
  const rt = loadSessionsFeature();
  rt.state.activeSessionId = 'sched-run-1';
  rt.state.scheduledRunContext = { sessionId: 'sched-run-1' };
  rt.state.scheduledTaskRecentRuns = [{ sessionId: 'sched-run-1', runId: 'run-1' }];
  const set = rt.defer('set_session_archived');
  const p = rt.api.archiveSession('sched-run-1');
  await new Promise(r => setTimeout(r, 0));
  assert.equal(rt.state.activeSessionId, null);
  rt.api.enterDraft(); // 等待期间再进草稿
  set.reject(new Error('backend down'));
  assert.equal(await p, false);
  assert.equal(rt.state.activeSessionId, null, '失败回滚不得劫持新草稿');
  assert.equal(rt.state.scheduledRunContext, null, 'context 也不得成对回滚');
});

test('archiveSession（scheduled run）：归档失败且无导航 → active/context 成对回滚（既有行为保持）', async () => {
  const rt = loadSessionsFeature();
  rt.state.activeSessionId = 'sched-run-1';
  rt.state.scheduledRunContext = { sessionId: 'sched-run-1' };
  rt.state.scheduledTaskRecentRuns = [{ sessionId: 'sched-run-1', runId: 'run-1' }];
  const set = rt.defer('set_session_archived');
  const p = rt.api.archiveSession('sched-run-1');
  await new Promise(r => setTimeout(r, 0));
  set.reject(new Error('backend down'));
  assert.equal(await p, false);
  assert.equal(rt.state.activeSessionId, 'sched-run-1', '无新导航时成对回滚');
  assert.deepEqual(rt.state.scheduledRunContext, { sessionId: 'sched-run-1' });
});
