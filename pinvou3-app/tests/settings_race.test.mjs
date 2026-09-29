/**
 * 同类竞态回归测试（PR #250 审计 → #260 settings 域）：
 * 陈旧读取覆盖 / await 后写入漂移——修复后的行为快照。
 * 覆盖 tauri settings 的模型列表加载。
 */
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const bridgeDir = path.join(here, '..', 'src', 'platform', 'tauri', 'bridge');

/** 通用 feature 装载器：vm 加载 IIFE(window) 形态的桥 feature 文件。 */
function loadFeature(fileName, state, contextOverrides) {
  const root = {};
  const src = fs.readFileSync(path.join(bridgeDir, fileName), 'utf8');
  vm.runInNewContext(src, {
    window: root,
    globalThis: root,
    setTimeout,
    clearTimeout,
  });
  const factory = root.__PINVOU_TAURI_BRIDGE_FEATURES__[fileName.replace('.js', '')];
  const deferreds = {};
  const calls = { invoke: [] };
  const api = factory(Object.assign({
    state,
    notify() { calls.notify = (calls.notify || 0) + 1; },
    bt(key) { return key; },
    addSystemItem() {},
    addChatItem() {},
    timeStr() { return ''; },
    invoke(name, args) {
      calls.invoke.push(name);
      if (deferreds[name] && deferreds[name].promise) return deferreds[name].promise;
      return Promise.resolve({});
    },
  }, contextOverrides || {}));
  return {
    api,
    state,
    calls,
    defer(name) {
      // 每次创建全新对象：同一 invoke 名的第二次 defer 不得覆盖第一次的
      // resolve（否则旧 promise 永远无人 resolve → 测试挂起）。
      const d = {};
      d.promise = new Promise((resolve, reject) => { d.resolve = resolve; d.reject = reject; });
      deferreds[name] = d;
      return d;
    },
  };
}

// ── settings.js：loadModels ────────────────────────────────────────

function loadSettingsFeature() {
  const state = { settings: {} };
  return loadFeature('settings.js', state, { listen() {} });
}

test('model refresh loaders return explicit success and preserve the last authority on swallowed failures', async () => {
  const success = loadSettingsFeature();
  assert.equal(await success.api.loadModels(), true);
  assert.equal(await success.api.loadEffectiveModelConfig(), true);
  assert.equal(success.state.modelsLoaded, true);

  const modelFailure = loadSettingsFeature();
  modelFailure.state.savedModels = [{ id: 'known-model' }];
  modelFailure.state.activeModelId = 'known-model';
  modelFailure.state.modelsLoaded = true;
  const failedModels = modelFailure.defer('list_models');
  const modelResult = modelFailure.api.loadModels();
  failedModels.reject(new Error('catalog unavailable'));
  assert.equal(await modelResult, false);
  assert.equal(modelFailure.state.modelsLoaded, true);
  assert.equal(modelFailure.state.savedModels[0].id, 'known-model');

  const configFailure = loadSettingsFeature();
  configFailure.state.effectiveModelConfig = { credential_state: 'missing' };
  const failedConfig = configFailure.defer('get_effective_model_config');
  const configResult = configFailure.api.loadEffectiveModelConfig();
  failedConfig.reject(new Error('effective config unavailable'));
  assert.equal(await configResult, false);
  assert.equal(configFailure.state.effectiveModelConfig.credential_state, 'missing');
});

test('loadEffectiveModelConfig is last-request-wins for the same active session', async () => {
  const rt = loadSettingsFeature();
  const first = rt.defer('get_effective_model_config');
  const firstLoad = rt.api.loadEffectiveModelConfig();
  const second = rt.defer('get_effective_model_config');
  const secondLoad = rt.api.loadEffectiveModelConfig();
  second.resolve({ model: 'new-model', credential_state: 'available' });
  assert.equal(await secondLoad, true);
  first.resolve({ model: 'stale-model', credential_state: 'missing' });
  assert.equal(await firstLoad, false);
  assert.equal(rt.state.effectiveModelConfig.model, 'new-model');
  assert.equal(rt.state.effectiveModelConfig.credential_state, 'available');
});

test('loadSessionModel cannot overwrite a newer authenticated effective-config refresh', async () => {
  const rt = loadSettingsFeature();
  rt.state.activeSessionId = 'session-1';
  const staleSessionConfig = rt.defer('get_effective_model_config');
  const sessionLoad = rt.api.loadSessionModel('session-1');
  const authenticatedConfig = rt.defer('get_effective_model_config');
  const authenticatedLoad = rt.api.loadEffectiveModelConfig('session-1');

  authenticatedConfig.resolve({ model: 'platform-model', credential_state: 'available' });
  assert.equal(await authenticatedLoad, true);
  staleSessionConfig.resolve({ model: 'old-model', credential_state: 'missing' });
  assert.equal(await sessionLoad, false);
  assert.equal(rt.state.effectiveModelConfig.model, 'platform-model');
  assert.equal(rt.state.effectiveModelConfig.credential_state, 'available');
});

test('loadModels 后发者胜（旧列表不得覆盖新列表）', async () => {
  const rt = loadSettingsFeature();
  const d1 = rt.defer('list_models');
  const p1 = rt.api.loadModels();                    // 加载 1（序号 1）
  const d2 = rt.defer('list_models');
  const p2 = rt.api.loadModels();                    // 加载 2（序号 2）
  d1.resolve({ models: [{ id: 'old' }], active_model_id: 'old' }); // 旧响应
  await p1;
  assert.equal(rt.state.savedModels, undefined, '陈旧列表不得写入');
  assert.equal(rt.state.activeModelId, undefined, '陈旧 activeModelId 不得写入');
  d2.resolve({ models: [{ id: 'new' }], active_model_id: 'new' }); // 新响应
  await p2;
  assert.equal(rt.state.savedModels[0].id, 'new', '新列表正常写入');
  assert.equal(rt.state.activeModelId, 'new', '新 activeModelId 正常写入');
});

test('loadModels 陈旧失败不覆盖（后发者胜同样适用于 catch 分支）', async () => {
  const rt = loadSettingsFeature();
  const d1 = rt.defer('list_models');
  const p1 = rt.api.loadModels();                    // 加载 1（序号 1）
  const d2 = rt.defer('list_models');
  const p2 = rt.api.loadModels();                    // 加载 2（序号 2）
  d2.resolve({ models: [{ id: 'new' }], active_model_id: 'new' }); // 新响应先落地
  await p2;
  assert.equal(rt.state.savedModels[0].id, 'new', '新列表正常写入');
  d1.reject(new Error('stale network failure'));     // 旧请求后失败
  await p1.catch(function () { /* 已知 reject */ });
  assert.equal(rt.state.savedModels[0].id, 'new', '陈旧失败不得清空新列表');
  assert.equal(rt.state.activeModelId, 'new', '陈旧失败不得覆盖 activeModelId');
});
