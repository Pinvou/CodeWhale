#!/usr/bin/env node
/**
 * tauri 桥 state 订阅身份契约(subscribeStateSlice / subscribeStateSlices):
 *
 *   - 无关 publication(chat 域的 composerPrefill)不得换掉 settings/models
 *     的单域切片与多域组合快照身份——React 借 Object.is 跳过无关渲染;
 *   - 回调仍每轮 publication 都触发,通知不丢;
 *   - 订阅域真实变化时,组合快照必须换新身份并携带新值,其中未变化域的
 *     切片身份保持不变。
 *
 * 与 web_bridge_domain_contract.test.mjs 的跨域身份断言互为镜像(web 端
 * stablePick 已锁定同款语义)。vm 整载 platform/tauri/bridge.js,以假
 * __TAURI__ 驱动公开方法,不 mock 订阅机制本身。
 *
 * Run: node --test pinvou3-app/tests/tauri_bridge_state_identity.test.mjs
 */
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '..');

let invokeResponse = async () => null;
const localStorage = {
  getItem: () => null,
  setItem() {},
  removeItem() {},
};
const windowObject = {
  __TAURI__: {
    core: { invoke: (...args) => invokeResponse(...args) },
    event: { listen: async () => () => {} },
    dialog: { open: async () => null },
  },
  performance: { now: () => 0 },
  addEventListener() {},
  localStorage,
  setTimeout,
  clearTimeout,
};
const context = vm.createContext({
  window: windowObject,
  document: {
    addEventListener() {},
    createElement: () => ({ style: {} }),
    body: { appendChild() {} },
  },
  navigator: {},
  console,
  setTimeout,
  clearTimeout,
  // 加载期特性脚本可能注册常驻轮询;真定时器会钉住事件循环,
  // 测试场景用不到轮询,替换为不调度的桩。
  setInterval: () => 0,
  clearInterval() {},
  structuredClone,
  localStorage,
});

// 页面加载序:shared/bridge-messages.js → platform/tauri/bridge/*.js 工厂
// → bridge.js(顺序断言在 classic_startup_bundle.test.mjs;这里解析
// index.html 取 bridge 脚本序,特性文件增减时自动跟随)。
const indexHtml = fs.readFileSync(path.join(root, 'src/index.html'), 'utf8');
const bridgeScripts = [...indexHtml.matchAll(/<script src="%BASE_URL%(shared\/bridge-messages\.js|platform\/tauri\/bridge(?:\.js|\/[^"]+))"[^>]*><\/script>/g)]
  .map(match => match[1]);
assert.ok(bridgeScripts.includes('platform/tauri/bridge.js'), 'index.html must load platform/tauri/bridge.js');
for (const relative of bridgeScripts) {
  vm.runInContext(
    fs.readFileSync(path.join(root, 'src', relative), 'utf8'),
    context,
    { filename: relative },
  );
}

const api = windowObject.TauriBridge;
assert.ok(api.available, 'desktop bridge must expose the real surface under a stubbed __TAURI__');

const combinedSnapshots = [];
const unsubscribeCombined = api.state.subscribeMany(['settings', 'models'], snapshot => {
  combinedSnapshots.push(snapshot);
});
const settingsSnapshots = [];
const unsubscribeSettings = api.state.subscribe('settings', snapshot => {
  settingsSnapshots.push(snapshot);
});

// 首轮 publication:chat 域变化,两个订阅都收到首份快照。
api.chat.prefillComposer('draft-1');
assert.equal(combinedSnapshots.length, 1, 'subscribeMany must deliver the first publication');
assert.equal(settingsSnapshots.length, 1, 'subscribe must deliver the first publication');
assert.ok(Object.isFrozen(combinedSnapshots[0]), 'the combined snapshot must be frozen');

// 第二轮仍是 chat-only publication:回调必须照常触发(通知不丢),
// 但无关域的身份必须稳定,让 React 能跳过无关渲染。
api.chat.prefillComposer('draft-2');
assert.equal(combinedSnapshots.length, 2, 'callbacks must fire on every publication');
assert.equal(settingsSnapshots.length, 2, 'single-domain callbacks must fire on every publication');
assert.equal(combinedSnapshots[0], combinedSnapshots[1],
  'a chat-only publication must preserve an unrelated multi-domain combined snapshot identity');
assert.equal(settingsSnapshots[0], settingsSnapshots[1],
  'a chat-only publication must preserve an unrelated single-domain snapshot identity');

// 订阅域(models)真实变化:组合快照必须换新身份并携带新值;
// 未变化的 settings 切片在重组的组合对象里保持身份。
invokeResponse = async () => ({ models: [{ id: 'm1', name: 'Model One' }], active_model_id: 'm1' });
await api.models.loadModels();
assert.equal(combinedSnapshots.length, 3, 'a subscribed-domain change must publish a new combined snapshot');
assert.notEqual(combinedSnapshots[1], combinedSnapshots[2],
  'a subscribed-domain change must rebuild the combined snapshot identity');
assert.equal(combinedSnapshots[2].activeModelId, 'm1',
  'the rebuilt combined snapshot must carry the changed models value');
assert.ok(Object.isFrozen(combinedSnapshots[2]), 'the rebuilt combined snapshot must be frozen');
assert.equal(combinedSnapshots[2].settings, combinedSnapshots[1].settings,
  'the unchanged settings slice must keep its identity inside the rebuilt combined snapshot');

// 精确领域失效还必须跳过未变化领域的结构遍历，而不只是最终返回同一引用。
// model 原始对象上的 getter 是确定性的遍历探针：models publication 会读取它；
// 后续 monitor-only publication 仍调用 models 订阅回调，但应直接命中 models
// slice 缓存，不能再次读取源对象。
let modelProbeReads = 0;
const modelWithTraversalProbe = { id: 'm2', name: 'Model Two' };
Object.defineProperty(modelWithTraversalProbe, 'traversal_probe', {
  enumerable: true,
  get() {
    modelProbeReads += 1;
    return 'stable';
  },
});
invokeResponse = async command => command === 'list_models'
  ? { models: [modelWithTraversalProbe], active_model_id: 'm2' }
  : {};
await api.models.loadModels();
assert.ok(modelProbeReads > 0, 'the models publication must materialize the models slice');
// Drain unrelated fire-and-forget work started by earlier model APIs before
// taking the traversal baseline for the monitor-only publication.
for (let attempt = 0; attempt < 5; attempt += 1) {
  await new Promise(resolve => setTimeout(resolve, 0));
}
const readsAfterModelsPublication = modelProbeReads;
const combinedSnapshotBeforeMonitor = combinedSnapshots.at(-1);
const combinedCallbackCountBeforeMonitor = combinedSnapshots.length;

const monitorSnapshot = {
  app: null,
  cpu: null,
  gpu: null,
  ram: null,
  self_perf: null,
  vllm: null,
};
invokeResponse = async command => command === 'get_monitor_snapshot' ? monitorSnapshot : {};
const monitorSnapshots = [];
const unsubscribeMonitor = api.state.subscribe('monitor', snapshot => {
  monitorSnapshots.push(snapshot);
});
api.monitor.startMonitorPolling();
for (let attempt = 0; attempt < 20
  && !monitorSnapshots.some(snapshot => snapshot.monitor?._fmt); attempt += 1) {
  await new Promise(resolve => setTimeout(resolve, 0));
}
assert.ok(monitorSnapshots.some(snapshot => snapshot.monitor?._fmt),
  'monitor polling must publish its first snapshot');
assert.equal(combinedSnapshots.length, combinedCallbackCountBeforeMonitor + 1,
  'precise invalidation must preserve the every-publication callback contract');
assert.equal(combinedSnapshots.at(-1), combinedSnapshotBeforeMonitor,
  'monitor-only publication must preserve the unrelated models snapshot identity');
assert.equal(modelProbeReads, readsAfterModelsPublication,
  'monitor-only publication must not traverse the unchanged models source');

// Monitor owns the displayed snapshot, but a newly discovered context window
// also updates chat.tokens.max. The precise publication must invalidate both
// domains so the token progress denominator cannot become stale.
const chatSnapshots = [];
const unsubscribeChat = api.state.subscribe('chat', snapshot => {
  chatSnapshots.push(snapshot);
});
const monitorWithContextLimit = {
  ...monitorSnapshot,
  vllm: {
    configured_model: 'model-two',
    health_status: 'verified',
    max_model_len: 77777,
    metrics_applicable: false,
    model: 'model-two',
    status: 'ready',
    target_kind: 'local',
  },
};
invokeResponse = async command => command === 'get_monitor_snapshot' ? monitorWithContextLimit : {};
api.monitor.startMonitorPolling();
for (let attempt = 0; attempt < 20
  && !chatSnapshots.some(snapshot => snapshot.tokens?.max === 77777); attempt += 1) {
  await new Promise(resolve => setTimeout(resolve, 0));
}
assert.ok(chatSnapshots.some(snapshot => snapshot.tokens?.max === 77777),
  'monitor context discovery must invalidate the chat token slice');

unsubscribeChat();
unsubscribeMonitor();
const combinedCountBeforeUnsubscribe = combinedSnapshots.length;
const settingsCountBeforeUnsubscribe = settingsSnapshots.length;
unsubscribeCombined();
unsubscribeSettings();
api.chat.prefillComposer('draft-3');
assert.equal(combinedSnapshots.length, combinedCountBeforeUnsubscribe,
  'unsubscribe must stop subscribeMany deliveries');
assert.equal(settingsSnapshots.length, settingsCountBeforeUnsubscribe,
  'unsubscribe must stop subscribe deliveries');
