import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import vm from 'node:vm';
import { builtinModelCatalog, isBuiltInAccountPlatformModel, selectableModels } from '../src/shared/model-options.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';

test('ordinary and account models remain selectable independently of cached login and balance', () => {
  const models = [{ id: 'builtin_account_platform', kind: 'builtin_account_platform' },
    { id: 'custom-one', model: 'custom-wire' }, { id: 'custom-two', model: 'custom-wire' }];
  assert.deepEqual(selectableModels(models), models);
  assert.equal(isBuiltInAccountPlatformModel(models[0]), true);
  assert.equal(isBuiltInAccountPlatformModel(models[1]), false);
});

test('a current directory can report no authorized selection without selecting a fallback and filters unknown fields', () => {
  const input = { available_models: ['wire-a', 'wire-b'], default_model: '',
    private_field: 'PUBLIC_TEST_SENTINEL' };
  const result = builtinModelCatalog(input);
  assert.deepEqual(result, { available_models: ['wire-a', 'wire-b'], default_model: '' });
  result.available_models.push('local-change');
  assert.deepEqual(input.available_models, ['wire-a', 'wire-b']);
  assert.deepEqual(builtinModelCatalog({ available_models: [], default_model: '' }),
    { available_models: [], default_model: '' });
});

test('unknown, oversized or malformed model directories fail closed', () => {
  for (const value of [null, [], {}, { available_models: ['a'], default_model: 'b' },
    { available_models: ['a', 'a'], default_model: 'a' },
    { available_models: [' '], default_model: 'a' },
    { available_models: ['あ'.repeat(1024 * 1024)], default_model: 'a' },
    { available_models: Array.from({ length: 4097 }, (_, index) => 'wire-' + index), default_model: 'a' },
    { available_models: ['a'], default_model: false }]) {
    assert.throws(() => builtinModelCatalog(value), { code: 'account_platform_request_failed' });
  }
});

test('existing long model IDs and internal spaces retain their exact wire identity', () => {
  const models = ['a'.repeat(201), 'あ'.repeat(100), 'existing model name', 'existing\u0001name'];
  assert.deepEqual(builtinModelCatalog({ available_models: models, default_model: models[0] }),
    { available_models: models, default_model: models[0] });
  const maximum = Array.from({ length: 4096 }, (_, index) => 'wire-' + index);
  assert.equal(builtinModelCatalog({ available_models: maximum, default_model: 'wire-0' }).available_models.length, 4096);
});

test('wire selection uses the account transport and validates exact membership and confirmation', async () => {
  const calls = [];
  const client = new AccountPlatformDesktopClient({
    getModelCatalog: async () => ({ available_models: ['a', 'b'], default_model: 'a' }),
    setDefaultModel: async model => {
      calls.push(model);
      return { available_models: ['a', 'b'], default_model: model };
    },
  });
  assert.equal((await client.getModelCatalog()).default_model, 'a');
  assert.equal((await client.setDefaultModel('b')).default_model, 'b');
  assert.deepEqual(calls, ['b']);
  for (const result of [
    { available_models: ['a', 'b'], default_model: 'a' },
    { available_models: ['a'], default_model: 'b' },
  ]) {
    const wrong = new AccountPlatformDesktopClient({ setDefaultModel: async () => result });
    await assert.rejects(wrong.setDefaultModel('b'), { code: 'account_platform_request_failed' });
  }
});

test('late directory and selection outcomes are hidden after cancellation without pretending to cancel native work', async () => {
  for (const method of ['getModelCatalog', 'setDefaultModel']) {
    for (const failure of [false, true]) {
      let settle;
      const promise = new Promise((resolve, reject) => { settle = failure ? reject : resolve; });
      const client = new AccountPlatformDesktopClient({ [method]: () => promise });
      const controller = new AbortController();
      const operation = method === 'getModelCatalog'
        ? client.getModelCatalog(controller.signal) : client.setDefaultModel('a', controller.signal);
      controller.abort();
      settle(failure ? 'authentication_required: PRIVATE_TEST_SENTINEL'
        : { available_models: ['a'], default_model: 'a' });
      await assert.rejects(operation, { name: 'AbortError' });
    }
  }
});

test('catalog errors preserve authentication classification without provider detail', async () => {
  // Tauri Result<String> rejects with its native public string protocol.
  const nativeFailure = 'authentication_required: PRIVATE_TEST_SENTINEL';
  const client = new AccountPlatformDesktopClient({ getModelCatalog: async () => {
    return Promise.reject(nativeFailure);
  } });
  await assert.rejects(client.getModelCatalog(), error => error.code === 'authentication_required'
    && error.status === 401 && error.message === 'authentication_required');
});

test('the actual account bridge invalidates catalog ownership around identity changes without exposing identity data', async () => {
  const source = fs.readFileSync(new URL('../src/platform/tauri/bridge/account-platform.js', import.meta.url), 'utf8');
  for (const method of ['login', 'loginSms', 'logout', 'request']) {
    for (const failed of [false, true]) {
      const window = {};
      vm.runInNewContext(source, { window, URL });
      let settle;
      const pending = new Promise((resolve, reject) => { settle = failed ? reject : resolve; });
      const commands = [];
      const transport = window.__PINVOU_TAURI_BRIDGE_FEATURES__['account-platform']({
        invoke(command, args) { commands.push({ command, args }); return pending; },
      });
      const changes = [];
      const unsubscribe = transport.subscribeIdentityChange(value => changes.push(value));
      const operation = method === 'request' ? transport.request({ area: 'account', method: 'POST',
        resource: '/v1/sessions/current/active-context', body: { context_type: 'PERSONAL' } })
        : transport[method]({ account: 'public-fixture', password: 'public-fixture-password' });
      assert.deepEqual(changes, [false, true]);
      settle(failed ? 'credential_store_failed: PUBLIC_SENTINEL' : { public_result: true });
      if (failed) await assert.rejects(operation);
      else await operation;
      assert.deepEqual(changes, [false, true, false]);
      assert.ok(changes.every(value => typeof value === 'boolean'));
      assert.equal(commands.length, 1, 'one identity action must invoke exactly one native operation');
      unsubscribe();
    }
  }
});

test('temporary failures of ordinary requests leave catalog ownership intact while confirmed auth failures invalidate it', async () => {
  const source = fs.readFileSync(new URL('../src/platform/tauri/bridge/account-platform.js', import.meta.url), 'utf8');
  const window = {};
  vm.runInNewContext(source, { window, URL });
  let failure = 'oidc_token_endpoint_unavailable: PUBLIC_SENTINEL';
  const transport = window.__PINVOU_TAURI_BRIDGE_FEATURES__['account-platform']({
    invoke() { return Promise.reject(failure); },
  });
  const changes = [];
  transport.subscribeIdentityChange(value => changes.push(value));
  const request = { area: 'token', method: 'GET', resource: '/v1/me/balance' };
  await assert.rejects(transport.request(request));
  assert.deepEqual(changes, [false]);
  failure = 'authentication_required: PUBLIC_SENTINEL';
  await assert.rejects(transport.request(request));
  assert.deepEqual(changes, [false, false]);
});

test('catalog invalidation uses the native canonical resource without changing the invoke payload', async () => {
  const source = fs.readFileSync(new URL('../src/platform/tauri/bridge/account-platform.js', import.meta.url), 'utf8');
  for (const [resource, method, expected] of [
    ['/v1/sessions/current/ignored/../active-context/', 'POST', true],
    ['/v1/sessions/current/./active-context', 'POST', true],
    ['/v1/account/ignored/../bootstrap/', 'POST', true],
    ['/v1/account/bootstrap?', 'POST', false],
    ['/v1/account/bootstrap#', 'POST', false],
    ['/v1/sessions/current/active-context', 'post', false],
    ['/v1/me/organizations', 'GET', false],
  ]) {
    const window = {};
    vm.runInNewContext(source, { window, URL });
    const commands = [];
    const transport = window.__PINVOU_TAURI_BRIDGE_FEATURES__['account-platform']({
      async invoke(command, args) { commands.push({ command, args }); return { status: 204 }; },
    });
    const changes = [];
    transport.subscribeIdentityChange(value => changes.push(value));
    const request = { area: 'account', method, resource, body: { context_type: 'PERSONAL' } };
    await transport.request(request);
    assert.deepEqual(changes, expected ? [false, true, false] : [false]);
    assert.equal(commands.length, 1);
    assert.equal(commands[0].args.request, request);
  }
});
