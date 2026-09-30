// E2E-07 contract: both desktop surfaces are driven by the A/B state machines —
// the B panel maps the real backend statuses (so the pairing code is visible),
// and the A conversation view renders the authoritative transcript while never
// assembling protocol fields itself.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';
import { controlledDeviceUiState } from '../src/features/remote-control/remote-control-logic.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const logicPath = path.join(root, 'src', 'features', 'remote-control', 'remote-control-state.js');
const context = { window: {} };
context.globalThis = context;
vm.createContext(context);
vm.runInContext(fs.readFileSync(logicPath, 'utf8'), context, { filename: logicPath });
const logic = context.window.__PINVOU_REMOTE_CONTROL_STATE__;
const read = (relative) => fs.readFileSync(path.join(root, 'src', relative), 'utf8');

test('the B panel maps the backend status enum and shows the pairing code', () => {
  const panel = read('features/remote-control/ControlledDevicePanel.jsx');
  const cases = [
    ['registering', 'registering'],
    ['waiting_for_pairing', 'waiting_for_pairing'],
    ['pairing_reserved', 'pairing_reserved'],
    ['handshake_pending', 'handshake_pending'],
    ['connected', 'connected'],
    ['reconnecting', 'reconnecting'],
    ['resume_provisional', 'resume_provisional'],
    ['resume_activated_pending_liveness', 'resume_activated_pending_liveness'],
    ['retiring_generation', 'retiring_generation'],
    ['stopping', 'stopping'],
    ['stopped', 'stopped'],
    ['error', 'error'],
    ['disabled', 'off'],
  ];
  for (const [status, expected] of cases) {
    const device = status === 'waiting_for_pairing' ? { status, pairingCode: '2345678' } : { status };
    assert.equal(controlledDeviceUiState(device, 120), expected, `backend status ${status}`);
  }
  assert.ok(panel.includes('codeVisible'), 'pairing code must be state-driven');
  assert.ok(panel.includes("['waiting_for_pairing', 'expiring']"), 'expired pairing codes must not remain visible');
  assert.ok(
    panel.includes('controllerDeviceName'),
    'the 主控设备 row must use the controller device name',
  );
  assert.ok(!panel.includes("onLocalStateChange?.('reserved')"), 'reservation cannot be invented by a UI timer');
  assert.ok(panel.includes('deviceUnknown'), 'unknown names fall back to i18n copy');
  assert.ok(!/device\.deviceName \|\| copy\.deviceUnknown/.test(panel));
});

test('the A shell is connection-only; ChatView consumes the RemoteProxy projection', () => {
  const shell = read('features/remote-control/RemoteConnectionShell.jsx');
  const chat = read('features/chat/ChatView.jsx');
  assert.ok(!shell.includes('RemoteConversationView'));
  assert.ok(!/transcript|remoteHistories|composer/.test(shell));
  assert.ok(chat.includes('remoteProxyChatItems(activeSessionId, remoteProxyHistory)'));
  assert.ok(chat.includes('controllerCapabilitySend'));
  assert.ok(!chat.includes('controllerTaskSend('));
  assert.ok(!/input_digest|client_request_id|capability_revision|controller_lease_id/.test(chat));
});

test('the main chat tolerates an absent remote connection', () => {
  const chat = read('features/chat/ChatView.jsx');
  assert.ok(
    chat.includes('liveRemoteProxyBinding(activeSessionId, remoteProxyHistory, remoteConnection)'),
    'local chat startup must not dereference a null remote connection',
  );
});

test('the main chat cannot submit before the remote capability snapshot is ready', () => {
  const chat = read('features/chat/ChatView.jsx');
  assert.ok(chat.includes('if (!remoteLinkReady) return false'));
  assert.ok(chat.includes('(remoteChatActive && !remoteNewDispatchReady)'));
  assert.ok(chat.includes("!pendingRemoteDispatches.some(item => item.state === 'prepared_not_sent')"));
  assert.ok(chat.includes('(!remoteChatActive || remoteNewDispatchReady)'));
});

test('A events cannot populate an in-memory second transcript', () => {
  const state = logic.createState();
  const feature = read('platform/tauri/bridge/desktop-remote-control.js');
  assert.equal(Object.hasOwn(state, 'transcript'), false);
  assert.equal(typeof logic.applyJournalEvent, 'undefined');
  assert.ok(!feature.includes('desktop-remote-controller-journal'));
  assert.ok(feature.includes('desktop_remote_proxy'));
});

test('the controlled chat keeps the native session timeline when that session is active', () => {
  const chat = read('features/chat/ChatView.jsx');
  assert.match(
    chat,
    /activeSessionId && activeSessionId === controlledSession\.sessionId[\s\S]*?\? chatItems[\s\S]*?: projectedRemoteChatItems/,
  );
});

test('the A bridge exposes intent commands without a second journal subscriber', () => {
  const bridge = read('platform/tauri/bridge/desktop-remote-control.js');
  assert.ok(!bridge.includes('"desktop-remote-controller-journal"'));
  assert.ok(!bridge.includes('applyJournalEvent'));
  assert.ok(bridge.includes('"desktop_remote_proxy"'));
  for (const method of ['controllerCapabilitySend', 'submitRemoteTask']) {
    assert.ok(bridge.includes(`${method}: ${method}`), `${method} must be exported`);
  }
  assert.ok(bridge.includes('desktop_remote_controller_capability_send'));
  assert.ok(!bridge.includes('desktop_remote_controller_task_cancel_send'));
  assert.ok(!bridge.includes('controllerTaskCancelSend:'));
});

test('the controlled-side authoritative journal is projected locally before forwarding to A', () => {
  const task = read('../src-tauri/src/app/commands/desktop_remote_task.rs');
  const history = read('../src-tauri/src/app/commands/desktop_remote_journal.rs');
  const lib = read('../src-tauri/src/lib.rs');
  assert.ok(task.includes('"journal_event_projected"'));
  assert.ok(task.includes('CONTROLLED_JOURNAL_EVENT'));
  assert.ok(task.indexOf('CONTROLLED_JOURNAL_EVENT') < task.indexOf('publish_authoritative_event('));
  assert.ok(history.includes('publish_authoritative_task_status(&self.app, event)'));
  assert.ok(lib.includes('DesktopTaskEventSink::new('));
  assert.ok(task.indexOf('app.emit(TASK_EVENT, &status)') < task.indexOf('tauri::async_runtime::spawn(async move'));
  assert.ok(history.includes('lane.seal("task_status", &payload, &capabilities)'));
});

test('controlled-side send failures are visible in the shared remote transcript area', () => {
  const chat = read('features/chat/ChatView.jsx');
  assert.ok(chat.includes('data-testid="chat-remote-send-error"'));
  assert.match(
    chat,
    /remoteTranscriptActive && remoteSendMessage[\s\S]*?data-testid="chat-remote-send-error"[\s\S]*?role="alert"/,
  );
  assert.ok(chat.includes("`controlled:${controlledDevice.generation || ''}:${controlledSession.sessionId || ''}`"));
  assert.ok(chat.includes('identity: sendIdentity'));
  assert.ok(chat.includes('remoteSendError?.identity === remoteSendIdentity'));
  assert.match(chat, /current && current\.identity !== remoteSendIdentity \? null : current/);
  assert.ok(chat.includes('remoteProxyLeaseIdentity(activeSessionId, remoteConnection)'));
  assert.ok(chat.includes('remoteSendIdentityRef.current === sendIdentity'));
  assert.ok(chat.includes('sameRemoteComposerOwner('));
  assert.ok(chat.includes('if (!accepted && mayRestore())'));
  assert.ok(chat.includes('if (mayRestore()) {'));
});

test('temporary model capability mismatches have actionable localized feedback', () => {
  const chat = read('features/chat/ChatView.jsx');
  const main = read('app/main.jsx');
  assert.ok(chat.includes("currentRemoteSendError === 'temporary_model_capability_required'"));
  assert.ok(chat.includes('t.remoteConversation.temporaryModelCapabilityRequired'));
  assert.ok(chat.includes('remoteTemporaryModelAvailable &&'));
  assert.ok(chat.includes("code: 'remote_task_intent_invalid'"));
  assert.ok(!chat.includes('controllerTaskSend('));
  assert.ok(main.includes('modelSourceForLease('));
  assert.ok(main.includes('setRemoteModelSource(nextModelSource)'));
  for (const locale of ['zh', 'en', 'ja']) {
    const messages = read(`shared/i18n/${locale}.js`);
    assert.ok(messages.includes('temporaryModelCapabilityRequired:'), `${locale} copy is required`);
  }
});

test('bounded command failures keep their code for the UI copy', () => {
  const bridge = read('platform/tauri/bridge/desktop-remote-control.js');
  assert.ok(bridge.includes('function asBoundedError'), 'bounded errors are restored');
  assert.ok(bridge.includes('error.code = text'));
  // A 端连接/会话/能力/任务/断开路径都必须恢复错误码。
  for (const command of [
    'desktop_remote_controller_connect',
    'desktop_remote_controller_disconnect',
    'desktop_remote_controller_capability_send',
  ]) {
    const index = bridge.indexOf(`"${command}"`);
    assert.ok(index >= 0, `${command} must be invoked`);
    const chain = bridge.slice(index, index + 700);
    assert.ok(
      chain.includes('asBoundedError'),
      `${command} must map failures to a bounded error code`,
    );
  }
});
