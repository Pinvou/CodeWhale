import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { act } from 'react';
import { createWindow } from '@mixmark-io/domino';
import { renderToString } from 'react-dom/server';
import { loadJsx } from './fixtures/jsx-module-hooks.mjs';
import { dict } from './helpers/i18n-all.js';
import { lanSample, lanError } from '../src/features/model-store/lan-access.js';
import { fetchLanStatus, setLanSharing, revokeApiKey } from '../src/features/model-store/model-store-api.js';

const { LanSharingPanel } = await loadJsx('../src/features/model-store/LanSharingPanel.jsx', import.meta.url);
const { ModelDetailDialog } = await loadJsx('../src/features/model-store/ModelDetailDialog.jsx', import.meta.url);

test('LAN IPC forwards explicit activation, address/port and only the selected revoke ID', async () => {
  const original = globalThis.__TAURI__;
  const calls = [];
  globalThis.__TAURI__ = { core: { invoke: async (command, payload) => { calls.push([command, payload]); return {}; } } };
  try {
    await fetchLanStatus('card-a');
    assert.deepEqual(calls.at(-1), ['model_store_lan_status', { cardId: 'card-a' }]);
    for (const value of [undefined, null, 'true', 'false', false, true]) {
      await setLanSharing('card-a', value, '192.0.2.2', 8091);
      assert.deepEqual(calls.at(-1), ['model_store_set_lan_sharing', { cardId: 'card-a', enabled: value === true, address: '192.0.2.2', port: 8091 }]);
    }
    await revokeApiKey('key-a');
    assert.deepEqual(calls.at(-1), ['model_store_revoke_api_key', { keyId: 'key-a' }]);
  } finally {
    if (original === undefined) delete globalThis.__TAURI__;
    else globalThis.__TAURI__ = original;
  }
});

test('copyable sample keeps the exact endpoint/model and never embeds keys', () => {
  const sample = lanSample({ endpoint: 'http://192.0.2.2:8091/v1/chat/completions', model_id: 'pinvou:card-a', key_value: 'DO-NOT-INCLUDE' });
  assert.match(sample, /http:\/\/192\.0\.2\.2:8091\/v1\/chat\/completions/);
  assert.match(sample, /"model":"pinvou:card-a"/);
  assert.match(sample, /"stream":true/);
  assert.match(sample, /Authorization: Bearer \$PINVOU_API_KEY/);
  assert.doesNotMatch(sample, /DO-NOT-INCLUDE|api_key=/);
  assert.equal(sample.split('\n').length, 4);
  assert.equal(lanSample(null), '');
  assert.ok(lanSample({ endpoint: 'http://192.0.2.2:8091/v1/chat/completions', model_id: "pinvou:it's-a-card" }).includes("'\\''"));
});

for (const language of ['zh', 'en', 'ja']) {
  test(`${language}: LAN controls render with a safe default and complete actionable errors`, () => {
    const copy = dict[language].uiModelStore;
    const html = renderToString(React.createElement(LanSharingPanel, { cardId: 'a', copy, keys: [{ card_id: 'a' }, { card_id: null }, { card_id: 'b' }], onCopy() {} }));
    assert.ok(html.includes(copy.lanTitle));
    assert.ok(html.includes(copy.lanLoading));
    assert.ok(html.includes(copy.lanKeyStatus(1)));
    assert.ok(html.includes(copy.lanEnable));
    assert.match(html, /disabled=""/);
    assert.doesNotMatch(html, /undefined|NaN|curl /);
    for (const code of ['lan_select_address', 'lan_address_changed', 'lan_bind_failed', 'lan_listener_in_use', 'lan_model_unavailable', 'lan_unsupported', 'lan_upstream_exposed', 'lan_isolation_unknown', 'lan_address_unavailable', 'lan_key_store', 'lan_unavailable']) {
      assert.ok(copy.lanErrors[code]?.length > 10);
      assert.equal(lanError(copy, new Error(code)), copy.lanErrors[code]);
    }
    assert.equal(lanError(copy, 'raw secret failure'), copy.lanErrors.lan_unavailable);
    assert.equal(lanError(copy, 'toString'), copy.lanErrors.lan_unavailable);
    assert.equal(lanError(copy, '__proto__'), copy.lanErrors.lan_unavailable);
  });
}

test('LAN panel opens only after an explicit address choice, copies exact details and closes', async () => {
  const window = createWindow('<div id="root"></div>');
  const previous = { window: globalThis.window, document: globalThis.document, tauri: globalThis.__TAURI__, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = window;
  globalThis.document = window.document;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const calls = [];
  const copied = [];
  const off = { enabled: false, available: true, reason: null, address: null, port: 8090, base_url: null, endpoint: null, model_id: 'pinvou:a', interfaces: [{ name: 'LAN', address: '192.0.2.2' }] };
  const on = { ...off, enabled: true, address: '192.0.2.2', base_url: 'http://192.0.2.2:8090/v1', endpoint: 'http://192.0.2.2:8090/v1/chat/completions' };
  globalThis.__TAURI__ = { core: { invoke: async (command, payload) => {
    calls.push([command, payload]);
    return command === 'model_store_set_lan_sharing' && payload.enabled ? on : off;
  } } };
  const { createRoot } = await import('react-dom/client');
  const root = createRoot(window.document.getElementById('root'));
  const copy = dict.en.uiModelStore;
  const button = (text) => Array.from(window.document.querySelectorAll('button')).find((item) => item.textContent === text);
  try {
    await act(async () => root.render(React.createElement(LanSharingPanel, { cardId: 'a', copy, keys: [{ card_id: 'a', key_value: 'never-embed' }], onCopy: (value) => copied.push(value) })));
    assert.ok(window.document.body.textContent.includes(copy.lanOff));
    assert.equal(button(copy.lanEnable).disabled, true);
    const select = window.document.querySelector('select');
    await act(async () => { select.value = '192.0.2.2'; select.dispatchEvent(new window.Event('change', { bubbles: true })); });
    assert.equal(button(copy.lanEnable).disabled, false);
    await act(async () => button(copy.lanEnable).click());
    assert.deepEqual(calls.at(-1), ['model_store_set_lan_sharing', { cardId: 'a', enabled: true, address: '192.0.2.2', port: 8090 }]);
    assert.ok(window.document.body.textContent.includes(on.endpoint));
    assert.ok(window.document.body.textContent.includes(on.model_id));
    assert.ok(window.document.body.textContent.includes(copy.lanOn));
    await act(async () => button(copy.lanCopySample).click());
    assert.match(copied.at(-1), /PINVOU_API_KEY/);
    assert.doesNotMatch(copied.at(-1), /never-embed/);
    await act(async () => button(copy.lanDisable).click());
    assert.equal(calls.at(-1)[1].enabled, false);
    assert.ok(window.document.body.textContent.includes(copy.lanOff));
    assert.ok(!window.document.querySelector('pre'));
  } finally {
    await act(async () => root.unmount());
    for (const [name, value] of [['window', previous.window], ['document', previous.document], ['__TAURI__', previous.tauri], ['IS_REACT_ACT_ENVIRONMENT', previous.act]]) {
      if (value === undefined) delete globalThis[name]; else globalThis[name] = value;
    }
  }
});

test('b048 real detail preserves LAN and key revocation across custom update confirmation', async () => {
  const window = createWindow('<div id="root"></div>');
  const names = ['window', 'document', '__TAURI__', 'IS_REACT_ACT_ENVIRONMENT'];
  const previous = new Map(names.map((name) => [name, Object.getOwnPropertyDescriptor(globalThis, name)]));
  globalThis.window = window;
  globalThis.document = window.document;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  let revoked = false;
  const calls = [];
  const actions = [];
  window.confirm = () => { throw new Error('Native confirmation must not be used'); };
  const copy = dict.en.uiModelStore;
  const secret = 'fixture-only-api-key-123456';
  globalThis.__TAURI__ = { core: { invoke: async (command, payload) => {
    calls.push([command, payload]);
    switch (command) {
      case 'model_store_model_detail': return { record: { card_id: 'a', card_name: 'Model A', status: 'deployed', running: false }, runtime_lines: [] };
      case 'model_store_api_keys': return revoked ? [] : [{ id: 'key-a', card_id: 'a', key_value: secret }];
      case 'model_store_lan_status': return { enabled: false, available: false, reason: 'lan_model_unavailable', interfaces: [], port: 8090 };
      case 'model_store_revoke_api_key': revoked = true; return { ok: true };
      default: throw new Error(`Unexpected IPC: ${command}`);
    }
  } } };
  const { createRoot } = await import('react-dom/client');
  const root = createRoot(window.document.getElementById('root'));
  const button = (label) => Array.from(window.document.querySelectorAll('button')).find((item) => item.textContent === label);
  try {
    await act(async () => root.render(React.createElement(ModelDetailDialog, {
      cardId: 'a', copy, theme: 'light', scriptUpdates: { a: true },
      onClose: () => actions.push('close'), onToast() {},
      onUpdateWithCleanup: (record, running) => actions.push([record.card_id, running]),
    })));
    assert.ok(window.document.querySelector('[data-testid="model-store-lan-sharing"]'));
    assert.ok(window.document.querySelector('[data-testid="model-store-detail-action-update"]'));
    assert.ok(window.document.body.textContent.includes('••••123456'));
    assert.ok(!window.document.body.textContent.includes(secret));
    await act(async () => button(copy.keyRevoke).click());
    assert.equal(revoked, false);
    await act(async () => window.document.querySelector('[data-testid="model-store-detail-action-update"]').click());
    assert.ok(window.document.querySelector('[data-testid="model-store-update-confirm"]'));
    assert.ok(window.document.body.textContent.includes(copy.updateConfirmStopped));
    assert.deepEqual(actions, []);
    assert.equal(revoked, false);
    await act(async () => window.document.querySelector('[data-testid="model-store-update-cancel"]').click());
    assert.ok(window.document.querySelector('[data-testid="model-store-lan-sharing"]'));
    assert.ok(button(copy.keyRevokeConfirm), 'Cancelling update must preserve the pending key confirmation');
    assert.deepEqual(actions, []);
    await act(async () => button(copy.lanCancel).click());
    assert.equal(revoked, false);
    await act(async () => button(copy.keyRevoke).click());
    await act(async () => button(copy.keyRevokeConfirm).click());
    assert.deepEqual(calls.filter(([name]) => name === 'model_store_revoke_api_key'), [
      ['model_store_revoke_api_key', { keyId: 'key-a' }],
    ]);
    assert.ok(window.document.body.textContent.includes(copy.keysEmpty));
    await act(async () => window.document.querySelector('[data-testid="model-store-detail-action-update"]').click());
    await act(async () => window.document.querySelector('[data-testid="model-store-update-confirm-submit"]').click());
    assert.deepEqual(actions, ['close', ['a', false]]);
    assert.equal(calls.filter(([name]) => name === 'model_store_revoke_api_key').length, 1);
  } finally {
    await act(async () => root.unmount());
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else delete globalThis[name];
    }
  }
});
