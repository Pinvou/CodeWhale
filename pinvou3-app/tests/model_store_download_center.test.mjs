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
  // install_local（AI/MCP 代办的下载类安装）与 UI 下载按同一张卡归一。
  assert.match(CENTER, /new Set\(\['running', 'cancelling'\]\)/);
  assert.match(CENTER, /new Set\(\['download', 'install_local'\]\)/);
  assert.match(CENTER, /DOWNLOAD_KINDS\.has\(task\.kind\) && ACTIVE_STATUS\.has\(task\.status\)/);
  // 已中断分区复用 IncompleteInstallPanel，不实现第二遍。
  assert.match(CENTER, /<IncompleteInstallPanel/);
  // 有活跃任务的卡片由「正在下载」代表：它的回执行不再重复出现在「已中断」。
  assert.match(CENTER, /const interrupted = installations\.filter\(\(item\) => !activeCards\.has\(item\.cardId\)\)/);
  // 空态要把「中断可续传」说出来。
  assert.match(CENTER, /downloadsEmptyHint/);
});

test('orphan section scans on demand and deletes only after an explicit confirmation', () => {
  const API = read('src', 'features', 'model-store', 'model-store-api.js');
  assert.match(API, /scanOrphans\(\)[\s\S]{0,60}model_store_scan_orphans/);
  assert.match(API, /removeOrphan\(path\)[\s\S]{0,60}model_store_remove_orphan/);
  // 扫描必须由按钮触发，本分区没有自动扫描的 effect——进页面就 du 上百 GB
  // 等于惩罚打开这个视图的人。
  assert.match(CENTER, /data-testid="model-store-orphan-scan"/);
  assert.doesNotMatch(CENTER, /useEffect\([\s\S]{0,200}scanOrphans/);
  // 删除两步确认：先出确认条（含完整路径与「不可恢复」承诺），再发请求。
  assert.match(CENTER, /data-testid="model-store-orphan-delete"/);
  assert.match(CENTER, /data-testid="model-store-orphan-confirm-button"/);
  assert.match(CENTER, /removeOrphan\(target\.path\)/);
  // 删除完成后必须重扫，不能让用户对着幽灵条目。
  const removeBody = CENTER.match(/const remove = async target => \{([\s\S]*?)\n  const entries =/)[1];
  assert.match(removeBody, /await removeOrphan\(target\.path\)[\s\S]*setResult\(await scanOrphans\(\)\)/);
  assert.match(removeBody, /catch \(failure\) \{\s*setResult\(null\)/);
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
  // 浮层在「下载」视图里必须剔除活跃下载卡：同一张卡两处渲染，日志展开/
  // 应答状态会各走各的。这是双渲染互斥的另一半（中心只列活跃是前一半）。
  assert.match(VIEW, /tasks=\{filter === 'downloads'\s*\n\s*\? floatingDownloadTasks\(tasks\)\s*\n\s*: tasks\}/);
  // 徽标挂在筛选胶囊上，且只在有内容时渲染。
  assert.match(VIEW, /model-store-filter-\$\{item\.key\}-badge/);
  assert.match(VIEW, /item\.badge \? \(\s*\n?\s*<span/);
  // 徽标走按卡片去重的统一实现：续传进行中时，活跃任务和它依据的失败回执
  // 只算一次。
  assert.match(VIEW, /const downloadBadge = downloadBadgeCount\(tasks, library\.incompleteInstallations\);/);
  assert.match(CENTER, /export function downloadBadgeCount\(tasks, installations\)/);
  assert.match(CENTER, /\.filter\(\(item\) => !activeCards\.has\(item\.cardId\)\)\.length/);
});

test('downloads copy exists in all three languages', () => {
  for (const text of DICTS.values()) {
    for (const key of ['filterDownloads:', 'downloadsTitle:', 'downloadsEmptyTitle:', 'downloadsEmptyHint:']) {
      assert.ok(text.includes(key), `${key} 缺失`);
    }
  }
});

test('orphan copy exists in all three languages and promises irreversibility', () => {
  const KEYS = ['orphansTitle:', 'orphansHint:', 'orphansScan:', 'orphansScanning:', 'orphansPartial:', 'orphansUnresolved:', 'orphansOwnershipUnresolved:', 'orphansEmpty:', 'orphansDelete:', 'orphansDeleteConfirmTitle:', 'orphansDeleteConfirmHint:', 'orphansDeleted:', 'orphanErrors:'];
  for (const text of DICTS.values()) {
    for (const key of KEYS) {
      assert.ok(text.includes(key), `${key} 缺失`);
    }
    // 后端拒绝码必须都有人话对应，缺一个界面上就裸奔错误码。
    for (const code of ['orphan_scan_unavailable', 'orphan_directory_owned', 'orphan_directory_other_user', 'orphan_activity_unresolved', 'orphan_library_ownership_unresolved', 'orphan_path_unsafe', 'orphan_remove_failed', 'orphan_resources_busy']) {
      assert.ok(text.includes(`${code}:`), `orphanErrors.${code} 缺失`);
    }
  }
  // 删除确认必须把「不可恢复」说出口——这是本分区最重要的承诺。
  assert.match(DICTS.get('zh'), /orphansDeleteConfirmHint:'[^']*不可恢复[^']*'/);
  assert.match(DICTS.get('en'), /orphansDeleteConfirmHint:'[^']*irreversib[^']*'/);
  assert.match(DICTS.get('ja'), /orphansDeleteConfirmHint:'[^']*元に戻せません[^']*'/);
});
