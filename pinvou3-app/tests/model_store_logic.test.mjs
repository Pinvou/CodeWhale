#!/usr/bin/env node
// features/model-store/model-store-logic.js 的纯逻辑单测。
//
// 这条链路上最容易出错的不是渲染，而是「脚本输出怎么折叠成界面状态」：进度行和
// 交互提示行必须被拦下来不进日志、又必须按出现顺序产生副作用；测速窗口不能被
// 无限增长的采样拖垮；进度被脚本算错时界面也不能把 120% 摆给用户看。
import assert from 'node:assert/strict';
import test from 'node:test';
import {
  MAX_LOG_LINES,
  UNLOCK_CLICKS,
  UNLOCK_CLICK_GAP_MS,
  advanceUnlockClicks,
  computeSpeed,
  etaSeconds,
  filterStoreCards,
  formatBytes,
  libraryStatusOf,
  storeGuideState,
  missingFormFields,
  parseProgressLine,
  parsePromptLine,
  progressPercent,
  reduceLogChunk,
  splitDuration,
  uninstallDoneText,
  uninstallOptionsFor,
  trimSamples,
} from '../src/features/model-store/model-store-logic.js';

const emptyState = () => ({ lines: [], progress: null, speed: null, prompt: null, samples: [] });

test('formatBytes：按 1024 进位，B 取整、其余保留一位小数', () => {
  assert.equal(formatBytes(0), '0B');
  assert.equal(formatBytes(512), '512B');
  assert.equal(formatBytes(1024), '1.0KB');
  assert.equal(formatBytes(1024 ** 3 * 1.5), '1.5GB');
  assert.equal(formatBytes(null), '0B');
});

test('parseProgressLine：识别进度行，并把已下载夹到总量以内', () => {
  assert.deepEqual(parseProgressLine('LMM_PROGRESS_BYTES=100/200'), { done: 100, total: 200 });
  // 真机上出现过「51.0GB / 30.6GB」：并发下载留下的孤儿临时块被重复统计了
  assert.deepEqual(parseProgressLine('LMM_PROGRESS_BYTES=300/200'), { done: 200, total: 200 });
  // 总量未知时不夹取，交给界面退化成不确定态
  assert.deepEqual(parseProgressLine('LMM_PROGRESS_BYTES=300/0'), { done: 300, total: 0 });
  assert.equal(parseProgressLine('正在下载 …'), null);
});

test('parsePromptLine：三种一问一答类型，不认识的退化成 text', () => {
  assert.deepEqual(parsePromptLine('LMM_PROMPT=text|请输入从机 IP'), { kind: 'text', message: '请输入从机 IP' });
  assert.deepEqual(parsePromptLine('LMM_PROMPT=password|密码'), { kind: 'password', message: '密码' });
  assert.deepEqual(parsePromptLine('LMM_PROMPT=confirm|继续吗'), { kind: 'confirm', message: '继续吗' });
  // 卡片脚本可能比应用新，多一种输入类型不该让整个流程跑不动
  assert.deepEqual(parsePromptLine('LMM_PROMPT=otp|验证码'), { kind: 'text', message: '验证码' });
  // 没有分隔符时整行都是提示语
  assert.deepEqual(parsePromptLine('LMM_PROMPT=请输入'), { kind: 'text', message: '请输入' });
  assert.equal(parsePromptLine('普通日志'), null);
});

test('parsePromptLine：表单式一次问齐多项', () => {
  const spec = { title: '从机信息', fields: [{ id: 'ip', label: 'IP' }, { id: 'pw', kind: 'password' }] };
  const parsed = parsePromptLine(`LMM_PROMPT_FORM=${JSON.stringify(spec)}`);
  assert.equal(parsed.kind, 'form');
  assert.equal(parsed.title, '从机信息');
  assert.equal(parsed.fields.length, 2);
});

test('parsePromptLine：表单 JSON 写坏或没有字段时退化成普通日志行', () => {
  // 吞掉这一行会让用户对着不动的进度条等到超时；退化成日志至少看得见发生了什么
  assert.equal(parsePromptLine('LMM_PROMPT_FORM={坏掉的 json'), null);
  assert.equal(parsePromptLine('LMM_PROMPT_FORM={"fields":[]}'), null);
});

test('reduceLogChunk：进度行与提示行不进日志，普通行按序累积', () => {
  const next = reduceLogChunk(
    emptyState(),
    ['开始下载', 'LMM_PROGRESS_BYTES=50/100', 'LMM_PROMPT=text|输入什么', '仍在继续'],
    1000,
  );
  assert.deepEqual(next.lines, ['开始下载', '仍在继续']);
  assert.deepEqual(next.progress, { done: 50, total: 100 });
  assert.deepEqual(next.prompt, { kind: 'text', message: '输入什么' });
  assert.equal(next.samples.length, 1);
});

test('reduceLogChunk：日志行数封顶，保留最后 MAX_LOG_LINES 行', () => {
  const chunk = Array.from({ length: MAX_LOG_LINES + 20 }, (_, index) => `line-${index}`);
  const next = reduceLogChunk(emptyState(), chunk, 0);
  assert.equal(next.lines.length, MAX_LOG_LINES);
  assert.equal(next.lines[next.lines.length - 1], `line-${MAX_LOG_LINES + 19}`);
});

test('reduceLogChunk：连续两批进度行能算出速度', () => {
  const first = reduceLogChunk(emptyState(), ['LMM_PROGRESS_BYTES=0/1000'], 0);
  const second = reduceLogChunk(first, ['LMM_PROGRESS_BYTES=500/1000'], 5000);
  assert.equal(second.speed, 100); // 5 秒传了 500 字节
});

test('computeSpeed：样本不足或计数倒退时不给速度', () => {
  assert.equal(computeSpeed([]), null);
  assert.equal(computeSpeed([{ t: 0, done: 1 }]), null);
  assert.equal(computeSpeed([{ t: 0, done: 500 }, { t: 1000, done: 100 }]), null);
  assert.equal(computeSpeed([{ t: 0, done: 0 }, { t: 2000, done: 200 }]), 100);
});

test('trimSamples：窗口外的老样本被丢掉，但至少留两个用来算速度', () => {
  const samples = [{ t: 0, done: 0 }, { t: 1, done: 1 }, { t: 2, done: 2 }, { t: 100000, done: 3 }];
  const kept = trimSamples(samples, 100000);
  assert.ok(kept.length >= 2);
  assert.ok(kept.length < samples.length);
});

test('progressPercent / etaSeconds：算不出时一律返回 null', () => {
  assert.equal(progressPercent(null), null);
  assert.equal(progressPercent({ done: 1, total: 0 }), null);
  assert.equal(progressPercent({ done: 50, total: 200 }), 25);
  assert.equal(etaSeconds({ done: 50, total: 100 }, 0), null);
  assert.equal(etaSeconds({ done: 100, total: 100 }, 10), null);
  assert.equal(etaSeconds({ done: 50, total: 100 }, 10), 5);
});

test('splitDuration：秒 / 分钟 / 小时三档，非法输入返回 null', () => {
  assert.deepEqual(splitDuration(45), { unit: 'sec', value: 45, rest: 0 });
  assert.deepEqual(splitDuration(605), { unit: 'min', value: 10, rest: 0 });
  assert.deepEqual(splitDuration(3600 * 2 + 60 * 5), { unit: 'hour', value: 2, rest: 5 });
  assert.equal(splitDuration(-1), null);
  assert.equal(splitDuration(Number.NaN), null);
});

test('storeGuideState：只有「还在问 / 这个平台没有 / 可用」三档', () => {
  assert.equal(storeGuideState(null), 'loading');
  assert.equal(storeGuideState({ available: true, reason: null }), 'ready');
  assert.equal(storeGuideState({ available: false, reason: '只在 Linux 上可用' }), 'unsupported');
  // 缺字段按不可用算：宁可少显示一个页面，也不要让一个什么都做不了的页面出现。
  assert.equal(storeGuideState({}), 'unsupported');
});

test('libraryStatusOf：运行中优先于已部署', () => {
  assert.equal(libraryStatusOf({ running: true, status: 'deployed' }), 'running');
  assert.equal(libraryStatusOf({ running: false, status: 'deployed' }), 'deployed');
  assert.equal(libraryStatusOf({ running: false, status: 'downloaded' }), 'pending');
  assert.equal(libraryStatusOf(null), 'pending');
});

test('filterStoreCards：名称/提供方/引擎/量化都能命中，大小写无关', () => {
  const cards = [
    { name: 'Qwen3-32B', org: 'Alibaba', engine_name: 'vllm', quantization: 'AWQ' },
    { name: 'Llama-3', org: 'Meta', engine_name: 'sglang', quantization: null },
  ];
  assert.equal(filterStoreCards(cards, '').length, 2);
  assert.equal(filterStoreCards(cards, '   ').length, 2);
  assert.equal(filterStoreCards(cards, 'qwen').length, 1);
  assert.equal(filterStoreCards(cards, 'meta').length, 1);
  assert.equal(filterStoreCards(cards, 'awq').length, 1);
  assert.equal(filterStoreCards(cards, 'sglang').length, 1);
  assert.equal(filterStoreCards(null, 'x').length, 0);
});

test('missingFormFields：required 默认为真，空白串不算填了', () => {
  const fields = [{ id: 'ip' }, { id: 'note', required: false }, { id: 'pw' }];
  const missing = missingFormFields(fields, { ip: '10.0.0.2', pw: '   ' });
  assert.deepEqual(missing.map((field) => field.id), ['pw']);
  assert.equal(missingFormFields([], {}).length, 0);
});

test('advanceUnlockClicks：连点够 7 次才算数，计数随即归零', () => {
  let state = null;
  for (let i = 1; i < UNLOCK_CLICKS; i += 1) {
    state = advanceUnlockClicks(state, i * 100);
    assert.equal(state.reached, false, `第 ${i} 下不该触发`);
    assert.equal(state.count, i);
  }
  state = advanceUnlockClicks(state, UNLOCK_CLICKS * 100);
  assert.equal(state.reached, true);
  // 归零：口令框关掉或输错之后，再连点七次应该还能再叫出来一次
  assert.equal(state.count, 0);
});

test('advanceUnlockClicks：间隔超过时间窗就重新计数，这一下算新一轮的第一下', () => {
  let state = { count: 6, lastAt: 1000 };
  state = advanceUnlockClicks(state, 1000 + UNLOCK_CLICK_GAP_MS + 1);
  assert.equal(state.reached, false);
  assert.equal(state.count, 1);
  // 卡在窗口边界上的那一下仍然算连点（> 才重置，不是 >=）
  state = { count: 6, lastAt: 1000 };
  assert.equal(advanceUnlockClicks(state, 1000 + UNLOCK_CLICK_GAP_MS).reached, true);
});

test('advanceUnlockClicks：第一次点击没有历史状态，也不会把 NaN 带进计数', () => {
  assert.deepEqual(advanceUnlockClicks(null, 0), { count: 1, lastAt: 0, reached: false });
  assert.deepEqual(advanceUnlockClicks(undefined, 500), { count: 1, lastAt: 500, reached: false });
});

// —— 卸载完成文案：必须按服务端回的实际结果说话 ——
//
// 这几条测的是一个真实踩过的坑：卸载脚本退出 0、库记录已删，但权重目录其实没删掉
// （设备上 21 GB 的 Qwen3.8-27B-NVFP4 就是这么留下的）。原来的实现按用户点了哪个
// 按钮拼话，那种情况下会说「模型权重已删除」——磁盘一个字节没少，而卡片已经从列表
// 消失，此后没有任何界面能管到那堆文件。
const COPY = {
  uninstallDonePurged: (n, s) => `purged:${n}:${s}`,
  uninstallDoneResidual: (n, c) => `residual:${n}:${c}`,
  uninstallDoneKept: (n) => `kept:${n}`,
};

test('uninstallDoneText：真删掉了才说"已删除"，并报出实测释放量', () => {
  const text = uninstallDoneText(COPY, 'Qwen', {
    purged_weights: true,
    freed_bytes: 21 * 1024 * 1024 * 1024,
    remaining_paths: [],
  });
  assert.equal(text, 'purged:Qwen:21.0GB');
});

test('uninstallDoneText：说要删却留下残留时必须改口，不能承诺没发生的事', () => {
  const text = uninstallDoneText(COPY, 'Qwen', {
    purged_weights: true,
    freed_bytes: 0,
    remaining_paths: ['/home/sky/models/Qwen3.8-27B-NVFP4'],
  });
  assert.equal(text, 'residual:Qwen:1');
});

test('uninstallDoneText：仅卸载保留权重时，权重还在不算残留', () => {
  const text = uninstallDoneText(COPY, 'Qwen', {
    purged_weights: false,
    freed_bytes: 0,
    remaining_paths: ['/home/sky/models/Qwen3.8-27B-NVFP4'],
  });
  assert.equal(text, 'kept:Qwen');
});

test('uninstallDoneText：结果缺失时退回"已保留"，宁可少说也不凭空承诺释放', () => {
  assert.equal(uninstallDoneText(COPY, 'Qwen', null), 'kept:Qwen');
  assert.equal(uninstallDoneText(COPY, 'Qwen', undefined), 'kept:Qwen');
  assert.equal(uninstallDoneText(COPY, 'Qwen', {}), 'kept:Qwen');
});

// ---------------------------------------------- 卸载对话框该给几个选项
//
// 「卸载」＝把模型从引擎上撤下来。卡片只是下载在本机时这件事无从做起，摆出来
// 就是个空按钮；用户真正想要的（腾磁盘）叫「删除」。这一档是卸载之后的落点，
// 所以这条线走得通不通，直接决定用户能不能把盘收回来。
test('uninstallOptionsFor：已部署 / 运行中给两个选项', () => {
  assert.equal(uninstallOptionsFor({ status: 'deployed', running: false }), 'uninstall-or-delete');
  assert.equal(uninstallOptionsFor({ status: 'deployed', running: true }), 'uninstall-or-delete');
  // running 为真时即便 status 还没跟上，也算已部署——判据跟 libraryStateOf 同源
  assert.equal(uninstallOptionsFor({ status: 'downloaded', running: true }), 'uninstall-or-delete');
});

test('uninstallOptionsFor：只下载过、没部署过时只给「删除」', () => {
  assert.equal(uninstallOptionsFor({ status: 'downloaded', running: false }), 'delete-only');
  // 卸载之后 LMM 把记录退回这个状态，此时再点垃圾桶应当只剩「删除」
  assert.equal(uninstallOptionsFor({ status: 'downloaded' }), 'delete-only');
  assert.equal(uninstallOptionsFor({}), 'delete-only');
  assert.equal(uninstallOptionsFor(null), 'delete-only');
});
