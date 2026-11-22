import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToString } from 'react-dom/server';
import { loadJsx } from './fixtures/jsx-module-hooks.mjs';
import { startDeploy, startEnable } from '../src/features/model-store/model-store-api.js';

const { MemoryAdmissionDialog } = await loadJsx('../src/features/model-store/MemoryAdmissionDialog.jsx', import.meta.url);
const { dict } = await import('./helpers/i18n-all.js');
const GiB = 1024 ** 3;
const noop = () => {};

test('enable and deploy only forward an explicit boolean memory confirmation', async () => {
  const original = globalThis.__TAURI__;
  const calls = [];
  globalThis.__TAURI__ = { core: { invoke: async (command, payload) => {
    calls.push({ command, payload });
    return { run_id: 'isolated-test' };
  } } };
  try {
    for (const value of [undefined, false, null, 'false', 'true', {}, true]) {
      await startDeploy('card', 'profile', value);
      await startEnable('card', value);
      const [deploy, enable] = calls.slice(-2);
      assert.equal(deploy.command, 'model_store_start_deploy');
      assert.equal(deploy.payload.profileId, 'profile');
      assert.equal(enable.command, 'model_store_start_lifecycle');
      assert.equal(enable.payload.action, 'enable');
      assert.equal(deploy.payload.memoryConfirmed, value === true);
      assert.equal(enable.payload.memoryConfirmed, value === true);
    }
  } finally {
    if (original === undefined) delete globalThis.__TAURI__;
    else globalThis.__TAURI__ = original;
  }
});

function props(language = 'zh', overrides = {}) {
  return {
    copy: dict[language].uiModelStore, theme: 'light', onClose: noop, onConfirm: noop,
    pending: {
      record: { card_name: 'Candidate' },
      admission: {
        reason: 'unverified', total_bytes: 120 * GiB, available_bytes: 50 * GiB,
        safety_reserve_bytes: 18 * GiB, candidate_peak_bytes: null,
        projected_available_bytes: null,
        running_models: [{ card_id: 'existing', card_name: 'Existing', rss_bytes: 20 * GiB }],
        ...overrides,
      },
    },
  };
}

for (const language of ['zh', 'en', 'ja']) {
  test(`${language}: risk dialogue uses complete translated copy and names both workloads`, () => {
    const input = props(language);
    const html = renderToString(React.createElement(MemoryAdmissionDialog, input));
    assert.ok(html.includes(input.copy.memoryTitle));
    assert.ok(html.includes(input.copy.memoryMessages.unverified));
    assert.ok(html.includes(input.copy.memoryUnknownPeak));
    assert.ok(html.includes(input.copy.memoryContinue));
    assert.ok(html.includes(input.copy.memoryCancel));
    assert.ok(html.includes('Candidate'));
    assert.ok(html.includes('Existing'));
    assert.doesNotMatch(html, /undefined|NaN/);
    for (const reason of ['insufficient', 'limited', 'sufficient', 'first_unverified', 'unverified', 'measurement_unavailable']) {
      assert.ok(input.copy.memoryMessages[reason], `${language}: ${reason}`);
    }
    assert.ok(input.copy.memoryCheckFailed('error').includes('error'));
  });
  test(`${language}: deployment explains that a script may start inference and uses deployment actions`, () => {
    const input = props(language);
    input.pending.action = 'deploy';
    const html = renderToString(React.createElement(MemoryAdmissionDialog, input));
    assert.ok(html.includes(input.copy.memoryDeployHint));
    assert.ok(html.includes(input.copy.memoryContinueDeploy));
    assert.ok(html.includes(input.copy.memoryCancelDeploy));
    assert.ok(html.includes(input.copy.memoryDeployCandidate('Candidate')));
  });
}

test('unknown prediction and unreadable process memory do not become zero', () => {
  const input = props('zh', { running_models: [{ card_id: 'existing', card_name: 'Existing', rss_bytes: null }] });
  const html = renderToString(React.createElement(MemoryAdmissionDialog, input));
  assert.ok(html.includes(input.copy.memoryUnreadable));
  assert.doesNotMatch(html, /启用后预计可用/);
  assert.doesNotMatch(html, /进程树当前占用 0/);
});

test('an actual zero projection is displayed as a risk, not treated as missing', () => {
  const input = props('zh', { reason: 'insufficient', candidate_peak_bytes: 60 * GiB, projected_available_bytes: 0 });
  const html = renderToString(React.createElement(MemoryAdmissionDialog, input));
  assert.ok(html.includes(input.copy.memoryMessages.insufficient));
  assert.match(html, /启用后预计可用：约 0B/);
  assert.doesNotMatch(html, /尚未提供可信的峰值/);
});

test('closed dialogue renders nothing and cannot start a model', () => {
  let confirmations = 0;
  const input = { ...props(), pending: null, onConfirm: () => { confirmations++; } };
  assert.equal(renderToString(React.createElement(MemoryAdmissionDialog, input)), '');
  assert.equal(confirmations, 0);
});

test('failed system sampling is displayed as unreadable, not zero physical memory', () => {
  const input = props('zh', {
    reason: 'measurement_unavailable', total_bytes: null, available_bytes: null,
    safety_reserve_bytes: null,
  });
  const html = renderToString(React.createElement(MemoryAdmissionDialog, input));
  assert.ok(html.includes(input.copy.memoryMessages.measurement_unavailable));
  assert.ok(html.includes(input.copy.memoryTotal(input.copy.memoryUnreadable)));
  assert.ok(html.includes(input.copy.memoryAvailable(input.copy.memoryUnreadable)));
  assert.ok(html.includes(input.copy.memoryReserve(input.copy.memoryUnreadable)));
  assert.doesNotMatch(html, /0B/);
});

test('confirmation is opt-in and cancellation never invokes it', () => {
  let confirmations = 0;
  let cancellations = 0;
  const tree = MemoryAdmissionDialog({ ...props(), onConfirm: () => { confirmations++; }, onClose: () => { cancellations++; } });
  const buttons = [];
  const walk = (node) => {
    if (!node || typeof node !== 'object') return;
    if (node.props?.tone) buttons.push(node);
    React.Children.forEach(node.props?.children, walk);
  };
  walk(tree);
  assert.equal(buttons.length, 2);
  assert.equal(confirmations, 0);
  buttons.find(button => button.props.tone === 'ghost').props.onClick();
  assert.equal(cancellations, 1);
  assert.equal(confirmations, 0);
  buttons.find(button => button.props.tone === 'danger').props.onClick();
  assert.equal(confirmations, 1);
});
