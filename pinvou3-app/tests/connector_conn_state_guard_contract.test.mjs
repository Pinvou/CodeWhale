/**
 * EIP/知道连接态守卫的源码契约(issue #1097 窄窗口①②,PR #1128)。
 *
 * ToolStoreView 对 EIP/知道连接态有三层防迟到机制,逻辑内嵌在组件里、
 * 无法单元测试,这里按 connector_busy_release_contract 的先例用源契约钉住:
 *  1. ref 镜像 + 代际守卫:连接态写入必须唯一收口于 mark*,mark* 记代际,
 *     挂载探测只在代际未变时落盘(事件先到、陈旧探测后到不打回新值);
 *  2. 挂载探测前有 isTauriAvailable 守卫(web 态不发探测);
 *  3. sso/sso_wait 监听先查 ref 镜像,已连接后迟到的 Reuse 重放不再弹登录框。
 * 任何一条被重构掉,这里的断言应当失效。
 */
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const source = await readFile(
  new URL('../src/features/tools/ToolStoreView.jsx', import.meta.url),
  'utf8',
);

// ── 1. 写入唯一收口:连接态 setState 只允许出现在 useState 解构与 mark* 内 ──
const setWrite = (setter) => (source.match(new RegExp(`\\b${setter}\\b`, 'g')) || []).length;
assert.ok(
  /\[eipConnected, setEipConnected\] = useState\(/.test(source) && /\[zhidaoConnected, setZhidaoConnected\] = useState\(/.test(source),
  '连接态 useState 解构必须存在',
);
assert.equal(setWrite('setEipConnected'), 2, 'setEipConnected 只能出现两次:解构 + markEipConnected 内部,不得有旁路写入');
assert.equal(setWrite('setZhidaoConnected'), 2, 'setZhidaoConnected 同上');

// ── 2. mark* 同步翻 ref 镜像并递增代际 ──────────────────────────────────────
for (const [mark, epoch, mirror, setter] of [
  ['markEipConnected', 'eipConnEpochRef', 'eipConnectedRef', 'setEipConnected'],
  ['markZhidaoConnected', 'zhidaoConnEpochRef', 'zhidaoConnectedRef', 'setZhidaoConnected'],
]) {
  const at = source.indexOf(`const ${mark} = `);
  assert.ok(at > 0, `必须存在 ${mark} 定义`);
  const body = source.slice(at, source.indexOf('};', at));
  assert.ok(body.includes(`${epoch}.current += 1`), `${mark} 必须递增代际`);
  assert.ok(body.includes(`${mirror}.current = v`), `${mark} 必须同步写 ref 镜像`);
  assert.ok(body.includes(`${setter}(v)`), `${mark} 必须经 setState 落渲染态`);
}

// ── 3. 挂载探测:isTauriAvailable 守卫 + 代际快照,落盘前比对代际 ────────────
const probeAt = source.indexOf("invokeTauri('eip_status')");
assert.ok(probeAt > 0, '必须存在 eip_status 挂载探测');
const probeBlock = source.slice(probeAt - 400, source.indexOf('}, []);', probeAt));
const guardAt = probeBlock.lastIndexOf('if (!isTauriAvailable()) return;');
assert.ok(guardAt >= 0 && guardAt < probeBlock.indexOf('const eipProbeEpoch'), '探测前必须有 isTauriAvailable 守卫(web 态不发探测)');
assert.ok(probeBlock.includes('const eipProbeEpoch = eipConnEpochRef.current'), '探测发出前必须快照 eip 代际');
assert.ok(probeBlock.includes('const zhidaoProbeEpoch = zhidaoConnEpochRef.current'), '探测发出前必须快照 zhidao 代际');
assert.ok(
  /if \(eipConnEpochRef\.current === eipProbeEpoch\) markEipConnected\(/.test(probeBlock),
  'eip 探测落盘前必须比对代际,陈旧响应不得打回事件刚写入的连接态',
);
assert.ok(
  /if \(zhidaoConnEpochRef\.current === zhidaoProbeEpoch\) markZhidaoConnected\(/.test(probeBlock),
  'zhidao 探测落盘前必须比对代际',
);

// ── 4. 迟到重放抑制:sso/sso_wait 监听先查 ref 镜像再弹窗 ────────────────────
for (const [event, mirror, setDialog] of [
  ['eip:sso_wait', 'eipConnectedRef', 'setEipSso'],
  ['eip:sso', 'eipConnectedRef', 'setEipSso'],
  ['zhidao:sso_wait', 'zhidaoConnectedRef', 'setZhidaoSso'],
  ['zhidao:sso', 'zhidaoConnectedRef', 'setZhidaoSso'],
]) {
  const listenAt = source.indexOf(`ev.listen('${event}'`);
  assert.ok(listenAt > 0, `必须存在 ${event} 监听`);
  const body = source.slice(listenAt, source.indexOf('}));', listenAt));
  const guard = body.indexOf(`if (${mirror}.current) return;`);
  const dialog = body.indexOf(`${setDialog}(`);
  assert.ok(guard >= 0, `${event} 监听必须带 ref 镜像守卫`);
  assert.ok(dialog > guard, `${event} 的守卫必须在弹窗写入之前`);
}

console.log('connector conn-state guard contract passed');
