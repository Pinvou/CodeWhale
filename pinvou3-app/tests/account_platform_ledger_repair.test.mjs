import test from 'node:test';
import assert from 'node:assert/strict';
import { financialRepairPrecheckBody, financialRepairCommandTypes } from '../src/features/account-platform/admin-financial-repair-form.js';
import { financialRepairApprovalDraft } from '../src/features/account-platform/admin-financial-repair-drafts.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { currentApprovalEditDraft } from '../src/features/account-platform/admin-finance.js';
import { ledgerRepairCopy } from '../src/shared/ledger-repair-i18n.js';
import { ledgerIntentStorage, readLedgerIntent, saveLedgerIntent, clearLedgerIntent } from '../src/features/account-platform/ledger-repair-intent.js';
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const values = { target_resource_id: id(1), target_journal_entry_id: id(2), wrong_ledger_account_id: id(3), correct_ledger_account_id: id(4), amount_microcredits: '9223372036854775807' };
const projection = () => ({ command_type: 'LEDGER_REPAIR', policy_version: 1, snapshot_schema_version: 1,
  target_resource_type: 'LEDGER_POSTING', target_resource_id: id(1),
  normalized_command: { target_posting_id: id(1), target_journal_entry_id: id(2), wrong_ledger_account_id: id(3), correct_ledger_account_id: id(4), amount_microcredits: values.amount_microcredits, reason_code: 'ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT' },
  critical_resource_versions: { policy_version: 1, posting_set_digest: 'a'.repeat(64) },
  financial_snapshot: { amount_microcredits: values.amount_microcredits, wallet_id: null } });

test('Ledger durable invocation preserves operator key, exact facts and actor across reload', () => {
  const memory = new Map();
  const storage = { getItem: key => memory.get(key) ?? null, setItem: (key, value) => memory.set(key, value), removeItem: key => memory.delete(key) };
  const facts = { projection: projection(), reason: 'Controlled attribution correction reason', requestKey: 'k'.repeat(111) };
  const record = ledgerIntentStorage.createIntent(id(8), 'ledger-repair-draft', facts);
  assert.equal(record.key, facts.requestKey);
  saveLedgerIntent(record, storage);
  const restored = readLedgerIntent(id(8), 'ledger-repair-draft', storage);
  assert.deepEqual(restored, record);
  assert.equal(financialRepairApprovalDraft(restored.facts.projection, restored.facts.reason, restored.key).idempotency_key.length, 120);
  assert.equal(readLedgerIntent(id(7), 'ledger-repair-draft', storage), null);
  assert.throws(() => ledgerIntentStorage.createIntent(id(8), 'ledger-repair-draft', { ...facts, requestKey: 'k'.repeat(112) }));
  assert.equal(ledgerIntentStorage.createIntent(id(8), 'ledger-repair-draft', { ...facts, requestKey: '键'.repeat(37) }).key, '键'.repeat(37));
  assert.throws(() => ledgerIntentStorage.createIntent(id(8), 'ledger-repair-draft', { ...facts, requestKey: '键'.repeat(38) }));
  assert.throws(() => saveLedgerIntent({ ...record, facts: { ...facts, reason: 'Changed unknown request reason' } }, storage));
  clearLedgerIntent(id(8), 'ledger-repair-draft', 'different-key', storage);
  assert.deepEqual(readLedgerIntent(id(8), 'ledger-repair-draft', storage), record);
  clearLedgerIntent(id(8), 'ledger-repair-draft', record.key, storage);
  assert.equal(readLedgerIntent(id(8), 'ledger-repair-draft', storage), null);
  const replacement = { ...record, facts: { ...record.facts, reason: 'New facts using the original operator key' } };
  saveLedgerIntent(replacement, storage);
  assert.throws(() => ledgerIntentStorage.clearRecord(record, storage));
  assert.deepEqual(readLedgerIntent(id(8), 'ledger-repair-draft', storage), replacement);
  ledgerIntentStorage.clearRecord(replacement, storage);
  assert.equal(readLedgerIntent(id(8), 'ledger-repair-draft', storage), null);
});

test('Ledger invalid, unwritable and mismatched storage cannot save a sendable invocation', () => {
  const facts = { projection: projection(), reason: 'Controlled attribution correction reason', requestKey: 'ledger-stable-key' };
  const record = ledgerIntentStorage.createIntent(id(8), 'ledger-repair-draft', facts);
  assert.throws(() => saveLedgerIntent(record, { getItem: () => null, setItem: () => { throw new Error('unwritable'); } }));
  assert.throws(() => saveLedgerIntent(record, { getItem: () => null, setItem: () => {} }));
  assert.throws(() => readLedgerIntent(id(8), 'ledger-repair-draft', { getItem: () => '{corrupt' }));
  assert.throws(() => ledgerIntentStorage.createIntent(id(8), 'other-operation', facts));
});
test('installed form sends owner-required fixed reason and full positive int64', () => {
  assert.deepEqual(financialRepairCommandTypes, ['LEDGER_REPAIR', 'RECEIVABLE_WRITE_OFF', 'PAYMENT_FINANCIAL_REPAIR', 'REFUND_EXCEPTION_REPAIR', 'CHARGEBACK_FINANCIAL_REPAIR']);
  assert.deepEqual(financialRepairPrecheckBody('LEDGER_REPAIR', values).body, { command_type: 'LEDGER_REPAIR', ...values, reason_code: 'ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT' });
  for (const amount of ['9007199254740993', '9223372036854775807']) assert.equal(financialRepairPrecheckBody('LEDGER_REPAIR', { ...values, amount_microcredits: amount }).body.amount_microcredits, amount);
  for (const amount of ['0', '-1', '9223372036854775808', '1e2', ' 1']) assert.ok(financialRepairPrecheckBody('LEDGER_REPAIR', { ...values, amount_microcredits: amount }).error);
  assert.ok(financialRepairPrecheckBody('RECEIVABLE_WRITE_OFF', values).error);
});
test('real client uses v2 BFF and lossless numeric JSON, while preserving CSRF and no-store', async () => {
  const previous = globalThis.fetch; let call;
  globalThis.fetch = async (url, options) => { call = { url, options }; return new Response(JSON.stringify(projection()), { status: 200 }); };
  try {
    const client = new AdminPlatformClient('https://portal.example'); client.csrfToken = 'controlled-csrf';
    await client.approvalCommandPrecheck(financialRepairPrecheckBody('LEDGER_REPAIR', values).body);
    assert.equal(call.url, 'https://portal.example/api/admin/account/admin/v2/approval-command-prechecks');
    assert.equal(call.options.method, 'POST'); assert.equal(call.options.headers['X-CSRF-Token'], 'controlled-csrf');
    assert.equal(call.options.credentials, 'same-origin'); assert.equal(call.options.cache, 'no-store');
    assert.ok(call.options.body.includes('"amount_microcredits":9223372036854775807'));
    assert.equal(call.options.headers['Idempotency-Key'], undefined);
    for (const status of [202, 204]) { globalThis.fetch = async () => new Response(status === 204 ? null : '{}', { status }); await assert.rejects(client.approvalCommandPrecheck({}), /INVALID_MUTATION_ACK/); }
  } finally { globalThis.fetch = previous; }
});
test('draft and typed EDIT retain exact original account/amount intent and fresh owner snapshots', async () => {
  const original = projection(); const draft = financialRepairApprovalDraft(original, 'Controlled attribution correction reason', 'ledger-stable-key');
  assert.deepEqual(draft.normalized_command, original.normalized_command);
  assert.equal(draft.idempotency_key, 'approval:ledger-stable-key');
  let read;
  const current = { ...original, critical_resource_versions: { ...original.critical_resource_versions, posting_set_digest: 'b'.repeat(64) } };
  const edited = await currentApprovalEditDraft({ approvalCommandPrecheck: async body => { read = body; return current; } }, { approval_request_id: id(9), command_type: 'LEDGER_REPAIR', status: 'DRAFT', allowed_actions: ['EDIT'], target: { resource_id: id(1) }, command: original.normalized_command }, 'Updated controlled review reason');
  assert.equal(read.amount_microcredits, values.amount_microcredits); assert.equal(read.wrong_ledger_account_id, id(3)); assert.equal(read.correct_ledger_account_id, id(4));
  assert.deepEqual(edited.normalized_command, original.normalized_command); assert.equal(edited.critical_resource_versions.posting_set_digest, 'b'.repeat(64));
});
test('all three languages have the complete current Ledger and WriteOff form labels', () => {
  const leaves = (v, prefix = '') => Object.entries(v).flatMap(([k, value]) => typeof value === 'string' ? [prefix + k] : leaves(value, prefix + k + '.'));
  const keys = leaves(ledgerRepairCopy.zh).sort();
  for (const language of ['zh', 'en', 'ja']) { assert.deepEqual(leaves(ledgerRepairCopy[language]).sort(), keys); assert.deepEqual(Object.keys(ledgerRepairCopy[language].targets), ['LEDGER_REPAIR', 'RECEIVABLE_WRITE_OFF', 'PAYMENT_FINANCIAL_REPAIR', 'REFUND_EXCEPTION_REPAIR', 'CHARGEBACK_FINANCIAL_REPAIR']); for (const key of Object.keys(values)) assert.ok(key === 'target_resource_id' ? ledgerRepairCopy[language].targets.LEDGER_REPAIR : ledgerRepairCopy[language].fields[key]); }
});

import { approvalCommands, approvalBusinessRoute, readApprovalRoute } from '../src/features/account-platform/admin-approval-actions.js';
test('installed Ledger center filter and business drilldown preserve the closed target', () => {
  assert.ok(approvalCommands.includes('LEDGER_REPAIR'));
  assert.equal(readApprovalRoute('?approval_command_type=LEDGER_REPAIR').commandType, 'LEDGER_REPAIR');
  const detail = { detail_availability: 'AVAILABLE', command_type: 'LEDGER_REPAIR', target: { resource_type: 'LEDGER_POSTING', resource_id: id(1) }, command: { target_posting_id: id(1) } };
  assert.deepEqual(approvalBusinessRoute(detail), { section: 'finance', tab: 'ledger' });
  for (const value of [{ ...detail, command: { target_posting_id: id(2) } }, { ...detail, detail_availability: 'DETAIL_PRESENTATION_UNAVAILABLE' }, { ...detail, command_type: 'RECEIVABLE_WRITE_OFF' }]) assert.equal(approvalBusinessRoute(value), null);
});
