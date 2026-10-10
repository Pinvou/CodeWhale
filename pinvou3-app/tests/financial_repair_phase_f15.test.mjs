import test from 'node:test';
import assert from 'node:assert/strict';
import { financialRepairCommands, financialRepairApprovalDraft, financialRepairPrecheck } from '../src/features/account-platform/admin-financial-repair-drafts.js';
import { financialRepairInputs, financialRepairPrecheckBody } from '../src/features/account-platform/admin-financial-repair-form.js';
const command = "RECONCILIATION_FINANCIAL_REPAIR";
const target = "11111111-1111-4111-8111-111111111111";
const normalized = {"reconciliation_case_id": "11111111-1111-4111-8111-111111111111", "payment_evidence_id": "22222222-2222-4222-8222-222222222222", "business_date": "2026-10-07", "resolution_code": "PROVIDER_REFUND_CONFIRMED"};
const projection = { command_type: command, policy_version: 1, snapshot_schema_version: 1,
 target_resource_type: financialRepairCommands[command].targetResourceType, target_resource_id: target,
 normalized_command: normalized, critical_resource_versions: { run_id: '33333333-3333-4333-8333-333333333333', case_id: target, provider_snapshot_digest: 'b'.repeat(64), facts_digest: 'a'.repeat(64), policy_version: 1 },
 financial_snapshot: { amount_fen: '9007199254740993', currency: 'CNY', provider_transaction_id: 'controlled-provider-transaction', ledger_effect: 'NONE' } };

test('F15 financial descriptor set exposes only delivered operations', () => {
 assert.deepEqual(Object.keys(financialRepairCommands).sort(), ["LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF", "PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR", "RECONCILIATION_FINANCIAL_REPAIR", "USAGE_SETTLEMENT_REPAIR"].sort());
 assert.deepEqual(Object.keys(financialRepairInputs).sort(), ["LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF", "PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR", "RECONCILIATION_FINANCIAL_REPAIR", "USAGE_SETTLEMENT_REPAIR"].sort());
 assert.equal(financialRepairPrecheck({ ...projection, command_type: 'UNKNOWN_REPAIR' }), null);
});

test('F15 owner target and complete normalized facts bind the request', () => {
 assert.ok(financialRepairPrecheck(projection));
 assert.equal(financialRepairPrecheck({ ...projection, target_resource_id: '33333333-3333-4333-8333-333333333333' }), null);
 for (const field of financialRepairCommands[command].fields) {
  const facts = { ...normalized }; delete facts[field.key];
  assert.equal(financialRepairPrecheck({ ...projection, normalized_command: facts }), null);
 }
 const input = { target_resource_id: target, payment_evidence_id: "22222222-2222-4222-8222-222222222222", business_date: '2026-10-07', usage_request_id: "22222222-2222-4222-8222-222222222222", loss_review_id: "22222222-2222-4222-8222-222222222222", untrusted_amount: '10' };
 const accepted = financialRepairPrecheckBody(command, input);
 assert.ok(accepted.body);
 assert.ok(!Object.hasOwn(accepted.body, 'untrusted_amount'));
 assert.ok(financialRepairPrecheckBody(command, { ...input, target_resource_id: 'not-an-owner-id' }).error);
});

test('F15 draft preserves int64 owner documents and closed authority', () => {
 const polluted = { ...projection, normalized_command: { ...normalized, attacker_amount: '10' } };
 const draft = financialRepairApprovalDraft(polluted, 'A legitimate operator explanation', 'f15-logical-attempt');
 assert.equal(draft.command_type, command);
 assert.equal(draft.target_resource_id, target);
 assert.strictEqual(draft.critical_resource_versions, projection.critical_resource_versions);
 assert.strictEqual(draft.financial_snapshot, projection.financial_snapshot);
 assert.equal(draft.financial_snapshot.amount_fen, '9007199254740993');
 assert.equal(draft.normalized_command[financialRepairCommands[command].reasonKey], financialRepairCommands[command].reasonCode);
 assert.ok(!Object.hasOwn(draft.normalized_command, 'attacker_amount'));
 assert.throws(() => financialRepairApprovalDraft(projection, 'short', 'f15-logical-attempt'));
});
