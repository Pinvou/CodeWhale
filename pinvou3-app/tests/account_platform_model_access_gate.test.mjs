import test from 'node:test';
import assert from 'node:assert/strict';
globalThis.window = {};
const { shouldShowModelAccessGate } = await import('../src/hooks/useBridge.js');
const carrier = { id: 'builtin_account_platform', kind: 'builtin_account_platform', readonly: true, system: true };
test('the platform carrier without a concrete selection is not a usable model', () => {
  const state = { modelsLoaded: true, savedModels: [carrier], defaultModelRef: null, effectiveModelConfig: null };
  assert.equal(shouldShowModelAccessGate(state, 'chat', true), true);
});
test('current selected platform config and ordinary local model remain usable', () => {
  assert.equal(shouldShowModelAccessGate({ modelsLoaded: true, savedModels: [carrier], effectiveModelConfig: { model: 'current-model-id', requires_user_api_key: false } }, 'chat', true), false);
  assert.equal(shouldShowModelAccessGate({ modelsLoaded: true, savedModels: [{ id: 'local-user-model', model: 'actual-local-model' }] }, 'chat', true), false);
});
test('settings and unsupported transports stay reachable while no model is selected', () => {
  const state = { modelsLoaded: true, savedModels: [carrier], defaultModelRef: null };
  assert.equal(shouldShowModelAccessGate(state, 'settings', true), false);
  assert.equal(shouldShowModelAccessGate(state, 'chat', false), false);
});
