import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = fs.readFileSync(path.join(root, 'src/shared/render-stage-probe.js'), 'utf8');

function harness({ stallRecorded = true } = {}) {
  const recorded = [];
  const rafQueue = [];
  const window = {
    requestAnimationFrame: fn => { rafQueue.push(fn); return rafQueue.length; },
    // 探针模块以 window 为 root（浏览器侧），诊断桩必须挂在同一对象上
    AuthoritySyncDiagnostics: stallRecorded ? { record: (event, details) => recorded.push({ event, details }) } : undefined,
  };
  const sandbox = { window };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'render-stage-probe.js' });
  return { probe: window.RenderStageProbe || sandbox.globalThis.RenderStageProbe, recorded, rafQueue, window };
}

function flushFrames(h) {
  const queue = h.rafQueue.splice(0);
  for (const fn of queue) fn();
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
