import assert from 'node:assert/strict';
import test from 'node:test';
import { checkBaseline, violations } from '../../../scripts/check-recharge-product-document-baseline.mjs';

test('requirements, architecture, task, and status baselines contain no runtime catalog facts', async () => {
  assert.deepEqual(await checkBaseline(), []);
});

test('document baseline rejects concrete catalog identifiers, prices, entitlements, and counts', () => {
  for (const fact of [
    'SKU/BOM 为 `EXAMPLE123`。',
    '商品金额：0.1 元。',
    '49 元档作为默认商品。',
    '该商品 BASE = 2000。',
    '当前仅保留 1 个充值商品。',
  ]) assert.ok(violations(fact).length, fact);
});
