import assert from 'node:assert/strict';
import test from 'node:test';
import vm from 'node:vm';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = relative => fs.readFileSync(path.join(appRoot, relative), 'utf8');
const knownRow = { path: '/public/retained.md', name: 'retained.md', mtime: 1, size: 5, sessionId: 'public-session', source: 'Public session' };

// Execute the current component's actual callback without copying its decision
// logic or requiring a DOM/React renderer. The registered production bridge is
// passed unchanged; only the native transport and React state setters are controlled.
function knowledgeRefresh(bridge, previousRows = [knownRow]) {
  const source = read('src/features/knowledge/KnowledgeView.jsx');
  const marker = 'const refreshOutputs = useCallback(async () => {';
  const markerAt = source.indexOf(marker);
  const end = source.indexOf('}, []);', markerAt + marker.length);
  assert.ok(markerAt >= 0 && end > markerAt, 'actual Knowledge refresh callback must exist');
  const sigLine = source.split('\n').find(line => line.includes('const outputListSig ='));
  assert.ok(sigLine, 'actual Knowledge row signature must exist');
  const result = { rows: previousRows, skipped: [], scanError: false, loaded: false };
  const context = vm.createContext({
    bridge, kbCache: { outputs: previousRows, outputsSkipped: [] },
    outputsDisposedRef: { current: false }, outputsSigRef: { current: '' }, outputsSkippedSigRef: { current: '' },
    invokeTauri() { throw new Error('registered facade must own this invocation'); },
    setOutputs: rows => { result.rows = rows; },
    setOutputsSkipped: skipped => { result.skipped = skipped; },
    setOutputsScanError: error => { result.scanError = error; },
    setOutputsLoaded: loaded => { result.loaded = loaded; },
  });
  vm.runInContext(sigLine + '\noutputsSigRef.current = outputListSig(kbCache.outputs);\nglobalThis.refresh = async () => {' + source.slice(markerAt + marker.length, end) + '};', context);
  return { result, refresh: context.refresh };
}

function bootDesktop(invoke, artifacts = []) {
  const window = {};
  const context = vm.createContext({ window, console });
  vm.runInContext(read('src/shared/bridge-shared-helpers.js'), context);
  vm.runInContext(read('src/platform/tauri/bridge/artifacts.js'), context);
  const api = window.__PINVOU_TAURI_BRIDGE_FEATURES__.artifacts({
    state: { activeSessionId: 'public-live', artifacts, sessions: [{ id: 'public-live', title: 'Public live' }], attachments: [] },
    sessionStates: {}, invoke, notify() {}, bt: key => key, addSystemItem() {},
    basename: value => String(value).split('/').pop(),
    isDeliverable: value => /\.(md|pdf)$/.test(value), isAbsPath: value => String(value).startsWith('/'),
  });
  return { artifacts: api };
}

// Load both actual Web classic scripts and their real domain facade. The
// existing canInvoke predicate reads the controlled negotiated command set;
// this is not a hand-written replacement of shared or facade methods.
function bootWeb(invoke, commands) {
  const storage = new Map();
  const localStorage = { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, String(value)), removeItem: key => storage.delete(key) };
  const window = {
    PinvouPlatform: { kind: 'web', isWeb: true, capabilities: {}, can: () => false, canInvoke: command => commands.includes(command), areInvokeCapabilitiesReady: () => true },
    __TAURI__: { core: { invoke }, event: { listen: async () => () => {} }, dialog: { open: async () => null } },
    location: { search: '', href: 'https://example.test/pinvou3/remote/' }, localStorage,
    crypto: { randomUUID: () => '00000000-0000-4000-8000-000000000000' }, performance: { now: () => 0 },
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {}, setTimeout, clearTimeout,
  };
  const context = vm.createContext({
    window, document: { readyState: 'loading', addEventListener() {}, createElement: () => ({ style: {}, click() {}, remove() {}, setAttribute() {} }), body: { appendChild() {} } },
    navigator: { mediaDevices: null }, localStorage, console, setTimeout, clearTimeout, setInterval, clearInterval,
    structuredClone, URL, URLSearchParams, Blob, Uint8Array, ArrayBuffer, TextEncoder, TextDecoder,
  });
  for (const file of ['src/shared/bridge-shared-helpers.js', 'src/shared/bridge-messages.js', 'src/shared/tool-store-events.js', 'src/platform/web/bridge.js', 'src/platform/web/bridge/domain-adapter.js']) {
    vm.runInContext(read(file), context, { filename: file });
  }
  assert.equal(typeof window.TauriBridge.artifacts.listDeliverableIndex, 'function');
  return window.TauriBridge;
}

test('desktop registered facade preserves partial v2 disclosure and disk/live hydration', async () => {
  const calls = [];
  const disk = { ...knownRow, path: '/public/disk.md', name: 'disk.md', mtime: 3 };
  const bridge = bootDesktop(async (command, args) => {
    calls.push([command, args]);
    if (command === 'list_deliverable_index_v2') return { rows: [disk], skipped: ['s-over-cap'] };
    if (command === 'artifact_info') return { modified: 2, size: 7 };
    throw new Error('unexpected native command: ' + command);
  }, [{ path: '/public/live.md' }, { path: '/public/disk.md' }]);
  const view = knowledgeRefresh(bridge);
  await view.refresh();
  assert.deepEqual(Array.from(view.result.skipped), ['s-over-cap']);
  assert.deepEqual(Array.from(view.result.rows, row => row.path), ['/public/disk.md', '/public/live.md']);
  assert.equal(view.result.rows[1].sessionId, 'public-live');
  assert.equal(view.result.rows[1].mtime, 2);
  assert.equal(view.result.rows[1].size, 7);
  assert.equal(view.result.scanError, false);
  assert.deepEqual(calls.map(([command]) => command), ['list_deliverable_index_v2', 'artifact_info']);
});

test('desktop failed v2 scan reaches actual Knowledge retry state without erasing successful rows', async () => {
  const calls = [];
  const view = knowledgeRefresh(bootDesktop(async command => { calls.push(command); throw new Error('controlled scanner root unavailable'); }));
  await view.refresh();
  assert.equal(view.result.scanError, true);
  assert.equal(view.result.loaded, true);
  assert.deepEqual(view.result.rows, [knownRow]);
  assert.deepEqual(calls, ['list_deliverable_index_v2']);
});

test('desktop successful empty v2 scan remains distinct from failure', async () => {
  const calls = [];
  const view = knowledgeRefresh(bootDesktop(async command => {
    calls.push(command);
    return { rows: [], skipped: [] };
  }));
  await view.refresh();
  assert.equal(view.result.scanError, false);
  assert.equal(view.result.rows.length, 0);
  assert.equal(view.result.skipped.length, 0);
  assert.deepEqual(calls, ['list_deliverable_index_v2']);
});

test('actual Web facade negotiates legacy array only for an older desktop', async () => {
  const calls = [];
  const view = knowledgeRefresh(bootWeb(async command => {
    calls.push(command);
    assert.equal(command, 'list_deliverable_index');
    return [knownRow];
  }, ['list_deliverable_index']));
  await view.refresh();
  assert.deepEqual(calls, ['list_deliverable_index']);
  assert.equal(view.result.rows.length, 1);
  assert.equal(view.result.rows[0].path, knownRow.path);
  assert.equal(view.result.skipped.length, 0);
  assert.equal(view.result.scanError, false);
});

test('actual Web facade carries v2 partial disclosure from a current desktop', async () => {
  const calls = [];
  const view = knowledgeRefresh(bootWeb(async command => {
    calls.push(command);
    assert.equal(command, 'list_deliverable_index_v2');
    return { rows: [knownRow], skipped: ['s-unreadable'] };
  }, ['list_deliverable_index', 'list_deliverable_index_v2']));
  await view.refresh();
  assert.deepEqual(calls, ['list_deliverable_index_v2']);
  assert.deepEqual(Array.from(view.result.skipped), ['s-unreadable']);
  assert.equal(view.result.rows.length, 1);
  assert.equal(view.result.scanError, false);
});

test('actual Web failed v2 scan does not retry legacy or hide total failure', async () => {
  const calls = [];
  const view = knowledgeRefresh(bootWeb(async command => { calls.push(command); throw new Error('controlled scanner unavailable'); }, ['list_deliverable_index', 'list_deliverable_index_v2']));
  await view.refresh();
  assert.deepEqual(calls, ['list_deliverable_index_v2']);
  assert.equal(view.result.scanError, true);
  assert.deepEqual(view.result.rows, [knownRow]);
});
