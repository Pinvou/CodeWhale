// DEV-21 contract: the B-side panel must stay trilingual, accessible and free
// of any Allow/Approve control. Source-level checks complement the language
// parity test and the browser smoke lane.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from '../src/shared/i18n-all.js';
import {
  CONTROLLED_UI_STATES,
  controlledAuthorityKey,
  controlledDeviceUiState,
  createAsyncOperationGuard,
  remoteControlTabTone,
} from '../src/features/remote-control/remote-control-logic.mjs';

const source = (relative) => readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');
const panel = source('features/remote-control/ControlledDevicePanel.jsx');
const panelCode = panel.replace(/\/\/[^\n]*|\/\*[\s\S]*?\*\//g, '');
const settings = source('features/settings/SettingsView.jsx');
const settingsPage = source('features/remote-control/RemoteControlSettingsPage.jsx');
const rustControlled = readFileSync(new URL('../src-tauri/src/features/remote_control/controller/controlled.rs', import.meta.url), 'utf8');

const CONTROLLED_KEYS = [
  'section', 'title', 'desc', 'enable', 'disable', 'statusIdle', 'statusWaiting', 'statusConnected',
  'controlledStatus',
  'codeLabel', 'codeHint', 'refresh', 'deviceLabel', 'workspacesTitle',
  'stateOff', 'stateReserving', 'stateWaiting', 'stateExpiring', 'stateExpired',
  'stateReserved', 'stateHandshake', 'stateConnected', 'stateReconnecting',
  'stateRefreshing', 'stateStopping', 'stateError', 'securityNotice', 'unsupported',
  'errorRelayUnavailable',
  // E2E-08 后的零配置体验：服务端点（生产/测试）内置在应用中，用户不需要环境变量。
  'endpointLabel', 'endpointProduction', 'endpointTest', 'endpointHint',
];

test('the built-in service endpoint stays hidden from B-side controls', () => {
  const bridge = source('platform/tauri/bridge/desktop-remote-control.js');
  // 零配置后，服务端点由桥接/后端内置；被控面板不再暴露生产/测试切换。
  assert.ok(!panel.includes('endpointProfiles'), 'panel must not load a user-facing endpoint list');
  assert.ok(!panel.includes('setEndpointProfile'), 'panel must not switch endpoints');
  assert.ok(!panel.includes('endpointLabel'), 'panel must not show endpoint switching copy');
  assert.ok(!/id="controlled-device-endpoint"/.test(panel), 'panel must not expose an endpoint control');
  assert.ok(bridge.includes('desktop_remote_endpoint_profiles'));
  assert.ok(bridge.includes('desktop_remote_set_endpoint_profile'));
});

test('controlled-device copy exists in all three languages', () => {
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(dict[language].controlledDevice, `${language}.controlledDevice must exist`);
    for (const key of CONTROLLED_KEYS) {
      assert.ok(dict[language].controlledDevice[key], `${language}.controlledDevice.${key} must exist`);
    }
  }
  assert.notEqual(dict.zh.controlledDevice.title, dict.en.controlledDevice.title);
  assert.notEqual(dict.zh.controlledDevice.title, dict.ja.controlledDevice.title);
  assert.notEqual(dict.en.controlledDevice.title, dict.ja.controlledDevice.title);
  // Functions are copy helpers, not shared across languages.
  assert.equal(typeof dict.zh.controlledDevice.codeExpires, 'function');
  assert.equal(typeof dict.en.controlledDevice.codeExpires, 'function');
  assert.equal(typeof dict.ja.controlledDevice.codeExpires, 'function');
});

test('the panel never exposes an approval control', () => {
  assert.ok(!/\bAllow\b|\bApprove\b|allowButton|approveButton/.test(panelCode));
  assert.ok(!panelCode.includes('remotePermissions'));
  assert.ok(!panelCode.includes('controlledTasks'));
  assert.ok(panelCode.includes('securityNotice'));
});

test('the controlled side exposes all twelve designed UI states', () => {
  assert.equal(CONTROLLED_UI_STATES.length, 12);
  assert.deepEqual(CONTROLLED_UI_STATES, [
    'off', 'reserving', 'waiting', 'expiring', 'expired', 'reserved',
    'handshake', 'connected', 'reconnecting', 'refreshing', 'stopping', 'error',
  ]);
  assert.equal(controlledDeviceUiState({ status: 'idle' }), 'off');
  assert.equal(controlledDeviceUiState({ status: 'waiting_connection', pairingCode: '2345678' }, 61), 'waiting');
  assert.equal(controlledDeviceUiState({ status: 'waiting_connection', pairingCode: '2345678' }, 60), 'expiring');
  assert.equal(controlledDeviceUiState({ status: 'waiting_connection' }, 0), 'expired');
  assert.equal(controlledDeviceUiState({ status: 'safety_handshake' }), 'handshake');
  assert.equal(controlledDeviceUiState({ status: 'connected' }), 'connected');
  assert.equal(controlledDeviceUiState({ status: 'reconnecting' }), 'reconnecting');
  assert.equal(controlledDeviceUiState({ status: 'refreshing' }), 'refreshing');
  assert.equal(controlledDeviceUiState({ status: 'error' }), 'error');
  // B6 is a short projection of the real waiting -> safety-handshake claim;
  // B11 is the pending controlledStop operation, not an invented Rust enum.
  assert.equal(controlledDeviceUiState({ status: 'safety_handshake' }, 0, 'reserved'), 'reserved');
  assert.equal(controlledDeviceUiState({ status: 'connected' }, 0, 'stopping'), 'stopping');
  assert.equal(controlledDeviceUiState({ status: 'idle' }, 0, 'reserving'), 'reserving');
  assert.equal(controlledDeviceUiState({ status: 'waiting_connection' }, 0, 'refreshing'), 'refreshing');
  assert.equal(controlledDeviceUiState({ status: 'idle' }, 0, 'error'), 'error');
  assert.ok(panel.includes('previousStatus'));
  assert.ok(panel.includes("remote.controlledStop()"));
  assert.ok(panel.includes("active ? 'stopping' : 'reserving'"));
  assert.ok(panel.includes("remote.controlledRefresh(), 'refreshing'"));
  assert.match(rustControlled, /WaitingConnection[\s\S]*SafetyHandshake/);
  assert.ok(!/enum ControlledStatusKind[\s\S]{0,500}\bStopping\b/.test(rustControlled));
  assert.ok(!/enum ControlledStatusKind[\s\S]{0,500}\bPairingReserved\b/.test(rustControlled));
});

test('pending code operations render a skeleton and local failures use the error alert', () => {
  assert.ok(panel.includes('codePending'));
  assert.ok(panel.includes("['reserving', 'refreshing']"));
  assert.ok(panel.includes('animate-pulse'));
  assert.ok(panel.includes("localError ? 'error'"));
  assert.ok(panel.includes("setLocalError('desktop_remote_control_unsupported')"));
});

test('authoritative status or generation changes invalidate stale local completion', () => {
  const guard = createAsyncOperationGuard();
  const attempt = guard.start();
  const before = controlledAuthorityKey({ generation: 7, status: 'waiting_connection' });
  const statusChanged = controlledAuthorityKey({ generation: 7, status: 'safety_handshake' });
  const generationChanged = controlledAuthorityKey({ generation: 8, status: 'waiting_connection' });
  assert.notEqual(before, statusChanged);
  assert.notEqual(before, generationChanged);
  guard.invalidate();
  assert.equal(guard.isCurrent(attempt), false);
  assert.ok(panel.includes('previousAuthority.current === authorityKey'));
  assert.ok(panel.includes('operationGuard.current.invalidate()'));
  assert.ok(panel.includes('setLocalError(null)'));
  assert.ok(panel.includes('authority.current === startingAuthority'));
});

test('folder picker failures share the operation guard and busy state', () => {
  const pickerIndex = panel.indexOf('bridge?.files?.pickFolders');
  const runIndex = panel.lastIndexOf("run('workspace'", pickerIndex);
  assert.ok(runIndex !== -1 && runIndex < pickerIndex, 'picker is inside the guarded workspace operation');
  assert.ok(panel.includes('if (busyOperation.current) return'));
  assert.ok(panel.includes("disabled={busy === 'workspace'}"));
  assert.ok(panel.includes("onLocalStateChange?.('error')"));
});

test('tab tones distinguish waiting, failure and off states', () => {
  assert.equal(remoteControlTabTone('waiting'), 'waiting');
  assert.equal(remoteControlTabTone('expiring'), 'waiting');
  assert.equal(remoteControlTabTone('expired'), 'off');
  assert.equal(remoteControlTabTone('off'), 'off');
  assert.equal(remoteControlTabTone('error'), 'error');
  assert.equal(remoteControlTabTone('connected'), 'success');
});

test('the panel uses the remote-control toggle as its only disconnect action', () => {
  // 旧版同时提供开关和「立即断开」；二者最终都会 stop 并吊销代际，必须收敛。
  assert.ok(panel.includes('<Toggle'));
  assert.ok(panel.includes('remote.controlledStop()'));
  assert.ok(!panelCode.includes('controlledDisconnectNow'));
  assert.ok(!panelCode.includes('disconnectNow'));
});

test('refresh is unavailable while a controller is connected', () => {
  const refresh = panel.slice(
    panel.indexOf('remote.controlledRefresh()'),
    panel.indexOf('</button>', panel.indexOf('remote.controlledRefresh()')),
  );

  assert.ok(refresh.includes("status === 'connected'"));
});

test('the pairing code appears in the controlled connection block', () => {
  const connectionStart = panel.indexOf('StatusBanner');
  const deviceStart = panel.indexOf('copy.deviceLabel');
  const connectionCard = panel.slice(connectionStart, deviceStart);
  const codeStart = panel.indexOf('controlled-device-code-label', connectionStart);
  const codeEnd = panel.indexOf('copy.codeExpires(seconds)', codeStart);

  assert.ok(connectionCard.includes('controlled-device-code-label'));
  assert.ok(connectionCard.includes('formatPairingCode(device.pairingCode)'));
  assert.ok(connectionCard.includes('copy.codeExpires(seconds)'));
  assert.ok(connectionStart < codeStart && codeStart < codeEnd && codeEnd < deviceStart);
});

test('the panel is keyboard reachable and screen-reader friendly', () => {
  assert.ok(panel.includes('aria-live'));
  assert.ok(panel.includes('role="group"'));
  assert.ok(panel.includes("role={state === 'error' || state === 'expired' ? 'alert' : 'status'}"));
  assert.ok(panel.includes('<button'));
  assert.ok(panel.includes('type="button"'));
  // The countdown is announced on a throttled schedule, not every second.
  assert.ok(panel.includes('COUNTDOWN_ANNOUNCE_EVERY'));
  assert.ok(!/aria-live="assertive"/.test(panel));
});

test('the panel reuses the shared i18n dictionary and the desktop bridge', () => {
  assert.ok(!/[\u4e00-\u9fff]/.test(panel.replace(/\/\/[^\n]*|\/\*[\s\S]*?\*\//g, '')));
  assert.ok(panel.includes("from '../../hooks/useBridge.js'"));
  assert.ok(panel.includes('controlledDevice'));
});

test('the panel explains when the relay controller lane is unavailable', () => {
  assert.ok(panel.includes('desktop_controller_relay_unavailable'));
  assert.ok(panel.includes('errorRelayUnavailable'));
});

test('settings keeps the Web share card and the desktop panel separate', () => {
  assert.ok(settings.includes('RemoteControlSettingsPage'));
  assert.ok(settingsPage.includes('ControlledDevicePanel'));
  assert.ok(settings.includes("can('desktopRemoteControl')"));
  assert.ok(settings.includes('WebAccessModal'));
});

test('the desktop panel is a second-level page under General, not a top-level tab', () => {
  assert.ok(
    !settings.includes('settings-section-remote') && !/<SectionButton id="remote"/.test(settings),
    'the panel must not occupy a top-level sidebar tab',
  );
  assert.ok(settings.includes('testId="desktop-remote-control-entry"'), 'General exposes a sub-menu entry');
  assert.ok(settings.includes("setActiveSection('remote')"), 'the entry opens the second-level page');
  assert.ok(settings.includes('desktop-remote-control-back'), 'the second-level page offers a way back');
  assert.ok(settings.includes("setActiveSection('general')"), 'the back action returns to General');
  assert.ok(settings.includes("activeSection === 'remote'"), 'content routes the second-level page');
  assert.ok(settings.includes('const renderRemote'), 'the second-level page has its own renderer');
  assert.ok(settings.includes('const canDesktopRemoteControl = can(\'desktopRemoteControl\')'), 'the entry is capability gated');
  const generalBody = settings.slice(
    settings.indexOf('const renderGeneral'),
    settings.indexOf('const renderRemote'),
  );
  assert.ok(generalBody.length > 0, 'general section must stay ahead of the remote page');
  assert.ok(
    !generalBody.includes('ControlledDevicePanel'),
    'the panel itself must render on the second-level page, not inline in General',
  );
  assert.ok(
    generalBody.includes('desktop-remote-control-entry'),
    'the General body holds only the entry row, not the panel',
  );
});
