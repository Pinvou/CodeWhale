import test from 'node:test';
import assert from 'node:assert/strict';
import { resolveModelAccessState, shouldShowModelAccessGate } from '../src/shared/model-access-state.js';
import { createModelAccessCoordinator } from '../src/features/settings/model-access.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';

const carrier = { id: 'builtin_account_platform', kind: 'builtin_account_platform' };
const platform = { source: 'ACCOUNT_PLATFORM', model_id: 'catalog-model' };
const local = { id: 'local', model: 'local-model', preset: 'local_vllm' };
const cloud = { id: 'cloud', model: 'cloud-model', credential_state: 'configured' };
const snapshot = config => ({ modelsLoaded: true, savedModels: [carrier], effectiveModelConfig: config });
const state = (bs, accountStatus = 'authenticated', refreshStatus = 'ready') => resolveModelAccessState(bs, { accountStatus, refreshStatus });
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };

test('initial reads and identity recovery never create an invented guest', () => {
  for (const accountStatus of ['unknown', 'checking', 'error', 'authenticated']) {
    assert.equal(shouldShowModelAccessGate(snapshot({}), 'chat', true, false, { accountStatus }), false);
  }
  assert.equal(state({ modelsLoaded: false, effectiveModelConfig: { credential_state: 'missing' } }, 'guest').kind, 'checking');
  assert.equal(state(snapshot(null), 'guest', 'error').kind, 'read_error');
  assert.equal(state(snapshot(null), 'guest', 'ready').kind, 'read_error', 'an absent completed projection must remain retryable');
});
test('authenticated account with no concrete platform selection needs a model choice', () => {
  assert.equal(state(snapshot({ credential_mode: 'backend_managed', model: 'carrier' })).kind, 'select_model');
  assert.equal(state(snapshot({ credential_state: 'env_override', model: 'carrier' })).kind, 'select_model');
  assert.equal(state(snapshot({})).kind, 'select_model');
});
test('typed platform selection uses account auth even with missing carrier API key', () => {
  const bs = { ...snapshot({ model: 'carrier', credential_state: 'missing', requires_user_api_key: true }), defaultModelRef: platform };
  assert.equal(state(bs).kind, 'ready', 'older Native projections must also work');
  assert.equal(state(bs, 'guest').kind, 'sign_in');
  assert.equal(state(bs, 'error').kind, 'account_error');
});
test('saved local and external models preserve the workspace without an account', () => {
  for (const model of [local, cloud]) {
    const bs = { ...snapshot(model), savedModels: [carrier, model] };
    assert.equal(state(bs, 'guest').kind, 'ready');
    assert.equal(state(bs, 'error').kind, 'ready');
  }
});
test('missing current cloud key offers existing alternatives without changing model selection', () => {
  const config = { model: 'cloud', credential_state: 'missing', requires_user_api_key: true };
  const bs = { ...snapshot(config), savedModels: [carrier, local] };
  assert.equal(state(bs).kind, 'switch_model');
  assert.equal(bs.effectiveModelConfig, config);
});
test('unreadable user key is not missing, guest, or a request to overwrite it', () => {
  const bs = { ...snapshot({ model: 'cloud', credential_state: 'unavailable', has_secret: true }), savedModels: [cloud] };
  assert.equal(state(bs, 'guest').kind, 'credential_unavailable');
  assert.equal(state(bs, 'authenticated').kind, 'credential_unavailable');
});
test('late effective config cannot override the current session model reference', () => {
  const bs = { ...snapshot({ model_ref: platform, model: 'old' }), currentSessionModelRef: { source: 'LOCAL_CONFIG', saved_model_id: 'local' } };
  assert.equal(state(bs).kind, 'checking');
});
test('history, remote sessions, and settings stay reachable without a model', () => {
  const bs = snapshot({}); const access = { accountStatus: 'guest' };
  assert.equal(shouldShowModelAccessGate(bs, 'chat', true, false, access), true);
  assert.equal(shouldShowModelAccessGate({ ...bs, activeSessionId: 'history' }, 'chat', true, false, access), false);
  assert.equal(shouldShowModelAccessGate(bs, 'chat', true, true, access), false);
  assert.equal(shouldShowModelAccessGate(bs, 'settings', true, false, access), false);
});
test('all password, SMS, and logout mutations refresh the shared model-access owner', async () => {
  let authenticated = false; let modelReads = 0;
  const client = new AccountPlatformDesktopClient({
    session: async () => ({ authenticated }), login: async () => { authenticated = true; },
    loginSms: async () => { authenticated = true; }, logout: async () => { authenticated = false; },
  });
  const owner = createModelAccessCoordinator({ loadModels: async () => { modelReads++; return true; }, loadEffectiveModelConfig: async () => true }, client);
  await owner.refresh(); assert.equal(owner.getState().accountStatus, 'guest');
  await client.login('sample', 'synthetic'); await owner.refresh(); assert.equal(owner.getState().accountStatus, 'authenticated');
  await client.logout(); await owner.refresh(); assert.equal(owner.getState().accountStatus, 'guest');
  await client.loginSms('13800000000', 'synthetic-challenge', '000000'); await owner.refresh(); assert.equal(owner.getState().accountStatus, 'authenticated');
  assert.ok(modelReads >= 4); owner.dispose(); assert.equal(client.authenticationListeners.size, 0);
});
test('a late startup guest cannot replace the account that just logged in', async () => {
  const old = deferred(); let calls = 0, notify;
  const client = { session: () => ++calls === 1 ? old.promise : Promise.resolve({ authenticated: true }), onAuthenticationChange: fn => { notify = fn; return () => {}; } };
  const models = { loadModels: async () => true, loadEffectiveModelConfig: async () => true };
  const owner = createModelAccessCoordinator(models, client);
  const initial = owner.refresh(); await Promise.resolve(); notify('changing'); notify('changed'); await owner.refresh();
  old.resolve({ authenticated: false }); assert.equal(await initial, null);
  assert.equal(owner.getState().accountStatus, 'authenticated'); owner.dispose();
});
test('account errors and partial model-refresh failures remain retryable and never become guest', async () => {
  let failed = true;
  const owner = createModelAccessCoordinator({ loadModels: async () => !failed, loadEffectiveModelConfig: async () => true }, { session: async () => failed ? { authenticated: false, errorCode: 'credential_store_unavailable' } : { authenticated: true } });
  await owner.refresh(); assert.deepEqual(owner.getState(), { accountStatus: 'error', refreshStatus: 'error' });
  failed = false; await owner.refresh(); assert.deepEqual(owner.getState(), { accountStatus: 'authenticated', refreshStatus: 'ready' }); owner.dispose();
});
test('a disposed or superseded owner cannot publish its late read', async () => {
  const pending = deferred(); let updates = 0;
  const owner = createModelAccessCoordinator({ loadModels: async () => true, loadEffectiveModelConfig: async () => true }, { session: () => pending.promise });
  owner.subscribe(() => updates++); const read = owner.refresh(); owner.dispose(); const before = updates;
  pending.resolve({ authenticated: true }); await read; assert.equal(updates, before);
});

test('draft model choice is resolved before a session exists without reusing the global credential', () => {
  const bs = { ...snapshot({ model: 'old-cloud', credential_state: 'missing', model_ref: platform }), savedModels: [carrier, local], pendingDraftModelRef: { source: 'LOCAL_CONFIG', saved_model_id: local.id } };
  assert.equal(state(bs, 'guest').kind, 'ready');
});

test('logout completion retires an already running account read', async () => {
  const old = deferred(); let notify, calls = 0;
  const owner = createModelAccessCoordinator({ loadModels: async () => true, loadEffectiveModelConfig: async () => true }, {
    session: () => ++calls === 1 ? old.promise : Promise.resolve({ authenticated: false }),
    onAuthenticationChange: callback => { notify = callback; return () => {}; },
  });
  const stale = owner.refresh(); await Promise.resolve(); notify('changed'); await owner.refresh();
  old.resolve({ authenticated: true }); await stale;
  assert.equal(owner.getState().accountStatus, 'guest'); owner.dispose();
});
