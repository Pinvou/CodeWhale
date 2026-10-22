#!/usr/bin/env node
// 「下载」筛选与集中视图的契约。
//
// 钉三件事：
//   1. 「下载」只列**活跃**的下载任务——终态任务卡仍归右下角面板管；两处同时
//      渲染同一张卡，用户会点两次「取消」，这是必须防住的重复；
//   2. 选中「下载」时琥珀横幅必须让位——它是同一份数据，一页出现两次，用户会
//      以为取消横幅没生效；
//   3. 徽标与三语词典——徽标是横幅之外唯一的主动提醒，三语缺词就是裸奔错误码。
//
// 按源码文本断言（同 model_store_dialog_safety.test.mjs 先例）。
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = (...parts) => fs.readFileSync(path.join(ROOT, ...parts), 'utf8');

const CENTER = read('src', 'features', 'model-store', 'DownloadCenterView.jsx');
const VIEW = read('src', 'features', 'model-store', 'ModelStoreView.jsx');
const PANEL = read('src', 'features', 'model-store', 'ModelStoreTaskPanel.jsx');
const LANGS = ['zh', 'en', 'ja'];
const DICTS = new Map(LANGS.map((lang) => [lang, read('src', 'shared', 'i18n', `${lang}.js`)]));

test('download center lists only active download tasks and reuses the interrupted panel', () => {
  // 活跃判据必须是白名单：running/cancelling。error/cancelled 是终态，归任务面板。
  assert.match(CENTER, /new Set\(\['running', 'cancelling'\]\)/);
  assert.match(CENTER, /task\.kind === 'download' && ACTIVE_STATUS\.has\(task\.status\)/);
  // 已中断分区复用 IncompleteInstallPanel，不实现第二遍。
  assert.match(CENTER, /<IncompleteInstallPanel/);
  // 空态要把「中断可续传」说出来。
  assert.match(CENTER, /downloadsEmptyHint/);
});

test('task card has an inline variant without the floating-panel chrome', () => {
  assert.match(PANEL, /export \{ ModelStoreTaskPanel, ModelStoreTaskCard/);
  assert.match(PANEL, /inline = false/);
  assert.match(PANEL, /inline \? undefined : \{ boxShadow/);
});

test('downloads filter switches the whole list and retires duplicate surfaces', () => {
  // 整页切换：不走 filterModels 管道。
  assert.match(VIEW, /if \(filter === 'downloads'\) \{\s*\n\s*return \(\s*\n\s*<DownloadCenterView/);
  // 横幅在「下载」视图里让位，避免同一份列表一页两次。
  assert.match(VIEW, /\{filter === 'downloads' \? null : \(\s*\n\s*<IncompleteInstallPanel/);
  // 搜索框在「下载」视图里隐藏：它筛的是操作实例，搜索没有意义。
  assert.match(VIEW, /\{filter === 'downloads' \? null : \(\s*\n\s*<IosSearchField/);
  // 徽标挂在筛选胶囊上，且只在有内容时渲染。
  assert.match(VIEW, /model-store-filter-\$\{item\.key\}-badge/);
  assert.match(VIEW, /item\.badge \? \(\s*\n?\s*<span/);
  // 徽标 = 活跃下载任务 + 未完成安装。
  assert.match(VIEW, /activeDownloadTasks\(tasks\)\.length \+ \(library\.incompleteInstallations \|\| \[\]\)\.length/);
});

test('downloads copy exists in all three languages', () => {
  for (const text of DICTS.values()) {
    for (const key of ['filterDownloads:', 'downloadsTitle:', 'downloadsEmptyTitle:', 'downloadsEmptyHint:']) {
      assert.ok(text.includes(key), `${key} 缺失`);
    }
  }
});
