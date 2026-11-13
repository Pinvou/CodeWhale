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


// r2 P2-5① → r6 P2-B → r8 P2-3 → r12 P2-3：先章有效分支的收尾语义变迁。
// r12 起：等待**未**超阈值的二次 markDispatched（终态 notify 的正常 ~16ms
// 间隙）不 finalize、不取消刚武装的 rAF——帧腿保留，量终态提交 paint 的
// 真实呈现；等待超阈值才补章并收尾（下方补章臂测试覆盖）。
test('先章有效：等待未超阈值保留帧腿，帧回调自量 t_frame（r12 P2-3）', () => {
  const h = harness();
  h.probe.begin('dbl');
  h.probe.markDispatched('dbl');
  assert.equal(h.rafCount(), 1);
  h.probe.markDispatched('dbl');          // 终态 notify 紧随（waited ≈ 0ms ≤ 120）
  assert.equal(h.rafCount(), 1, '帧腿保留：不取消刚武装的 rAF');
  assert.equal(h.cancelled.size, 0, '无取消');
  const entry = h.probe._liveEntry('dbl');
  assert.ok(entry && !entry.finalized, '条目未收尾，等帧回调落章');
  assert.equal(entry.t_frame, null, '未到补章阈值，不补 t_frame');
  flushFrames(h);                          // 终态提交 paint 的帧
  assert.equal(h.rafCount(), 0, '帧回调后清空');
  assert.ok(h.probe._liveEntry('dbl') === null || h.probe._liveEntry('dbl').finalized,
    '帧回调落章后 finalize');
  assert.equal(h.recorded.length, 0, '正常速度不上报');
  const snap = h.probe.snapshot();
  assert.ok(snap[snap.length - 1].dispatch_to_frame_ms != null,
    '帧腿保留后 dispatch→frame 有量值（原实现此段恒 null）');
});

// r12 P2-3 行为锁：等待**超**阈值的二次 markDispatched 仍走补章+收尾臂
// （r7 P2-3 冻结中排队语义不变），补章收尾取消已武装的帧回调。
test('先章有效：等待超阈值仍补章收尾并取消 rAF（r7 P2-3 语义保持）', () => {
  const h = harness();
  h.probe.begin('frz');
  h.probe.markDispatched('frz');
  const fe = h.probe._liveEntry('frz');
  fe.t_recv -= 400;                        // 两章一起回拨：recv→dispatch 保持 ≈0（聚焦帧段）
  fe.t_dispatch -= 400;                    // 帧等待 400ms（>120）：解冻后的终态 notify
  h.probe.markDispatched('frz');
  assert.equal(h.rafCount(), 0, '补章臂收尾并取消 rAF');
  assert.ok(h.probe._liveEntry('frz') === null || h.probe._liveEntry('frz').finalized,
    '超阈值补章后收尾');
  assert.equal(h.recorded.length, 1, '真实冻结上报（dispatch→frame）');
});

// r12 P2-4：打标未派发条目在其 sid 永不再 begin 时，由**其它会话**的后续
// 活动按 5s 新鲜度上界扫掉——currentBySession 不再无界滞留。先标后章窗口
// （打标后 ~16-32ms 的合法补章）不受影响（既有 P2-2 测试覆盖）。
test('滞留打标条目经跨会话活动有界收口（r12 P2-4）', () => {
  const h = harness();
  h.probe.begin('ghost');
  const ghost = h.probe._liveEntry('ghost');
  ghost.t_recv -= 60000;                   // 一分钟前（远超 5s 上界）
  h.probe.markThrottled('ghost');          // 后台终态收口：此后该 sid 不再 begin
  h.probe.begin('other');                  // 任何后续活动
  assert.ok(h.probe._liveEntry('ghost') === null || h.probe._liveEntry('ghost').finalized,
    '滞留打标条目被扫尾（原实现永远滞留到窗口隐藏）');
  // 5s 以内的打标条目不受扫尾影响（先标后章窗口）。
  h.probe.begin('fresh');
  h.probe.markThrottled('fresh');
  h.probe.begin('other2');
  const fresh = h.probe._liveEntry('fresh');
  assert.ok(fresh && !fresh.finalized, '新鲜打标条目不被误扫');
});

// r12 P3（评审随批点名）：120ms 阈值的排他边界无行为锁——变异 `>` → `>=`
// 后全套件曾 21/21 假绿。两段各钉「恰 120 不上报、刚超即上报」。段差由
// **手工对齐时间戳**构造（不靠回拨+真实时钟的 ε），边界值逐毫秒精确。
test('120ms 阈值边界锁：恰 120 不上报，>120 上报（两段）', () => {
  // recv→dispatch 段：盖章后把 t_recv 对齐到 t_dispatch−120（恰）/−121（超）。
  const a = harness();
  a.probe.begin('b1');
  a.probe.markDispatched('b1');
  const e1 = a.probe._liveEntry('b1');
  e1.t_recv = e1.t_dispatch - 120;
  flushFrames(a);
  assert.equal(a.recorded.length, 0, 'recv→dispatch 恰 120ms：排他语义不上报');
  const b = harness();
  b.probe.begin('b2');
  b.probe.markDispatched('b2');
  const e2 = b.probe._liveEntry('b2');
  e2.t_recv = e2.t_dispatch - 121;
  flushFrames(b);
  assert.equal(b.recorded.length, 1, 'recv→dispatch 121ms 必须上报');
  // dispatch→frame 段：armed 条目手工对齐 t_frame−t_dispatch 后经 begin-supersede
  // 收尾（supersede 的补章门按真实 waited≈0 判定不补章，段差用手工值）。
  const c = harness();
  c.probe.begin('b3');
  c.probe.markDispatched('b3');
  const e3 = c.probe._liveEntry('b3');
  e3.t_dispatch -= 100;                    // waited ≈ 100+ε < 120：supersede 不补章
  e3.t_frame = e3.t_dispatch + 120;        // dispatch→frame 恰 120
  c.probe.begin('b3');
  assert.equal(c.recorded.length, 0, 'dispatch→frame 恰 120ms：排他语义不上报');
  const d = harness();
  d.probe.begin('b4');
  d.probe.markDispatched('b4');
  const e4 = d.probe._liveEntry('b4');
  e4.t_dispatch -= 100;
  e4.t_frame = e4.t_dispatch + 121;        // 刚超
  d.probe.begin('b4');
  assert.equal(d.recorded.length, 1, 'dispatch→frame 121ms 必须上报');
});

// r12 P3（评审随批点名）：record 抛错不得烧掉 10s 节流窗口——诊断恢复后
// 同会话的下一条真实 stall 仍可上报。原实现只测「模块缺失」，未测「抛错」。
test('record 抛错不烧节流配额（r12 P3）', () => {
  const recorded = [];
  const frames = [];
  let throwOnce = true;
  const window = {
    requestAnimationFrame: fn => { frames.push(fn); return frames.length; },
    cancelAnimationFrame: () => {},
    PinvouAuthoritySyncDiagnostics: {
      record: (event, details) => {
        if (throwOnce) { throwOnce = false; throw new Error('sink unavailable'); }
        recorded.push({ event, details });
      },
    },
  };
  const listeners = {};
  const document = {
    visibilityState: 'visible',
    addEventListener(name, fn) { (listeners[name] = listeners[name] || []).push(fn); },
    dispatchEvent(event) { (listeners[event.type] || []).forEach((fn) => fn(event)); },
  };
  const sandbox = { window, document, Event: class { constructor(type) { this.type = type; } } };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'render-stage-probe.js' });
  const probe = window.RenderStageProbe;
  const flush = () => { const fns = frames.splice(0); for (const fn of fns) if (fn) fn(); };
  probe.begin('t1');
  probe._liveEntry('t1').t_recv -= 500;
  probe.markScheduled('t1');
  probe.markDispatched('t1');               // 第一次 stall：record 抛错
  probe._liveEntry('t1').t_frame = probe._liveEntry('t1').t_dispatch;
  flush();
  assert.equal(recorded.length, 0, '抛错的那次未落账');
  probe.begin('t1');                        // 同会话紧接着第二条真实 stall（<10s）
  probe._liveEntry('t1').t_recv -= 500;
  probe.markScheduled('t1');
  probe.markDispatched('t1');
  probe._liveEntry('t1').t_frame = probe._liveEntry('t1').t_dispatch;
  flush();
  assert.equal(recorded.length, 1, '抛错未烧节流配额：恢复后的 stall 照常上报');
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

// r9 P2：markThrottled 只对**未盖章**条目生效——已盖章（publish 已跑、React
// 状态已更新）的条目处于「等下一帧」段，它的 dispatch→frame 量的是真实渲染/
// 冻结。迟到的节流标记（流中切走/冻结跨帧/多会话流三几何，受控时钟亲证）
// 若能打到已盖章条目，真实冻结被 finalize 的 veto 吞掉、零记录。删掉
// `entry.t_dispatch == null` 守卫此测试必红。
test('r9 P2：已盖章条目的真实冻结不得被迟到的 markThrottled 吞掉', () => {
  const h = harness();
  h.probe.begin('fg');
  const entry = h.probe._liveEntry('fg');
  h.probe.markScheduled('fg');
  h.probe.markDispatched('fg');           // publish 已跑，条目在等下一帧
  entry.t_recv -= 490;                    // recv→dispatch 段保持 <120（聚焦帧段）
  entry.t_dispatch -= 964;                // 帧等待 964ms 真实冻结（评审对照实验值）
  h.probe.markThrottled('fg');            // 迟到的节流标记不得否决已盖章条目
  flushFrames(h);
  assert.equal(h.recorded.length, 1,
    '已盖章条目的 dispatch→frame 真实冻结必须上报（无守卫时被 throttledOrigin veto 吞掉）');
  assert.ok(h.recorded[0].details.dispatch_to_frame_ms >= 490,
    JSON.stringify(h.recorded[0].details));
});

// r9 P2 对照臂：未盖章条目打节流标记的既有语义（r7 P2-2）不受守卫影响。
test('r9 P2 对照：未盖章条目的节流标记照常抑制上报', () => {
  const h = harness();
  h.probe.begin('bg');
  h.probe._liveEntry('bg').t_recv -= 1032;
  h.probe.markThrottled('bg');            // 此刻 t_dispatch 仍为 null → 照常生效
  h.probe.markScheduled('bg');
  h.probe.markDispatched('bg');
  flushFrames(h);
  assert.equal(h.recorded.length, 0, '未盖章条目的节流抑制语义不变');
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
