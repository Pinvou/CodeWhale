// 「模型市场」和「我的模型」合并成一份列表 —— 这是整个页面的地基，错了满屏都错。
//
// 用例里的字段取值全部来自真机实测（测试设备 上那 5 张卡），不是编的：
// 库记录用 card_id/card_name，商店卡片用 id/name，两套字段名不一样，这正是
// 合并最容易出错的地方。
import assert from 'node:assert/strict';
import test from 'node:test';

import {
  filterModels,
  mergeModels,
  sortModels,
} from '../src/features/model-store/model-store-logic.js';

/** 商店卡片（未下载）：字段名是 id/name。 */
const STORE_QWEN = {
  id: 'qwen38-27b-sglang0519',
  name: 'Qwen3.8-27B (NVFP4 · SGLang v0.5.19)',
  org: 'Qwen',
  description: '基于 SGLang v0.5.19 的单机部署包',
  engine_name: 'sglang',
  engine_version: 'v0.5.19',
  quantization: 'NVFP4',
  size_bytes: 23444511852,
  deploy_topology: 'single',
};
const STORE_ORNITH = {
  id: 'ornith15-bf16',
  name: 'Ornith-1.5-35B-A3B',
  org: 'ornith-ai',
  engine_name: 'vllm',
  quantization: 'BF16',
  size_bytes: 67000000000,
  deploy_topology: 'single',
};

/** 库记录（已下载）：字段名是 card_id/card_name，且带运行态。 */
const LIB_QWEN_RUNNING = {
  card_id: 'qwen38-27b-sglang0519',
  card_name: 'Qwen3.8-27B (NVFP4 · SGLang v0.5.19)',
  model_name: 'qwen3.8-27b-nvfp4-sglang-v0.5.19--nvfp4',
  org: 'Qwen',
  engine_name: 'sglang',
  engine_version: 'v0.5.19',
  quantization: 'NVFP4',
  size_bytes: 23444511852,
  deploy_topology: 'single',
  source_repo_id: 'unsloth/Qwen3.8-27B-NVFP4',
  status: 'deployed',
  running: true,
};

test('同一张卡在两边都有时只出现一次，且以库记录为准', () => {
  // 之所以钉死这条：合并写错最典型的表现就是同一个模型出现两行，
  // 一行"可下载"一行"运行中"，用户完全不知道该点哪个。
  const merged = mergeModels([STORE_QWEN, STORE_ORNITH], { models: [LIB_QWEN_RUNNING] });
  assert.equal(merged.length, 2, '两张卡合并后就是两项');

  const qwen = merged.find((m) => m.id === 'qwen38-27b-sglang0519');
  assert.equal(qwen.state, 'running', '库记录说它在跑，就得是 running，不能被商店那份覆盖成 available');
  assert.equal(qwen.record, LIB_QWEN_RUNNING, 'record 必须挂上，后续所有生命周期操作都要用它');
  // 字段名归一：库记录叫 card_name，合并后统一成 name
  assert.equal(qwen.name, 'Qwen3.8-27B (NVFP4 · SGLang v0.5.19)');
});

test('库记录缺的字段用商店那份补，已有的不覆盖', () => {
  // description 在库记录里经常是空的（商店才有），但 size_bytes 这类
  // 库记录自己有的，绝不能被商店的旧值盖掉。
  const merged = mergeModels([STORE_QWEN], { models: [{ ...LIB_QWEN_RUNNING, description: undefined, size_bytes: 111 }] });
  const qwen = merged[0];
  assert.equal(qwen.description, '基于 SGLang v0.5.19 的单机部署包', '缺的要补');
  assert.equal(qwen.size_bytes, 111, '库记录已有的不许被商店覆盖');
});

test('只在商店里的是 available，record 为 null', () => {
  const merged = mergeModels([STORE_ORNITH], { models: [] });
  assert.equal(merged[0].state, 'available');
  assert.equal(merged[0].record, null, 'record 必须是 null 而不是 undefined：调用方靠它判断能不能做生命周期操作');
});

test('只在本地库里的也要出现（商店可能已经下架了这张卡）', () => {
  // 商店下架、或者商店服务器连不上时 cards 是空的。这时候本地已下载的模型
  // 绝不能从列表里消失——用户机器上明明有，界面上找不到是最糟的。
  const merged = mergeModels([], { models: [LIB_QWEN_RUNNING] });
  assert.equal(merged.length, 1);
  assert.equal(merged[0].state, 'running');
});

test('三档已下载状态各自对得上', () => {
  const base = { card_id: 'x', card_name: 'X' };
  const stateOf = (extra) => mergeModels([], { models: [{ ...base, ...extra }] })[0].state;
  assert.equal(stateOf({ running: true, status: 'deployed' }), 'running');
  assert.equal(stateOf({ running: false, status: 'deployed' }), 'deployed');
  assert.equal(stateOf({ running: false, status: 'pending' }), 'downloaded', '下完但没部署');
});

test('脏数据不许让整页崩掉', () => {
  // 商店响应的形状是远端给的，库记录来自另一个进程。任何一边给了 null/缺 id，
  // 都只该丢掉那一条，不该让整个列表炸。
  assert.deepEqual(mergeModels(null, null), []);
  assert.deepEqual(mergeModels(undefined, { models: null }), []);
  assert.equal(mergeModels([{ name: '没有 id' }], { models: [{ card_name: '没有 card_id' }] }).length, 0);
});

test('filterModels：installed 指"本机有权重的"，不管部署没部署', () => {
  const merged = mergeModels([STORE_ORNITH], {
    models: [LIB_QWEN_RUNNING, { card_id: 'z', card_name: 'Z', status: 'pending' }],
  });
  const installed = filterModels(merged, { filter: 'installed' });
  assert.deepEqual(installed.map((m) => m.id).sort(), ['qwen38-27b-sglang0519', 'z']);
  const running = filterModels(merged, { filter: 'running' });
  assert.deepEqual(running.map((m) => m.id), ['qwen38-27b-sglang0519']);
  assert.equal(filterModels(merged, { filter: 'all' }).length, 3);
});

test('filterModels：关键词命中名称/提供方/引擎/量化', () => {
  // 用户记得住的往往不是全名，而是「那个 qwen 的」「那个 NVFP4 的」。
  const merged = mergeModels([STORE_QWEN, STORE_ORNITH], { models: [] });
  const hit = (q) => filterModels(merged, { query: q }).map((m) => m.id);
  assert.deepEqual(hit('qwen'), ['qwen38-27b-sglang0519']);
  assert.deepEqual(hit('NVFP4'), ['qwen38-27b-sglang0519'], '大小写无关');
  assert.deepEqual(hit('vllm'), ['ornith15-bf16']);
  assert.deepEqual(hit('ornith-ai'), ['ornith15-bf16'], '提供方也要能命中');
  assert.equal(hit('不存在的东西').length, 0);
});

test('filterModels：筛选和关键词是与的关系', () => {
  const merged = mergeModels([STORE_ORNITH], { models: [LIB_QWEN_RUNNING] });
  assert.equal(filterModels(merged, { filter: 'installed', query: 'ornith' }).length, 0,
    'ornith 没下载，即使关键词命中也不该出现在"已安装"里');
});

test('sortModels：能用的排前面，同档按名字', () => {
  const models = [
    { id: 'd', name: '待下载', state: 'available' },
    { id: 'b', name: '已部署', state: 'deployed' },
    { id: 'a', name: '运行中', state: 'running' },
    { id: 'c', name: '已下载', state: 'downloaded' },
  ];
  assert.deepEqual(sortModels(models).map((m) => m.id), ['a', 'b', 'c', 'd']);
  // 不改入参：调用方常常把同一个数组同时喂给别的地方
  assert.deepEqual(models.map((m) => m.id), ['d', 'b', 'a', 'c']);
});

test('sortModels：未知状态排到最后而不是插队', () => {
  // 后端将来可能加新状态。加了之后前端不该把它误排到最前面。
  const models = [
    { id: 'new', name: 'N', state: '将来才有的状态' },
    { id: 'run', name: 'R', state: 'running' },
  ];
  assert.deepEqual(sortModels(models).map((m) => m.id), ['run', 'new']);
});
