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
