import assert from 'node:assert/strict';
import { test } from 'node:test';
import { act } from 'react';
import { adminComponent, adminDOM } from './helpers/admin-component-fixture.mjs';
import { RECONCILIATION_BILL_MAX_BYTES, reconciliationImportPreview } from '../src/features/account-platform/reconciliation-contract.js';
import { reconciliationCopy } from '../src/shared/i18n.js';

const bill = (paid_at = '2026-10-07T10:00:00+08:00') => JSON.stringify([{ merchant_order_no: 'SYNTHETIC-ORDER', channel_transaction_id: 'SYNTHETIC-TRANSACTION', channel: 'WECHAT', amount_fen: 100, currency: 'CNY', status: 'SUCCESS', paid_at }]);

for (const language of ['zh', 'en', 'ja']) for (const mode of ['file', 'paste']) test('typed bill fields block malformed values and recover to valid import: ' + language + '/' + mode, async t => {
  const { AdminReconciliationBillImport } = await adminComponent('AdminReconciliationBillImport.jsx');
  const ui = await adminDOM(t), copy = reconciliationCopy[language], imports = [];
  await ui.render(AdminReconciliationBillImport, { channel: 'WECHAT', date: '2026-10-07', copy, onImport: items => imports.push(items) });
  if (mode === 'paste') await ui.click(ui.document.querySelector('.adm-reconciliation-bill-source button'));
  const input = async text => {
    if (mode === 'file') {
      const node = ui.document.querySelector('input[type=file]');
      Object.defineProperty(node, 'files', { configurable: true, value: [{ name: 'synthetic.json', size: Buffer.byteLength(text), text: async () => text }] });
      await ui.change(node, '');
    } else {
      await ui.change(ui.document.querySelector('textarea'), text);
      await ui.click(Array.from(ui.document.querySelectorAll('button')).find(node => node.textContent === copy.billPreview));
    }
  };
  for (const field of ['entry_type', 'channel']) for (const value of [123, {}, [], true]) {
    const rows = JSON.parse(bill()); rows[0][field] = value;
    await input(JSON.stringify(rows));
    const alert = ui.document.querySelector('[role=alert]'); assert.ok(alert, field);
    assert.equal(alert.textContent, copy.billInvalidField.replace('{row}', '1').replace('{field}', field));
    assert.equal(ui.document.querySelector('footer button').disabled, true);
    await ui.click(ui.document.querySelector('footer button'));
    assert.equal(imports.length, 0);
  }
  for (const value of [undefined, '', null]) {
    const rows = JSON.parse(bill()); rows[0].entry_type = value; rows[0].channel = value;
    await input(JSON.stringify(rows));
    assert.ok(!ui.document.querySelector('[role=alert]'));
    assert.equal(ui.document.querySelector('footer button').disabled, false, 'retain Go string default semantics');
  }
  await ui.click(ui.document.querySelector('footer button'));
  assert.equal(imports.length, 1);
  assert.equal(imports[0][0].entry_type, null);
  assert.equal(imports[0][0].channel, null);
});

for (const language of ['zh', 'en', 'ja']) test('six bill validation errors render exact localized feedback and block import: ' + language, async t => {
  const { AdminReconciliationBillImport } = await adminComponent('AdminReconciliationBillImport.jsx');
  const ui = await adminDOM(t), copy = reconciliationCopy[language];
  const missingReference = JSON.parse(bill()); delete missingReference[0].channel_transaction_id;
  const cases = [
    ['billMissingReference', JSON.stringify(missingReference)],
    ['billInvalidAmount', bill().replace('"amount_fen":100', '"amount_fen":9223372036854775808')],
    ['billEmpty', '[]'], ['billNotArray', '{}'],
    ['billTooMany', JSON.stringify(Array(100001).fill({}))],
    ['billTooLarge', null],
  ];
  let writes = 0, oversizedReads = 0;
  for (const [kind, text] of cases) {
    await ui.render(AdminReconciliationBillImport, { channel: 'WECHAT', date: '2026-10-07', copy, onImport() { writes++; } });
    const input = ui.document.querySelector('input[type=file]');
    Object.defineProperty(input, 'files', { configurable: true, value: [{ name: 'synthetic-invalid.json', size: text === null ? RECONCILIATION_BILL_MAX_BYTES + 1 : Buffer.byteLength(text), text: async () => { if (text === null) oversizedReads++; return text; } }] });
    await ui.change(input, '');
    assert.equal(ui.document.querySelector('[role=alert]').textContent, copy[kind].replace('{row}', kind === 'billMissingReference' ? '1' : '').replace('{field}', ''), kind);
    assert.equal(ui.document.querySelector('footer button').disabled, true, kind);
    await ui.click(ui.document.querySelector('footer button'));
    assert.equal(writes, 0, kind);
  }
  assert.equal(oversizedReads, 0, 'reject size before reading the file');
});
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

test('changing channel/date retires both a ready preview and a late file read; the next valid preview remains usable', async t => {
  const { AdminReconciliationBillImport } = await adminComponent('AdminReconciliationBillImport.jsx');
  const ui = await adminDOM(t), copy = reconciliationCopy.en, calls = [];
  const props = { channel: 'WECHAT', date: '2026-10-07', copy, onImport: items => calls.push(items) };
  await ui.render(AdminReconciliationBillImport, props);
  await ui.click(ui.document.querySelector('.adm-reconciliation-bill-source button'));
  await ui.change(ui.document.querySelector('textarea'), bill());
  await ui.click(Array.from(ui.document.querySelectorAll('button')).find(node => node.textContent === copy.billPreview));
  assert.equal(ui.document.querySelector('footer button').disabled, false);
  await ui.render(AdminReconciliationBillImport, { ...props, channel: 'ALIPAY' });
  assert.equal(ui.document.querySelector('footer button').disabled, true);
  assert.ok(!ui.document.querySelector('textarea'));
  assert.equal(calls.length, 0);
  await ui.render(AdminReconciliationBillImport, props);
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const oldInput = ui.document.querySelector('input[type=file]');
  Object.defineProperty(oldInput, 'files', { value: [{ name: 'synthetic-old.json', size: 100, text: () => pending }] });
  await ui.change(oldInput, '');
  await ui.render(AdminReconciliationBillImport, { ...props, date: '2026-10-08' });
  await act(async () => release(bill()));
  assert.equal(ui.document.querySelector('footer button').disabled, true);
  assert.equal(ui.document.querySelector('.adm-reconciliation-bill-preview [role=status]').textContent, copy.billPending);
  const nextInput = ui.document.querySelector('input[type=file]');
  Object.defineProperty(nextInput, 'files', { value: [{ name: 'synthetic-new.json', size: 100, text: async () => bill('2026-10-08T10:00:00+08:00') }] });
  await ui.change(nextInput, '');
  assert.equal(ui.document.querySelector('footer button').disabled, false);
  await ui.click(ui.document.querySelector('footer button'));
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0].paid_at, '2026-10-08T10:00:00+08:00');
});
