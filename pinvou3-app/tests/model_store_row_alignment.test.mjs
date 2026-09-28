#!/usr/bin/env node
// 模型行卡的横向对齐契约。
//
// 「本机已有」和「可下载」是同一个两列网格里上下相邻的两个分区，用户会把它们当成
// 一张表来看。行内右侧只要有一处宽度随状态变化，两个分区的按钮就会错开一截 ——
// 这是纯视觉问题，任何断言不到的地方都只能靠人眼发现，所以在这里钉住成因。
//
// 两处成因（都真实出现过）：
//
// ① 卸载图标只有「本机已有」的行才有，「可下载」的行没有。不给后者留等宽空位，
//    两个分区的主按钮右边缘就差出整整一个按钮的宽度。
// ② 四个主动作里只有「部署」不带图标，比「下载/启用/停用」窄一个图标加间距，
//    同一列上下两行的按钮左边缘对不齐。
//
// 用源码文本断言而不是渲染断言：仓库里没有 DOM 测试环境（无 jsdom/puppeteer），
// 而 tests/move_dialog_contract.test.mjs 早就用同一套路钉 main.jsx 了。
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const ROW = fs.readFileSync(path.join(ROOT, 'src', 'features', 'model-store', 'ModelRow.jsx'), 'utf8');

test('没有卸载按钮的行必须留等宽空位，否则两个分区的主按钮右边缘对不齐', () => {
  // 卸载按钮是条件渲染的，那个三元的 else 分支不能是 null
  assert.match(
    ROW,
    /data-testid="model-store-trailing-spacer"/,
    '「可下载」的行没有卸载按钮，必须补一个占位元素'
  );
  assert.doesNotMatch(
    ROW,
    /data-testid="model-store-uninstall"[\s\S]{0,600}?\)\s*:\s*null\}/,
    '卸载按钮那个三元的 else 分支又变回 null 了，占位没了'
  );
  // 占位和按钮必须用同一个尺寸常量，分别写死迟早会分叉
  const slotUses = ROW.match(/TRAILING_SLOT/g) || [];
  assert.ok(
    slotUses.length >= 3,
    `占位与卸载按钮要共用 TRAILING_SLOT（定义 1 处 + 使用 2 处），实际出现 ${slotUses.length} 次`
  );
});

test('四个主动作按钮宽度下限一致，「部署」不能因为没图标而窄一截', () => {
  const primaries = ROW.match(/data-testid="model-store-(download|deploy|enable|stop)"/g) || [];
  assert.equal(primaries.length, 4, `应该正好四个主动作，实际：${primaries.join(', ')}`);

  // 每个主动作按钮的 className 都要带上那个下限
  const withMinWidth = ROW.match(/className=\{`\$\{BTN_(?:PRIMARY|DANGER)\} \$\{ACTION_MIN_W\}`\}/g) || [];
  assert.equal(
    withMinWidth.length,
    4,
    `四个主动作都要带 ACTION_MIN_W，实际只有 ${withMinWidth.length} 个`
  );

  // 下限本身得是个真值，别被改成空串把这条测试架空
  assert.match(ROW, /const ACTION_MIN_W = 'min-w-\[\d+px\]';/, 'ACTION_MIN_W 必须是一个具体的 min-w 值');
});
