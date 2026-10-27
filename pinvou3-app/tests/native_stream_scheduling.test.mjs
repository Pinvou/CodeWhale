import assert from 'node:assert/strict';
import { test } from 'node:test';
import vm from 'node:vm';
import { marked } from 'marked';
import { createNativeStreamHarness, nativeStreamSources } from './helpers/native-stream-harness.mjs';

test('background previews skip repeated parsing while foreground keeps its short budget', () => {
  const h = createNativeStreamHarness();
  for (let i = 0; i < 20; i++) {
    h.emit('s0', 'chat:delta', { text: '**front** ' });
    h.emit('s1', 'chat:delta', { text: '**back** ' });
    h.advance((i + 1) * 20);
  }
  assert.ok(h.renders.filter(r => r.sessionId === 's0').length >= 3);
  assert.equal(h.renders.filter(r => r.sessionId === 's1').length, 1);
  assert.equal(h.view('s1').chatItems[0].text, '**back** '.repeat(20));
});

test('activating a background stream publishes current HTML synchronously and cancels stale work', () => {
  const h = createNativeStreamHarness();
  h.emit('s1', 'chat:delta', { text: 'first' });
  h.advance(40);
  h.emit('s1', 'chat:delta', { text: ' **latest**' });
  h.advance(50);
  const notifications = h.notificationCount;
  h.activate('s1');
  assert.equal(h.notificationCount, notifications + 1);
  assert.equal(h.snapshots.at(-1).html, marked.parse('first **latest**'));
  const renders = h.renders.length;
  h.advance(1200);
  assert.equal(h.renders.length, renders, 'activation must cancel the old background timer');
  assert.equal(h.notificationCount, notifications + 1, 'activation must cancel the older pending publication');
  h.emit('s1', 'chat:delta', { text: ' foreground' });
  h.advance(1400);
  assert.equal(h.state.chatItems[0].html, marked.parse('first **latest** foreground'));
});

for (const frames of [true, false]) test(`a still-background preview publishes at its 1000ms edge (frames=${frames})`, () => {
  const h = createNativeStreamHarness({ frames });
  h.emit('s1', 'chat:delta', { text: 'first' });
  h.advance(40);
  h.emit('s1', 'chat:delta', { text: ' latest' });
  h.advance(80);
  const notifications = h.notificationCount;
  h.advance(999);
  assert.equal(h.notificationCount, notifications);
  assert.equal(h.view('s1').chatItems[0].html, marked.parse('first'));
  h.advance(1000);
  assert.equal(h.view('s1').chatItems[0].html, marked.parse('first latest'));
  assert.equal(h.notificationCount, notifications);
  h.advance(frames ? 1016 : 1032);
  assert.equal(h.notificationCount, notifications + 1, 'the trailing preview must publish through the shared scheduler');
  assert.equal(h.state.activeSessionId, 's0');
});

test('activating an unchanged first-delta preview does not parse it again', () => {
  const h = createNativeStreamHarness();
  h.emit('s1', 'chat:delta', { text: '**already current**' });
  h.advance(40);
  const renders = h.renders.length;
  h.activate('s1');
  assert.equal(h.snapshots.at(-1).html, marked.parse('**already current**'));
  assert.equal(h.renders.length, renders);
});

test('switching repeatedly between stalled current previews does not reparse accumulated text', () => {
  const h = createNativeStreamHarness();
  const text = '**stalled** '.repeat(4096);
  for (const sid of ['s0', 's1']) h.emit(sid, 'chat:delta', { text });
  h.advance(40);
  for (const sid of ['s0', 's1']) h.emit(sid, 'chat:delta', { text: ' latest' });
  h.advance(1200);
  const renders = h.renders.length;
  for (const sid of ['s1', 's0', 's1', 's0']) {
    h.activate(sid);
    assert.equal(h.snapshots.at(-1).html, marked.parse(text + ' latest'));
  }
  assert.equal(h.renders.length, renders);
});

test('render tracking is isolated across sessions with identical stream text', () => {
  const h = createNativeStreamHarness();
  for (const sid of ['s0', 's1']) {
    h.emit(sid, 'chat:delta', { text: 'first' });
    h.emit(sid, 'chat:delta', { text: ' latest' });
  }
  h.activate('s1');
  h.activate('s0');
  assert.equal(h.renders.length, 4);
  for (const sid of ['s0', 's1']) assert.equal(h.view(sid).chatItems[0].html, marked.parse('first latest'));
});

test('same-session notifications preserve the foreground render budget', () => {
  const h = createNativeStreamHarness();
  h.emit('s0', 'chat:delta', { text: 'first' });
  h.emit('s0', 'chat:delta', { text: ' second' });
  for (let i = 0; i < 10; i++) h.notify();
  assert.equal(h.renders.length, 1, 'ordinary notifications must not reparse every delta');
});

test('foreground preview completion publishes immediately at the existing 180ms budget', () => {
  const h = createNativeStreamHarness();
  h.emit('s0', 'chat:delta', { text: 'first' });
  h.advance(40);
  h.emit('s0', 'chat:delta', { text: ' latest' });
  h.advance(180);
  assert.equal(h.snapshots.at(-1).html, marked.parse('first latest'),
    'foreground publication must not wait for another animation frame');
});

test('returning from a draft refreshes even when the previous conversation ID is reused', () => {
  const h = createNativeStreamHarness();
  h.emit('s0', 'chat:delta', { text: 'first' });
  h.emit('s0', 'chat:delta', { text: ' latest' });
  h.state.activeSessionId = null;
  h.notify();
  h.state.activeSessionId = 's0';
  h.notify();
  assert.equal(h.snapshots.at(-1).html, marked.parse('first latest'));
});

test('activation isolates a preview failure and retries on the next publication', () => {
  let fail = false;
  const h = createNativeStreamHarness({ render(text) {
    if (fail) { fail = false; throw new Error('synthetic renderer failure'); }
    return marked.parse(text);
  } });
  h.emit('s1', 'chat:delta', { text: 'first' });
  h.emit('s1', 'chat:delta', { text: ' latest' });
  fail = true;
  assert.doesNotThrow(() => h.activate('s1'));
  assert.equal(h.errors.length, 1);
  assert.equal(h.snapshots.at(-1).text, 'first latest');
  h.notify();
  assert.equal(h.snapshots.at(-1).html, marked.parse('first latest'));
});

test('returning to the previous session after a failed activation refreshes pending text', () => {
  let fail = false;
  const h = createNativeStreamHarness({ render(text) {
    if (fail) { fail = false; throw new Error('synthetic renderer failure'); }
    return marked.parse(text);
  } });
  h.emit('s0', 'chat:delta', { text: 'front' });
  h.emit('s1', 'chat:delta', { text: 'back' });
  h.emit('s1', 'chat:delta', { text: ' latest' });
  fail = true;
  assert.doesNotThrow(() => h.activate('s1'));
  h.emit('s0', 'chat:delta', { text: ' pending' });
  h.activate('s0');
  assert.equal(h.errors.length, 1);
  assert.equal(h.snapshots.at(-1).text, 'front pending');
  assert.equal(h.snapshots.at(-1).html, marked.parse('front pending'));
});

test('hosts without timers retain synchronous full-content rendering', () => {
  const h = createNativeStreamHarness({ frames: false, timers: false });
  h.emit('s1', 'chat:delta', { text: 'first' });
  h.emit('s1', 'chat:delta', { text: ' final' });
  assert.equal(h.view('s1').chatItems[0].html, marked.parse('first final'));
});

test('one stream publication covers the current state of several sessions', () => {
  const h = createNativeStreamHarness({ count: 5 });
  for (let i = 0; i < 5; i++) h.emit(`s${i}`, 'chat:delta', { text: 'hello' });
  const immediate = h.notificationCount;
  h.advance(40);
  assert.ok(h.notificationCount - immediate <= 1, 'pending streams should share a publication');
  for (let i = 0; i < 5; i++) assert.equal(h.view(`s${i}`).chatItems[0].text, 'hello');
});

test('a coalesced subscriber can enqueue another session update without losing its next publication', () => {
  let reentered = false;
  const h = createNativeStreamHarness({ count: 3, onNotify() {
    if (reentered) return;
    reentered = true;
    h.emit('s2', 'chat:delta', { text: ' reentrant' });
  } });
  h.emit('s1', 'chat:delta', { text: 'first session' });
  h.emit('s2', 'chat:delta', { text: 'second session' });
  h.advance(16);
  assert.equal(h.notificationCount, 1);
  h.advance(40);
  assert.equal(h.notificationCount, 2, 'new work must survive cancellation of the previous batch');
  assert.equal(h.view('s2').chatItems[0].text, 'second session reentrant');
  assert.equal(h.state.activeSessionId, 's0');
  h.activate('s2');
  assert.equal(h.snapshots.at(-1).html, marked.parse('second session reentrant'));
});

test('a throwing coalesced subscriber cannot strand later publications or corrupt session routing', () => {
  let fail = true;
  const h = createNativeStreamHarness({ count: 3, onNotify() {
    if (fail) { fail = false; throw new Error('synthetic subscriber failure'); }
  } });
  h.emit('s1', 'chat:delta', { text: 'first' });
  h.emit('s2', 'chat:delta', { text: 'other session' });
  assert.doesNotThrow(() => h.advance(16));
  assert.equal(h.errors.length, 1);
  assert.equal(h.state.activeSessionId, 's0');
  h.emit('s1', 'chat:delta', { text: ' recovered' });
  h.advance(40);
  assert.equal(h.notificationCount, 2);
  h.activate('s1');
  assert.equal(h.snapshots.at(-1).html, marked.parse('first recovered'));
  assert.equal(h.view('s2').chatItems[0].text, 'other session');
});

for (const frames of [true, false]) test(`background input and errors remain synchronous (frames=${frames})`, () => {
  const h = createNativeStreamHarness({ frames });
  h.emit('s1', 'chat:delta', { text: 'background' });
  const before = h.notificationCount;
  h.emit('s1', 'chat:user_input_required', { id: 'q', questions: [{ question: 'Continue?' }] });
  assert.ok(h.notificationCount > before);
  assert.equal(h.view('s1').chatItems.at(-1).type, 'user_input');
  const beforeError = h.notificationCount;
  h.emit('s1', 'chat:transient_error', { error: 'synthetic retry' });
  assert.ok(h.notificationCount > beforeError);
  h.advance(1200);
  assert.equal(h.errors.length, 0);
});

test('tool boundaries flush a background preview without waiting for its timer', () => {
  const h = createNativeStreamHarness();
  h.emit('s1', 'chat:delta', { text: 'first' });
  h.emit('s1', 'chat:delta', { text: ' final' });
  h.emit('s1', 'chat:tool_start', { id: 'q', name: 'request_user_input', args: {} });
  const item = h.view('s1').chatItems[0];
  assert.equal(item.html, marked.parse('first final'));
  assert.equal(item.streaming, false);
  h.advance(1200);
  assert.equal(item.html, marked.parse('first final'));
});

for (const status of ['Completed', 'Failed', 'Interrupted']) test(`background ${status} flushes final content and persists its own session`, async () => {
  const h = createNativeStreamHarness();
  h.emit('s1', 'chat:delta', { text: 'first' });
  h.emit('s1', 'chat:delta', { text: ' final' });
  h.emit('s1', 'chat:done', { status, ...(status === 'Failed' ? { error: 'synthetic failure' } : {}) });
  assert.equal(h.view('s1').chatItems.find(item => item.type === 'assistant').html, marked.parse('first final'));
  assert.equal(h.view('s1').busy, false);
  assert.equal(h.state.activeSessionId, 's0');
  const renders = h.renders.length;
  h.advance(1200);
  assert.equal(h.renders.length, renders);
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  assert.ok(h.storageCalls.includes('s1'));
  assert.equal(h.errors.length, 0);
});

test('evicting a background session cancels its timers without dropping the active publication', () => {
  const h = createNativeStreamHarness();
  h.emit('s0', 'chat:delta', { text: 'front' });
  h.emit('s0', 'chat:delta', { text: ' latest' });
  h.emit('s1', 'chat:delta', { text: 'back' });
  h.purge('s1');
  h.advance(1200);
  assert.equal(h.state.chatItems[0].html, marked.parse('front latest'));
  assert.equal(h.renders.filter(r => r.sessionId === 's1').length, 1);
});

test('timer-only hosts publish current streams within their fallback budget', () => {
  const h = createNativeStreamHarness({ frames: false });
  h.emit('s1', 'chat:delta', { text: 'background' });
  h.advance(32);
  assert.ok(h.notificationCount > 0);
});

test('the host wires activation refresh before subscriber publication', () => {
  assert.match(nativeStreamSources.host, /refreshActiveStreamRender = chatEventsFeature\.refreshActiveStreamRender/);
});

// ---- 评审 P2-5：流式接线探针钩子锁住（burst→markScheduled/markDispatched 调用序列）----
// r2 升级为行为锁：记录式探针桩经 harness 注入（生产钩子读 window.RenderStageProbe），
// 流式事件走真实路由驱动调用序列——删掉任一钩子接线、或把钩子挂错路径，这里的
// 序列断言直接红，取代此前零区分度的源码 grep。
test('流式 delta 经真实路由驱动探针 markDispatched/markScheduled 序列（行为接线锁）', () => {
  const calls = [];
  const h = createNativeStreamHarness({
    count: 1,
    probe: {
      begin() {},
      markScheduled(sid) { calls.push(['markScheduled', sid]); },
      markDispatched(sid) { calls.push(['markDispatched', sid]); },
    },
  });
  // 首条 delta（活跃会话、未抑制）→ 直发路径：notify() 同步发布 → markDispatched
  h.emit('s0', 'chat:delta', { text: 'first' });
  assert.deepEqual(calls, [['markDispatched', 's0']], '直发路径必须打派发标记');

  // 第二条 delta → 合并帧路径：rAF/32ms 定时器武装 → markScheduled；
  // 帧到点 publish → markDispatched
  h.emit('s0', 'chat:delta', { text: ' more' });
  assert.deepEqual(calls.slice(-1), [['markScheduled', 's0']], '合并帧武装必须打调度标记');
  h.advance(60);
  assert.deepEqual(
    calls,
    [['markDispatched', 's0'], ['markScheduled', 's0'], ['markDispatched', 's0']],
    '完整的直发→合并帧→发布序列',
  );
});

// bridge.js 的 begin 钩子在 onSessionEvent 委托包装层里，harness 只运行
// notify() 切片覆盖不到——切出包装层源码在受控沙箱里行为验证：流式事件
// 打 begin、其它事件不打、所有事件原样委托。`pre_chat:delta` 用例同时
// 锁住事件名正则的 ^ 锚定（$-only 锚定会匹配前缀拼凑名而在这里变红）。
test('bridge onSessionEvent 包装层：流式事件打 begin、始终委托共享实现（行为锁）', () => {
  const source = nativeStreamSources.host;
  const start = source.indexOf('function onSessionEvent(e, fn, options) {');
  const delegateAt = source.indexOf('return pinvouSharedtauriMain().onSessionEvent(e, fn, options);', start);
  const end = source.indexOf('}', delegateAt);
  assert.ok(start >= 0 && delegateAt > start && end > delegateAt, 'onSessionEvent 包装层源码切片存在');
  const wrapperSource = source.slice(start, end + 1);

  const begun = [];
  const delegated = [];
  const sandbox = {
    window: { RenderStageProbe: { begin(sid) { begun.push(sid); } } },
    pinvouSharedtauriMain: () => ({
      onSessionEvent(...args) { delegated.push(args); return 'delegated'; },
    }),
  };
  vm.createContext(sandbox);
  vm.runInContext(wrapperSource, sandbox, { filename: 'bridge-onSessionEvent-wrapper.js' });

  assert.equal(sandbox.onSessionEvent({ event: 'chat:delta', payload: { session_id: 'sB' } }), 'delegated');
  sandbox.onSessionEvent({ event: 'chat:reasoning_delta', payload: { session_id: 'sB2' } });
  assert.deepEqual(begun, ['sB', 'sB2'], '两条流式事件各打一次 begin；前缀/后缀拼凑名均不触发');
  sandbox.onSessionEvent({ event: 'pre_chat:delta', payload: { session_id: 'no1' } });
  sandbox.onSessionEvent({ event: 'chat:delta_x', payload: { session_id: 'no3' } });  // r5: $ 锚定
  sandbox.onSessionEvent({ event: 'chat:turn_completed', payload: { session_id: 'no2' } });
  assert.deepEqual(begun, ['sB', 'sB2'], '前缀拼凑名与非流式事件不得触发 begin');
  assert.equal(delegated.length, 5, '所有事件（含未打 begin 的）都必须原样委托');
});

// r7 P1：抑制态 notify 不盖章的**行为锁**——r5 P2-2 的修复本体
// （chat-events notify 钩子的 !isNotifySuppressed() 门控）此前没有任何测试
// 能在删掉门控后变红。场景：后台会话的语义事件（reasoning_done 无
// deferBackgroundNotify）在 runSyncOnSession 内执行——此刻 suppressNotify=true、
// activeSessionId 翻转为该后台 sid，notify() 被调用但**没有任何渲染发布发生**，
// 盖章即假 stall。删掉门控 → markDispatched 记到后台 sid → 第一条断言必红。
test('后台路由期间的抑制态 notify 不打派发标记（r5 P2-2 行为锁）', () => {
  const calls = [];
  const h = createNativeStreamHarness({
    count: 2,
    probe: {
      begin(sid) { calls.push(['begin', sid]); },
      markScheduled(sid) { calls.push(['markScheduled', sid]); },
      markDispatched(sid) { calls.push(['markDispatched', sid]); },
    },
  });
  // s1 是后台会话（s0 活跃）：reasoning_done 的处理器在抑制态路由内跑
  h.emit('s1', 'chat:reasoning_done', { index: 0 });
  assert.equal(
    calls.filter(([kind]) => kind === 'markDispatched').length, 0,
    '抑制态 notify 不得打派发标记（删掉 !isNotifySuppressed() 门控此断言必红）',
  );
  // 对照：同一事件给活跃会话 → 直发路径是真实发布 → 必须盖章
  h.emit('s0', 'chat:reasoning_done', { index: 0 });
  assert.ok(
    calls.some(([kind, sid]) => kind === 'markDispatched' && sid === 's0'),
    '活跃会话的 notify 是真实发布，必须盖章',
  );
});

// r7 P2-2 接线锁：后台渲染腿（1s 预览）发布前必须给探针条目打节流标记——
// 否则 LLM 思考间隙的后台会话每 10s 上报一条 ~1032ms 的节律性假 stall。
// 删掉 chat-events 里的 markThrottled 接线 → 此断言必红。
// 首条 delta 同步渲染（无可再渲染的内容），第二条 delta 的文本要等 1s
// 后沿才渲染——节流标记正是那一条后沿发布的（与「1000ms edge」用例同构）。
test('后台渲染腿给探针条目打节流标记（r7 P2-2 接线锁）', () => {
  const calls = [];
  const h = createNativeStreamHarness({
    count: 2,
    probe: {
      begin() {},
      markScheduled() {},
      markThrottled(sid) { calls.push(['markThrottled', sid]); },
      markDispatched(sid) { calls.push(['markDispatched', sid]); },
    },
  });
  h.emit('s1', 'chat:delta', { text: 'first' });
  h.advance(40);
  h.emit('s1', 'chat:delta', { text: ' latest' });
  h.advance(2200); // 后台渲染腿（1000ms）fire → 渲染 → 后台分支发布
  assert.ok(
    calls.some(([kind, sid]) => kind === 'markThrottled' && sid === 's1'),
    '后台渲染腿必须打节流标记（删掉该接线此断言必红）',
  );
});
