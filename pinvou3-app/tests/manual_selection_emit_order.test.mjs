import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import url from 'node:url';

const root = path.resolve(path.dirname(url.fileURLToPath(import.meta.url)), '..');
const read = (...p) => fs.readFileSync(path.join(root, ...p), 'utf8');

/** 提取 `async function name(...) { ... }` 的函数体（花括号配对）。 */
function bodyOf(source, name) {
  const match = source.match(new RegExp(`async function ${name}\\(`));
  assert.ok(match, `${name} 未找到`);
  const start = match.index;
  // 花括号配对会被模板串/正则字面量里的花括号干扰；取到下一个顶层
  // `async function` 或文件尾作为函数区段，对这两个小函数足够精确。
  const rest = source.slice(start + match[0].length);
  const next = rest.search(/\n\s{2}async function |\n\}/);
  const end = next >= 0 ? next : rest.length;
  return rest.slice(0, end);
}

/**
 * r10 P2-A 行为钉：手动选择意图（pinvou:manual-model-selection）必须在切换
 * 成功后广播。四个生产发射点均为直线 async（无 try/catch 吞错），因此
 * 「dispatchEvent 语句位于其依赖的最后一个 await invoke 之后」在语义上
 * 等价于「invoke 抛出时 dispatch 不可达」——失败不登记抑制意图。
 */
const featureFiles = [
  ['tauri', 'src/platform/tauri/bridge/settings.js'],
  ['web', 'src/platform/web/bridge.js'],
];

for (const [name, rel] of featureFiles) {
  const source = read(...rel.split('/'));
  for (const fn of ['switchModelRef', 'setActiveModel']) {
    test(`${name}.${fn}: dispatch 在最后 invoke 之后且不在 try 内`, () => {
      const body = bodyOf(source, fn);
      const dispatches = [...body.matchAll(/pinvou:manual-model-selection/g)].map((m) => m.index);
      assert.ok(dispatches.length >= 1, '存在发射点');
      let lastInvokeAt = -1;
      for (const m of body.matchAll(/await invoke\(/g)) lastInvokeAt = m.index;
      assert.ok(lastInvokeAt >= 0, '存在 invoke 依赖');
      for (const at of dispatches) {
        assert.ok(at > lastInvokeAt, `发射@${at} 在最后一个 invoke@${lastInvokeAt} 之后（失败不可达）`);
      }
      assert.ok(!/\btry\s*\{/.test(body), '函数体无 try 包裹（顺序断言语义成立的前提）');
    });
  }
}

/** Rust 侧同一性质的结构钉：set_active_model 的 emit 在事务成功点之后
 * （settings.rs；set_session_model 同性质由其源顺序共同保证）。 */
test('rust: set_active_model 的 manual_model_selection emit 在事务之后', () => {
  const source = read('src-tauri/src/app/commands/settings.rs');
  const fn = source.split('pub async fn set_active_model')[1].split('\n}')[0];
  const emitAt = fn.indexOf('"session:manual_model_selection"');
  const joinAt = fn.indexOf('.await?;');
  assert.ok(emitAt > joinAt, 'emit 在 join_blocking .await? 之后（失败路径不可达）');
});
