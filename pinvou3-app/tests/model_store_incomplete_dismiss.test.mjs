#!/usr/bin/env node
// 未完成安装「放弃」能力的契约。
//
// 这条链路的每一段都是**静默失败也算成功**的形态，只能靠契约钉住：
//   1. 后端只打标不删文件——放弃的文案承诺"文件原样保留、可续传"，如果实现
//      哪天顺手删了文件，文案就变成谎言，用户以为还留着的几十 GB 已经没了；
//   2. 放弃必须走两步确认——它不可逆（不再提示、不能重试）；
//   3. 三语词典必须同键——放弃是失败路径上的操作，缺词就是界面上裸奔错误码。
//
// 按源码文本断言（同 tests/model_store_dialog_safety.test.mjs 的先例：
// JSX 与 i18n 大对象没法在 node --test 里渲染）。
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = (...parts) => fs.readFileSync(path.join(ROOT, ...parts), 'utf8');

const PANEL = read('src', 'features', 'model-store', 'IncompleteInstallPanel.jsx');
const API = read('src', 'features', 'model-store', 'model-store-api.js');
const LANGS = ['zh', 'en', 'ja'];
const DICTS = new Map(LANGS.map((lang) => [lang, read('src', 'shared', 'i18n', `${lang}.js`)]));

test('dismiss goes through the dedicated command with a two-step inline confirmation', () => {
  assert.match(API, /dismissIncomplete\(runId\)[\s\S]{0,80}model_store_dismiss_incomplete/);
  // 只有已终结的记录才给放弃入口（进行中的任务没有"放弃"语义，后端也会拒绝）。
  assert.match(PANEL, /item\.terminal && <MsButton tone="ghost"[\s\S]{0,120}incompleteDismiss/);
  // 两步确认：先出确认条，确认条里有继续按钮；点放弃本身不直接发请求。
  assert.match(PANEL, /confirmingId && <div role="alertdialog"[\s\S]*?data-testid="incomplete-dismiss-confirm"/);
  assert.match(PANEL, /data-testid="incomplete-dismiss-confirm-button"/);
  assert.match(PANEL, /setConfirmingId\(item\.runId\)/);
  // 确认条打开期间记录可能被整表刷新移除：target 缺失必须安静收回，不能崩。
  assert.match(PANEL, /if \(!item\) \{\s*\n\s*setConfirmingId\(null\);\s*\n\s*return;/);
});

test('dismiss copy promises files are kept and resumable in all three languages', () => {
  for (const text of DICTS.values()) {
    for (const key of ['incompleteDismiss:', 'incompleteDismissConfirmTitle:', 'incompleteDismissConfirmHint:']) {
      assert.ok(text.includes(key), `${key} 缺失`);
    }
    // 错误码文案同步补齐：放弃后的重试会被后端 installation_dismissed 拒绝。
    assert.ok(text.includes('installation_dismissed:'), 'installation_dismissed 文案缺失');
    assert.ok(text.includes('installation_not_dismissable:'), 'installation_not_dismissable 文案缺失');
    // 落盘失败与记录缺失也是放弃按钮可直接触达的错误，缺词就是裸奔错误码。
    assert.ok(text.includes('installation_dismiss_persist_failed:'), 'installation_dismiss_persist_failed 文案缺失');
    assert.ok(text.includes('installation_record_unavailable:'), 'installation_record_unavailable 文案缺失');
    assert.ok(text.includes('installation_order_ambiguous:'), 'installation_order_ambiguous 文案缺失');
    assert.ok(text.includes('installation_index_unavailable:'), 'installation_index_unavailable 文案缺失');
  }
  // 三个语言的确认文案都必须把「文件保留」说出口——这是本功能最重要的承诺。
  assert.match(DICTS.get('zh'), /incompleteDismissConfirmHint:'[^']*原样保留[^']*'/);
  assert.match(DICTS.get('en'), /incompleteDismissConfirmHint:'[^']*kept untouched[^']*'/);
  assert.match(DICTS.get('ja'), /incompleteDismissConfirmHint:'[^']*保持[^']*'/);
});
