// DEV-21 contract: the B-side panel must stay trilingual, accessible and free
// of any Allow/Approve control. Source-level checks complement the language
// parity test and the browser smoke lane.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from './helpers/i18n-all.js';
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
  'stateOff', 'stateStopped', 'stateRegistering', 'stateWaiting', 'stateExpiring', 'stateExpired',
  'statePairingReserved', 'stateHandshakePending', 'stateConnected', 'stateReconnecting',
  'stateResumeProvisional', 'stateResumeActivatedPendingLiveness', 'stateRetiringGeneration',
  'stateStopping', 'stateError', 'securityNotice', 'unsupported',
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
  assert.ok(panelCode.includes('currentControlledTasks(device, bs?.controlledSession, bs?.controlledTasks)'));
  assert.ok(panelCode.includes('permissionList(task.taskId)'));
  assert.ok(!panelCode.includes('permissionDecide('));
  assert.ok(panelCode.includes('securityNotice'));
});

test('the controlled side exposes every authoritative status without a local reservation', () => {
  assert.equal(CONTROLLED_UI_STATES.length, 15);
  assert.deepEqual(CONTROLLED_UI_STATES, [
    'off', 'registering', 'waiting_for_pairing', 'expiring', 'expired',
    'pairing_reserved', 'handshake_pending', 'connected', 'reconnecting',
    'resume_provisional', 'resume_activated_pending_liveness', 'retiring_generation',
    'stopping', 'stopped', 'error',
  ]);
  assert.equal(controlledDeviceUiState({ status: 'disabled' }), 'off');
  assert.equal(controlledDeviceUiState({ status: 'stopped' }), 'stopped');
  assert.equal(controlledDeviceUiState({ status: 'waiting_for_pairing', pairingCode: '2345678' }, 61), 'waiting_for_pairing');
  assert.equal(controlledDeviceUiState({ status: 'waiting_for_pairing', pairingCode: '2345678' }, 60), 'expiring');
  assert.equal(controlledDeviceUiState({ status: 'waiting_for_pairing' }, 0), 'expired');
  assert.equal(controlledDeviceUiState({ status: 'pairing_reserved' }), 'pairing_reserved');
  assert.equal(controlledDeviceUiState({ status: 'handshake_pending' }), 'handshake_pending');
  assert.equal(controlledDeviceUiState({ status: 'connected' }), 'connected');
  assert.equal(controlledDeviceUiState({ status: 'reconnecting' }), 'reconnecting');
  assert.equal(controlledDeviceUiState({ status: 'resume_provisional' }), 'resume_provisional');
  assert.equal(controlledDeviceUiState({ status: 'resume_activated_pending_liveness' }), 'resume_activated_pending_liveness');
  assert.equal(controlledDeviceUiState({ status: 'retiring_generation' }), 'retiring_generation');
  assert.equal(controlledDeviceUiState({ status: 'error' }), 'error');
  assert.equal(controlledDeviceUiState({ status: 'unrecognized_future_state' }), 'error');
  // Local button progress cannot fabricate a protocol state.
  assert.equal(controlledDeviceUiState({ status: 'connected' }, 0, 'stopping'), 'connected');
  assert.equal(controlledDeviceUiState({ status: 'disabled' }, 0, 'registering'), 'off');
  assert.ok(!panel.includes('previousStatus'));
  assert.ok(!panel.includes("onLocalStateChange?.('reserved')"));
  assert.ok(panel.includes("remote.controlledStop()"));
  assert.ok(!panel.includes('onLocalStateChange'));
  assert.ok(panel.includes('remote.controlledRefresh(), true'));
  assert.match(rustControlled, /PairingReserved[\s\S]*HandshakePending/);
  assert.match(rustControlled, /ResumeProvisional[\s\S]*ResumeActivatedPendingLiveness/);
});

test('pending code operations render a skeleton and local failures use the error alert', () => {
  assert.ok(panel.includes('codePending'));
  assert.ok(panel.includes("['registering', 'retiring_generation']"));
  assert.ok(panel.includes('animate-pulse'));
  assert.ok(panel.includes('localError && <div'));
  assert.ok(panel.includes("setLocalError('desktop_remote_control_unsupported')"));
});

test('authoritative status or generation changes invalidate stale local completion', () => {
  const guard = createAsyncOperationGuard();
  const attempt = guard.start();
  const before = controlledAuthorityKey({ generation: 7, status: 'waiting_for_pairing' });
  const statusChanged = controlledAuthorityKey({ generation: 7, status: 'handshake_pending' });
  const generationChanged = controlledAuthorityKey({ generation: 8, status: 'waiting_for_pairing' });
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
  assert.ok(panel.includes('disabled={!!busy}'));
  assert.ok(panel.includes('setLocalError(failure?.code'));
});

test('tab tones distinguish waiting, failure and off states', () => {
  assert.equal(remoteControlTabTone('waiting_for_pairing'), 'waiting');
  assert.equal(remoteControlTabTone('expiring'), 'waiting');
  assert.equal(remoteControlTabTone('expired'), 'off');
  assert.equal(remoteControlTabTone('off'), 'off');
  assert.equal(remoteControlTabTone('error'), 'error');
  assert.equal(remoteControlTabTone('connected'), 'success');
});

test('the panel offers both a deliberate off switch and an always-local immediate disconnect', () => {
  assert.ok(panel.includes('<Toggle'));
  assert.ok(panel.includes('remote.controlledStop()'));
  assert.ok(panelCode.includes('remote.controlledDisconnectNow()'));
  assert.ok(panelCode.includes('copy.disconnectNow'));
});

test('refresh remains available in live and recovery states and closes only while retiring or stopping', () => {
  const refresh = panel.slice(
    panel.indexOf('remote.controlledRefresh()'),
    panel.indexOf('</button>', panel.indexOf('remote.controlledRefresh()')),
  );

  assert.ok(!refresh.includes("status === 'connected'"));
  assert.ok(refresh.includes("['retiring_generation', 'stopping'].includes(uiState)"));
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
  assert.ok(panel.includes("uiState === 'expired' && <p"));
  assert.ok(connectionStart < codeStart && codeStart < codeEnd && codeEnd < deviceStart);
});

test('the panel is keyboard reachable and screen-reader friendly', () => {
  assert.ok(panel.includes('aria-live'));
  assert.ok(panel.includes('role="group"'));
  assert.ok(panel.includes("role={state === 'error' || state === 'expired' ? 'alert' : 'status'}"));
  assert.ok(panel.includes('<button'));
  assert.ok(panel.includes('type="button"'));
  // The countdown is announced on a throttled schedule, not every second.
  assert.ok(panel.includes('countdownAnnouncement(seconds)'));
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
  assert.ok(!panel.includes('String(errorCode)'), 'raw protocol errors must not render');
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
