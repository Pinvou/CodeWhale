import assert from 'node:assert/strict';
import { after, test } from 'node:test';
import { cmsSettingsEntryState } from '../src/features/megacube/settings-entry.js';
import { INITIAL_DEVICE_MANAGEMENT, deviceManagementState, deviceManagementTitle } from '../src/features/megacube/device-management-state.js';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';
import { dictZh } from '../src/shared/i18n/zh.js';
import { dictEn } from '../src/shared/i18n/en.js';
import { dictJa } from '../src/shared/i18n/ja.js';

const ok = value => ({ status: 'fulfilled', value });
const failed = { status: 'rejected', reason: new Error('backend unavailable') };

test('offline cmsd does not hide installation and rescue on a supported host', () => {
  assert.equal(cmsSettingsEntryState([ok({ available: false, reason: 'socket_missing' }), ok({ supported: true })]), 'available');
});
test('query failure and old backend missing commands preserve an unknown entry', () => {
  assert.equal(cmsSettingsEntryState([failed, failed]), 'unknown');
  assert.equal(cmsSettingsEntryState([ok({ available: false, reason: 'socket_missing' }), failed]), 'unknown');
});
test('explicit unsupported platform hides the entry only when all probes agree', () => {
  assert.equal(cmsSettingsEntryState([ok({ available: false, reason: 'unsupported_platform' }), ok({ supported: false })]), 'unsupported');
  assert.equal(cmsSettingsEntryState([ok({ supported: false }), failed]), 'unknown');
});
test('local daemon support exposes setup without granting writes', () => {
  assert.equal(cmsSettingsEntryState([ok({ available: true }), failed]), 'available');
  assert.equal(cmsSettingsEntryState([failed, ok({ supported: true })]), 'available');
});
test('malformed or incomplete capability replies do not assert support or absence', () => {
  assert.equal(cmsSettingsEntryState([]), 'unknown');
  assert.equal(cmsSettingsEntryState([ok(null), ok({ supported: 'false', available: 'true' })]), 'unknown');
  assert.equal(cmsSettingsEntryState([ok({ supported: false })]), 'unknown');
});

test('a running daemon does not identify ordinary hardware as Cube', () => {
  const state = deviceManagementState(INITIAL_DEVICE_MANAGEMENT,
    ok({ supported: true, detected: false, hardware: { state: 'not_checked' } }), [ok({ available: true })], true);
  assert.equal(state.isCube, false); assert.equal(state.managementAvailable, true);
});
test('Cube hardware keeps recovery accessible with an offline daemon', () => {
  const state = deviceManagementState(INITIAL_DEVICE_MANAGEMENT,
    ok({ supported: true, hardware: { state: 'compatible' } }), [failed, failed], true);
  assert.equal(state.isCube, true); assert.equal(state.managementAvailable, true);
});
test('transient query failure retains known Cube identity while confirmed incompatible hardware clears it', () => {
  const known = { isCube: true, managementAvailable: true };
  assert.equal(deviceManagementState(known, failed, [failed, failed], true).isCube, true);
  assert.equal(deviceManagementState(known, ok({ supported: true, hardware: { state: 'incompatible' } }), [failed], true).isCube, false);
});
test('unsupported commands preserve only the ordinary runtime destination', () => {
  const caps = [ok({ supported: false }), ok({ supported: false })];
  const state = deviceManagementState(INITIAL_DEVICE_MANAGEMENT, ok({ supported: false }), caps, true);
  assert.equal(state.isCube, false); assert.equal(state.managementAvailable, false);
});
test('navigation title follows device identity in all three actual dictionaries', () => {
  for (const copy of [dictZh, dictEn, dictJa]) {
    assert.equal(deviceManagementTitle({ isCube: false }, copy), copy.monitor);
    assert.equal(deviceManagementTitle({ isCube: true }, copy), copy.deviceManagement);
    assert.ok(copy.deviceManagement);
  }
});

// Component rendering checks are separate from real backend/device acceptance.
globalThis.window = { TauriBridge: { available: false }, matchMedia: () => ({ matches: false }) };
const vite = await createServer({ configFile: false, root: fileURLToPath(new URL('..', import.meta.url)),
  logLevel: 'error', server: { middlewareMode: true, watch: null, hmr: false, ws: false },
  optimizeDeps: { noDiscovery: true }, plugins: [react()] });
const { DeviceManagementView } = await vite.ssrLoadModule('/src/features/megacube/DeviceManagementView.jsx');
after(async () => { await vite.close(); delete globalThis.window; });
function render(device, section, copy = dictZh) {
  return renderToStaticMarkup(React.createElement(DeviceManagementView, {
    theme: 'light', t: copy, bs: {}, device, section, onSectionChange() {},
  }));
}
test('Cube opens the real management panel with service recovery and an accessible runtime tab', () => {
  const html = render({ isCube: true, managementAvailable: true }, null);
  assert.match(html, /设备管理/); assert.doesNotMatch(html, /factory-onboarding/);
  assert.match(html, /aria-selected="true"[^>]*>MegaCube/);
  assert.match(html, /role="tabpanel"/); assert.match(html, /tabindex="-1"/);
});
test('ordinary hardware opens runtime metrics and can explicitly select MegaCube controls', () => {
  const normal = render({ isCube: false, managementAvailable: true }, null);
  assert.doesNotMatch(normal, /data-testid="device-onboarding"/);
  const management = render({ isCube: false, managementAvailable: true }, 'management');
  assert.doesNotMatch(management, /data-testid="factory-onboarding"/);
});
test('unsupported hosts retain runtime metrics without a dead management tab', () => {
  const html = render({ isCube: false, managementAvailable: false }, 'management');
  assert.doesNotMatch(html, /role="tablist"/); assert.doesNotMatch(html, /device-onboarding/);
});
test('all languages can render the management destination', () => {
  for (const copy of [dictZh, dictEn, dictJa]) {
    const html = render({ isCube: true, managementAvailable: true }, null, copy);
    assert.ok(html.includes(copy.deviceManagement));
  }
});

test('verified board identity survives an unconfirmed runtime query without granting model control', () => {
  const state = deviceManagementState(INITIAL_DEVICE_MANAGEMENT,
    ok({ supported: true, detected: true, hardware: { state: 'unknown' } }), [failed], true);
  assert.equal(state.isCube, true);
  assert.equal(state.managementAvailable, true);
});
