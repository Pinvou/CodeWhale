#!/usr/bin/env node
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const sourcePath = path.join(__dirname, '..', 'src', 'hooks', 'useBridge.js');
const source = fs.readFileSync(sourcePath, 'utf8');
const bridgeSource = fs.readFileSync(
  path.join(__dirname, '..', 'src', 'platform', 'tauri', 'bridge', 'settings.js'),
  'utf8',
);
const start = source.indexOf('function baseUrlIsLoopback(');
const end = source.indexOf('\nexport {', start);

assert.notStrictEqual(start, -1, 'baseUrlIsLoopback must exist');
assert.notStrictEqual(end, -1, 'credential helper boundary must exist');

const modelSource = fs.readFileSync(path.join(__dirname, '..', 'src', 'shared', 'model-options.js'), 'utf8');
const helperStart = modelSource.indexOf('function isBuiltInAccountPlatformModel(');
const helperEnd = modelSource.indexOf('\n}', helperStart) + 2;
assert.notStrictEqual(helperStart, -1);
const context = { URL };
vm.createContext(context);
vm.runInContext(modelSource.slice(helperStart, helperEnd), context);
vm.createContext(context);
vm.runInContext(
  `${source.slice(start, end)}
this.baseUrlIsLoopback = baseUrlIsLoopback;
this.isLocalModel = isLocalModel;
this.shouldShowModelAccessGate = shouldShowModelAccessGate;`,
  context,
  { filename: sourcePath },
);

const { baseUrlIsLoopback, isLocalModel, shouldShowModelAccessGate } = context;
const state = (credentialState, config = {}) => ({
  effectiveModelConfig: {
    preset: 'deepseek',
    base_url: 'https://api.deepseek.com',
    credential_state: credentialState,
    ...config,
  },
});

assert.strictEqual(baseUrlIsLoopback('http://localhost:8000/v1'), true);
assert.strictEqual(baseUrlIsLoopback('http://localhost.:8000/v1'), true);
assert.strictEqual(baseUrlIsLoopback('http://127.0.0.42:8000/v1'), true);
assert.strictEqual(baseUrlIsLoopback('http://[::1]:8000/v1'), true);
assert.strictEqual(baseUrlIsLoopback('https://localhost.example.com/v1'), false);
assert.strictEqual(baseUrlIsLoopback('https://127.0.0.10.example.com/v1'), false);
assert.strictEqual(baseUrlIsLoopback('not a url'), false);

assert.strictEqual(isLocalModel({ preset: 'local_vllm', base_url: '' }), true);
assert.strictEqual(isLocalModel({ preset: 'openai_compatible', base_url: 'http://localhost:11434/v1' }), true);
assert.strictEqual(isLocalModel({ preset: 'openai_compatible', base_url: 'https://api.openai.com/v1' }), false);

assert.strictEqual(shouldShowModelAccessGate(state('missing'), 'chat', true), true);
assert.strictEqual(shouldShowModelAccessGate({modelsLoaded:false,savedModels:[]}, 'chat', true), false,
  'an unreturned catalog must not invent a first-use failure');
assert.strictEqual(shouldShowModelAccessGate({modelsLoaded:true,savedModels:[{id:'builtin_account_platform',kind:'builtin_account_platform'}]}, 'chat', true), true,
  'the managed carrier alone is not a concrete callable selection');
assert.strictEqual(shouldShowModelAccessGate({modelsLoaded:true,savedModels:[{id:'custom'}]}, 'chat', true), false,
  'a configured external model remains reachable without managed account login');
assert.strictEqual(shouldShowModelAccessGate(state('unavailable'), 'chat', true), true);
assert.strictEqual(shouldShowModelAccessGate(state('configured'), 'chat', true), false);
assert.strictEqual(
  shouldShowModelAccessGate(state('missing', {
    credential_mode: 'backend_managed',
    requires_user_api_key: false,
  }), 'chat', true),
  false,
  'runtime-managed credentials must not open the user API Key gate',
);
assert.strictEqual(
  shouldShowModelAccessGate(state('unavailable', {
    credential_mode: 'backend_managed',
    requires_user_api_key: false,
  }), 'chat', true),
  false,
  'temporary backend credential failures must stay in the backend-managed flow',
);
assert.strictEqual(
  shouldShowModelAccessGate(state('unavailable', {
    credential_mode: 'none',
    requires_user_api_key: false,
  }), 'chat', true),
  false,
  'no-auth models must not open the user API Key gate',
);
assert.strictEqual(shouldShowModelAccessGate(state('missing'), 'settings', true), false);
assert.strictEqual(shouldShowModelAccessGate(state('missing'), 'chat', false), false);
assert.strictEqual(
  shouldShowModelAccessGate(state('missing', {
    preset: 'openai_compatible',
    base_url: 'http://127.0.0.1:11434/v1',
  }), 'chat', true),
  false,
);
assert.strictEqual(
  shouldShowModelAccessGate({
    ...state('missing'),
    scheduledRunContext: { id: 'run-1' },
  }, 'scheduled', true),
  true,
);
assert.strictEqual(shouldShowModelAccessGate(state('missing'), 'scheduled', true), false);

// 会话切换/热切模型必须把 sessionId 传给后端重新解析真正生效的模型，不能继续沿用全局默认。
assert.match(
  bridgeSource,
  /invoke\("get_effective_model_config", \{ sessionId: requestedSessionId \}\)/,
  'loadSessionModel must refresh effective config for the requested session',
);
assert.match(
  bridgeSource,
  /await loadSessionModel\(sessionId\)/,
  'switchModel must refresh the session-scoped credential gate after switching',
);

console.log('api_key_gate_logic: ok');
