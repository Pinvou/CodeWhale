// Load the complete classic bridge, not an injected can() or mocked state slice.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { abandonRemoteUpload, uploadRemoteFiles } from '../src/features/remote-control/remote-file-transfer.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const fixtures = JSON.parse(fs.readFileSync(path.join(root, 'tests/fixtures/desktop_remote_status.json'), 'utf8'));

function host(onSessionsPorts) {
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
  const scripts = [...html.matchAll(/<script src="%BASE_URL%(shared\/(?:bridge-messages|bridge-shared-helpers|chunked-file-upload)\.js|features\/remote-control\/remote-control-state\.js|platform\/tauri\/bridge(?:\.js|\/[^"]+))"[^>]*><\/script>/g)]
    .map(match => match[1]);
  assert.ok(scripts.includes('platform/tauri/bridge.js'));
  for (const script of scripts) {
    vm.runInContext(fs.readFileSync(path.join(root, 'src', script), 'utf8'), context, { filename: script });
    if (script === 'platform/tauri/bridge/sessions.js' && onSessionsPorts) {
      const factory = window.__PINVOU_TAURI_BRIDGE_FEATURES__.sessions;
      window.__PINVOU_TAURI_BRIDGE_FEATURES__.sessions = ports => {
        onSessionsPorts(ports);
        return factory(ports);
      };
    }
  }
  return { window, api: window.TauriBridge, respond(fn) { response = fn; } };
}

test('real public bridge forwards bounded user-question intents and retains unknown mutation identity', async () => {
  const h = host();
  const calls = [];
  let attempts = 0;
  h.respond(async (command, args) => {
    if (command === 'get_platform_capabilities') return { os: 'windows', desktopRemoteControlSupported: true };
    if (command === 'desktop_remote_controller_status') return { status: 'idle' };
    assert.equal(command, 'desktop_remote_action_a');
    calls.push(structuredClone(args));
    if (args.intent.operation === 'user_input_list') return { requests: [] };
    attempts += 1;
    if (attempts === 1) throw { code: 'result_unknown', requestId: args.requestId };
    if (attempts === 2) throw { code: 'remote_action_failed', requestId: args.requestId };
    return { state: 'accepted' };
  });
  await h.api.platform.loadPlatformCapabilities();
  await h.api.desktopRemoteControl.userInputList('local', 'ref', 'task-1');
  await h.api.desktopRemoteControl.userInputList('local', 'ref', 'task-1');
  assert.notEqual(calls[0].requestId, calls[1].requestId, 'each read observes a fresh snapshot');
  const answers = [{ question_id: '0', option_ids: ['0', '1'] }];
  await assert.rejects(h.api.desktopRemoteControl.userInputRespond('local', 'ref', 'task-1', 'input-1', false, answers),
    failure => failure.resultUnknown === true);
  await h.api.desktopRemoteControl.userInputRespond('local', 'ref', 'task-1', 'input-1', false, answers);
  assert.equal(calls[2].requestId, calls[3].requestId);
  assert.equal(calls[3].requestId, calls[4].requestId, 'retry after a different transport failure recovers the original command');
  const { requestId, ...payload } = calls[4];
  assert.ok(requestId);
  assert.deepEqual(payload, { localSessionId: 'local', remoteRef: 'ref', intent: {
    operation: 'user_input_respond', taskId: 'task-1', inputId: 'input-1', cancel: false, answers,
  } });
});

test('unknown question reads release correlation capacity and cannot exhaust answer admission', async () => {
  const h = host(); const reads = new Set(); let mutation = 0;
  h.respond(async (command, args) => {
    if (command === 'get_platform_capabilities') return { os: 'windows', desktopRemoteControlSupported: true };
    if (command === 'desktop_remote_controller_status') return { status: 'idle' };
    assert.equal(command, 'desktop_remote_action_a');
    if (args.intent.operation === 'user_input_list') {
      reads.add(args.requestId);
      throw { code: 'result_unknown', requestId: args.requestId };
    }
    mutation += 1; return { state: 'accepted' };
  });
  await h.api.platform.loadPlatformCapabilities();
  for (let i = 0; i < 140; i += 1) await assert.rejects(h.api.desktopRemoteControl.userInputList('local', 'ref', 'task-1'));
  assert.equal(reads.size, 140);
  await h.api.desktopRemoteControl.userInputRespond('local', 'ref', 'task-1', 'input-1', true, []);
  assert.equal(mutation, 1);
  await assert.rejects(h.api.desktopRemoteControl.userInputRespond('local', 'ref', 'task-1', 'bad/id', true, []));
  assert.equal(mutation, 1, 'invalid IDs never reach native effects');
});

test('production sessions facade keeps navigation distinct from background and view reads', async () => {
  let sessionsPorts;
  const h = host(ports => { sessionsPorts = ports; });
  const reads = [];
  h.respond((command, args) => {
    if (command === 'get_platform_capabilities') {
      return Promise.resolve({ os: 'windows', desktopRemoteControlSupported: true });
    }
    if (command === 'desktop_remote_proxy_history_read') {
      return new Promise(resolve => reads.push({ id: args.localSessionId, resolve }));
    }
    return Promise.resolve(null);
  });
  await h.api.platform.loadPlatformCapabilities();
  assert.ok(sessionsPorts, 'the actual facade installs the sessions feature');
  const navigation = sessionsPorts.readRemoteProxyHistory('remote-local-1');
  const background = h.api.desktopRemoteControl.readRemoteProxyHistory('remote-local-1');
  const view = h.api.desktopRemoteControl.readRemoteProxyHistory('remote-local-1', { view: true });
  assert.equal(reads.length, 3);
  const complete = index => reads[index].resolve({
    localSessionId: reads[index].id, title: '', entries: [], authenticatedCursor: null,
    pendingUnknownCount: 0, unattemptedPending: [],
  });
  complete(1);
  complete(2);
  assert.equal((await background)?.localSessionId, 'remote-local-1');
  assert.equal((await view)?.localSessionId, 'remote-local-1');
  complete(0);
  const record = await navigation;
  assert.equal(record?.localSessionId, 'remote-local-1', 'the facade must use the distinct navigation intent');
  assert.equal(record.title, '');
  assert.equal(sessionsPorts.isRemoteProxyReadCurrent(record), true);
});

test('real backend capability projection follows loaded host support and fails closed on errors', async () => {
  const h = host();
  const { can } = await import('../src/shared/platform.js');
  const previous = globalThis.window;
  globalThis.window = h.window;
  try {
    assert.equal(h.api.desktopRemoteControl.isAvailable(), false);
    assert.equal(can('desktopRemoteControl'), false);
    for (const [os, supported] of [['windows', true], ['macos', false], ['linux', true], ['unknown', false]]) {
      h.respond(async () => ({ os, desktopRemoteControlSupported: supported }));
      await h.api.platform.loadPlatformCapabilities();
      assert.equal(h.api.desktopRemoteControl.isAvailable(), supported);
      assert.equal(can('desktopRemoteControl'), supported);
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
  assert.ok(Array.isArray(current.controlledTasks));
  assert.equal(snapshots.at(-1).controllerConnection.connectionId, 'connection-0001');
  assert.ok(Object.isFrozen(snapshots.at(-1)));
  const stable = snapshots.at(-1);
  h.api.chat.prefillComposer('unrelated');
  assert.equal(snapshots.at(-1), stable, 'unrelated publications preserve projection identity');
  off();
});

test('obsolete unauthenticated native entries stay closed after authenticated A08 wiring', () => {
  const h = host();
  for (const name of ['historyList', 'historyLoad', 'historyDelete', 'historyMarkOffline',
    'openRemoteSession', 'switchRemoteSession', 'submitRemoteTask']) {
    assert.equal(name in h.api.desktopRemoteControl, false, name);
  }
  const native = fs.readFileSync(path.join(root, 'src-tauri/src/lib.rs'), 'utf8');
  assert.doesNotMatch(native, /commands::desktop_remote_(?:model::desktop_remote_model_(?:install|release|active)|permission::desktop_remote_permission_(?:request|decide|list)|history::desktop_remote_history_(?:list|load|delete|create|mark_offline|apply_event|apply_snapshot)),/);
  assert.doesNotMatch(native, /commands::desktop_remote_(?:attachment::desktop_remote_attachment_(?:begin|chunk|abort)|artifact::desktop_remote_artifact_(?:index|preview|download_begin|download_chunk|download_abort)|task::desktop_remote_task_(?:submit|open_session|switch_session)),/);
  assert.match(native, /commands::desktop_remote_action::desktop_remote_action_a,/);
  assert.match(native, /commands::desktop_remote_permission::desktop_remote_permission_list_b,/);
  const commands = path.join(root, 'src-tauri/src/app/commands');
  for (const file of ['desktop_remote_attachment.rs', 'desktop_remote_artifact.rs', 'desktop_remote_task.rs']) {
    assert.doesNotMatch(fs.readFileSync(path.join(commands, file), 'utf8'), /pub\s+(?:async\s+)?fn\s+desktop_remote_(?:attachment_(?:begin|chunk|abort)|artifact_(?:index|preview|download_begin|download_chunk|download_abort)|task_(?:submit|open_session|switch_session))\b/);
  }
  const controlled = fs.readFileSync(path.join(root, 'src-tauri/src/features/remote_control/controller/controlled.rs'), 'utf8');
  assert.doesNotMatch(controlled, /\bwith_live_lease\b/, 'no manager lock is held across synchronous transfer I/O');
  const bridge = fs.readFileSync(path.join(root, 'src/platform/tauri/bridge/desktop-remote-control.js'), 'utf8');
  assert.doesNotMatch(bridge, /permissionRequest:/, 'no unbound self-approval placeholder is submitted');
  assert.doesNotMatch(bridge, /invoke\("desktop_remote_(?:attachment|artifact|permission_decide|permission_request)/);
  const model = fs.readFileSync(path.join(commands, 'desktop_remote_model.rs'), 'utf8');
  assert.match(model, /let credentials = SystemCredentialStore::new\(\);/);
  assert.match(model, /prepare_export_models\(&prefs, model\.clone\(\), &credentials\)/);
  assert.doesNotMatch(model, /pub fn desktop_remote_model_(?:install|release|active)\b/);
});


test('public attachment cleanup confirmation forwards exact identity and preserves receipts independently of logical abort', async () => {
  const h = host();
  const calls = [];
  let receipt;
  h.respond(async (command, args) => {
    if (command === 'get_platform_capabilities') return { os: 'windows', desktopRemoteControlSupported: true };
    // Capability bootstrap performs this read before any attachment action.
    if (command === 'desktop_remote_controller_status') return { status: 'idle' };
    calls.push({ command, args: structuredClone(args) });
    return receipt;
  });
  await h.api.platform.loadPlatformCapabilities();
  const api = h.api.desktopRemoteControl;
  const ids = ['local-cleanup', 'ref-cleanup', 'task-cleanup', 'transfer-cleanup'];
  for (const proof of [
    { transfer_id: ids[3], cleanup_confirmed: true },
    { transfer_id: ids[3], cleanup_confirmed: false },
    { transfer_id: 'another-transfer', cleanup_confirmed: true },
  ]) {
    receipt = proof;
    assert.equal(await api.attachmentAbortConfirmed(...ids), proof, 'the facade preserves the native receipt without inventing proof');
    const { requestId, ...payload } = calls.at(-1).args;
    assert.equal(calls.at(-1).command, 'desktop_remote_action_a');
    assert.ok(typeof requestId === 'string' && requestId.length > 0);
    assert.deepEqual(payload, { localSessionId: ids[0], remoteRef: ids[1],
      intent: { operation: 'attachment_abort_confirmed', taskId: ids[2], transferId: ids[3] } });
  }
  receipt = { aborted: true };
  assert.equal(await api.attachmentAbort(...ids), receipt);
  assert.equal(calls.at(-1).args.intent.operation, 'attachment_abort');
  assert.equal(calls.length, 4);
});

test('public cleanup confirmation rejects invalid identities and unavailable capabilities before native effects', async () => {
  const h = host();
  const calls = [];
  let capabilities = { os: 'windows', desktopRemoteControlSupported: true };
  h.respond(async (command, args) => {
    if (command === 'get_platform_capabilities') return capabilities;
    // Capability bootstrap performs this read before any attachment action.
    if (command === 'desktop_remote_controller_status') return { status: 'idle' };
    calls.push({ command, args });
    return null;
  });
  const api = h.api.desktopRemoteControl;
  const ids = ['local-validation', 'ref-validation', 'task-validation', 'transfer-validation'];
  await assert.rejects(async () => api.attachmentAbortConfirmed(...ids), { code: 'desktop_remote_control_unsupported' });
  await h.api.platform.loadPlatformCapabilities();
  for (let index = 0; index < ids.length; index += 1) {
    for (const invalid of ['', 'invalid identity', 'x'.repeat(129)]) {
      const input = ids.slice();
      input[index] = invalid;
      await assert.rejects(async () => api.attachmentAbortConfirmed(...input), { code: 'remote_task_intent_invalid' });
    }
  }
  for (const unsupported of [null, { os: 'windows', desktopRemoteControlSupported: false }]) {
    capabilities = unsupported;
    await h.api.platform.loadPlatformCapabilities();
    await assert.rejects(async () => api.attachmentAbortConfirmed(...ids), { code: 'desktop_remote_control_unsupported' });
  }
  assert.deepEqual(calls, []);
});

test('public cleanup confirmation preserves rejection and unknown-outcome admission for the following call', async () => {
  const h = host();
  const calls = [];
  let failure = Object.assign(new Error('transfer_staging_cleanup_failed'), { authoritative: true });
  const receipt = { transfer_id: 'transfer-errors', cleanup_confirmed: true };
  h.respond(async (command, args) => {
    if (command === 'get_platform_capabilities') return { os: 'windows', desktopRemoteControlSupported: true };
    // Capability bootstrap performs this read before any attachment action.
    if (command === 'desktop_remote_controller_status') return { status: 'idle' };
    calls.push({ command, args: structuredClone(args) });
    if (failure) throw failure;
    return receipt;
  });
  await h.api.platform.loadPlatformCapabilities();
  const api = h.api.desktopRemoteControl;
  const ids = ['local-errors', 'ref-errors', 'task-errors', 'transfer-errors'];
  await assert.rejects(api.attachmentAbortConfirmed(...ids), error =>
    error.code === 'transfer_staging_cleanup_failed' && error.authoritative === true && typeof error.requestId === 'string');
  assert.equal(calls.length, 1, 'confirmed cleanup never automatically retries');
  failure = null;
  assert.equal(await api.attachmentAbortConfirmed(...ids), receipt);
  assert.equal(calls.length, 2, 'an authoritative rejection does not block a subsequent explicit call');
  failure = new Error('result_unknown');
  const unknownIds = [...ids.slice(0, 3), 'transfer-unknown'];
  await assert.rejects(api.attachmentAbortConfirmed(...unknownIds), { code: 'result_unknown' });
  await assert.rejects(api.attachmentAbortConfirmed(...unknownIds), { code: 'result_unknown' });
  assert.equal(calls.length, 3, 'a retained unknown result forbids an accidental resend');
  failure = null;
  assert.equal(await api.attachmentAbortConfirmed(...ids), receipt);
  assert.equal(calls.length, 4, 'the unrelated known operation remains usable');
});

test('real upload helper waits for public facade physical proof before local abandon and a subsequent upload', async () => {
  const h = host();
  const calls = [];
  const binding = { localSessionId: 'local-helper', remoteRef: 'ref-helper' };
  const handle = { taskId: 'task-helper', clientRequestId: 'request-helper' };
  let current = true;
  let firstBegin = true;
  let resolveProof;
  let markCleanupEntered;
  const proof = new Promise(resolve => { resolveProof = resolve; });
  const cleanupEntered = new Promise(resolve => { markCleanupEntered = resolve; });
  h.respond(async (command, args) => {
    if (command === 'get_platform_capabilities') return { os: 'windows', desktopRemoteControlSupported: true };
    // Capability bootstrap performs this read before any attachment action.
    if (command === 'desktop_remote_controller_status') return { status: 'idle' };
    calls.push({ command, args: structuredClone(args) });
    if (command === 'desktop_remote_task_abandon_prepared_a') return { abandoned: true };
    assert.equal(command, 'desktop_remote_action_a');
    switch (args.intent.operation) {
      case 'attachment_begin':
        if (firstBegin) { firstBegin = false; current = false; }
        return { transfer_id: 'transfer-helper', chunk_bytes: 1, total_bytes: 1 };
      case 'attachment_chunk': return { state: 'completed', received_bytes: 1 };
      case 'attachment_abort_confirmed': markCleanupEntered(); return proof;
      default: assert.fail(`unexpected action: ${args.intent.operation}`);
    }
  });
  await h.api.platform.loadPlatformCapabilities();
  const api = h.api.desktopRemoteControl;
  const file = { name: 'a.txt', type: 'text/plain', size: 1, arrayBuffer: async () => new Uint8Array([97]).buffer };
  const options = { ownerIdentity: 'owner-public-helper', leaseIdentity: 'lease-public-helper', isCurrent: () => current };
  const upload = task => uploadRemoteFiles(api, binding, task, [file], () => {}, options);
  await assert.rejects(upload(handle.taskId), { code: 'result_unknown' });
  current = true;
  await assert.rejects(upload('task-next'), { code: 'remote_attachment_fenced' });
  const abandoning = abandonRemoteUpload(api, options.ownerIdentity, binding, handle, () => current);
  await cleanupEntered;
  const cleanup = calls.at(-1);
  assert.deepEqual(cleanup.args.intent, { operation: 'attachment_abort_confirmed', taskId: handle.taskId, transferId: 'transfer-helper' });
  assert.equal(cleanup.args.localSessionId, binding.localSessionId);
  assert.equal(cleanup.args.remoteRef, binding.remoteRef);
  assert.equal(calls.some(call => call.command === 'desktop_remote_task_abandon_prepared_a'), false);
  resolveProof({ transfer_id: 'transfer-helper', cleanup_confirmed: true });
  assert.equal(await abandoning, true);
  const abandons = calls.filter(call => call.command === 'desktop_remote_task_abandon_prepared_a');
  assert.deepEqual(abandons.map(call => call.args), [{ localSessionId: binding.localSessionId, clientRequestId: handle.clientRequestId, taskId: handle.taskId }]);
  assert.deepEqual(await upload('task-next'), ['transfer-helper']);
});
