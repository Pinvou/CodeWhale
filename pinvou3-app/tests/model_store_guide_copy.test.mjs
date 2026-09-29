#!/usr/bin/env node
// 引导态 → 文案键的契约测试。
//
// 为什么值得单独一个文件：这条链路断掉的时候**没有任何报错**。`GUIDE_TITLE_KEYS`
// 查不到就 `|| copy.guideLoadingTitle` 兜底，于是界面照常渲染，只是说了句不对的话。
// 首次合入时就漏过一格，结果最常走的那条路径上标题写着「无法使用」，而那其实是
// 一次完全正常的加载。
//
// 所以这里同时钉两头：`storeGuideState` 能吐出的每一个非 ready 状态都要在映射表里
// 有一格，且那一格指向的键必须在**三份词典里都存在**。少任何一边都算失败。
//
// 2026-09-21 重写。原版写于 sidecar 时期，钉的是当时的五态设计（loading/stopped/
// starting/error/not-installed，从 sidecar 的进程状态推导）。09-18 模型商店改成
// 品悟内的原生模块之后，「sidecar 起没起来」这件事不存在了，状态收敛成三态：
// 还没问到 / 能用 / 这个平台没有。原版 import 的 `lmmGuideState` 早已改名为
// `storeGuideState`，ESM 具名导入直接 SyntaxError，整个文件 0 条用例在跑——
// 而且改名也救不回来，它断言的那几个状态本身已经不存在了。
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { storeGuideState } from '../src/features/model-store/model-store-logic.js';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = (...parts) => fs.readFileSync(path.join(ROOT, ...parts), 'utf8');

// ModelStoreCards.jsx 是 JSX，node --test 没有 loader，所以按源码文本取映射表。
// 仓库里已有先例（tests/move_dialog_contract.test.mjs 就是这么钉 main.jsx 的）。
const CARDS = read('src', 'features', 'model-store', 'ModelStoreCards.jsx');

/**
 * 从源码里取出 `GUIDE_TITLE_KEYS` 的 state → 文案键映射。
 *
 * 按「键: 值」逐对抓，不按行——这张表现在是单行写的，逐行解析只会得到一张空表，
 * 而空表会让下面每条断言都轻松通过。原版正是逐行的：表改成单行之后它不报错，
 * 只是静默失去了全部约束力。
 * @returns {Map<string, string>} 映射表。
 */
function guideTitleKeys() {
  const block = /const GUIDE_TITLE_KEYS = \{([\s\S]*?)\};/.exec(CARDS);
  assert.ok(block, 'ModelStoreCards.jsx 里找不到 GUIDE_TITLE_KEYS —— 改名了就把这个测试一起改');
  const map = new Map();
  for (const [, state, key] of block[1].matchAll(/'?([\w-]+)'?\s*:\s*'([\w]+)'/g)) {
    map.set(state, key);
  }
  assert.ok(map.size > 0, 'GUIDE_TITLE_KEYS 解析出来是空的 —— 解析器跟不上写法了');
  return map;
}

// storeGuideState 能返回的全部状态。'ready' 不进引导态（那时候渲染的是列表本身），
// 其余每一个都必须有标题。
const GUIDE_STATES = [
  storeGuideState(null),
  storeGuideState(undefined),
  storeGuideState({ available: true }),
  storeGuideState({ available: false, reason: '这个平台不支持' }),
].filter((state) => state !== 'ready');

test('GUIDE_TITLE_KEYS 覆盖 storeGuideState 的每一个非 ready 状态', () => {
  const map = guideTitleKeys();
  for (const state of new Set(GUIDE_STATES)) {
    assert.ok(
      map.has(state),
      `引导态 '${state}' 没有标题键。查不到不会报错，只会静默回落到 guideLoadingTitle —— `
        + '于是「这个平台不支持」会被说成「正在加载」，用户会一直等下去'
    );
  }
});

test('storeGuideState 只有三态，多一个就要补映射', () => {
  // 钉住状态集合本身。将来谁加了第四态（比如「商店连不上」），这条会先红，
  // 提醒他去 GUIDE_TITLE_KEYS 补一格——而不是等用户看到一句不对的提示。
  const all = new Set([
    storeGuideState(null),
    storeGuideState({ available: true }),
    storeGuideState({ available: false }),
  ]);
  assert.deepEqual([...all].sort(), ['loading', 'ready', 'unsupported']);
});

test('每个引导态标题键在 zh/en/ja 三份词典里都存在', () => {
  const dictionaries = ['zh', 'en', 'ja'].map((lang) => ({
    lang,
    text: read('src', 'shared', 'i18n', `${lang}.js`),
  }));
  for (const key of new Set(guideTitleKeys().values())) {
    for (const { lang, text } of dictionaries) {
      assert.ok(
        text.includes(`${key}:`),
        `${lang}.js 里缺 ${key} —— 缺的那一语会静默回落到 guideLoadingTitle`
      );
    }
  }
});
