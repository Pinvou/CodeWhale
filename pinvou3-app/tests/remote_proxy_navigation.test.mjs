import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const source = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)),
  '../src/platform/tauri/bridge/sessions.js'), 'utf8');
const chatSource = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)),
  '../src/platform/tauri/bridge/chat.js'), 'utf8');

function install(read, remove, select) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = { window: root, console };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  vm.runInContext(chatSource, sandbox);
  const invokes = [];
  const sessionStates = Object.create(null);
  const draftValues = new Map();
  const remoteProxyDrafts = {
    get: id => draftValues.get(id) || '',
    has: id => draftValues.has(id),
    set(id, text) { draftValues.set(id, String(text)); return true; },
    delete: id => draftValues.delete(id),
  };
  const state = {
    sessions: [], remoteProxyHistories: [{ localSessionId: 'remote-local-1' }],
    remoteProxyHistory: null, activeSessionId: null, messages: [], chatItems: [],
    artifacts: [], tokens: { input: 0, max: 0 }, modeState: { mode: 'yolo' },
  };
  const feature = root.__PINVOU_TAURI_BRIDGE_FEATURES__.sessions({
    state, sessionStates, remoteProxyDrafts,
    scheduledRunSessionOwners: {}, personaPlaceholderTitles: {},
    invoke(command) { invokes.push(command); return Promise.reject(new Error('local load rejected')); },
    readRemoteProxyHistory: read,
    selectRemoteProxyHistory: select,
    deleteRemoteProxyHistory: remove,
    notify() {}, bt: value => value, currentDraftModeState: () => ({ mode: 'yolo' }),
    addSystemItem() {},
    loadMemoryOverview: async () => {},
  });
  const chat = root.__PINVOU_TAURI_BRIDGE_FEATURES__.chat({
    state, sessionStates, remoteProxyDrafts,
    isKnownRemoteProxy: feature.isKnownRemoteProxy,
    isRemoteProxyView: feature.isRemoteProxyView,
    notify() {}, voice: () => ({}),
  });
  return { feature, chat, invokes, sessionStates, state, remoteProxyDrafts, root, sandbox };
}

test('verified RemoteProxy navigation adopts one active ID without load_session or local buffer', async () => {
  const { feature, invokes, sessionStates, state } = install(async id => ({
    localSessionId: id, entries: [{ journalEpoch: 'e1', sequence: 1, eventId: 'a', kind: 'user_message', body: 'hello' }],
  }));
  assert.equal(await feature.switchToSession('remote-local-1'), true);
  assert.equal(state.activeSessionId, 'remote-local-1');
  assert.deepEqual(invokes, []);
  assert.deepEqual(Object.keys(sessionStates), []);
  assert.deepEqual(Array.from(state.chatItems), [], 'projection is not copied into the local working set');
  feature.enterDraft();
  assert.equal(state.activeSessionId, null);
  assert.deepEqual(Object.keys(sessionStates), [], 'leaving a proxy never creates a ghost Engine/ACP buffer');
});

test('a background A history refresh cannot cancel or overwrite navigation to B', async () => {
  let remoteFeature;
  const pending = [];
  const harness = install(
    id => remoteFeature.readRemoteProxyHistory(id, { navigation: true }),
    undefined,
    record => remoteFeature.selectRemoteProxyHistory(record),
  );
  const { feature, state, root, sandbox } = harness;
  vm.runInContext(fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)),
    '../src/features/remote-control/remote-control-state.js'), 'utf8'), sandbox);
  vm.runInContext(fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)),
    '../src/platform/tauri/bridge/desktop-remote-control.js'), 'utf8'), sandbox);
  state.desktopRemoteControl = root.__PINVOU_REMOTE_CONTROL_STATE__.createState();
  remoteFeature = root.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state, can: () => true, notify() {}, listen: () => Promise.resolve(() => {}),
    invoke(command, args) {
      if (command !== 'desktop_remote_proxy_history_read') return Promise.resolve([]);
      return new Promise(resolve => pending.push({ id: args.localSessionId, resolve }));
    },
  });
  state.remoteProxyHistories.push(
    { localSessionId: 'remote-local-1' }, { localSessionId: 'remote-local-2' },
  );
  const aNavigation = feature.switchToSession('remote-local-1');
  await Promise.resolve();
  assert.equal(pending.length, 1);
  pending.shift().resolve({ localSessionId: 'remote-local-1', title: 'A', entries: [],
    pendingUnknownCount: 0, unattemptedPending: [] });
  assert.equal(await aNavigation, true);

  const bNavigation = feature.switchToSession('remote-local-2');
  const aRefresh = remoteFeature.readRemoteProxyHistory('remote-local-1');
  await Promise.resolve();
  const aRequest = pending.find(item => item.id === 'remote-local-1');
  const bRequest = pending.find(item => item.id === 'remote-local-2');
  aRequest.resolve({ localSessionId: 'remote-local-1', title: 'A refreshed', entries: [],
    pendingUnknownCount: 0, unattemptedPending: [] });
  assert.equal((await aRefresh).title, 'A refreshed');
  bRequest.resolve({ localSessionId: 'remote-local-2', title: 'B', entries: [],
    pendingUnknownCount: 0, unattemptedPending: [] });
  assert.equal(await bNavigation, true);
  assert.equal(state.activeSessionId, 'remote-local-2');
  assert.equal(state.remoteProxyHistory.title, 'B');

  const lateA = remoteFeature.readRemoteProxyHistory('remote-local-1');
  pending.at(-1).resolve({ localSessionId: 'remote-local-1', title: 'Late A', entries: [],
    pendingUnknownCount: 0, unattemptedPending: [] });
  assert.equal((await lateA).title, 'Late A');
  assert.equal(state.remoteProxyHistory.title, 'B');
});

test('stale or rejected Rust read cannot select a proxy and never falls back to load_session', async () => {
  for (const read of [async () => null, async () => ({ localSessionId: 'remote-other', entries: [] }),
    async () => { throw new Error('remote_proxy_not_found'); }]) {
    const { feature, invokes, state } = install(read);
    assert.equal(await feature.switchToSession('remote-local-1'), false);
    assert.equal(state.activeSessionId, null);
    assert.deepEqual(invokes, []);
  }
});

test('a remote-looking ID absent from the authoritative list is not treated as a proxy', async () => {
  const { feature, invokes, state } = install(async () => {
    throw new Error('must not read');
  });
  assert.equal(await feature.switchToSession('remote-guess'), false);
  assert.deepEqual(invokes, ['load_session']);
  assert.equal(state.activeSessionId, null);
});

test('a late read cannot override a later draft navigation', async () => {
  let finish;
  const { feature, state } = install(() => new Promise(resolve => { finish = resolve; }));
  const pending = feature.switchToSession('remote-local-1');
  feature.enterDraft();
  finish({ localSessionId: 'remote-local-1', entries: [] });
  assert.equal(await pending, false);
  assert.equal(state.activeSessionId, null);
});

test('legacy history helpers cannot reopen the second in-memory/sidecar read path', () => {
  const bridge = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)),
    '../src/platform/tauri/bridge/desktop-remote-control.js'), 'utf8');
  const facade = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)),
    '../src/platform/tauri/bridge.js'), 'utf8');
  for (const name of [
    'controllerListHistory', 'controllerCreateHistory',
    'historyList', 'historyLoad', 'historyDelete', 'historyMarkOffline',
  ]) {
    assert.ok(!bridge.includes(`function ${name}(`));
    assert.ok(!facade.includes(`${name}: desktopRemoteControlFeature.${name}`));
  }
  for (const command of [
    'desktop_remote_controller_list_history', 'desktop_remote_controller_create_history',
    'desktop_remote_history_list', 'desktop_remote_history_load',
    'desktop_remote_history_delete', 'desktop_remote_history_mark_offline',
  ]) assert.ok(!bridge.includes(`invoke("${command}"`));
});

test('RemoteProxy delete soft-hides and metadata actions never enter local Session Commands', async () => {
  const removed = [];
  const { feature, invokes, state } = install(
    async id => ({ localSessionId: id, entries: [] }),
    async id => { removed.push(id); },
  );
  assert.equal(await feature.switchToSession('remote-local-1'), true);
  assert.equal(await feature.renameSession('remote-local-1', 'new'), false);
  assert.equal(await feature.toggleSessionPinned('remote-local-1', true), false);
  assert.equal(await feature.archiveSession('remote-local-1'), false);
  assert.equal(await feature.deleteSession('remote-local-1'), true);
  assert.deepEqual(removed, ['remote-local-1']);
  assert.equal(state.activeSessionId, null);
  assert.deepEqual(invokes, []);
});

test('composer-only drafts remain isolated across two proxies and local draft navigation', async () => {
  const { feature, chat, state, sessionStates, remoteProxyDrafts } = install(async id => ({ localSessionId: id, entries: [] }));
  state.remoteProxyHistories.push({ localSessionId: 'remote-local-2' });
  assert.equal(await feature.switchToSession('remote-local-1'), true);
  chat.setComposerDraft('proxy one');
  assert.equal(await feature.switchToSession('remote-local-2'), true);
  chat.setComposerDraft('proxy two');
  assert.equal(await feature.switchToSession('remote-local-1'), true);
  assert.equal(chat.getComposerDraft(), 'proxy one');
  feature.enterDraft();
  chat.setComposerDraft('local draft');
  assert.equal(await feature.switchToSession('remote-local-2'), true);
  assert.equal(chat.getComposerDraft(), 'proxy two');
  feature.enterDraft();
  assert.equal(chat.getComposerDraft(), '', 'a new local draft does not inherit proxy text');
  assert.deepEqual(Object.keys(sessionStates), []);
  assert.equal(remoteProxyDrafts.get('remote-local-1'), 'proxy one');
});

test('background failure restores only the originating proxy draft and soft-delete clears it', async () => {
  const { feature, chat, state, remoteProxyDrafts } = install(
    async id => ({ localSessionId: id, entries: [] }), async () => {},
  );
  state.remoteProxyHistories.push({ localSessionId: 'remote-local-2' });
  await feature.switchToSession('remote-local-1');
  chat.setComposerDraft('first draft');
  await feature.switchToSession('remote-local-2');
  chat.setComposerDraft('second draft');
  const owner = { sessionId: 'remote-local-1', restored: false };
  assert.equal(chat.restoreTaskDraft('failed text', owner), true);
  assert.equal(chat.getComposerDraft(), 'second draft');
  assert.equal(remoteProxyDrafts.get('remote-local-1'), 'first draft\nfailed text');
  await feature.switchToSession('remote-local-1');
  assert.equal(chat.getComposerDraft(), 'first draft\nfailed text');
  assert.equal(await feature.deleteSession('remote-local-1'), true);
  assert.equal(remoteProxyDrafts.has('remote-local-1'), false);
});

test('an unattempted pending task blocks soft-delete without dropping its draft or list entry', async () => {
  const { feature, chat, state, remoteProxyDrafts } = install(
    async id => ({ localSessionId: id, entries: [] }),
    async () => { throw Object.assign(new Error('blocked'), { code: 'remote_unattempted_pending_exists' }); },
  );
  await feature.switchToSession('remote-local-1');
  chat.setComposerDraft('still here');
  const errors = [];
  assert.equal(await feature.deleteSession('remote-local-1', code => errors.push(code)), false);
  assert.deepEqual(errors, ['remote_unattempted_pending_exists']);
  assert.equal(state.activeSessionId, 'remote-local-1');
  assert.equal(remoteProxyDrafts.get('remote-local-1'), 'still here');
  assert.equal(state.remoteProxyHistories.length, 1);
});
