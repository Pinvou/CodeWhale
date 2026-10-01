// DEV-22 contract: code normalization, unified service errors, no code reuse
// and an inline A-side connection zone on the remote-control settings page.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from './helpers/i18n-all.js';
import {
  connectionErrorKey,
  groupPairingCode,
  normalizePairingCode,
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
  assert.ok(bridge.includes('logic.applyControllerStatus(current(), payload)'));
  assert.ok(bridge.includes('controllerStatus().catch(function () {})'));
  assert.ok(bridge.includes('var payload = shallow(result)'));
  assert.ok(bridge.includes('if (current().revision !== revision) return result'));
});

test('the shell separates hiding the page section from disconnecting', () => {
  assert.ok(shell.includes('backgroundHint'));
  assert.ok(shell.includes('disconnect'));
  // Closing the shell calls onClose; only disconnect requests the transport.
  assert.ok(shell.includes('onRequestDisconnect'));
  assert.ok(!/onClose[\s\S]{0,80}controllerDisconnect/.test(shell));
});

test('the controller and controlled operation zones stay separated', () => {
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
  assert.ok(controllerIndex < panelIndex);
  assert.ok(panelIndex < controlledIndex);
  assert.ok(controlledIndex < controlledPanelIndex);
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
  assert.ok(chatView.includes('const displayChatItems = remoteChatActive ? projectedRemoteChatItems : chatItems;'));
  assert.ok(chatView.includes('data-testid="chat-remote-status"'));
  assert.ok(chatView.includes('data-testid="chat-remote-status-banner"'));
  assert.ok(chatView.includes('remoteSessionReady'));
  assert.ok(chatView.includes('controllerSessionOpen(t.remoteConversation.title || \'remote\', \'chat\')'));
});
