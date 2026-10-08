// Load the complete classic bridge, not an injected can() or mocked state slice.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const fixtures = JSON.parse(fs.readFileSync(path.join(root, 'tests/fixtures/desktop_remote_status.json'), 'utf8'));

function host() {
  let response = async () => null;
  const localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  const window = {
    __TAURI__: {
      core: { invoke: (...args) => response(...args) },
      event: { listen: async () => () => {} },
      dialog: { open: async () => null },
    },
    performance: { now: () => 0 }, addEventListener() {}, localStorage,
    setTimeout, clearTimeout,
  };
  const context = vm.createContext({
    window, document: { readyState: 'loading', addEventListener() {}, createElement: () => ({ style: {} }), body: { appendChild() {} } },
    navigator: {}, console: { ...console, warn() {} }, localStorage,
    setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {}, structuredClone,
  });
  const html = fs.readFileSync(path.join(root, 'src/index.html'), 'utf8');
  const scripts = [...html.matchAll(/<script src="%BASE_URL%(shared\/bridge-messages\.js|features\/remote-control\/remote-control-state\.js|platform\/tauri\/bridge(?:\.js|\/[^"]+))"[^>]*><\/script>/g)]
    .map(match => match[1]);
  assert.ok(scripts.includes('platform/tauri/bridge.js'));
  for (const script of scripts) vm.runInContext(fs.readFileSync(path.join(root, 'src', script), 'utf8'), context, { filename: script });
  return { window, api: window.TauriBridge, respond(fn) { response = fn; } };
}

test('real backend capability projection fails closed until Windows support is loaded', async () => {
  const h = host();
  const { can } = await import('../src/shared/platform.js');
  const previous = globalThis.window;
  globalThis.window = h.window;
  try {
    assert.equal(h.api.desktopRemoteControl.isAvailable(), false);
    assert.equal(can('desktopRemoteControl'), false);
    for (const os of ['windows', 'macos', 'linux', 'unknown']) {
      h.respond(async () => ({ os, desktopRemoteControlSupported: true }));
      await h.api.platform.loadPlatformCapabilities();
      assert.equal(h.api.desktopRemoteControl.isAvailable(), os === 'windows');
      assert.equal(can('desktopRemoteControl'), os === 'windows');
    }
    h.respond(async () => ({ os: 'windows', desktopRemoteControlSupported: true }));
    await h.api.platform.loadPlatformCapabilities();
    h.respond(async () => { throw new Error('backend unavailable'); });
    await h.api.platform.loadPlatformCapabilities();
    assert.equal(h.api.desktopRemoteControl.isAvailable(), false, 'failed reload must revoke stale support');
    assert.equal(can('desktopRemoteControl'), false);
    h.respond(async () => ({ os: 'windows', desktopRemoteControlSupported: false }));
    await h.api.platform.loadPlatformCapabilities();
    assert.equal(can('desktopRemoteControl'), false);
  } finally {
    if (previous === undefined) delete globalThis.window; else globalThis.window = previous;
  }
});

test('real getMany and subscribeMany expose the nested remote-control domain', async () => {
  const h = host();
  const snapshots = [];
  const off = h.api.state.subscribeMany(['desktopRemoteControl'], snapshot => snapshots.push(snapshot));
  h.respond(async command => command === 'get_platform_capabilities'
    ? { os: 'windows', desktopRemoteControlSupported: true }
    : fixtures.controllerConnected);
  await h.api.platform.loadPlatformCapabilities();
  await h.api.desktopRemoteControl.controllerConnect('2345678');
  const current = h.api.state.getMany(['desktopRemoteControl']);
  assert.equal(current.controllerConnection.connectionId, 'connection-0001');
  assert.equal(current.controllerConnection.status, 'connected');
  assert.ok(Array.isArray(current.tasks));
  assert.equal(snapshots.at(-1).controllerConnection.connectionId, 'connection-0001');
  assert.ok(Object.isFrozen(snapshots.at(-1)));
  const stable = snapshots.at(-1);
  h.api.chat.prefillComposer('unrelated');
  assert.equal(snapshots.at(-1), stable, 'unrelated publications preserve projection identity');
  off();
});

test('unfinished sensitive renderer entries remain unregistered until authenticated wiring', () => {
  const h = host();
  for (const name of ['capabilitiesSnapshot', 'listExportableModels', 'permissionList', 'permissionDecide', 'permissionDenyTask',
    'historyList', 'historyLoad', 'historyDelete', 'historyMarkOffline',
    'artifactDownloadAbort', 'artifactDownloadBegin', 'artifactDownloadChunk', 'artifactIndex', 'artifactPreview',
    'attachmentAbort', 'attachmentBegin', 'attachmentChunk',
    'openRemoteSession', 'switchRemoteSession', 'submitRemoteTask']) {
    assert.equal(name in h.api.desktopRemoteControl, false, name);
  }
  const native = fs.readFileSync(path.join(root, 'src-tauri/src/lib.rs'), 'utf8');
  assert.doesNotMatch(native, /commands::desktop_remote_(?:model::desktop_remote_model_(?:list_exportable|install|release|active)|permission::desktop_remote_permission_(?:request|decide|list|deny_task)|history::desktop_remote_history_(?:list|load|delete|create|mark_offline|apply_event|apply_snapshot)),/);
  assert.doesNotMatch(native, /commands::desktop_remote_(?:attachment::desktop_remote_attachment_(?:begin|chunk|abort)|artifact::desktop_remote_artifact_(?:index|preview|download_begin|download_chunk|download_abort)|task::desktop_remote_task_(?:submit|open_session|switch_session)),/);
  const commands = path.join(root, 'src-tauri/src/app/commands');
  for (const file of ['desktop_remote_attachment.rs', 'desktop_remote_artifact.rs', 'desktop_remote_task.rs']) {
    assert.doesNotMatch(fs.readFileSync(path.join(commands, file), 'utf8'), /pub\s+(?:async\s+)?fn\s+desktop_remote_(?:attachment_(?:begin|chunk|abort)|artifact_(?:index|preview|download_begin|download_chunk|download_abort)|task_(?:submit|open_session|switch_session))\b/);
  }
  const controlled = fs.readFileSync(path.join(root, 'src-tauri/src/features/remote_control/controller/controlled.rs'), 'utf8');
  assert.doesNotMatch(controlled, /\bwith_live_lease\b/, 'no manager lock is held across synchronous transfer I/O');
  const bridge = fs.readFileSync(path.join(root, 'src/platform/tauri/bridge/desktop-remote-control.js'), 'utf8');
  assert.doesNotMatch(bridge, /permissionRequest:|selectedModelId:/, 'unconsumed placeholders are not submitted');
});
