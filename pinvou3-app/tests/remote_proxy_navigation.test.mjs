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

// Simple sessions-only fixtures explicitly stand in for a validated read.
// installRemoteHistory below uses the real remote bridge identity checker.
function install(read, remove, select, isCurrent = () => true) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = { window: root, console };
  vm.createContext(sandbox);
  // #566 共享模块移植：sessions/chat feature 工厂的共享函数体在 bridge-shared-helpers.js，
  // 必须先于工厂文件装载（与 index.html 脚本序一致）。
  const sharedHelpers = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)),
    '../src/shared/bridge-shared-helpers.js'), 'utf8');
  vm.runInContext(sharedHelpers, sandbox);
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
    isRemoteProxyReadCurrent: isCurrent,
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

function installRemoteHistory() {
  let remote;
  const requests = [];
  const harness = install(
    id => remote.readRemoteProxyHistory(id, { navigation: true }),
    id => remote.deleteRemoteProxyHistory(id),
    record => remote.selectRemoteProxyHistory(record),
    record => remote.isRemoteProxyReadCurrent(record),
  );
  for (const file of ['features/remote-control/remote-control-state.js',
    'platform/tauri/bridge/desktop-remote-control.js']) {
    vm.runInContext(fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)),
      '../src', file), 'utf8'), harness.sandbox);
  }
  harness.state.desktopRemoteControl = harness.root.__PINVOU_REMOTE_CONTROL_STATE__.createState();
  remote = harness.root.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
    state: harness.state, can: () => true, notify() {}, listen: async () => () => {},
    invoke(command, args) {
      if (command !== 'desktop_remote_proxy_history_read') return Promise.resolve([]);
      return new Promise((resolve, reject) => requests.push({ id: args.localSessionId, resolve, reject }));
    },
  });
  // The production bridge mirrors the domain list into the sessions routing hint.
  harness.state.desktopRemoteControl.remoteProxyHistories = [
    { localSessionId: 'remote-local-1' }, { localSessionId: 'remote-local-2' },
  ];
  harness.state.remoteProxyHistories = harness.state.desktopRemoteControl.remoteProxyHistories;
  function complete(index, title = 'verified', sequence = 1, metadata = {}) {
    assert.ok(requests[index], `read request ${index} exists`);
    const { journalEpoch = 'epoch-1', ...fields } = metadata;
    requests[index].resolve({ localSessionId: requests[index].id, title,
      // Rust validate_snapshot_window requires a contiguous window ending at
      // its authenticated cursor, including a tail entry for nonzero cursors.
      entries: Array.from({ length: sequence }, (_, index) => ({
        journalEpoch, sequence: index + 1, eventId: `${journalEpoch}-event-${index + 1}`,
        kind: 'assistant_message', body: `message-${index + 1}`, taskId: null, attachments: [],
      })),
      authenticatedCursor: { journalEpoch, sequence },
      pendingUnknownCount: 0, unattemptedPending: [], ...fields });
  }
  return { ...harness, remote, requests, complete };
}

for (const newer of ['navigation', 'refresh']) {
  test(`an older cross-kind reply cannot replace the published ${newer} snapshot`, async () => {
    const { feature, remote, state, complete } = installRemoteHistory();
    const initial = feature.switchToSession('remote-local-1');
    complete(0, 'initial', 1);
    assert.equal(await initial, true);
    let navigation, refresh;
    if (newer === 'refresh') {
      navigation = feature.switchToSession('remote-local-1');
      refresh = remote.readRemoteProxyHistory('remote-local-1');
    } else {
      refresh = remote.readRemoteProxyHistory('remote-local-1');
      navigation = feature.switchToSession('remote-local-1');
    }
    complete(2, 'newer', 3);
    await (newer === 'refresh' ? refresh : navigation);
    assert.equal(state.remoteProxyHistory.authenticatedCursor.sequence, 3);
    complete(1, 'older', 2);
    assert.equal(await navigation, true);
    assert.equal((await refresh).title, newer === 'refresh' ? 'newer' : 'older');
    assert.equal(state.remoteProxyHistory.title, 'newer');
    assert.equal(state.remoteProxyHistory.authenticatedCursor.sequence, 3);
  });
}

for (const fresherCompletesFirst of [true, false]) {
  test(`same-epoch cursor protects reads executing out of request order (${fresherCompletesFirst})`, async () => {
    const { feature, remote, state, complete } = installRemoteHistory();
    const initial = feature.switchToSession('remote-local-1');
    complete(0, 'initial', 1);
    assert.equal(await initial, true);
    // The earlier IPC invocation may acquire the Rust store lock later and
    // capture the fresher snapshot. Request order is not snapshot order.
    const refresh = remote.readRemoteProxyHistory('remote-local-1');
    const navigation = feature.switchToSession('remote-local-1');
    complete(fresherCompletesFirst ? 1 : 2, fresherCompletesFirst ? 'fresh' : 'old',
      fresherCompletesFirst ? 3 : 2);
    await (fresherCompletesFirst ? refresh : navigation);
    complete(fresherCompletesFirst ? 2 : 1, fresherCompletesFirst ? 'old' : 'fresh',
      fresherCompletesFirst ? 2 : 3);
    assert.equal(await navigation, true);
    assert.equal((await refresh).title, 'fresh');
    assert.equal(state.remoteProxyHistory.authenticatedCursor.sequence, 3);
    assert.equal(state.remoteProxyHistory.entries.at(-1).body, 'message-3');
  });
}

for (const earlier of ['navigation', 'refresh']) {
  for (const sequence of [0, 1]) {
    test(`initial null-cursor snapshot cannot replace authenticated history (${earlier}, cursor ${sequence})`, async () => {
    const { feature, remote, state, requests, complete } = installRemoteHistory();
    const initial = feature.switchToSession('remote-local-1');
    complete(0, 'history', 0, { authenticatedCursor: null });
    assert.equal(await initial, true);
    assert.equal(state.remoteProxyHistory.authenticatedCursor, null);
    const old = earlier === 'navigation' ? feature.switchToSession('remote-local-1')
      : remote.readRemoteProxyHistory('remote-local-1');
    const fresh = earlier === 'navigation' ? remote.readRemoteProxyHistory('remote-local-1')
      : feature.switchToSession('remote-local-1');
    assert.equal(requests.length, 3);
    complete(2, 'history', sequence);
    const freshResult = await fresh;
    if (earlier === 'navigation') assert.equal(freshResult.authenticatedCursor.sequence, sequence);
    else assert.equal(freshResult, true);
    const populated = state.remoteProxyHistory;
    assert.equal(populated.authenticatedCursor.sequence, sequence);
    complete(1, 'history', 0, { authenticatedCursor: null });
    const oldResult = await old;
    if (earlier === 'navigation') assert.equal(oldResult, true);
    else assert.equal(oldResult.authenticatedCursor, null);
    assert.equal(state.remoteProxyHistory, populated, 'initial snapshot does not publish');
    assert.equal(state.remoteProxyHistory.entries.length, sequence);
    if (sequence) assert.equal(state.remoteProxyHistory.entries[0].body, 'message-1');
    });
  }
}

test('a different proxy can still open its initial null-cursor history', async () => {
  const { feature, state, complete } = installRemoteHistory();
  const first = feature.switchToSession('remote-local-1');
  complete(0, 'first', 1);
  assert.equal(await first, true);
  const second = feature.switchToSession('remote-local-2');
  complete(1, 'second', 0, { authenticatedCursor: null });
  assert.equal(await second, true);
  assert.equal(state.activeSessionId, 'remote-local-2');
  assert.equal(state.remoteProxyHistory.localSessionId, 'remote-local-2');
  assert.equal(state.remoteProxyHistory.authenticatedCursor, null);
  assert.equal(state.remoteProxyHistory.entries.length, 0);
});

for (const kind of ['navigation', 'refresh']) {
  for (const crossEpoch of [false, true]) {
    test(`${kind} requested earlier can publish later metadata (${crossEpoch ? 'cross epoch' : 'equal cursor'})`, async () => {
      const { feature, remote, state, complete } = installRemoteHistory();
      const initial = feature.switchToSession('remote-local-1');
      complete(0, 'initial');
      assert.equal(await initial, true);
      // Two admitted IPC workers may acquire the store lock in reverse order.
      // Pending metadata can change without appending a journal entry.
      const first = kind === 'navigation' ? feature.switchToSession('remote-local-1')
        : remote.readRemoteProxyHistory('remote-local-1');
      const second = kind === 'navigation' ? remote.readRemoteProxyHistory('remote-local-1')
        : feature.switchToSession('remote-local-1');
      complete(2, 'earlier snapshot', 1, { pendingUnknownCount: 1 });
      await second;
      assert.equal(state.remoteProxyHistory.pendingUnknownCount, 1);
      complete(1, 'later snapshot', 1, {
        journalEpoch: crossEpoch ? 'epoch-2' : 'epoch-1', pendingUnknownCount: 0,
      });
      await first;
      assert.equal(state.remoteProxyHistory.pendingUnknownCount, 0);
      assert.equal(state.remoteProxyHistory.title, 'later snapshot');
      assert.equal(state.remoteProxyHistory.authenticatedCursor.journalEpoch,
        crossEpoch ? 'epoch-2' : 'epoch-1');
    });
  }
}

for (const first of ['old', 'new']) {
  test(`two different proxy navigations retain the latest selection (${first} first)`, async () => {
    const { feature, state, complete, requests, invokes } = installRemoteHistory();
    const old = feature.switchToSession('remote-local-1');
    const latest = feature.switchToSession('remote-local-2');
    assert.equal(requests.length, 2);
    assert.equal(state.activeSessionId, null);
    complete(first === 'old' ? 0 : 1, first);
    assert.equal(await (first === 'old' ? old : latest), first === 'new');
    assert.equal(state.activeSessionId, first === 'old' ? null : 'remote-local-2');
    assert.equal(state.remoteProxyHistory?.localSessionId ?? null,
      first === 'old' ? null : 'remote-local-2');
    complete(first === 'old' ? 1 : 0, first === 'old' ? 'new' : 'old');
    assert.equal(await old, false);
    assert.equal(await latest, true);
    assert.equal(state.activeSessionId, 'remote-local-2');
    assert.equal(state.remoteProxyHistory.title, 'new');
    assert.deepEqual(invokes, []);
  });
}

for (const first of ['view', 'navigation']) {
  test(`archive view and navigation have independent read identities (${first} first)`, async () => {
    const { feature, remote, state, requests, complete } = installRemoteHistory();
    const initial = feature.switchToSession('remote-local-1');
    complete(0, 'sealed history');
    assert.equal(await initial, true);
    await remote.archiveRemoteProxyHistory('remote-local-1');
    assert.equal(requests.length, 2, 'archive schedules one background read');
    complete(1, 'sealed history', 1, { archived: true });
    await Promise.resolve();
    // Production list refresh removes archived IDs, but sessions retains its
    // selected-proxy routing hint. Both reads now return an archived record.
    state.remoteProxyHistories = [];
    assert.equal(feature.isRemoteProxyView('remote-local-1'), true);
    const beforeView = state.remoteProxyHistory;
    const navigation = feature.switchToSession('remote-local-1');
    const view = remote.readRemoteProxyHistory('remote-local-1', { view: true });
    assert.equal(requests.length, 4);
    complete(first === 'view' ? 3 : 2, 'sealed history', 1, { archived: true });
    let navigationProjection;
    if (first === 'view') {
      assert.equal((await view).title, 'sealed history');
      assert.equal(state.activeSessionId, 'remote-local-1');
      assert.equal(state.remoteProxyHistory, beforeView, 'view does not publish');
    } else {
      assert.equal(await navigation, true);
      navigationProjection = state.remoteProxyHistory;
    }
    complete(first === 'view' ? 2 : 3, 'sealed history', 1, { archived: true });
    assert.equal(await navigation, true);
    assert.equal((await view).title, 'sealed history');
    assert.equal(state.remoteProxyHistory.title, 'sealed history');
    assert.equal(state.remoteProxyHistory.archived, true);
    if (first === 'navigation') assert.equal(state.remoteProxyHistory, navigationProjection,
      'late view leaves the selected projection object unchanged');
  });
}

test('a newer isolated view supersedes its own older read without publishing', async () => {
  const { remote, state, complete, requests } = installRemoteHistory();
  const old = remote.readRemoteProxyHistory('remote-local-1', { view: true });
  const latest = remote.readRemoteProxyHistory('remote-local-1', { view: true });
  assert.equal(requests.length, 2);
  complete(1, 'sealed history', 1, { archived: true });
  assert.equal((await latest).title, 'sealed history');
  complete(0, 'sealed history', 1, { archived: true });
  assert.equal(await old, null);
  assert.equal(state.remoteProxyHistory, null);
});

test('sessions fails closed when its read identity dependency is missing', async () => {
  const { feature, state } = install(async id => ({ localSessionId: id, entries: [] }),
    undefined, undefined, null);
  assert.equal(await feature.switchToSession('remote-local-1'), false);
  assert.equal(state.activeSessionId, null);
});

test('production sessions wiring forwards the remote identity check and view intent stays separate', () => {
  const facade = fs.readFileSync(new URL('../src/platform/tauri/bridge.js', import.meta.url), 'utf8');
  assert.match(facade, /isRemoteProxyReadCurrent:\s*function\s*\(record\)\s*\{\s*return desktopRemoteControlFeature\.isRemoteProxyReadCurrent\(record\);\s*\}/u);
  const panel = fs.readFileSync(new URL('../src/features/remote-control/RemoteProxyArchivePanel.jsx', import.meta.url), 'utf8');
  assert.match(panel, /readRemoteProxyHistory\(id,\s*\{\s*view:\s*true\s*\}\)/u);
});

for (const retire of ['stop', 'deleteRemoteProxyHistory', 'archiveRemoteProxyHistory']) {
  test(`controlled IPC: ${retire} between validation and adoption prevents selection`, async () => {
    const { feature, remote, state, requests, complete } = installRemoteHistory();
    const navigation = feature.switchToSession('remote-local-1');
    complete(0);
    await Promise.resolve();
    // This controlled IPC resolves mutations in the microtask drain. It is not
    // a claim that separate production IPC completions can interleave here.
    // stop is synchronous; retain the adoption guard as defensive wiring.
    const retirement = remote[retire]('remote-local-1');
    await retirement;
    assert.equal(await navigation, false);
    assert.equal(state.activeSessionId, null);
    assert.equal(state.remoteProxyHistory, null);
    for (let index = 1; index < requests.length; index += 1) complete(index, 'archive refresh');
  });
}

for (const first of ['navigation', 'refresh']) {
  test(`same-proxy navigation survives a concurrent refresh completing ${first} first`, async () => {
    const { feature, remote, state, complete, invokes } = installRemoteHistory();
    const navigation = feature.switchToSession('remote-local-1');
    const refresh = remote.readRemoteProxyHistory('remote-local-1');
    if (first === 'navigation') {
      complete(0);
      assert.equal(await navigation, true);
      complete(1);
    } else {
      complete(1);
      assert.equal((await refresh).localSessionId, 'remote-local-1');
      assert.equal(state.activeSessionId, null, 'refresh does not navigate');
      complete(0);
    }
    assert.equal(await navigation, true);
    assert.equal((await refresh).localSessionId, 'remote-local-1');
    assert.equal(state.activeSessionId, 'remote-local-1');
    assert.equal(state.remoteProxyHistory.localSessionId, 'remote-local-1');
    assert.deepEqual(invokes, [], 'no local session fallback');
  });
}

for (const first of ['old', 'new']) {
  test(`two same-proxy navigations retain latest intent when ${first} completes first`, async () => {
    const { feature, remote, state, complete } = installRemoteHistory();
    const old = feature.switchToSession('remote-local-1');
    const latest = feature.switchToSession('remote-local-1');
    const refresh = remote.readRemoteProxyHistory('remote-local-1');
    complete(2, 'background');
    assert.equal((await refresh).title, 'background');
    complete(first === 'old' ? 0 : 1, first);
    assert.equal(await (first === 'old' ? old : latest), first !== 'old');
    complete(first === 'old' ? 1 : 0, first === 'old' ? 'new' : 'old');
    assert.equal(await old, false);
    assert.equal(await latest, true);
    assert.equal(state.remoteProxyHistory.title, 'new');
  });
}

test('background refreshes still discard earlier replies and recover after a failed read', async () => {
  const { feature, remote, state, requests, complete } = installRemoteHistory();
  const navigation = feature.switchToSession('remote-local-1');
  complete(0);
  assert.equal(await navigation, true);
  const old = remote.readRemoteProxyHistory('remote-local-1');
  const latest = remote.readRemoteProxyHistory('remote-local-1');
  complete(2, 'latest');
  assert.equal((await latest).title, 'latest');
  complete(1, 'old');
  assert.equal(await old, null);
  assert.equal(state.remoteProxyHistory.title, 'latest');
  const failed = remote.readRemoteProxyHistory('remote-local-1');
  requests[3].reject(new Error('remote_proxy_not_found'));
  await assert.rejects(failed);
  const recovered = remote.readRemoteProxyHistory('remote-local-1');
  complete(4, 'recovered');
  assert.equal((await recovered).title, 'recovered');
  assert.equal(state.remoteProxyHistory.title, 'recovered');
});

for (const retire of ['stop', 'deleteRemoteProxyHistory', 'archiveRemoteProxyHistory']) {
  test(`${retire} invalidates all pending read kinds, including after a replacement read`, async () => {
    const { feature, remote, state, requests, complete } = installRemoteHistory();
    const navigation = feature.switchToSession('remote-local-1');
    const refresh = remote.readRemoteProxyHistory('remote-local-1');
    const view = remote.readRemoteProxyHistory('remote-local-1', { view: true });
    await remote[retire]('remote-local-1');
    // A new request for the same ID must not resurrect retired reply identities.
    // A successful delete remains hidden; archive returns its sealed record.
    const replacement = remote.readRemoteProxyHistory('remote-local-1', { navigation: true });
    const replacementIndex = requests.length - 1;
    complete(0, 'history');
    complete(1, 'history');
    complete(2, 'history');
    assert.equal(await navigation, false);
    assert.equal(await refresh, null);
    assert.equal(await view, null);
    assert.equal(state.activeSessionId, null);
    assert.equal(state.remoteProxyHistory, null);
    if (retire === 'deleteRemoteProxyHistory') {
      requests[replacementIndex].reject(new Error('remote_proxy_not_found'));
      await assert.rejects(replacement);
      const next = remote.readRemoteProxyHistory('remote-local-2');
      complete(requests.length - 1, 'other history');
      assert.equal((await next).localSessionId, 'remote-local-2');
    } else {
      complete(replacementIndex, 'history', 1, { archived: retire === 'archiveRemoteProxyHistory' });
      const record = await replacement;
      assert.equal(record.title, 'history');
      assert.equal(record.archived, retire === 'archiveRemoteProxyHistory');
    }
    // Archive schedules its own refresh. Settle it to leave no unresolved work.
    for (let index = 3; index < replacementIndex; index += 1) complete(index, 'history', 1, { archived: true });
  });
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
