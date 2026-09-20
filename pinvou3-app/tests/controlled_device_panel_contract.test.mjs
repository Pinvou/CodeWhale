// DEV-21 contract: the B-side panel must stay trilingual, accessible and free
// of any Allow/Approve control. Source-level checks complement the language
// parity test and the browser smoke lane.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from '../src/shared/i18n-all.js';

const source = (relative) => readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');
const panel = source('features/remote-control/ControlledDevicePanel.jsx');
const panelCode = panel.replace(/\/\/[^\n]*|\/\*[\s\S]*?\*\//g, '');
const settings = source('features/settings/SettingsView.jsx');

const CONTROLLED_KEYS = [
  'section', 'title', 'desc', 'enable', 'disable', 'statusIdle', 'statusWaiting', 'statusConnected',
  'codeLabel', 'codeHint', 'refresh', 'deviceLabel', 'modelSource', 'workspacesTitle',
  'readOnlyNotice', 'disconnectNow', 'stopTasks', 'pendingPermissions', 'noAllowControl', 'unsupported',
  // E2E-08 后的零配置体验：服务端点（生产/测试）内置在应用中，用户不需要环境变量。
  'endpointLabel', 'endpointProduction', 'endpointTest', 'endpointHint',
];

test('the B panel switches the built-in service endpoint through the bridge', () => {
  const bridge = source('platform/tauri/bridge/desktop-remote-control.js');
  assert.ok(panel.includes('endpointProfiles'), 'panel loads the built-in endpoint list');
  assert.ok(panel.includes('setEndpointProfile'), 'panel switches endpoint through the bridge');
  assert.ok(panel.includes('endpointLabel'), 'endpoint control uses shared i18n copy');
  assert.ok(/id="controlled-device-endpoint"/.test(panel), 'endpoint control is addressable');
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
  assert.ok(panelCode.includes('noAllowControl'));
  assert.ok(panelCode.includes('readOnlyNotice'));
});

test('the panel is keyboard reachable and screen-reader friendly', () => {
  assert.ok(panel.includes('aria-live'));
  assert.ok(panel.includes('role="status"'));
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

test('the desktop panel owns a dedicated settings sub-page', () => {
  assert.ok(settings.includes('id="remote"'), 'nav registers a remote-control section');
  assert.ok(settings.includes("activeSection === 'remote'"), 'content routes the remote-control section');
  assert.ok(settings.includes('const renderRemote'), 'the section has its own renderer');
  assert.ok(settings.includes('const canDesktopRemoteControl = can(\'desktopRemoteControl\')'), 'the section is capability gated');
  const generalBody = settings.slice(
    settings.indexOf('const renderGeneral'),
    settings.indexOf('const renderRemote'),
  );
  assert.ok(generalBody.length > 0, 'general section must stay ahead of the remote section');
  assert.ok(
    !generalBody.includes('ControlledDevicePanel'),
    'the desktop panel must not be rendered inside the general section anymore',
  );
});
