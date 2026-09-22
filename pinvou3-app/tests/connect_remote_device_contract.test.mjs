// DEV-22 contract: code normalization, unified service errors, no code reuse
// and an inline A-side connection zone on the remote-control settings page.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from '../src/shared/i18n-all.js';
import {
  connectionErrorKey,
  groupPairingCode,
  isControlledDeviceChatActive,
  normalizePairingCode,
  openControlledDeviceChatWithRetry,
} from '../src/features/remote-control/remote-control-logic.mjs';

const source = (relative) => readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');
const form = source('features/remote-control/RemoteConnectForm.jsx');
const panel = source('features/remote-control/RemoteControllerPanel.jsx');
const shell = source('features/remote-control/RemoteConnectionShell.jsx');
const main = source('app/main.jsx');
const settings = source('features/settings/SettingsView.jsx');
const bridge = source('platform/tauri/bridge/desktop-remote-control.js');
const chatView = source('features/chat/ChatView.jsx');

test('pasted, mixed-case and separated codes normalize to one wire value', () => {
  assert.equal(normalizePairingCode('2345 678'), '2345678');
  assert.equal(normalizePairingCode('2345-678'), '2345678');
  assert.equal(normalizePairingCode('  2345\t678  '), '2345678');
  assert.equal(normalizePairingCode('2345678'), '2345678');
  // Ambiguous characters from the frozen alphabet are dropped, not guessed.
  assert.equal(normalizePairingCode('234567O'), null);
  assert.equal(normalizePairingCode('234567'), null);
  assert.equal(normalizePairingCode('23456789'), null);
  assert.equal(normalizePairingCode(''), null);
  assert.equal(groupPairingCode('2345678'), '2345 678');
});

test('every service failure maps to one shared copy key', () => {
  const keys = new Set();
  for (const code of [
    'controller_busy', 'desktop_controller_active', 'desktop_controller_pending',
    'controller_already_connecting', 'pairing_code_generation_failed',
    'endpoint_not_found', 'controller_offline', 'pairing_code_invalid', 'pairing_expired',
    'protocol_version_unsupported', 'unsupported_capability', 'anything_else',
    // E2E-07：A 端连接链路自己的有界码也必须有专属文案。
    'endpoint_offline', 'invalid_message',
    'desktop_remote_control_unsupported',
  ]) {
    const key = connectionErrorKey(code);
    keys.add(key);
    assert.ok(dict.zh.controllerConnection[key], `copy key ${key} must exist`);
    assert.ok(dict.en.controllerConnection[key], `copy key ${key} must exist`);
    assert.ok(dict.ja.controllerConnection[key], `copy key ${key} must exist`);
  }
  // Distinct server reasons collapse onto a small bounded set of copy keys.
  assert.ok(keys.size <= 8);
  assert.equal(connectionErrorKey('endpoint_not_found'), connectionErrorKey('pairing_expired'));
  assert.equal(connectionErrorKey('endpoint_offline'), 'errorOffline');
  assert.equal(connectionErrorKey('invalid_message'), 'errorProtocol');
  assert.equal(connectionErrorKey('controller_already_connecting'), 'errorLocalBusy');
  assert.equal(connectionErrorKey('desktop_remote_control_unsupported'), 'errorUnsupported');
});

test('the inline form never persists or replays a code', () => {
  assert.ok(!/localStorage|sessionStorage|indexedDB/.test(form));
  assert.ok(!/recent|lastCode|codeHistory/i.test(form));
  assert.ok(form.includes('autoComplete="off"'));
  assert.ok(form.includes('settings-error-code'));
  assert.ok(form.includes('type="submit"'));
  assert.ok(!form.includes('role="dialog"'));
  assert.ok(!form.includes('fixed inset-0'));
});

test('connecting state keeps its own copy instead of claiming failure', () => {
  assert.ok(panel.includes("connection.status === 'connecting'"));
  assert.ok(panel.includes('? copy.connecting'));
});

test('A-side connection status is restored from the backend state machine', () => {
  assert.ok(bridge.includes('listen("desktop-remote-controller-status"'));
  assert.ok(bridge.includes('controllerStatusPayload(payload)'));
  assert.ok(bridge.includes('controllerStatus().catch(function () {})'));
  assert.ok(bridge.includes('controllerStatusPayload(result)'));
  assert.ok(bridge.includes('deviceName: wire.deviceDisplayName || null'));
});

test('the shell separates hiding the page section from disconnecting', () => {
  assert.ok(shell.includes('backgroundHint'));
  assert.ok(shell.includes('disconnect'));
  // Closing the shell calls onClose; only disconnect requests the transport.
  assert.ok(shell.includes('onRequestDisconnect'));
  assert.ok(!/onClose[\s\S]{0,80}controllerDisconnect/.test(shell));
});

test('the controlled operation zone precedes the controller zone in settings', () => {
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(dict[language].controllerConnection.controllerSide);
    assert.ok(dict[language].controllerConnection.controlledSide);
  }
  const remoteStart = settings.indexOf('const renderRemote');
  const modelsStart = settings.indexOf('const renderModels', remoteStart);
  const remoteBody = settings.slice(remoteStart, modelsStart);
  const controllerIndex = remoteBody.indexOf('controllerSide');
  const panelIndex = remoteBody.indexOf('<RemoteControllerPanel');
  const controlledIndex = remoteBody.indexOf('controlledSide');
  const controlledPanelIndex = remoteBody.indexOf('<ControlledDevicePanel');

  assert.notEqual(remoteStart, -1);
  assert.notEqual(modelsStart, -1);
  assert.notEqual(controllerIndex, -1);
  assert.notEqual(panelIndex, -1);
  assert.notEqual(controlledIndex, -1);
  assert.notEqual(controlledPanelIndex, -1);
  assert.ok(controlledIndex < controlledPanelIndex);
  assert.ok(controlledPanelIndex < controllerIndex);
  assert.ok(controllerIndex < panelIndex);
});

test('entering remote-control settings resets the shared content scroller to the top', () => {
  assert.ok(settings.includes('const settingsContentRef = useRef(null);'));
  assert.match(
    settings,
    /useLayoutEffect\(\(\) => \{[\s\S]*?activeSection !== 'remote'[\s\S]*?settingsContentRef\.current\.scrollTop = 0;[\s\S]*?\}, \[activeSection\]\);/,
  );
  assert.ok(settings.includes('<main ref={settingsContentRef} data-testid="settings-content"'));
});

test('connection is integrated into the remote-control page instead of a detached dialog', () => {
  assert.ok(settings.includes('RemoteControllerPanel'));
  assert.ok(panel.includes('<RemoteConnectForm'));
  assert.ok(!panel.includes('<RemoteConnectionShell'));
  assert.ok(main.includes('LazyRemoteConnectionShell'));
  assert.ok(main.includes('remoteConversationOpen && ('));
  assert.ok(main.includes('remoteControllerConnected = !!(bs && bs.controllerConnection && bs.controllerConnection.status === \'connected\')'));
  assert.ok(main.includes('remoteChatConnection = (remoteControllerConnected || remoteConnectionReady)'));
  assert.ok(main.includes('remoteConnection={remoteChatConnection}'));
  assert.ok(main.includes('onRemoteConversationOpen={() => setRemoteConversationOpen(true)}'));
  assert.ok(main.includes('onRemoteConnected={() => {'));
  assert.ok(main.includes('setRemoteConversationOpen(false);'));
  assert.ok(main.includes("navigateFromScheduledRun('chat');"));
  assert.ok(!main.includes('LazyConnectRemoteDeviceDialog'));
  assert.ok(!main.includes('remoteConnectOpen'));
  assert.ok(!settings.includes('connect-remote-device-entry'));
  assert.ok(form.includes('aria-invalid'));
  assert.ok(shell.includes('aria-live'));
});

test('connected remote output renders in the ordinary ChatView timeline', () => {
  assert.ok(chatView.includes('chat-transcript-projection.mjs'));
  assert.ok(chatView.includes('const projectedRemoteChatItems = useMemo('));
  assert.ok(chatView.includes('const displayChatItems = remoteTranscriptActive ? projectedRemoteChatItems : chatItems;'));
  assert.ok(chatView.includes('data-testid="chat-remote-status"'));
  assert.ok(chatView.includes('data-testid="chat-remote-status-banner"'));
  assert.ok(chatView.includes('remoteSessionReady'));
  assert.ok(chatView.includes('controllerSessionOpen(t.remoteConversation.title || \'remote\', \'chat\')'));
});

test('the controlled side opens chat once per connection and shows its own status', () => {
  assert.equal(isControlledDeviceChatActive({ status: 'connected' }), true);
  assert.equal(isControlledDeviceChatActive({ status: 'reconnecting' }), true);
  assert.equal(isControlledDeviceChatActive({ status: 'waiting_connection' }), false);
  assert.equal(isControlledDeviceChatActive({ status: 'stopped' }), false);
  assert.equal(isControlledDeviceChatActive(null), false);

  assert.ok(main.includes('controlledDeviceAutoOpenRef'));
  assert.ok(main.includes('controlledDeviceAutoOpenInFlightRef'));
  assert.ok(main.includes("if (controlledDeviceStatus !== 'connected')"));
  assert.ok(main.includes("const connectionKey = controlledDeviceGeneration || 'legacy-connected';"));
  assert.ok(main.includes('openControlledDeviceChatWithRetry({'));
  assert.ok(main.includes('if (opened) controlledDeviceAutoOpenRef.current = connectionKey;'));
  assert.ok(chatView.includes('isControlledDeviceChatActive(controlledDevice)'));
  assert.ok(chatView.includes('data-testid="chat-controlled-status"'));
  assert.ok(chatView.includes('t.controlledDevice.controlledStatus'));
});

test('controlled-side auto-open marks success only after navigation succeeds', async () => {
  let attempts = 0;
  let waits = 0;
  const opened = await openControlledDeviceChatWithRetry({
    navigate: async () => {
      attempts += 1;
      return attempts === 2;
    },
    wait: async () => { waits += 1; },
  });
  assert.equal(opened, true);
  assert.equal(attempts, 2);
  assert.equal(waits, 1);
});

test('controlled-side auto-open stops retrying after the original intent is superseded', async () => {
  let isCurrent = true;
  let attempts = 0;
  const opened = await openControlledDeviceChatWithRetry({
    navigate: async () => {
      attempts += 1;
      isCurrent = false;
      return false;
    },
    isCurrent: () => isCurrent,
    wait: async () => assert.fail('superseded navigation must not wait or retry'),
  });
  assert.equal(opened, false);
  assert.equal(attempts, 1);
});

test('controlled-side auto-open contains a rejected navigation and retries', async () => {
  let attempts = 0;
  let errors = 0;
  const opened = await openControlledDeviceChatWithRetry({
    navigate: async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('transient navigation failure');
      return true;
    },
    wait: async () => {},
    onError: () => { errors += 1; },
  });
  assert.equal(opened, true);
  assert.equal(attempts, 2);
  assert.equal(errors, 1);
});

test('the controlled device projects an admitted task into its main chat view', () => {
  assert.ok(chatView.includes('data-testid="chat-controlled-task-banner"'));
  assert.ok(chatView.includes('controlledActiveTasks[0].text'));
  assert.ok(chatView.includes('taskStatusKey(controlledActiveTasks[0].status)'));
  assert.ok(chatView.includes('const remoteTranscriptActive = remoteChatActive || controlledChatActive'));
  assert.ok(chatView.includes('bs ? bs.controlledTasks : []'));
  assert.ok(chatView.includes('bridge.desktopRemoteControl.controlledTaskSend(controlledSession.sessionId, outgoing)'));
  assert.ok(bridge.includes('function controlledTaskSend(sessionId, text)'));
  assert.ok(bridge.includes('logic.applyControlledTaskEvent(current()'));
});
