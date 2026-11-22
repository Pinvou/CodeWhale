import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
const bridge = readFileSync(new URL('../src/platform/tauri/bridge.js', import.meta.url), 'utf8');
const fallback = readFileSync(new URL('../src/shared/markdown-bridge-fallback.js', import.meta.url), 'utf8');
// #701 T4b 壳化后 renderMarkdown 等委托 window.PinvouBridgeShared（index.html
// 在两个桥之前先载共享包）；vm 装载须遵循同一载入序。独立预览 facade 的
// 安全合同（无后端能力、escapeHtml 转义、渲染器优先级）保持原断言。
const sharedHelpers = readFileSync(new URL('../src/shared/bridge-shared-helpers.js', import.meta.url), 'utf8');
function load(window, before) {
  const context = vm.createContext({ window, console: { warn() {} } });
  vm.runInContext(sharedHelpers, context, { timeout: 1000 });
  if (before) vm.runInContext(before, context, { timeout: 1000 });
  vm.runInContext(bridge, context, { timeout: 1000 });
  return window.TauriBridge;
}
test('actual standalone bridge preview exposes no backend capabilities and escapes raw HTML', async () => {
  const bridge = load({});
  assert.equal(bridge.available, false);
  assert.deepEqual(Object.keys(bridge).sort(), ['available', 'lifecycle', 'rendering', 'state']);
  await bridge.lifecycle.init();
  assert.deepEqual(Object.keys(bridge.state.get()), []);
  assert.equal(typeof bridge.state.subscribe(() => {}), 'function');
  const raw = `<img src=x onerror="alert(1)"><script>alert(2)</script>&'`;
  const html = bridge.rendering.renderMarkdown(raw);
  assert.equal(html, '&lt;img src=x onerror=&quot;alert(1)&quot;&gt;&lt;script&gt;alert(2)&lt;/script&gt;&amp;&#39;');
  assert.equal(bridge.accountPlatform, undefined);
});
test('standalone preview reuses the actual shared plain-script safe fallback', () => {
  const bridge = load({}, fallback);
  assert.equal(bridge.rendering.renderMarkdown('<iframe src=x>'), '&lt;iframe src=x&gt;');
});
test('standalone preview keeps the existing authoritative renderer precedence', () => {
  const calls = [];
  const window = {
    PinvouMarkdownRenderer: { renderMarkdown(text) { calls.push(text); return 'controlled sanitized markup'; } },
    PinvouMarkdownBridgeFallback: { renderMarkdown() { throw new Error('lower-priority renderer used'); } },
  };
  const bridge = load(window);
  assert.equal(bridge.rendering.renderMarkdown('controlled input'), 'controlled sanitized markup');
  assert.deepEqual(calls, ['controlled input']);
});
test('actual desktop-bridge Web early guard retains the existing Web transport', () => {
  const current = { publicWeb: true };
  const window = { PinvouPlatform: { kind: 'web' }, TauriBridge: current };
  assert.equal(load(window), current);
});
