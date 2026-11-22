import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { latestModelLifecycleRejection, isModelLifecycleRejection } from '../src/features/account-platform/model-lifecycle.js';
import { createNativeLane, hydrateNativeLane, appendLocalUserMessage, removeLocalUserMessage, applyNativeChatEvent, refreshNativeTurnTimeline } from '../src/features/codex/code-native-lane.js';

const rejection = { schema_version: 1, code: 'MODEL_UNAVAILABLE', model_id: '01a08b7a-654d-7e8d-8a7c-87b4506f2c7c' };
const legacyRejection = { schema_version: 1, code: 'MODEL_UNAVAILABLE', model_id: '', legacy_binding: true };
const start = (id, timestamp = 1) => ({ event: 'user_start', turn_id: id, timestamp });
const done = (id, marker = rejection, status = 'Failed', timestamp = 2) => ({ event: 'assistant_done', turn_id: id, timestamp, status, model_rejection: marker });
for (const status of ['Failed', 'send_error']) assert.deepEqual(latestModelLifecycleRejection([start('a'), done('a', rejection, status)]), { turnId: 'a', rejection });
for (const status of ['Failed', 'send_error']) assert.deepEqual(latestModelLifecycleRejection([start('a'), done('a', legacyRejection, status)]), { turnId: 'a', rejection: legacyRejection });
for (const value of [{ ...legacyRejection, model_id: 'deepseek-v4-flash' }, { ...legacyRejection, model_id: rejection.model_id }, { ...legacyRejection, code: 'MODEL_DEPRECATED' }, { ...legacyRejection, legacy_binding: false }, { ...rejection, legacy_binding: 'true' }, { ...rejection, legacy_binding: null }]) assert.equal(isModelLifecycleRejection(value), false);
for (const status of ['Completed', 'succeeded', 'Cancelled', 'Interrupted', '', 'unknown']) assert.equal(latestModelLifecycleRejection([start('a'), done('a', rejection, status)]), null);
for (const code of ['MODEL_UNAVAILABLE', 'MODEL_DEPRECATED']) assert.equal(isModelLifecycleRejection({ ...rejection, code }), true);
for (const value of [{ ...rejection, schema_version: 2 }, { ...rejection, model_id: 'not-a-uuid' }, { ...rejection, model_id: '00000000-0000-0000-0000-000000000000' }, { ...rejection, code: 'MODEL_REMOVED' }]) assert.equal(isModelLifecycleRejection(value), false);
for (const text of ['模型不可用 MODEL_UNAVAILABLE', 'Model unavailable MODEL_DEPRECATED', 'モデル利用不可']) assert.equal(latestModelLifecycleRejection([start('a'), { ...done('a', null), error: text }]), null);
assert.equal(latestModelLifecycleRejection([start('a'), done('a'), start('b')]), null);
assert.equal(latestModelLifecycleRejection([start('a'), done('a'), start('b'), done('b', null, 'Completed')]), null);
assert.equal(latestModelLifecycleRejection([start('a'), done('a'), { event: 'user_start' }]), null);

const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
const disk = [start('disk', 10), done('disk', rejection, 'send_error', 20)];
// The native production projection changes timeline only; successful recovery
// retains item, token and pending-request state and old billing/user guidance.
const lane = createNativeLane();
hydrateNativeLane(lane, { messages: [] }, []);
const item = { id: 44, type: 'system', text: 'retained' }; lane.items.push(item);
lane.tokens.input = 123;
assert.equal(await refreshNativeTurnTimeline(lane, async () => disk, () => true), true);
assert.deepEqual(latestModelLifecycleRejection(lane.timeline)?.rejection, rejection);
assert.equal(lane.items[0], item); assert.equal(lane.tokens.input, 123);
for (const change of ['next-send', 'next-turn-started', 'hydrate', 'deleted-recreated', 'unmount']) {
  const current = createNativeLane(); let owned = true;
  const waiting = deferred();
  const pending = refreshNativeTurnTimeline(current, () => waiting.promise, () => owned);
  if (change === 'next-send') appendLocalUserMessage(current, 'later');
  else if (change === 'next-turn-started') applyNativeChatEvent(current, 'chat:turn_started', { turn_id: 'later' });
  else if (change === 'hydrate') hydrateNativeLane(current, { messages: [] }, [start('new')]);
  else owned = false;
  waiting.resolve(disk); assert.equal(await pending, false, change);
  assert.equal(latestModelLifecycleRejection(current.timeline), null);
}
const ordered = createNativeLane(), first = deferred(), second = deferred();
const p1 = refreshNativeTurnTimeline(ordered, () => first.promise, () => true);
const p2 = refreshNativeTurnTimeline(ordered, () => second.promise, () => true);
second.resolve(disk); assert.equal(await p2, true);
first.resolve([start('older'), done('older')]); assert.equal(await p1, false);
const preSubmit = createNativeLane();
const optimistic = appendLocalUserMessage(preSubmit, 'send'); removeLocalUserMessage(preSubmit, optimistic);
const preparedDisk = [start("prepared-rust-id", preSubmit.timelineTurnAnchor.submittedAt + 1), done("prepared-rust-id", rejection, "send_error", preSubmit.timelineTurnAnchor.submittedAt + 2)];
assert.equal(await refreshNativeTurnTimeline(preSubmit, async () => preparedDisk, () => true), true);
assert.deepEqual(latestModelLifecycleRejection(preSubmit.timeline)?.rejection, rejection);

const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(new URL('../src/shared/bridge-shared-helpers.js', import.meta.url), 'utf8'), sandbox);
vm.runInNewContext(fs.readFileSync(new URL('../src/platform/tauri/bridge/chat-events.js', import.meta.url), 'utf8'), sandbox);
const install = sandbox.window.__PINVOU_TAURI_BRIDGE_FEATURES__['chat-events'];
function fixture() {
  const state = { activeSessionId: 's', turnTimeline: [], chatItems: [] };
  const owner = { turnTimeline: [] }, owners = { s: owner }, handlers = {}, deleted = new Set();
  const waiting = [];
  const feature = install({ state, sessionStates: owners, listen(name, fn) { handlers[name] = fn; },
    invoke() { const d = deferred(); waiting.push(d); return d.promise; },
    runSyncOnSession(sid, action) { if (owners[sid]) action(); }, notify() {}, safeConsoleInfo() {},
    onSessionDeleted(fn) { deleted.add(fn); return () => deleted.delete(fn); },
    onSessionEvent(_event, fn) { fn(); }, startThinking() {},
  });
  return { state, owner, owners, waiting, deleted, handlers, feature };
}
for (const change of ['deleted-recreated', 'next-send', 'next-turn-started', 'latest-anchor']) {
  const f = fixture(), pending = f.feature.refreshAuthoritativeTurnTimeline('s');
  if (change === 'deleted-recreated') { f.deleted.forEach(fn => fn('s')); f.owners.s = { turnTimeline: [] }; }
  else if (change === 'next-send') f.owner.timelineReadEpoch = 1;
  else if (change === 'next-turn-started') { f.state.thinking = { active: true }; f.handlers['chat:turn_started']({ payload: { session_id: 's' } }); }
  else f.state.turnTimeline = [start('later')];
  f.waiting[0].resolve(disk); assert.equal(await pending, false, change);
  assert.equal(latestModelLifecycleRejection(f.state.turnTimeline), null);
  assert.equal(f.deleted.size, 0, 'per-read delete listener must be released');
}
const f = fixture();
const a = f.feature.refreshAuthoritativeTurnTimeline('s'), b = f.feature.refreshAuthoritativeTurnTimeline('s');
f.waiting[1].resolve(disk); assert.equal(await b, true);
f.waiting[0].resolve([start('old'), done('old')]); assert.equal(await a, false);
assert.deepEqual(latestModelLifecycleRejection(f.state.turnTimeline)?.rejection, rejection);
delete f.owners.s;
assert.equal(await f.feature.refreshAuthoritativeTurnTimeline('s'), false, 'read must never allocate a deleted buffer');

// A stayed current throughout the read, but B was already current before it.
// The old snapshot must be rejected even when completion is less than 5s old.
for (const completed of [false, true]) {
  for (const sameMillisecond of [false, true]) {
    const now = Date.now(), aTime = sameMillisecond ? now : now - 10;
    const old = [start('rust-A', aTime), done('rust-A', rejection, 'Failed', aTime)];
    const current = createNativeLane(); hydrateNativeLane(current, { messages: [] }, old);
    const optimisticId = appendLocalUserMessage(current, 'B');
    if (completed) applyNativeChatEvent(current, 'chat:done', { status: 'Completed' });
    const before = current.timeline;
    assert.equal(await refreshNativeTurnTimeline(current, async () => old, () => true), false, 'native older A is excluded before the read begins');
    assert.equal(current.timeline, before); assert.equal(latestModelLifecycleRejection(current.timeline), null);
    const f = fixture(); f.state.turnTimeline = [...old, start('ui-B', now)];
    if (completed) f.state.turnTimeline.push(done('ui-B', null, 'Completed', now));
    const waiting = f.feature.refreshAuthoritativeTurnTimeline('s');
    f.waiting[0].resolve(old); assert.equal(await waiting, false, 'plain older A is excluded before the read begins');
    assert.equal(latestModelLifecycleRejection(f.state.turnTimeline), null);
    // Exact same-ms new Rust ids remain admissible; no synthetic id equality.
    const typedB = [...old, start('rust-B', current.timelineTurnAnchor.submittedAt), done('rust-B', rejection, 'Failed', current.timelineTurnAnchor.submittedAt + 1)];
    assert.equal(await refreshNativeTurnTimeline(current, async () => typedB, () => true), true);
    assert.deepEqual(latestModelLifecycleRejection(current.timeline)?.rejection, rejection);
    const next = f.feature.refreshAuthoritativeTurnTimeline('s');
    f.waiting[1].resolve(typedB); assert.equal(await next, true);
    assert.deepEqual(latestModelLifecycleRejection(f.state.turnTimeline)?.rejection, rejection);
    assert.ok(current.timeline.some(event => event.turn_id === 'rust-A' && event.model_rejection), 'A evidence remains history');
    void optimisticId;
  }
}

// The real native IPC catch explicitly retains only the failed current UI turn;
// the helper's default zero-timeline rollback assertion stays unchanged.
const failed = createNativeLane(), failedAt = Date.now();
hydrateNativeLane(failed, { messages: [] }, [start('older-A', failedAt), done('older-A', rejection, 'Failed', failedAt)]);
const failedId = appendLocalUserMessage(failed, 'prepare B');
removeLocalUserMessage(failed, failedId, { preserveFailedTurn: true });
assert.equal(latestModelLifecycleRejection(failed.timeline), null);
assert.equal(await refreshNativeTurnTimeline(failed, async () => [start('older-A', failedAt), done('older-A', rejection, 'Failed', failedAt)], () => true), false);
const failedDisk = [start('new-prepared-B', failed.timelineTurnAnchor.submittedAt + 1), done('new-prepared-B', rejection, 'send_error', failed.timelineTurnAnchor.submittedAt + 2)];
assert.equal(await refreshNativeTurnTimeline(failed, async () => failedDisk, () => true), true);
assert.deepEqual(latestModelLifecycleRejection(failed.timeline)?.rejection, rejection);

// A received native start can lag real backend timing. Its timestamp is not a
// pre-IPC anchor; known A is excluded while a distinct new B is still accepted.
const remote = createNativeLane(), past = Date.now() - 10;
hydrateNativeLane(remote, { messages: [] }, [start('remote-A', past), done('remote-A', rejection, 'Failed', past)]);
applyNativeChatEvent(remote, 'chat:turn_started', { session_id: 's' });
assert.equal(remote.timelineTurnAnchor.submittedAt, null);
assert.equal(await refreshNativeTurnTimeline(remote, async () => [start('remote-A', past), done('remote-A')], () => true), false);
assert.equal(await refreshNativeTurnTimeline(remote, async () => [start('remote-A', past), done('remote-A'), start('remote-rust-B', past + 1), done('remote-rust-B', rejection, 'Failed', past + 2)], () => true), true);

// Actual plain chat factory → failed IPC catch → actual chat-events authority
// read. No user_start event is delivered for this preparation failure.
vm.runInNewContext(fs.readFileSync(new URL('../src/platform/tauri/bridge/chat.js', import.meta.url), 'utf8'), sandbox);
const installChat = sandbox.window.__PINVOU_TAURI_BRIDGE_FEATURES__.chat;
const failedPlain = fixture(), originalTime = Date.now();
const plainOld = [start('plain-old-A', originalTime), done('plain-old-A', rejection, 'Failed', originalTime)];
failedPlain.state.turnTimeline = plainOld;
failedPlain.state.messages = []; failedPlain.state.busy = false;
const plainReadPromises = [];
const plainContext = {
  state: failedPlain.state, sessionStates: failedPlain.owners,
  invoke() { return Promise.reject(new Error('controlled prepare failure')); },
  refreshAuthoritativeTurnTimeline(sid) {
    const promise = failedPlain.feature.refreshAuthoritativeTurnTimeline(sid);
    plainReadPromises.push(promise); return promise;
  },
  notify() {}, TAURI: null, turnUsageDirty: {}, personaPlaceholderTitles: {},
  renderMarkdown(value) { return value; }, safeConsoleInfo() {}, bt(key) { return key; },
  runSyncOnSession(sid, action) { if (failedPlain.owners[sid]) action(); },
  getBuffer() { return failedPlain.owner; }, startThinking() {}, stopThinking() {},
  ensureSessionBufferLoaded() { return Promise.resolve(); }, ensureSession() { return Promise.resolve('s'); },
  reconcileRemoteTurn() { return Promise.resolve(true); }, markRemoteTurn() {},
  isScheduledRunSession() { return false; }, userMessageDisplayText() { return ''; },
  parseScheduledTaskDraftFromText() { return null; }, autoCreateScheduledTaskDraft() {},
  pendingAssistantText: '', pendingAssistantBlocks: [], currentStreamText: '', currentStreamId: 0, itemIdSeq: 0,
};
const plainChat = installChat(plainContext);
assert.equal(await plainChat.doSendFor('s', 'B', 'B', [], null, false, false), false);
assert.equal(plainReadPromises.length, 1, 'prepare catch refreshes without chat:done');
assert.ok(failedPlain.owner.timelineTurnAnchor.knownStartIds.includes('plain-old-A'));
assert.equal(failedPlain.state.turnTimeline.at(-1).status, 'send_error');
assert.equal(latestModelLifecycleRejection(failedPlain.state.turnTimeline), null);
assert.equal(failedPlain.state.chatItems.some(item => item.type === 'user'), false, 'failed user bubble stays rolled back');
failedPlain.waiting[0].resolve(plainOld);
assert.equal(await plainReadPromises[0], false, 'actual plain catch cannot reinstall old A');
assert.equal(latestModelLifecycleRejection(failedPlain.state.turnTimeline), null);
const plainTypedRead = failedPlain.feature.refreshAuthoritativeTurnTimeline('s');
const plainBAt = failedPlain.owner.timelineTurnAnchor.submittedAt;
failedPlain.waiting[1].resolve([...plainOld, start('plain-rust-B', plainBAt), done('plain-rust-B', rejection, 'send_error', plainBAt + 1)]);
assert.equal(await plainTypedRead, true, 'new backend prepare timing id differs from UI start and is accepted');
assert.deepEqual(latestModelLifecycleRejection(failedPlain.state.turnTimeline)?.rejection, rejection);


// A previous authority read can fail while the UI retains only synthetic A.
// SID-only remote B then has no reliable UI↔Rust identity. Old Rust A must not
// become B merely because it is an unfamiliar id or less than five seconds old.
for (const sameMillisecond of [false, true]) {
  const t = Date.now();
  const stale = [start('unknown-rust-A', sameMillisecond ? t : t - 1), done('unknown-rust-A', rejection, 'Failed', t + 1)];
  const unknownNative = createNativeLane();
  appendLocalUserMessage(unknownNative, 'A');
  applyNativeChatEvent(unknownNative, 'chat:user_message', { session_id: 's', content: 'A' });
  applyNativeChatEvent(unknownNative, 'chat:done', { status: 'Failed' });
  applyNativeChatEvent(unknownNative, 'chat:user_message', { session_id: 's', content: 'remote B' });
  applyNativeChatEvent(unknownNative, 'chat:turn_started', { session_id: 's' });
  assert.equal(unknownNative.timelineTurnAnchor.submittedAt, null);
  const retained = unknownNative.timeline;
  assert.equal(await refreshNativeTurnTimeline(unknownNative, async () => stale, () => true), false);
  assert.equal(unknownNative.timeline, retained);
  assert.equal(latestModelLifecycleRejection(unknownNative.timeline), null);

  const unknownPlain = fixture();
  unknownPlain.state.turnTimeline = [start('ui_A', t), done('ui_A', null, 'Failed', t)];
  unknownPlain.state.thinking = { active: true };
  unknownPlain.handlers['chat:turn_started']({ payload: { session_id: 's' } });
  assert.equal(unknownPlain.owner.timelineTurnAnchor.submittedAt, null);
  const pending = unknownPlain.feature.refreshAuthoritativeTurnTimeline('s');
  unknownPlain.waiting[0].resolve(stale);
  assert.equal(await pending, false);
  assert.equal(latestModelLifecycleRejection(unknownPlain.state.turnTimeline), null);
}

// A local pre-IPC clock at exactly the old Rust millisecond is ambiguous even
// with an empty UI history: an earlier prepare failure may have no saved user.
for (const history of [[], [start('ui_old_A', 1), done('ui_old_A', null, 'Failed', 2)]]) {
  const current = createNativeLane(); current.timeline = history;
  const optimisticId = appendLocalUserMessage(current, 'local B');
  const submittedAt = current.timelineTurnAnchor.submittedAt;
  applyNativeChatEvent(current, 'chat:turn_started', { session_id: 's' });
  assert.equal(current.timelineTurnAnchor.submittedAt, submittedAt, 'native SID-only start preserves actual local pre-IPC anchor');
  const ambiguous = [start('unknown-old-same-ms', submittedAt), done('unknown-old-same-ms', rejection, 'Failed', submittedAt + 1)];
  assert.equal(await refreshNativeTurnTimeline(current, async () => ambiguous, () => true), false);
  const provenNew = [start('new-after-submit', submittedAt + 1), done('new-after-submit', rejection, 'Failed', submittedAt + 2)];
  assert.equal(await refreshNativeTurnTimeline(current, async () => provenNew, () => true), true);
  void optimisticId;
}

// A complete authority snapshot containing known Rust A followed by B proves
// sequence identity at equal milliseconds; a snapshot missing A does not.
const canonical = createNativeLane(), canonicalT = Date.now();
hydrateNativeLane(canonical, { messages: [] }, [start('canonical-A', canonicalT), done('canonical-A', null, 'Completed', canonicalT)]);
applyNativeChatEvent(canonical, 'chat:turn_started', { session_id: 's' });
assert.equal(await refreshNativeTurnTimeline(canonical, async () => [start('older-unknown-Z', canonicalT), done('older-unknown-Z')], () => true), false);
assert.equal(await refreshNativeTurnTimeline(canonical, async () => [start('canonical-A', canonicalT), done('canonical-A', null, 'Completed', canonicalT), start('canonical-B', canonicalT), done('canonical-B', rejection, 'Failed', canonicalT + 1)], () => true), true);

const runningPlain = fixture(), pendingPlainIPC = deferred();
runningPlain.state.messages = []; runningPlain.state.thinking = { active: true };
const runningPlainChat = installChat({ ...plainContext,
  state: runningPlain.state, sessionStates: runningPlain.owners,
  invoke() { return pendingPlainIPC.promise; },
  runSyncOnSession(sid, action) { if (runningPlain.owners[sid]) action(); },
  getBuffer() { return runningPlain.owner; },
  refreshAuthoritativeTurnTimeline(sid) { return runningPlain.feature.refreshAuthoritativeTurnTimeline(sid); },
});
const localPlainSend = runningPlainChat.doSendFor('s', 'B', 'B', [], null, false, false);
const localPlainClock = runningPlain.owner.timelineTurnAnchor.submittedAt;
runningPlain.handlers['chat:turn_started']({ payload: { session_id: 's' } });
assert.equal(runningPlain.owner.timelineTurnAnchor.submittedAt, localPlainClock, 'actual plain local submit then SID-only start preserves pre-IPC anchor');
const ambiguousPlainRead = runningPlain.feature.refreshAuthoritativeTurnTimeline('s');
runningPlain.waiting[0].resolve([start('old-unhydrated-same-ms', localPlainClock), done('old-unhydrated-same-ms', rejection, 'Failed', localPlainClock + 1)]);
assert.equal(await ambiguousPlainRead, false, 'plain empty local history is not proof of absent old timing');
const provenPlainRead = runningPlain.feature.refreshAuthoritativeTurnTimeline('s');
runningPlain.waiting[1].resolve([start('new-after-local-submit', localPlainClock + 1), done('new-after-local-submit', rejection, 'Failed', localPlainClock + 2)]);
assert.equal(await provenPlainRead, true, 'plain different new Rust timing id strictly after actual submit is accepted');
pendingPlainIPC.resolve(); await localPlainSend;

// Turn ids come from the latest authoritative start/terminal pair, never rows.
for (const turnId of [null, undefined, 5, {}, '', '   ']) {
  assert.equal(latestModelLifecycleRejection([start(turnId), done(turnId)]), null);
}
assert.deepEqual(latestModelLifecycleRejection([start('old'), done('old'), start('rust-current'), done('other'), done('rust-current')]), { turnId: 'rust-current', rejection });
