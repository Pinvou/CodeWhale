#!/usr/bin/env node

import { readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
export const baselineFiles = [
  'account-platform/docs/requirements.md',
  'account-platform/docs/architecture.md',
  'account-platform/docs/implementation-tasks.md',
  'account-platform/docs/implementation-status.md',
];

const rules = [
  ['catalog identifier', /(?:SKU(?:\s*\/\s*BOM)?|sku_bom|external_sku|商品编码|product_id)\s*(?:为|是|=|:|：)\s*`?[A-Z0-9][A-Z0-9_-]{4,}`?/i],
  ['catalog amount', /(?:售价|商品金额|充值金额|价格)\s*(?:为|是|=|:|：)?\s*`?(?:CNY|RMB|¥|￥)?\s*\d+(?:\.\d+)?\s*(?:元|分)`?/i],
  ['catalog tier', /\d+(?:\.\d+)?\s*元档/i],
  ['entitlement value', /(?:商品|SKU|充值档|目录).{0,120}(?:BASE|BONUS|积分|额度|微额度)\s*(?:为|是|=|:|：)\s*`?\d[\d_,]*`?/i],
  ['catalog cardinality', /(?:当前|在售|只保留|仅保留|共)\s*`?\d+`?\s*(?:个|款|项)\s*(?:充值)?商品/i],
];

export function violations(source, file = '<memory>') {
  const findings = [];
  source.split(/\r?\n/).forEach((line, index) => {
    for (const [rule, pattern] of rules) {
      if (pattern.test(line)) findings.push(`${file}:${index + 1}: ${rule}`);
    }
  });
  return findings;
}

export async function checkBaseline(root = repositoryRoot, files = baselineFiles) {
  const findings = [];
  for (const file of files) {
    findings.push(...violations(await readFile(path.join(root, file), 'utf8'), file));
  }
  return findings;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const findings = await checkBaseline();
  if (findings.length) {
    console.error(`Recharge product document baseline contains runtime catalog facts:\n${findings.join('\n')}`);
    process.exitCode = 1;
  } else {
    console.log(`Recharge product document baseline passed (${baselineFiles.length} files).`);
  }
}
