// DEV-21 contract: the B-side panel must stay trilingual, accessible and free
// of any Allow/Approve control. Source-level checks complement the language
// parity test and the browser smoke lane.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from './helpers/i18n-all.js';

const source = (relative) => readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');
const panel = source('features/remote-control/ControlledDevicePanel.jsx');
const panelCode = panel.replace(/\/\/[^\n]*|\/\*[\s\S]*?\*\//g, '');
const settings = source('features/settings/SettingsView.jsx');

const CONTROLLED_KEYS = [
  'section', 'title', 'desc', 'enable', 'disable', 'statusIdle', 'statusWaiting', 'statusConnected',
  'codeLabel', 'codeHint', 'refresh', 'deviceLabel', 'modelSource', 'workspacesTitle',
  'readOnlyNotice', 'stopTasks', 'pendingPermissions', 'noAllowControl', 'unsupported',
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
  assert.ok(panelCode.includes('readOnlyNotice'));
});

test('the panel uses the remote-control toggle as its only disconnect action', () => {
  // 旧版同时提供开关和「立即断开」；二者最终都会 stop 并吊销代际，必须收敛。
  assert.ok(panel.includes('<Toggle'));
  assert.ok(panel.includes('remote.controlledStop()'));
  assert.ok(!panelCode.includes('controlledDisconnectNow'));
  assert.ok(!panelCode.includes('disconnectNow'));
});

test('the pairing code appears in the controlled connection block', () => {
  const connectionStart = panel.indexOf('copy.connectionTitle');
  const deviceStart = panel.indexOf('copy.deviceLabel');
  const connectionCard = panel.slice(connectionStart, deviceStart);
  const outerCardStart = panel.lastIndexOf('<div className="mb-4 rounded-xl border', connectionStart);
  const nextCardStart = panel.indexOf('<div className="mb-4 grid gap-3 sm:grid-cols-2"', connectionStart);
  const codeStart = panel.indexOf('controlled-device-code', connectionStart);
  const codeEnd = panel.indexOf('copy.codeExpires(seconds)', codeStart);

  assert.ok(connectionCard.includes('controlled-device-code'));
  assert.ok(connectionCard.includes('formatPairingCode(device.pairingCode)'));
  assert.ok(connectionCard.includes('copy.codeExpires(seconds)'));
  assert.ok(outerCardStart !== -1 && nextCardStart !== -1);
  assert.ok(outerCardStart < codeStart && codeStart < codeEnd && codeEnd < nextCardStart);
});

test('the panel is keyboard reachable and screen-reader friendly', () => {
  assert.ok(panel.includes('aria-live'));
  assert.ok(panel.includes('role="group"'));
  assert.ok(panel.includes('role="alert"'));
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

test('settings keeps the Web share card and the desktop panel separate', () => {
  assert.ok(settings.includes('ControlledDevicePanel'));
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
