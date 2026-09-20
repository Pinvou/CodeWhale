#!/usr/bin/env node
// 引导态 → 文案键的契约测试。
//
// 为什么值得单独一个文件：这条链路断掉的时候**没有任何报错**。`GUIDE_TITLE_KEYS`
// 查不到就 `|| copy.guideErrorTitle` 兜底，于是界面照常渲染，只是说了句相反的话。
// 首次合入时就漏了 'loading' 这一格 —— 而 `lmmGuideState(null)` 返回的正是
// 'loading'（status 还没拿到的首帧，每次进页面都有），自动拉起 sidecar 的那 15 秒
// 也是它。结果最常走的一条路径上，标题写着「本地模型管理无法使用」，下面配一个转圈
// 的「正在启动…」按钮。
//
// 所以这里同时钉两头：`lmmGuideState` 能吐出的每一个状态都要在映射表里有一格，
// 且那一格指向的键必须在**三份词典里都存在**。少任何一边都算失败。
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { lmmGuideState } from '../src/features/model-store/model-store-logic.js';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = (...parts) => fs.readFileSync(path.join(ROOT, ...parts), 'utf8');

// ModelStoreCards.jsx 是 JSX，node --test 没有 loader，所以按源码文本取映射表。
// 仓库里已有先例（tests/move_dialog_contract.test.mjs 就是这么钉 main.jsx 的）。
const CARDS = read('src', 'features', 'model-store', 'ModelStoreCards.jsx');

/**
 * 从源码里取出 `GUIDE_TITLE_KEYS` 的 state → 文案键映射。
 * @returns {Map<string, string>} 映射表。
 */
function guideTitleKeys() {
  const block = /const GUIDE_TITLE_KEYS = \{([\s\S]*?)\};/.exec(CARDS);
  assert.ok(block, 'ModelStoreCards.jsx 里找不到 GUIDE_TITLE_KEYS —— 改名了就把这个测试一起改');
  const map = new Map();
  for (const line of block[1].split('\n')) {
    const entry = /^\s*'?([\w-]+)'?\s*:\s*'([\w]+)'\s*,?\s*$/.exec(line);
    if (entry) map.set(entry[1], entry[2]);
  }
  assert.ok(map.size > 0, 'GUIDE_TITLE_KEYS 解析出来是空的');
  return map;
}

// lmmGuideState 能返回的全部状态。'ready' 不进引导态（那时候渲染的是列表本身），
// 其余每一个都必须有标题。
const GUIDE_STATES = [
  lmmGuideState(null),
  lmmGuideState({ installed: false }),
  lmmGuideState({ installed: true, error: '起不来' }),
  lmmGuideState({ installed: true, running: false }),
].filter((state) => state !== 'ready');

test('GUIDE_TITLE_KEYS 覆盖 lmmGuideState 的每一个非 ready 状态', () => {
  const map = guideTitleKeys();
  for (const state of new Set(GUIDE_STATES)) {
    assert.ok(
      map.has(state),
      `引导态 '${state}' 没有标题键。查不到不会报错，只会静默回落到 guideErrorTitle，`
        + '对用户说「无法使用」——而这可能是一条完全正常的路径'
    );
  }
});

test("ModelStoreView 把自动拉起期间映射成的 'loading' 也在映射表里", () => {
  // ModelStoreView.jsx 里那句 `guide === 'stopped' && starting && !startError ? 'loading' : guide`
  // 是凭空造出一个 lmmGuideState 不会返回的状态，最容易被漏掉的就是它。
  const view = read('src', 'features', 'model-store', 'ModelStoreView.jsx');
  assert.match(view, /\? 'loading' : guide/, '自动拉起期间的 loading 映射不见了，这个测试要跟着改');
  assert.ok(guideTitleKeys().has('loading'));
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
        `${lang}.js 里缺 ${key} —— 缺的那一语会静默回落到 guideErrorTitle`
      );
    }
  }
});
