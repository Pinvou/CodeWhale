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
  for (const status of [
    'reserving',
    'waiting_connection',
    'safety_handshake',
    'connected',
    'reconnecting',
    'refreshing',
    'stopped',
    'error',
    'disabled',
  ]) {
    assert.ok(panel.includes(`'${status}'`), `panel must map backend status ${status}`);
  }
  assert.ok(panel.includes('CODE_STATUSES'), 'pairing code must be status-driven');
  assert.ok(panel.includes("'waiting_connection'"), 'pairing code shows while waiting');
  assert.ok(
    panel.includes('controllerDeviceName'),
    'the 主控设备 row must use the controller device name',
  );
  // E2E-07：刷新状态不得借用「正在停止」的文案。
  assert.ok(
    /case 'refreshing':\s*return 'refreshing';/.test(panel),
    'Refreshing must keep its own copy instead of statusStopping',
  );
  assert.ok(panel.includes('deviceUnknown'), 'unknown names fall back to i18n copy');
  assert.ok(!/device\.deviceName \|\| copy\.deviceUnknown/.test(panel));
});

test('the A view opens the session, requests capabilities and renders the transcript', () => {
  const view = read('features/remote-control/RemoteConversationView.jsx');
  assert.ok(view.includes('controllerSessionOpen'));
  assert.ok(view.includes('controllerCapabilitySend'));
  assert.ok(view.includes('controllerTaskSend'));
  assert.ok(view.includes('controllerTaskCancelSend'));
  assert.ok(view.includes('transcript'), 'the view renders the authoritative transcript');
  assert.ok(view.includes('localBadge') && view.includes('remoteBadge'));
  // No wire field names may appear in the view.
  assert.ok(!/input_digest|client_request_id|capability_revision|controller_lease_id/.test(view));
});

test('the main chat tolerates an absent remote connection', () => {
  const chat = read('features/chat/ChatView.jsx');
  assert.ok(
    chat.includes('remoteConnection?.sessionRef && remoteConnection.capabilityRevision != null'),
    'local chat startup must not dereference a null remote connection',
  );
});

test('the main chat cannot submit before the remote capability snapshot is ready', () => {
  const chat = read('features/chat/ChatView.jsx');
  assert.ok(chat.includes('if (!remoteLinkReady) return false'));
  assert.ok(chat.includes('(remoteChatActive && !remoteLinkReady)'));
  assert.ok(chat.includes('(!remoteChatActive || remoteLinkReady)'));
});

test('journal events project into a deduplicated transcript', () => {
  const frame = (kind, sequence, text) => ({
    event: {
      eventId: `event-${sequence}`,
      epoch: 'epoch-1',
      sequence,
      kind,
      sessionId: 'session-a',
      taskId: null,
      terminalStatus: null,
      payload: { text },
    },
  });
  let state = logic.createState();
  state = logic.applyJournalEvent(state, frame('user_message', 1, 'hello from A'));
  state = logic.applyJournalEvent(state, frame('assistant_delta', 2, 'hello from B'));
  assert.equal(state.transcript.length, 2);
  assert.equal(state.transcript[0].role, 'user');
  assert.equal(state.transcript[1].role, 'assistant');
  assert.equal(state.transcript[1].text, 'hello from B');
  // 重复投递不得产生第二条；非正文事件（任务状态）不进 transcript。
  const duplicate = logic.applyJournalEvent(state, frame('assistant_delta', 2, 'hello from B'));
  assert.equal(duplicate, state);
  const statusOnly = logic.applyJournalEvent(state, frame('task_status', 3, 'succeeded'));
  assert.equal(statusOnly, state);
  // 快照替换该会话的全部条目。
  const snapshot = logic.applyJournalEvent(state, {
    snapshot: {
      epoch: 'epoch-2',
      sequence: 5,
      digest: 'a'.repeat(64),
      events: [
        {
          eventId: 'event-9',
          epoch: 'epoch-2',
          sequence: 5,
          kind: 'assistant_delta',
          sessionId: 'session-a',
          taskId: null,
          terminalStatus: null,
          payload: { text: 'after snapshot' },
        },
      ],
    },
  });
  assert.equal(snapshot.transcript.length, 1);
  assert.equal(snapshot.transcript[0].text, 'after snapshot');
});

test('the bridge subscribes the journal stream and exposes the A-side commands', () => {
  const bridge = read('platform/tauri/bridge/desktop-remote-control.js');
  assert.ok(bridge.includes('"desktop-remote-controller-journal"'));
  assert.ok(bridge.includes('applyJournalEvent'));
  for (const method of ['controllerCapabilitySend', 'controllerTaskSend', 'controllerTaskCancelSend']) {
    assert.ok(bridge.includes(`${method}: ${method}`), `${method} must be exported`);
  }
  assert.ok(bridge.includes('desktop_remote_controller_capability_send'));
});

test('the controlled-side authoritative journal is projected locally before forwarding to A', () => {
  const task = read('../src-tauri/src/app/commands/desktop_remote_task.rs');
  const history = read('../src-tauri/src/app/commands/desktop_remote_history.rs');
  assert.ok(task.includes('"journal_event_projected"'));
  assert.ok(task.includes('CONTROLLED_JOURNAL_EVENT'));
  assert.ok(task.indexOf('CONTROLLED_JOURNAL_EVENT') < task.indexOf('publish_authoritative_event(&self.app, &event)'));
  assert.ok(task.includes('publish_authoritative_task_status(&self.app, event)'));
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
});

test('bounded command failures keep their code for the UI copy', () => {
  const bridge = read('platform/tauri/bridge/desktop-remote-control.js');
  assert.ok(bridge.includes('function asBoundedError'), 'bounded errors are restored');
  assert.ok(bridge.includes('error.code = text'));
  // A 端连接/会话/能力/任务/断开路径都必须恢复错误码。
  for (const command of [
    'desktop_remote_controller_connect',
    'desktop_remote_controller_disconnect',
    'desktop_remote_controller_session_open',
    'desktop_remote_controller_capability_send',
    'desktop_remote_controller_task_send',
    'desktop_remote_controller_task_cancel_send',
    'desktop_remote_controller_apply_capability_snapshot',
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
