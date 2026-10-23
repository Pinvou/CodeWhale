import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = fs.readFileSync(path.join(root, 'src/shared/render-stage-probe.js'), 'utf8');

function harness({ stallRecorded = true, visibility = 'visible' } = {}) {
  const recorded = [];
  const rafQueue = [];
  const cancelled = new Set();
  const window = {
    requestAnimationFrame: fn => { rafQueue.push(fn); return rafQueue.length; },
    cancelAnimationFrame: handle => {
      cancelled.add(handle);
      // 真取消语义（P2-1/P2-4 断言需要）：句柄已取消的回调不再可触发。
      const index = handle - 1;
      if (index >= 0 && index < rafQueue.length) rafQueue.splice(index, 1);
    },
    // 探针模块以 window 为 root（浏览器侧），诊断桩必须挂在同一对象上
    PinvouAuthoritySyncDiagnostics: stallRecorded ? { record: (event, details) => recorded.push({ event, details }) } : undefined,
  };
  const listeners = {};
  const document = {
    visibilityState: visibility,
    addEventListener(name, fn) { (listeners[name] = listeners[name] || []).push(fn); },
    dispatchEvent(event) { (listeners[event.type] || []).forEach((fn) => fn(event)); },
  };
  const sandbox = { window, document, Event: class { constructor(type) { this.type = type; } } };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'render-stage-probe.js' });
  return { probe: window.RenderStageProbe || sandbox.globalThis.RenderStageProbe, recorded, rafQueue, cancelled, window, document: sandbox.document };
}

function flushFrames(h) {
  const queue = h.rafQueue.splice(0);
  for (const fn of queue) if (fn) fn();
}

test('正常速度的周期不上报', () => {
  const h = harness();
  h.probe.begin('s1');
  h.probe.markScheduled('s1');
  h.probe.markDispatched('s1');
  flushFrames(h);
  assert.equal(h.recorded.length, 0, '无卡顿不应产生诊断记录');
  assert.equal(h.probe.snapshot().length, 1);
});

test('接收→派发超阈值上报，且不含任何内容字段', () => {
  const h = harness();
  h.probe.begin('s2');
  const entry = h.probe._liveEntry('s2');
  // 手动把接收时间戳拨回 200ms 前，模拟事件到达后 JS 长时间未被调度
  entry.t_recv -= 200;
  h.probe.markScheduled('s2');
  h.probe.markDispatched('s2');
  flushFrames(h);
  assert.equal(h.recorded.length, 1);
  const { event, details } = h.recorded[0];
  assert.equal(event, 'render_stage_stall');
  assert.equal(details.session_id, 's2');
  assert.ok(details.recv_to_dispatch_ms >= 200);
  assert.ok(Number.isInteger(details.stage_seq));
  // 契约：只允许 id/序号/时间差——出现任何内容性字段即违约
  const allowed = new Set(['session_id', 'stage_seq', 'recv_to_dispatch_ms', 'dispatch_to_frame_ms']);
  for (const key of Object.keys(details)) assert.ok(allowed.has(key), `非法字段 ${key}`);
});

test('派发→帧超阈值上报（帧迟迟不来 = 呈现停滞的最强代理信号）', () => {
  const h = harness();
  h.probe.begin('s3');
  h.probe.markDispatched('s3');
  // 不 flush rAF 队列：下一帧没有发生
  const entry = h.probe._liveEntry('s3');
  entry.t_dispatch -= 300;
  flushFrames(h);
  assert.equal(h.recorded.length, 1);
  assert.ok(h.recorded[0].details.dispatch_to_frame_ms >= 300);
});

test('同 session 新周期会先收尾上一个未完结周期（缺段不误报也不静默吞）', () => {
  const h = harness();
  h.probe.begin('s4');           // 只 begin，没有 scheduled/dispatched（回调从未运行）
  h.probe.begin('s4');           // 下一周期到达
  const snap = h.probe.snapshot();
  assert.equal(snap.length, 2);
  assert.equal(snap[0].recv_to_dispatch_ms, null, '缺段记录为 null，不编造数值');
  assert.equal(h.recorded.length, 0, '缺段不算卡顿（可能只是直发路径已被取代）');
});

test('环形缓冲有界', () => {
  const h = harness();
  for (let i = 0; i < 200; i++) h.probe.begin('s5');
  assert.ok(h.probe.snapshot().length <= 64, '环形缓冲不得超过 64 条');
});

test('诊断模块不可用时探针不抛错', () => {
  const h = harness({ stallRecorded: false });
  h.probe.begin('s6');
  const entry = h.probe._liveEntry('s6');
  entry.t_recv -= 500;
  h.probe.markDispatched('s6');
  flushFrames(h);
  assert.equal(h.recorded.length, 0);
});

// ---- 2026-10-21 评审 P2 三条：隐藏窗口误报 / 上报节流 / rAF 句柄取消 ----

function makeStall(h, sid) {
  h.probe.begin(sid);
  const entry = h.probe._liveEntry(sid);
  entry.t_recv -= 500;
  h.probe.markDispatched(sid);
  flushFrames(h);
}

test('隐藏窗口不上报（定时器节流造成的假 stall 不挤占诊断队列）', () => {
  const h = harness({ visibility: 'hidden' });
  makeStall(h, 'hidden1');
  makeStall(h, 'hidden2');
  makeStall(h, 'hidden3');
  assert.equal(h.recorded.length, 0, '隐藏窗口的时差不可信，全部不上报');
  // 恢复可见后真实卡顿仍能上报
  h.document.visibilityState = 'visible';
  makeStall(h, 'hidden4');
  assert.equal(h.recorded.length, 1);
});

test('持续卡顿按会话节流：同一会话 10 秒内不重复，不同会话互不压制（P2-3）', () => {
  const h = harness();
  makeStall(h, 't1');
  makeStall(h, 't1');
  makeStall(h, 't1');
  assert.equal(h.recorded.length, 1, '同一会话的重复 stall 只记第一条');
  makeStall(h, 't2');
  assert.equal(h.recorded.length, 2, '另一会话的真实 stall 不被 t1 的节流压制');
});

// P2-1：隐藏→恢复可见不再产生横跨隐藏期的假 stall——窗口隐藏瞬间收尾武装条目
test('窗口隐藏时收尾武装条目，恢复可见的补触发 rAF 不再量到隐藏期（P2-1）', () => {
  const h = harness();
  h.probe.begin('vis1');
  h.probe.markDispatched('vis1');          // rAF 已武装，尚未触发
  assert.equal(h.rafQueue.length, 1);
  // 窗口隐藏 → 收尾：此刻的时间差都落在可见期，未超阈值不上报
  h.document.visibilityState = 'hidden';
  h.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(h.rafQueue.length, 0, '隐藏时收尾应取消武装的 rAF');
  // 恢复可见后旧 rAF 即使补触发也无效（已 finalize）
  h.document.visibilityState = 'visible';
  h.flushFrames && h.flushFrames();
  assert.equal(h.recorded.length, 0, '正常速度周期的隐藏切换不产生假 stall');
});

// P2-2：流式中被下一条 begin 中止的超阈值帧等待要补报
test('supersede 中止的帧等待超阈值时补报（流式中冻结不再失测，P2-2）', () => {
  const h = harness();
  h.probe.begin('sup1');
  const entry = h.probe._liveEntry('sup1');
  entry.t_recv -= 200;                      // recv→dispatch 也超
  h.probe.markDispatched('sup1');
  entry.t_dispatch -= 200;                  // 帧等待已 200ms（>120ms 阈值）
  h.probe.begin('sup1');                    // 下一 delta supersede
  assert.equal(h.recorded.length, 1, '被中止的帧等待必须上报');
  assert.ok(h.recorded[0].details.dispatch_to_frame_ms >= 200, JSON.stringify(h.recorded[0].details));
});

test('周期被下一事件收尾时，已武装的帧回调被取消（隐藏窗口 rAF 不堆积）', () => {
  const h = harness();
  h.probe.begin('leak');
  h.probe.markDispatched('leak'); // 武装了 rAF，还没触发
  const armed = h.rafQueue.length;
  assert.equal(armed, 1);
  h.probe.begin('leak'); // 下一周期到达 → finalize 上一条 → 取消其 rAF
  assert.ok(h.cancelled.size >= 1, '收尾时调用了 cancelAnimationFrame');
  const entry = h.probe._liveEntry('leak');
  assert.equal(entry.rafHandle, null, '新条目没有遗留句柄');
});


// r2 P2-5①：markDispatched 覆盖旧句柄前必须 cancel——同一 entry 双派发场景。
// harness cancel 已改为按句柄删除（真取消语义），此测试在旧实现（不 cancel
// 直接覆盖）下必红。
test('同一周期二次 markDispatched 时旧 rAF 句柄被取消（P2-5 行为锁）', () => {
  const h = harness();
  h.probe.begin('dbl');
  h.probe.markDispatched('dbl');
  assert.equal(h.rafQueue.length, 1);
  const stale = h.rafQueue[0];
  h.probe.markDispatched('dbl');
  assert.equal(h.rafQueue.length, 1, '二次派发不应堆积两个 rAF');
  assert.notEqual(h.rafQueue[0], stale, '旧回调应已被替换而非残留');
});

// r2 P1-1 行为锁：窗口隐藏期间的 markDispatched 不武装 rAF（同步收尾），
// 恢复可见后无补触发。
test('隐藏期间的 markDispatched 不武装 rAF，恢复可见不补触发（P1-1）', () => {
  const h = harness({ visibility: 'hidden' });
  h.probe.begin('hid1');
  const entry = h.probe._liveEntry('hid1');
  entry.t_recv -= 200;
  h.probe.markDispatched('hid1');
  assert.equal(h.rafQueue.length, 0, '隐藏期不武装 rAF');
  h.document.visibilityState = 'visible';
  h.probe.begin('hid1');
  assert.equal(h.recorded.length, 0, '隐藏期收尾被豁免，不产假 stall');
});
