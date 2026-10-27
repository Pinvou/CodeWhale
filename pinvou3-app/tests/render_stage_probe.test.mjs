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
  // rAF 桩用「++序号发句柄 + Map 按句柄删除」的真实浏览器语义（r2 P2-5④）：
  // 句柄不复用、取消按身份删——按数组下标/位置删的桩会让「取消了错误回调」
  // 的实现假绿。
  const rafByHandle = new Map();
  let rafSeq = 0;
  const cancelled = new Set();
  const window = {
    requestAnimationFrame: fn => { const handle = ++rafSeq; rafByHandle.set(handle, fn); return handle; },
    cancelAnimationFrame: handle => { cancelled.add(handle); rafByHandle.delete(handle); },
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
  return {
    probe: window.RenderStageProbe || sandbox.globalThis.RenderStageProbe,
    recorded, cancelled, window, document: sandbox.document,
    rafCount: () => rafByHandle.size,
    rafCallbacks: () => [...rafByHandle.values()],
    takeRafs() { const fns = [...rafByHandle.values()]; rafByHandle.clear(); return fns; },
  };
}

function flushFrames(h) {
  for (const fn of h.takeRafs()) if (fn) fn();
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

// P2-1：隐藏→恢复可见不再产生横跨隐藏期的假 stall——窗口隐藏瞬间收尾武装条目。
// r2 P2-5：旧版此处的 `h.flushFrames && h.flushFrames()` 是 no-op（flushFrames
// 是模块函数而非 h 属性），「恢复可见补触发」分支从未真正执行。这里直接持有
// 武装回调并在恢复可见后手动触发——等价于无 cancelAnimationFrame 环境里仍会
// 进来的迟到回调，行为锁同时覆盖 markFrame 的 finalized 守卫。
test('窗口隐藏时收尾武装条目，恢复可见的补触发 rAF 不再量到隐藏期（P2-1）', () => {
  const h = harness();
  h.probe.begin('vis1');
  h.probe.markDispatched('vis1');          // rAF 已武装，尚未触发
  assert.equal(h.rafCount(), 1);
  const stale = h.rafCallbacks()[0];       // 收尾取消前先拿到回调引用
  // 窗口隐藏 → 收尾：此刻的时间差都落在可见期，未超阈值不上报
  h.document.visibilityState = 'hidden';
  h.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(h.rafCount(), 0, '隐藏时收尾应取消武装的 rAF');
  // 恢复可见后旧 rAF 即使补触发也无效（已 finalize，不得改写段值/上报）
  h.document.visibilityState = 'visible';
  stale();
  assert.equal(h.recorded.length, 0, '正常速度周期的隐藏切换不产生假 stall');
  assert.equal(h.probe.snapshot()[0].dispatch_to_frame_ms, null,
    '迟到回调不得污染已收尾条目的段值');
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
  assert.equal(h.rafCount(), 1);
  h.probe.begin('leak'); // 下一周期到达 → finalize 上一条 → 取消其 rAF
  assert.ok(h.cancelled.size >= 1, '收尾时调用了 cancelAnimationFrame');
  assert.equal(h.rafCount(), 0, '取消按句柄删除，无残留回调');
  const entry = h.probe._liveEntry('leak');
  assert.equal(entry.rafHandle, null, '新条目没有遗留句柄');
});


// r2 P2-5①：markDispatched 覆盖旧句柄前必须 cancel——同一 entry 双派发场景。
// 桩按句柄删除（真取消语义），此测试在旧实现（不 cancel 直接覆盖）下必红。
test('同一周期二次 markDispatched 时旧 rAF 句柄被取消（P2-5 行为锁）', () => {
  const h = harness();
  h.probe.begin('dbl');
  h.probe.markDispatched('dbl');
  assert.equal(h.rafCount(), 1);
  // r6 P2-B：先章有效——已有 t_dispatch 的二次 markDispatched 直接收尾，
  // 不重臂 rAF、不堆积句柄
  h.probe.markDispatched('dbl');
  assert.ok(h.rafCount() <= 1, '二次派发不堆积 rAF（finalize 后可能为 0）');
  assert.ok(h.probe._liveEntry('dbl') === null || h.probe._liveEntry('dbl').finalized,
    '先章有效：二次 markDispatched 应 finalize 而非覆盖');
});

// r2 P1-1 行为锁：窗口隐藏期间的 markDispatched 不武装 rAF（同步收尾），
// 恢复可见后无补触发。
test('隐藏期间的 markDispatched 不武装 rAF，恢复可见不补触发（P1-1）', () => {
  const h = harness({ visibility: 'hidden' });
  h.probe.begin('hid1');
  const entry = h.probe._liveEntry('hid1');
  entry.t_recv -= 200;
  h.probe.markDispatched('hid1');
  assert.equal(h.rafCount(), 0, '隐藏期不武装 rAF');
  h.document.visibilityState = 'visible';
  h.probe.begin('hid1');
  assert.equal(h.recorded.length, 0, '隐藏期收尾被豁免，不产假 stall');
});

// r4 P2-A 行为锁：后台会话的陈旧条目（未派发存活数分钟）被任意 notify() 补盖
// 派发标记时，不得盖章成分钟级假 stall——静默收尾。
test('陈旧条目的 markDispatched 静默收尾，不盖章不上报（P2-A）', () => {
  const h = harness();
  h.probe.begin('stale1');
  h.probe.markScheduled('stale1');
  const entry = h.probe._liveEntry('stale1');
  entry.t_sched -= 60000;                 // 一分钟前武装的合并帧，publish 只标记活跃 sid
  h.probe.markDispatched('stale1');       // 数分钟后某事件路径的 notify() 补盖
  assert.equal(h.recorded.length, 0, '分钟级 recv→dispatch 是补盖伪差，不得上报');
  assert.equal(h.probe.snapshot()[0].recv_to_dispatch_ms, null, '未盖章，段差保持 null');
  assert.ok(h.probe._liveEntry('stale1') === null || h.probe._liveEntry('stale1').finalized !== false,
    '陈旧条目应已收尾');
});

// r4 P2-B 行为锁：begin 发生在隐藏期、恢复可见后定时器腿才落地的条目，
// recv→dispatch 含浏览器 ~1s 节流伪差——豁免上报；可见期起源的同类时差仍正常上报。
test('隐藏期起源的条目恢复可见后不产节流假 stall（P2-B）', () => {
  const h = harness({ visibility: 'hidden' });
  h.probe.begin('hid2');                    // 隐藏期到达
  const entry = h.probe._liveEntry('hid2');
  entry.t_recv -= 1000;                     // 标准节流 ~1s 桶
  h.document.visibilityState = 'visible';   // 恢复可见，32ms 定时器腿此刻才 fire
  h.probe.markDispatched('hid2');           // 此刻可见：武装 rAF
  flushFrames(h);
  assert.equal(h.recorded.length, 0, '节流伪差不得进诊断流');
  // 对照组：可见期起源的同样时差是真实卡顿，必须照常上报
  h.probe.begin('vis2');
  h.probe._liveEntry('vis2').t_recv -= 1000;
  h.probe.markDispatched('vis2');
  flushFrames(h);
  assert.equal(h.recorded.length, 1, '可见起源不受豁免影响');
});

// r5 P2-1 行为锁：→hidden 转变收尾的条目**必须能上报**（测量全在可见期，
// 隐藏豁免不应吞掉真 stall——用户对冻结的自然反应就是切走窗口）。
test('切走瞬间的真 stall 经转变收尾上报（P2-1）', () => {
  const h = harness();
  h.probe.begin('visStall');
  const entry = h.probe._liveEntry('visStall');
  entry.t_recv -= 200;                    // recv→dispatch 200ms（>120ms 阈值），全在可见期
  h.probe.markDispatched('visStall');     // 武装 rAF
  // 切走：转变收尾应补章 t_frame 并绕过隐藏豁免上报
  h.document.visibilityState = 'hidden';
  h.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(h.recorded.length, 1, '切走时的真 stall 必须被记录');
  assert.ok(h.recorded[0].details.recv_to_dispatch_ms >= 200,
    JSON.stringify(h.recorded[0].details));
});

// r6 P2-E（r7 P2-4 换真锁）：已有 t_dispatch 的条目，主线程冻结期间排队的
// 终态 notify 解冻后二次 markDispatched——先章有效（t_dispatch 不被覆盖）+
// 补章 t_frame（dispatch→frame 段诚实覆盖冻结时长）。上一版依赖两次调用间的
// 真实墙钟差，同毫秒执行时 t_dispatch === firstDispatch 巧合为真（变异全绿的
// 空转锁）；本版把 t_recv/t_dispatch 同步回拨模拟确定性冻结，不依赖时钟。
test('冻结中排队的终态 notify：首章有效且 dispatch→frame 补章（P2-B+P2-3）', () => {
  const h = harness();
  h.probe.begin('frozen');
  const entry = h.probe._liveEntry('frozen');
  entry.t_recv -= 100;                     // recv→dispatch ~100ms（<120，不因该段上报）
  h.probe.markDispatched('frozen');        // 首次派发章，武装 rAF
  const firstDispatch = entry.t_dispatch;
  // 主线程冻结 500ms：终态 notify 在解冻后先于迟到的 rAF 执行——等效于
  // 「此刻距首章已 500ms」（两个旧章同步回拨，两段关系不变）。
  entry.t_recv -= 500;
  entry.t_dispatch -= 500;
  h.probe.markDispatched('frozen');        // 二次（终态 notify 路径）
  assert.equal(entry.t_dispatch, firstDispatch - 500,
    't_dispatch 首章值不被重章覆盖');
  assert.equal(h.recorded.length, 1, '真实冻结必须产出记录（不补章则是零记录——r7 P2-3）');
  const details = h.recorded[0].details;
  assert.ok(details.recv_to_dispatch_ms < 200,
    `recv→dispatch 保持在首章量级（~100ms），不被重章拉长：${JSON.stringify(details)}`);
  assert.ok(details.dispatch_to_frame_ms >= 500,
    `dispatch→frame 覆盖冻结时长：${JSON.stringify(details)}`);
});

// r7 P2-4：武装条目（已派发、帧未回）等待超阈值后切走——转变收尾的补章分支
// （waited>120ms 时 t_frame=now）此前无测试覆盖（既有 visStall 用例 waited≈0
// 触不到该分支）。
test('武装条目帧等待超阈值后切走：dispatch→frame 经转变收尾上报', () => {
  const h = harness();
  h.probe.begin('armed');
  const entry = h.probe._liveEntry('armed');
  h.probe.markDispatched('armed');         // 武装 rAF，帧未回
  entry.t_recv -= 490;                     // recv→dispatch 段保持 <120（聚焦帧段）
  entry.t_dispatch -= 490;                 // 帧等待 490ms（>120ms 阈值）
  h.document.visibilityState = 'hidden';
  h.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(h.recorded.length, 1, '武装条目的帧等待必须经转变收尾上报');
  assert.ok(h.recorded[0].details.dispatch_to_frame_ms >= 490,
    JSON.stringify(h.recorded[0].details));
});

// r7 P2-2：后台预览腿（1s 合并节流）的条目打了 throttledOrigin 标记后
// finalize 不上报——节流不是冻结，节律性假信号（每会话每 10s 一条 ~1032ms）
// 会支配诊断流。snapshot 仍保留段差可查。
test('后台预览腿节流条目不上报但保留 snapshot（P2-2）', () => {
  const h = harness();
  h.probe.begin('bg');
  const entry = h.probe._liveEntry('bg');
  entry.t_recv -= 1032;                    // 后台 1s 预览腿量级（受控时钟实测 ~1032ms）
  h.probe.markThrottled('bg');
  h.probe.markScheduled('bg');
  h.probe.markDispatched('bg');
  flushFrames(h);
  assert.equal(h.recorded.length, 0, '应用自己的合并节流不是冻结，不进诊断流');
  const snap = h.probe.snapshot();
  assert.ok(snap.length > 0 && snap[snap.length - 1].recv_to_dispatch_ms >= 1032,
    'snapshot 仍可查段差（只拦上报，不拦观测）');
  // 对照组：同样时差、无节流标记 → 是真实卡顿，必须照常上报
  h.probe.begin('fg');
  h.probe._liveEntry('fg').t_recv -= 1032;
  h.probe.markScheduled('fg');
  h.probe.markDispatched('fg');
  flushFrames(h);
  assert.equal(h.recorded.length, 1, '无节流标记的同类时差是真实卡顿，必须上报');
});
