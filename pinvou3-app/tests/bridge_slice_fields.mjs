// 桌面 STATE_SLICE_FIELDS 解析器——域契约测试的单一真相源。
// round-6 评审 P2 之前 web 侧契约测试手抄键集（单向锁：桌面加字段漏改 web
// 时测试看不见），现在 web/desktop 两侧都从这里的解析结果取期望值。
// FE14 wave 5 起注册表收敛到 shared/bridge-shared-helpers.js（两 lane 一份），
// 这里读取同一份常量。
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

export function parseSliceFields(source) {
  const match = source.match(/const STATE_SLICE_FIELDS = \{([\s\S]*?)\n {2}\};/);
  assert.ok(match, 'STATE_SLICE_FIELDS not found in shared/bridge-shared-helpers.js');
  const domains = {};
  for (const entry of match[1].matchAll(/(\w+): \[([^\]]*)\]/g)) {
    domains[entry[1]] = entry[2]
      .split(',')
      .map((field) => field.trim().replace(/^"|"$/g, ''))
      .filter(Boolean);
  }
  return domains;
}

export function desktopSliceFields() {
  return parseSliceFields(
    readFileSync(new URL('../src/shared/bridge-shared-helpers.js', import.meta.url), 'utf8'),
  );
}
