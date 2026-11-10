import assert from 'node:assert/strict';
import { test } from 'node:test';
import { adminComponent, adminDOM } from './helpers/admin-component-fixture.mjs';
import { reconciliationImportPreview } from '../src/features/account-platform/reconciliation-contract.js';
import { reconciliationCopy } from '../src/shared/i18n.js';

const bill = (paid_at = '2026-10-07T10:00:00+08:00') => JSON.stringify([{ merchant_order_no: 'SYNTHETIC-ORDER', channel_transaction_id: 'SYNTHETIC-TRANSACTION', channel: 'WECHAT', amount_fen: 100, currency: 'CNY', status: 'SUCCESS', paid_at }]);
test('preview rejects malformed RFC3339 timestamps and impossible calendar dates at their row/field', () => {
  for (const timestamp of ['not-a-date', '2026-02-29T10:00:00Z', '2026-02-30T10:00:00Z', '2026-10-07', '2026-10-07T24:00:00Z', '2026-10-07T10:60:00Z', '2026-10-07T10:00:00', '2026-10-07T10:00:00+25:00', '2026-10-07T10:00:00+08:00\n', '2026-10-07T10:00:00+08:00\r']) {
    assert.throws(() => reconciliationImportPreview(bill(timestamp)), error => error.kind === 'billInvalidField' && error.row === 1 && error.field === 'paid_at', timestamp);
  }
  for (const timestamp of ['2024-02-29T10:00:00Z', '2026-10-07T10:00:00.123456789+08:00', '2026-10-07T10:00:00,123-05:30']) {
    assert.equal(reconciliationImportPreview(bill(timestamp)).items[0].paid_at, timestamp, 'preserve the original authoritative timestamp');
  }
});

for (const mode of ['file', 'paste']) for (const timestamp of ['not-a-date', '2026-10-07T10:00:00+08:00\n']) test('invalid paid_at cannot become ready through the actual ' + mode + ' input: ' + JSON.stringify(timestamp), async t => {
  const { AdminReconciliationBillImport } = await adminComponent('AdminReconciliationBillImport.jsx');
  const ui = await adminDOM(t), copy = reconciliationCopy.en;
  await ui.render(AdminReconciliationBillImport, { channel: 'WECHAT', date: '2026-10-07', copy, onImport() { assert.fail('invalid bill dispatched'); } });
  if (mode === 'file') {
    const input = ui.document.querySelector('input[type=file]');
    Object.defineProperty(input, 'files', { value: [{ name: 'synthetic.json', size: 100, text: async () => bill(timestamp) }] });
    await ui.change(input, '');
  } else {
    await ui.click(ui.document.querySelector('.adm-reconciliation-bill-source button'));
    await ui.change(ui.document.querySelector('textarea'), bill(timestamp));
    await ui.click(Array.from(ui.document.querySelectorAll('button')).find(node => node.textContent === copy.billPreview));
  }
  assert.equal(ui.document.querySelector('footer button').disabled, true);
  assert.match(ui.document.querySelector('[role=alert]').textContent, /paid_at/);
});
